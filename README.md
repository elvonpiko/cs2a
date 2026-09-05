# cs2a

> A minimal Counter-Strike 2 server manager.
> Web panel for admins and players, plus a small agent on the VPS. Two Go binaries, SQLite, no containers.

## Install

Fresh Ubuntu/Debian VPS, as root:

```sh
curl -fsSL https://elvonpiko.github.io/cs2a/install.sh | sudo bash
```

The installer discovers before it asks. It looks for SteamCMD, an existing CS2 install, its systemd unit, the game port, the RCON password in `server.cfg`, Caddy and ufw — then installs and configures only what is missing. On a bare VPS that means SteamCMD, the CS2 server (~40 GB), the systemd units, firewall rules and the panel; on a machine that already runs CS2 it means the panel and agent alone, with your unit file left untouched.

Reruns are safe: the agent token, admin password and RCON password are reused rather than rotated.

```sh
sudo bash scripts/bootstrap.sh --no-cs2            # never install the game
sudo bash scripts/bootstrap.sh --domain cs.example # panel over HTTPS via Caddy
sudo bash scripts/bootstrap.sh --unattended        # no questions, all defaults
```

You are asked at most a handful of questions, and never for something the machine can answer itself. Give it a domain and Caddy provides automatic HTTPS with proxy timeouts long enough for plugin installs; without one the panel serves on `:8080`.

Uninstall: `sudo bash scripts/uninstall.sh` (add `--purge` for config and data, `--purge-game` for the CS2 install). A unit cs2a did not write is never removed.

## What you get

**Admins**
- Live server page — status, players, uptime, map preview; start/stop/restart with confirm dialogs
- Map changes that **keep everyone connected** (`changelevel`; only a restart drops players)
- One-click plugin catalog — Metamod:Source, CounterStrikeSharp, WeaponPaints, MatchZy, retakes, deathmatch, admin tools — always resolving the current upstream release, with a JSON config editor per plugin
- Installs run as background jobs with live progress, so a 50 MB download cannot time out the request
- Server password and SteamID whitelist, applied live — no restart
- Panel accounts (admin/player) with an audit trail

**Players**
- Server info, read-only
- Map change (the one action they get)
- Loadout: knife, gloves and agent per side, picked from image galleries and synced to the server per SteamID — like the official in-game loadout

## How it fits together

```
browser → cs2a-panel :8080 (SSR + htmx, sessions, SQLite)
            └─ loopback :8100, bearer token → cs2a-agent
                                                 ├─ RCON + A2S → CS2 server
                                                 └─ configs, plugins, whitelist, systemd
```

The agent never listens on a public interface — it refuses a non-loopback bind unless explicitly overridden — and the panel talks to it over loopback with a generated token compared in constant time. Panel forms are protected by Go's `CrossOriginProtection`; set a domain and cookies become `Secure` automatically.

## Development

```sh
scripts/devenv.sh       # source me: writable Go caches
go tool templ generate  # after editing internal/panel/web/**/*.templ
make test               # go test ./...
make build              # dist/cs2a-agent + dist/cs2a-panel
bash tests/bootstrap_test.sh
```

```
cmd/               panel + agent entrypoints
internal/agent     runtime: RCON, A2S, systemd, plugins, jobs, cosmetics
internal/panel     HTTP server, sessions, agent client, templ views
internal/cs2       SteamIDs, status parsing, server.cfg managed blocks
internal/bootstrap install plan, unit/config rendering, secret generation
scripts/           bootstrap.sh installer, install.sh wrapper, uninstall.sh
tests/             shell tests for the installer
```

## Status

Working MVP, a weekend-hobby project — use at your own risk. Both services currently run as root; see `docs/PLAN.md` for planned hardening and `docs/PLUGINS.md` for how the plugin stack works under the hood.
