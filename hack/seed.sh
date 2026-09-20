#!/usr/bin/env bash
# Seeds a Horopter stack with changes in all three shapes — closed, open and point — so a
# dashboard has something to show and every rendering has something to render.
#
# Each run uses a fresh run id in its idempotency keys and correlation refs, so running it
# twice adds changes rather than colliding. The development database is never truncated,
# which is what makes that important.
set -euo pipefail

url="${HOROPTER_INGRESS_URL:-http://localhost:8080}"
token="${HOROPTER_INGRESS_TOKEN:-local-dev-token}"
run="$(date +%s)"
# A hex rendering of run, so a dashboard showing the commit attribute sees something shaped
# like a commit ref instead of a bare epoch-second number. run itself still does the
# uniqueness work, in idempotency keys and correlation refs below.
commit="$(printf '%07x' "$run")"

for tool in curl jq; do
  command -v "$tool" > /dev/null || {
    echo "seed.sh needs $tool" >&2
    exit 1
  }
done

# minutes_ago renders an RFC 3339 timestamp N minutes before now, on BSD date (macOS) or
# GNU date (Linux, and the containers).
minutes_ago() {
  date -u -v -"${1}"M +%Y-%m-%dT%H:%M:%SZ 2> /dev/null \
    || date -u -d "${1} minutes ago" +%Y-%m-%dT%H:%M:%SZ
}

# post <tenant> <source> <type> <subject> <minutes-ago> <ref-or-empty> <title>
post() {
  local tenant=$1 source=$2 type=$3 subject=$4 ago=$5 ref=$6 title=$7
  local occurred body status
  occurred="$(minutes_ago "$ago")"

  body="$(jq -n \
    --arg tenant "$tenant" --arg source "$source" --arg type "$type" \
    --arg subject "$subject" --arg occurred "$occurred" --arg title "$title" \
    --arg key "${run}-${ref:-$type}-${ago}" --arg ref "$ref" --arg run "$run" \
    --arg commit "$commit" \
    '{tenant: $tenant, source: $source, type: $type, subject: $subject,
      occurred_at: $occurred, title: $title, actor: "seed.sh",
      idempotency_key: $key,
      attributes: {
        commit: $commit,
        version: "1.4.0",
        "horopter.description": "seeded by hack/seed.sh run \($run)"
      }}
     + (if $ref == "" then {} else {correlation_ref: "\($run)-\($ref)"} end)')"

  status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$url/v1/events" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d "$body")"

  if [[ $status != 201 ]]; then
    echo "seed.sh: POST /v1/events returned $status for $type ${ref:-(point)}" >&2
    exit 1
  fi
  echo "posted $tenant $type ${ref:-(point)} at $occurred"
}

# Six closed changes: a start and a terminal event sharing a correlation ref.
post acme ci deployment.started acme/prod 330 dep-1 "Deploy api v1.4.0 to prod"
post acme ci deployment.completed acme/prod 324 dep-1 "Deploy api v1.4.0 to prod"
post acme ci deployment.started acme/prod 280 dep-2 "Deploy web v2.1.7 to prod"
post acme ci deployment.failed acme/prod 276 dep-2 "Deploy web v2.1.7 to prod"
post acme terraform infrastructure.started acme/prod 240 tf-1 "Apply network module"
post acme terraform infrastructure.completed acme/prod 231 tf-1 "Apply network module"
post globex flux reconciliation.started globex/staging 200 flux-1 "Reconcile ingress-nginx"
post globex flux reconciliation.completed globex/staging 198 flux-1 "Reconcile ingress-nginx"
post globex flux reconciliation.started globex/staging 150 flux-2 "Reconcile cert-manager"
post globex flux reconciliation.rolled_back globex/staging 146 flux-2 "Reconcile cert-manager"
post globex ci deployment.started globex/prod 120 dep-3 "Deploy worker v0.9.2"
post globex ci deployment.completed globex/prod 110 dep-3 "Deploy worker v0.9.2"

# Three open changes: a start with nothing terminal yet.
post acme ci deployment.started acme/staging 90 dep-4 "Deploy api v1.5.0-rc1 to staging"
post globex flux reconciliation.started globex/prod 45 flux-3 "Reconcile monitoring stack"
post acme terraform infrastructure.started acme/staging 20 tf-2 "Apply database module"

# Three points: one event, no correlation ref, a terminal type, so start and end coincide.
post acme ci incident.resolved acme/prod 175 "" "Checkout latency back to normal"
post globex ci incident.resolved globex/prod 95 "" "Queue backlog cleared"
post acme ci configuration.completed acme/prod 30 "" "Feature flag checkout-v2 enabled"

echo "seeded 12 changes for run $run"
