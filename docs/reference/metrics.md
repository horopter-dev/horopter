---
title: Metrics
description: The metrics Horopter records about its own operation, and the attributes on its read spans.
---

`horopter-ingress` and `horopter-egress` serve Prometheus metrics at `/metrics` on a listener of
their own, and push the same metrics over OTLP when a collector is configured; `horopter-migrate`
pushes over OTLP only, as it finishes. [Configuration](configuration.md#telemetry) has the
variables, and [Observe Horopter](https://docs.horopter.dev/how-to/observe-horopter/) what to
alert on.

No metric carries a tenant, a ref, an event id or any other value a caller chooses: every
attribute comes from a fixed set. `OTEL_RESOURCE_ATTRIBUTES` appears on `/metrics` as the
`target_info` series.

## Horopter's Instruments

Prometheus names replace each `.` with `_`, and add `_total` to a counter and `_bytes` to a
measure in bytes; attribute names become labels the same way.

| instrument | Prometheus | kind | recorded by | attributes |
| --- | --- | --- | --- | --- |
| `horopter.event.appends` | `horopter_event_appends_total` | counter | ingress | `horopter.append.outcome`, `horopter.append.dedupe_path`, `horopter.fold.out_of_order`, `horopter.error.kind` |
| `horopter.refold` | `horopter_refold_total` | counter | ingress | `horopter.refold.altered` |
| `horopter.read.shed` | `horopter_read_shed_total` | counter | egress | `horopter.read.shed.gate` |
| `horopter.read.admission.limit` | `horopter_read_admission_limit` | gauge | egress | |
| `horopter.read.admission.in_flight` | `horopter_read_admission_in_flight` | gauge | egress | |
| `horopter.read.memory.reserved` | `horopter_read_memory_reserved_bytes` | gauge | egress | |
| `horopter.read.memory.capacity` | `horopter_read_memory_capacity_bytes` | gauge | egress | |

- **`horopter.event.appends`** counts every event submitted to the store.
  `horopter.append.outcome` is `created`, `deduplicated` or `failed`;
  `horopter.append.dedupe_path` is `none`, `lookup` (a retry after the first attempt committed)
  or `unique_violation` (concurrent retries of one idempotency key); `horopter.error.kind`,
  on failures only, is `invalid_event` or `transient`; `horopter.fold.out_of_order` is whether
  the event's id sorts below the latest one already folded into its change, which makes the
  append re-fold that change from its log.
- **`horopter.refold`** counts those re-folds. `horopter.refold.altered` is whether the
  re-fold moved the change's status or end; a re-fold that moved only its subject, type,
  title, actor, attributes or latest event is counted as `false`.
  `horopter.refold.altered=true` is the rate at which late arrivals change recorded outcomes,
  the number worth an alert. Each re-fold reads the
  change's whole history inside the append's transaction, so its rate, times those changes'
  event counts, is the append latency it adds.
- **`horopter.read.shed`** counts reads refused with `503`; `horopter.read.shed.gate` is
  `concurrency` or `memory`.
- **The four gauges** are read admission's state: the concurrency limit and the reads holding
  a place under it, and the memory pool's reservations and capacity.

## Library Metrics

- **HTTP server**: request duration and body sizes from `otelhttp`, by method, route and
  status code. Ingress answers each append outcome with its own status, `201`, `200`, `400` or
  `503`, so these split appends by outcome too.
- **Database**: operation durations and errors from `otelpgx`, and `pgxpool` connection-pool
  statistics.
- **Go runtime and process**, on `/metrics` only: goroutines, memory, garbage collection,
  resident memory and open file descriptors.

## Read Span Attributes

The server span of every read egress gates carries what admission decided:

- `horopter.admission.outcome`: `admitted`, `shed_concurrency`, `shed_memory` or `too_large`.
- `horopter.admission.wait_ms`: how long it waited at the gates.
- `horopter.db.connection_wait_ms`, once past the concurrency gate: its longest wait for a
  database connection.
- `horopter.read.estimated_bytes`, once estimated.
- `horopter.read.reserved_bytes` and `horopter.read.byte_cut`, once memory is reserved.
