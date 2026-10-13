---
title: HTTP API
description: Every route on horopter-ingress and horopter-egress, with its capability, parameters and responses.
---

`horopter-ingress` records events; `horopter-egress` serves what they describe. Both speak
JSON over HTTP.

## Routes

| route | daemon | capability |
| --- | --- | --- |
| `POST /v1/events` | `horopter-ingress` | `append` |
| `POST /v1/events/cloudevents` | `horopter-ingress` | `append` |
| `POST /v1/admin/refold` | `horopter-ingress` | `admin` |
| `GET /healthz` | `horopter-ingress` | none |
| `GET /readyz` | `horopter-ingress` | none |
| `GET /v1/changes` | `horopter-egress` | `read` |
| `GET /v1/events` | `horopter-egress` | `read` |
| `GET /adapters/grafana/annotations` | `horopter-egress` | `read` |
| `GET /adapters/otlp/spans` | `horopter-egress` | `read` |
| `GET /healthz` | `horopter-egress` | none |
| `GET /readyz` | `horopter-egress` | none |

Each daemon also serves Prometheus metrics at `/metrics`, on a listener of its own; see
[Metrics](metrics.md).

## Credentials

A credential is sent as `Authorization: Bearer <key>`. Which role grants which capability is
[Credentials and Capabilities](https://docs.horopter.dev/concepts/credentials-and-capabilities/).

| answer | when |
| --- | --- |
| `401` | no credential, or one that is malformed, unknown, revoked or expired. The body is the same for each |
| `403` | a credential without the route's capability, or one whose `--subject-pattern` does not cover the event's subject |

A revocation reaches new requests within the credential cache's TTL, 30 seconds.

### Tenants

Ingress takes the tenant from the credential: a request body naming `tenant` is answered
`400`. Every egress route reads `?tenant=`:

| credential | `?tenant=` | answer |
| --- | --- | --- |
| scoped to a tenant | absent, or its own | read in its own tenant |
| scoped to a tenant | another | `403` |
| global | absent | `400` |
| global | a tenant that does not exist | `404` |

## Errors

Every error Horopter answers is a JSON object, `{"error": "<message>"}`. A read that fails is
answered `500`, and an append the store cannot take `503`, with a generic message; the cause,
which can name tables and the database host, goes to the daemon's log.

## `POST /v1/events`

Appends one event. The body is a JSON object, at most 1 MiB (`413` above it):

| field | required | meaning |
| --- | --- | --- |
| `type` | yes | what happened, e.g. `deployment.started`. Its suffix may assert a lifecycle; see [Type Suffixes](#type-suffixes-and-the-lifecycle-field) |
| `subject` | yes | what it happened to, e.g. `acme/prod` |
| `occurred_at` | yes | when, RFC 3339 |
| `correlation_ref` | no | the ref events share to describe one change. Without one, the event is a point |
| `title` | no | a human-readable title; a change without one is titled by its type |
| `actor` | no | who or what did it |
| `idempotency_key` | no | a key a retry repeats; an append matching a stored key returns the stored event |
| `attributes` | no | a JSON object; see [Attribute Size](#attribute-size) |
| `lifecycle` | no | `started`, `completed`, `failed`, `rolled_back`, `cancelled` or `none` |

`tenant` and `source` are refused with `400`, whatever their value, `null` included: both come
from the credential.

### The Append Response

`201` for a new event, `200` for an idempotency key already stored. Both append routes answer
the same four fields:

| field | use |
| --- | --- |
| `id` | the event's id, for audit |
| `ref` | the stored ref, `<locus>/<ref>`: look the change up with `GET /v1/changes?ref=` |
| `correlation_ref` | the amend handle: send it unmodified as the next event's `correlation_ref` to add to this change |
| `created` | `false` when the append matched a stored idempotency key |

Do not send `ref` as the next `correlation_ref`: it is already prefixed with the credential's
locus, and would be prefixed again. Ingress refuses that mistake with `400`, naming what would
have been stored: a `correlation_ref` whose first segment is the credential's own locus.
`HOROPTER_INGRESS_ALLOW_LOCUS_PREFIXED_REF=true` lifts the check for every producer on that
ingress; such a ref is then stored with the double prefix. A ref may otherwise contain `/`
freely: `myorg/repo#123` is stored as `<locus>/myorg/repo#123`.

### Attribute Size

An event whose attributes encode to more than
`HOROPTER_INGRESS_MAX_EVENT_ATTRIBUTE_BYTES` (default `4096`) bytes of compact JSON, measured
without HTML escaping, is refused with `400`:

    invalid event "01J8Z3Q4R5S6T7V8W9X0Y1Z2A3": attributes encode to 5120 bytes of JSON;
    the limit is 4096 — keep attributes to identifiers and short text, and link to anything
    larger

(one line, wrapped here). Events already stored are not checked again when the limit changes.
What belongs in attributes is
[Attributes](https://docs.horopter.dev/concepts/attributes/).

## `POST /v1/events/cloudevents`

Appends one event from a CloudEvents 1.0 envelope in structured mode, with
`Content-Type: application/cloudevents+json`. It answers as `POST /v1/events` does.

- `id` and `source` are required, and together are the idempotency key, so a redelivered
  envelope is deduplicated. The event's own `source` comes from the credential, not the
  envelope.
- `time` becomes `occurred_at`; without it, the time the envelope was received.
- `type` and `subject` are the event's.
- `data`, when present, must be a JSON object. Four keys in it are reserved, lifted into the
  event's own fields and removed from its attributes; every other key is kept as an attribute.

| `data` key | becomes | if absent |
| --- | --- | --- |
| `correlation_ref` | the change's ref | the event is a point |
| `title` | the title | the type |
| `actor` | the actor | empty |
| `lifecycle` | the lifecycle assertion | the type's suffix decides |

A reserved key holding anything but a string is left in the attributes, and the field is
treated as absent. A `data.tenant` of any value, `null` included, is refused with `400`.

## Type Suffixes and the Lifecycle Field

Each event asserts whether its change starts, ends, or neither. An explicit `lifecycle`, the
field or the `data` key, decides; without one, the type's final dot-separated segment does:

| asserts | type ends in |
| --- | --- |
| a start | `.started`, `.begun` |
| `completed` | `.completed`, `.finished`, `.succeeded`, `.resolved`, `.closed`, `.done`, `.ended` |
| `failed` | `.failed`, `.errored`, `.timed_out` |
| `rolled_back` | `.rolled_back`, `.rolledback` |
| `cancelled` | `.cancelled`, `.canceled`, `.aborted` |

A type ending in anything else asserts nothing. `lifecycle: "none"` asserts nothing whatever the
type ends in, so a type that happens to end `.started` can be recorded as a point.

## `POST /v1/admin/refold`

Rebuilds one stored change from its events, with ingest running. The body is
`{"ref": "<locus>/<ref>"}`; a global key adds `"tenant": "<tenant id>"`. The answer names how
many events were read and which fields of the stored change were repaired, `[]` when none
were:

    {"ref":"ci/dep-1","events":12,"altered":["end","status"]}

A ref with no events is answered `404`. How and when to use it is
[Re-fold One Change](https://docs.horopter.dev/how-to/re-fold-one-change/).

## `GET /v1/changes`

The changes overlapping a window, oldest first.

| parameter | meaning |
| --- | --- |
| `from`, `to` | the window, RFC 3339; required unless `ref` is given. `from` after `to` is a `400` |
| `ref` | one change, by its stored `<locus>/<ref>` |
| `limit` | at most this many rows, from 1; above 1000 is read as 1000 |
| `cursor` | the page to continue from; see [Paging](#paging) |
| `tenant` | see [Tenants](#tenants) |

It takes the [filters](#filters) too. The answer is `{"changes": [...], "next": "..."}`, each
change an object of:

| field | meaning |
| --- | --- |
| `ref` | the stored ref |
| `subject`, `type`, `title`, `actor` | folded from its events |
| `start` | when it started: `started_at` when a producer asserted one, `first_seen_at` otherwise |
| `started_at` | the earliest start a producer asserted; absent until one does |
| `first_seen_at` | its earliest event's `occurred_at` |
| `end` | when it ended; absent while it is open |
| `status` | `in_progress` while open, `occurred` for a point, or how it ended: `completed`, `failed`, `rolled_back` or `cancelled` |
| `attributes` | its events' attributes, a later event's value replacing an earlier one's |
| `sources` | the source of every credential that reported it |
| `last_event_id` | the latest event folded into it |

## `GET /v1/events`

A tenant's event log, for audit and export, oldest first.

| parameter | meaning |
| --- | --- |
| `ref` | one change's events, by its stored `<locus>/<ref>` |
| `limit`, `cursor`, `tenant` | as on `GET /v1/changes` |

The answer is `{"events": [...], "next": "..."}`, each event an object of `id`, `tenant`,
`idempotency_key`, `schema_version`, `source`, `type`, `subject`, `actor`, `title`,
`occurred_at`, `received_at`, `correlation_ref`, `attributes`, `lifecycle`, `locus`, `origin`
(whether a human or a process appended it) and `edited`.

## `GET /adapters/grafana/annotations`

The changes overlapping a window, as Grafana annotations: an array of objects with `time`,
`timeEnd`, `title`, `tags` and `text`. `text` is the change's `horopter.description`
attribute.

| parameter | meaning |
| --- | --- |
| `from`, `to` | the window, in epoch milliseconds; required |
| `limit`, `tenant` | as on `GET /v1/changes` |

It takes the [filters](#filters). `ref` is refused with `400`: fetch one change with
`GET /v1/changes?ref=`. Wiring it to Grafana is
[Show Changes on Your Grafana Dashboards](https://docs.horopter.dev/how-to/grafana/).

## `GET /adapters/otlp/spans`

The changes overlapping a window, as OTLP/JSON trace data (`resourceSpans`), one span per
change, carrying `horopter.subject`, `horopter.ref` and `horopter.status` among its
attributes.

| parameter | meaning |
| --- | --- |
| `from`, `to` | the window, RFC 3339; required |
| `limit`, `tenant` | as on `GET /v1/changes` |

It takes the [filters](#filters).

## Filters

`GET /v1/changes` and both adapter routes take these, all optional, combined with AND. An
empty value is no filter.

| parameter | matches |
| --- | --- |
| `subject` | a pattern, against every subject the change touched up to its end; repeat it to match any of several |
| `type` | the change's type, exactly |
| `status` | the change's status, exactly |
| `locus` | the part of the stored ref before its first `/`, exactly |
| `source` | any source that reported the change, exactly |
| `attr.<key>` | an attribute's value, exactly. Only `deployment.environment.name` is filterable |

`subject` uses `path.Match` syntax, `*` within one `/`-separated segment, `?` one character,
`[...]` a class and `\` an escape, plus `{a,b}` alternation: `subject=*/{lastlight,logging}/*`.
Inside braces every comma separates alternatives, and `\,` does not escape one. A subject first
seen after the change ended does not count. A malformed pattern, one that is not valid UTF-8,
or a reversed range such as `[9-0]` is a `400`.

Any other `attr.` key is a `400` naming the keys that are filterable, its value empty or not.

## Paging

Every read returns at most 1000 rows. `GET /v1/changes` and `GET /v1/events` page past that:
while rows remain, the answer carries `next`, the next request with every parameter repeated
and a `cursor` added, and the same URL in a `Link: <...>; rel="next"` header. Follow it until it
is absent:

    url="/v1/changes?from=$FROM&to=$TO"
    while [ -n "$url" ]; do
      page="$(curl -s -H "Authorization: Bearer $KEY" "$EGRESS$url")"
      echo "$page" | jq -c '.changes[]'
      url="$(echo "$page" | jq -r '.next // empty')"
    done

A cursor is bound to its query: sent with other filters or another window, it is a `400`.
`limit` may change between pages. Pages are not a snapshot: a change's start can move while you
page, so a change can be missed or returned twice.

### Truncation

When rows were left out, the answer carries `X-Horopter-Truncated: true`; when none were, the
header is absent. All four read routes set it, the adapters too, whose vendor-owned bodies have
no room for a field. Rows are oldest first, so what is left out is the most recent. Narrow the
window or filter, and ask again until the header is gone.

## Read Admission

Egress serves a read only when it has the capacity to:
[Read Admission](https://docs.horopter.dev/concepts/read-admission/).

| answer | when | what to do |
| --- | --- | --- |
| `503` with `Retry-After` | a gate could not admit the read within `HOROPTER_EGRESS_ADMISSION_WAIT` | retry after `Retry-After` seconds |
| `200` with `X-Horopter-Truncated: true` | the read was larger than the memory pool, and was cut to the oldest rows that fit | narrow it, as for any truncation |
| `422` | the first row alone needs more than the pool; the body names the limit | filter out that row, or raise `HOROPTER_EGRESS_READ_MEMORY_BYTES` |

## `GET /healthz` and `GET /readyz`

Both daemons answer them without a credential, and read admission never sheds them.
`/healthz` answers `200` while the process runs and never touches the database; `/readyz`
answers `200` when the database answers, and `503` when it does not. Point a liveness probe at
`/healthz` and a readiness probe at `/readyz`. The images carry no `HEALTHCHECK`: they have no
shell, so a container health check has nothing to run.
