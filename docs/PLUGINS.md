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

Distributions:

| Plugin | Distribution | cs2a install source |
|---|---|---|
| Metamod:Source | tar.gz snapshots at `mms.alliedmods.net/mmsdrop/2.0/` | `mmsource-latest-linux.tar.gz` |
| CounterStrikeSharp | GitHub releases (`roflmuffin/CounterStrikeSharp`), zip with runtime | release asset matching `with-runtime.*linux*.zip` |
| cs2-WeaponPaints | GitHub releases (`Nereziel/cs2-WeaponPaints`), zip | release asset matching `weaponpaints*.zip` |
| mm-cs2whitelist | GitHub releases (`FemboyKZ/mm-cs2whitelist`) | release asset |

Install = download once into a cache → extract into `game/csgo/` → run
post-install patchers (gameinfo, core.json) → record installed top-level
dirs in the agent DB so uninstall removes exactly those paths.

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

`mm-cs2whitelist` reads `cfg/cs2whitelist/whitelist.txt` (one SteamID per
line) and is toggled with the `mm_whitelist_enable` cvar. cs2a:

- normalizes every accepted format (`STEAM_1:y:z`, `[U:1:N]`, raw SteamID64)
  to SteamID64,
- dedupes + sorts, writes the file atomically with a small header,
- keeps the enable cvar in the `server.cfg` managed block,
- lets admins whitelist a panel user with one click (uses the SteamID linked
  on their account).

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
- Source RCON protocol: <https://developer.valvesoftware.com/wiki/Source_RCON_Protocol>
- A2S queries: <https://developer.valvesoftware.com/wiki/Server_queries>
