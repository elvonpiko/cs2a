# cs2a — plan & design decisions

## Goals

1. **Minimal**: two Go binaries, two SQLite files, no external services.
2. **Useful day one**: status, start/stop/restart, map changes, plugin
   installs, server password, whitelist, player loadouts.
3. **Safe by default**: agent loopback-only with token auth, firewall limited
   to game + panel ports, secrets on disk are 0600 and never committed.
4. **Adoptable**: an install onto a machine that already runs CS2 must leave that
   server working. The installer reads the existing unit rather than replacing
   it, and never creates a service account the running server does not use.

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
- The agent runs as root, the game does not. Everything the agent writes into the
  game tree — extracted plugins, plugin configs, whitelist files — is handed to
  the account the game runs as, or the server loses the ability to write its own
  configs.

## Key decisions

| Decision | Rationale |
|---|---|
| Go + templ + SQLite + htmx | single static binaries, server-rendered pages with sprinkles of interactivity, zero JS build step (htmx vendored) |
| RCON + A2S implemented in-repo | tiny protocols; avoids pulling heavy SDKs, exact behavior needed (multi-packet reassembly, challenge handling) is small and testable |
| `server.cfg` managed block | cs2a only edits between `>>> cs2a managed block <<<` markers, preserving everything the admin wrote by hand |
| Plugin installs via GitHub releases | the CS2 plugin ecosystem distributes as zip/tar.gz releases on GitHub; each catalog entry declares the paths it owns so uninstall removes exactly those and never the shared `addons/` tree |
| Installs run as agent-side jobs | a 50 MB download behind a reverse proxy outlives any sane request timeout, so the panel starts a job (202), polls a partial while it runs, and stops polling when it finishes |
| systemd for the game process | CS2 is happiest under a supervisor; cs2a gets uptime, restart-on-crash and journal access for free |
| First-admin via one-time setup token | avoids shipping a default password; the installer prints the token, the panel consumes it on first use |
| Installer discovers before it asks | RCON password, game port, install dir and unit name are all readable from the machine; asking for them is a worse experience *and* a chance to get them wrong |
| Adopt an existing server, never rewrite it | most installs land on a machine that already runs CS2; cs2a reads the unit instead of replacing it, adds `-usercon` through a drop-in, and dials the address the unit actually binds |
| Failed logins are throttled in memory | the panel is the only internet-facing piece and authenticates with a password; counting failures per address and per username needs no store, no goroutine and no dependency |
| A partial protocol answer is an error *with* data | a `status` reply cut off at 4096 bytes and an unreassembled A2S split both used to read as success with fewer players; cs2a now returns what it parsed **and** says it is incomplete |
| Upstream behaviour is verified against upstream source | the whitelist plugin's "reload" command and WeaponPaints' table names were both wrong in the first implementation; every plugin integration is now checked against the released source or artifact, not documentation |
| A failed side effect never fails a successful save | a loadout that reached the panel database but not WeaponPaints' is reported as a warning, so players stop retrying something only an admin can fix |

## Milestones

- [x] M1 — protocols: RCON client, A2S query, SteamID normalization, status parsing
- [x] M2 — agent: config/store/systemd/plugins/whitelist/loadout + JSON API
- [x] M3 — panel: sessions, pages, actions, live status partial
- [x] M4 — installer: interactive idempotent bootstrap + uninstall
- [x] M5 — docs, tests green, repo hygiene
- [x] M6 — end-to-end audit: async install jobs, RCON fire-and-forget for
  `changelevel`, per-entry archive layouts, discovery-first installer, CSRF +
  secure cookies behind a proxy, panel redesign
- [x] M7 — journal viewer (server console tail) in the panel
- [x] M8 — protocol & integration hardening after the first real VPS deploy:
  RCON diagnosis (`-usercon`/`rcon_password`/bind address/wrong password), truncated
  RCON and split A2S replies surfaced as partial data, plugin pointer downloads
  retried across mirrors with an HTTP/1.1 fallback, dependency errors flattened,
  ownership preserved on every write into the game tree, session GC + server-side
  logout, live whitelist commands that the plugin actually implements
- [ ] M9 — workshop maps + map rotation editor
- [ ] M10 — hardening: non-root agent, audit log UI

## Testing strategy

- Unit tests per package (protocols against fake servers, config parsing,
  SteamID math, store CRUD, agent API with fake services).
- Panel integration test: real panel server + fake agent + cookie sessions,
  covering setup → login → roles → actions → loadout.
- Shell tests for the installer: `tests/bootstrap_test.sh` sources
  `scripts/bootstrap.sh` in library mode (`CS2A_BOOTSTRAP_LIB=1`) and runs its
  real helpers against fixture unit files and a fake CS2 tree, so discovery and
  JSON generation are tested rather than grepped for.
- Go tests for the install plan (`internal/bootstrap`): unit rendering, firewall
  rules, Caddy timeouts, and config JSON that survives secrets containing
  quotes and backslashes.
- Every fixed bug gets a test that fails on the old behaviour: adopting a server
  that binds a public address, a unit without `-usercon`, a lifecycle action that
  reports success for a unit that died, a whitelist switch that would lock the
  operator out, a cosmetic catalog that decodes to empty defaults.
- Protocol tests assert raw wire bytes, not the client's own assumptions. Several
  fakes were rewritten after they turned out to encode a shape no shipping CS2
  build emits (a `status` row with a SteamID column, an A2S player entry carrying
  one, RCON chunks that made the terminator unobservable); the replacements use
  verbatim server dumps.

## Known gaps

- Both services run as root (M10).
- One host per install; no multi-server management.
- Plugin config editing is raw JSON, with no schema validation beyond
  "is it parseable".
- The audit trail is written but only readable with `sqlite3` (M10).
- Plugin catalog versions are resolved at install time from GitHub releases;
  there is no notification when an installed plugin has a newer release.
