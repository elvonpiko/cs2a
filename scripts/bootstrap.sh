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
#    --panel-local           bind the panel to 127.0.0.1 (reach it over an SSH
#                            tunnel instead of exposing plain HTTP)
#    --no-firewall           do not touch ufw
#    -h, --help              show this header
#
#  Environment overrides: CS2A_ROOT, CS2A_PANEL_PORT, CS2A_AGENT_PORT,
#  CS2A_GAME_PORT, CS2A_SERVICE_GAME, CS2A_STEAM_USER, CS2A_PANEL_DOMAIN,
#  CS2A_GSLT, CS2A_RCON_PASSWORD, CS2A_ADMIN_USER, CS2A_VERSION,
#  CS2A_CS2_DIR, CS2A_PANEL_LOCAL.
# ============================================================================
set -euo pipefail

# ----------------------------- defaults ------------------------------------
CS2A_ROOT="${CS2A_ROOT:-/opt/cs2a}"
CS2A_SERVICE_GAME="${CS2A_SERVICE_GAME:-cs2-server}"
CS2A_AGENT_PORT="${CS2A_AGENT_PORT:-8100}"
CS2A_PANEL_PORT="${CS2A_PANEL_PORT:-8080}"
CS2A_GAME_PORT="${CS2A_GAME_PORT:-27015}"
# CS2A_STEAM_USER_EXPLICIT records that the operator named the account, so
# adopting a unit that runs as someone else does not silently override them.
CS2A_STEAM_USER_EXPLICIT="${CS2A_STEAM_USER:+1}"
CS2A_STEAM_USER="${CS2A_STEAM_USER:-steam}"
CS2A_APP_ID="${CS2A_APP_ID:-730}"
PANEL_DOMAIN="${CS2A_PANEL_DOMAIN:-}"
GSLT="${CS2A_GSLT:-}"
# RCON_PASS_EXPLICIT records an operator-supplied password, which then wins over
# whatever is in server.cfg (a rerun's agent.json value never does).
RCON_PASS_EXPLICIT="${CS2A_RCON_PASSWORD:+1}"
RCON_PASS="${CS2A_RCON_PASSWORD:-}"
SETUP_ADMIN="${CS2A_ADMIN_USER:-admin}"

UNATTENDED=0
WITH_CS2=-1        # -1 = decide from discovery, 0 = never, 1 = always
SETUP_SKIN_DB=-1   # -1 = ask, 0 = no, 1 = yes
TOUCH_FIREWALL=1
# PANEL_LOCAL_ONLY binds the panel to loopback even without a domain, for
# operators who reach it through an SSH tunnel instead of exposing plain HTTP.
PANEL_LOCAL_ONLY="${CS2A_PANEL_LOCAL:-0}"

# CS2A_BOOTSTRAP_LIB=1 sources only the helpers and discovery functions and
# returns before anything is inspected or changed. The test suite uses it to
# exercise this script's logic directly.
LIB_MODE="${CS2A_BOOTSTRAP_LIB:-0}"

if [[ $LIB_MODE != 1 ]]; then
# need_val guards options that take a separate operand. Without it
# "bootstrap.sh --domain" consumed the last argument inside the case body and
# then died on the trailing `shift` under errexit — exit 1, no message at all.
need_val() { # need_val <flag> <count> <value>
  [[ $2 -ge 2 && -n ${3:-} ]] || {
    printf 'cs2a: %s needs a value (try --help)\n' "$1" >&2; exit 2; }
}
while [[ $# -gt 0 ]]; do
  case "$1" in
    --unattended)  UNATTENDED=1 ;;
    --no-cs2)      WITH_CS2=0 ;;
    --with-cs2)    WITH_CS2=1 ;;
    --skin-db)     SETUP_SKIN_DB=1 ;;
    --no-skin-db)  SETUP_SKIN_DB=0 ;;
    --no-firewall) TOUCH_FIREWALL=0 ;;
    --panel-local) PANEL_LOCAL_ONLY=1 ;;
    --domain)      need_val --domain $# "${2:-}"; PANEL_DOMAIN="$2"; shift ;;
    --domain=*)    PANEL_DOMAIN="${1#*=}" ;;
    --root)        need_val --root $# "${2:-}"; CS2A_ROOT="$2"; shift ;;
    --root=*)      CS2A_ROOT="${1#*=}" ;;
    --panel-port)  need_val --panel-port $# "${2:-}"; CS2A_PANEL_PORT="$2"; shift ;;
    --panel-port=*) CS2A_PANEL_PORT="${1#*=}" ;;
    --game-port)   need_val --game-port $# "${2:-}"; CS2A_GAME_PORT="$2"; shift ;;
    --game-port=*) CS2A_GAME_PORT="${1#*=}" ;;
    -h|--help)     sed -n '2,37p' "${BASH_SOURCE[0]:-$0}"; exit 0 ;;
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
# Warnings scroll off the screen during a long install, so every one is also
# collected and replayed in the final summary. A missing -usercon used to be
# announced 300 lines above the "cs2a is installed" banner and never seen again.
WARNINGS=()
warn()  { WARNINGS+=("$1"); printf "    %s!%s %s\n" "$AMBER" "$RESET" "$1"; }
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

# json_field reads a top-level "key": "value" string out of a JSON file.
#
# The sed fallback cannot un-escape JSON: a stored password containing " or \
# came back mangled, and reuse_secrets then fed that wrong value to the agent
# while server.cfg kept the right one — a first install that worked broke on the
# second run. python3/jq are used when present; otherwise a value whose escaping
# is non-trivial is reported as unreadable rather than silently corrupted.
json_field() { # json_field <file> <key>
  [[ -f $1 ]] || return 1
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$1" "$2" <<'PY' 2>/dev/null || return 1
import json, sys
try:
    with open(sys.argv[1]) as fh:
        doc = json.load(fh)
except Exception:
    sys.exit(1)
val = doc.get(sys.argv[2])
if isinstance(val, str):
    sys.stdout.write(val)
elif val is not None:
    sys.stdout.write(str(val))
PY
    return 0
  fi
  local raw
  raw=$(sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\(\([^\"\\\\]\|\\\\.\)*\)\".*/\1/p" "$1" | head -1)
  # a backslash means the value was escaped on write; without a JSON parser we
  # cannot reverse that faithfully, so refuse instead of guessing
  case "$raw" in
    *\\*) return 1 ;;
  esac
  printf '%s' "$raw"
}

# cfg_value reads one console-variable value out of a CS2 .cfg file. It is the
# single parser for both "is it set?" and "what is it?": the two used to
# disagree (a trailing // comment made the value unreadable while the presence
# check still said yes), so cs2a generated a new RCON password, wrote it only
# into agent.json, and authenticated with a password the server never saw.
# Exit status: 0 value printed, 1 key absent, 2 key present but unparsable.
cfg_value() { # cfg_value <file> <cvar>
  [[ -f $1 ]] || return 1
  local line val
  line=$(grep -iE "^[[:space:]]*$2[[:space:]]+" "$1" | tail -1)
  [[ -n $line ]] || return 1
  # drop the cvar name, then any trailing comment (// or ;) outside quotes
  val=${line#"${line%%[![:space:]]*}"}
  val=${val#"$2"}; val=${val#"${val%%[![:space:]]*}"}
  case "$val" in
    '"'*)  val=${val#\"}; val=${val%%\"*} ;;
    "'"*)  val=${val#\'}; val=${val%%\'*} ;;
    *)     val=${val%%//*}; val=${val%%;*}; val=${val%%[[:space:]]*} ;;
  esac
  [[ -n $val ]] || return 2
  printf '%s' "$val"
}

# cfg_has_key reports whether the cvar appears at all, parsable or not.
cfg_has_key() { # cfg_has_key <file> <cvar>
  [[ -f $1 ]] && grep -qiE "^[[:space:]]*$2[[:space:]]" "$1"
}

# cs2_root_of walks up from a path until it finds the install root. Suffix
# stripping alone deleted a real directory component when the install was named
# "cs2" (WorkingDirectory=/home/cs2server/cs2 became /home/cs2server), which
# turned adoption into a second 40 GB download into a tree the running server
# never reads.
cs2_root_of() { # cs2_root_of <path>
  local p=${1%/}
  [[ -n $p ]] || return 1
  # a file path (cs2.sh, the cs2 binary) starts one level up
  [[ -d $p ]] || p=$(dirname "$p")
  local i c
  for ((i = 0; i < 6; i++)); do
    is_cs2_dir "$p" && { printf '%s' "$p"; return 0; }
    # Wrapper-managed installs (LinuxGSM) keep the game one level below the
    # working directory. Only probe downwards at the starting point: doing it on
    # the way up would happily claim an unrelated install in a sibling tree.
    if [[ $i -eq 0 ]]; then
      for c in "$p/serverfiles" "$p/cs2"; do
        is_cs2_dir "$c" && { printf '%s' "$c"; return 0; }
      done
    fi
    [[ $p == / || $p == . || -z $p ]] && break
    p=$(dirname "$p")
  done
  return 1
}

# ----------------------------- discovery ------------------------------------
OS_NAME=""; PKG=""
DETECTED_CS2_DIR=""; DETECTED_UNIT=""; DETECTED_PORT=""; DETECTED_RCON=""; DETECTED_GSLT=""
# DETECTED_IP is the existing unit's -ip. An adopted server that binds a public
# address is not reachable on 127.0.0.1, which is what made every RCON action
# fail with "connect: connection refused".
DETECTED_IP=""
# DETECTED_USERCON is 1 when the existing launch line already has -usercon.
DETECTED_USERCON=0
# DETECTED_USER is the account the game unit runs as. cs2a must chown the files
# it writes to that user, not to a "steam" account it invented.
DETECTED_USER=""
# DETECTED_UNIT_FILE is the fragment path of the adopted unit (it is not always
# under /etc/systemd/system: distro packages ship units in /lib).
DETECTED_UNIT_FILE=""
# DETECTED_RCON_UNREADABLE is 1 when server.cfg sets rcon_password but the value
# could not be parsed. Generating a fresh one in that case made the agent
# authenticate with a password the server had never seen.
DETECTED_RCON_UNREADABLE=0

STEAMCMD_BIN=""; HAVE_CADDY=0; HAVE_UFW=0; UFW_ACTIVE=0; HAVE_MYSQL=0
UFW_STATUS=""
# CADDY_SERVING is 1 only when Caddy accepted the config and reloaded, which is
# what decides whether the https:// URL in the summary is real.
CADDY_SERVING=0
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

# unit_exec_line returns the effective ExecStart command of a unit.
#
# systemd is asked first: `systemctl show` merges drop-ins (which routinely
# override ExecStart), unwraps backslash continuations and expands specifiers,
# none of which a sed over the fragment file can do. Reading the file is the
# fallback for fixtures and for units systemd does not know about.
unit_exec_line() { # unit_exec_line <unit-name> <fragment-file>
  local unit=$1 file=${2:-} raw=""
  if [[ -n $unit ]] && command -v systemctl >/dev/null 2>&1; then
    raw=$(systemctl show -p ExecStart --value "$unit.service" 2>/dev/null || true)
    # "{ path=… ; argv[]=/usr/bin/x -a -b ; ignore_errors=no ; … }"
    if [[ $raw == *"argv[]="* ]]; then
      raw=${raw#*argv[]=}
      raw=${raw%%;*}
      raw=${raw%%\}*}
      printf '%s' "${raw%"${raw##*[![:space:]]}"}"
      return 0
    fi
  fi
  [[ -f $file ]] || return 1
  # join backslash continuations, then take the first (last-wins is systemd's
  # rule for a single ExecStart, but a fragment with several is already odd)
  raw=$(sed -e ':a' -e '/\\$/{N;s/\\\n[[:space:]]*//;ba' -e '}' "$file" |
        sed -n 's/^ExecStart=//p' | head -1)
  # systemd's "-" / "@" / "+" / "!" ExecStart prefixes are not part of the path
  while [[ $raw == [-@+!:]* ]]; do raw=${raw#?}; done
  [[ -n $raw ]] || return 1
  printf '%s' "$raw"
}

# unit_property reads one merged unit property, falling back to the fragment.
unit_property() { # unit_property <unit-name> <Property> <fragment-file>
  local unit=$1 prop=$2 file=${3:-} v=""
  if [[ -n $unit ]] && command -v systemctl >/dev/null 2>&1; then
    v=$(systemctl show -p "$prop" --value "$unit.service" 2>/dev/null || true)
    [[ -n $v ]] && { printf '%s' "$v"; return 0; }
  fi
  [[ -f $file ]] || return 1
  v=$(sed -n "s/^$prop=//p" "$file" | tail -1)
  [[ -n $v ]] || return 1
  printf '%s' "$v"
}

# launch_flag reads a launch-line flag value ("-port N", "+port N", "-port=N").
launch_flag() { # launch_flag <exec-line> <flag>
  local v
  v=$(printf '%s' "$1" | grep -oE "[-+]$2[= ]+[^ ]+" | head -1 | grep -oE '[^= ]+$' || true)
  printf '%s' "$v"
}

# detect_unit finds the systemd unit that launches a CS2 server and reads the
# install dir, bind address, port, GSLT and service user out of its merged
# ExecStart / WorkingDirectory / User.
# CS2A_UNIT_DIRS is overridable so the test suite can point it at a fixture.
CS2A_UNIT_DIRS="${CS2A_UNIT_DIRS:-/etc/systemd/system /lib/systemd/system /usr/lib/systemd/system}"
detect_unit() {
  local d f unit exec_line wd root
  local -a found=()
  # CS2A_UNIT_DIRS is whitespace-separated (systemd's own search paths never
  # contain spaces); reading it into an array keeps the split explicit.
  local -a dirs=()
  read -r -a dirs <<<"$CS2A_UNIT_DIRS"
  for d in "${dirs[@]}"; do
    for f in "$d"/*.service; do
      [[ -f $f ]] || continue
      unit=$(basename "$f" .service)
      # template units cannot be started as-is ("cs2@" is not a service)
      [[ $unit == *@ ]] && continue
      exec_line=$(unit_exec_line "$unit" "$f" || true)
      [[ -n $exec_line ]] || continue
      # a CS2 launcher runs cs2.sh or the cs2 binary
      [[ $exec_line == *cs2.sh* || $exec_line == */cs2\ * || $exec_line == */cs2 ]] || continue
      found+=("$unit")
      # keep the first match's details; later ones only feed the warning below
      [[ -n $DETECTED_UNIT ]] && continue

      DETECTED_UNIT="$unit"
      DETECTED_UNIT_FILE="$f"
      wd=$(unit_property "$unit" WorkingDirectory "$f" || true)
      # the install root holds game/csgo/gameinfo.gi; walk up to it instead of
      # stripping suffixes, which used to eat a directory literally named cs2
      root=$(cs2_root_of "${wd:-$(printf '%s' "$exec_line" | awk '{print $1}')}" || true)
      [[ -z $root ]] && root=$(cs2_root_of "$(printf '%s' "$exec_line" | awk '{print $1}')" || true)
      [[ -n $root ]] && DETECTED_CS2_DIR="$root"

      DETECTED_PORT=$(launch_flag "$exec_line" port)
      # bind address: the agent has to dial exactly what the game binds
      DETECTED_IP=$(launch_flag "$exec_line" ip)
      # -usercon must be a real argument: a commented-out or Environment= copy
      # satisfied a plain grep over the unit file and hid a broken RCON setup
      case " $exec_line " in *" -usercon "*|*" -usercon") DETECTED_USERCON=1 ;; esac
      DETECTED_GSLT=$(launch_flag "$exec_line" sv_setsteamaccount)
      # the game user owns the files cs2a writes; assuming "steam" handed a
      # running server's cfg/ to the wrong account
      DETECTED_USER=$(unit_property "$unit" User "$f" || true)
    done
  done
  if [[ ${#found[@]} -gt 1 ]]; then
    warn "found ${#found[@]} CS2 units (${found[*]}) — managing ${found[0]}; rerun with CS2A_SERVICE_GAME=<unit> to pick another"
  fi
  return 0
}

# detect_server_cfg reads the values cs2a would otherwise have to ask for.
# DETECTED_RCON_UNREADABLE marks "rcon_password is set but we cannot read it",
# which must never be treated as "no password" — that generated a new one and
# left the agent authenticating with a secret the server never saw.
detect_server_cfg() {
  local cfg="$DETECTED_CS2_DIR/game/csgo/cfg/server.cfg"
  [[ -f $cfg ]] || return 0
  local v rc
  v=$(cfg_value "$cfg" rcon_password) && rc=0 || rc=$?
  if [[ $rc -eq 0 ]]; then
    DETECTED_RCON="$v"
  elif [[ $rc -eq 2 ]] || cfg_has_key "$cfg" rcon_password; then
    DETECTED_RCON_UNREADABLE=1
  fi
  v=$(cfg_value "$cfg" sv_setsteamaccount) || v=""
  [[ -n $v && -z $DETECTED_GSLT ]] && DETECTED_GSLT="$v"
  v=$(cfg_value "$cfg" hostport) || v=""
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
    [[ -n ${v:-} ]] && { SETUP_PASS="$v"; REUSED_SECRETS=1; PASS_REUSED=1; }
  fi
  v=$(json_field "$CS2A_ROOT/etc/panel.json" public_url || true)
  if [[ -n ${v:-} && -z $PANEL_DOMAIN && $v == https://* ]]; then
    PANEL_DOMAIN="${v#https://}"
  fi
}

AGENT_TOKEN=""; SETUP_PASS=""; SETUP_TOKEN=""; WP_DSN=""
# PASS_REUSED marks a password carried over from a previous install. The panel
# lets admins change their own password, so that value can be stale — printing
# it as "sign in with this" sent people to a login that rejects them.
PASS_REUSED=0

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

# A public address is only used for the printed URL, so nothing here may be
# fatal. Each fallback ends in `|| true`: the bare `hostname -I` assignment used
# to be the last command after `||`, so a machine without the hostname binary
# and without outbound internet aborted the whole installer with exit 127.
PUBLIC_IP=$(curl -fs --max-time 4 https://api.ipify.org 2>/dev/null || true)
[[ -n $PUBLIC_IP ]] || PUBLIC_IP=$(ip route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([^ ]*\).*/\1/p' | head -1 || true)
[[ -n $PUBLIC_IP ]] || PUBLIC_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
[[ -n $PUBLIC_IP ]] && found "public address: $PUBLIC_IP"

detect_unit
[[ -n $DETECTED_CS2_DIR ]] || DETECTED_CS2_DIR=$(detect_cs2_dir || true)
detect_server_cfg

# A unit was found but its install root was not: adopting it half-way would
# point cs2a at a tree the running server never reads (and, with --with-cs2,
# download a second 40 GB copy). Stop and let the operator name the path.
if [[ -n $DETECTED_UNIT && -z $DETECTED_CS2_DIR ]]; then
  warn "found $DETECTED_UNIT.service but could not locate its CS2 install"
  if [[ $UNATTENDED -eq 1 ]]; then
    die "cannot tell where $DETECTED_UNIT.service installed CS2 — rerun with CS2A_CS2_DIR=/path/to/install (the dir containing game/csgo/gameinfo.gi)"
  fi
  ask DETECTED_CS2_DIR "Path of the CS2 install $DETECTED_UNIT.service runs (contains game/)" "${CS2A_CS2_DIR:-}"
  [[ -n $DETECTED_CS2_DIR ]] || die "no path given — rerun once you know where that server is installed"
  is_cs2_dir "$DETECTED_CS2_DIR" || die "$DETECTED_CS2_DIR does not contain game/csgo/gameinfo.gi"
  detect_server_cfg
fi

if [[ -n $DETECTED_CS2_DIR ]]; then
  found "CS2 server: $DETECTED_CS2_DIR"
  [[ -n $DETECTED_UNIT ]] && found "systemd unit: $DETECTED_UNIT.service"
  [[ -n $DETECTED_USER ]] && found "runs as user: $DETECTED_USER"
  [[ -n $DETECTED_PORT ]] && found "game port: $DETECTED_PORT (from the launch line)"
  [[ -n $DETECTED_IP ]] && found "bind address: $DETECTED_IP (from the launch line)"
  [[ -n $DETECTED_RCON ]] && found "rcon_password: read from server.cfg (not shown)"
  [[ -n $DETECTED_GSLT ]] && found "GSLT: already configured"
  [[ -n $DETECTED_UNIT && $DETECTED_USERCON -eq 0 ]] \
    && warn "your launch line has no -usercon — the panel cannot change maps or run commands until it does (the panel offers a one-click fix)"
  [[ $WITH_CS2 -eq -1 ]] && WITH_CS2=0
else
  info "no CS2 install found"
fi

# The game user is whoever the existing unit runs as. Assuming "steam" created a
# pointless account and then handed a running server's cfg/ to it, so the game
# could no longer write the configs it owns.
if [[ -n $DETECTED_USER && $DETECTED_USER != "$CS2A_STEAM_USER" ]]; then
  if [[ -n ${CS2A_STEAM_USER_EXPLICIT:-} ]]; then
    warn "your unit runs as '$DETECTED_USER' but CS2A_STEAM_USER=$CS2A_STEAM_USER was set — using $CS2A_STEAM_USER"
  else
    CS2A_STEAM_USER="$DETECTED_USER"
    info "cs2a will write files as '$CS2A_STEAM_USER' (the user your server already runs as)"
  fi
fi

if detect_steamcmd; then found "steamcmd: $STEAMCMD_BIN"; fi
command -v caddy &>/dev/null && { HAVE_CADDY=1; found "caddy installed"; }
if command -v ufw &>/dev/null; then
  HAVE_UFW=1
  # `ufw status | head -1` exits 141 (SIGPIPE) on a long rule list, and pipefail
  # turned that into "firewall inactive" on an active firewall. Capture once.
  UFW_STATUS=$(ufw status 2>/dev/null || true)
  [[ $UFW_STATUS == *[Ss]tatus:*[Aa]ctive* ]] && UFW_ACTIVE=1
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

# RCON/A2S address: a wildcard bind is reachable on loopback (which keeps agent
# traffic off the network), but an explicit -ip must be dialled verbatim.
case "${DETECTED_IP:-}" in
  ""|0.0.0.0|::|"[::]"|"*") CS2A_RCON_HOST="127.0.0.1" ;;
  *) CS2A_RCON_HOST="$DETECTED_IP"
     info "the agent will reach RCON at $CS2A_RCON_HOST:$CS2A_GAME_PORT (your unit binds that address)" ;;
esac

# rcon password: server.cfg is the source of truth, because that is the file the
# game engine reads. A stale value in agent.json used to win, which left the
# agent authenticating with a password the server had never seen.
if [[ -n $DETECTED_RCON ]]; then
  if [[ -n $RCON_PASS && $RCON_PASS != "$DETECTED_RCON" ]]; then
    if [[ -n ${RCON_PASS_EXPLICIT:-} ]]; then
      warn "CS2A_RCON_PASSWORD differs from rcon_password in server.cfg — the game uses server.cfg, so change it there and restart the server"
    else
      info "server.cfg has a different rcon_password than the last install — using the one from server.cfg"
    fi
  fi
  [[ -n ${RCON_PASS_EXPLICIT:-} ]] || RCON_PASS="$DETECTED_RCON"
elif [[ $DETECTED_RCON_UNREADABLE -eq 1 && -z $RCON_PASS ]]; then
  # "set but unparsable" is not "absent": generating a password here wrote it
  # only into agent.json and every RCON call was rejected.
  die "server.cfg sets rcon_password but cs2a could not read the value.
    Put the password in the environment and rerun:
      sudo CS2A_RCON_PASSWORD='<the password>' bash bootstrap.sh
    (or simplify the line in $CS2_DIR/game/csgo/cfg/server.cfg to: rcon_password \"<password>\")"
elif [[ -z $RCON_PASS ]]; then
  RCON_PASS=$(gen_secret)
  info "generated an RCON password (written to server.cfg and agent.json)"
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

# Panel bind address. With a domain, Caddy fronts the panel and the panel itself
# only needs loopback. Without one there is no TLS, so a public bind sends the
# login form and its session cookie over plaintext HTTP — allowed (it is what
# makes the one-command install usable) but never silent.
if [[ -n $PANEL_DOMAIN ]]; then
  PANEL_BIND="127.0.0.1"
elif [[ $PANEL_LOCAL_ONLY -eq 1 ]]; then
  PANEL_BIND="127.0.0.1"
else
  PANEL_BIND="0.0.0.0"
fi

printf "\n%s%sPLAN%s\n" "$BOLD" "$SIGNAL" "$RESET"
cat <<PLAN
    install root : $CS2A_ROOT
    cs2 dir      : $CS2_DIR $([[ $WITH_CS2 -eq 1 ]] && echo "(steamcmd will install it)" || echo "(existing)")
    game server  : $CS2A_SERVICE_GAME.service on port $CS2A_GAME_PORT $([[ $MANAGE_GAME_UNIT -eq 1 ]] && echo "(cs2a writes this unit)" || echo "(yours, left untouched)")
    agent        : 127.0.0.1:$CS2A_AGENT_PORT (loopback only, bearer token)
    panel        : $PANEL_URL $([[ $PANEL_BIND == 127.0.0.1 ]] && echo "(bound to loopback)" || echo "(bound to all interfaces)")
    skin sync    : $([[ $SETUP_SKIN_DB -eq 1 ]] && echo "MariaDB will be provisioned" || { [[ -n $WP_DSN ]] && echo "already configured" || echo "off"; })
    firewall     : $([[ $TOUCH_FIREWALL -eq 1 && $HAVE_UFW -eq 1 ]] && echo "ufw rules for the ports above" || echo "not touched")
PLAN
if [[ -z $PANEL_DOMAIN && $PANEL_BIND != 127.0.0.1 ]]; then
  warn "no domain given: the panel is served over plain HTTP, so your password and session cookie cross the network unencrypted. Rerun with --domain <host> for automatic HTTPS, or with --panel-local and reach it through: ssh -L $CS2A_PANEL_PORT:127.0.0.1:$CS2A_PANEL_PORT <this-server>"
fi
if [[ $UNATTENDED -eq 0 ]]; then
  ask_yn "Proceed?" y || die "aborted — nothing was changed"
fi

# ----------------------------- directories ----------------------------------
step "Directories and service user"
ensure_dirs "$CS2A_ROOT" "$CS2A_ROOT/bin" "$CS2A_ROOT/etc" "$CS2A_ROOT/var" "$CS2A_ROOT/cache/plugins"
chmod 700 "$CS2A_ROOT/etc"
ok "layout ready under $CS2A_ROOT"
# An adopted server already has an account. Creating one anyway (and then
# chowning the game tree to it) is how a working server stopped being able to
# write its own configs.
if id -u "$CS2A_STEAM_USER" &>/dev/null; then
  skip "user '$CS2A_STEAM_USER' exists"
elif [[ -n $DETECTED_USER ]]; then
  die "your server runs as '$DETECTED_USER', which does not exist on this system — fix the unit's User= or rerun with CS2A_STEAM_USER=<account>"
else
  ensure_user "$CS2A_STEAM_USER"
fi

# ----------------------------- binaries -------------------------------------
step "cs2a binaries"
# BASH_SOURCE is unset when this script arrives on stdin (curl … | sudo bash),
# and under `set -u` that aborted here — after the install root and the service
# user had already been created.
if [[ -n ${BASH_SOURCE[0]:-} ]]; then
  SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
else
  SCRIPT_DIR=""
fi
if [[ -n $SCRIPT_DIR && -f $SCRIPT_DIR/../dist/cs2a-agent && -f $SCRIPT_DIR/../dist/cs2a-panel ]]; then
  install -m 0755 "$SCRIPT_DIR/../dist/cs2a-agent" "$SCRIPT_DIR/../dist/cs2a-panel" "$CS2A_ROOT/bin/"
  ok "installed from the local dist/ build"
else
  CS2A_VERSION="${CS2A_VERSION:-latest}"
  BASE="https://github.com/elvonpiko/cs2a/releases"
  # The release asset is per-architecture: a machine-independent name silently
  # installed amd64 binaries on arm64, and the only symptom was "agent not
  # answering" at the very end.
  case "$(uname -m)" in
    x86_64|amd64) ASSET_ARCH="amd64" ;;
    aarch64|arm64) ASSET_ARCH="arm64" ;;
    *) die "no cs2a release for $(uname -m).
    Build from source on this machine instead:
      git clone https://github.com/elvonpiko/cs2a && cd cs2a && make build
      sudo bash scripts/bootstrap.sh" ;;
  esac
  ASSET="cs2a-linux-$ASSET_ARCH.tar.gz"
  if [[ $CS2A_VERSION == latest ]]; then
    URL="$BASE/latest/download/$ASSET"
  else
    URL="$BASE/download/$CS2A_VERSION/$ASSET"
  fi
  info "downloading $URL"
  TMPD=$(mktemp -d)
  trap 'rm -rf "$TMPD"' EXIT
  curl -fL --retry 3 --retry-delay 2 -o "$TMPD/cs2a.tar.gz" "$URL" || die "no release archive at $URL
    A release exists only after a v* tag is pushed. Either pin CS2A_VERSION to an
    existing tag, or build locally (make build) and rerun this script from the
    repository so dist/cs2a-agent and dist/cs2a-panel are picked up."
  # These binaries run as root, so the published checksum is verified when it is
  # available (the release workflow publishes <asset>.sha256 next to the asset).
  if curl -fsL --max-time 20 -o "$TMPD/cs2a.tar.gz.sha256" "$URL.sha256" 2>/dev/null &&
     [[ -s $TMPD/cs2a.tar.gz.sha256 ]] && command -v sha256sum >/dev/null 2>&1; then
    WANT=$(awk '{print $1}' "$TMPD/cs2a.tar.gz.sha256" | head -1)
    GOT=$(sha256sum "$TMPD/cs2a.tar.gz" | awk '{print $1}')
    [[ -n $WANT && $WANT == "$GOT" ]] || die "checksum mismatch for $ASSET
    expected $WANT
    got      $GOT
    Refusing to install binaries that will run as root. Retry, or build from source."
    ok "release checksum verified"
  else
    warn "no published checksum for $ASSET — installed without verifying it"
  fi
  # --no-same-owner/--no-same-permissions: as root, tar otherwise restores
  # archived uid/gid and mode bits (including setuid) from the archive.
  tar --no-same-owner --no-same-permissions -xzf "$TMPD/cs2a.tar.gz" -C "$TMPD"
  AGENT_BIN=$(find "$TMPD" -type f -name cs2a-agent | head -1)
  PANEL_BIN=$(find "$TMPD" -type f -name cs2a-panel | head -1)
  [[ -n $AGENT_BIN && -n $PANEL_BIN ]] || die "release archive did not contain cs2a-agent and cs2a-panel"
  install -m 0755 "$AGENT_BIN" "$PANEL_BIN" "$CS2A_ROOT/bin/"
  rm -rf "$TMPD"
  trap - EXIT
  ok "installed release binaries ($CS2A_VERSION)"
fi
# A binary that cannot run (wrong architecture, missing loader) must fail here
# rather than 200 lines later as "agent not answering".
"$CS2A_ROOT/bin/cs2a-agent" -h >/dev/null 2>&1 ||
  die "$CS2A_ROOT/bin/cs2a-agent will not run on this machine ($(uname -m)).
    Try: $CS2A_ROOT/bin/cs2a-agent -h
    Building from source (make build) produces binaries for this host."
"$CS2A_ROOT/bin/cs2a-panel" -h >/dev/null 2>&1 ||
  die "$CS2A_ROOT/bin/cs2a-panel will not run on this machine ($(uname -m))"
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
  # steamcmd exits non-zero routinely (rate limits, disk full, 0x202). With
  # pipefail the pipeline's failure used to terminate the script silently, one
  # line before the die below could report anything.
  STEAM_LOG=$(mktemp)
  set +o pipefail
  sudo -u "$CS2A_STEAM_USER" "$STEAMCMD_BIN" +force_install_dir "$CS2_DIR" \
    +login anonymous +app_update "$CS2A_APP_ID" validate +quit 2>&1 |
    tee "$STEAM_LOG" |
    while IFS= read -r line; do
      case "$line" in
        *"Update state"*|*"Success"*|*"ERROR"*|*"error"*) info "$line" ;;
      esac
    done
  STEAM_RC=${PIPESTATUS[0]}
  set -o pipefail
  if [[ $STEAM_RC -ne 0 ]] && ! is_cs2_dir "$CS2_DIR"; then
    printf '\n'
    tail -n 15 "$STEAM_LOG" | while IFS= read -r line; do info "$line"; done
    rm -f "$STEAM_LOG"
    die "steamcmd exited $STEAM_RC and CS2 is not installed (see the last lines above).
    Common causes: not enough free disk space, a Steam rate limit (retry in a few
    minutes), or a partially written install — rerun this script to resume."
  fi
  [[ $STEAM_RC -eq 0 ]] || warn "steamcmd exited $STEAM_RC but the install looks complete — verify the server starts"
  rm -f "$STEAM_LOG"
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
CFG_NEEDS_RESTART=0
ensure_dirs "$CFG_DIR"
if [[ ! -f $CFG_DIR/server.cfg ]]; then
  # 0640 with the game group: the file holds the RCON password, and a
  # world-readable copy hands full server control to every local account.
  ( umask 027
    cat > "$CFG_DIR/server.cfg" <<CFG
hostname "cs2a server"
rcon_password "$RCON_PASS"
sv_lan 0
$([[ -n $GSLT ]] && printf 'sv_setsteamaccount "%s"\n' "$GSLT")
CFG
  )
  ok "server.cfg created (0640 — it holds the RCON password)"
  CFG_NEEDS_RESTART=1
elif cfg_has_key "$CFG_DIR/server.cfg" rcon_password; then
  skip "server.cfg already sets rcon_password"
else
  printf '\nrcon_password "%s"\n' "$RCON_PASS" >> "$CFG_DIR/server.cfg"
  chmod o-rwx "$CFG_DIR/server.cfg" 2>/dev/null || true
  ok "added rcon_password to your server.cfg (everything else untouched)"
  CFG_NEEDS_RESTART=1
fi

# CS2 reads rcon_password when the map loads, so a server that is running right
# now still has no RCON listener. Offer the restart instead of letting the panel
# report "connection refused" as the user's first experience.
if [[ $CFG_NEEDS_RESTART -eq 1 ]] && systemctl is-active --quiet "$CS2A_SERVICE_GAME.service" 2>/dev/null; then
  if [[ $UNATTENDED -eq 0 ]] && ask_yn "Your server is running with the old config. Restart $CS2A_SERVICE_GAME.service now so RCON starts working?" y; then
    if systemctl restart "$CS2A_SERVICE_GAME.service" 2>/dev/null; then
      ok "restarted $CS2A_SERVICE_GAME.service — RCON will be reachable once the map finishes loading"
    else
      warn "could not restart $CS2A_SERVICE_GAME.service — run: systemctl restart $CS2A_SERVICE_GAME.service"
    fi
  else
    warn "your server is running with the old config — restart it (systemctl restart $CS2A_SERVICE_GAME.service, or the panel's Restart button) so RCON starts working"
  fi
fi

# The game user must keep write access to its own config directory: plugins such
# as CounterStrikeSharp generate configs at runtime. Only fix ownership when it
# is actually wrong, and say so when it cannot be done.
CFG_OWNER=$(stat -c '%U' "$CFG_DIR" 2>/dev/null || echo "")
if [[ -n $CFG_OWNER && $CFG_OWNER != "$CS2A_STEAM_USER" ]]; then
  if chown -R "$CS2A_STEAM_USER:$CS2A_STEAM_USER" "$CFG_DIR" 2>/dev/null; then
    ok "cfg/ now belongs to $CS2A_STEAM_USER (was $CFG_OWNER)"
  else
    warn "could not chown $CFG_DIR to $CS2A_STEAM_USER — the server may not be able to write its configs"
  fi
else
  chown "$CS2A_STEAM_USER:$CS2A_STEAM_USER" "$CFG_DIR/server.cfg" 2>/dev/null ||
    warn "could not chown $CFG_DIR/server.cfg to $CS2A_STEAM_USER"
fi

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
    # A rerun must not rotate the password: WeaponPaints' own config still holds
    # the old one, and skin sync would break silently. Reuse the DSN when it is
    # already known, and only create the account when it does not exist.
    if [[ -n $WP_DSN ]]; then
      skip "skin database already configured (reusing the existing credentials)"
    else
      WP_DB_PASS=$(gen_secret)
      # An existing 'cs2a' account whose password cs2a does not know cannot be
      # rotated safely: WeaponPaints may already be using it.
      WP_USER_EXISTS=$("$MYSQL_BIN" -N -B -e \
        "SELECT 1 FROM mysql.user WHERE user='cs2a' AND host='localhost' LIMIT 1" 2>/dev/null || true)
      if [[ -n ${WP_USER_EXISTS//[[:space:]]/} ]]; then
        warn "MySQL user 'cs2a' already exists but cs2a has no record of its password — skin sync stays off. Drop the user (or put its DSN in agent.json as wp_dsn) and rerun."
      elif "$MYSQL_BIN" <<SQL >/dev/null 2>&1
CREATE DATABASE IF NOT EXISTS cs2_wp CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'cs2a'@'localhost' IDENTIFIED BY '$WP_DB_PASS';
GRANT ALL PRIVILEGES ON cs2_wp.* TO 'cs2a'@'localhost';
FLUSH PRIVILEGES;
SQL
      then
        WP_DSN="cs2a:$WP_DB_PASS@tcp(127.0.0.1:3306)/cs2_wp"
        ok "database cs2_wp ready (user cs2a)"
        # The agent writes these into WeaponPaints.json itself at install time
        # (see writeWeaponPaintsDefaultConfig), so nothing has to be retyped and
        # the password never has to appear on screen.
        info "WeaponPaints will be configured with this database automatically when you install it"
        info "the credentials live in $CS2A_ROOT/etc/agent.json (0600)"
      else
        warn "could not provision the database (is the root socket login available?) — skin sync stays off"
      fi
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
  \"rcon_addr\": \"$CS2A_RCON_HOST:$CS2A_GAME_PORT\",
  \"rcon_password\": $(json_str "$RCON_PASS"),
  \"a2s_addr\": \"$CS2A_RCON_HOST:$CS2A_GAME_PORT\",
  \"db_path\": $(json_str "$CS2A_ROOT/var/agent.db"),
  \"plugin_cache\": $(json_str "$CS2A_ROOT/cache/plugins")"
[[ -n $WP_DSN ]] && AGENT_JSON+=",
  \"wp_dsn\": $(json_str "$WP_DSN")"
AGENT_JSON+="
}
"

PANEL_JSON="{
  \"listen\": \"$PANEL_BIND:$CS2A_PANEL_PORT\",
  \"agent_url\": \"http://127.0.0.1:$CS2A_AGENT_PORT\",
  \"agent_token\": $(json_str "$AGENT_TOKEN"),
  \"db_path\": $(json_str "$CS2A_ROOT/var/panel.db"),
  \"setup_token_file\": $(json_str "$CS2A_ROOT/etc/panel-setup-token")"
[[ -n $PANEL_DOMAIN ]] && PANEL_JSON+=",
  \"public_url\": $(json_str "https://$PANEL_DOMAIN")"
PANEL_JSON+="
}
"

write_secret "$CS2A_ROOT/etc/agent.json" "$AGENT_JSON" || true
write_secret "$CS2A_ROOT/etc/panel.json" "$PANEL_JSON" || true
write_secret "$CS2A_ROOT/etc/panel.env" \
  "$(printf 'CS2A_ADMIN_USER=%s\nCS2A_ADMIN_PASSWORD=%s\n' "$SETUP_ADMIN" "$SETUP_PASS")" || true

# The binaries themselves are the authority on what a valid config is, so both
# are validated with the code that will read them. This used to be a python3
# one-liner that only checked agent.json and did nothing at all when python3 was
# missing.
CHECK_OUT=$("$CS2A_ROOT/bin/cs2a-agent" -config "$CS2A_ROOT/etc/agent.json" -check 2>&1) ||
  die "agent.json is not usable:
    $CHECK_OUT"
CHECK_OUT=$("$CS2A_ROOT/bin/cs2a-panel" -config "$CS2A_ROOT/etc/panel.json" -check 2>&1) ||
  die "panel.json is not usable:
    $CHECK_OUT"
ok "agent.json and panel.json validated by the binaries that read them"

# The setup token is consumed by the panel when the first admin is created. Only
# a token that is actually on disk may be printed: a rerun regenerates the value
# in memory, and printing that one had the user typing a code the panel rejects.
if [[ ! -f $CS2A_ROOT/var/panel.db ]]; then
  ( umask 177; printf '%s' "$SETUP_TOKEN" > "$CS2A_ROOT/etc/panel-setup-token" )
  SETUP_TOKEN_LIVE="$SETUP_TOKEN"
  ok "first-login setup token written"
elif [[ -f $CS2A_ROOT/etc/panel-setup-token ]]; then
  SETUP_TOKEN_LIVE=$(cat "$CS2A_ROOT/etc/panel-setup-token" 2>/dev/null || true)
  skip "panel database exists — keeping the setup token already on disk"
else
  SETUP_TOKEN_LIVE=""
  skip "panel database exists — setup token already used"
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
  CADDY_SITE="# cs2a panel — managed by cs2a, safe to delete when you uninstall it
$PANEL_DOMAIN {
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
  # cs2a owns its own file and the main Caddyfile only imports it. Detecting an
  # existing site block by grepping for "<domain> {" missed the forms people
  # actually write ("https://host {", "host, www.host {") and appended a second
  # block, which broke the whole config; a fixed-string import line cannot.
  CADDY_SITE_FILE="/etc/caddy/cs2a.caddyfile"
  CADDY_IMPORT="import $CADDY_SITE_FILE"
  CADDY_RESTORE=""
  printf '%s' "$CADDY_SITE" > "$CADDY_SITE_FILE"
  ok "wrote $CADDY_SITE_FILE (the panel's site block)"
  if [[ ! -f /etc/caddy/Caddyfile ]]; then
    printf '%s\n' "$CADDY_IMPORT" > /etc/caddy/Caddyfile
    ok "Caddyfile created, importing the cs2a site"
  elif grep -qxF "$CADDY_IMPORT" /etc/caddy/Caddyfile; then
    skip "Caddyfile already imports $CADDY_SITE_FILE"
  else
    CADDY_RESTORE="/etc/caddy/Caddyfile.cs2a-backup.$(date +%s)"
    cp -a /etc/caddy/Caddyfile "$CADDY_RESTORE"
    # An import has to be at the top level: prepending keeps it out of any site
    # block the operator already has at the end of the file.
    printf '%s\n\n%s' "$CADDY_IMPORT" "$(cat /etc/caddy/Caddyfile)" > /etc/caddy/Caddyfile
    ok "added the cs2a import to your Caddyfile (backup: $CADDY_RESTORE)"
  fi
  if [[ $HAVE_CADDY -eq 1 ]]; then
    if caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1; then
      systemctl enable caddy >/dev/null 2>&1 || true
      if systemctl reload caddy 2>/dev/null || systemctl restart caddy 2>/dev/null; then
        CADDY_SERVING=1
        ok "caddy running — a certificate for $PANEL_DOMAIN is issued on first request"
      else
        warn "caddy would not reload — run: systemctl status caddy"
      fi
    else
      # A broken config left in place would keep Caddy on its old one and the
      # summary would still advertise https://<domain>. Put the file back.
      if [[ -n $CADDY_RESTORE && -f $CADDY_RESTORE ]]; then
        cp -a "$CADDY_RESTORE" /etc/caddy/Caddyfile
        warn "the combined Caddyfile did not validate — your original was restored; add '$CADDY_IMPORT' by hand and run: caddy validate --config /etc/caddy/Caddyfile"
      else
        warn "the Caddyfile did not validate — check /etc/caddy/Caddyfile and run: systemctl reload caddy"
      fi
    fi
  fi
fi
# Without a working Caddy there is no HTTPS listener. A domain also means the
# panel itself is bound to loopback, so the public IP is NOT reachable — telling
# the operator to open http://<ip>:<port> sent them to a port nothing answers on.
if [[ -n $PANEL_DOMAIN && $CADDY_SERVING -eq 0 ]]; then
  warn "caddy is not serving $PANEL_DOMAIN yet, and the panel is bound to loopback because a domain was given — until Caddy works, reach it with: ssh -L $CS2A_PANEL_PORT:127.0.0.1:$CS2A_PANEL_PORT root@${PUBLIC_IP:-this-server} and open http://127.0.0.1:$CS2A_PANEL_PORT"
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
    # ufw's own status output is the only source of truth here, and it must be
    # read in one shot: `ufw status | grep` under pipefail turned a long rule
    # list (SIGPIPE, status 141) into "rule missing" and re-added rules forever.
    local rules; rules=$(ufw status 2>/dev/null || true)
    if grep -qE "^${1%% *}([[:space:]]|/|\$)" <<<"$rules"; then
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
    # Closing the raw panel port is only safe once Caddy actually serves the
    # domain — otherwise this removes the one rule that made the panel
    # reachable and locks the operator out of their own install.
    if [[ $CADDY_SERVING -eq 1 ]]; then
      ufw delete allow "$CS2A_PANEL_PORT/tcp" >/dev/null 2>&1 && info "closed $CS2A_PANEL_PORT/tcp (caddy fronts the panel)" || true
    else
      ufw_allow "$CS2A_PANEL_PORT/tcp" "panel (caddy is not serving yet)"
    fi
  else
    ufw_allow "$CS2A_PANEL_PORT/tcp" "panel"
  fi
  [[ $UFW_ACTIVE -eq 0 ]] && info "ufw is inactive — rules apply once you run: ufw enable"
fi

# ----------------------------- systemd --------------------------------------
step "systemd units"
# An adopted unit does not have to live in /etc/systemd/system: distro and
# package units live under /lib or /usr/lib, and assuming the /etc path made
# cs2a skip its own -usercon check and then claim the unit did not exist.
GAME_UNIT_FILE=""
if [[ $MANAGE_GAME_UNIT -eq 1 ]]; then
  GAME_UNIT_FILE="/etc/systemd/system/$CS2A_SERVICE_GAME.service"
else
  GAME_UNIT_FILE=$(systemctl show -p FragmentPath --value "$CS2A_SERVICE_GAME.service" 2>/dev/null || true)
  [[ -n $GAME_UNIT_FILE ]] || GAME_UNIT_FILE="$DETECTED_UNIT_FILE"
  [[ -n $GAME_UNIT_FILE ]] || GAME_UNIT_FILE="/etc/systemd/system/$CS2A_SERVICE_GAME.service"
fi
# GAME_UNIT_EXISTS is what decides whether the panel has anything to control.
GAME_UNIT_EXISTS=0
[[ -f $GAME_UNIT_FILE ]] && GAME_UNIT_EXISTS=1
if [[ $GAME_UNIT_EXISTS -eq 0 ]] &&
   systemctl list-unit-files "$CS2A_SERVICE_GAME.service" 2>/dev/null | grep -q "$CS2A_SERVICE_GAME.service"; then
  GAME_UNIT_EXISTS=1
fi

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
  GAME_UNIT_EXISTS=1
  ok "wrote $CS2A_SERVICE_GAME.service"
elif [[ $GAME_UNIT_EXISTS -eq 1 ]]; then
  skip "keeping your existing $CS2A_SERVICE_GAME.service untouched ($GAME_UNIT_FILE)"
  # The merged launch line is the only thing that matters: a commented-out
  # "# -usercon" or one inside an Environment= line satisfied a plain grep over
  # the unit file and hid the exact problem this check exists for.
  if [[ $DETECTED_USERCON -eq 0 ]]; then
    ADOPTED_EXEC=$(unit_exec_line "$CS2A_SERVICE_GAME" "$GAME_UNIT_FILE" || true)
    case " $ADOPTED_EXEC " in
      *" -usercon "*|*" -usercon") DETECTED_USERCON=1 ;;
    esac
  fi
  if [[ $DETECTED_USERCON -eq 0 ]]; then
    # A drop-in adds the flag without touching the operator's unit file, and it
    # is exactly what the panel's "Fix it for me" button writes.
    if [[ $UNATTENDED -eq 0 ]] && ask_yn "Your launch line has no -usercon, so RCON cannot work. Add it with a systemd drop-in now?" y; then
      ADOPTED_EXEC=$(unit_exec_line "$CS2A_SERVICE_GAME" "$GAME_UNIT_FILE" || true)
      if [[ -n $ADOPTED_EXEC ]]; then
        ensure_dirs "/etc/systemd/system/$CS2A_SERVICE_GAME.service.d"
        cat > "/etc/systemd/system/$CS2A_SERVICE_GAME.service.d/10-cs2a-usercon.conf" <<DROPIN
# managed by cs2a — adds -usercon so the panel can use RCON.
# Delete this file to go back to your original launch line.
[Service]
ExecStart=
ExecStart=$ADOPTED_EXEC -usercon
DROPIN
        systemctl daemon-reload
        DETECTED_USERCON=1
        ok "added -usercon via a drop-in (restart the server to apply it)"
        warn "restart $CS2A_SERVICE_GAME.service to pick up -usercon: systemctl restart $CS2A_SERVICE_GAME.service"
      else
        warn "could not read the current ExecStart of $CS2A_SERVICE_GAME.service — add -usercon by hand"
      fi
    else
      warn "your unit's ExecStart has no -usercon: RCON (map changes, commands) will not work until you add it — the panel's Server page has a one-click fix"
    fi
  fi
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
# A failing restart must not abort the run: the summary carries the admin
# password and the setup code, and without them the install is unrecoverable
# without reading this script.
for svc in cs2a-agent cs2a-panel; do
  if systemctl restart "$svc.service" 2>/dev/null; then
    continue
  fi
  warn "$svc.service did not start — see the log lines below and: journalctl -u $svc -n 50"
  journalctl -u "$svc" -n 12 --no-pager 2>/dev/null | while IFS= read -r line; do info "$line"; done
  START_FAILED=1
done
[[ ${START_FAILED:-0} -eq 1 ]] || ok "agent and panel started"

# Enabling a unit changes what happens at boot, which is the operator's
# decision for a server cs2a did not create.
if [[ $MANAGE_GAME_UNIT -eq 1 ]]; then
  systemctl enable "$CS2A_SERVICE_GAME.service" >/dev/null 2>&1 || true
  ok "$CS2A_SERVICE_GAME.service enabled (start it from the panel)"
elif [[ $GAME_UNIT_EXISTS -eq 1 ]]; then
  GAME_ENABLED=$(systemctl is-enabled "$CS2A_SERVICE_GAME.service" 2>/dev/null || true)
  case "$GAME_ENABLED" in
    enabled*) skip "$CS2A_SERVICE_GAME.service already starts at boot" ;;
    *) info "$CS2A_SERVICE_GAME.service is ${GAME_ENABLED:-not enabled} — left as you had it (systemctl enable $CS2A_SERVICE_GAME.service to change that)" ;;
  esac
fi

# ----------------------------- verify ---------------------------------------
step "Verifying"
AGENT_OK=0; PANEL_OK=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  # The authenticated endpoint, not /api/v1/health: health is unauthenticated,
  # so it proved nothing about the token the panel will use.
  curl -fs --max-time 3 -H "Authorization: Bearer $AGENT_TOKEN" \
    "http://127.0.0.1:$CS2A_AGENT_PORT/api/v1/status" -o /tmp/cs2a-status.$$ 2>/dev/null && { AGENT_OK=1; break; }
  sleep 1
done
for _ in 1 2 3 4 5 6 7 8 9 10; do
  code=$(curl -fs -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$CS2A_PANEL_PORT/login" 2>/dev/null || echo 000)
  [[ $code == 200 ]] && { PANEL_OK=1; break; }
  sleep 1
done
if [[ $AGENT_OK -eq 1 ]]; then
  ok "agent healthy on 127.0.0.1:$CS2A_AGENT_PORT (token accepted)"
  # The agent reports exactly what the panel will show, including why RCON is
  # not reachable — the whole control path, not just an open port.
  STATUS_JSON=$(cat "/tmp/cs2a-status.$$" 2>/dev/null || true)
  case "$STATUS_JSON" in
    *'"active":true'*) ok "$CS2A_SERVICE_GAME.service is running" ;;
    *) info "$CS2A_SERVICE_GAME.service is not running — start it from the panel" ;;
  esac
  DIAG_REASON=$(printf '%s' "$STATUS_JSON" |
    sed -n 's/.*"diag":{[^}]*"reason":"\([^"]*\)".*/\1/p' | head -1)
  if [[ -n $DIAG_REASON ]]; then
    warn "RCON is not usable yet: $DIAG_REASON (the panel's Server page can fix this)"
  elif [[ $STATUS_JSON == *'"rcon"'* ]]; then
    ok "RCON answered on $CS2A_RCON_HOST:$CS2A_GAME_PORT"
  fi
else
  warn "agent not answering — check: journalctl -u cs2a-agent -n 50"
fi
rm -f "/tmp/cs2a-status.$$"
# LoadState tells the operator whether the unit the agent was configured with
# actually exists, which is what "the buttons do nothing" usually comes down to.
if [[ $GAME_UNIT_EXISTS -eq 1 ]]; then
  LOAD_STATE=$(systemctl show -p LoadState --value "$CS2A_SERVICE_GAME.service" 2>/dev/null || true)
  [[ $LOAD_STATE == loaded ]] || warn "systemd reports $CS2A_SERVICE_GAME.service as '${LOAD_STATE:-unknown}' — the panel's Start/Stop buttons cannot work until that unit loads"
else
  warn "the agent is configured for $CS2A_SERVICE_GAME.service, which does not exist — Start/Stop/Restart will fail until it does"
fi
[[ $PANEL_OK -eq 1 ]] && ok "panel serving on port $CS2A_PANEL_PORT" \
  || warn "panel not answering — check: journalctl -u cs2a-panel -n 50"

# ----------------------------- summary --------------------------------------
# The URL must be one that actually works right now: a domain whose Caddy never
# came up, or a loopback bind, are both cases where the planned URL is wrong.
if [[ -n $PANEL_DOMAIN && $CADDY_SERVING -eq 1 ]]; then
  PANEL_URL="https://$PANEL_DOMAIN"
elif [[ $PANEL_BIND == 127.0.0.1 ]]; then
  PANEL_URL="http://127.0.0.1:$CS2A_PANEL_PORT"
else
  PANEL_URL="http://${PUBLIC_IP:-127.0.0.1}:$CS2A_PANEL_PORT"
fi
printf "\n%s%s  cs2a is installed  %s\n\n" "$BOLD" "$SIGNAL" "$RESET"
cat <<SUMMARY
    Panel      $PANEL_URL
    Sign in    $SETUP_ADMIN / $([[ $PASS_REUSED -eq 1 ]] && echo "(the password from your first install — or whatever you changed it to)" || echo "$SETUP_PASS")
    Setup code $([[ -n ${SETUP_TOKEN_LIVE:-} ]] && echo "$SETUP_TOKEN_LIVE" || echo "(already used — sign in with the credentials above)")

    Server     $CS2A_SERVICE_GAME.service on port $CS2A_GAME_PORT
    RCON       $CS2A_RCON_HOST:$CS2A_GAME_PORT
    Config     $CS2A_ROOT/etc/{agent,panel}.json  ${DIM}(0600, contains secrets)${RESET}
    Logs       journalctl -u cs2a-panel -u cs2a-agent -f

    Next:
      1. Open $PANEL_URL and sign in.
      2. Plugins → install Metamod:Source, then CounterStrikeSharp.
      3. Server → Start.
SUMMARY
if [[ $PANEL_BIND == 127.0.0.1 && -z $PANEL_DOMAIN ]]; then
  printf "\n    %sThe panel listens on loopback only. Reach it with:%s\n      ssh -L %s:127.0.0.1:%s %s\n" \
    "$DIM" "$RESET" "$CS2A_PANEL_PORT" "$CS2A_PANEL_PORT" "root@${PUBLIC_IP:-this-server}"
elif [[ $PANEL_BIND == 127.0.0.1 && $CADDY_SERVING -eq 0 ]]; then
  # A domain was configured but Caddy is not serving it: the panel is on
  # loopback and nothing fronts it, so the only way in is a tunnel. Printing the
  # https:// URL here would be a dead link.
  printf "\n    %sCaddy is not serving %s yet, and the panel is on loopback. Until then:%s\n      ssh -L %s:127.0.0.1:%s %s\n      then open http://127.0.0.1:%s\n" \
    "$DIM" "$PANEL_DOMAIN" "$RESET" "$CS2A_PANEL_PORT" "$CS2A_PANEL_PORT" "root@${PUBLIC_IP:-this-server}" "$CS2A_PANEL_PORT"
fi
if [[ ${#WARNINGS[@]} -gt 0 ]]; then
  printf "\n    %sWorth your attention:%s\n" "$BOLD" "$RESET"
  for w in "${WARNINGS[@]}"; do
    printf "      %s!%s %s\n" "$AMBER" "$RESET" "$w"
  done
fi
[[ -n $PANEL_DOMAIN ]] && printf "\n    %sPoint %s at %s before the first HTTPS request, or Caddy cannot get a certificate.%s\n" \
  "$DIM" "$PANEL_DOMAIN" "${PUBLIC_IP:-this server}" "$RESET"
printf "\n"
