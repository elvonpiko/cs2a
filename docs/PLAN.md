# cs2a — plan & design decisions

## Goals

1. **Minimal**: two Go binaries, two SQLite files, no external services.
2. **Useful day one**: status, start/stop/restart, map changes, plugin
   installs, server password, whitelist, player loadouts.
3. **Safe by default**: agent loopback-only with token auth, firewall limited
   to game + panel ports, secrets on disk are 0600 and never committed.

## Non-goals (MVP)

- Multi-server / multi-host management (one VPS = one cs2a).
- Match/tournament tooling, demos, stats.
- Workshop map management.
- Running the agent as a non-root service (systemd + file ownership make this
  a future hardening step).

## Architecture

```
browser ──▶ cs2a-panel (templ SSR + htmx, :8080)
               │ HTTP + bearer token, loopback
               ▼
           cs2a-agent (JSON API, 127.0.0.1:8100)
               ├─ systemd ─▶ cs2-server.service (start/stop/restart/uptime)
               ├─ RCON (TCP 27015) ─▶ changelevel, cvars, status
               ├─ A2S (UDP 27015) ─▶ live info/player counts
               └─ filesystem ─▶ server.cfg managed block, plugin archives,
                                plugin configs, whitelist file
```

### Why this split

- The panel is the only internet-facing piece; it holds no game secrets.
- The agent holds the RCON password and touches game files; it binds to
  loopback only and refuses non-loopback binds unless `CS2A_AGENT_EXPOSE=1`.
- Both speak one tiny authenticated JSON API, so the panel can be re-skinned
  or replaced without touching game integration.

## Key decisions

| Decision | Rationale |
|---|---|
| Go + templ + SQLite + htmx | single static binaries, server-rendered pages with sprinkles of interactivity, zero JS build step (htmx vendored) |
| RCON + A2S implemented in-repo | tiny protocols; avoids pulling heavy SDKs, exact behavior needed (multi-packet reassembly, challenge handling) is small and testable |
| `server.cfg` managed block | cs2a only edits between `>>> cs2a managed block <<<` markers, preserving everything the admin wrote by hand |
| Plugin installs via GitHub releases | the CS2 plugin ecosystem distributes as zip/tar.gz releases on GitHub; manifest records installed top-level dirs so uninstall is exact |
| systemd for the game process | CS2 is happiest under a supervisor; cs2a gets uptime, restart-on-crash and journal access for free |
| First-admin via one-time setup token | avoids shipping a default password; the installer prints the token, the panel consumes it on first use |

## Milestones

- [x] M1 — protocols: RCON client, A2S query, SteamID normalization, status parsing
- [x] M2 — agent: config/store/systemd/plugins/whitelist/loadout + JSON API
- [x] M3 — panel: sessions, pages, actions, live status partial
- [x] M4 — installer: interactive idempotent bootstrap + uninstall
- [x] M5 — docs, tests green, repo hygiene
- [ ] M6 — journal viewer (server console tail) in the panel
- [ ] M7 — workshop maps + map rotation editor
- [ ] M8 — hardening: non-root agent, HTTPS guidance (Caddy reverse proxy), audit log UI

## Testing strategy

- Unit tests per package (protocols against fake servers, config parsing,
  SteamID math, store CRUD, agent API with fake services).
- Panel integration test: real panel server + fake agent + cookie sessions,
  covering setup → login → roles → actions → loadout.
- Shell tests for the installer (syntax + idempotency + unit-template
  invariants), Go tests for the install plan (`internal/bootstrap`).
