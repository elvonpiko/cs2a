#!/usr/bin/env bash
# ============================================================================
#  cs2a uninstaller — removes the panel/agent and (optionally) the game data.
#  Usage: sudo bash uninstall.sh [--purge-game]
# ============================================================================
set -euo pipefail
PURGE_GAME=0
for arg in "$@"; do [[ $arg == --purge-game ]] && PURGE_GAME=1; done

CS2A_ROOT="${CS2A_ROOT:-/opt/cs2a}"
GAME_UNIT="${CS2A_SERVICE_GAME:-cs2-server}"

echo "cs2a: stopping services…"
systemctl disable --now cs2a-panel.service cs2a-agent.service 2>/dev/null || true
systemctl disable --now "$GAME_UNIT.service" 2>/dev/null || true

echo "cs2a: removing units and binaries…"
rm -f /etc/systemd/system/cs2a-panel.service /etc/systemd/system/cs2a-agent.service "/etc/systemd/system/$GAME_UNIT.service"
systemctl daemon-reload
rm -rf "$CS2A_ROOT/bin"

if [[ $PURGE_GAME -eq 1 ]]; then
  echo "cs2a: removing install root (including game data)…"
  rm -rf "$CS2A_ROOT"
else
  echo "cs2a: keeping $CS2A_ROOT/{etc,var} (config + data). Remove manually if desired."
fi
echo "cs2a: done."
