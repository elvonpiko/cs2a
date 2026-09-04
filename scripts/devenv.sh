#!/usr/bin/env sh
# Source this in dev shells: . scripts/devenv.sh
# Keeps Go caches in one place; override via environment if you prefer.
export GOCACHE="${GOCACHE:-/tmp/cs2a-go/gocache}"
export GOMODCACHE="${GOMODCACHE:-/tmp/cs2a-go/gomodcache}"
export GOPATH="${GOPATH:-/tmp/cs2a-go/gopath}"
export GOTMPDIR="${GOTMPDIR:-/tmp/cs2a-go/gotmp}"
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOPATH" "$GOTMPDIR"
