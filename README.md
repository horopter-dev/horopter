# Horopter

**horopter** (/həˈrɒptər/, hor-**OP**-ter) — in binocular vision, the locus of
points that fall on corresponding retinal points in both eyes: where two
independent views agree and fuse into one.

One eye gives a flat image; two give depth. Metrics alone are flat. Metrics plus
change events give causal depth.

A vendor-neutral store for change events. Humans and machines write change
events in; observability tools read them out to enrich graphs.

## Status

v0.1 in development.

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

A read that fails is answered `500` with a generic body, and the underlying error — which
can name tables, constraints and the database host — is written to the daemon's log
instead, so a failure does not hand internal detail to that same caller.

The missing authentication is a deliberate decision for v0.1, not an oversight.
`horopter-ingress` does require a bearer token (`HOROPTER_INGRESS_TOKEN`) on writes, so
this exposure is specific to the read side. If you deploy this anywhere it is reachable by
anyone you would not trust with the full change log, put a network boundary or a reverse
proxy with its own authentication in front of port 8081 — Horopter provides neither.

**`docker-compose.yml` also publishes Postgres itself (5432) on the host, with the
username and password both `horopter`.** That is direct read and write access to the
event log, past every check either daemon makes, for anyone who can reach the port. The
compose file is a local development stack and nothing else: do not run it anywhere its
ports are reachable from outside the machine, and do not reuse its credentials.

**The `/metrics` listener is unauthenticated on both daemons.** It binds `localhost` by
default, which is reachable only from inside the same container or pod. Bound to another
interface, it exposes metric names, route paths and the `target_info` series — and so
whatever is in `OTEL_RESOURCE_ATTRIBUTES` — to anyone who can reach it, but no tenant,
event or database identity: see the Telemetry section below for what it records.

### Container health checks

The images carry no `HEALTHCHECK`, and will not. They are built `FROM
gcr.io/distroless/static-debian12`, which has no shell, no `curl` and no `wget`, so a
`HEALTHCHECK` would have to be a probe subcommand compiled into each binary — new
surface on the write daemon to duplicate a check an orchestrator already makes against
`/readyz` over the network. Point your orchestrator's liveness probe at `/healthz` and
its readiness probe at `/readyz`.

## Telemetry

Horopter reports on its own operation through OpenTelemetry: telemetry spans describing its
HTTP requests, store operations and SQL statements, and metrics describing request rates,
event appends, its connection pool and the process itself. This is separate from the
change-event product surface: it describes Horopter's own request handling and database
calls, never anything a producer submitted.

Everything is configured by environment variables, resolved once at startup.
**`OTEL_SDK_DISABLED=true` turns all of it off** — traces, OTLP metrics and the `/metrics`
endpoint. This is the standard OTel variable name; `opentelemetry-go` does not implement it
itself (a proposal to add it to the SDK was declined on the grounds that applications can),
so Horopter reads it directly.

Two variables shape both signals alike, because they configure the resource every span and
metric is reported against:

- `OTEL_SERVICE_NAME` — overrides the `service.name` resource attribute reported with every
  span and metric. Defaults to the binary's own name: `horopter-ingress`, `horopter-egress`
  or `horopter-migrate`.
- `OTEL_RESOURCE_ATTRIBUTES` — comma-separated `key=value` pairs merged onto the resource
  every span and metric carries, e.g. `deployment.environment.name=prod,team=platform`.
  **This one is different from the others**: Horopter's own configuration code never reads
  it. It works because building the resource calls the OpenTelemetry SDK's
  `resource.Default()`, and that function's own environment detector reads this variable —
  the same thing it would do for any Go program built on this SDK, not something Horopter
  added. It is genuinely honoured, but it is upstream behaviour Horopter inherits rather than
  configures, and no test in this project exercises it — a future SDK upgrade could change or
  drop it with nothing here to notice.

### Tracing

Tracing is **off by default** in all three binaries.

- `OTEL_EXPORTER_OTLP_ENDPOINT` — the OTLP/HTTP collector to send telemetry to, e.g.
  `http://otel-collector:4318`. It turns on both tracing and OTLP metrics. **Absent means
  both are off**, deliberately: OTel's own SDK default is `localhost:4318`, and honouring
  that default would mean every developer running Horopter with no collector nearby gets a
  stream of export failures that look like a fault and are not one.
- `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` — the signal-specific form, e.g.
  `http://otel-collector:4318/v1/traces`. It turns on tracing only, and takes precedence in
  the exporter, which is the standard OTel rule and the normal shape when traces and metrics
  go to different collectors.
- `HOROPTER_TRACE_DISABLE` — a comma-separated list of instrumentation areas to silence even
  while tracing is otherwise on. It affects tracing only; metrics have no per-area switch.
  Recognised areas:
  - `http` — the server span `otelhttp` wraps every request in.
  - `store` — the spans the store package creates around each store-level operation: append
    an event, list events, list changes, and rebuild a tenant's projection. Disabling it on
    `horopter-migrate` silences the rebuild span, which is the one an operator running
    `--rebuild-tenant` most wants.
  - `sql` — the per-statement spans `otelpgx` creates around every SQL statement the store
    issues.

  An unrecognised name is accepted rather than rejected — Horopter never validates it
  against the areas above. Concretely: misspell an area and nothing errors, it just silently
  disables nothing, which is a real footgun if you mistype one. The same tolerance also means
  it is safe to disable an area that doesn't exist yet — if a later release instruments a new
  one, a config written for it ahead of time needs no change; it starts taking effect the
  moment that area's spans start naming themselves that.

**Recommended production setting:** enable `sql` on `horopter-ingress`; set
`HOROPTER_TRACE_DISABLE=sql` on `horopter-egress` to disable it there. An append runs several
SQL statements inside one transaction, and when one of them is what's slow or blocked, a
per-statement span is the only way to see which. On the read side those same per-statement
spans buy much less for much more: a dashboard refreshing every ten seconds across twenty
engineers turns them into a permanent, high-volume stream, while the one fact worth knowing
on a read — whether the result was truncated — is already an attribute on the `store`-area
span itself, not something a `sql`-area span would add.

### Metrics

`horopter-ingress` and `horopter-egress` serve Prometheus metrics at `/metrics` on a listener
of their own, **on by default**, and push the same metrics over OTLP when a collector is
configured.

- `OTEL_EXPORTER_PROMETHEUS_HOST` — the host the `/metrics` listener binds. Default
  `localhost`.
- `OTEL_EXPORTER_PROMETHEUS_PORT` — its port. Default `9464`, on both daemons.
- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` — the signal-specific OTLP endpoint for metrics, e.g.
  `http://otel-collector:4318/v1/metrics`. Setting it, or `OTEL_EXPORTER_OTLP_ENDPOINT`,
  turns on OTLP metrics.
- `OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT` and
  `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` — read by the OpenTelemetry SDK and
  exporter themselves. Defaults: a push every 60 seconds, a 30-second timeout, cumulative
  temporality.

`horopter-migrate` runs once and exits, so nothing could scrape it and it serves no
`/metrics`. With a collector configured, its metrics go out over OTLP as it finishes.

The host and port defaults are the OpenTelemetry specification's, which has two consequences:

- **Inside a container, `localhost` is reachable only from the same network namespace.** A
  collector sidecar in the same pod can scrape it as shipped; anything scraping over the
  network needs `OTEL_EXPORTER_PROMETHEUS_HOST=0.0.0.0`. The images have no shell or `curl`,
  so `docker exec` cannot reach it either. `docker-compose.yml` sets the host and publishes
  the ports on the host's loopback: `curl localhost:9464/metrics` for ingress,
  `curl localhost:9465/metrics` for egress.
- **Both daemons default to port 9464.** In separate containers or pods that does not matter.
  Running both binaries on one host, or in one pod, needs a different port on one of them.

What is recorded:

- `horopter_event_appends_total` — every event submitted to the store, labelled with
  `horopter_append_outcome` (`created`, `deduplicated` or `failed`),
  `horopter_append_dedupe_path` (`none`, `lookup` or `unique_violation`),
  `horopter_error_kind` (`invalid_event` or `transient`, on failures only) and
  `horopter_fold_out_of_order` — whether the event arrived with an id below the latest one
  already folded into its change. It answers whether ingress is accepting, deduplicating or
  rejecting events, without tracing turned on. `horopter_fold_out_of_order="true"` is the
  out-of-order rate, the measurement the projection re-fold decision is waiting on.
  `horopter_append_dedupe_path="unique_violation"` counts concurrent retries of one
  idempotency key racing each other, as distinct from a retry that lands after the first
  attempt has already committed.
- HTTP server request durations and sizes, by method, route and status code. Ingress answers
  each append outcome with its own status code — 201, 200, 400 or 503 — so these split
  appends by outcome too.
- Database operation durations and errors, and `pgxpool_*` connection-pool statistics.
- Go runtime and process metrics: goroutines, memory, garbage collection, open file
  descriptors.

**No metric carries a tenant, a ref, an event id or any other value a caller chooses.** Every
label comes from a fixed set, so no caller can create series without limit by sending
requests. Per-request detail, tenant included, is on telemetry spans, where it can be sliced
by anything.

Two cautions. `OTEL_RESOURCE_ATTRIBUTES` appears on `/metrics` as the `target_info` series,
visible to anyone who can reach the listener, so put nothing sensitive in it. And a backend
should not both scrape `/metrics` and receive OTLP metrics from the same process, or every
series is stored twice.

### When telemetry fails

**A metrics listener that cannot start stops the daemon.** A port that is not an integer from
1 to 65535, a host that does not resolve, or an address already in use makes the daemon exit
with an error naming the variable and its value. The endpoint is on by default, so one that
was silently missing would look like a working configuration; a daemon that refuses to start
loses no events, because during a rolling deploy the old instances keep serving.

**Anything that depends on a collector never stops a daemon.** If the tracer provider or the
OTLP metric exporter cannot be built, the binary logs a single `ERROR` line naming the service
and the cause, then carries on without it. Losing change events is the worst failure this
system has, and exported telemetry is secondary. Watch for that line: it is the only signal
that export is configured but not running.

**Everything OpenTelemetry reports about itself is logged through the same structured logger
as the rest of the binary.** Two cases are worth knowing because neither stops the process:

- **A malformed `OTEL_EXPORTER_OTLP_*` value** — an unparsable endpoint, an unreadable
  certificate file, malformed headers, a non-numeric timeout, an unknown protocol. The SDK
  does not fail on any of these. It logs the problem at `ERROR` and falls back to its own
  defaults, so the exporter comes up pointed somewhere you did not intend. **That log line is
  the only signal**, and it names the value it could not parse; treat one at startup as a
  broken collector configuration rather than as noise.
- **Export failures at run time** — a collector that is down or rejecting data — are logged at
  `WARN`. The exporter retries with backoff; once it gives up, those spans are gone for good,
  and spans buffered during a collector outage do not come back when it recovers. Metric
  totals survive, because with the default cumulative temporality every push carries running
  totals.

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

## CloudEvents: reserved data keys

`POST /v1/events/cloudevents` takes a CloudEvents envelope. CloudEvents carries an
instant but no duration, lifecycle, or tenancy, so four fields that `POST /v1/events`
sends as first-class request fields travel in the `data` payload instead:

| data key | becomes | if absent |
|---|---|---|
| `tenant` | the event's tenant | `default` |
| `correlation_ref` | the ref two events share to describe one interval | the event stands alone as a point |
| `title` | the change title | falls back to the event type |
| `actor` | who performed the change | empty |

These four keys are **reserved**: each is lifted into its field and removed from the
attribute map, so it is not also stored as a generic attribute. Every other key in `data`
is kept as an attribute untouched.

A reserved key holding a non-string value is left alone rather than coerced — `{"title":
7}` stays an attribute and the title falls back as though it were absent.

The envelope's own `time` supplies `occurred_at`. It is optional in CloudEvents; when it
is missing, receipt time is used, since that is the earliest moment the system can attest
to.

## Attributes

Attribute keys follow OpenTelemetry semantic conventions rather than an invented
vocabulary. It costs nothing and buys legibility, plus mechanical mapping into any
OTel-aware tool. The keys a producer should aim for:

- `service.name`, `service.version`
- `deployment.environment.name`
- `cicd.pipeline.name`, `cicd.pipeline.result`, `cicd.pipeline.task.name`,
  `cicd.pipeline.task.run.id`, `cicd.pipeline.task.run.result`
- `cloudevents.event_id`, `.event_source`, `.event_type`, `.event_subject`

Nothing rejects an attribute outside this list — it is a convention, not a schema.

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
