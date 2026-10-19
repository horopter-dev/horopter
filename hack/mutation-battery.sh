#!/usr/bin/env bash
# Patch headers git apply ignores: "# package: <pkg>..." and "# expect: <test>...".
set -euo pipefail

usage() {
  echo "usage: hack/mutation-battery.sh <patch-dir> [name-filter]" >&2
  exit 2
}

[[ $# -ge 1 ]] || usage
dir=$1
filter=${2:-}
[[ -d $dir ]] || {
  echo "no such directory: $dir" >&2
  exit 2
}
if [[ -z ${HOROPTER_TEST_DSN:-} ]]; then
  echo "HOROPTER_TEST_DSN is unset: the database tests would skip, and every mutant they" \
    "guard would look like a survivor. Run docker compose up -d db and set it." >&2
  exit 2
fi
if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "the working tree has uncommitted changes: commit them first, so that reverting a" \
    "mutant can never touch your own work" >&2
  exit 2
fi

applied=""
revert() {
  if [[ -n $applied ]]; then
    git apply -R "$applied"
    applied=""
  fi
}
trap revert EXIT INT TERM

field() { sed -n "s/^# $1: //p" "$2" | head -n 1; }

caught=0
survived=0
broken=0
for patch in "$dir"/*.patch; do
  name=$(basename "$patch" .patch)
  [[ -z $filter || $name == *"$filter"* ]] || continue
  pkg=$(field package "$patch")
  expect=$(field expect "$patch")
  if ! git apply "$patch" 2> /dev/null; then
    echo "BROKEN    $name: the patch no longer applies; re-derive it against the current code"
    broken=$((broken + 1))
    continue
  fi
  applied=$patch
  log=$(mktemp)
  read -ra pkgs <<< "$pkg"
  go test "${pkgs[@]}" -count=1 -v > "$log" 2>&1 && status=0 || status=$?
  revert
  failed=$(sed -n 's/^ *--- FAIL: \([^ ]*\).*/\1/p' "$log" | grep -v / | paste -sd, - || true)
  skipped=$(grep -c -- '--- SKIP' "$log" || true)
  if grep -q '\[build failed\]\|\[setup failed\]' "$log"; then
    echo "BROKEN    $name: the mutant does not compile; see $log"
    broken=$((broken + 1))
  elif [[ $status -ne 0 && -n $failed ]]; then
    echo "CAUGHT    $name: by $failed (expected $expect)"
    caught=$((caught + 1))
    rm -f "$log"
  else
    echo "SURVIVED  $name: no test failed ($skipped skipped; expected $expect); see $log"
    survived=$((survived + 1))
  fi
done

echo "caught $caught, survived $survived, broken $broken"
[[ $survived -eq 0 && $broken -eq 0 ]]
