#!/usr/bin/env bash
# Renders deploy/docker-compose.yaml into .render/, with the release's tag substituted for
# ${HOROPTER_IMAGE_TAG}. The released file names exact image versions, so whoever downloads
# it sets no variables and looks nothing up.
#
# Writes to .render/, not dist/: GoReleaser's `before.hooks` (which calls this script) run
# before GoReleaser's own "dist must be empty" check, so a hook that writes into dist/ always
# fails that check. release.extra_files does not require the file live under dist/, so it
# stays out of GoReleaser's way here.
set -euo pipefail

tag="${1:?usage: render-compose.sh <tag>}"
mkdir -p .render

# Escapes backslash, &, and the | delimiter, any of which would otherwise be read as sed
# syntax inside the replacement rather than as literal characters of the tag.
escaped_tag="$(printf '%s' "$tag" | sed -e 's/[\&|]/\\&/g')"
sed "s|\${HOROPTER_IMAGE_TAG}|${escaped_tag}|g" deploy/docker-compose.yaml \
  > .render/docker-compose.yaml

# A rendered file that still holds the variable would publish a stack nobody can start.
if grep -q 'HOROPTER_IMAGE_TAG' .render/docker-compose.yaml; then
  echo "render-compose.sh: .render/docker-compose.yaml still references HOROPTER_IMAGE_TAG" >&2
  exit 1
fi
