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
`horopter-migrate` and `horoctl` — and a compose file that runs them. A fifth,
`horopter-flux`, runs in a Kubernetes cluster beside Flux ([Flux](#flux)). A release carries a
`docker-compose.yaml` release asset with that release's image tags already rendered in:

    curl -O https://github.com/horopter-dev/horopter/releases/download/v0.1.0/docker-compose.yaml
    docker compose up -d

`docker compose --profile grafana up -d` adds Prometheus on `127.0.0.1:9090` and Grafana on
`127.0.0.1:3000`, a local demo: Grafana lets anyone who reaches it in as a Viewer, installs the
Infinity plugin from grafana.com at start, and opens on a dashboard of both daemons' request
rates with Horopter's changes as annotations. It needs Compose 2.23.1 or later.

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

The stack starts with no credentials of its own. After `horopter-migrate` applies the schema,
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
      -d '{"type":"deployment.started","subject":"acme/prod",
           "occurred_at":"2026-09-14T10:00:00Z","correlation_ref":"dep-1"}'
    curl -H "Authorization: Bearer $HOROPTER_DEV_READER_KEY" \
      "localhost:8081/adapters/grafana/annotations?from=1789376400000&to=1789383600000"

The compose file pulls `ghcr.io/horopter-dev/{horopter-ingress,horopter-egress,horopter-migrate,horoctl}`
at the tag it names, runs `horopter-migrate` — the only standalone binary permitted to apply
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
- `HOROPTER_INGRESS_ALLOW_LOCUS_PREFIXED_REF` — accept a `correlation_ref` whose first
  segment is the credential's own locus: default `false`, which refuses it with `400`. See
  Filtering changes' sibling note under the append response below.
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

## horo

`horo` explores what Horopter recorded, and records changes (see [Recording](#recording)).
Exploring reads egress with a reader key:

    export HOROPTER_EGRESS_URL=http://localhost:8081
    export HOROPTER_EGRESS_KEY=<reader key>
    horo changes                         # the last 24 hours
    horo changes --since 168h --subject acme/prod --status failed
    horo changes --from 2026-09-14T00:00:00Z --to 2026-09-15T00:00:00Z --output json
    horo show ci/dep-1                   # one change and all its events

`horo changes` follows egress's `next` links, so it returns every matching change however many
there are; `--limit` caps it. Output is a table on a terminal and one JSON object per line
otherwise; `--output table|json|jsonl` chooses. Exit codes: `0` success, `1` an error from
egress, `2` a usage error, `3` `horo show` found no such ref.
`horo refold` repairs one change; see [Re-folding one change](#re-folding-one-change).

Every binary, `horo`, `horoctl` and the daemons alike, answers `--version` with the release it
was built from (`horo v0.9.0`) and `--build-info` with that release, the commit, the commit's
time, whether the tree was modified, the Go version, OS and architecture. Either must be the
first argument, and both take `--json` or `--format text|json`. Only text output prefixes the
version with `v`: `--json` reports `"version": "0.9.0"`, the form `horoctl` passes its plugins
as `HOROCTL_VERSION`, and `--version --format bare` prints `0.9.0` alone, for a script to
compare. A binary built by `go install …@v0.9.0` reports the module version.

Install from a release archive, or, while the repository is private:

    GOPRIVATE=horopter.dev go install horopter.dev/horopter/cmd/horo@latest

### Recording

`horo run` records a command's start and how it ended, and `horo event` records one event.
Both post to ingress with a producer key:

    export HOROPTER_INGRESS_URL=http://localhost:8080
    export HOROPTER_INGRESS_KEY=<producer key>
    horo run --type deployment --subject acme/prod --title "Deploy api" -- ./deploy.sh
    horo event --type incident.resolved --subject acme/prod --title "Queue drained"

`horo run` posts `<type>.started`, runs the command with its output untouched, then posts
`<type>.completed`, `.failed` (with `exit_code`) or `.cancelled` (with `signal`), and exits with
the command's own code — `128 + n` when a signal ended it, `127` when it could not start.
`SIGTERM` is passed on to the command, and so is `SIGINT` unless `horo` runs in the foreground
of a terminal: there Ctrl-C already reaches the command, and a second interrupt would cut short
tools, such as Terraform, that shut down gracefully on the first. To stop a wrapped command from
another shell, send `horo` `SIGTERM`. A Ctrl-C or `SIGTERM` before the command starts aborts it:
the command is not run, and the change is recorded as cancelled.

**Recording never blocks the work.** If Horopter cannot be reached, or no URL or key is set,
`horo run` warns and runs the command anyway. It waits at most 10 seconds for the start, and if
the start was not recorded it does not record the end either. If Horopter fails while the
command runs, it retries the end for up to 30 seconds after the command; Ctrl-C stops that wait.
`--strict` refuses to run the command when the start cannot be recorded, and fails when the end
cannot.

It prints both forms of the change's ref on stderr: the stored `<locus>/<ref>`, which `horo show`
takes, and the plain ref, which `--ref` takes. If the end cannot be confirmed, `horo run` prints
the `horo event` command that closes the change. It carries the time the command ended and the
end's own idempotency key, so running it is safe even if the end did arrive.

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
  or `horopter-migrate` — the same default whether an operator ran `horopter-migrate` or
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

**Every request writes one `request` line at `info`**, except `GET /healthz` and
`GET /readyz`: `method`, `route` (the matched pattern, `unmatched` for a 404 or 405), `status`,
`duration_ms` and `bytes`; `tenant` and `credential` (the key prefix) once a credential
resolves; `error`, the message the client received, when a 4xx or 5xx answer is a JSON error
object, as every Horopter error is. A 404 or 405 from an unmatched path, and a 500 from a
panic, have no such body and log no `error`. Ingress adds `subject`,
`type`, `ref` and, when the event has one, `idempotency_key`. Egress adds the `query` string of
an authenticated read. Every string is cut at 1 KiB, with `<field>_truncated: true` when it
was. Keys, request bodies, event attributes and client addresses are never logged.

To follow the request lines:

    docker compose logs --no-log-prefix -f ingress | jq -R 'fromjson? | select(.msg == "request")'

A line logged while a span is active also carries `trace_id` and `span_id`, which is what links
it to a trace in a backend such as Tempo. Every read-admission line has them **when tracing is
on** — `read shed`, `read too large` and the halved-limit warning — as do the request
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
    projection. Disabling it on `horopter-migrate` silences the rebuild span, which is the one
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

`horopter-migrate` (and `horoctl migrate`) runs once and exits, so nothing could scrape it and
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
  (`deploy/docker-compose.yaml`) sets the host too but publishes neither the Postgres port
  nor these metrics ports; its `grafana` profile's Prometheus scrapes them over the stack's
  own network.
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
  rate at which an append triggers the inline per-ref re-fold that keeps the projection
  converged to id order — see "Rebuilding a tenant's projection" below.
  `horopter_append_dedupe_path="unique_violation"` counts concurrent retries of one
  idempotency key racing each other, as distinct from a retry that lands after the first
  attempt has already committed.
- `horopter_refold_total` — refs recomputed from the log because an event arrived with an
  id below the change's watermark, labelled with `horopter_refold_altered` — whether the
  recomputation moved the change's recorded outcome, Status or End.
  `horopter_refold_altered="true"` is the rate at which late arrivals are changing recorded
  outcomes, which is the number worth an alert; a re-fold that moved only Subject, Type,
  Title, Actor, Attributes or `last_event_id` is not counted here, however often it fires —
  that detail is on the append span's `horopter.refold.moved` attribute instead. The
  unlabelled total is the out-of-order rate itself. Every re-fold reads the ref's whole
  event history inside the append's own transaction, under the ref-exclusive and
  tenant-shared locks `AppendEvent` already holds, so `horopter_refold_total`'s rate
  multiplied by the affected refs' event counts is the append latency this adds — a
  long-lived, frequently-out-of-order ref pays for a full re-read on every such append, and
  holding the tenant-shared lock for longer is also more of what `RebuildProjection` waits
  out against `RebuildLockTimeout`.
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

`horopter-migrate` and `horoctl`'s compiled-in `migrate` verb — the same `internal/migrate.Run`
behind both, so neither can drift from the other — are the only ways schema DDL is applied.
Run one to completion against the database before starting any daemon at the new version;
each daemon asserts the recorded schema version at startup and refuses to run against one it
does not understand, so a skewed rollout fails loudly rather than corrupting data quietly.

Every pending migration file runs inside one transaction (`postgres.Migrate`), so a lock any
one of them takes is held until the whole batch commits, not just for its own statement.
Migration `0009` rebuilds an index on `event` — the largest table, and the only one every
append writes to. It is not the first migration since `0001` to touch `event`, and not the
heaviest: `0004` backfills `tenant` on every row via an `UPDATE ... FROM tenant`, then a
second `UPDATE` prefixes every correlated event's `correlation_ref` with `legacy/` — two
full-table passes, the second over rows the first already rewrote — under a validating
`CHECK` on `origin`; `0005` adds a foreign key (an `ACCESS EXCLUSIVE` lock plus a
validation scan); `0006` also rewrites every row (`ADD COLUMN`, two full-table `UPDATE`s
that between them cover every row exactly once, then `SET NOT NULL` and two `CHECK`
constraints); and `0008` adds a column under another `CHECK` constraint. `0004`'s two
passes touch more total rows than any of the others — `0006`'s two `UPDATE`s partition the
table without overlap, `0004`'s second one revisits what the first already rewrote — so
`0004`, not `0006`, is the comparator strictly heavier than `0009`'s index build. `0009`
gets its own note because, unlike those, it buys nothing: it costs a full index rebuild on
`event` for no measured gain (see its own comment for the `EXPLAIN` evidence), and it is
kept rather than reverted only because reverting it would cost a second rebuild. Its
`DROP INDEX` takes an `ACCESS EXCLUSIVE` lock on `event` that, because of the shared
transaction, is held for as long as the `CREATE INDEX` that follows takes to build,
blocking every append and read against `event` for the whole of that window. On a
database with a large event history, size `0009`'s downtime before running it in
production.

### Re-folding one change

A change is a fold of its events. If one is wrong — corrupted by hand, or folded under an older
rule — re-fold it from its log, with ingest running:

    export HOROPTER_INGRESS_URL=http://localhost:8080
    export HOROPTER_ADMIN_KEY=<tenant_admin or superuser key>
    horo refold ci/dep-1

or `POST /v1/admin/refold` with `{"ref": "ci/dep-1"}`; a global superuser key adds
`"tenant": "<tenant id>"`. The answer says how many events were read and which fields of the
stored change were repaired — `[]` when it was already right:

    {"ref":"ci/dep-1","events":12,"altered":["end","status"]}

It takes the same locks an append takes: appends to every other ref carry on, and one to the
same ref waits a moment. The whole-tenant rebuild below holds the tenant's lock exclusively and
halts its ingest, so prefer this for a change you can name. A key needs the `admin` capability,
held by `tenant_admin` and `superuser`; `horo refold` reads it from `HOROPTER_ADMIN_KEY` only,
never from `HOROPTER_INGRESS_KEY`. A ref with no events answers `404`, and `horo refold` exits `3`.

### Rebuilding a tenant's projection

    horopter-migrate --rebuild-tenant=<tenant-id>

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

Live appends and a rebuild now agree, from this version forward. An append whose event id
sorts below the change's current watermark re-folds that ref from the log, so the
projection equals a fold of the ref's events in id order regardless of the order they
arrived in, and a rebuild replaying the same log in the same order finds nothing left to
change.

That guarantee needs a ref that has never drifted: the watermark it checks, the change's
stored `last_event_id`, is the ref's true maximum id only if the ref has always folded in
id order. A ref whose live projection had already diverged before this version shipped can
hold a watermark below its real maximum, so a later append whose id is above the watermark
but below that maximum still takes the ordinary incremental path, and the old drift
survives it. `--rebuild-tenant` still corrects such a ref — a rebuild always replays the
whole log in id order regardless of what the stored watermark says — so it remains the
remedy for data that predates this version. "Corrects" can still mean what it always did:
for such a ref the rebuild can change the recorded Subject, Type, Title, Actor, attributes,
`last_event_id`, End or Status, so a change your dashboard shows as `completed` can read
`failed` afterward.
Read `GET /v1/events?ref=<ref>` before and after a rebuild for any ref you need to explain
that change on, the same as before this version.

This makes the system self-consistent, not causally correct: event ids are minted from
receipt time with `crypto/rand` entropy, so two events minted in the same millisecond have
an arbitrary relative order, and a producer that emits events in the wrong order is beyond
anything the store can repair.

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

### `GET /v1/changes` and `GET /v1/events` answer with an envelope

Both routes used to answer with a bare JSON array. They now answer
`{"changes": [...], "next": "..."}` and `{"events": [...], "next": "..."}`: read the rows from
`changes` or `events`, and follow `next` until it is absent to read past the 1000-row cap (see
Truncated responses). `X-Horopter-Truncated` is unchanged, and the adapter routes are unchanged.

### `source` comes from the credential

An event's `source` is now the source its credential was issued with
(`horoctl admin keys issue --source`), on both ingest routes, rather than whatever the producer
claimed.

- `POST /v1/events` answers `400` to a body carrying `source` — any value, including `""` and
  `null` — the same as it does for `tenant`. Remove the field; the event is otherwise unchanged.
- `POST /v1/events/cloudevents` still requires the envelope's `source`, as CloudEvents does,
  but it no longer becomes the event's `source`. It keeps its CloudEvents role as half of the
  idempotency key, so redelivery still deduplicates.
- Events already stored keep the `source` their producer claimed. `?source=` therefore matches
  credential sources for events written after the upgrade and claimed ones before it.

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

**`GET /v1/changes` and `GET /v1/events` page instead.** Their bodies are an envelope —
`{"changes": [...], "next": "..."}` and `{"events": [...], "next": "..."}` — and `next`, present
until the last page, is the next request ready to send: every parameter repeated, with a
`cursor` added. The same URL comes as a `Link: <...>; rel="next"` header. In a shell:

    url="/v1/changes?from=$FROM&to=$TO"
    while [ -n "$url" ]; do
      page="$(curl -s -H "Authorization: Bearer $KEY" "$EGRESS$url")"
      echo "$page" | jq -c '.changes[]'
      url="$(echo "$page" | jq -r '.next // empty')"
    done

A cursor is bound to its query: resending it with different filters or a different window is a
`400`. `limit` may change between pages. Pages are not a snapshot: a change's start can move
while you page — earlier when an older event arrives, later when a `started` event follows an
annotation — so a change can be missed, or returned on two pages. `horo` shows each ref once;
re-reading finds a missed one.

The signal is a header rather than a field in the body because two of these routes render
vendor-owned shapes — Grafana annotations and OTLP spans — that cannot carry a Horopter
envelope.

## Filtering changes

`GET /v1/changes` and both adapter routes take the same filters, all optional, all combining
with AND:

| parameter | matches |
|---|---|
| `subject` | a pattern matched against every subject the change touched up to its end — repeat it to OR several |
| `type` | the change's type |
| `status` | the change's status |
| `locus` | the namespace the ref was issued under — the part of a stored ref before its `/` |
| `source` | the source of a credential that reported the change — matches if it is any of them |
| `attr.<key>` | an attribute's exact value |

**`subject` is a pattern.** It uses the syntax of a credential's `--subject-pattern` (Go's
`path.Match`: `*` within one `/`-separated segment, `?` one character, `[...]` a class, `\`
escapes) plus `{a,b}` alternation, and matches a change if **any** subject among its events
matches — not only the last event's. A subject first seen after the change ended does not count, so
a later drift correction does not widen an old rollout. `subject=*/lastlight/*` finds every change
that touched the `lastlight` namespace; `subject=*/{lastlight,logging}/*` adds `logging`. Every
comma inside braces separates alternatives, and `\,` does not escape one; outside braces a comma
is literal, so give an alternative that contains one its own `subject` parameter. A
malformed pattern, one that is not valid UTF-8, or one with a reversed range such as `[9-0]` is
answered **400**. The other filters are exact.

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

`source` filters against **every** system that contributed to the change, not just the last
one. A ref whose events come from two systems — a change CI starts and a cluster controller
completes, say — is found by filtering on either.

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
instant but no duration or lifecycle, so four fields that `POST /v1/events` sends as
first-class request fields travel in the `data` payload instead:

| data key | becomes | if absent |
|---|---|---|
| `correlation_ref` | the ref two events share to describe one interval | the event stands alone as a point |
| `title` | the change title | falls back to the event type |
| `actor` | who performed the change | empty |
| `lifecycle` | the explicit lifecycle assertion | the type's suffix decides — see below |

These four keys are **reserved**: each is lifted into its field and removed from the
attribute map, so it is not also stored as a generic attribute. Every other key in `data`
is kept as an attribute untouched.

A reserved key holding a non-string value is left alone rather than coerced — `{"title":
7}` stays an attribute and the title falls back as though it were absent.

CloudEvents carries no tenancy either, but `tenant` is not one more reserved key alongside
the ones above: it is derived from the credential, the same as on `POST /v1/events`, and a
`data.tenant` of any kind — including `null` — is rejected with `400` rather than read.

The envelope's `source` is not the event's `source` either. The event takes its source from the
credential; the envelope's stays half of the idempotency key, which is the event's identity in
CloudEvents' own terms.

The envelope's own `time` supplies `occurred_at`. It is optional in CloudEvents; when it
is missing, receipt time is used, since that is the earliest moment the system can attest
to.

## Flux

Two ways in: `horopter-flux`, a client that watches Flux's objects, and the older ingest route
below, which it replaces.

### horopter-flux

`horopter-flux` runs in the cluster, watches GitRepositories, Kustomizations, HelmReleases and
their Kubernetes Events read-only, and posts what they show with a producer key. It keeps no
state: on start it derives every change from the objects' status, and its idempotency keys,
prefixed with the `kube-system` Namespace's uid, make re-posting harmless.

| Flux shows | recorded as | type | lifecycle |
|---|---|---|---|
| a GitRepository artifact at a new Git revision | commit change start | `flux.artifact.new` | `started` |
| Kustomization Ready=True `ReconciliationSucceeded` | commit change end | `flux.kustomization.succeeded` | `completed` |
| Kustomization Ready=False, except `DependencyNotReady` and `Progressing` | commit change end and a point | `flux.kustomization.failed` | `failed`, `none` |
| Kustomization `Progressing` Event | one event per changed object | `flux.kustomization.applied` | `none` |
| HelmRelease "Running 'install'/'upgrade' action" | release change start | `flux.helmrelease.<action>.started` | `started` |
| that revision reaching `status.history`, deployed or failed | release change end | `flux.helmrelease.<action>.succeeded` / `.failed` | `completed`, `failed` |
| a rollback remediating it | release change end | `flux.helmrelease.<action>.rolled_back` / `.rollback.failed` | `rolled_back`, `failed` |
| HelmRelease Released=False `PendingRelease` | a point, and the open release change ends | `flux.helmrelease.pending_release` | `none` |
| HelmRelease `DriftCorrected*`, `Test*`, `Uninstall*` Event | a point | `flux.helmrelease.<reason>` | `none` |

A commit change's ref is the Git revision and its title the commit subject, from
source-controller's `NewArtifact` Event; if that Event has expired, the title is the branch and
short SHA (`main@b1f2477`). A release change's ref is
`HelmRelease/<namespace>/<name>/<uid>/v<helm revision>` and its title the app version. Only
Git sources start a commit change.

It reads `HOROPTER_INGRESS_URL` and `HOROPTER_INGRESS_KEY`, and exits with status 2 if either is
empty. Issue one key per cluster, without `--subject-pattern`, for the same reason as the route's
below. The locus defaults to the source, so each cluster is its own correlation space:

    horoctl admin keys issue --dsn "$DATABASE_URL" --tenant <tenant-id> --role producer \
      --source flux-<cluster>

It needs `get`, `list` and `watch` on `gitrepositories`, `kustomizations`, `helmreleases` and
`events`, and `get` on the `kube-system` Namespace. An event Horopter cannot take for now (no
answer, a 5xx or a 429) is retried with backoff; any other 4xx is logged at ERROR and dropped.

### The ingest route

`POST /v1/events/flux` takes Flux notification-controller's `generic` provider payload and
records one change per Git revision, as `decisions/2026-10-04-flux-lifecycle-mapping.md` in
`horopter-internal` lays out:

| Flux event | recorded as | type | lifecycle |
|---|---|---|---|
| `GitRepository` `NewArtifact` | change start | `flux.artifact.new` | `started` |
| Kustomization `ReconciliationSucceeded` | change end | `flux.kustomization.succeeded` | `completed` |
| Kustomization, `severity: error` | change end and a point | `flux.kustomization.failed` | `failed`, `none` |
| Kustomization `Progressing` | one event per changed object, on the revision's change | `flux.kustomization.applied` | `none` |
| HelmRelease `…Succeeded` / `…Failed` | a point | `flux.helmrelease.<reason>` | `none` |

The change's ref is the Git revision, e.g. `flux/refs/heads/main@sha1:…` under a `flux`
credential. Its title is the commit's subject, as source-controller's `NewArtifact` quotes it
(Flux truncates it to about 50 characters), since a change is titled by its start. A
Kustomization's later runs at the same revision repeat its key, so only its first outcome is
folded. Anything else, including a non-Git revision, is answered
`200 {"recorded": 0, "ignored": "<why>"}`, which notification-controller counts as delivered.

A `Progressing` event is kustomize-controller's changeset, one changed object per line
(`Deployment/lastlight/cloudflared configured`). Each line is recorded with the object as its
subject and the action as its `action` attribute, so a rollout's change records what it changed.
A line whose object is not `Kind/name` or `Kind/namespace/name` is skipped.
Because these subjects are the applied objects rather than the Kustomization, issue the Flux
credential **without** `--subject-pattern`: a pattern narrowed to `Kustomization/*/*` refuses every
changeset with `403`, and no single pattern covers both `Kind/name` and `Kind/namespace/name`.

A Flux `Provider` sends the key as a header from its secret:

```yaml
apiVersion: notification.toolkit.fluxcd.io/v1beta3
kind: Provider
metadata: { name: horopter, namespace: flux-system }
spec:
  type: generic
  address: http://horopter-ingress.horopter.svc.cluster.local:8080/v1/events/flux
  secretRef: { name: horopter-flux-producer }   # key `headers`: "Authorization: Bearer <key>"
```

An `Alert`'s event sources match a namespace exactly, so list every namespace holding a
`GitRepository`, `Kustomization` or `HelmRelease` you want recorded.

Event metadata becomes attributes, and so tags in Grafana. That covers every key except the
ones Flux sets for its own use (`revision`, `originRevision`, `token`, `commit`,
`commit_status`, `change_request`, `summary`). It includes helm-controller's `app-version`,
which comes from the chart's `appVersion`. You can add your own keys with annotations on the
object, which Flux copies into its events:

```yaml
metadata:
  annotations:
    event.toolkit.fluxcd.io/team: platform   # recorded as the attribute team: platform
```

A key the route sets itself, such as `kind` or `chart_version`, keeps the route's value. If the
copied keys together exceed 1 KiB of JSON, none of them are kept, and the event is recorded
without them.

## Type suffixes and the lifecycle field

Every event asserts something about its change's lifecycle: whether a projection opens,
stays open, or closes. The assertion comes from the explicit `lifecycle` field on
`POST /v1/events` (or the `lifecycle` data key on the CloudEvents route, above) when the
producer sets one; the event type's final dot-separated segment decides otherwise. The
suffix vocabulary below is transcribed from `internal/core/fold.go`'s `StatusForType` and
`isStartType`, the only place it is defined — read there if this ever needs rechecking.

A type ending in `.started` or `.begun` asserts a start. A type ending in one of these
suffixes asserts the terminal status next to it:

| status | suffixes |
|---|---|
| `completed` | `.completed`, `.finished`, `.succeeded`, `.resolved`, `.closed`, `.done`, `.ended` |
| `failed` | `.failed`, `.errored`, `.timed_out` |
| `rolled_back` | `.rolled_back`, `.rolledback` |
| `cancelled` | `.cancelled`, `.canceled`, `.aborted` |

A type matching none of these — `deployment.annotated`, say — asserts nothing by suffix.

The explicit `lifecycle` field overrides the suffix entirely, for producers whose own type
vocabulary does not follow it: `started`, `completed`, `failed`, `rolled_back` and
`cancelled` assert exactly what their names say, whatever the event's type ends in. `none`
asserts nothing, and is how a producer whose type happens to end in `.started` (a type
reused from another system, say) records that particular event as a point rather than the
start of an interval.

Absence — no `lifecycle` field, no `lifecycle` data key — does not mean "nothing happened";
it means the type's suffix decides. Only an explicit `none`, or a type matching neither list
above, leaves an event with no lifecycle assertion at all.

## The append response

`POST /v1/events` and `POST /v1/events/cloudevents` answer the same four fields:

| field | goes |
|---|---|
| `id` | nowhere — it identifies the *event*, for dedup and audit |
| `ref` | `GET /v1/changes?ref=…` |
| `correlation_ref` | the next append's `correlation_ref` |
| `created` | nowhere — it says whether this append created the event or matched an existing idempotency key |

`correlation_ref` is the amend handle: to add another event to this same change, echo it
back, unmodified, as that next event's own `correlation_ref`. Do **not** echo `ref` there
instead — `ref` is already prefixed with the credential's locus, so composing it again
prefixes it a second time and silently addresses a different change. `id` names the event,
not the change, and has no second use; `ref` is for looking a change up by its stored
identity, not for amending it.

**Ingress refuses that mistake rather than storing it.** A `correlation_ref` whose first
segment equals the appending credential's locus is answered `400`, naming what would have
been stored. Only that shape is refused: a producer ref may contain `/` freely —
`myorg/repo#123` is composed to `<locus>/myorg/repo#123` and read back whole, because only
the locus prefix is stripped, and a locus may never contain `/` itself.

If your refs genuinely begin with your own locus, set
`HOROPTER_INGRESS_ALLOW_LOCUS_PREFIXED_REF=true`. It lifts the guard for **every** producer
on that ingress, not just yours — it is a daemon setting, not a per-credential one — and the
double prefix is then stored as sent rather than silently stripped.

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
