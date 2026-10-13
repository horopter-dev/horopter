---
title: horo
description: The horo command line, which reads Horopter's changes and records new ones.
---

`horo` reads changes from egress with a key holding `read`, and records changes through ingress
with a key holding `append`. Each subcommand's URL and key flags default to the variables in
[Configuration](configuration.md#horo).

## Exit Codes

| code | meaning |
| --- | --- |
| `0` | success |
| `1` | an error from Horopter, or a `--strict` refusal |
| `2` | a usage error |
| `3` | `horo show` or `horo refold` found no such ref |

`horo run` exits with its command's own code instead; see [`horo run`](#horo-run).

## Reading Flags

`horo changes` and `horo show` take:

| flag | default | meaning |
| --- | --- | --- |
| `--egress-url` | `$HOROPTER_EGRESS_URL` | egress's base URL |
| `--key` | `$HOROPTER_EGRESS_KEY` | a key holding `read` |
| `--format` | `table` on a terminal, `jsonl` otherwise | `table`, `json` or `jsonl` |
| `--json` | | `--format json` |
| `--utc` | | show times in UTC |

## `horo changes`

    horo changes                         # the last 24 hours
    horo changes --since 168h --subject acme/prod --status failed
    horo changes --from 2026-09-14T00:00:00Z --to 2026-09-15T00:00:00Z --json

Lists the changes overlapping a window. It follows egress's `next` links, so it returns every
matching change however many there are.

| flag | default | meaning |
| --- | --- | --- |
| `--since` | `24h` | the window, back from now |
| `--from`, `--to` | | the window, RFC 3339, in place of `--since` |
| `--subject` | | a subject pattern, as egress's [`subject` filter](http-api.md#filters) |
| `--type`, `--status`, `--locus` | | the matching filter, exactly |
| `--source` | | a source that reported the change |
| `--attr` | | `key=value` on a filterable attribute; repeatable |
| `--limit` | `0` | at most this many changes; `0` for all |

## `horo show`

    horo show ci/dep-1

Shows one change, by its stored `<locus>/<ref>`, and every event in it. It exits `3` when there
is no such change.

## Recording Flags

`horo run` and `horo event` take:

| flag | default | meaning |
| --- | --- | --- |
| `--ingress-url` | `$HOROPTER_INGRESS_URL` | ingress's base URL |
| `--key` | `$HOROPTER_INGRESS_KEY` | a key holding `append` |
| `--type` | | the event type; `horo run` takes the change's type and adds the suffix |
| `--subject` | | what the change happened to, e.g. `acme/prod` |
| `--title` | | a human-readable title |
| `--actor` | | who or what performed it |
| `--ref` | | the change's ref, as the producer sends it; without one, `horo run` makes one, and an event is a point |
| `--attr` | | a `key=value` attribute; repeatable |

## `horo run`

    horo run --type deployment --subject acme/prod --title "Deploy api" -- ./deploy.sh

Records a command as a change: posts `<type>.started`, runs the command with its output
untouched, then posts `<type>.completed`, `<type>.failed` with `exit_code`, or
`<type>.cancelled` with `signal`. Why recording never blocks the work, and how signals reach the
command, is [Recording a Change](https://docs.horopter.dev/concepts/recording-a-change/).

| flag | default | meaning |
| --- | --- | --- |
| `--strict` | | refuse to run the command when its start cannot be recorded, and fail when its end cannot |
| `--report` | `$HORO_RUN_REPORT` | write a JSON report of what was recorded to this file |

It exits with the command's own code: `128 + n` when signal `n` ended it, `127` when it could
not start, and `1` under `--strict` when the start or the end could not be recorded.

It waits up to 10 seconds for the start to be recorded, and retries the end for up to 30
seconds after the command. On stderr it prints the stored ref, which `horo show` takes, and the
plain ref, which `--ref` takes; if the end cannot be confirmed, it prints the `horo event`
command that closes the change.

### The Report

    {"start_posted":true,"end_posted":true,"ref":"ci/dep-1","producer_ref":"dep-1",
     "outcome":"completed","exit_code":0,"reason":null}

| field | meaning |
| --- | --- |
| `start_posted` | whether the start was recorded |
| `end_posted` | whether the end was recorded; `null` when the command was not run and no change was closed |
| `ref` | the stored ref; `null` when nothing was stored |
| `producer_ref` | the plain ref |
| `outcome` | `completed`, `failed` or `cancelled`; `cancelled` too when the command was not run but the change was closed |
| `exit_code` | `horo run`'s own exit code |
| `reason` | why the change was not recorded in full; `null` when it was |

The report is written on every path past argument parsing. If it cannot be written, `horo run`
warns and keeps its exit code.

## `horo event`

    horo event --type incident.resolved --subject acme/prod --title "Queue drained"

Records one event.

| flag | default | meaning |
| --- | --- | --- |
| `--lifecycle` | | `started`, `completed`, `failed`, `rolled_back`, `cancelled` or `none`; see [Type Suffixes](http-api.md#type-suffixes-and-the-lifecycle-field) |
| `--occurred-at` | now | when it happened, RFC 3339 |
| `--idempotency-key` | one per invocation | the key a retried post repeats |

## `horo refold`

    horo refold ci/dep-1

Rebuilds one stored change from its events, through
[`POST /v1/admin/refold`](http-api.md#post-v1adminrefold), and prints its answer. It exits `3`
when the ref has no events.

| flag | default | meaning |
| --- | --- | --- |
| `--ingress-url` | `$HOROPTER_INGRESS_URL` | ingress's base URL |
| `--key` | `$HOROPTER_ADMIN_KEY` | a key holding `admin`; never `$HOROPTER_INGRESS_KEY` |
| `--tenant` | | the tenant to act in, for a global key |

## Version and Build Information

Every Horopter binary, `horo`, `horoctl` and the daemons alike, answers `--version` with the
release it was built from (`horo v0.13.0`) and `--build-info` with the release, the commit, the
commit's time, whether the tree was modified, the Go version, the OS and the architecture.
Either must be the first argument.

| flag | meaning |
| --- | --- |
| `--format text` | the default |
| `--format json`, `--json` | JSON; the version without its `v`, `"version": "0.13.0"` |
| `--format bare` | the version alone, `0.13.0`, for a script to compare; `--version` only |

A binary built by `go install …@v0.13.0` reports the module version.
