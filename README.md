# Horopter

**horopter** (/həˈrɒptər/, hor-**OP**-ter) — in binocular vision, the locus of
points that fall on corresponding retinal points in both eyes: where two
independent views agree and fuse into one.

One eye gives a flat image; two give depth. Metrics alone are flat. Metrics plus
change events give causal depth.

A vendor-neutral store for change events. Humans and machines write change
events in; observability tools read them out to enrich graphs.

## Status

v0.1.0. The 0.1 HTTP API and storage schema are settled — a change that breaks
either gets a new minor or major, not a patch inside the line.

Every non-probe route on both `horopter-ingress` and `horopter-egress` now requires an
issued credential; read [Security](#security) for what each role grants before putting
either daemon anywhere its port can be reached.

## Quick start

Horopter is published as four container images — `horopter-ingress`, `horopter-egress`,
`horoctl-migrate` and `horoctl` — and a compose file that runs them. A release carries a
`docker-compose.yaml` release asset with that release's image tags already rendered in:

    curl -O https://github.com/horopter-dev/horopter/releases/download/v0.1.0/docker-compose.yaml
    docker compose up -d

**Packages stay private while the repository is**, so pulling needs a `docker login` first
(below) from someone who can read the repository. Both become public when the project is
ready to be read by strangers, which no version number triggers.

To work on Horopter today, clone it and run the development stack:

    task up:local    # builds images from the working tree with goreleaser release --snapshot
    task seed        # fills the running stack with changes in all three shapes

`task up:local` builds from the working tree; `task up` pulls the tag `docker-compose.yaml`
names instead of building locally:

    gh auth refresh -s read:packages
    gh auth token | docker login ghcr.io -u "$(gh api user --jq .login)" --password-stdin
    task up

`task stop` halts the stack and `task down` removes its containers, both keeping the
database; `task down:wipe` also **destroys the database volume**.

The stack starts with no credentials of its own. After `horoctl-migrate` applies the schema,
a `bootstrap` service runs `horoctl admin bootstrap`, which creates a `default` tenant if
absent and issues two fixed development credentials if their key prefixes are absent — one
`producer` (`append` only) and one `reader` (`read` only), never a `superuser`, which also
grants `issue` and would otherwise put a non-expiring, credential-minting key in `.env`.
`bootstrap` is idempotent, so running `task up` twice does not error. It reconciles rather
than skipping: a dev key left bound to an earlier tenant or role is rebound, and the line it
prints on stderr names what moved. The one thing it will not undo is a revocation — it stops
and names the credential instead. Export the two dev keys
to follow along below (they are also `docker-compose.yaml`'s and `hack/seed.sh`'s own
defaults, so `task seed` needs neither set):

    export HOROPTER_DEV_PRODUCER_KEY=horo_dead00000001_00000000000000000000000000000000000000000000000000000000000000
    export HOROPTER_DEV_READER_KEY=horo_dead00000002_11111111111111111111111111111111111111111111111111111111111111

Then post an event and read it back:

    curl -X POST localhost:8080/v1/events \
      -H "Authorization: Bearer $HOROPTER_DEV_PRODUCER_KEY" -H 'Content-Type: application/json' \
      -d '{"source":"ci","type":"deployment.started","subject":"acme/prod",
           "occurred_at":"2026-09-14T10:00:00Z","correlation_ref":"dep-1"}'
    curl -H "Authorization: Bearer $HOROPTER_DEV_READER_KEY" \
      "localhost:8081/adapters/grafana/annotations?from=1789376400000&to=1789383600000"

The compose file pulls `ghcr.io/horopter-dev/{horopter-ingress,horopter-egress,horoctl-migrate,horoctl}`
at the tag it names, runs `horoctl-migrate` — the only standalone binary permitted to apply
schema DDL — to completion, then `bootstrap` (above), before starting `horopter-ingress` on
port 8080 and `horopter-egress` on port 8081.

Each service is configured with environment variables:

- `HOROPTER_IMAGE_TAG` — the published tag the repository's `docker-compose.yaml` runs;
  defaults to the version committed in that file. `task up:local` ignores it, since it builds
  images from the working tree instead of pulling one.
- `DATABASE_URL` — PostgreSQL connection string, required by all four binaries.
- `HOROPTER_DEV_PRODUCER_KEY`, `HOROPTER_DEV_READER_KEY` — the two credentials `bootstrap`
  issues; see [Issuing credentials](#issuing-credentials) below for keys beyond the two fixed
  dev ones.
- `HOROPTER_ADDR` — listen address for `horopter-ingress` and `horopter-egress`
  (defaults to `0.0.0.0:8080` and `0.0.0.0:8081` respectively).
- `HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES` — the largest attribute map
  `horopter-ingress` accepts, in bytes of JSON: default `4096`, from `1` to `1048576`. See
  Attributes below.
- `HOROPTER_EGRESS_READ_MEMORY_BYTES` — the memory pool `horopter-egress` reserves reads
  against: default `268435456` (256 MiB), at least `1048576`.
- `HOROPTER_EGRESS_ADMISSION_WAIT` — how long a read waits at each admission gate before it
  is shed: default `500ms`, above 0 and at most `10s`.
- `HOROPTER_EGRESS_DB_WAIT_THRESHOLD` — the database connection wait above which a read
  counts as overloaded: default `100ms`, above 0. See Read admission below.
- `HOROPTER_LOG_LEVEL` — the minimum level to log: `debug`, `info`, `warn` or `error`,
  case-insensitive. Default `info`. See Logs below.

A value that does not parse, or is out of range, stops the daemon at startup with an error
naming the variable and the value.

### Issuing credentials

`horoctl` issues, lists and revokes credentials directly against `DATABASE_URL` — no daemon
needs to be running, and no credential of `horoctl`'s own is required, since authority here
comes from database access, not from a presented key:

    horoctl admin tenant create --dsn "$DATABASE_URL" --display-name acme
    horoctl admin keys issue --dsn "$DATABASE_URL" --role reader --tenant <tenant-id> \
      --source github --locus github

`--dsn` defaults to `DATABASE_URL` from the environment, so it can be omitted when that is
already set — for the repository's own `docker-compose.yaml`,
`postgres://horopter:horopter@localhost:5432/horopter?sslmode=disable`, since `docker-compose.yaml`
publishes Postgres on that port.
`admin tenant create` prints the new tenant's opaque id to stdout; `admin keys issue` prints
the full key to stdout **once** — it cannot be recovered afterwards — and everything else
(the prefix, role, tenant and locus it issued) to stderr.

Four roles are seeded: `producer` (`append`), `reader` (`read`), `tenant_admin` (`read`,
`admin`) and `superuser` (`append`, `read`, `admin`, `issue`) — see
[Security](#security) for what each capability gates. They are rows in a `role` table rather
than a list in code, so the vocabulary can grow without a migration; `keys issue` reads a
role's capabilities from that table, which is the same fact the daemons check at request
time, so a role added later is refused or allowed identically at both ends. `--tenant` scopes a
credential to one tenant; omit it for a global credential valid for any tenant named with
`?tenant=`. `admin keys list [--tenant <id>]` and `admin keys revoke --prefix <prefix>`
manage what already exists.

`--key <value>` on `keys issue`, or `--producer-key`/`--reader-key` on `admin bootstrap`,
sets an explicit key instead of generating one — development only, and refused below 32
bytes or outside the `horo_<12-hex-char-prefix>_<secret>` shape a presented credential is
actually parsed as, since anything else would store a credential that can never
authenticate.

## Security

**Every non-probe route on both daemons now requires an issued credential.** `horopter-ingress`
gates `POST /v1/events` and `POST /v1/events/cloudevents` on the `append` capability;
`horopter-egress` gates its four routes — `GET /v1/changes`, `GET /v1/events`,
`GET /adapters/grafana/annotations` and `GET /adapters/otlp/spans` — on `read`. One change is
fetched by its stored ref with `GET /v1/changes?ref=<locus>/<ref>`, for which `from` and `to`
are not required; `GET /adapters/grafana/annotations` answers `400` to a `ref`, since it
renders what overlaps a window and a primary-key lookup ignores one. `/healthz`
and `/readyz` stay open on both, so an orchestrator's probes need no credential. A request
with no credential, or one that is malformed, unknown, revoked or expired, is answered `401`
with an identical body regardless of which — telling those apart would let a caller probe
for a valid prefix. A credential presented and accepted, but lacking the route's capability
(a `reader` key on `POST /v1/events`, say), is answered `403`.

A credential is scoped to one tenant, or to none. `POST /v1/events` takes no tenant field at
all — a producer's writes land in its own credential's tenant, and a request body naming one
is answered `400` — while every `horopter-egress` route reads `?tenant=` as a selector: a
tenant-scoped credential may name only its own tenant there (`403` for any other), and a
global credential must name one that exists (`400` naming none, `404` naming one that does
not exist). A credential is issued as a role, and a role is a stored set of capabilities: the
four seeded are `producer` (`append`), `reader` (`read`), `tenant_admin` (`read`, `admin`) and
`superuser` (`append`, `read`, `admin`, `issue` — the only role that can mint further
credentials). An `admin` grant cannot be narrowed by `--subject-pattern`, because a rebuild is
whole-tenant by construction; that is refused at issue time against the role's stored
capabilities, so it holds for a role added after this list was written. See
[Issuing credentials](#issuing-credentials).

**No compose file in this repository ever issues a `superuser` credential.** `bootstrap`
issues a `producer` and a `reader` only — the two dev keys have no `read`+`append` overlap,
which is what makes `POST /v1/events` with the reader key `403` rather than succeeding, and
neither can issue a further credential. A credential's revocation (`horoctl admin keys
revoke --prefix <prefix>`) is visible to new requests within its cache TTL (30s by default),
not instantly.

A read that fails is answered `500` with a generic body, and the underlying error — which
can name tables, constraints and the database host — is written to the daemon's log
instead, so a failure does not hand internal detail to that same caller.

A read refused because egress is at capacity is answered `503`, and one too large to serve at
all `422`. Neither body carries anything from the store: the `422` names only the configured
memory limit. That admission control limits what egress spends on reads — database connections
and memory. It is not protection against a flood of requests, which belongs in front of the
port, in a WAF or equivalent.

**`docker-compose.yaml` publishes both API ports (8080, 8081) and Postgres itself (5432) on
the host, with the username and password both `horopter`.** The API ports being reachable is
no longer an exposure by itself — every route still needs a credential — but direct Postgres
access is direct read and write access to the event log and the credential table, past every
check either daemon or `horoctl` makes, for anyone who can reach the port. The compose file
is a local development stack and nothing else: do not run it anywhere its ports are reachable
from outside the machine, and do not reuse its credentials.

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
  or `horoctl-migrate` — the same default whether an operator ran `horoctl-migrate` or
  `horoctl migrate`, since both apply the identical migration.
- `OTEL_RESOURCE_ATTRIBUTES` — comma-separated `key=value` pairs merged onto the resource
  every span and metric carries, e.g. `deployment.environment.name=prod,team=platform`.
  **This one is different from the others**: Horopter's own configuration code never reads
  it. It works because building the resource calls the OpenTelemetry SDK's
  `resource.Default()`, and that function's own environment detector reads this variable —
  the same thing it would do for any Go program built on this SDK, not something Horopter
  added. It is genuinely honoured, but it is upstream behaviour Horopter inherits rather than
  configures, and no test in this project exercises it — a future SDK upgrade could change or
  drop it with nothing here to notice.

### Logs

All three binaries log JSON to stderr, one object per line, with the fields each call site
supplies — `route`, `tenant`, `waited_ms` and the rest. Pipe them through `jq` when reading
them by hand: `docker compose logs --no-log-prefix -f egress | jq -R 'fromjson? // .'`
(`--no-log-prefix` matters: without it, compose prepends the service name to each line, and
that no longer parses as JSON).

- `HOROPTER_LOG_LEVEL` — the minimum level to log: `debug`, `info`, `warn` or `error`,
  case-insensitive. Default `info`. A value that does not parse stops the daemon at startup.

A line logged while a span is active also carries `trace_id` and `span_id`, which is what links
it to a trace in a backend such as Tempo. Every read-admission line has them **when tracing is
on** — `read shed`, `read too large` and the halved-limit warning — as do the ingress request
lines. With tracing at its default off, none of them do: there is no span to attach to. Startup
and telemetry-failure lines never carry them, tracing on or off — they run before any span
exists.

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
  - `store` — the spans the store package creates around each store-level operation: append an
    event, list events, list changes, estimate a read's size, and rebuild a tenant's
    projection. Disabling it on `horoctl-migrate` silences the rebuild span, which is the one
    an operator running `--rebuild-tenant` most wants.
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

Every read `horopter-egress` gates carries what admission decided on its server span:
`horopter.admission.outcome` (`admitted`, `shed_concurrency`, `shed_memory` or `too_large`) and
`horopter.admission.wait_ms`; once past the concurrency gate, `horopter.db.connection_wait_ms`,
the longest it waited for a database connection; once estimated,
`horopter.read.estimated_bytes`; and once memory is reserved, `horopter.read.reserved_bytes`
and `horopter.read.byte_cut`.

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

`horoctl-migrate` (and `horoctl migrate`) runs once and exits, so nothing could scrape it and
it serves no `/metrics`. With a collector configured, its metrics go out over OTLP as it
finishes.

The host and port defaults are the OpenTelemetry specification's, which has two consequences:

- **Inside a container, `localhost` is reachable only from the same network namespace.** A
  collector sidecar in the same pod can scrape it as shipped; anything scraping over the
  network needs `OTEL_EXPORTER_PROMETHEUS_HOST=0.0.0.0`. The images have no shell or `curl`,
  so `docker exec` cannot reach it either. This applies to the **repository's**
  `docker-compose.yaml` (`task up`/`task up:local`): it sets the host and publishes the ports
  on the host's loopback — `curl localhost:9464/metrics` for ingress, `curl
  localhost:9465/metrics` for egress. The released standalone `docker-compose.yaml`
  (`deploy/docker-compose.yaml`) publishes neither the Postgres port nor these metrics ports.
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
- `horopter_read_admission_limit` and `horopter_read_admission_in_flight` — `horopter-egress`'s
  read concurrency limit, and the reads holding a place under it;
  `horopter_read_memory_reserved_bytes` and `horopter_read_memory_capacity_bytes` — its read
  memory pool; and `horopter_read_shed_total`, labelled with `horopter_read_shed_gate`
  (`concurrency` or `memory`) — reads refused with `503`. See Read admission.
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

`horoctl-migrate` and `horoctl`'s compiled-in `migrate` verb — the same `internal/migrate.Run`
behind both, so neither can drift from the other — are the only ways schema DDL is applied.
Run one to completion against the database before starting any daemon at the new version;
each daemon asserts the recorded schema version at startup and refuses to run against one it
does not understand, so a skewed rollout fails loudly rather than corrupting data quietly.

### Rebuilding a tenant's projection

    horoctl-migrate --rebuild-tenant=<tenant-id>

The `change` table is a derived view of the immutable `event` log, so it can be discarded and
replayed. This runs only after the schema-version assertion passes. What makes that true is
that a change's ref is derived from its events via `core.RefFor` — `correlation_ref` for a
correlated change, `<locus>/<event id>` for a point — so any migration that rewrites one side
of that derivation has to rewrite the other, or the projection stops being a function of the
log. `0004` rewrites `event.correlation_ref` and `change.ref` together for correlated changes,
which is why refs that predate it carry `legacy/` on **both** sides; `0006` does the analogous
rewrite for point changes, backfilling `event.locus` and prefixing the point changes `0004`
left bare.

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
`title`, `actor`, `subject`, `source` and attributes — can come back different. `status`
and `end` follow the narrower rule "last *terminal* event wins", so a
`deployment.completed` and a `deployment.failed` on one ref can swap, but nothing that
arrives later can reopen a change that has already closed. `start`,
`started_at` and `first_seen_at` cannot: they are minima, so replay order does not reach
them. Rebuilding twice gives the same answer both times — it is the live projection the
replay may disagree with, not itself. If a change's outcome matters to you, read `GET
/v1/events?ref=<stored-ref>` (the log is immutable and is never reordered) before and after.
`<stored-ref>` is the ref as stored, which is what `GET /v1/changes` returns — `<locus>/<ref>`,
where the locus is `legacy` for anything the locus migrations could not recover (see "Refs
written before the locus migrations", below) and the appending credential's locus otherwise.

### Refs written before the locus migrations carry a `legacy/` prefix

Every ref is stored as `<locus>/<ref>` within a tenant, where the locus comes from the
appending credential. Neither change shape carried one from the start: a correlated ref
predating `0004` and a point ref predating `0006` were both appended without one, so each
migration assigns the locus `legacy` to whatever it cannot recover. A `deploy-42` posted
under the old model reads back as `legacy/deploy-42`, in `GET /v1/changes` and in
`GET /v1/events?ref=`. A point change is no different — `0006` backfills `event.locus` from
a correlated ref's own prefix where one exists, and falls back to `legacy` for a point
event, which never carried one to recover, before prefixing the point changes `0004` left
bare.

The prefix is permanent and is not cosmetic damage: those refs genuinely predate the locus
model, and a bare `github/deploy-1` written before `0004` — or a bare point ref written
before `0006` — would otherwise be indistinguishable from one a `github` credential wrote
after it. Queries by ref need the stored form, so a dashboard or script carrying a
pre-`0004` or pre-`0006` ref has to be updated to the prefixed one.

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

### v0.1 pre-release: event attributes are capped at 4 KiB

`horopter-ingress` now refuses, with `400`, an event whose attributes encode to more than 4096
bytes of JSON. A producer sending larger attribute maps has those events refused, and a producer
that does not act on a `400` loses them. Shrink the attributes — link to large text rather than
embedding it — or raise `HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES`, up to `1048576`. Events
already stored are not re-checked.

### v0.1 pre-release: the images moved to the horopter-dev organisation

The repository moved from `yo61` to `horopter-dev`, and the images moved with it — to
`ghcr.io/horopter-dev/horopter-{ingress,egress,migrate}`. GitHub does not move GHCR packages
with a repository, so **v0.1.0-dev.1's images stay at `ghcr.io/yo61/horopter-*`**, and the
`docker-compose.yaml` asset that release carries names an address this project no longer
controls. That asset cannot be rewritten, only superseded: take v0.1.0-dev.2's instead, or
re-pull this repository's `docker-compose.yaml`.

Nothing in the database changes. An operator on v0.1.0-dev.1 needs new image references and
a `docker login` that can read the new organisation's packages; the schema version is
untouched.

## Truncated responses

Every read endpoint caps its result set at 1000 rows. When rows were discarded, the
response carries **`X-Horopter-Truncated: true`**; when nothing was discarded the header
is absent entirely. All four read routes set it:

- `GET /v1/changes`
- `GET /v1/events`
- `GET /adapters/grafana/annotations`
- `GET /adapters/otlp/spans`

The header is also set when a read was larger than egress's read memory pool and was cut to the
oldest rows that fit; see Read admission. The same advice applies.

Act on it. Changes are returned oldest first, so truncation discards the **most recent**
rows — during an incident, exactly the ones being looked for — and a truncated response
is otherwise indistinguishable from a complete one. Narrow the time window, or filter by
`subject`, `type`, `status`, `locus`, `source` or `attr.deployment.environment.name`, and
repeat the query until the header is gone. A `limit` query parameter below 1000 asks for
fewer rows; it cannot raise the cap.

The signal is a header rather than a field in the body because two of these routes render
vendor-owned shapes — Grafana annotations and OTLP spans — that cannot carry a Horopter
envelope.

## Filtering changes

`GET /v1/changes` and both adapter routes take the same filters, all exact equality, all
optional, all combining with AND:

| parameter | matches |
|---|---|
| `subject` | the change's subject |
| `type` | the change's type |
| `status` | the change's status |
| `locus` | the namespace the ref was issued under — the part of a stored ref before its `/` |
| `source` | the system that reported the change, from the last event folded into it |
| `attr.<key>` | an attribute's exact value |

An empty value is no filter, so `?status=` and an omitted `status` mean the same thing. That
holds for an allowlisted `attr.<key>` too. It does **not** make an unfilterable key
acceptable: `?attr.service.version=` is refused exactly as `?attr.service.version=2.5.0` is,
because what is wrong there is the key rather than the filter. Accepting the empty one and
refusing the other would make a single mistake behave two ways depending on whether the
caller filled the box in.

**Attribute keys are allowlisted.** Only `deployment.environment.name` is filterable today.
Any other `attr.` key is answered **400** naming the key and listing what is permitted,
rather than being ignored — a filter that silently matches everything answers a narrowing
question with the wrong result set. Each filterable key needs its own expression index, so
adding one is a migration rather than a configuration change; that cost is deliberate,
because a key that becomes filterable is one callers may depend on afterwards.

`source` is the **last** event's source, not the set of every source that contributed. A ref
whose events come from two systems reports one, and is found by filtering on that one only.

## Read admission

`horopter-egress` serves a read only when it has the capacity to. Every read route passes two
gates; `/healthz` and `/readyz` pass neither, so probes answer normally while reads are refused.

1. **A concurrency limit.** It starts at the size of egress's database connection pool and
   adapts. A read that waited longer than `HOROPTER_EGRESS_DB_WAIT_THRESHOLD` for a connection,
   or failed to get one, halves it; every other read raises it by one, up to four times the
   pool's size.
2. **A memory pool** of `HOROPTER_EGRESS_READ_MEMORY_BYTES`. Before a read runs, egress asks
   Postgres how large its rows are, and reserves a multiple of that from the pool until the
   response has been written.

A read that cannot pass a gate within `HOROPTER_EGRESS_ADMISSION_WAIT` is answered **`503` with
`Retry-After`**: the wait, rounded up to whole seconds. Retry after that interval.

A read larger than the whole pool is served as far as the pool allows — the oldest rows that
fit, with `X-Horopter-Truncated: true` — exactly as for the row cap.

A read whose **first row alone** needs more than the pool is answered **`422`**, naming the
limit. Narrowing the time window does not help when that row is a long-running change that
overlaps every window: narrowing to a subject, type or status that the oversized row does not
have, or (on `/v1/events`) to a `ref`, avoids it; otherwise raise
`HOROPTER_EGRESS_READ_MEMORY_BYTES`.

| Variable | Default | Valid values |
|---|---|---|
| `HOROPTER_EGRESS_READ_MEMORY_BYTES` | `268435456` (256 MiB) | whole number, at least `1048576` |
| `HOROPTER_EGRESS_ADMISSION_WAIT` | `500ms` | Go duration, above 0 and at most `10s` |
| `HOROPTER_EGRESS_DB_WAIT_THRESHOLD` | `100ms` | Go duration, above 0 |

**Sizing.** Set `HOROPTER_EGRESS_READ_MEMORY_BYTES` to about half the memory available to
egress: at the default `GOGC`, the Go heap grows to twice its live size before it is collected.
The concurrency limit follows the pool, which is `max(4, NumCPU)` connections unless
`DATABASE_URL` sets `pool_max_conns`. Each replica has its own limit and pool, and they do not
coordinate, so size the replicas' pools together within Postgres's `max_connections`. A read
waits at most twice `HOROPTER_EGRESS_ADMISSION_WAIT`, once at each gate, and that comes out of
egress's 30-second write timeout, so keep the wait short.

The connection wait egress measures, and compares against `HOROPTER_EGRESS_DB_WAIT_THRESHOLD`,
covers the whole pool acquisition, not just time queued behind other reads: it also includes
opening a new connection when the pool has none idle, and pgxpool's liveness ping of a
connection that has been idle for over a second. Set `HOROPTER_EGRESS_DB_WAIT_THRESHOLD`
comfortably above the time to open a connection to the database plus a round trip, or that
ordinary setup cost reads as congestion and halves the concurrency limit for no reason.

**Worth alerting on:**

- `horopter_read_shed_total`, by `horopter_read_shed_gate`: any sustained rate means reads are
  being refused.
- `read shed` and `read too large` log lines, at `WARN`: one per refused read, naming the route,
  the tenant and the gate's figures.
- `read concurrency limit halved …` at `WARN`: each time reads found the connection pool
  congested.
- `horopter_read_admission_in_flight` against `horopter_read_admission_limit`, and
  `horopter_read_memory_reserved_bytes` against `horopter_read_memory_capacity_bytes`: how
  close to each gate egress is running.
- Reads waiting at the memory gate hold their concurrency permit, so once the memory pool is
  full, later reads are shed at the concurrency gate instead. When concurrency sheds rise,
  compare `horopter_read_memory_reserved_bytes` with `horopter_read_memory_capacity_bytes` too.

Admission control protects the database and egress's memory. It is not protection against a
flood of requests; put that in front of Horopter.

### Seeing it work

`hack/load.sh [concurrency] [iterations]` (also `task load -- [concurrency] [iterations]`)
drives concurrent reads at every rendering and prints the status codes it saw. Against a
healthy local database it will not shed: shedding needs connection waits above
`HOROPTER_EGRESS_DB_WAIT_THRESHOLD`, and an idle database has none. Two settings make the
interesting paths reachable:

- `pool_max_conns=1` appended to `DATABASE_URL` makes reads queue for the one connection.
  Measured: `hack/load.sh 20 50` and `hack/load.sh 60 100` shed nothing — a seeded query
  takes a few milliseconds, so the queue drains inside `HOROPTER_EGRESS_ADMISSION_WAIT`
  (`500ms` by default). `hack/load.sh 150 200` did shed, all at the concurrency gate, with
  `"limit":4` in the `read shed` line — the ceiling for a pool of one connection.
- `HOROPTER_EGRESS_READ_MEMORY_BYTES=1048576`, the minimum, makes ordinary seeded reads
  larger than the pool, so they are cut — `200` with `X-Horopter-Truncated: true`, and **no
  log line**: only the `422` and `503` paths log a `WARN` (see Logs above). A `422` needs a
  single row whose text, times the 46x admission overhead, exceeds the pool by itself —
  roughly 23 KB of row text, well past anything `hack/seed.sh` writes.

## CloudEvents: reserved data keys

`POST /v1/events/cloudevents` takes a CloudEvents envelope. CloudEvents carries an
instant but no duration or lifecycle, so three fields that `POST /v1/events` sends as
first-class request fields travel in the `data` payload instead:

| data key | becomes | if absent |
|---|---|---|
| `correlation_ref` | the ref two events share to describe one interval | the event stands alone as a point |
| `title` | the change title | falls back to the event type |
| `actor` | who performed the change | empty |

These three keys are **reserved**: each is lifted into its field and removed from the
attribute map, so it is not also stored as a generic attribute. Every other key in `data`
is kept as an attribute untouched.

A reserved key holding a non-string value is left alone rather than coerced — `{"title":
7}` stays an attribute and the title falls back as though it were absent.

CloudEvents carries no tenancy either, but `tenant` is not a fourth reserved key: it is
derived from the credential, the same as on `POST /v1/events`, and a `data.tenant` of any
kind — including `null` — is rejected with `400` rather than read.

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

Nothing rejects an attribute outside this list — it is a convention, not a schema. What is
limited is size: `horopter-ingress` refuses, with `400`, an event whose attributes encode to more
than `HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES` (default 4 KiB) of compact JSON, measured
without HTML escaping:

    invalid event "01J8Z3Q4R5S6T7V8W9X0Y1Z2A3": attributes encode to 5120 bytes of JSON;
    the limit is 4096 — keep attributes to identifiers and short text, and link to anything
    larger

(one line, wrapped here). The keys above, fully populated, come to about 560 bytes, and a
40-line release note as `horopter.description` to about 3.2 KiB.

One key is a Horopter-specific extension, not an OTel convention, because nothing in
that vocabulary covers it: **`horopter.description`**. The Grafana annotations adapter
(`GET /adapters/grafana/annotations`) reads it as the annotation body text. It is
namespaced under `horopter.` the same way the OTLP adapter's own `horopter.subject`,
`horopter.ref` and `horopter.status` attributes are, to keep it distinct from a
producer's own attributes — a bare, unnamespaced `description` attribute is not read for
this purpose.

## Licence

Apache License 2.0 — `SPDX-License-Identifier: Apache-2.0`. [`LICENSE`](LICENSE)
holds the canonical text, unmodified.

The licence conveys the code and nothing else. Section 6 withholds trademark
rights explicitly: the name Horopter, the `horopter-dev` organisation and the
project's domains are not granted by it.

Contributions are accepted under the same licence, certified by a `Signed-off-by`
line per the Developer Certificate of Origin. There is no CLA. See
[`CONTRIBUTING.md`](CONTRIBUTING.md).
