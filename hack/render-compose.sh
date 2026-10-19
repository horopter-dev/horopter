#!/usr/bin/env bash
# Writes .render/, not dist/: GoReleaser's before hooks run before its dist-must-be-empty check.
set -euo pipefail

tag="${1:?usage: render-compose.sh <tag>}"
mkdir -p .render

# Escapes \, & and the | delimiter, which sed would read as replacement syntax.
escaped_tag="$(printf '%s' "$tag" | sed -e 's/[\&|]/\\&/g')"
sed "s|\${HOROPTER_IMAGE_TAG}|${escaped_tag}|g" deploy/docker-compose.yaml \
  > .render/docker-compose.yaml

# A rendered file that still holds the variable would publish a stack nobody can start.
if grep -q 'HOROPTER_IMAGE_TAG' .render/docker-compose.yaml; then
  echo "render-compose.sh: .render/docker-compose.yaml still references HOROPTER_IMAGE_TAG" >&2
  exit 1
fi
