# cs2a

> A super minimal, lightweight Counter-Strike 2 server manager.
> Web panel for admins & players + a small agent on the VPS. Two Go binaries, SQLite, no containers.

## Install

Fresh Ubuntu/Debian VPS, as root:

```sh
curl -fsSL https://elvonpiko.github.io/cs2a/install.sh | sudo bash
```

That drops you into an interactive TUI (CasaOS-style): it installs SteamCMD + the CS2 server (~40 GB), writes the systemd units, opens the firewall, and prints the panel URL + generated admin credentials. Reruns are safe.

Already got CS2 running? Skip that part and keep your unit file untouched:

```sh
sudo bash bootstrap.sh --no-cs2
```

It auto-detects your install dir, systemd unit, game port and even the RCON password from `server.cfg`. Optional: enter a domain during setup and Caddy gives the panel automatic HTTPS — otherwise it serves on `:8080` directly.

Uninstall: `sudo scripts/uninstall.sh`.

## What you get

**Admins**
- Live server page — status, players, uptime, map preview; start/stop/restart with styled confirm dialogs
- Map changes that **keep everyone connected** (`changelevel` — players ride along; only a restart drops them)
- One-click plugins: Metamod:Source → CounterStrikeSharp → cs2-WeaponPaints / whitelist, with a JSON config editor per plugin (WeaponPaints ships preconfigured, all cosmetics enabled)
- Server password (`sv_password`) and SteamID whitelist, applied live — no restart
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

The agent never listens on a public interface. The panel talks to it over loopback with a generated token.

## Development

```sh
scripts/devenv.sh       # source me: writable Go caches
go tool templ generate  # after editing internal/panel/web/**/*.templ
make test               # go test ./...
make build              # dist/cs2a-agent + dist/cs2a-panel
```

```
cmd/               panel + agent entrypoints
internal/agent     runtime: RCON, A2S, systemd, plugins, cosmetics
internal/panel     HTTP server, sessions, agent client
internal/cs2       SteamIDs, status parsing, server.cfg managed blocks
internal/bootstrap install plan + secret generation
scripts/           bootstrap.sh installer, install.sh wrapper, uninstall.sh
tests/             shell tests for the installer
```

## Status

Working MVP, a weekend-hobby project — use at your own risk. See `docs/PLUGINS.md` for how the plugin stack works under the hood.
