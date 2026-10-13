---
title: Configuration
description: Every environment variable Horopter's binaries read, with its default and meaning.
---

Every Horopter binary is configured by environment variables, read once at startup. A daemon
given a value that does not parse, or is out of range, stops at startup with an error naming
the variable and the value.

## Daemons and Database Tools

| variable | default | meaning |
| --- | --- | --- |
| `DATABASE_URL` | none; required | PostgreSQL connection string, read by both daemons, `horopter-migrate` and `horoctl`. `--dsn` overrides it. Its `pool_max_conns` sets the connection pool, `max(4, NumCPU)` otherwise |
| `HOROPTER_ADDR` | `0.0.0.0:8080` on ingress, `0.0.0.0:8081` on egress | the daemons' listen address. `--addr` overrides it |
| `HOROPTER_LOG_LEVEL` | `info` | the minimum level logged, by both daemons, `horopter-migrate`, `horoctl migrate` and `horopter-flux`: `debug`, `info`, `warn` or `error`, case-insensitive |

## Ingress

Read by `horopter-ingress`.

| variable | default | meaning |
| --- | --- | --- |
| `HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES` | `4096` | the largest attribute map accepted, in bytes of compact JSON without HTML escaping; from `1` to `1048576`. See [Attribute Size](http-api.md#attribute-size) |
| `HOROPTER_INGRESS_ALLOW_LOCUS_PREFIXED_REF` | `false` | accept a `correlation_ref` whose first segment is the credential's own locus, for every producer on this ingress. See [The Append Response](http-api.md#the-append-response) |

## Egress

Read by `horopter-egress`. What each one governs is
[Read Admission](https://docs.horopter.dev/concepts/read-admission/).

| variable | default | meaning |
| --- | --- | --- |
| `HOROPTER_EGRESS_READ_MEMORY_BYTES` | `268435456` (256 MiB) | the memory pool reads reserve against; a whole number, at least `1048576` |
| `HOROPTER_EGRESS_ADMISSION_WAIT` | `500ms` | how long a read waits at each admission gate before it is shed; a Go duration, above 0 and at most `10s` |
| `HOROPTER_EGRESS_DB_WAIT_THRESHOLD` | `100ms` | the database connection wait above which a read counts as congested and halves the concurrency limit; a Go duration, above 0 |

## Telemetry

Read by `horopter-ingress`, `horopter-egress`, `horopter-migrate` and `horoctl migrate`. How to
use them is
[Observe Horopter](https://docs.horopter.dev/how-to/observe-horopter/).

| variable | default | meaning |
| --- | --- | --- |
| `OTEL_SDK_DISABLED` | `false` | `true` turns off all telemetry: traces, OTLP metrics and the `/metrics` listener |
| `OTEL_SERVICE_NAME` | the binary's name | the `service.name` every span and metric is reported with. `horoctl migrate` reports as `horopter-migrate` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset | the OTLP/HTTP collector, e.g. `http://otel-collector:4318`. Setting it turns on tracing and OTLP metrics; unset, both are off |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | unset | the traces endpoint, e.g. `http://otel-collector:4318/v1/traces`. Setting it turns on tracing, and it takes precedence over the general endpoint |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | unset | the metrics endpoint, e.g. `http://otel-collector:4318/v1/metrics`. Setting it turns on OTLP metrics, and it takes precedence over the general endpoint |
| `OTEL_EXPORTER_PROMETHEUS_HOST` | `localhost` | the host the `/metrics` listener binds, on the daemons |
| `OTEL_EXPORTER_PROMETHEUS_PORT` | `9464` | the `/metrics` listener's port, on both daemons |
| `HOROPTER_TRACE_DISABLE` | empty | a comma-separated list of tracing areas to silence: `http`, the server span of each request; `store`, the span of each store operation; `sql`, the span of each SQL statement. An unknown name is accepted and silences nothing |

The OpenTelemetry SDK reads further variables itself, among them `OTEL_RESOURCE_ATTRIBUTES`,
`OTEL_METRIC_EXPORT_INTERVAL`, `OTEL_METRIC_EXPORT_TIMEOUT`,
`OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` and the rest of `OTEL_EXPORTER_OTLP_*`.
Horopter does not validate them; see the
[OpenTelemetry SDK configuration](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/).

## `horo`

| variable | default | meaning |
| --- | --- | --- |
| `HOROPTER_EGRESS_URL` | none | egress's base URL, for `horo changes` and `horo show`. `--egress-url` overrides it |
| `HOROPTER_EGRESS_KEY` | none | a key with `read`, for `horo changes` and `horo show`. `--key` overrides it |
| `HOROPTER_INGRESS_URL` | none | ingress's base URL, for `horo run`, `horo event` and `horo refold`. `--ingress-url` overrides it |
| `HOROPTER_INGRESS_KEY` | none | a key with `append`, for `horo run` and `horo event`. `--key` overrides it |
| `HOROPTER_ADMIN_KEY` | none | a key with `admin`, for `horo refold` only. `--key` overrides it |
| `HORO_RUN_REPORT` | none | the file `horo run` writes its report to. `--report` overrides it |

## `horopter-flux`

| variable | default | meaning |
| --- | --- | --- |
| `HOROPTER_INGRESS_URL` | none; required | ingress's base URL |
| `HOROPTER_INGRESS_KEY` | none; required | a key with `append`, issued without `--subject-pattern` |

It exits with status 2 when either is empty. Outside a cluster, it reads its Kubernetes client
configuration from `KUBECONFIG`, as client-go does.
