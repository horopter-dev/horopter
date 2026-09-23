#!/usr/bin/env bash
# Drives concurrent reads at all four renderings, so the read-admission metrics move and the
# counters can be seen to be real.
#
# /v1/changes and /adapters/otlp/spans require RFC3339 from/to; /adapters/grafana/annotations
# requires the same window in epoch milliseconds instead; /v1/events takes no window at all —
# EventQuery has no From/To, so passing one is pointless and mixing formats elsewhere gets a
# 400. Every route also requires a credential now, and a tenant-scoped one may only read its
# own tenant — HOROPTER_EGRESS_READER_KEY must be one scoped to (or a global credential naming)
# whatever HOROPTER_TENANT names, or every request 401s and this prints a 401 histogram
# instead of measuring anything. This drives a six-hour window ending now against a seeded
# tenant (acme by default; override with HOROPTER_TENANT).
#
# horoctl does not exist yet (Task 7), so there is no `horoctl admin keys issue` to point at:
# mint a reader credential by hand against the dev database, e.g.
#   INSERT INTO principal (id, display_name) VALUES ('prn_load', 'prn_load');
#   INSERT INTO credential (id, principal_id, type, key_prefix, key_hash, role, tenant_id,
#                           source, locus)
#     VALUES ('cred_load', 'prn_load', 'machine_key', '<prefix>', '<sha256 of secret>',
#             'reader', '<tenant id>', 'dev', 'dev');
# and export the resulting horo_<prefix>_<secret> key. Once Task 8 lands, this becomes
# `horoctl admin keys issue --role reader --tenant "$HOROPTER_TENANT"`.
#
# It will not shed against a healthy local Postgres: shedding needs connection waits above
# HOROPTER_EGRESS_DB_WAIT_THRESHOLD, and an idle database has none. To exercise the
# interesting paths, restart the stack with pool_max_conns=1 in DATABASE_URL (reads then
# queue for connections) or HOROPTER_EGRESS_READ_MEMORY_BYTES=1048576, the minimum, which
# makes ordinary reads too large. See the README.
set -euo pipefail

url="${HOROPTER_EGRESS_URL:-http://localhost:8081}"
tenant="${HOROPTER_TENANT:-acme}"
key="${HOROPTER_EGRESS_READER_KEY:?set HOROPTER_EGRESS_READER_KEY to a credential that can read tenant \"$tenant\"}"
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

paths=(
  "/v1/changes?from=${from_rfc}&to=${to_rfc}&tenant=${tenant}&limit=1000"
  "/v1/events?tenant=${tenant}&limit=1000"
  "/adapters/grafana/annotations?from=${from_ms}&to=${to_ms}&tenant=${tenant}"
  "/adapters/otlp/spans?from=${from_rfc}&to=${to_rfc}&tenant=${tenant}"
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

echo "load: ${concurrency} workers x ${iterations} iterations over ${#paths[@]} renderings" \
  "(tenant=${tenant}, window ${from_rfc}..${to_rfc})"

seq "$iterations" \
  | xargs -P "$concurrency" -I{} bash -c 'one_worker "$@"' _ "${paths[@]}" \
  | sort | uniq -c | sort -rn
