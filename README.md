# cs2a

> A minimal Counter-Strike 2 server manager.
> Web panel for admins and players, plus a small agent on the VPS. Two Go binaries, SQLite, no containers.

## Install

Fresh Ubuntu/Debian VPS, as root:

```sh
curl -fsSL https://elvonpiko.github.io/cs2a/install.sh | sudo bash
```

The installer discovers before it asks. It looks for SteamCMD, an existing CS2 install, its systemd unit, the game port, the address the unit binds, the account it runs as, the RCON password in `server.cfg`, Caddy and ufw — then installs and configures only what is missing. On a bare VPS that means SteamCMD, the CS2 server (~40 GB), the systemd units, firewall rules and the panel; on a machine that already runs CS2 it means the panel and agent alone, with your unit file left untouched.

Adopting a running server is the case that gets the most care: the agent dials exactly the address your launch line binds (not an assumed `127.0.0.1`), writes files as the user your unit already runs as, and tells you when `-usercon` is missing — the one flag without which CS2 never opens its RCON port, so map changes and console commands cannot work. It offers to add that flag through a systemd drop-in, leaving your unit file byte-for-byte as you wrote it.

Reruns are safe: the agent token, admin password, RCON password and skin-database credentials are reused rather than rotated.

```sh
sudo bash scripts/bootstrap.sh --no-cs2            # never install the game
sudo bash scripts/bootstrap.sh --domain cs.example # panel over HTTPS via Caddy
sudo bash scripts/bootstrap.sh --panel-local       # panel on loopback (use an SSH tunnel)
sudo bash scripts/bootstrap.sh --unattended        # no questions, all defaults
```

You are asked at most a handful of questions, and never for something the machine can answer itself. Give it a domain and Caddy provides automatic HTTPS with proxy timeouts long enough for plugin installs; without one the panel serves plain HTTP on `:8080` and says so, since your password would otherwise cross the network in the clear.

Uninstall: `sudo bash scripts/uninstall.sh` (add `--purge` for config and data, `--purge-game` for the CS2 install). A unit cs2a did not write is never removed, but the drop-in and Caddy site it added are.

## What you get

**Admins**
- Live server page — status, players, uptime, map preview; start/stop/restart with confirm dialogs
- Lifecycle actions that tell the truth: the agent waits for the unit to settle and brings back the journal tail when a start fails, instead of reporting success the moment `systemctl` exits
- A diagnosed RCON problem instead of `connection refused`: the panel names the cause (wrong bind address, no `-usercon`, no boot-time password) and offers a one-click repair
- Map changes that **keep everyone connected** (`changelevel`; only a restart drops players)
- One-click plugin catalog — Metamod:Source, CounterStrikeSharp, WeaponPaints, MatchZy, retakes, deathmatch, admin tools — always resolving the current upstream release, with a JSON config editor per plugin
- Installs run as background jobs with live progress, so a 50 MB download cannot time out the request; finished installs update their cards without a reload
- Server password and SteamID whitelist, applied live — no restart. Enforcement stays off until the list has someone on it, because an enforced empty whitelist locks out the operator too
- Panel accounts (admin/player) with an audit trail; failed sign-ins are throttled, expired sessions are swept, and signing out invalidates the token server-side

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

The agent never listens on a public interface — it refuses a non-loopback bind unless explicitly overridden — and the panel talks to it over loopback with a generated token compared in constant time. Panel forms are protected by Go's `CrossOriginProtection`; set a domain and cookies become `Secure` automatically. Files the agent installs into the game tree are handed to the account the game runs as, since the agent is root and the game is not.

RCON and A2S are implemented in-repo against the wire format, so a `status` reply longer than one 4096-byte packet and a split A2S answer both come back complete — and when one genuinely cannot be reassembled, the panel shows the players it did parse *and* says the answer was incomplete, rather than quietly reporting a smaller server.

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
