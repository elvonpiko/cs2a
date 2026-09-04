#!/usr/bin/env bash
# Shell tests for the cs2a installer scripts.
# Usage: bash tests/bootstrap_test.sh
set -uo pipefail
FAILED=0
check() { # check <label> <cmd...>
  local label=$1; shift
  if "$@" >/dev/null 2>&1; then
    echo "  ok  $label"
  else
    echo "FAIL  $label"
    FAILED=1
  fi
}

echo "== bootstrap.sh =="
check "syntax"           bash -n scripts/bootstrap.sh
check "uninstall syntax" bash -n scripts/uninstall.sh
check "uninstall help"   grep -q "purge-game" scripts/uninstall.sh

echo
echo "== idempotent helpers =="
# ensure_user / ensure_line_in_file are defined inline; test their logic here
ensure_line_in_file() { grep -qxF "$2" "$1" 2>/dev/null || echo "$2" >> "$1"; }
TMP=$(mktemp)
ensure_line_in_file "$TMP" "alpha"
ensure_line_in_file "$TMP" "beta"
ensure_line_in_file "$TMP" "alpha"   # duplicate must not double-append
count=$(grep -c alpha "$TMP")
check "ensure_line_in_file dedupes" test "$count" = "1"
rm -f "$TMP"

echo
echo "== unit template consistency =="
check "game unit uses -usercon"    grep -q -- "-usercon" scripts/bootstrap.sh
check "game unit binds 0.0.0.0"    grep -q -- "+ip 0.0.0.0" scripts/bootstrap.sh
check "agent binds loopback"       grep -q '"listen": "127.0.0.1:\$CS2A_AGENT_PORT"' scripts/bootstrap.sh
check "panel env file is secret"   grep -q "panel.env" scripts/bootstrap.sh
check "steamcmd app id 730"        grep -q "app_update \"\$CS2A_APP_ID\"" scripts/bootstrap.sh
check "steamclient.so sdk64 link"  grep -q "sdk64" scripts/bootstrap.sh
check "firewall opens game port"   grep -q "ufw allow \"\$CS2A_GAME_PORT/udp\"" scripts/bootstrap.sh
check "caddy domain question"      grep -q 'PANEL_DOMAIN' scripts/bootstrap.sh
check "caddy reverse_proxy block"  grep -q "reverse_proxy 127.0.0.1:\$CS2A_PANEL_PORT" scripts/bootstrap.sh
check "caddy closes raw port"      grep -q 'ufw delete allow' scripts/bootstrap.sh
check "fallback keeps direct port" grep -q 'CS2A_PANEL_PORT/tcp' scripts/bootstrap.sh
check "asks existing unit name"    grep -q 'Name of your EXISTING CS2 systemd unit' scripts/bootstrap.sh
check "never clobbers game unit"   grep -q 'keeping your existing.*untouched' scripts/bootstrap.sh
check "wp_dsn optional field"      grep -q 'wp_dsn' scripts/bootstrap.sh
check "mariadb provisioning"       grep -q 'CREATE DATABASE IF NOT EXISTS cs2_wp' scripts/bootstrap.sh
check "game port is asked"         grep -q 'Game port (A2S + RCON' scripts/bootstrap.sh
check "detects existing install"   grep -q 'detect_existing' scripts/bootstrap.sh
check "parses rcon from cfg"       grep -q 'rcon_password' scripts/bootstrap.sh
check "reuse mode default on detect" grep -q 'DETECTED_OK=1' scripts/bootstrap.sh
check "install wrapper exists"     test -x scripts/install.sh
check "wrapper re-attaches tty"    grep -q '/dev/tty' scripts/install.sh
check "wrapper pins version"       grep -q 'CS2A_VERSION' scripts/install.sh
check "wrapper unattended fallback" grep -q -- '--unattended' scripts/install.sh
check "pages workflow ships site"  grep -q 'deploy-pages' .github/workflows/pages.yml
check "landing page has copy btn"  grep -q 'copy' site/index.html

echo
echo "== go-side plan package =="
( . scripts/devenv.sh && go test ./internal/bootstrap/ -count=1 ) || FAILED=1

echo
if [[ $FAILED -eq 0 ]]; then echo "ALL SHELL TESTS PASSED"; else echo "SHELL TESTS FAILED"; exit 1; fi
