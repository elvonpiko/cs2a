#!/usr/bin/env bash
# ============================================================================
#  cs2a bootstrap installer
#  Interactive, idempotent installer for the cs2a agent + panel and
#  (optionally) SteamCMD + a CS2 dedicated server, on a fresh Linux VPS.
#
#  Usage:
#    sudo bash bootstrap.sh
#    sudo bash bootstrap.sh --unattended           # non-interactive defaults
#    sudo bash bootstrap.sh --no-cs2               # panel+agent only, point at
#                                                  #   an existing CS2 install
#  Options: --unattended, --no-cs2
#  The installer asks for an optional domain; when given, Caddy serves the
#  panel with automatic HTTPS (Let's Encrypt). Without a domain the panel
#  stays on its plain HTTP port as today.
# ============================================================================
set -euo pipefail

# ----------------------------- tunables ------------------------------------
CS2A_ROOT="${CS2A_ROOT:-/opt/cs2a}"
CS2A_SERVICE_GAME="${CS2A_SERVICE_GAME:-cs2-server}"
CS2A_AGENT_PORT="${CS2A_AGENT_PORT:-8100}"
CS2A_PANEL_PORT="${CS2A_PANEL_PORT:-8080}"
CS2A_GAME_PORT="${CS2A_GAME_PORT:-27015}"
CS2A_STEAM_USER="${CS2A_STEAM_USER:-steam}"
CS2A_APP_ID="${CS2A_APP_ID:-730}"
UNATTENDED=0
WITH_CS2=1

for arg in "$@"; do
  case "$arg" in
    --unattended) UNATTENDED=1 ;;
    --no-cs2)     WITH_CS2=0 ;;
    -h|--help)    sed -n '2,12p' "$0"; exit 0 ;;
  esac
done

[[ $EUID -eq 0 ]] || { echo "cs2a: run me as root (sudo)"; exit 1; }

# ----------------------------- tui helpers ---------------------------------
if [[ -t 1 ]]; then
  BOLD=$'\e[1m'; DIM=$'\e[2m'; RESET=$'\e[0m'
  ORANGE=$'\e[38;5;209m'; GREEN=$'\e[38;5;114m'; RED=$'\e[38;5;174m'
else
  BOLD=""; DIM=""; RESET=""; ORANGE=""; GREEN=""; RED=""
fi

banner() {
  cat <<'ART'
      ______ ____    ______  ___
     / ____// __ \  / __ \ \/  /
    / /    / / / / / / / /\  /
   / /____/ /_/ / / /_/ / / /
   \____/\____/  \____/ /_/
ART
  printf "%s  minimal CS2 server manager — agent + panel installer%s\n\n" "$DIM" "$RESET"
}

step()    { printf "\n%s==>%s %s%s%s\n" "$ORANGE" "$RESET" "$BOLD" "$1" "$RESET"; }
info()    { printf "    %s\n" "$1"; }
ok()      { printf "    %s✔%s %s\n" "$GREEN" "$RESET" "$1"; }
warn()    { printf "    %s!%s %s\n" "$RED" "$RESET" "$1"; }
die()     { warn "$1"; exit 1; }

ask() { # ask <var> <prompt> <default>
  local __var=$1 __prompt=$2 __default=${3:-}
  if [[ $UNATTENDED -eq 1 ]]; then
    printf -v "$__var" '%s' "$__default"
    return
  fi
  if [[ -n $__default ]]; then
    read -r -p "$(printf '%s?%s %s [%s%s%s]: ' "$ORANGE" "$RESET" "$__prompt" "$DIM" "$__default" "$RESET")" __reply
    printf -v "$__var" '%s' "${__reply:-$__default}"
  else
    read -r -p "$(printf '%s?%s %s: ' "$ORANGE" "$RESET" "$__prompt")" __reply
    printf -v "$__var" '%s' "$__reply"
  fi
}

ask_secret() { # ask_secret <var> <prompt>
  local __var=$1 __prompt=$2
  if [[ $UNATTENDED -eq 1 ]]; then
    printf -v "$__var" '%s' "$(head -c 18 /dev/urandom | base64 | tr -d '/=+\n' | cut -c1-20)"
    return
  fi
  read -r -s -p "$(printf '%s?%s %s (enter = generate): ' "$ORANGE" "$RESET" "$__prompt")" __reply
  printf '\n'
  if [[ -z $__reply ]]; then
    __reply="$(head -c 18 /dev/urandom | base64 | tr -d '/=+\n' | cut -c1-20)"
  fi
  printf -v "$__var" '%s' "$__reply"
}

spinner() { # spinner <pid> <label>
  local pid=$1 label=$2 spin='|/-\' i=0
  while kill -0 "$pid" 2>/dev/null; do
    printf "\r    %s%s%s %s" "$DIM" "${spin:i++%4:1}" "$RESET" "$label"
    sleep 0.15
  done
  printf "\r    %s✔%s %s\n" "$GREEN" "$RESET" "$label"
}

# ----------------------------- idempotency ---------------------------------
ensure_dir() { [[ -d $1 ]] || { mkdir -p "$1"; info "created $1"; }; }

ensure_user() {
  if id -u "$1" &>/dev/null; then ok "user '$1' exists"; return; fi
  useradd --system --create-home --shell /bin/bash "$1"
  ok "created system user '$1'"
}

ensure_line_in_file() { # ensure_line_in_file <file> <line>
  grep -qxF "$2" "$1" 2>/dev/null || echo "$2" >> "$1"
}

# ----------------------------- detection ------------------------------------
# When a CS2 install already exists (systemd unit / SteamCMD dir), prefill the
# answers from it instead of asking blind questions. Everything remains
# overridable interactively (or via env in --unattended).
DETECTED_CS2_DIR=""
DETECTED_UNIT=""
DETECTED_PORT=""
DETECTED_RCON=""

detect_existing() {
  # find candidate cs2 dirs from systemd unit files with "cs2" in ExecStart
  local f dir
  for f in /etc/systemd/system/*.service /lib/systemd/system/*.service; do
    [[ -f $f ]] || continue
    grep -qiE "ExecStart.*(cs2|srcds)" "$f" 2>/dev/null || continue
    grep -qi "usercon" "$f" 2>/dev/null || continue
    DETECTED_UNIT=$(basename "$f" .service)
    dir=$(grep -oE "ExecStart=[^ ]*cs2[^ ]*" "$f" 2>/dev/null | head -1)
    # derive install root: prefer WorkingDirectory=.../game, else force_install_dir
    local wd; wd=$(grep -oE "^WorkingDirectory=.*" "$f" | head -1 | cut -d= -f2)
    if [[ $wd == */game ]]; then DETECTED_CS2_DIR="${wd%/game}"
    else DETECTED_CS2_DIR=$(dirname "$(dirname "$dir")" 2>/dev/null); fi
    # port from the launch line
    DETECTED_PORT=$(grep -oE "\+?port [0-9]+" "$f" | head -1 | awk '{print $2}')
    break
  done
  # rcon_password straight from the server.cfg
  local cfg="$DETECTED_CS2_DIR/game/csgo/cfg/server.cfg"
  [[ -f $cfg ]] || cfg="$EXISTING_CS2_DIR_FALLBACK/game/csgo/cfg/server.cfg"
  if [[ -n $DETECTED_CS2_DIR && -f $DETECTED_CS2_DIR/game/csgo/cfg/server.cfg ]]; then
    DETECTED_RCON=$(grep -oE '^rcon_password\s+"?[^"]*"?' "$DETECTED_CS2_DIR/game/csgo/cfg/server.cfg" | head -1 | sed -E 's/^rcon_password\s+"?//; s/"$//')
  fi
  DETECTED_PORT=${DETECTED_PORT:-27015}
}
EXISTING_CS2_DIR_FALLBACK=""
detect_existing
if [[ -n $DETECTED_CS2_DIR && -f $DETECTED_CS2_DIR/game/csgo/gameinfo.gi ]]; then
  WITH_CS2=0   # existing install found: default to reuse mode
  DETECTED_OK=1
fi

# ----------------------------- preflight -----------------------------------
banner
step "Preflight"

if [[ $(uname -s) != Linux ]]; then die "linux only"; fi
if ! command -v systemctl &>/dev/null; then die "systemd is required (systemctl not found)"; fi
ok "linux + systemd detected: $(. /etc/os-release && echo "${PRETTY_NAME:-$(uname -r)}")"

MISSING=()
for tool in curl tar gzip; do command -v $tool &>/dev/null || MISSING+=("$tool"); done
if [[ ${#MISSING[@]} -gt 0 ]]; then
  info "installing missing tools: ${MISSING[*]}"
  if command -v apt-get &>/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${MISSING[@]}" >/dev/null
  elif command -v dnf &>/dev/null; then dnf install -y -q "${MISSING[@]}" >/dev/null; fi
fi
ok "base tools present"

# ----------------------------- questions -----------------------------------
step "Configuration"

PUBLIC_IP=$(curl -fs --max-time 4 https://api.ipify.org 2>/dev/null || hostname -I | awk '{print $1}')
info "public ip: ${PUBLIC_IP:-unknown}"

if [[ ${DETECTED_OK:-0} == 1 ]]; then
  ok "found existing CS2 install: $DETECTED_CS2_DIR (unit: ${DETECTED_UNIT:-?}, port: $DETECTED_PORT) — reusing it"
  info "answer with a dash (-) at any prompt to keep the detected value"
fi

ask PANEL_DOMAIN "Domain for the panel (optional — enables automatic HTTPS via Caddy, enter to skip)" ""
if [[ -n $PANEL_DOMAIN ]]; then
  info "caddy will serve https://$PANEL_DOMAIN → 127.0.0.1:$CS2A_PANEL_PORT"
fi

ask CS2A_ROOT "Install root" "$CS2A_ROOT"
ask GSLT "GSLT token for public servers (from steamcommunity.com/dev/managegameservers, enter to skip)" ""
if [[ $WITH_CS2 -eq 1 ]]; then
  ask INSTALL_CS2 "Install SteamCMD + CS2 server now? ~40GB (Y/n)" "Y"
  [[ ${INSTALL_CS2,,} == y* ]] || WITH_CS2=0
fi
if [[ $WITH_CS2 -eq 0 ]]; then
  ask EXISTING_CS2_DIR "Path of your existing CS2 install (contains game/)" "${DETECTED_CS2_DIR:-$CS2A_ROOT/cs2}"
  [[ $EXISTING_CS2_DIR == "-" ]] && EXISTING_CS2_DIR="${DETECTED_CS2_DIR:-$CS2A_ROOT/cs2}"
  ask CS2A_SERVICE_GAME "Name of your EXISTING CS2 systemd unit" "${DETECTED_UNIT:-$CS2A_SERVICE_GAME}"
  [[ $CS2A_SERVICE_GAME == "-" ]] && CS2A_SERVICE_GAME="${DETECTED_UNIT:-cs2-server}"
  info "the installer will keep your unit as-is and point cs2a at it"
fi
ask CS2A_GAME_PORT "Game port (A2S + RCON, must match the server launch)" "${DETECTED_PORT:-$CS2A_GAME_PORT}"
[[ $CS2A_GAME_PORT == "-" ]] && CS2A_GAME_PORT="${DETECTED_PORT:-27015}"
ask SET_PANEL_PORT "Panel port" "$CS2A_PANEL_PORT"
ask SETUP_ADMIN "Admin username for the panel" "admin"
ask_secret SETUP_PASS "Admin password"
ask_secret AGENT_TOKEN "Agent API token"
ask_secret SETUP_TOKEN "First-login setup token"
if [[ $WITH_CS2 -eq 0 ]]; then
  info "RCON password: use the one already in your server.cfg (and make sure the launch line has -usercon)"
fi
ask SETUP_WP_DB "Set up MariaDB + a database for skin sync (WeaponPaints)? (y/N)" "n"
ask_secret RCON_PASS "RCON password"

CS2_DIR="$CS2A_ROOT/cs2"
[[ $WITH_CS2 -eq 0 ]] && CS2_DIR="$EXISTING_CS2_DIR"

printf "\n%s%sINSTALL PLAN%s\n" "$BOLD" "$ORANGE" "$RESET"
cat <<PLAN
    root        : $CS2A_ROOT
    cs2 dir     : $CS2_DIR
    game server : systemd:$CS2A_SERVICE_GAME (port $CS2A_GAME_PORT)
    agent       : 127.0.0.1:$CS2A_AGENT_PORT (loopback, token auth)
    panel       : 0.0.0.0:$CS2A_PANEL_PORT  →  $([[ -n $PANEL_DOMAIN ]] && echo "https://$PANEL_DOMAIN (caddy, auto-HTTPS)" || echo "http://$PUBLIC_IP:$CS2A_PANEL_PORT")
    cs2 install : $([[ $WITH_CS2 -eq 1 ]] && echo "yes (steamcmd)" || echo "reuse existing")
PLAN
if [[ $UNATTENDED -eq 0 ]]; then
  read -r -p "$(printf '%s?%s Proceed? [Y/n]: ' "$ORANGE" "$RESET")" GO
  [[ ${GO,,} != n* ]] || die "aborted by user"
fi

# ----------------------------- directories ---------------------------------
step "Directories & user"
ensure_dir "$CS2A_ROOT" "$CS2A_ROOT/bin" "$CS2A_ROOT/etc" "$CS2A_ROOT/var" "$CS2A_ROOT/cache"
ensure_user "$CS2A_STEAM_USER"

# ----------------------------- binaries -------------------------------------
step "cs2a binaries"
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [[ -f $SCRIPT_DIR/../dist/cs2a-agent && -f $SCRIPT_DIR/../dist/cs2a-panel ]]; then
  cp -f "$SCRIPT_DIR/../dist/cs2a-agent" "$SCRIPT_DIR/../dist/cs2a-panel" "$CS2A_ROOT/bin/"
  ok "installed from local dist/ build"
else
  # fall back to prebuilt release download (repo releases carry binaries)
  CS2A_VERSION="${CS2A_VERSION:-latest}"
  BASE="https://github.com/elvonpiko/cs2a/releases"
  if [[ $CS2A_VERSION == latest ]]; then
    URL="$BASE/latest/download/cs2a-linux-amd64.tar.gz"
  else
    URL="$BASE/download/$CS2A_VERSION/cs2a-linux-amd64.tar.gz"
  fi
  info "downloading $URL"
  TMP=$(mktemp -d)
  curl -fL --retry 3 -o "$TMP/cs2a.tar.gz" "$URL"
  tar -xzf "$TMP/cs2a.tar.gz" -C "$TMP"
  cp -f "$TMP"/cs2a-agent "$TMP"/cs2a-panel "$CS2A_ROOT/bin/" 2>/dev/null \
    || cp -f "$TMP"/dist/* "$CS2A_ROOT/bin/"
  rm -rf "$TMP"
  ok "downloaded release binaries"
fi
chmod 755 "$CS2A_ROOT/bin/cs2a-agent" "$CS2A_ROOT/bin/cs2a-panel"

# ----------------------------- configs --------------------------------------
step "Config files"
CONFIG_CHANGED=0
write_if_changed() { # write_if_changed <file> <content>
  if [[ ! -f $1 ]] || ! grep -qxF "$2" <(head -c 0 "$1" >/dev/null; cat "$1" 2>/dev/null) 2>/dev/null; then
    :  # fallthrough below; simple compare
  fi
  if [[ ! -f $1 ]] || ! cmp -s <(printf '%s' "$2") "$1"; then
    umask 077; printf '%s' "$2" > "$1"; umask 022
    CONFIG_CHANGED=1
    info "wrote $1"
  else
    ok "$1 unchanged"
  fi
}

WP_JSON_LINE=""
[[ -n $WP_DSN ]] && WP_JSON_LINE=$',\n  "wp_dsn": "'$WP_DSN'"'

AGENT_JSON=$(cat <<EOF
{
  "listen": "127.0.0.1:$CS2A_AGENT_PORT",
  "token": "$AGENT_TOKEN",
  "cs2_dir": "$CS2_DIR",
  "service_name": "$CS2A_SERVICE_GAME",
  "rcon_addr": "127.0.0.1:$CS2A_GAME_PORT",
  "rcon_password": "$RCON_PASS",
  "a2s_addr": "127.0.0.1:$CS2A_GAME_PORT",
  "db_path": "$CS2A_ROOT/var/agent.db",
  "plugin_cache": "$CS2A_ROOT/cache/plugins"$([[ -n $WP_DSN ]] && echo ,
  "wp_dsn": "$WP_DSN")$
EOF
)
PANEL_JSON=$(cat <<EOF
{
  "listen": "0.0.0.0:$CS2A_PANEL_PORT",
  "agent_url": "http://127.0.0.1:$CS2A_AGENT_PORT",
  "agent_token": "$AGENT_TOKEN",
  "db_path": "$CS2A_ROOT/var/panel.db",
  "setup_token_file": "$CS2A_ROOT/etc/panel-setup-token"
}
EOF
)
write_if_changed "$CS2A_ROOT/etc/agent.json" "$AGENT_JSON"
write_if_changed "$CS2A_ROOT/etc/panel.json" "$PANEL_JSON"
write_if_changed "$CS2A_ROOT/etc/panel.env"  "$(printf 'CS2A_ADMIN_USER=%s\nCS2A_ADMIN_PASSWORD=%s\n' "$SETUP_ADMIN" "$SETUP_PASS")"

# setup token file only on first run (it is consumed by the panel)
if [[ ! -f $CS2A_ROOT/etc/panel-setup-token && ! -f $CS2A_ROOT/var/panel.db ]]; then
  printf '%s' "$SETUP_TOKEN" > "$CS2A_ROOT/etc/panel-setup-token"
  chmod 600 "$CS2A_ROOT/etc/panel-setup-token"
  ok "setup token written (consumed on first admin creation)"
fi

# ----------------------------- CS2 server -----------------------------------
if [[ $WITH_CS2 -eq 1 ]]; then
  step "SteamCMD"
  if command -v steamcmd &>/dev/null; then
    ok "steamcmd already installed"
  else
    if command -v apt-get &>/dev/null; then
      info "enabling multiverse + installing steamcmd (i386)…"
      dpkg --add-architecture i386
      (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq steamcmd lib32gcc-s1 libicu74 >/dev/null) \
        || (add-apt-repository -y multiverse >/dev/null 2>&1; apt-get update -qq; DEBIAN_FRONTEND=noninteractive apt-get install -y -qq steamcmd lib32gcc-s1 libicu74 >/dev/null)
    else
      die "steamcmd not found and apt unavailable — install it manually, then rerun with --no-cs2"
    fi
    ok "steamcmd installed"
  fi
  STEAMCMD_BIN=$(command -v steamcmd || echo "$HOME/.local/share/Steam/steamcmd/steamcmd.sh")
  # steamclient.so symlink for the srcds runtime
  sudo -u "$CS2A_STEAM_USER" bash -c "mkdir -p ~/.steam/sdk64 && ln -sf $(dirname "$STEAMCMD_BIN")/linux64/steamclient.so ~/.steam/sdk64/steamclient.so" || true
  ok "steamclient.so linked"

  step "CS2 dedicated server (~40GB, coffee break)"
  if [[ -f $CS2_DIR/game/csgo/gameinfo.gi ]]; then
    ok "CS2 already present at $CS2_DIR — skipping download (app_update will still run to refresh)"
  fi
  "$STEAMCMD_BIN" +force_install_dir "$CS2_DIR" +login anonymous +app_update "$CS2A_APP_ID" validate +quit 2>&1 |
    while IFS= read -r line; do [[ $line == *"Success"* || $line == *"Update state"* ]] && info "$line"; done
  [[ -f $CS2_DIR/game/csgo/gameinfo.gi ]] || die "CS2 install failed — gameinfo.gi missing"
  ok "CS2 installed"
  chown -R "$CS2A_STEAM_USER:$CS2A_STEAM_USER" "$CS2_DIR"

  step "server.cfg"
  CFG_DIR="$CS2_DIR/game/csgo/cfg"
  mkdir -p "$CFG_DIR"
  if [[ ! -f $CFG_DIR/server.cfg ]]; then
    cat > "$CFG_DIR/server.cfg" <<CFG
hostname "cs2a server"
rcon_password "$RCON_PASS"
sv_lan 0
sv_setsteamaccount "$GSLT"
CFG
    ok "server.cfg written"
  else
    grep -q '^rcon_password' "$CFG_DIR/server.cfg" || echo "rcon_password \"$RCON_PASS\"" >> "$CFG_DIR/server.cfg"
    ok "server.cfg kept, rcon_password ensured"
  fi
  chown -R "$CS2A_STEAM_USER:$CS2A_STEAM_USER" "$CFG_DIR"
fi

# ----------------------------- MariaDB (optional, skin sync) ----------------
WP_DSN=""
if [[ ${SETUP_WP_DB,,} == y* ]]; then
  step "MariaDB + WeaponPaints database"
  if ! command -v mysql &>/dev/null && ! command -v mariadb &>/dev/null; then
    if command -v apt-get &>/dev/null; then
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq mariadb-server >/dev/null
      ok "mariadb-server installed"
    else
      warn "no apt — install MariaDB manually, then rerun to configure the database"
    fi
  else
    ok "mariadb already present"
  fi
  if command -v mysql &>/dev/null || command -v mariadb &>/dev/null; then
    MYSQL_BIN=$(command -v mariadb || command -v mysql)
    WP_DB_PASS=$(head -c 18 /dev/urandom | base64 | tr -d '/=+\n' | cut -c1-20)
    systemctl enable --now mariadb >/dev/null 2>&1
    $MYSQL_BIN <<SQL
CREATE DATABASE IF NOT EXISTS cs2_wp CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER IF NOT EXISTS 'cs2a'@'localhost' IDENTIFIED BY '$WP_DB_PASS';
ALTER USER 'cs2a'@'localhost' IDENTIFIED BY '$WP_DB_PASS';
GRANT ALL PRIVILEGES ON cs2_wp.* TO 'cs2a'@'localhost';
FLUSH PRIVILEGES;
SQL
    WP_DSN="cs2a:$WP_DB_PASS@tcp(127.0.0.1:3306)/cs2_wp"
    ok "database cs2_wp ready (user cs2a)"
    info "WeaponPaints reads its own DB config — after installing it, paste these creds"
    info "into its config via Panel → Plugins → cs2-WeaponPaints → Edit config:"
    info "  host 127.0.0.1  port 3306  db cs2_wp  user cs2a  password $WP_DB_PASS"
  fi
fi

# ----------------------------- caddy (optional) -----------------------------
if [[ -n $PANEL_DOMAIN ]]; then
  step "Caddy reverse proxy (automatic HTTPS for $PANEL_DOMAIN)"
  if ! command -v caddy &>/dev/null; then
    if command -v apt-get &>/dev/null; then
      info "installing caddy from the official repository…"
      apt-get install -y -qq debian-keyring debian-archive-keyring apt-transport-https curl >/dev/null
      curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg 2>/dev/null
      curl -fsSL 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
      apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq caddy >/dev/null
      ok "caddy installed"
    else
      warn "caddy not installed automatically (no apt) — install it manually, the Caddyfile is still written"
      mkdir -p /etc/caddy
    fi
  else
    ok "caddy already installed"
  fi
  mkdir -p /etc/caddy
  # idempotent, minimal site block; keeps any existing config intact
  if ! grep -q "^$PANEL_DOMAIN " /etc/caddy/Caddyfile 2>/dev/null; then
    cat > /etc/caddy/Caddyfile <<CADDY
$PANEL_DOMAIN {
    reverse_proxy 127.0.0.1:$CS2A_PANEL_PORT
}

CADDY
    ok "Caddyfile written for $PANEL_DOMAIN"
  else
    ok "Caddyfile already has a $PANEL_DOMAIN site block"
  fi
  systemctl enable --now caddy >/dev/null 2>&1 || systemctl restart caddy
  systemctl reload caddy 2>/dev/null || true
  ok "caddy running — certificate for $PANEL_DOMAIN will be issued automatically"
  # when caddy fronts the panel, the raw HTTP port need not be public
  if command -v ufw &>/dev/null; then
    ufw delete allow "$CS2A_PANEL_PORT/tcp" >/dev/null 2>&1 || true
  fi
fi

# ----------------------------- firewall -------------------------------------
step "Firewall (safe: only game + panel port + 80/443 when needed)"
if command -v ufw &>/dev/null; then
  ufw allow "$CS2A_GAME_PORT/tcp" >/dev/null 2>&1 && ok "ufw allow $CS2A_GAME_PORT/tcp (rcon)"
  ufw allow "$CS2A_GAME_PORT/udp" >/dev/null 2>&1 && ok "ufw allow $CS2A_GAME_PORT/udp (game + a2s)"
  if [[ -n $PANEL_DOMAIN ]]; then
    ufw allow 80/tcp >/dev/null 2>&1 && ok "ufw allow 80/tcp (ACME http-01)"
    ufw allow 443/tcp >/dev/null 2>&1 && ok "ufw allow 443/tcp (panel via caddy)"
  else
    ufw allow "$CS2A_PANEL_PORT/tcp" >/dev/null 2>&1 && ok "ufw allow $CS2A_PANEL_PORT/tcp (panel)"
  fi
else
  info "ufw not present — if a firewall is active, open: $CS2A_GAME_PORT/{tcp,udp} and $CS2A_PANEL_PORT/tcp"
fi

# ----------------------------- systemd --------------------------------------
step "systemd units"
GAME_UNIT_FILE="/etc/systemd/system/$CS2A_SERVICE_GAME.service"
if [[ $WITH_CS2 -eq 1 ]]; then
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
ExecStart=$CS2_DIR/game/cs2.sh -dedicated -console -usercon +ip 0.0.0.0 +port $CS2A_GAME_PORT +map de_dust2 +maxplayers_override 12 ${GSLT:++sv_setsteamaccount $GSLT} +exec server.cfg
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
UNIT
elif [[ -f $GAME_UNIT_FILE ]]; then
  ok "keeping your existing $CS2A_SERVICE_GAME.service untouched"
else
  warn "unit $CS2A_SERVICE_GAME.service not found — the agent cannot start/stop/restart the server until a unit exists"
fi

cat > /etc/systemd/system/cs2a-agent.service <<UNIT
[Unit]
Description=cs2a agent (CS2 server manager)
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
Description=cs2a panel (control plane UI)
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

systemctl daemon-reload
systemctl enable --now cs2a-agent.service cs2a-panel.service >/dev/null 2>&1
ok "cs2a-agent + cs2a-panel enabled and started"
if [[ -f $GAME_UNIT_FILE ]]; then
  systemctl enable "$CS2A_SERVICE_GAME.service" >/dev/null 2>&1
  ok "$CS2A_SERVICE_GAME.service enabled (manage it from the panel)"
fi

# ----------------------------- summary --------------------------------------
sleep 1
HEALTH=$(curl -fs "http://127.0.0.1:$CS2A_AGENT_PORT/api/v1/health" 2>/dev/null || echo "")
PANEL_UP=$(curl -fs -o /dev/null -w '%{http_code}' "http://127.0.0.1:$CS2A_PANEL_PORT/login" 2>/dev/null || echo "")

printf "\n%s%s──────── INSTALLATION COMPLETE ────────%s\n\n" "$BOLD" "$GREEN" "$RESET"
cat <<SUMMARY
    Panel URL      : $([[ -n $PANEL_DOMAIN ]] && echo "https://$PANEL_DOMAIN" || echo "http://$PUBLIC_IP:$CS2A_PANEL_PORT")
    Admin user     : $SETUP_ADMIN
    Admin password : $SETUP_PASS
    Agent health   : ${HEALTH:+ok}${HEALTH:-starting…}
    Panel login    : HTTP $PANEL_UP
    Skin sync      : $([[ -n $WP_DSN ]] && echo "MariaDB configured (wp_dsn set)" || echo "not configured — rerun and choose MariaDB setup to enable")
    Config files   : $CS2A_ROOT/etc/{agent,panel}.json
    Service units  : cs2a-agent, cs2a-panel$([[ $WITH_CS2 -eq 1 ]] && echo ", $CS2A_SERVICE_GAME")

    ${DIM}Secrets are stored 0600 in $CS2A_ROOT/etc/. Keep them safe.${RESET}

    Next steps:
      1. Open the panel URL and sign in with the admin credentials.
      2. ${DIM}$( [[ $WITH_CS2 -eq 1 ]] && echo "Start the server from the Server page." || echo "Point the agent at your CS2 install and start it." )${RESET}
      3. Install plugins (Metamod + CounterStrikeSharp first) from the Plugins page.
SUMMARY
printf "\n"
