#!/usr/bin/env bash
# goreleaser does not template kos.platforms, so snapshots narrow it to the host arch here.
set -euo pipefail

arch="$(go env GOARCH)"
multi='platforms: [linux/amd64, linux/arm64]'
single="platforms: [linux/${arch}]"

# grep -c exits 1 on zero matches; set -e would abort before the error below explains it.
want="$(grep -c -F "$multi" .goreleaser.yaml || true)"
[[ $want -gt 0 ]] || {
  echo "snapshot-config.sh: no '$multi' in .goreleaser.yaml; the ko platform list changed" >&2
  exit 1
}

# The brackets are escaped because sed reads an unescaped [...] as a character class.
sed "s|platforms: \[linux/amd64, linux/arm64\]|$single|" .goreleaser.yaml \
  > .goreleaser.snapshot.yaml

got="$(grep -c -F "$single" .goreleaser.snapshot.yaml || true)"
[[ $got -eq $want ]] || {
  echo "snapshot-config.sh: rewrote $got of $want platform lists" >&2
  exit 1
}
