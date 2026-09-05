# CS2 plugin ecosystem — research notes

How plugin support in cs2a works, and why. Sources were reviewed while
designing the catalog (`internal/agent/catalog.go`).

## The stack

CS2 plugins form two layers:

1. **Metamod:Source** — the loader. Hooks into the engine via `gameinfo.gi`.
   cs2a patches the game's `gameinfo.gi` to add
   `Game    csgo/addons/metamod` above the `Game csgo` line (idempotently,
   preserving whatever indentation the file uses).
2. **CounterStrikeSharp (CSSharp)** — the scripting runtime on top of
   Metamod. Most modern CS2 plugins (WeaponPaints, whitelisters, admin tools)
   are CSSharp plugins. Requires `FollowCS2ServerGuidelines: false` in
   `addons/counterstrikesharp/configs/core.json` for plugins that touch
   cosmetics/commands outside the official guidelines (WeaponPaints needs it).

## The catalog

`internal/agent/catalog.go` is the single source of truth. Versions are never
hardcoded: every entry resolves against the live release list at install time,
so the panel always offers the current upstream build.

| Entry | Kind | Source | Asset |
|---|---|---|---|
| Metamod:Source | runtime | `mms.alliedmods.net/mmsdrop/2.0/` | `mmsource-latest-linux` pointer |
| CounterStrikeSharp | runtime | `roflmuffin/CounterStrikeSharp` | `counterstrikesharp-with-runtime-linux-*.zip` |
| cs2-WeaponPaints | cssharp plugin | `Nereziel/cs2-WeaponPaints` | `WeaponPaints.zip` |
| mm-cs2whitelist | metamod plugin | `FemboyKZ/mm-cs2whitelist` | `cs2whitelist-*-linux.zip` |
| CS2-SimpleAdmin | cssharp plugin | `daffyyyy/CS2-SimpleAdmin` | `CS2-SimpleAdmin-*.zip` |
| MatchZy | cssharp plugin | `shobhit-pathak/MatchZy` | `MatchZy-<ver>.zip` |
| Retakes | cssharp plugin | `B3none/cs2-retakes` | `RetakesPlugin-<ver>.zip` |
| Deathmatch | cssharp plugin | `NockyCZ/CS2-Deathmatch` | `Deathmatch.zip` |
| Custom Votes | cssharp plugin | `imi-tat0r/CS2-CustomVotes` | `CS2-CustomVotes-*.zip` |
| CS2Fixes | metamod plugin | `Source2ZE/CS2Fixes` | `CS2Fixes-*-steamrt3.tar.gz` |

Two details that are easy to get wrong:

- **Metamod publishes no usable GitHub release.** Its GitHub releases carry only
  2.0.x prereleases and a 1.12 line with no CS2 binary, so the catalog uses the
  AlliedModders drop directory instead. `mmsource-latest-linux` there is a
  *pointer file* whose body is the current build's filename
  (`mmsource-2.0.0-git1411-linux.tar.gz`). Fetching `mmsource-latest-linux.tar.gz`
  directly 404s as soon as upstream rolls a build, so `URLIsPointer` makes the
  installer read the pointer and resolve the real artifact against its directory.
  `mms.alliedmods.net` is a single point of failure for every plugin (everything
  depends on Metamod), and it does drop connections mid-response — the panel
  reported `read pointer: … unexpected EOF`. The fetcher therefore retries,
  falls back to HTTP/1.1, and then tries `www.metamodsource.net` and
  `www.sourcemm.net`, which serve the byte-identical tarball. Upstream publishes
  no checksum file for this directory, so there is nothing to verify against.
- **Several releases ship near-identical assets.** MatchZy publishes
  `-with-cssharp-linux` and `-windows` bundles beside the plain zip; cs2-retakes
  publishes a `-no-map-configs` variant that leaves retakes unplayable without
  hand-sourced spawns. `AssetReject` removes those, and a pattern that still
  matches more than one asset is a hard error — the installer refuses to guess
  rather than silently install a Windows build.

Install = resolve the release → download once into the plugin cache → extract
into `game/csgo/` at the entry's `Dest` (stripping wrapper directories) → run
post-install patchers → hand the extracted files to the account the game runs
as → record the entry's `Owns` paths in the agent DB.

That ownership step matters because the agent runs as root while the game server
does not: files left root-owned load fine but the plugin cannot write its own
configs or logs, and the panel's config editor would create root-owned files
inside a tree the game user has to manage. The owner is taken from the csgo
directory itself, falling back to the game unit's `User=`.

`Owns` is what makes uninstall safe. Most plugins extract into the shared
`addons/` tree, so removing "the top-level directories the archive created"
would delete every other plugin with it; `Owns` names the exact paths an entry
is responsible for, and uninstall removes only those.

Uninstalling a runtime that other installed plugins need is refused outright:
removing Metamod used to leave every CSSharp plugin present but silently dead,
with nothing in the panel explaining why.

Archives are extracted with a hard size cap, path-traversal checks and a
symlink refusal, since the payload comes from a third-party release.

## WeaponPaints data model (loadout sync)

WeaponPaints stores player selections in **MySQL** (no SQLite support, no
REST API). When the admin configures `wp_dsn` in the agent config, cs2a
writes loadout changes straight into its tables:

- `wp_player_skins` — per-weapon skin selections
- `wp_player_knife` — `steamid`, `weapon_team` (2 = T, 3 = CT), `knife`
  (model name string, e.g. `weapon_knife_karambit`); upsert on duplicate key
- `wp_player_gloves` — same shape, gloves
- `wp_player_agents` — agent models

cs2a also keeps its own copy in the agent SQLite (`loadout:<steamid>` meta
keys) so the panel works even when MySQL is not configured — the panel shows
a "not syncing" notice in that case.

Knife model names supported by the panel dropdown: `weapon_bayonet`,
`weapon_knife_css`, `weapon_knife_flip`, `weapon_knife_gut`,
`weapon_knife_karambit`, `weapon_knife_m9_bayonet`, `weapon_knife_tactical`,
`weapon_knife_falchion`, `weapon_knife_survival_bowie`,
`weapon_knife_butterfly`, `weapon_knife_push`, `weapon_knife_cord`,
`weapon_knife_canis`, `weapon_knife_ursus`, `weapon_knife_gypsy_jackknife`,
`weapon_knife_outdoor`, `weapon_knife_stiletto`, `weapon_knife_widowmaker`,
`weapon_knife_skeleton`, `weapon_knife_kukri`.

Players apply cosmetics in-game with `!wp`; cs2a only pre-sets the defaults.

## Whitelist

`mm-cs2whitelist` reads `cfg/cs2whitelist/whitelist.txt` (one SteamID64 per
line, `//`, `#` and `;` comments allowed). Enforcement has **two** switches that
must agree:

- `Enable` in `cfg/cs2whitelist/core.cfg` (KeyValues) — read once when Metamod
  finishes loading plugins, and copied into the cvar below. This is the
  persistent setting.
- `mm_whitelist_enable` — the live cvar. Changing only the file leaves the
  running server on its old value until it restarts.

cs2a therefore writes the file *and* pushes the cvar. It:

- normalizes every accepted format (`STEAM_1:y:z`, `[U:1:N]`, raw SteamID64)
  to SteamID64,
- dedupes + sorts, writes the file atomically with a `//` header,
- flips `Enable` in `core.cfg` from the panel's Access page, rewriting only
  that key so operator settings in the file survive,
- pushes `mm_whitelist_enable 0|1` over RCON so the toggle takes effect at once,
- runs `mm_whitelist_reload` after a list change, then
  `mm_whitelist_cache_clear`: the plugin caches its allow/reject decision per
  player for the current map, so a reload alone would keep rejecting someone
  who was just added,
- lets admins whitelist a panel user with one click (uses the SteamID linked
  on their account).

The command names come from the plugin's own console commands. cs2a used to send
`wl_reload`, which no released version implements — every "applied live" claim
was silently discarded by the server.

An **enforced but empty** whitelist rejects everyone, including the admin who
flipped the switch. The panel therefore leaves the toggle disabled until the list
has at least one entry, and the agent refuses the change even if the request
arrives some other way.

## Player-facing cosmetics workflow (end to end)

1. Installer: answer **yes** to the MariaDB question (or run it later and set
   `wp_dsn` in `/opt/cs2a/etc/agent.json`).
2. Panel → **Plugins**: install **Metamod:Source**, then **CounterStrikeSharp**,
   then **cs2-WeaponPaints** (dependencies install automatically).
3. Panel → **Plugins → cs2-WeaponPaints → Edit config**: put the MariaDB
   credentials the installer printed into `DatabaseConfig`.
4. Restart the server from the panel. WeaponPaints creates its tables on boot.
5. Knives / gloves / agents: players pick them on the panel **Loadout** page
   (cs2a writes `wp_player_knife`, `wp_player_gloves`, `wp_player_agents`).
6. Gun skin paint kits: chosen **in game** with WeaponPaints' own `!wp` menu
   (stored in `wp_player_skins`). The panel intentionally does not duplicate
   the hundreds of paint-kit IDs.
7. Loadouts apply on connect; `!wp` in game refreshes without reconnecting.

## References

- Metamod:Source downloads: <https://mms.alliedmods.net/mmsdrop/2.0/>
- CounterStrikeSharp: <https://github.com/roflmuffin/CounterStrikeSharp>
- cs2-WeaponPaints: <https://github.com/Nereziel/cs2-WeaponPaints>
- mm-cs2whitelist: <https://github.com/FemboyKZ/mm-cs2whitelist>
- CS2-SimpleAdmin: <https://github.com/daffyyyy/CS2-SimpleAdmin>
- MatchZy: <https://shobhit-pathak.github.io/MatchZy/>
- cs2-retakes: <https://github.com/B3none/cs2-retakes>
- CS2-Deathmatch: <https://github.com/NockyCZ/CS2-Deathmatch>
- CS2-CustomVotes: <https://github.com/imi-tat0r/CS2-CustomVotes>
- CS2Fixes: <https://github.com/Source2ZE/CS2Fixes>
- Knife/glove model names: <https://github.com/ByMykel/CSGO-API>
- Source RCON protocol: <https://developer.valvesoftware.com/wiki/Source_RCON_Protocol>
- A2S queries: <https://developer.valvesoftware.com/wiki/Server_queries>
