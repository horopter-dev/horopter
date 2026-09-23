#!/usr/bin/env bash
# Writes .goreleaser.snapshot.yaml, a copy of .goreleaser.yaml whose ko images target the
# host architecture only, so `task up:local` produces images that run natively.
#
# It exists because goreleaser does not template kos.platforms: a "{{ if .IsSnapshot }}"
# there is handed to ko verbatim and fails with "no matching platforms in base image
# index". The conditional therefore cannot live in the config and has to live in what
# invokes it. Release builds read .goreleaser.yaml unchanged and still publish both.
#
# ko loads a single platform into the Docker daemon, taking the first entry of a
# multi-platform list, so [linux/amd64, linux/arm64] yields an amd64 image on every host.
set -euo pipefail

arch="$(go env GOARCH)"
multi='platforms: [linux/amd64, linux/arm64]'
single="platforms: [linux/${arch}]"

# grep -c exits 1 on zero matches, which set -e would turn into a silent abort before the
# error below could explain it. The counts are the diagnostic, so never let them be fatal.
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
