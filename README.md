# cs2a

> A super minimal, lightweight Counter-Strike 2 dedicated server manager.
> One agent on the VPS, one panel for admins & players. SQLite. Single binary per component.

## Architecture

```
                      ┌──────────────────────────────────────────┐
                      │                 cs2a agent               │
  browser ──HTTPS──▶  │  control plane (SSR UI, htmx, sessions)  │
                      │        │  loopback / token auth          │
                      │        ▼                                 │
                      │  agent runtime ──RCON──▶ CS2 (srcds)     │
                      │       │  └─A2S query──▶ 27015/udp        │
                      │       └─ files ─▶ server.cfg, plugins,   │
                      │                  steamclient / systemd   │
                      └──────────────────────────────────────────┘
```

Two components, one host:

| Component     | What it is                                                                 | Listens on |
|---------------|-----------------------------------------------------------------------------|------------|
| `cs2a-agent`  | Runs next to the CS2 server. Executes the real work: RCON commands, config edits, plugin install/remove, map changes, whitelist, service control. Talks to the game via **RCON** (TCP 27015) and **A2S** queries (UDP 27015). Exposes a small authenticated JSON API on **loopback**. | `127.0.0.1:8100` |
| `cs2a-panel`  | The web UI (server page, plugins, loadout, whitelist, login). Server-rendered with `templ` + htmx. Calls the agent's API. | `:8080` (web) |

- **Control plane → agent**: JSON over HTTP, agent token, loopback by default.
- **Agent → game**: RCON for commands (`changelevel`, `sv_setsteamaccount`, cvars), A2S for live status, direct file edits for `server.cfg`, plugin configs, whitelists.
- **Panel → users**: SSR pages + htmx partials; SSE for live server state (map, players, uptime, restart progress).
- **Storage**: one SQLite database per component (panel: users/sessions/loadout/whitelist/audit; agent: plugin state).

## Repository layout

```
cmd/cs2a-agent     agent entrypoint
cmd/cs2a-panel     panel entrypoint
internal/agent     agent HTTP API + runtime services
internal/panel     panel HTTP UI (templ pages + htmx partials)
internal/rcon      minimal Source RCON client
internal/a2s       minimal A2S_INFO / A2S_PLAYER query client
internal/cfg       server.cfg read/write helpers
web/               templ templates & static assets
scripts/           bootstrap installer, dev helpers
```

## Development

```sh
go tool templ generate   # after editing web/**/*.templ
make test                # go test ./...
make build               # dist/cs2a-agent + dist/cs2a-panel
```

## Install (target experience)

```sh
curl -fsSL https://raw.githubusercontent.com/elvonpiko/cs2a/main/scripts/bootstrap.sh | bash
```

Interactive TUI installer that sets up SteamCMD, the CS2 server, systemd units,
firewall rules (27015/udp + panel port), and prints the generated admin secret.

## Status

Early MVP — see `docs/PLAN.md`.
