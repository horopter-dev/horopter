#!/usr/bin/env bash
# Drives concurrent reads at all four renderings, so the read-admission metrics move and the
# counters can be seen to be real.
#
# /v1/changes and /adapters/otlp/spans require RFC3339 from/to; /adapters/grafana/annotations
# requires the same window in epoch milliseconds instead; /v1/events takes no window at all —
# EventQuery has no From/To, so passing one is pointless and mixing formats elsewhere gets a
# 400. Every route also requires a credential now. HOROPTER_DEV_READER_KEY defaults to the
# compose stack's own fixed dev reader key, tenant-scoped to the bootstrap "default" tenant,
# so by default HOROPTER_TENANT is left unset and "tenant" is omitted from every query
# entirely — a tenant-scoped credential reads its own tenant with no "?tenant=" needed, and
# naming one explicitly only works if it is that credential's own (otherwise 403). Set
# HOROPTER_TENANT only alongside a global credential in HOROPTER_DEV_READER_KEY, e.g. one
# issued with `horoctl admin keys issue --role reader --source dev --locus dev` (no
# --tenant), which must then name a tenant to read.
#
# It will not shed against a healthy local Postgres: shedding needs connection waits above
# HOROPTER_EGRESS_DB_WAIT_THRESHOLD, and an idle database has none. To exercise the
# interesting paths, restart the stack with pool_max_conns=1 in DATABASE_URL (reads then
# queue for connections) or HOROPTER_EGRESS_READ_MEMORY_BYTES=1048576, the minimum, which
# makes ordinary reads too large.
#
# - pool_max_conns=1, measured: `load.sh 20 50` and `load.sh 60 100` shed nothing, since a
#   seeded query takes a few milliseconds and the queue drains inside
#   HOROPTER_EGRESS_ADMISSION_WAIT; `load.sh 150 200` shed, all at the concurrency gate, with
#   "limit":4 in the `read shed` line, the ceiling for a pool of one connection.
# - The minimum pool cuts ordinary seeded reads: 200 with X-Horopter-Truncated: true, and no
#   log line, since only the 422 and 503 paths log at WARN. A 422 needs a single row whose
#   text, times the 46x admission overhead, exceeds the pool by itself: roughly 23 KB of row
#   text, well past anything hack/seed.sh writes.
set -euo pipefail

url="${HOROPTER_EGRESS_URL:-http://localhost:8081}"
tenant="${HOROPTER_TENANT:-}"
key="${HOROPTER_DEV_READER_KEY:-horo_dead00000002_11111111111111111111111111111111111111111111111111111111111111}"
concurrency="${1:-8}"
iterations="${2:-50}"

command -v curl > /dev/null || {
  echo "load.sh needs curl" >&2
  exit 1
}

# from_rfc/to_rfc/from_ms/to_ms are the same six-hour window ending now, in the two formats
# the four renderings expect. -v-6H is BSD date (macOS); GNU date (Linux, and the containers)
# needs -d instead.
from_rfc="$(date -u -v-6H +%Y-%m-%dT%H:%M:%SZ 2> /dev/null \
  || date -u -d '6 hours ago' +%Y-%m-%dT%H:%M:%SZ)"
to_rfc="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
from_ms="$((($(date +%s) - 21600) * 1000))"
to_ms="$(($(date +%s) * 1000))"

# tenant_param is empty unless HOROPTER_TENANT is set, so a tenant-scoped reader key (the
# default) hits every route with no "?tenant=" at all rather than one that must happen to
# match the credential's own tenant id.
tenant_param=""
[[ -n $tenant ]] && tenant_param="&tenant=${tenant}"

paths=(
  "/v1/changes?from=${from_rfc}&to=${to_rfc}&limit=1000${tenant_param}"
  "/v1/events?limit=1000${tenant_param}"
  "/adapters/grafana/annotations?from=${from_ms}&to=${to_ms}${tenant_param}"
  "/adapters/otlp/spans?from=${from_rfc}&to=${to_rfc}${tenant_param}"
)

# one_worker runs every rendering once and prints one status code per line. curl's -w prints
# the code (000 on a connection failure) whether or not curl itself exits non-zero, so the
# trailing `|| true` only keeps a failed request from killing the script under set -e — it
# must not print anything of its own, or a single failure is counted twice. It is exported so
# xargs can run it in its own shell.
one_worker() {
  local path
  for path in "$@"; do
    curl -sS -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer ${key}" \
      "${url}${path}" || true
  done
}
export -f one_worker
export url key

tenant_label="${tenant:-its own tenant}"
echo "load: ${concurrency} workers x ${iterations} iterations over ${#paths[@]} renderings" \
  "(tenant=${tenant_label}, window ${from_rfc}..${to_rfc})"

seq "$iterations" \
  | xargs -P "$concurrency" -I{} bash -c 'one_worker "$@"' _ "${paths[@]}" \
  | sort | uniq -c | sort -rn
