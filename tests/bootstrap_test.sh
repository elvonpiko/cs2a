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
checknot() { # checknot <label> <cmd...>  (passes when the command fails)
  local label=$1; shift
  if "$@" >/dev/null 2>&1; then
    echo "FAIL  $label"
    FAILED=1
  else
    echo "  ok  $label"
  fi
}
checkeq() { # checkeq <label> <got> <want>
  if [[ "$2" == "$3" ]]; then
    echo "  ok  $1"
  else
    echo "FAIL  $1 (got '$2', want '$3')"
    FAILED=1
  fi
}

echo "== syntax =="
check "bootstrap.sh"     bash -n scripts/bootstrap.sh
check "uninstall.sh"     bash -n scripts/uninstall.sh
check "install.sh"       bash -n scripts/install.sh
check "install.sh is executable" test -x scripts/install.sh

# Everything below runs the installer's own helpers by sourcing it in library
# mode, so the tests exercise real code instead of grepping for strings.
export CS2A_BOOTSTRAP_LIB=1
# shellcheck source=../scripts/bootstrap.sh
. scripts/bootstrap.sh
unset CS2A_BOOTSTRAP_LIB

echo
echo "== helpers =="
# ensure_dirs must create every argument (it once created only the first)
TD=$(mktemp -d)
ensure_dirs "$TD/bin" "$TD/etc" "$TD/var" "$TD/cache/plugins"
for d in bin etc var cache/plugins; do
  check "ensure_dirs creates $d" test -d "$TD/$d"
done
ensure_dirs "$TD/bin"   # second call must not fail
check "ensure_dirs is idempotent" test -d "$TD/bin"

checkeq "json_str escapes quotes"     "$(json_str 'a"b')"      '"a\"b"'
checkeq "json_str escapes backslash"  "$(json_str 'a\b')"      '"a\\b"'
checkeq "json_str escapes newline"    "$(json_str $'a\nb')"    '"a\nb"'
checkeq "json_str keeps DSN untouched" "$(json_str 'u:p@tcp(127.0.0.1:3306)/db')" '"u:p@tcp(127.0.0.1:3306)/db"'

echo
echo "== generated agent.json is valid JSON =="
# This is the regression that made a fresh install unusable: the old heredoc
# emitted a stray '$' and never closed the object when wp_dsn was set.
gen_agent_json() { # gen_agent_json <wp_dsn>
  local dsn=$1 out
  out="{
  \"listen\": \"127.0.0.1:8100\",
  \"token\": $(json_str 'tok"en\x'),
  \"cs2_dir\": $(json_str /opt/cs2a/cs2),
  \"service_name\": $(json_str cs2-server),
  \"rcon_addr\": \"127.0.0.1:27015\",
  \"rcon_password\": $(json_str 'p@ss"word'),
  \"a2s_addr\": \"127.0.0.1:27015\",
  \"db_path\": $(json_str /opt/cs2a/var/agent.db),
  \"plugin_cache\": $(json_str /opt/cs2a/cache/plugins)"
  [[ -n $dsn ]] && out+=",
  \"wp_dsn\": $(json_str "$dsn")"
  out+="
}
"
  printf '%s' "$out"
}
if command -v python3 >/dev/null 2>&1; then
  for dsn in "" 'cs2a:pw@tcp(127.0.0.1:3306)/cs2_wp'; do
    label="without wp_dsn"; [[ -n $dsn ]] && label="with wp_dsn"
    if gen_agent_json "$dsn" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d["token"]=="tok\"en\\x" and d["rcon_password"]=="p@ss\"word" else 1)'; then
      echo "  ok  agent.json parses $label"
    else
      echo "FAIL  agent.json parses $label"
      FAILED=1
    fi
  done
else
  echo "  --  python3 missing, skipping JSON validation"
fi

echo
echo "== auto discovery =="
FAKE=$(mktemp -d)
mkdir -p "$FAKE/cs2/game/csgo/cfg" "$FAKE/units"
echo 'Game csgo' > "$FAKE/cs2/game/csgo/gameinfo.gi"

check "is_cs2_dir accepts an install"  is_cs2_dir "$FAKE/cs2"
checknot "is_cs2_dir rejects a plain dir" is_cs2_dir "$FAKE"

# a unit written by cs2a itself
cat > "$FAKE/units/cs2-server.service" <<UNIT
[Service]
WorkingDirectory=$FAKE/cs2/game
ExecStart=$FAKE/cs2/game/cs2.sh -dedicated -console -usercon -ip 0.0.0.0 -port 27015 -maxplayers 12 +map de_dust2 +exec server.cfg
UNIT
DETECTED_UNIT=""; DETECTED_CS2_DIR=""; DETECTED_PORT=""; DETECTED_GSLT=""; DETECTED_IP=""; DETECTED_USERCON=0
CS2A_UNIT_DIRS="$FAKE/units" detect_unit
checkeq "detects unit name"     "$DETECTED_UNIT"    "cs2-server"
checkeq "detects install dir"   "$DETECTED_CS2_DIR" "$FAKE/cs2"
checkeq "detects game port"     "$DETECTED_PORT"    "27015"
checkeq "detects wildcard bind" "$DETECTED_IP"      "0.0.0.0"
checkeq "detects -usercon"      "$DETECTED_USERCON" "1"

# a unit binding a public address: the agent must NOT assume loopback, which is
# what made every RCON action fail with "connection refused"
rm -f "$FAKE/units/cs2-server.service"
cat > "$FAKE/units/pub.service" <<UNIT
[Service]
WorkingDirectory=$FAKE/cs2/game
ExecStart=$FAKE/cs2/game/cs2.sh -dedicated -console -ip 203.0.113.7 -port 27016 +map de_dust2
UNIT
DETECTED_UNIT=""; DETECTED_CS2_DIR=""; DETECTED_PORT=""; DETECTED_GSLT=""; DETECTED_IP=""; DETECTED_USERCON=0
CS2A_UNIT_DIRS="$FAKE/units" detect_unit
checkeq "detects explicit bind"    "$DETECTED_IP"      "203.0.113.7"
checkeq "detects port next to it"  "$DETECTED_PORT"    "27016"
checkeq "notices missing usercon"  "$DETECTED_USERCON" "0"
rm -f "$FAKE/units/pub.service"

# a hand-rolled unit: legacy +port, steamcmd binary path, GSLT on the command line
cat > "$FAKE/units/mycs2.service" <<UNIT
[Service]
WorkingDirectory=$FAKE/cs2/game/bin/linuxsteamrt64
ExecStart=$FAKE/cs2/game/bin/linuxsteamrt64/cs2 -dedicated -usercon +port 27045 +sv_setsteamaccount GSLTVALUE +map de_inferno
UNIT
DETECTED_UNIT=""; DETECTED_CS2_DIR=""; DETECTED_PORT=""; DETECTED_GSLT=""; DETECTED_IP=""; DETECTED_USERCON=0
CS2A_UNIT_DIRS="$FAKE/units" detect_unit
checkeq "detects foreign unit"       "$DETECTED_UNIT"    "mycs2"
checkeq "unwraps linuxsteamrt64"     "$DETECTED_CS2_DIR" "$FAKE/cs2"
checkeq "detects legacy +port"       "$DETECTED_PORT"    "27045"
checkeq "detects GSLT from launch"   "$DETECTED_GSLT"    "GSLTVALUE"
checkeq "no -ip means all ifaces"    "$DETECTED_IP"      ""

# non-CS2 units must be ignored entirely
rm -f "$FAKE/units"/*.service
cat > "$FAKE/units/nginx.service" <<UNIT
[Service]
ExecStart=/usr/sbin/nginx -g daemon off;
UNIT
DETECTED_UNIT=""; DETECTED_PORT=""
CS2A_UNIT_DIRS="$FAKE/units" detect_unit
checkeq "ignores unrelated units" "$DETECTED_UNIT" ""

# server.cfg is the source of truth for rcon_password — never ask the user
printf 'hostname "my server"\n// comment\nrcon_password   "s3cret pass"\nsv_setsteamaccount "TOK123"\n' \
  > "$FAKE/cs2/game/csgo/cfg/server.cfg"
DETECTED_CS2_DIR="$FAKE/cs2"; DETECTED_RCON=""; DETECTED_GSLT=""
detect_server_cfg
checkeq "reads rcon_password from server.cfg" "$DETECTED_RCON" "s3cret pass"
checkeq "reads GSLT from server.cfg"          "$DETECTED_GSLT" "TOK123"

# a rerun must reuse the token and admin password instead of rotating them
mkdir -p "$FAKE/root/etc"
printf '{\n  "token": "reusedtoken",\n  "rcon_password": "reusedrcon",\n  "wp_dsn": "u:p@tcp(127.0.0.1:3306)/cs2_wp"\n}\n' > "$FAKE/root/etc/agent.json"
printf '{\n  "public_url": "https://panel.example.com"\n}\n' > "$FAKE/root/etc/panel.json"
printf 'CS2A_ADMIN_USER=bob\nCS2A_ADMIN_PASSWORD=bobspass\n' > "$FAKE/root/etc/panel.env"
CS2A_ROOT="$FAKE/root"; AGENT_TOKEN=""; RCON_PASS=""; WP_DSN=""; SETUP_PASS=""; SETUP_ADMIN=""; PANEL_DOMAIN=""
reuse_secrets
checkeq "reuses agent token"    "$AGENT_TOKEN"   "reusedtoken"
checkeq "reuses rcon password"  "$RCON_PASS"     "reusedrcon"
checkeq "reuses wp_dsn"         "$WP_DSN"        "u:p@tcp(127.0.0.1:3306)/cs2_wp"
checkeq "reuses admin user"     "$SETUP_ADMIN"   "bob"
checkeq "reuses admin password" "$SETUP_PASS"    "bobspass"
checkeq "recovers panel domain" "$PANEL_DOMAIN"  "panel.example.com"

checkeq "json_field on a missing key" "$(json_field "$FAKE/root/etc/panel.json" nope)" ""

# A password containing " or \ must survive the JSON round trip, or a rerun
# reuses a mangled value and the agent authenticates with a password the server
# has never seen.
printf '{\n  "token": %s,\n  "rcon_password": %s\n}\n' "$(json_str 'tok"en')" "$(json_str 'p@ss"wo\rd')" \
  > "$FAKE/root/etc/escaped.json"
if command -v python3 >/dev/null 2>&1; then
  checkeq "json_field un-escapes quotes"    "$(json_field "$FAKE/root/etc/escaped.json" token)"         'tok"en'
  checkeq "json_field un-escapes backslash" "$(json_field "$FAKE/root/etc/escaped.json" rcon_password)" 'p@ss"wo\rd'
else
  # without a parser the value must be refused, never silently corrupted
  checknot "json_field refuses escaped values" json_field "$FAKE/root/etc/escaped.json" rcon_password
fi

echo
echo "== cfg parsing (one parser for 'is it set' and 'what is it') =="
CFGF="$FAKE/cs2/game/csgo/cfg/parse.cfg"
printf 'rcon_password "pw with spaces"   // do not change\n' > "$CFGF"
checkeq "value survives a trailing comment" "$(cfg_value "$CFGF" rcon_password)" "pw with spaces"
printf "rcon_password 'single'\n" > "$CFGF"
checkeq "single quotes are stripped" "$(cfg_value "$CFGF" rcon_password)" "single"
printf 'rcon_password bare123\n' > "$CFGF"
checkeq "bare values work" "$(cfg_value "$CFGF" rcon_password)" "bare123"
printf 'rcon_password ""\n' > "$CFGF"
check "empty value is reported as present" cfg_has_key "$CFGF" rcon_password
if cfg_value "$CFGF" rcon_password >/dev/null 2>&1; then
  echo "FAIL  an empty password must not parse as a value"; FAILED=1
else
  echo "  ok  an empty password must not parse as a value"
fi
printf 'hostname "x"\n' > "$CFGF"
checknot "absent key is absent" cfg_has_key "$CFGF" rcon_password

# "set but unreadable" must never be mistaken for "not set": generating a new
# password in that case wrote it only into agent.json.
printf 'rcon_password ""\n' > "$FAKE/cs2/game/csgo/cfg/server.cfg"
DETECTED_CS2_DIR="$FAKE/cs2"; DETECTED_RCON=""; DETECTED_RCON_UNREADABLE=0; DETECTED_GSLT=""; DETECTED_PORT=""
detect_server_cfg
checkeq "unreadable rcon_password is flagged" "$DETECTED_RCON_UNREADABLE" "1"
checkeq "unreadable rcon_password is not a value" "$DETECTED_RCON" ""

echo
echo "== install root discovery =="
# An install literally named cs2 must not lose its last path component: suffix
# stripping turned /home/cs2server/cs2 into /home/cs2server, so cs2a "adopted"
# a unit and then installed a second copy of the game somewhere else.
mkdir -p "$FAKE/home/cs2server/cs2/game/csgo"
echo 'Game csgo' > "$FAKE/home/cs2server/cs2/game/csgo/gameinfo.gi"
checkeq "root named cs2 survives"   "$(cs2_root_of "$FAKE/home/cs2server/cs2")"          "$FAKE/home/cs2server/cs2"
checkeq "walks up from game/"       "$(cs2_root_of "$FAKE/home/cs2server/cs2/game")"     "$FAKE/home/cs2server/cs2"
checkeq "walks up from a binary"    "$(cs2_root_of "$FAKE/home/cs2server/cs2/game/cs2.sh")" "$FAKE/home/cs2server/cs2"
# LinuxGSM keeps the game under serverfiles/ next to the wrapper
mkdir -p "$FAKE/home/lgsm/serverfiles/game/csgo"
echo 'Game csgo' > "$FAKE/home/lgsm/serverfiles/game/csgo/gameinfo.gi"
checkeq "finds serverfiles/"        "$(cs2_root_of "$FAKE/home/lgsm")"                   "$FAKE/home/lgsm/serverfiles"
checknot "unrelated dir is not a root" cs2_root_of "$FAKE/home"

# a unit whose WorkingDirectory ends in /cs2 must still be adopted correctly
mkdir -p "$FAKE/units2"
cat > "$FAKE/units2/cs2server.service" <<UNIT
[Service]
User=cs2server
WorkingDirectory=$FAKE/home/cs2server/cs2
ExecStart=$FAKE/home/cs2server/cs2/game/cs2.sh -dedicated -usercon -port 27015
UNIT
DETECTED_UNIT=""; DETECTED_CS2_DIR=""; DETECTED_PORT=""; DETECTED_GSLT=""; DETECTED_IP=""
DETECTED_USERCON=0; DETECTED_USER=""; DETECTED_UNIT_FILE=""
CS2A_UNIT_DIRS="$FAKE/units2" detect_unit
checkeq "adopts an install named cs2" "$DETECTED_CS2_DIR" "$FAKE/home/cs2server/cs2"
checkeq "reads the unit's User"       "$DETECTED_USER"    "cs2server"
checkeq "records the fragment path"   "$DETECTED_UNIT_FILE" "$FAKE/units2/cs2server.service"

echo
echo "== launch line parsing =="
# A backslash-continued ExecStart used to yield no port at all, and cs2a then
# wrote 27015 into rcon_addr — a refused connection on a server using 27045.
cat > "$FAKE/units2/wrapped.service" <<'UNIT'
[Service]
ExecStart=/srv/cs2/game/cs2.sh -dedicated \
    -usercon \
    -port 27045
UNIT
checkeq "joins continued ExecStart" \
  "$(launch_flag "$(unit_exec_line "" "$FAKE/units2/wrapped.service")" port)" "27045"
# systemd's ignore-failure prefix is not part of the command
cat > "$FAKE/units2/prefixed.service" <<'UNIT'
[Service]
ExecStart=-/srv/cs2/game/cs2.sh -dedicated -port 27060
UNIT
checkeq "strips the '-' ExecStart prefix" \
  "$(unit_exec_line "" "$FAKE/units2/prefixed.service")" "/srv/cs2/game/cs2.sh -dedicated -port 27060"

# -usercon must be a real argument: a commented-out or Environment= copy used to
# satisfy the check and hide the one setting RCON depends on.
rm -f "$FAKE/units2"/*.service
cat > "$FAKE/units2/fake-usercon.service" <<UNIT
[Service]
Environment=EXTRA=-usercon
# ExecStart=$FAKE/cs2/game/cs2.sh -usercon
ExecStart=$FAKE/cs2/game/cs2.sh -dedicated -port 27015
UNIT
DETECTED_UNIT=""; DETECTED_CS2_DIR=""; DETECTED_PORT=""; DETECTED_GSLT=""; DETECTED_IP=""
DETECTED_USERCON=0; DETECTED_USER=""; DETECTED_UNIT_FILE=""
CS2A_UNIT_DIRS="$FAKE/units2" detect_unit
checkeq "commented -usercon does not count" "$DETECTED_USERCON" "0"

# template units cannot be started by name, so they must not be adopted
rm -f "$FAKE/units2"/*.service
cat > "$FAKE/units2/cs2@.service" <<UNIT
[Service]
ExecStart=$FAKE/cs2/game/cs2.sh -dedicated -port %i
UNIT
DETECTED_UNIT=""; DETECTED_CS2_DIR=""
CS2A_UNIT_DIRS="$FAKE/units2" detect_unit
checkeq "template units are skipped" "$DETECTED_UNIT" ""

# more than one CS2 unit must be reported rather than picked silently
rm -f "$FAKE/units2"/*.service
for n in alpha beta; do
  cat > "$FAKE/units2/$n.service" <<UNIT
[Service]
WorkingDirectory=$FAKE/cs2/game
ExecStart=$FAKE/cs2/game/cs2.sh -dedicated -usercon -port 27015
UNIT
done
DETECTED_UNIT=""; DETECTED_CS2_DIR=""; WARNINGS=()
CS2A_UNIT_DIRS="$FAKE/units2" detect_unit
checkeq "first of several units wins" "$DETECTED_UNIT" "alpha"
if [[ ${#WARNINGS[@]} -gt 0 && ${WARNINGS[0]} == *"found 2 CS2 units"* ]]; then
  echo "  ok  several CS2 units are reported"
else
  echo "FAIL  several CS2 units are reported (warnings: ${WARNINGS[*]:-none})"
  FAILED=1
fi
WARNINGS=()

echo
echo "== option parsing =="
# "bootstrap.sh --domain" used to consume the last argument and then die on the
# trailing shift under errexit: exit 1, no message at all.
OUT=$(bash scripts/bootstrap.sh --domain 2>&1 || true)
if [[ $OUT == *"--domain needs a value"* ]]; then
  echo "  ok  a missing option value is explained"
else
  echo "FAIL  a missing option value is explained (got: ${OUT:-<empty>})"
  FAILED=1
fi
for flag in --root --panel-port --game-port; do
  OUT=$(bash scripts/bootstrap.sh "$flag" 2>&1 || true)
  [[ $OUT == *"$flag needs a value"* ]] && echo "  ok  $flag needs a value" || {
    echo "FAIL  $flag needs a value (got: ${OUT:-<empty>})"; FAILED=1; }
done
OUT=$(bash scripts/bootstrap.sh --help 2>&1 || true)
[[ $OUT == *"--panel-local"* ]] && echo "  ok  --help lists --panel-local" || {
  echo "FAIL  --help lists --panel-local"; FAILED=1; }

rm -rf "$FAKE" "$TD"

echo
echo "== install correctness =="
check "game unit enables rcon"      grep -q -- "-usercon" scripts/bootstrap.sh
check "game unit binds all ifaces"  grep -q -- "-ip 0.0.0.0" scripts/bootstrap.sh
check "agent stays on loopback"     grep -q '\\"listen\\": \\"127.0.0.1:\$CS2A_AGENT_PORT\\"' scripts/bootstrap.sh
check "panel env holds the secret"  grep -q "panel.env" scripts/bootstrap.sh
check "steamcmd app id is used"     grep -q 'app_update "\$CS2A_APP_ID"' scripts/bootstrap.sh
check "steamclient.so is linked"    grep -q "sdk64" scripts/bootstrap.sh
check "steamcmd runs unprivileged"  grep -q 'sudo -u "\$CS2A_STEAM_USER" "\$STEAMCMD_BIN"' scripts/bootstrap.sh
check "steamcmd failure is fatal"   grep -q 'STEAM_RC=\${PIPESTATUS\[0\]}' scripts/bootstrap.sh
check "warns on missing usercon"    grep -q 'no -usercon' scripts/bootstrap.sh
check "offers the usercon drop-in"  grep -q '10-cs2a-usercon.conf' scripts/bootstrap.sh
check "replays warnings in summary" grep -q 'Worth your attention' scripts/bootstrap.sh
check "agent dials the real bind"   grep -q 'CS2A_RCON_HOST:\$CS2A_GAME_PORT' scripts/bootstrap.sh
check "warns when cfg needs restart" grep -q 'running with the old config' scripts/bootstrap.sh
check "server.cfg is not world-readable" grep -q 'umask 027' scripts/bootstrap.sh
check "release asset follows uname" grep -q 'cs2a-linux-\$ASSET_ARCH.tar.gz' scripts/bootstrap.sh
check "release checksum verified"   grep -q 'checksum mismatch' scripts/bootstrap.sh
check "tar drops archive ownership" grep -q -- '--no-same-owner' scripts/bootstrap.sh
check "release ships both arches"   grep -q 'cs2a-linux-arm64.tar.gz.sha256' .github/workflows/release.yml
check "public_url set behind caddy" grep -q '\\"public_url\\"' scripts/bootstrap.sh
check "caddy raises proxy timeouts" grep -q "response_header_timeout 10m" scripts/bootstrap.sh
check "caddy validated before use"  grep -q "caddy validate" scripts/bootstrap.sh
check "existing Caddyfile backed up" grep -q "Caddyfile.cs2a-backup" scripts/bootstrap.sh
check "caddy site lives in its own file" grep -q '/etc/caddy/cs2a.caddyfile' scripts/bootstrap.sh
check "firewall opens game udp"     grep -q 'ufw_allow "\$CS2A_GAME_PORT/udp"' scripts/bootstrap.sh
check "firewall opens panel port"   grep -q 'ufw_allow "\$CS2A_PANEL_PORT/tcp"' scripts/bootstrap.sh
check "raw port closes only once caddy serves" \
                                    grep -q 'CADDY_SERVING -eq 1' scripts/bootstrap.sh
check "config files are 0600"       grep -q "chmod 600" scripts/bootstrap.sh
check "configs checked by the binaries" grep -q -- '-check 2>&1' scripts/bootstrap.sh
check "verifies agent with its token" grep -q 'api/v1/status' scripts/bootstrap.sh
check "reports why rcon is unusable" grep -q 'RCON is not usable yet' scripts/bootstrap.sh
check "checks the game unit loads"  grep -q 'LoadState' scripts/bootstrap.sh
check "verifies panel responds"     grep -q "login" scripts/bootstrap.sh
check "summary survives a failed start" grep -q 'START_FAILED' scripts/bootstrap.sh
check "uninstall documents purge"    grep -q "purge-game" scripts/uninstall.sh
check "cs2a marks its own unit"      grep -q "managed by cs2a" scripts/bootstrap.sh
check "uninstall keeps foreign unit" grep -q "managed by cs2a" scripts/uninstall.sh
check "uninstall removes its dropin" grep -q '10-cs2a-usercon.conf' scripts/uninstall.sh
check "uninstall cleans up caddy"    grep -q 'cs2a.caddyfile' scripts/uninstall.sh
check "uninstall confirms first"     grep -q "Continue?" scripts/uninstall.sh
check "uninstall prunes empty root"  grep -q "rmdir" scripts/uninstall.sh
check "wrapper re-attaches tty"     grep -q '/dev/tty' scripts/install.sh
check "wrapper pins version"        grep -q 'CS2A_VERSION' scripts/install.sh
check "wrapper unattended fallback" grep -q -- '--unattended' scripts/install.sh
check "pages workflow ships site"   grep -q 'deploy-pages' .github/workflows/pages.yml
check "landing page has copy btn"   grep -q 'copy' site/index.html

echo
echo "== go-side plan package =="
( . scripts/devenv.sh && go test ./internal/bootstrap/ -count=1 ) || FAILED=1

echo
if [[ $FAILED -eq 0 ]]; then echo "ALL SHELL TESTS PASSED"; else echo "SHELL TESTS FAILED"; exit 1; fi
