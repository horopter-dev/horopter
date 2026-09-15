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

TBD
