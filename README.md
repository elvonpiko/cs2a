# cs2a

> A super minimal, lightweight Counter-Strike 2 dedicated server manager.
> One agent on the VPS, one panel for admins & players. SQLite. Single binary per component.

```
                      ┌──────────────────────────────────────────┐
                      │                 cs2a agent               │
  browser ──HTTP───▶  │  control plane (SSR UI, htmx, sessions)  │
                      │        │  loopback / token auth          │
                      │        ▼                                 │
                      │  agent runtime ──RCON──▶ CS2 (srcds)     │
                      │       │  └─A2S query──▶ 27015/udp        │
                      │       └─ files ─▶ server.cfg, plugins,   │
                      │                  whitelist, systemd      │
                      └──────────────────────────────────────────┘
```

Two components, one host:

| Component     | What it is                                                                 | Listens on |
|---------------|-----------------------------------------------------------------------------|------------|
| `cs2a-agent`  | Runs next to the CS2 server. Executes the real work: RCON commands, config edits, plugin install/remove, map changes, whitelist, service control. Talks to the game via **RCON** (TCP 27015) and **A2S** queries (UDP 27015). Exposes a small authenticated JSON API. | `127.0.0.1:8100` (loopback only) |
| `cs2a-panel`  | The web UI (server page, plugins, loadout, access control, users). Server-rendered with `templ` + htmx. Calls the agent's API with the agent token. | `:8080` |

- **Panel → agent**: JSON over HTTP, bearer token, loopback by default.
- **Agent → game**: RCON for commands (`changelevel`, cvars, `status`), A2S for live player counts, direct file edits for `server.cfg` (managed block), plugin configs, whitelists.
- **Panel → users**: SSR pages + htmx partials (status card auto-refreshes every 5s).
- **Storage**: one SQLite database per component (panel: users/sessions/audit; agent: plugin state).

## Features

**Admins**
- Server page: live status (map, online players, uptime), start/stop/restart with confirmation dialogs, map changes
- Plugins: one-click install/uninstall of Metamod:Source, CounterStrikeSharp, WeaponPaints, cs2whitelist (with dependency resolution), plus a JSON config editor per plugin
- Access: server password (`sv_password`), whitelist management (raw SteamIDs in any format, normalized to SteamID64), link panel users to whitelisted SteamIDs
- Users: create admin/player accounts

**Players**
- Server info (read-only)
- Loadout: knife per team (synced to the WeaponPaints plugin when its MySQL is configured)

## Repository layout

```
cmd/cs2a-agent       agent entrypoint
cmd/cs2a-panel       panel entrypoint
internal/agent       agent HTTP API + runtime services (systemd, RCON, plugins)
internal/panel       panel HTTP server, sessions, agent client
internal/panel/web   templ templates + static assets (css, htmx, app.js)
internal/rcon        minimal Source RCON client
internal/a2s         minimal A2S_INFO / A2S_PLAYER query client
internal/cs2         SteamID normalization, status parsing, server.cfg managed blocks
internal/bootstrap   typed install plan + secret generation (used by the installer)
scripts/             bootstrap.sh installer, uninstall.sh, devenv.sh
tests/               shell tests for the installer
docs/                plan + plugin ecosystem notes
```

## Development

```sh
scripts/devenv.sh      # source me: sets up writable Go caches
go tool templ generate # after editing internal/panel/web/**/*.templ
make test              # go test ./...
make build             # dist/cs2a-agent + dist/cs2a-panel
```

## Install

On a fresh Ubuntu/Debian VPS as root — one command:

```sh
curl -fsSL https://elvonpiko.github.io/cs2a/install.sh | sudo bash
```

Works straight after the first push too, straight from the repo (no Pages needed):

```sh
curl -fsSL https://raw.githubusercontent.com/elvonpiko/cs2a/main/scripts/bootstrap.sh -o bootstrap.sh
sudo bash bootstrap.sh
```

The `install.sh` wrapper re-attaches your terminal (so the interactive TUI works
under `curl | bash`), installs from the latest release tag by default, and
supports pinning for repeatable setups:

```sh
curl -fsSL https://elvonpiko.github.io/cs2a/install.sh | sudo CS2A_VERSION=v0.1.0 bash
```

The landing page + wrapper + installer are published to GitHub Pages
automatically by `.github/workflows/pages.yml` on every `v*` tag — no domain
required (the `elvonpiko.github.io` subdomain is HTTPS out of the box; a custom
domain is optional later).

The interactive installer sets up SteamCMD + the CS2 server (~40GB), writes
systemd units (`cs2-server`, `cs2a-agent`, `cs2a-panel`), opens the firewall
for the game port and panel, and prints the generated admin credentials and
panel URL. It is idempotent — rerunning it is safe. `sudo bash bootstrap.sh
--no-cs2` installs only the manager and points it at an existing CS2 install.

## Status

Working MVP — see `docs/PLAN.md` for the roadmap and `docs/PLUGINS.md` for
how plugin support works under the hood.
