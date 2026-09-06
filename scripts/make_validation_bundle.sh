#!/usr/bin/env bash
# ============================================================================
#  cs2a validation bundle
#
#  Packages the current working tree (with local dist/ binaries) into a
#  single tarball that can be copied to a VPS and rerun there, to validate a
#  fix before anything is committed. This script is not part of the release
#  flow: it exists so "build, deploy, validate" is one command during a fix
#  round.
#
#  Usage (on the dev machine):
#    bash scripts/make_validation_bundle.sh [output.tar.gz]
#
#  Then on the VPS:
#    tar -xzf cs2a-validation.tar.gz -C /tmp
#    sudo bash /tmp/cs2a-validation/scripts/bootstrap.sh
#
#  The bundle carries the already-built dist/, so bootstrap.sh installs the
#  local binaries instead of downloading a release.
# ============================================================================
set -euo pipefail

OUT="${1:-dist/cs2a-validation.tar.gz}"
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# Resolve OUT relative to the repo root (an absolute OUT stays absolute).
case "$OUT" in
  /*) BUNDLE="$OUT" ;;
  *)  BUNDLE="$ROOT/$OUT" ;;
esac

# The binaries must exist: the bundle installs them on the target machine.
[[ -f $ROOT/dist/cs2a-agent && -f $ROOT/dist/cs2a-panel ]] || {
  printf 'cs2a: dist/ has no binaries — run `make build` first\n' >&2
  exit 1
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
STAGE="$TMP/cs2a-validation"
# bootstrap.sh picks up local binaries from ../dist relative to its own
# directory ($SCRIPT_DIR/../dist), so the bundle must preserve the repo
# layout: scripts/ beside dist/.
mkdir -p "$STAGE/scripts" "$STAGE/dist"

# Everything bootstrap.sh needs at runtime: itself, the sibling scripts it
# references, and the dist/ binaries it prefers over a GitHub download.
cp "$ROOT/scripts/bootstrap.sh" "$ROOT/scripts/install.sh" "$ROOT/scripts/uninstall.sh" "$STAGE/scripts/"
cp "$ROOT/dist/cs2a-agent" "$ROOT/dist/cs2a-panel" "$STAGE/dist/"

mkdir -p "$(dirname "$BUNDLE")"
tar -czf "$BUNDLE" -C "$TMP" cs2a-validation
printf 'bundle: %s\n' "$BUNDLE"
printf 'copy it to the VPS, then:\n'
printf '  tar -xzf %s -C /tmp\n' "$(basename "$BUNDLE")"
printf '  sudo bash /tmp/cs2a-validation/scripts/bootstrap.sh\n'
