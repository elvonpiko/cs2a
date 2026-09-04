#!/usr/bin/env bash
# cs2a installer entrypoint — the `curl | bash` target.
#
#   curl -fsSL https://elvonpiko.github.io/cs2a/install.sh | sudo bash
#
# Why a wrapper: `curl | bash` makes stdin the pipe, so any interactive prompt
# inside the real installer would read the pipe (and hit EOF) instead of your
# keyboard. This wrapper therefore downloads the installer to a temp file and
# re-executes it with the TTY re-attached — the same approach CasaOS, rustup
# and Homebrew use. You also get version pinning and clean arg passthrough.
#
# Pin a version (recommended for automation):
#   curl -fsSL https://elvonpiko.github.io/cs2a/install.sh | sudo CS2A_VERSION=v0.1.0 bash
#
# Env overrides:
#   CS2A_VERSION   tag/branch/commit to install from   (default: latest tag, fallback main)
#   CS2A_RAW_BASE  mirror base URL                     (default: raw.githubusercontent.com/elvonpiko/cs2a)
set -euo pipefail

CS2A_VERSION="${CS2A_VERSION:-}"
CS2A_RAW_BASE="${CS2A_RAW_BASE:-https://raw.githubusercontent.com/elvonpiko/cs2a}"

c_info() { printf '\033[38;5;209m==>\033[0m \033[1m%s\033[0m\n' "$1"; }
c_die()  { printf '\033[31merror:\033[0m %s\n' "$1" >&2; exit 1; }

# --- sanity ------------------------------------------------------------------
[[ $(uname -s) == "Linux" ]] || c_die "cs2a installs on Linux only (this is $(uname -s))."
[[ $EUID -eq 0 ]] || c_die "run as root — try: curl -fsSL <url> | sudo bash"
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || c_die "need curl or wget to download the installer."

ARCH=$(uname -m)
case $ARCH in
  x86_64|aarch64|armv7l) ;;
  *) c_info "note: untested architecture '$ARCH' — continuing anyway." ;;
esac

# --- resolve version ---------------------------------------------------------
fetch() { # fetch <url> -> stdout
  if command -v curl >/dev/null 2>&1; then curl -fsSL "$1"
  else wget -qO- "$1"; fi
}

if [[ -z $CS2A_VERSION ]]; then
  # latest release tag; fall back to main for first-install-before-first-tag
  LATEST=$(fetch "https://api.github.com/repos/elvonpiko/cs2a/releases/latest" 2>/dev/null |
    grep -oE '"tag_name":\s*"[^"]+"' | head -1 | cut -d'"' -f4 || true)
  CS2A_VERSION="${LATEST:-main}"
fi
c_info "installing cs2a from ${CS2A_VERSION}"

# --- download + re-exec with TTY ---------------------------------------------
TMP=$(mktemp /tmp/cs2a-bootstrap.XXXXXX.sh)
trap 'rm -f "$TMP"' EXIT

fetch "$CS2A_RAW_BASE/$CS2A_VERSION/scripts/bootstrap.sh" > "$TMP" ||
  c_die "could not download bootstrap.sh from $CS2A_RAW_BASE/$CS2A_VERSION (wrong version tag? repo private?)"
[[ -s $TMP ]] || c_die "downloaded installer is empty."
grep -q "cs2a" "$TMP" || c_die "downloaded file does not look like the cs2a installer."

# hand stdin back to the terminal so the interactive TUI works under `curl | bash`
if [[ -t 0 ]]; then
  exec bash "$TMP" "$@"
elif [[ -e /dev/tty ]]; then
  c_info "re-attaching your terminal for the interactive setup…"
  exec bash "$TMP" "$@" < /dev/tty
else
  printf '\033[33mwarning:\033[0m no TTY available — running with --unattended defaults.\n' >&2
  exec bash "$TMP" --unattended "$@"
fi
