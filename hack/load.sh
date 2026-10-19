#!/usr/bin/env bash
# To shed, run the stack with pool_max_conns=1 or HOROPTER_EGRESS_READ_MEMORY_BYTES=1048576.
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

# -v-6H is BSD date (macOS); GNU date (Linux, and the containers) needs -d instead.
from_rfc="$(date -u -v-6H +%Y-%m-%dT%H:%M:%SZ 2> /dev/null \
  || date -u -d '6 hours ago' +%Y-%m-%dT%H:%M:%SZ)"
to_rfc="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
from_ms="$((($(date +%s) - 21600) * 1000))"
to_ms="$(($(date +%s) * 1000))"

tenant_param=""
[[ -n $tenant ]] && tenant_param="&tenant=${tenant}"

paths=(
  "/v1/changes?from=${from_rfc}&to=${to_rfc}&limit=1000${tenant_param}"
  "/v1/events?limit=1000${tenant_param}"
  "/adapters/grafana/annotations?from=${from_ms}&to=${to_ms}${tenant_param}"
  "/adapters/otlp/spans?from=${from_rfc}&to=${to_rfc}${tenant_param}"
)

# `|| true` must print nothing: curl -w already prints 000, so a failure would count twice.
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
