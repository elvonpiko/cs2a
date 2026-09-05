#!/usr/bin/env bash
# ============================================================================
#  cs2a bootstrap installer
#
#  Installs the cs2a control plane (panel + agent) on a Linux VPS and, when a
#  CS2 dedicated server is not already there, SteamCMD and the server too.
#
#  It discovers first and asks second: an existing CS2 install, its systemd
#  unit, game port, RCON password and GSLT are read off the machine, and every
#  install step is skipped when it is already satisfied. Reruns are safe — the
#  existing agent token, admin credentials and RCON password are reused rather
#  than rotated.
#
#  Usage:
#    sudo bash bootstrap.sh                  # discover, confirm, install
#    sudo bash bootstrap.sh --unattended      # no questions, all defaults
#    sudo bash bootstrap.sh --domain cs.gg    # panel behind Caddy + HTTPS
#    sudo bash bootstrap.sh --no-cs2          # never install the game server
#
#  Options:
#    --unattended            accept every default, never prompt
#    --domain <host>         serve the panel over HTTPS via Caddy
#    --no-cs2                do not install CS2 even if none is found
#    --with-cs2              install CS2 even in --unattended mode
#    --root <dir>            install root (default /opt/cs2a)
#    --panel-port <port>     panel HTTP port (default 8080)
#    --game-port <port>      CS2 game/RCON port (default 27015)
#    --skin-db               provision MariaDB for WeaponPaints skin sync
#    --no-firewall           do not touch ufw
#    -h, --help              show this header
#
#  Environment overrides: CS2A_ROOT, CS2A_PANEL_PORT, CS2A_AGENT_PORT,
#  CS2A_GAME_PORT, CS2A_SERVICE_GAME, CS2A_STEAM_USER, CS2A_PANEL_DOMAIN,
#  CS2A_GSLT, CS2A_RCON_PASSWORD, CS2A_ADMIN_USER, CS2A_VERSION.
# ============================================================================
set -euo pipefail

# ----------------------------- defaults ------------------------------------
CS2A_ROOT="${CS2A_ROOT:-/opt/cs2a}"
CS2A_SERVICE_GAME="${CS2A_SERVICE_GAME:-cs2-server}"
CS2A_AGENT_PORT="${CS2A_AGENT_PORT:-8100}"
CS2A_PANEL_PORT="${CS2A_PANEL_PORT:-8080}"
CS2A_GAME_PORT="${CS2A_GAME_PORT:-27015}"
CS2A_STEAM_USER="${CS2A_STEAM_USER:-steam}"
CS2A_APP_ID="${CS2A_APP_ID:-730}"
PANEL_DOMAIN="${CS2A_PANEL_DOMAIN:-}"
GSLT="${CS2A_GSLT:-}"
RCON_PASS="${CS2A_RCON_PASSWORD:-}"
SETUP_ADMIN="${CS2A_ADMIN_USER:-admin}"

UNATTENDED=0
WITH_CS2=-1        # -1 = decide from discovery, 0 = never, 1 = always
SETUP_SKIN_DB=-1   # -1 = ask, 0 = no, 1 = yes
TOUCH_FIREWALL=1

# CS2A_BOOTSTRAP_LIB=1 sources only the helpers and discovery functions and
# returns before anything is inspected or changed. The test suite uses it to
# exercise this script's logic directly.
LIB_MODE="${CS2A_BOOTSTRAP_LIB:-0}"

if [[ $LIB_MODE != 1 ]]; then
while [[ $# -gt 0 ]]; do
  case "$1" in
    --unattended)  UNATTENDED=1 ;;
    --no-cs2)      WITH_CS2=0 ;;
    --with-cs2)    WITH_CS2=1 ;;
    --skin-db)     SETUP_SKIN_DB=1 ;;
    --no-skin-db)  SETUP_SKIN_DB=0 ;;
    --no-firewall) TOUCH_FIREWALL=0 ;;
    --domain)      PANEL_DOMAIN="${2:-}"; shift ;;
    --domain=*)    PANEL_DOMAIN="${1#*=}" ;;
    --root)        CS2A_ROOT="${2:-}"; shift ;;
    --root=*)      CS2A_ROOT="${1#*=}" ;;
    --panel-port)  CS2A_PANEL_PORT="${2:-}"; shift ;;
    --panel-port=*) CS2A_PANEL_PORT="${1#*=}" ;;
    --game-port)   CS2A_GAME_PORT="${2:-}"; shift ;;
    --game-port=*) CS2A_GAME_PORT="${1#*=}" ;;
    -h|--help)     sed -n '2,35p' "$0"; exit 0 ;;
    *)             printf 'cs2a: unknown option %q (try --help)\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

[[ $EUID -eq 0 ]] || { echo "cs2a: run me as root (sudo bash bootstrap.sh)"; exit 1; }
fi

# ----------------------------- output helpers -------------------------------
if [[ -t 1 ]]; then
  BOLD=$'\e[1m'; DIM=$'\e[2m'; RESET=$'\e[0m'
  SIGNAL=$'\e[38;5;79m'; GREEN=$'\e[38;5;114m'; RED=$'\e[38;5;174m'; AMBER=$'\e[38;5;179m'
else
  BOLD=""; DIM=""; RESET=""; SIGNAL=""; GREEN=""; RED=""; AMBER=""
fi

banner() {
  printf '%s%s' "$BOLD" "$SIGNAL"
  cat <<'ART'
   ┌─┐┌─┐┌─┐┌─┐
   │  └─┐┌─┘├─┤   counter-strike 2 server manager
   └─┘└─┘└─┘┴ ┴
ART
  printf '%s' "$RESET"
  printf "%s   panel + agent installer%s\n\n" "$DIM" "$RESET"
}

step()  { printf "\n%s==>%s %s%s%s\n" "$SIGNAL" "$RESET" "$BOLD" "$1" "$RESET"; }
info()  { printf "    %s\n" "$1"; }
ok()    { printf "    %s✔%s %s\n" "$GREEN" "$RESET" "$1"; }
skip()  { printf "    %s•%s %s %s(already done)%s\n" "$DIM" "$RESET" "$1" "$DIM" "$RESET"; }
found() { printf "    %s◆%s %s\n" "$SIGNAL" "$RESET" "$1"; }
warn()  { printf "    %s!%s %s\n" "$AMBER" "$RESET" "$1"; }
die()   { printf "    %s✖%s %s\n" "$RED" "$RESET" "$1"; exit 1; }

ask() { # ask <var> <prompt> <default>
  local __var=$1 __prompt=$2 __default=${3:-} __reply=""
  if [[ $UNATTENDED -eq 1 ]]; then
    printf -v "$__var" '%s' "$__default"
    return
  fi
  if [[ -n $__default ]]; then
    read -r -p "$(printf '%s?%s %s [%s%s%s]: ' "$SIGNAL" "$RESET" "$__prompt" "$DIM" "$__default" "$RESET")" __reply || true
    printf -v "$__var" '%s' "${__reply:-$__default}"
  else
    read -r -p "$(printf '%s?%s %s: ' "$SIGNAL" "$RESET" "$__prompt")" __reply || true
    printf -v "$__var" '%s' "$__reply"
  fi
}

ask_yn() { # ask_yn <prompt> <default y|n> -> exit status 0 for yes
  local prompt=$1 default=$2 reply=""
  if [[ $UNATTENDED -eq 1 ]]; then
    [[ ${default,,} == y* ]]
    return
  fi
  local hint="[y/N]"; [[ ${default,,} == y* ]] && hint="[Y/n]"
  read -r -p "$(printf '%s?%s %s %s%s%s ' "$SIGNAL" "$RESET" "$prompt" "$DIM" "$hint" "$RESET")" reply || true
  reply="${reply:-$default}"
  [[ ${reply,,} == y* ]]
}

gen_secret() { head -c 24 /dev/urandom | base64 | tr -d '/=+\n' | cut -c1-24; }

# ----------------------------- small utilities ------------------------------
ensure_dirs() { # ensure_dirs <dir>...  (the old version silently ignored $2+)
  local d
  for d in "$@"; do
    [[ -d $d ]] || mkdir -p "$d"
  done
}

ensure_user() { # ensure_user <name>
  if id -u "$1" &>/dev/null; then skip "user '$1' exists"; return; fi
  useradd --system --create-home --shell /bin/bash "$1"
  ok "created system user '$1'"
}

# json_str quotes an arbitrary shell value as a JSON string.
json_str() {
  local s=${1-}
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\t'/\\t}
  s=${s//$'\r'/\\r}
  s=${s//$'\n'/\\n}
  printf '"%s"' "$s"
}

# write_secret writes content to a 0600 file only when it differs.
write_secret() { # write_secret <path> <content>
  if [[ -f $1 ]] && printf '%s' "$2" | cmp -s - "$1"; then
    skip "$(basename "$1") unchanged"
    return 1
  fi
  local tmp; tmp=$(mktemp "$(dirname "$1")/.$(basename "$1").XXXXXX")
  printf '%s' "$2" > "$tmp"
  chmod 600 "$tmp"
  mv -f "$tmp" "$1"
  ok "wrote $1"
  return 0
}

# json_field reads a top-level "key": "value" string out of a JSON file
# without needing jq (the files are written by this script).
json_field() { # json_field <file> <key>
  [[ -f $1 ]] || return 1
  sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$1" | head -1
}

port_of() { printf '%s' "${1##*:}"; }

# ----------------------------- discovery ------------------------------------
OS_NAME=""; PKG=""
DETECTED_CS2_DIR=""; DETECTED_UNIT=""; DETECTED_PORT=""; DETECTED_RCON=""; DETECTED_GSLT=""
STEAMCMD_BIN=""; HAVE_CADDY=0; HAVE_UFW=0; UFW_ACTIVE=0; HAVE_MYSQL=0
REUSED_SECRETS=0
PUBLIC_IP=""

detect_os() {
  if [[ -r /etc/os-release ]]; then
    # shellcheck disable=SC1091
    OS_NAME=$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-$ID}")
  fi
  OS_NAME="${OS_NAME:-$(uname -sr)}"
  if command -v apt-get &>/dev/null; then PKG=apt
  elif command -v dnf &>/dev/null; then PKG=dnf
  elif command -v yum &>/dev/null; then PKG=yum
  elif command -v pacman &>/dev/null; then PKG=pacman
  fi
}

# pkg_install installs packages with whatever package manager exists. It never
# aborts the run: callers decide whether a missing package is fatal.
pkg_install() {
  [[ $# -gt 0 ]] || return 0
  case "$PKG" in
    apt) DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" >/dev/null 2>&1 ;;
    dnf) dnf install -y -q "$@" >/dev/null 2>&1 ;;
    yum) yum install -y -q "$@" >/dev/null 2>&1 ;;
    pacman) pacman -Sy --noconfirm --quiet "$@" >/dev/null 2>&1 ;;
    *) return 1 ;;
  esac
}

APT_UPDATED=0
apt_refresh() {
  [[ $PKG == apt ]] || return 0
  [[ $APT_UPDATED -eq 0 ]] || return 0
  DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null 2>&1 || true
  APT_UPDATED=1
}

# is_cs2_dir reports whether dir is a CS2 install root (contains game/csgo).
is_cs2_dir() { [[ -f "$1/game/csgo/gameinfo.gi" ]]; }

# detect_cs2_dir looks for an install in the usual places.
detect_cs2_dir() {
  local c
  for c in \
    "$CS2A_ROOT/cs2" \
    /home/"$CS2A_STEAM_USER"/cs2 \
    /home/"$CS2A_STEAM_USER"/cs2server \
    /home/"$CS2A_STEAM_USER"/serverfiles \
    /home/"$CS2A_STEAM_USER"/cs2server/serverfiles \
    /opt/cs2 /opt/cs2server /srv/cs2 /root/cs2
  do
    if is_cs2_dir "$c"; then printf '%s' "$c"; return 0; fi
  done
  return 1
}

# detect_unit finds the systemd unit that launches a CS2 server and reads the
# install dir, port and GSLT out of its ExecStart / WorkingDirectory.
# CS2A_UNIT_DIRS is overridable so the test suite can point it at a fixture.
CS2A_UNIT_DIRS="${CS2A_UNIT_DIRS:-/etc/systemd/system /lib/systemd/system}"
detect_unit() {
  local d f exec_line wd dir
  for d in $CS2A_UNIT_DIRS; do
    for f in "$d"/*.service; do
      [[ -f $f ]] || continue
      exec_line=$(sed -n 's/^ExecStart=//p' "$f" | head -1)
      [[ -n $exec_line ]] || continue
      # a CS2 launcher runs cs2.sh or the cs2 binary
      [[ $exec_line == *cs2.sh* || $exec_line == */cs2\ * || $exec_line == */cs2 ]] || continue

      DETECTED_UNIT=$(basename "$f" .service)
      wd=$(sed -n 's/^WorkingDirectory=//p' "$f" | head -1)

      # install root = the dir holding game/, derived from either value
      dir="${wd%/}"
      [[ -n $dir ]] || dir=$(printf '%s' "$exec_line" | awk '{print $1}')
      dir="${dir%/cs2.sh}"
      dir="${dir%/cs2}"
      dir="${dir%/bin/linuxsteamrt64}"
      dir="${dir%/game}"
      if is_cs2_dir "$dir"; then DETECTED_CS2_DIR="$dir"; fi

      # port: accepts "-port N", "+port N" and "-port=N"
      DETECTED_PORT=$(printf '%s' "$exec_line" | grep -oE '[-+]port[= ]+[0-9]+' | head -1 | grep -oE '[0-9]+' || true)
      DETECTED_GSLT=$(printf '%s' "$exec_line" | grep -oE '[-+]sv_setsteamaccount[= ]+[^ ]+' | head -1 | grep -oE '[^= ]+$' || true)
      return 0
    done
  done
  return 0
}

# detect_server_cfg reads the values cs2a would otherwise have to ask for.
detect_server_cfg() {
  local cfg="$DETECTED_CS2_DIR/game/csgo/cfg/server.cfg"
  [[ -f $cfg ]] || return 0
  local v
  v=$(sed -n 's/^[[:space:]]*rcon_password[[:space:]]\+"\?\([^"]*\)"\?[[:space:]]*$/\1/p' "$cfg" | tail -1)
  [[ -n $v ]] && DETECTED_RCON="$v"
  v=$(sed -n 's/^[[:space:]]*sv_setsteamaccount[[:space:]]\+"\?\([^"]*\)"\?[[:space:]]*$/\1/p' "$cfg" | tail -1)
  [[ -n $v && -z $DETECTED_GSLT ]] && DETECTED_GSLT="$v"
  v=$(sed -n 's/^[[:space:]]*hostport[[:space:]]\+"\?\([0-9]*\)"\?[[:space:]]*$/\1/p' "$cfg" | tail -1)
  [[ -n $v && -z $DETECTED_PORT ]] && DETECTED_PORT="$v"
  return 0
}

detect_steamcmd() {
  local c
  for c in "$(command -v steamcmd 2>/dev/null || true)" \
           /usr/games/steamcmd /usr/bin/steamcmd \
           /home/"$CS2A_STEAM_USER"/steamcmd/steamcmd.sh \
           /home/"$CS2A_STEAM_USER"/.local/share/Steam/steamcmd/steamcmd.sh \
           /opt/steamcmd/steamcmd.sh
  do
    [[ -n $c && -x $c ]] && { STEAMCMD_BIN="$c"; return 0; }
  done
  return 1
}

# reuse_secrets keeps a rerun from rotating credentials the panel already uses.
reuse_secrets() {
  local v
  v=$(json_field "$CS2A_ROOT/etc/agent.json" token || true)
  [[ -n ${v:-} ]] && { AGENT_TOKEN="$v"; REUSED_SECRETS=1; }
  v=$(json_field "$CS2A_ROOT/etc/agent.json" rcon_password || true)
  [[ -n ${v:-} && -z $RCON_PASS ]] && RCON_PASS="$v"
  v=$(json_field "$CS2A_ROOT/etc/agent.json" wp_dsn || true)
  [[ -n ${v:-} ]] && WP_DSN="$v"
  if [[ -f $CS2A_ROOT/etc/panel.env ]]; then
    v=$(sed -n 's/^CS2A_ADMIN_USER=//p' "$CS2A_ROOT/etc/panel.env" | head -1)
    [[ -n ${v:-} ]] && SETUP_ADMIN="$v"
    v=$(sed -n 's/^CS2A_ADMIN_PASSWORD=//p' "$CS2A_ROOT/etc/panel.env" | head -1)
    [[ -n ${v:-} ]] && { SETUP_PASS="$v"; REUSED_SECRETS=1; }
  fi
  v=$(json_field "$CS2A_ROOT/etc/panel.json" public_url || true)
  if [[ -n ${v:-} && -z $PANEL_DOMAIN && $v == https://* ]]; then
    PANEL_DOMAIN="${v#https://}"
  fi
}

AGENT_TOKEN=""; SETUP_PASS=""; SETUP_TOKEN=""; WP_DSN=""

# In library mode the caller only wants the functions above.
if [[ $LIB_MODE == 1 ]]; then
  return 0 2>/dev/null || exit 0
fi

# ----------------------------- preflight ------------------------------------
banner
step "Looking around"

[[ $(uname -s) == Linux ]] || die "cs2a installs on Linux only (this is $(uname -s))"
command -v systemctl &>/dev/null || die "systemd is required (systemctl not found)"
detect_os
ok "$OS_NAME${PKG:+ (package manager: $PKG)}"

MISSING=()
for tool in curl tar gzip; do command -v "$tool" &>/dev/null || MISSING+=("$tool"); done
if [[ ${#MISSING[@]} -gt 0 ]]; then
  info "installing base tools: ${MISSING[*]}"
  apt_refresh
  pkg_install "${MISSING[@]}" || die "could not install: ${MISSING[*]} — install them and rerun"
fi
ok "base tools present (curl, tar, gzip)"

PUBLIC_IP=$(curl -fs --max-time 4 https://api.ipify.org 2>/dev/null || true)
[[ -n $PUBLIC_IP ]] || PUBLIC_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
[[ -n $PUBLIC_IP ]] && found "public address: $PUBLIC_IP"

detect_unit
[[ -n $DETECTED_CS2_DIR ]] || DETECTED_CS2_DIR=$(detect_cs2_dir || true)
detect_server_cfg

if [[ -n $DETECTED_CS2_DIR ]]; then
  found "CS2 server: $DETECTED_CS2_DIR"
  [[ -n $DETECTED_UNIT ]] && found "systemd unit: $DETECTED_UNIT.service"
  [[ -n $DETECTED_PORT ]] && found "game port: $DETECTED_PORT (from the launch line)"
  [[ -n $DETECTED_RCON ]] && found "rcon_password: read from server.cfg (not shown)"
  [[ -n $DETECTED_GSLT ]] && found "GSLT: already configured"
  [[ $WITH_CS2 -eq -1 ]] && WITH_CS2=0
else
  info "no CS2 install found"
fi

if detect_steamcmd; then found "steamcmd: $STEAMCMD_BIN"; fi
command -v caddy &>/dev/null && { HAVE_CADDY=1; found "caddy installed"; }
if command -v ufw &>/dev/null; then
  HAVE_UFW=1
  ufw status 2>/dev/null | head -1 | grep -qi "status: active" && UFW_ACTIVE=1
  found "ufw present ($([[ $UFW_ACTIVE -eq 1 ]] && echo active || echo inactive))"
fi
(command -v mariadb &>/dev/null || command -v mysql &>/dev/null) && { HAVE_MYSQL=1; found "MariaDB/MySQL client present"; }

if [[ -f $CS2A_ROOT/etc/agent.json ]]; then
  reuse_secrets
  [[ $REUSED_SECRETS -eq 1 ]] && found "existing cs2a install at $CS2A_ROOT — reusing its tokens and credentials"
fi
[[ $WITH_CS2 -eq -1 ]] && WITH_CS2=1

# ----------------------------- questions ------------------------------------
# Everything discovered above becomes a default, so the happy path is a few
# Enter presses. Whatever could not be discovered is asked exactly once.
step "Configuration"

CS2_DIR="${DETECTED_CS2_DIR:-$CS2A_ROOT/cs2}"

if [[ -n $DETECTED_CS2_DIR ]]; then
  if [[ $UNATTENDED -eq 0 ]] && ! ask_yn "Manage the CS2 server at $DETECTED_CS2_DIR?" y; then
    ask CS2_DIR "Path of the CS2 install to manage (contains game/)" "$DETECTED_CS2_DIR"
    is_cs2_dir "$CS2_DIR" || die "$CS2_DIR does not contain game/csgo/gameinfo.gi"
    WITH_CS2=0
  fi
elif [[ $WITH_CS2 -eq 1 ]]; then
  if [[ $UNATTENDED -eq 0 ]] && ! ask_yn "Download and install the CS2 dedicated server now? (~40 GB)" y; then
    WITH_CS2=0
    ask CS2_DIR "Path of your existing CS2 install (contains game/)" "$CS2A_ROOT/cs2"
    is_cs2_dir "$CS2_DIR" || die "$CS2_DIR does not contain game/csgo/gameinfo.gi — install CS2 first or rerun and let cs2a do it"
  fi
fi

# unit name: reuse the discovered one, otherwise cs2a manages its own
if [[ -n $DETECTED_UNIT ]]; then
  CS2A_SERVICE_GAME="$DETECTED_UNIT"
  MANAGE_GAME_UNIT=0
else
  MANAGE_GAME_UNIT=1
fi
[[ $WITH_CS2 -eq 1 && -z $DETECTED_UNIT ]] && MANAGE_GAME_UNIT=1

# game port: the launch line wins, since RCON must match it exactly
if [[ -n $DETECTED_PORT ]]; then
  CS2A_GAME_PORT="$DETECTED_PORT"
else
  ask CS2A_GAME_PORT "Game port (UDP game + A2S, TCP RCON)" "$CS2A_GAME_PORT"
fi

# rcon password: from server.cfg, else generated (never asked)
if [[ -z $RCON_PASS ]]; then
  if [[ -n $DETECTED_RCON ]]; then
    RCON_PASS="$DETECTED_RCON"
  else
    RCON_PASS=$(gen_secret)
    info "generated an RCON password (written to server.cfg and agent.json)"
  fi
fi

# GSLT: only asked when the machine has none and a fresh server is installed
if [[ -z $GSLT && -n $DETECTED_GSLT ]]; then
  GSLT="$DETECTED_GSLT"
elif [[ -z $GSLT && $WITH_CS2 -eq 1 ]]; then
  ask GSLT "GSLT token (steamcommunity.com/dev/managegameservers — Enter to skip, LAN only)" ""
fi

if [[ -z $PANEL_DOMAIN && $UNATTENDED -eq 0 ]]; then
  ask PANEL_DOMAIN "Domain for the panel (Enter to skip — HTTPS via Caddy when set)" ""
fi

# admin credentials + tokens: generated unless a previous install had them
[[ -n $AGENT_TOKEN ]] || AGENT_TOKEN=$(gen_secret)
[[ -n $SETUP_PASS ]] || SETUP_PASS=$(gen_secret)
SETUP_TOKEN=$(gen_secret)

if [[ $SETUP_SKIN_DB -eq -1 ]]; then
  if [[ -n $WP_DSN ]]; then
    SETUP_SKIN_DB=0   # already configured
  elif [[ $UNATTENDED -eq 1 ]]; then
    SETUP_SKIN_DB=0
  elif ask_yn "Set up MariaDB for WeaponPaints skin sync?" n; then
    SETUP_SKIN_DB=1
  else
    SETUP_SKIN_DB=0
  fi
fi

PANEL_URL="http://${PUBLIC_IP:-127.0.0.1}:$CS2A_PANEL_PORT"
[[ -n $PANEL_DOMAIN ]] && PANEL_URL="https://$PANEL_DOMAIN"

printf "\n%s%sPLAN%s\n" "$BOLD" "$SIGNAL" "$RESET"
cat <<PLAN
    install root : $CS2A_ROOT
    cs2 dir      : $CS2_DIR $([[ $WITH_CS2 -eq 1 ]] && echo "(steamcmd will install it)" || echo "(existing)")
    game server  : $CS2A_SERVICE_GAME.service on port $CS2A_GAME_PORT $([[ $MANAGE_GAME_UNIT -eq 1 ]] && echo "(cs2a writes this unit)" || echo "(yours, left untouched)")
    agent        : 127.0.0.1:$CS2A_AGENT_PORT (loopback only, bearer token)
    panel        : $PANEL_URL
    skin sync    : $([[ $SETUP_SKIN_DB -eq 1 ]] && echo "MariaDB will be provisioned" || { [[ -n $WP_DSN ]] && echo "already configured" || echo "off"; })
    firewall     : $([[ $TOUCH_FIREWALL -eq 1 && $HAVE_UFW -eq 1 ]] && echo "ufw rules for the ports above" || echo "not touched")
PLAN
if [[ $UNATTENDED -eq 0 ]]; then
  ask_yn "Proceed?" y || die "aborted — nothing was changed"
fi

# ----------------------------- directories ----------------------------------
step "Directories and service user"
ensure_dirs "$CS2A_ROOT" "$CS2A_ROOT/bin" "$CS2A_ROOT/etc" "$CS2A_ROOT/var" "$CS2A_ROOT/cache/plugins"
chmod 700 "$CS2A_ROOT/etc"
ok "layout ready under $CS2A_ROOT"
ensure_user "$CS2A_STEAM_USER"

# ----------------------------- binaries -------------------------------------
step "cs2a binaries"
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [[ -f $SCRIPT_DIR/../dist/cs2a-agent && -f $SCRIPT_DIR/../dist/cs2a-panel ]]; then
  install -m 0755 "$SCRIPT_DIR/../dist/cs2a-agent" "$SCRIPT_DIR/../dist/cs2a-panel" "$CS2A_ROOT/bin/"
  ok "installed from the local dist/ build"
else
  CS2A_VERSION="${CS2A_VERSION:-latest}"
  BASE="https://github.com/elvonpiko/cs2a/releases"
  if [[ $CS2A_VERSION == latest ]]; then
    URL="$BASE/latest/download/cs2a-linux-amd64.tar.gz"
  else
    URL="$BASE/download/$CS2A_VERSION/cs2a-linux-amd64.tar.gz"
  fi
  info "downloading $URL"
  TMPD=$(mktemp -d)
  trap 'rm -rf "$TMPD"' EXIT
  curl -fL --retry 3 --retry-delay 2 -o "$TMPD/cs2a.tar.gz" "$URL" || die "no release archive at $URL
    A release exists only after a v* tag is pushed. Either pin CS2A_VERSION to an
    existing tag, or build locally (make build) and rerun this script from the
    repository so dist/cs2a-agent and dist/cs2a-panel are picked up."
  tar -xzf "$TMPD/cs2a.tar.gz" -C "$TMPD"
  AGENT_BIN=$(find "$TMPD" -type f -name cs2a-agent | head -1)
  PANEL_BIN=$(find "$TMPD" -type f -name cs2a-panel | head -1)
  [[ -n $AGENT_BIN && -n $PANEL_BIN ]] || die "release archive did not contain cs2a-agent and cs2a-panel"
  install -m 0755 "$AGENT_BIN" "$PANEL_BIN" "$CS2A_ROOT/bin/"
  rm -rf "$TMPD"
  trap - EXIT
  ok "installed release binaries ($CS2A_VERSION)"
fi
"$CS2A_ROOT/bin/cs2a-agent" -h >/dev/null 2>&1 || true
ok "cs2a-agent + cs2a-panel in $CS2A_ROOT/bin"

# ----------------------------- SteamCMD + CS2 -------------------------------
if [[ $WITH_CS2 -eq 1 ]]; then
  step "SteamCMD"
  if [[ -n $STEAMCMD_BIN ]]; then
    skip "steamcmd present at $STEAMCMD_BIN"
  else
    case "$PKG" in
      apt)
        info "enabling i386 packages and installing steamcmd"
        dpkg --add-architecture i386 >/dev/null 2>&1 || true
        # steamcmd lives in multiverse (Ubuntu) / non-free (Debian)
        command -v add-apt-repository &>/dev/null && add-apt-repository -y multiverse >/dev/null 2>&1 || true
        APT_UPDATED=0; apt_refresh
        echo steamcmd steam/question select "I AGREE" | debconf-set-selections 2>/dev/null || true
        echo steamcmd steam/license note '' | debconf-set-selections 2>/dev/null || true
        pkg_install steamcmd lib32gcc-s1 lib32stdc++6 || true
        ;;
      *) warn "no apt — steamcmd must be installed manually" ;;
    esac
    detect_steamcmd || die "steamcmd not available. Install it, then rerun (or rerun with --no-cs2 and point cs2a at an existing server)."
    ok "steamcmd installed"
  fi

  # srcds looks for steamclient.so under ~/.steam/sdk64
  SDK_DIR=$(getent passwd "$CS2A_STEAM_USER" | cut -d: -f6)/.steam/sdk64
  STEAM_LIB=$(dirname "$STEAMCMD_BIN")/linux64/steamclient.so
  [[ -f $STEAM_LIB ]] || STEAM_LIB=/usr/lib/games/steam/steamclient.so
  if [[ -f $STEAM_LIB ]]; then
    sudo -u "$CS2A_STEAM_USER" mkdir -p "$SDK_DIR"
    sudo -u "$CS2A_STEAM_USER" ln -sf "$STEAM_LIB" "$SDK_DIR/steamclient.so"
    ok "steamclient.so linked into ~$CS2A_STEAM_USER/.steam/sdk64"
  else
    warn "steamclient.so not found — the server may refuse to start; check your steamcmd install"
  fi

  step "CS2 dedicated server (app $CS2A_APP_ID)"
  ensure_dirs "$CS2_DIR"
  chown "$CS2A_STEAM_USER:$CS2A_STEAM_USER" "$CS2_DIR"
  if is_cs2_dir "$CS2_DIR"; then
    info "already installed — running app_update to refresh"
  else
    info "downloading ~40 GB, this takes a while"
  fi
  sudo -u "$CS2A_STEAM_USER" "$STEAMCMD_BIN" +force_install_dir "$CS2_DIR" \
    +login anonymous +app_update "$CS2A_APP_ID" validate +quit 2>&1 |
    while IFS= read -r line; do
      case "$line" in
        *"Update state"*|*"Success"*|*"ERROR"*|*"error"*) info "$line" ;;
      esac
    done
  is_cs2_dir "$CS2_DIR" || die "CS2 install failed — $CS2_DIR/game/csgo/gameinfo.gi is missing"
  ok "CS2 installed at $CS2_DIR"
else
  step "CS2 dedicated server"
  is_cs2_dir "$CS2_DIR" || die "$CS2_DIR does not look like a CS2 install (missing game/csgo/gameinfo.gi)"
  skip "using the existing install at $CS2_DIR"
fi

# ----------------------------- server.cfg -----------------------------------
# cs2a only guarantees rcon_password: the panel needs it, and everything else
# in server.cfg belongs to the operator.
step "server.cfg"
CFG_DIR="$CS2_DIR/game/csgo/cfg"
ensure_dirs "$CFG_DIR"
if [[ ! -f $CFG_DIR/server.cfg ]]; then
  cat > "$CFG_DIR/server.cfg" <<CFG
hostname "cs2a server"
rcon_password "$RCON_PASS"
sv_lan 0
$([[ -n $GSLT ]] && printf 'sv_setsteamaccount "%s"\n' "$GSLT")
CFG
  ok "server.cfg created"
elif grep -qE '^[[:space:]]*rcon_password[[:space:]]' "$CFG_DIR/server.cfg"; then
  skip "server.cfg already sets rcon_password"
else
  printf '\nrcon_password "%s"\n' "$RCON_PASS" >> "$CFG_DIR/server.cfg"
  ok "added rcon_password to your server.cfg (everything else untouched)"
fi
chown -R "$CS2A_STEAM_USER:$CS2A_STEAM_USER" "$CFG_DIR" 2>/dev/null || true

# ----------------------------- MariaDB (optional) ---------------------------
if [[ $SETUP_SKIN_DB -eq 1 ]]; then
  step "MariaDB for WeaponPaints skin sync"
  if [[ $HAVE_MYSQL -eq 0 ]]; then
    apt_refresh
    pkg_install mariadb-server && { HAVE_MYSQL=1; ok "mariadb-server installed"; } \
      || warn "could not install MariaDB automatically — skipping skin sync setup"
  else
    skip "MariaDB already installed"
  fi
  if [[ $HAVE_MYSQL -eq 1 ]]; then
    MYSQL_BIN=$(command -v mariadb || command -v mysql)
    systemctl enable --now mariadb >/dev/null 2>&1 || systemctl enable --now mysql >/dev/null 2>&1 || true
    WP_DB_PASS=$(gen_secret)
    if "$MYSQL_BIN" <<SQL >/dev/null 2>&1
CREATE DATABASE IF NOT EXISTS cs2_wp CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'cs2a'@'localhost' IDENTIFIED BY '$WP_DB_PASS';
ALTER USER 'cs2a'@'localhost' IDENTIFIED BY '$WP_DB_PASS';
GRANT ALL PRIVILEGES ON cs2_wp.* TO 'cs2a'@'localhost';
FLUSH PRIVILEGES;
SQL
    then
      WP_DSN="cs2a:$WP_DB_PASS@tcp(127.0.0.1:3306)/cs2_wp"
      ok "database cs2_wp ready (user cs2a)"
      info "after installing WeaponPaints, paste these into its config from the panel:"
      info "  host 127.0.0.1  port 3306  database cs2_wp  user cs2a  password $WP_DB_PASS"
    else
      warn "could not provision the database (is the root socket login available?) — skin sync stays off"
    fi
  fi
fi

# ----------------------------- config files ---------------------------------
step "Config files"
AGENT_JSON="{
  \"listen\": \"127.0.0.1:$CS2A_AGENT_PORT\",
  \"token\": $(json_str "$AGENT_TOKEN"),
  \"cs2_dir\": $(json_str "$CS2_DIR"),
  \"service_name\": $(json_str "$CS2A_SERVICE_GAME"),
  \"rcon_addr\": \"127.0.0.1:$CS2A_GAME_PORT\",
  \"rcon_password\": $(json_str "$RCON_PASS"),
  \"a2s_addr\": \"127.0.0.1:$CS2A_GAME_PORT\",
  \"db_path\": $(json_str "$CS2A_ROOT/var/agent.db"),
  \"plugin_cache\": $(json_str "$CS2A_ROOT/cache/plugins")"
[[ -n $WP_DSN ]] && AGENT_JSON+=",
  \"wp_dsn\": $(json_str "$WP_DSN")"
AGENT_JSON+="
}
"

PANEL_JSON="{
  \"listen\": \"0.0.0.0:$CS2A_PANEL_PORT\",
  \"agent_url\": \"http://127.0.0.1:$CS2A_AGENT_PORT\",
  \"agent_token\": $(json_str "$AGENT_TOKEN"),
  \"db_path\": $(json_str "$CS2A_ROOT/var/panel.db"),
  \"setup_token_file\": $(json_str "$CS2A_ROOT/etc/panel-setup-token")"
[[ -n $PANEL_DOMAIN ]] && PANEL_JSON+=",
  \"public_url\": $(json_str "https://$PANEL_DOMAIN")"
PANEL_JSON+="
}
"

CONFIG_CHANGED=0
write_secret "$CS2A_ROOT/etc/agent.json" "$AGENT_JSON" && CONFIG_CHANGED=1
write_secret "$CS2A_ROOT/etc/panel.json" "$PANEL_JSON" && CONFIG_CHANGED=1
write_secret "$CS2A_ROOT/etc/panel.env" \
  "$(printf 'CS2A_ADMIN_USER=%s\nCS2A_ADMIN_PASSWORD=%s\n' "$SETUP_ADMIN" "$SETUP_PASS")" && CONFIG_CHANGED=1

# the agent must be able to parse what we just wrote
if ! python3 -c "import json,sys;json.load(open(sys.argv[1]))" "$CS2A_ROOT/etc/agent.json" 2>/dev/null; then
  command -v python3 >/dev/null 2>&1 && die "agent.json is not valid JSON — refusing to continue"
fi

# the setup token is consumed by the panel on first admin creation
if [[ ! -f $CS2A_ROOT/var/panel.db ]]; then
  printf '%s' "$SETUP_TOKEN" > "$CS2A_ROOT/etc/panel-setup-token"
  chmod 600 "$CS2A_ROOT/etc/panel-setup-token"
  ok "first-login setup token written"
else
  skip "panel database exists — setup token not reissued"
fi

# ----------------------------- Caddy (optional) -----------------------------
if [[ -n $PANEL_DOMAIN ]]; then
  step "Caddy reverse proxy for $PANEL_DOMAIN"
  if [[ $HAVE_CADDY -eq 0 ]]; then
    if [[ $PKG == apt ]]; then
      info "installing caddy from the official repository"
      pkg_install debian-keyring debian-archive-keyring apt-transport-https || true
      curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
        | gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg 2>/dev/null || true
      curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
        > /etc/apt/sources.list.d/caddy-stable.list 2>/dev/null || true
      APT_UPDATED=0; apt_refresh
      pkg_install caddy && HAVE_CADDY=1
    fi
    [[ $HAVE_CADDY -eq 1 ]] && ok "caddy installed" \
      || warn "caddy could not be installed automatically — the Caddyfile is still written"
  else
    skip "caddy already installed"
  fi
  ensure_dirs /etc/caddy
  # Long-running admin actions (plugin installs, map changes) must not be cut
  # off by the proxy: the agent streams progress and the panel polls, so the
  # proxy needs generous header and stream timeouts.
  CADDY_SITE="$PANEL_DOMAIN {
	encode zstd gzip
	reverse_proxy 127.0.0.1:$CS2A_PANEL_PORT {
		# plugin downloads and map changes can take minutes
		transport http {
			dial_timeout 10s
			response_header_timeout 10m
		}
		flush_interval -1
	}
}
"
  if [[ -f /etc/caddy/Caddyfile ]] && grep -qE "^[[:space:]]*$PANEL_DOMAIN[[:space:]]*\{" /etc/caddy/Caddyfile; then
    skip "Caddyfile already has a $PANEL_DOMAIN site block"
  else
    if [[ -f /etc/caddy/Caddyfile ]]; then
      cp -a /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.cs2a-backup.$(date +%s)"
      printf '\n%s' "$CADDY_SITE" >> /etc/caddy/Caddyfile
      ok "appended a $PANEL_DOMAIN site block (previous Caddyfile backed up)"
    else
      printf '%s' "$CADDY_SITE" > /etc/caddy/Caddyfile
      ok "Caddyfile written for $PANEL_DOMAIN"
    fi
  fi
  if [[ $HAVE_CADDY -eq 1 ]]; then
    if caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1; then
      systemctl enable caddy >/dev/null 2>&1 || true
      systemctl reload caddy 2>/dev/null || systemctl restart caddy 2>/dev/null || true
      ok "caddy running — a certificate for $PANEL_DOMAIN is issued on first request"
    else
      warn "the Caddyfile did not validate — check /etc/caddy/Caddyfile and run: systemctl reload caddy"
    fi
  fi
fi

# ----------------------------- firewall -------------------------------------
step "Firewall"
if [[ $TOUCH_FIREWALL -eq 0 ]]; then
  skip "left alone (--no-firewall)"
elif [[ $HAVE_UFW -eq 0 ]]; then
  info "ufw not installed — if another firewall is active, open:"
  info "  $CS2A_GAME_PORT/udp (game + A2S), $CS2A_GAME_PORT/tcp (rcon)$([[ -n $PANEL_DOMAIN ]] && echo ", 80/tcp + 443/tcp (panel)" || echo ", $CS2A_PANEL_PORT/tcp (panel)")"
else
  ufw_allow() { # ufw_allow <rule> <label>
    if ufw status 2>/dev/null | grep -qE "^${1%% *}[[:space:]]"; then
      skip "ufw already allows $1"
    elif ufw allow "$1" >/dev/null 2>&1; then
      ok "ufw allow $1 ($2)"
    else
      warn "could not add ufw rule $1"
    fi
  }
  ufw_allow "$CS2A_GAME_PORT/udp" "game + A2S"
  ufw_allow "$CS2A_GAME_PORT/tcp" "rcon"
  if [[ -n $PANEL_DOMAIN ]]; then
    ufw_allow "80/tcp" "ACME http-01 challenge"
    ufw_allow "443/tcp" "panel via caddy"
    # the raw panel port need not be public once Caddy fronts it
    ufw delete allow "$CS2A_PANEL_PORT/tcp" >/dev/null 2>&1 && info "closed $CS2A_PANEL_PORT/tcp (caddy fronts the panel)" || true
  else
    ufw_allow "$CS2A_PANEL_PORT/tcp" "panel"
  fi
  [[ $UFW_ACTIVE -eq 0 ]] && info "ufw is inactive — rules apply once you run: ufw enable"
fi

# ----------------------------- systemd --------------------------------------
step "systemd units"
GAME_UNIT_FILE="/etc/systemd/system/$CS2A_SERVICE_GAME.service"
if [[ $MANAGE_GAME_UNIT -eq 1 ]]; then
  # -usercon is what makes RCON reachable at all; without it the panel can
  # read A2S status but cannot change maps or run commands.
  GAME_EXEC="$CS2_DIR/game/cs2.sh -dedicated -console -usercon -ip 0.0.0.0 -port $CS2A_GAME_PORT -maxplayers 12 +map de_dust2 +exec server.cfg"
  [[ -n $GSLT ]] && GAME_EXEC+=" +sv_setsteamaccount $GSLT"
  cat > "$GAME_UNIT_FILE" <<UNIT
[Unit]
Description=CS2 dedicated server (managed by cs2a)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$CS2A_STEAM_USER
Group=$CS2A_STEAM_USER
WorkingDirectory=$CS2_DIR/game
ExecStart=$GAME_EXEC
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
  ok "wrote $CS2A_SERVICE_GAME.service"
elif [[ -f $GAME_UNIT_FILE ]]; then
  skip "keeping your existing $CS2A_SERVICE_GAME.service untouched"
  grep -q -- "-usercon" "$GAME_UNIT_FILE" \
    || warn "your unit's ExecStart has no -usercon: RCON (map changes, commands) will not work until you add it"
else
  warn "no $CS2A_SERVICE_GAME.service on this machine — the panel cannot start/stop the server until a unit exists"
fi

cat > /etc/systemd/system/cs2a-agent.service <<UNIT
[Unit]
Description=cs2a agent (CS2 server control)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=$CS2A_ROOT/bin/cs2a-agent -config $CS2A_ROOT/etc/agent.json
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT

cat > /etc/systemd/system/cs2a-panel.service <<UNIT
[Unit]
Description=cs2a panel (web UI)
After=network-online.target cs2a-agent.service
Wants=network-online.target

[Service]
Type=simple
User=root
EnvironmentFile=$CS2A_ROOT/etc/panel.env
ExecStart=$CS2A_ROOT/bin/cs2a-panel -config $CS2A_ROOT/etc/panel.json
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
UNIT
ok "wrote cs2a-agent.service + cs2a-panel.service"

systemctl daemon-reload
systemctl enable cs2a-agent.service cs2a-panel.service >/dev/null 2>&1 || true
systemctl restart cs2a-agent.service
systemctl restart cs2a-panel.service
ok "agent and panel started"
if [[ -f $GAME_UNIT_FILE ]]; then
  systemctl enable "$CS2A_SERVICE_GAME.service" >/dev/null 2>&1 || true
  ok "$CS2A_SERVICE_GAME.service enabled (start it from the panel)"
fi

# ----------------------------- verify ---------------------------------------
step "Verifying"
AGENT_OK=0; PANEL_OK=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  curl -fs --max-time 2 -H "Authorization: Bearer $AGENT_TOKEN" \
    "http://127.0.0.1:$CS2A_AGENT_PORT/api/v1/health" >/dev/null 2>&1 && { AGENT_OK=1; break; }
  sleep 1
done
for _ in 1 2 3 4 5 6 7 8 9 10; do
  code=$(curl -fs -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$CS2A_PANEL_PORT/login" 2>/dev/null || echo 000)
  [[ $code == 200 ]] && { PANEL_OK=1; break; }
  sleep 1
done
[[ $AGENT_OK -eq 1 ]] && ok "agent healthy on 127.0.0.1:$CS2A_AGENT_PORT" \
  || warn "agent not answering — check: journalctl -u cs2a-agent -n 50"
[[ $PANEL_OK -eq 1 ]] && ok "panel serving on port $CS2A_PANEL_PORT" \
  || warn "panel not answering — check: journalctl -u cs2a-panel -n 50"

# ----------------------------- summary --------------------------------------
printf "\n%s%s  cs2a is installed  %s\n\n" "$BOLD" "$SIGNAL" "$RESET"
cat <<SUMMARY
    Panel      $PANEL_URL
    Sign in    $SETUP_ADMIN / $SETUP_PASS
    Setup code $([[ -f $CS2A_ROOT/etc/panel-setup-token ]] && echo "$SETUP_TOKEN" || echo "(already used)")

    Server     $CS2A_SERVICE_GAME.service on port $CS2A_GAME_PORT
    Config     $CS2A_ROOT/etc/{agent,panel}.json  ${DIM}(0600, contains secrets)${RESET}
    Logs       journalctl -u cs2a-panel -u cs2a-agent -f

    Next:
      1. Open $PANEL_URL and sign in.
      2. Plugins → install Metamod:Source, then CounterStrikeSharp.
      3. Server → Start.
SUMMARY
[[ -n $PANEL_DOMAIN ]] && printf "\n    %sPoint %s at %s before the first HTTPS request, or Caddy cannot get a certificate.%s\n" \
  "$DIM" "$PANEL_DOMAIN" "${PUBLIC_IP:-this server}" "$RESET"
printf "\n"
