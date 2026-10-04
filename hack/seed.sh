#!/usr/bin/env bash
# Seeds a Horopter stack with changes in all three shapes — closed, open and point — so a
# dashboard has something to show and every rendering has something to render.
#
# Each run uses a fresh run id in its idempotency keys and correlation refs, so running it
# twice adds changes rather than colliding. The development database is never truncated,
# which is what makes that important.
set -euo pipefail

url="${HOROPTER_INGRESS_URL:-http://localhost:8080}"
# Defaults to the compose stack's own fixed dev producer key (docker-compose.yaml's
# bootstrap service); set HOROPTER_DEV_PRODUCER_KEY to override it, e.g. after issuing your
# own credential with `horoctl admin keys issue --role producer`.
token="${HOROPTER_DEV_PRODUCER_KEY:-horo_dead00000001_00000000000000000000000000000000000000000000000000000000000000}"
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

# post <type> <subject> <minutes-ago> <ref-or-empty> <title>
#
# tenant and source are not parameters: both are derived from the producer credential, and
# the ingress API rejects a request body that names either. Every event this script posts lands in
# whatever tenant HOROPTER_DEV_PRODUCER_KEY is bound to — the bootstrap service's "default"
# tenant by default — and the credential's locus ("dev") is what prefixes every stored ref
# below, points included since 0006, so "dep-1" is stored and queried back as
# "dev/<run>-dep-1", and a point is stored as "dev/<its own event id>".
post() {
  local type=$1 subject=$2 ago=$3 ref=$4 title=$5
  local occurred body status
  occurred="$(minutes_ago "$ago")"
  # deployment.environment.name is the one attribute key this branch makes filterable, so
  # the seed data has to vary it. subject is already "<team>/<environment>", which gives
  # prod and staging alternating across the seeded changes for free.
  local env="${subject##*/}"

  body="$(jq -n \
    --arg type "$type" \
    --arg subject "$subject" --arg occurred "$occurred" --arg title "$title" \
    --arg key "${run}-${ref:-$type}-${ago}" --arg ref "$ref" --arg run "$run" \
    --arg commit "$commit" --arg env "$env" \
    '{type: $type, subject: $subject,
      occurred_at: $occurred, title: $title, actor: "seed.sh",
      idempotency_key: $key,
      attributes: {
        commit: $commit,
        version: "1.4.0",
        "horopter.description": "seeded by hack/seed.sh run \($run)",
        "deployment.environment.name": $env
      }}
     + (if $ref == "" then {} else {correlation_ref: "\($run)-\($ref)"} end)')"

  status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$url/v1/events" \
    -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d "$body")"

  if [[ $status != 201 ]]; then
    echo "seed.sh: POST /v1/events returned $status for $type ${ref:-(point)}" >&2
    exit 1
  fi
  echo "posted $type ${ref:-(point)} at $occurred"
}

# Six closed changes: a start and a terminal event sharing a correlation ref.
post deployment.started acme/prod 330 dep-1 "Deploy api v1.4.0 to prod"
post deployment.completed acme/prod 324 dep-1 "Deploy api v1.4.0 to prod"
post deployment.started acme/prod 280 dep-2 "Deploy web v2.1.7 to prod"
post deployment.failed acme/prod 276 dep-2 "Deploy web v2.1.7 to prod"
post infrastructure.started acme/prod 240 tf-1 "Apply network module"
post infrastructure.completed acme/prod 231 tf-1 "Apply network module"
post reconciliation.started globex/staging 200 flux-1 "Reconcile ingress-nginx"
post reconciliation.completed globex/staging 198 flux-1 "Reconcile ingress-nginx"
post reconciliation.started globex/staging 150 flux-2 "Reconcile cert-manager"
post reconciliation.rolled_back globex/staging 146 flux-2 "Reconcile cert-manager"
post deployment.started globex/prod 120 dep-3 "Deploy worker v0.9.2"
post deployment.completed globex/prod 110 dep-3 "Deploy worker v0.9.2"

# Three open changes: a start with nothing terminal yet.
post deployment.started acme/staging 90 dep-4 "Deploy api v1.5.0-rc1 to staging"
post reconciliation.started globex/prod 45 flux-3 "Reconcile monitoring stack"
post infrastructure.started acme/staging 20 tf-2 "Apply database module"

# Three points: one event, no correlation ref, a terminal type, so start and end coincide.
post incident.resolved acme/prod 175 "" "Checkout latency back to normal"
post incident.resolved globex/prod 95 "" "Queue backlog cleared"
post configuration.completed acme/prod 30 "" "Feature flag checkout-v2 enabled"

echo "seeded 12 changes for run $run"
