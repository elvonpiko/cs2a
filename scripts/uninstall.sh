#!/usr/bin/env bash
# ============================================================================
#  cs2a uninstaller — removes the panel and agent, and optionally the game.
#
#  Usage:
#    sudo bash uninstall.sh                # remove cs2a, keep config + game
#    sudo bash uninstall.sh --purge        # also remove /opt/cs2a/{etc,var}
#    sudo bash uninstall.sh --purge-game   # also remove the CS2 install (~40 GB)
#    sudo bash uninstall.sh --yes          # skip the confirmation prompt
#
#  A CS2 unit cs2a did not write is never touched: the installer marks its own
#  unit with "managed by cs2a", and anything else is left for its owner.
# ============================================================================
set -euo pipefail

PURGE_CONFIG=0
PURGE_GAME=0
ASSUME_YES=0
for arg in "$@"; do
  case "$arg" in
    --purge)      PURGE_CONFIG=1 ;;
    --purge-game) PURGE_GAME=1; PURGE_CONFIG=1 ;;
    --yes|-y)     ASSUME_YES=1 ;;
    -h|--help)    sed -n '2,13p' "$0"; exit 0 ;;
    *) printf 'cs2a: unknown option %q (try --help)\n' "$arg" >&2; exit 2 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "cs2a: run me as root (sudo bash uninstall.sh)"; exit 1; }

CS2A_ROOT="${CS2A_ROOT:-/opt/cs2a}"
GAME_UNIT="${CS2A_SERVICE_GAME:-cs2-server}"
# The unit cs2a was actually configured for, so an adopted server is recognised
# instead of being compared against the default name.
if [[ -z ${CS2A_SERVICE_GAME:-} && -f $CS2A_ROOT/etc/agent.json ]]; then
  svc=$(sed -n 's/.*"service_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CS2A_ROOT/etc/agent.json" | head -1)
  [[ -n ${svc:-} ]] && GAME_UNIT="$svc"
fi
GAME_UNIT_FILE="/etc/systemd/system/$GAME_UNIT.service"
# cs2a's -usercon drop-in belongs to cs2a even when the unit does not.
USERCON_DROPIN="/etc/systemd/system/$GAME_UNIT.service.d/10-cs2a-usercon.conf"

# The game dir comes from agent.json when it is there, so --purge-game removes
# what cs2a actually installed rather than a guessed path.
CS2_DIR=""
if [[ -f $CS2A_ROOT/etc/agent.json ]]; then
  CS2_DIR=$(sed -n 's/.*"cs2_dir"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$CS2A_ROOT/etc/agent.json" | head -1)
fi
[[ -n $CS2_DIR ]] || CS2_DIR="$CS2A_ROOT/cs2"

# cs2a only removes a game unit it wrote itself.
OWN_GAME_UNIT=0
if [[ -f $GAME_UNIT_FILE ]] && grep -q "managed by cs2a" "$GAME_UNIT_FILE"; then
  OWN_GAME_UNIT=1
fi

echo "cs2a: this will remove"
echo "  - cs2a-panel.service and cs2a-agent.service"
echo "  - $CS2A_ROOT/bin"
[[ $OWN_GAME_UNIT -eq 1 ]] && echo "  - $GAME_UNIT.service (written by cs2a)"
[[ -f $USERCON_DROPIN ]] && echo "  - $USERCON_DROPIN (the -usercon drop-in cs2a added)"
[[ -f /etc/caddy/cs2a.caddyfile ]] && echo "  - /etc/caddy/cs2a.caddyfile and its import line"
[[ $PURGE_CONFIG -eq 1 ]] && echo "  - $CS2A_ROOT/etc and $CS2A_ROOT/var (config, database, credentials)"
[[ $PURGE_GAME -eq 1 ]] && echo "  - $CS2_DIR (the CS2 install — this cannot be undone)"
if [[ $OWN_GAME_UNIT -eq 0 && -f $GAME_UNIT_FILE ]]; then
  echo "  (keeping $GAME_UNIT.service — cs2a did not write it)"
fi
if [[ $PURGE_CONFIG -eq 0 ]]; then
  echo "  (keeping $CS2A_ROOT/etc and $CS2A_ROOT/var — pass --purge to remove them)"
fi
if [[ $PURGE_GAME -eq 0 ]]; then
  echo "  (keeping the CS2 install at $CS2_DIR — pass --purge-game to remove it)"
fi

if [[ $ASSUME_YES -eq 0 ]]; then
  read -r -p "Continue? [y/N] " reply || true
  [[ ${reply,,} == y* ]] || { echo "cs2a: aborted — nothing was changed."; exit 0; }
fi

echo "cs2a: stopping services…"
systemctl disable --now cs2a-panel.service cs2a-agent.service 2>/dev/null || true
[[ $OWN_GAME_UNIT -eq 1 ]] && systemctl disable --now "$GAME_UNIT.service" 2>/dev/null || true

echo "cs2a: removing units and binaries…"
rm -f /etc/systemd/system/cs2a-panel.service /etc/systemd/system/cs2a-agent.service
[[ $OWN_GAME_UNIT -eq 1 ]] && rm -f "$GAME_UNIT_FILE"
# The drop-in is cs2a's even on an adopted unit: leaving it behind would keep
# changing someone else's launch line after cs2a is gone.
if [[ -f $USERCON_DROPIN ]]; then
  rm -f "$USERCON_DROPIN"
  rmdir "$(dirname "$USERCON_DROPIN")" 2>/dev/null || true
  echo "cs2a: removed the -usercon drop-in (restart $GAME_UNIT.service to apply)"
fi
systemctl daemon-reload
rm -rf "$CS2A_ROOT/bin" "$CS2A_ROOT/cache"

if [[ $PURGE_CONFIG -eq 1 ]]; then
  echo "cs2a: removing config and data…"
  rm -rf "$CS2A_ROOT/etc" "$CS2A_ROOT/var"
fi

if [[ $PURGE_GAME -eq 1 ]]; then
  if [[ -f "$CS2_DIR/game/csgo/gameinfo.gi" ]]; then
    echo "cs2a: removing the CS2 install at $CS2_DIR…"
    rm -rf "$CS2_DIR"
  else
    echo "cs2a: $CS2_DIR does not look like a CS2 install — leaving it alone."
  fi
fi

# Only prune an empty root, so a leftover game dir is never taken with it.
rmdir "$CS2A_ROOT" 2>/dev/null && echo "cs2a: removed $CS2A_ROOT" || true

echo "cs2a: done."
# cs2a owns its own Caddy file, so this can be cleaned up exactly instead of
# telling the operator to go find a site block by hand.
if [[ -f /etc/caddy/cs2a.caddyfile ]]; then
  rm -f /etc/caddy/cs2a.caddyfile
  if [[ -f /etc/caddy/Caddyfile ]] && grep -qxF 'import /etc/caddy/cs2a.caddyfile' /etc/caddy/Caddyfile; then
    cp -a /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.cs2a-backup.$(date +%s)"
    grep -vxF 'import /etc/caddy/cs2a.caddyfile' /etc/caddy/Caddyfile > /etc/caddy/Caddyfile.cs2a-tmp &&
      mv -f /etc/caddy/Caddyfile.cs2a-tmp /etc/caddy/Caddyfile
    echo "cs2a: removed the panel site from /etc/caddy/Caddyfile — reload caddy: systemctl reload caddy"
  fi
elif [[ -f /etc/caddy/Caddyfile ]] && grep -q "cs2a" /etc/caddy/Caddyfile; then
  echo "cs2a: an older cs2a site block may still be in /etc/caddy/Caddyfile — remove it by hand if you no longer need it."
fi
