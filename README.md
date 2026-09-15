# Horopter

**horopter** (/həˈrɒptər/, hor-**OP**-ter) — in binocular vision, the locus of
points that fall on corresponding retinal points in both eyes: where two
independent views agree and fuse into one.

One eye gives a flat image; two give depth. Metrics alone are flat. Metrics plus
change events give causal depth.

A vendor-neutral store for change events. Humans and machines write change
events in; observability tools read them out to enrich graphs.

## Status

v0.1 in development. See `docs/superpowers/specs/` for the design.

## Quick start

    task up
    curl -X POST localhost:8080/v1/events \
      -H 'Authorization: Bearer local-dev-token' -H 'Content-Type: application/json' \
      -d '{"source":"ci","type":"deployment.started","subject":"acme/prod",
           "occurred_at":"2026-09-14T10:00:00Z","correlation_ref":"dep-1"}'
    curl "localhost:8081/adapters/grafana/annotations?from=1789376400000&to=1789383600000"
    task down

`task up` builds and starts the full stack with `docker compose`: Postgres, then
`horopter-migrate` (which must run to completion before the daemons start — it is the
only binary permitted to apply schema DDL), then `horopter-ingress` on port 8080 and
`horopter-egress` on port 8081. `task down` stops the stack and removes the database
volume.

Each service is configured with environment variables:

- `DATABASE_URL` — PostgreSQL connection string, required by all three binaries.
- `HOROPTER_INGRESS_TOKEN` — bearer token `horopter-ingress` requires on writes; it
  refuses to start without one.
- `HOROPTER_ADDR` — listen address for `horopter-ingress` and `horopter-egress`
  (defaults to `0.0.0.0:8080` and `0.0.0.0:8081` respectively).

## Security

**`horopter-egress` has no authentication in v0.1, and `docker-compose.yml` publishes its
port (8081) on the host.** Every route it serves — `GET /v1/changes`, `GET /v1/events`,
`GET`/`POST /adapters/grafana/annotations` and its `/{tenant}/annotations` path variant,
and `GET /adapters/otlp/spans` — answers anyone who can reach that port, with no
credential check of any kind. A caller can list a tenant's full change history and raw
event log: subjects, actors, titles, correlation refs, and every attribute a producer
attached, including whatever was put in `horopter.description`.

Egress also relays raw store errors verbatim in its HTTP responses, so a failure can
surface internal detail — a Postgres error message, a query shape — to that same
unauthenticated caller.

Both are deliberate decisions for v0.1, not oversights. `horopter-ingress` does require a
bearer token (`HOROPTER_INGRESS_TOKEN`) on writes, so this exposure is specific to the
read side. If you deploy this anywhere it is reachable by anyone you would not trust with
the full change log, put a network boundary or a reverse proxy with its own
authentication in front of port 8081 — Horopter provides neither.

**`docker-compose.yml` also publishes Postgres itself (5432) on the host, with the
username and password both `horopter`.** That is direct read and write access to the
event log, past every check either daemon makes, for anyone who can reach the port. The
compose file is a local development stack and nothing else: do not run it anywhere its
ports are reachable from outside the machine, and do not reuse its credentials.

### Container health checks

The images carry no `HEALTHCHECK`, and will not. They are built `FROM
gcr.io/distroless/static-debian12`, which has no shell, no `curl` and no `wget`, so a
`HEALTHCHECK` would have to be a probe subcommand compiled into each binary — new
surface on the write daemon to duplicate a check an orchestrator already makes against
`/readyz` over the network. Point your orchestrator's liveness probe at `/healthz` and
its readiness probe at `/readyz`.

## Upgrading

`horopter-migrate` is the only binary permitted to apply schema DDL. Run it to completion
against the database before starting any daemon at the new version; each daemon asserts the
recorded schema version at startup and refuses to run against one it does not understand,
so a skewed rollout fails loudly rather than corrupting data quietly.

### Rebuilding a tenant's projection

    horopter-migrate --rebuild-tenant=<tenant-id>

The `change` table is a derived view of the immutable `event` log, so it can always be
discarded and replayed. This runs only after the schema-version assertion passes.

**It blocks every append for that tenant for the whole of its run.** Blocked appends fail
after a few seconds with a retryable `503`, so producers retry rather than losing events,
but the rebuild is not batched, reports no progress, and cannot run online. Run it when the
tenant's write traffic is quiet.

Migration `0003` needs it. It splits a change's start into `started_at` (what a producer
asserted) and `first_seen_at` (the earliest event seen), and existing rows cannot be
backfilled truthfully from the projection alone — which of the two an old `start_at` was is
exactly what the previous model destroyed. The migration records `first_seen_at = start_at`
and `started_at = NULL`, meaning "we do not know". A rebuild is what replaces that with the
real values from the event log.

**A rebuild can change a change's recorded outcome.** It replays the log in event-id
order, while live appends folded it in commit order, and the two differ whenever a
same-ref event committed before another that carries a lower id. That needs no unusual
timing: an id is minted on the ingress replica that received the request, before its
append starts, so with more than one replica the window is the clock skew between them,
and even with one replica an append that minted a lower id can lose the race for the
per-ref lock. Every field whose rule is "last event wins" — `last_event_id`, `type`,
`title`, `actor`, `subject` and attributes — can come back different. `status` and `end`
follow the narrower rule "last *terminal* event wins", so a `deployment.completed` and a
`deployment.failed` on one ref can swap, but nothing that arrives later can reopen a change
that has already closed. `start`,
`started_at` and `first_seen_at` cannot: they are minima, so replay order does not reach
them. Rebuilding twice gives the same answer both times — it is the live projection the
replay may disagree with, not itself. If a change's outcome matters to you, read `GET
/v1/events?ref=<ref>` (the log is immutable and is never reordered) before and after.

### v0.1 pre-release: the CloudEvents idempotency key format changed

The key `POST /v1/events/cloudevents` dedupes on was the CloudEvents `id` alone. It is now
an unambiguous encoding of `source` **and** `id` together, because CloudEvents guarantees
uniqueness of the pair and not of the id by itself — two producers sharing an id had the
second event silently swallowed and answered `200 {"created": false}`.

An envelope stored under the old key is a cache miss under the new one, so **a producer
retry that spans the upgrade creates a second event** rather than being recognised as a
duplicate. There is no migration for this: the old keys stay in the `event` table as they
are and simply stop matching.

For v0.1 pre-release, with no production data, that is accepted rather than worked around.
If you are carrying data you care about, quiesce producers across the upgrade, or accept
and later reconcile the duplicates. `POST /v1/events` is unaffected — its `idempotency_key`
is supplied by the caller verbatim.

## Truncated responses

Every read endpoint caps its result set at 1000 rows. When rows were discarded, the
response carries **`X-Horopter-Truncated: true`**; when nothing was discarded the header
is absent entirely. All four read endpoints set it, across six registered routes:

- `GET /v1/changes`
- `GET /v1/events`
- `GET /adapters/grafana/annotations` and `POST /adapters/grafana/annotations`
  (including the `/adapters/grafana/{tenant}/annotations` variant)
- `GET /adapters/otlp/spans`

Act on it. Changes are returned oldest first, so truncation discards the **most recent**
rows — during an incident, exactly the ones being looked for — and a truncated response
is otherwise indistinguishable from a complete one. Narrow the time window, or filter by
`subject`, `type` or `status`, and repeat the query until the header is gone. A `limit`
query parameter below 1000 asks for fewer rows; it cannot raise the cap.

The signal is a header rather than a field in the body because two of these routes render
vendor-owned shapes — Grafana annotations and OTLP spans — that cannot carry a Horopter
envelope.

## Attributes

Attribute keys follow OpenTelemetry semantic conventions rather than an invented
vocabulary — see `docs/superpowers/specs/2026-09-14-horopter-design.md` §5.4 for the
`service.*`, `deployment.*`, `cicd.*` and `cloudevents.*` keys every producer should aim
for.

One key is a Horopter-specific extension, not an OTel convention, because nothing in
that vocabulary covers it: **`horopter.description`**. The Grafana annotations adapter
(`GET`/`POST /adapters/grafana/annotations`) reads it as the annotation body text. It is
namespaced under `horopter.` the same way the OTLP adapter's own `horopter.subject`,
`horopter.ref` and `horopter.status` attributes are, to keep it distinct from a
producer's own attributes — a bare, unnamespaced `description` attribute is not read for
this purpose.

## Licence

None yet, deliberately. This repository is private, and the licensing decision is
deferred rather than pending — there is nothing to decide while the only readers
are people who already have access.

Note what the absence of a licence means, because it is the opposite of what an
unlicensed repository is often assumed to mean: with no licence granted, default
copyright applies and all rights are reserved. Nobody may use, copy, modify or
redistribute this. That is the correct and intended position for private code.

**Settle this before making the repository public**, not after — the default
above does not become permissive by publication, and contributions accepted
without a licence in place are awkward to relicense later.
