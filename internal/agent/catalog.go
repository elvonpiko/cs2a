package agent

// CatalogKind distinguishes how a component is loaded by the engine.
type CatalogKind string

const (
	// KindRuntime components (metamod, cssharp) load at server boot.
	KindRuntime CatalogKind = "runtime"
	// KindMetamodPlugin components are loaded by metamod at boot.
	KindMetamodPlugin CatalogKind = "metamod-plugin"
	// KindCSSharpPlugin components are hot-refreshable via css_plugins.
	KindCSSharpPlugin CatalogKind = "plugin"
)

// CatalogEntry describes one installable component.
//
// Dest/Strip/Owns are derived from the real release archives: upstream authors
// package their plugins inconsistently (some ship a full `addons/...` tree,
// some a bare plugin folder, some wrap everything in a version directory), so
// each entry states exactly where its artifact belongs.
type CatalogEntry struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Author      string      `json:"author,omitempty"`
	Kind        CatalogKind `json:"kind"`
	Requires    []string    `json:"requires,omitempty"`
	// Repo is "owner/name" on GitHub when the release comes from GitHub.
	Repo string `json:"repo,omitempty"`
	// AssetRegex picks the release asset to install.
	AssetRegex string `json:"asset_regex,omitempty"`
	// TagRegex restricts which release in the repo's list is used, when the
	// latest published release is not the right one. Metamod is the reason this
	// exists: its CS2 line (2.0.x) ships as prereleases, so "latest" answers
	// the 1.12 branch, which has no CS2 binary. Matching the tag keeps the
	// resolution on the right branch without pinning a version.
	TagRegex string `json:"tag_regex,omitempty"`
	// AssetReject drops assets that AssetRegex also matches. RE2 has no
	// negative lookahead, and several projects publish variants next to the
	// artifact a server actually wants (a Windows build, a website bundle, a
	// build with CounterStrikeSharp already inside). Encoding the exclusion
	// separately keeps both patterns readable.
	AssetReject string `json:"asset_reject,omitempty"`
	// URL is a direct download (used for metamod's stable latest build).
	URL string `json:"url,omitempty"`
	// URLMirrors are alternate hosts serving the identical artifact, tried in
	// order when URL fails. The AlliedModders drop is Cloudflare-fronted and
	// occasionally truncates responses; metamod is the root dependency of the
	// whole catalog, so one bad edge there must not fail every install.
	URLMirrors []string `json:"url_mirrors,omitempty"`
	// URLIsPointer marks URL as a text file whose body names the real
	// artifact, relative to URL's directory (the AlliedModders "-latest-"
	// scheme). Without this the URL 404s as soon as upstream rolls a build.
	URLIsPointer bool `json:"url_is_pointer,omitempty"`
	// Dest is the extraction destination relative to game/csgo ("" = csgo
	// itself).
	Dest string `json:"dest,omitempty"`
	// Strip drops this many leading path components from every archive entry
	// (for archives wrapped in a release/version directory).
	Strip int `json:"strip,omitempty"`
	// Owns lists the csgo-relative paths this entry created, used for an exact
	// uninstall. Without it, uninstalling something that ships into `addons/`
	// would delete every other plugin too.
	Owns []string `json:"owns,omitempty"`
	// PostInstall runs after extraction ("gameinfo-metamod", "guidelines-off",
	// "wp-default-config", "whitelist-core-cfg").
	PostInstall []string `json:"post_install,omitempty"`
	// Homepage for the UI "docs" link.
	Homepage string `json:"homepage,omitempty"`
	// ConfigPath is the JSON config file (relative to game/csgo) the panel may
	// edit, if any.
	ConfigPath string `json:"config_path,omitempty"`

	// Recommended marks the components cs2a's own features build on
	// (WeaponPaints powers the Loadout, cs2whitelist the Access page's
	// restrictive mode). Bootstrap offers to install them all on a fresh
	// server and the panel badges them, so "first-class" is visible without
	// a second tier of plugin-hood.
	Recommended bool `json:"recommended,omitempty"`
	// Installed and InstalledVersion are runtime state filled in by
	// Installer.Catalog. They are typed fields rather than an annotation
	// spliced into Description: the panel used to parse "[installed v1.2] …"
	// back out of the description text, which broke as soon as a version
	// string contained a "]" and silently mislabelled cards.
	Installed        bool   `json:"installed,omitempty"`
	InstalledVersion string `json:"installed_version,omitempty"`
	// RequiredBy names installed components that depend on this one. Runtime
	// state, like Installed: the panel uses it to explain why Uninstall is
	// unavailable instead of letting the operator break their own server.
	RequiredBy []string `json:"required_by,omitempty"`
}

// HasConfig reports whether the panel can offer a config editor for this entry.
func (e CatalogEntry) HasConfig() bool { return e.ConfigPath != "" }

// cssharpPlugins is where CounterStrikeSharp loads plugin folders from.
const cssharpPlugins = "addons/counterstrikesharp/plugins"

// cssharpConfigs is where CounterStrikeSharp generates plugin configs.
const cssharpConfigs = "addons/counterstrikesharp/configs/plugins"

// metamodMirrors are the official AlliedModders aliases for the Metamod drop.
// All three hostnames front the same origin and serve byte-identical builds
// (verified by sha256), so they are interchangeable when one edge misbehaves.
var metamodMirrors = []string{
	"https://www.metamodsource.net/mmsdrop/2.0/mmsource-latest-linux",
	"https://www.sourcemm.net/mmsdrop/2.0/mmsource-latest-linux",
}

// DefaultCatalog is the shipped catalog. Versions are never hardcoded: every
// entry resolves against the live release list at install time. Layouts below
// were verified against the actual published artifacts.
func DefaultCatalog() []CatalogEntry {
	return []CatalogEntry{
		{
			ID:          "metamod",
			Recommended: true,
			Name:        "Metamod:Source",
			Description: "Plugin loader for the Source 2 engine. Required by every other component.",
			Author:      "AlliedModders",
			Kind:        KindRuntime,
			// Metamod resolves from the AlliedModders GitHub releases, not the
			// mmsdrop pointer: mms.alliedmods.net and both its mirrors are the
			// same Cloudflare origin, and a host whose route to that origin
			// resets TLS got "EOF" from all three mirrors at once — while the
			// GitHub API (already the source of every other entry) worked
			// fine. The same 2.0 builds are published as GitHub prereleases
			// (byte-identical tarball names), and TagRegex keeps the pick on
			// the 2.0 branch because "latest" answers 1.12, which has no CS2
			// binary. The drop remains as fallback mirrors: pointer first,
			// GitHub asset appended, so one origin dying cannot take the root
			// dependency of the catalog with it.
			Repo:         "alliedmodders/metamod-source",
			AssetRegex:   `(?i)^mmsource-2\.\d+\.\d+-git\d+-linux\.tar\.gz$`,
			TagRegex:     `^2\.`,
			URL:          "https://mms.alliedmods.net/mmsdrop/2.0/mmsource-latest-linux",
			URLMirrors:   metamodMirrors,
			URLIsPointer: true,
			Owns:         []string{"addons/metamod", "addons/metamod.vdf", "addons/metamod_x64.vdf"},
			PostInstall:  []string{"gameinfo-metamod"},
			Homepage:     "https://cs2.poggu.me/metamod/installation/",
		},
		{
			ID:          "cssharp",
			Recommended: true,
			Name:        "CounterStrikeSharp",
			Description: "The dominant CS2 plugin runtime (C#). Required by all cssharp-based plugins. The with-runtime build bundles .NET, so nothing else has to be installed.",
			Author:      "roflmuffin",
			Kind:        KindRuntime,
			Requires:    []string{"metamod"},
			Repo:        "roflmuffin/CounterStrikeSharp",
			AssetRegex:  `(?i)^counterstrikesharp-with-runtime-linux.*\.zip$`,
			Owns: []string{
				"addons/counterstrikesharp",
				"addons/metamod/counterstrikesharp.vdf",
			},
			// The release binary is linked with an executable stack
			// (GNU_STACK RWE). Debian 13 / Ubuntu 25.04 refuse to load such
			// objects ("cannot enable executable stack as shared object
			// requires"), so the install clears the flag on the file it just
			// placed (see clearExecStack).
			PostInstall: []string{"cssharp-execstack", "cssharp-icu-check"},
			Homepage:    "https://docs.cssharp.dev",
		},
		{
			// The three NickFox007 libraries WeaponPaints builds on: its
			// menus are MenuManager screens, and player choices persist via
			// PlayerSettings over AnyBaseLib's database layer. Upstream
			// documents them as hard requirements, and a WeaponPaints install
			// without them fails at runtime (MenuCapability.Get) rather than at
			// install time — which is exactly the failure a dependency
			// declaration prevents.
			ID:          "anybaselib",
			Name:        "AnyBaseLib",
			Description: "Database access layer for CounterStrikeSharp plugins (SQLite/MySQL/PostgreSQL). Library dependency of PlayerSettings, MenuManager and WeaponPaints.",
			Author:      "NickFox007",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp"},
			Repo:        "NickFox007/AnyBaseLibCS2",
			AssetRegex:  `(?i)^anybaselib\.zip$`,
			Dest:        cssharpPlugins,
			Owns:        []string{cssharpPlugins + "/AnyBaseLib"},
			Homepage:    "https://github.com/NickFox007/AnyBaseLibCS2",
		},
		{
			ID:          "playersettings",
			Name:        "PlayerSettings",
			Description: "Per-player persistent settings storage (ClientCookies analogue). Library dependency of MenuManager and WeaponPaints.",
			Author:      "NickFox007",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp", "anybaselib"},
			Repo:        "NickFox007/PlayerSettingsCS2",
			AssetRegex:  `(?i)^playersettings\.zip$`,
			Dest:        cssharpPlugins,
			Owns:        []string{cssharpPlugins + "/PlayerSettings"},
			Homepage:    "https://github.com/NickFox007/PlayerSettingsCS2",
		},
		{
			ID:          "menumanager",
			Name:        "MenuManager",
			Description: "In-game menu system for CounterStrikeSharp plugins. WeaponPaints renders its !ws / !knife menus through it.",
			Author:      "NickFox007",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp", "anybaselib", "playersettings"},
			Repo:        "NickFox007/MenuManagerCS2",
			AssetRegex:  `(?i)^menumanager\.zip$`,
			Dest:        cssharpPlugins,
			Owns:        []string{cssharpPlugins + "/MenuManager"},
			Homepage:    "https://github.com/NickFox007/MenuManagerCS2",
		},
		{
			ID:          "weaponpaints",
			Recommended: true,
			Name:        "WeaponPaints",
			Description: "Player-selected weapon, knife, glove and agent skins applied server-side. Backed by a MySQL database — cs2a writes players' loadout choices straight into it.",
			Author:      "Nereziel",
			Kind:        KindCSSharpPlugin,
			// Upstream's own dependency tree: the plugin builds on cssharp's
			// API, AnyBaseLib's database layer, PlayerSettings for persistence
			// and MenuManager for its in-game menus. Installing only
			// WeaponPaints left it failing at MenuCapability.Get on every
			// boot, which is why the whole chain is declared here.
			Requires: []string{"cssharp", "anybaselib", "playersettings", "menumanager"},
			Repo:     "Nereziel/cs2-WeaponPaints",
			// The release also carries WeaponPaints-Website.zip, which is the
			// PHP web UI and must not be installed onto the game server.
			AssetRegex:  `(?i)^weaponpaints(-[^/]*)?\.zip$`,
			AssetReject: `(?i)(website|windows)`,
			// WeaponPaints.zip contains a bare WeaponPaints/ folder, not an
			// addons/ tree, so it is placed into the plugins directory. It also
			// ships a second top-level gamedata/ dir, which wp-gamedata moves
			// to the path the plugin reads (see placeWeaponPaintsGamedata).
			Dest: cssharpPlugins,
			Owns: []string{
				cssharpPlugins + "/WeaponPaints",
				"addons/counterstrikesharp/gamedata/weaponpaints.json",
			},
			PostInstall: []string{"guidelines-off", "wp-gamedata", "wp-default-config"},
			Homepage:    "https://github.com/Nereziel/cs2-WeaponPaints",
			ConfigPath:  cssharpConfigs + "/WeaponPaints/WeaponPaints.json",
		},
		{
			ID:          "cs2whitelist",
			Recommended: true,
			Name:        "CS2 Whitelist",
			Description: "Restrict server access to whitelisted SteamIDs, IPs or Steam groups (Metamod plugin). cs2a manages the whitelist file and its on/off switch from the Access page.",
			Author:      "FemboyKZ",
			Kind:        KindMetamodPlugin,
			Requires:    []string{"metamod"},
			Repo:        "FemboyKZ/mm-cs2whitelist",
			AssetRegex:  `(?i)^cs2whitelist-.*-linux\.zip$`,
			Owns: []string{
				"addons/cs2whitelist",
				"addons/metamod/cs2whitelist.vdf",
			},
			// Writes cfg/cs2whitelist/core.cfg (KeyValues, not JSON) so the
			// plugin is enabled and reads the file cs2a maintains.
			PostInstall: []string{"whitelist-core-cfg"},
			Homepage:    "https://github.com/FemboyKZ/mm-cs2whitelist",
		},
		{
			ID:          "simpleadmin",
			Name:        "CS2-SimpleAdmin",
			Description: "Full admin toolkit: ban/mute/kick/slay, warns, penalties and admin groups, with MySQL storage shared across servers.",
			Author:      "daffyyyy",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp"},
			Repo:        "daffyyyy/CS2-SimpleAdmin",
			AssetRegex:  `(?i)^cs2-simpleadmin-.*\.zip$`,
			// Ships a bare counterstrikesharp/ tree (plugins/ + shared/).
			Dest: "addons",
			// The archive installs two companion modules next to the main
			// plugin; both carry their own copy of the shared API and break
			// when it is removed, so uninstall has to take all four paths.
			Owns: []string{
				cssharpPlugins + "/CS2-SimpleAdmin",
				cssharpPlugins + "/CS2-SimpleAdmin_FunCommands",
				cssharpPlugins + "/CS2-SimpleAdmin_StealthModule",
				"addons/counterstrikesharp/shared/CS2-SimpleAdminApi",
			},
			Homepage:   "https://github.com/daffyyyy/CS2-SimpleAdmin",
			ConfigPath: cssharpConfigs + "/CS2-SimpleAdmin/CS2-SimpleAdmin.json",
		},
		{
			ID:          "matchzy",
			Name:        "MatchZy",
			Description: "Competitive match/scrim/pug management: knife round, warmup, pauses, backups, .ready system, demo recording and GOTV.",
			Author:      "shobhit-pathak",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp"},
			Repo:        "shobhit-pathak/MatchZy",
			// The plain asset only: the release also ships
			// "-with-cssharp-linux"/"-windows" bundles, and cs2a installs
			// CounterStrikeSharp itself as a dependency.
			AssetRegex:  `(?i)^matchzy-[0-9][^/]*\.zip$`,
			AssetReject: `(?i)(with-cssharp|windows)`,
			Owns: []string{
				cssharpPlugins + "/MatchZy",
				"cfg/MatchZy",
			},
			Homepage:   "https://shobhit-pathak.github.io/MatchZy/",
			ConfigPath: cssharpConfigs + "/MatchZy/MatchZy.json",
		},
		{
			ID:          "retakes",
			Name:        "Retakes",
			Description: "Retakes game mode with bombsite allocation, per-map spawns and weapon allocation — the CS2 port of splewis' classic plugin.",
			Author:      "B3none",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp"},
			Repo:        "B3none/cs2-retakes",
			// Releases were renamed from cs2-retakes-* to RetakesPlugin-*;
			// accept both so pinning an older tag still resolves.
			AssetRegex: `(?i)^(retakesplugin|cs2-retakes)-[0-9][^/]*\.zip$`,
			// The default bundle includes the per-map spawn configs; the
			// "-no-map-configs" variant would leave retakes unplayable until
			// the admin sourced spawns themselves. "shared" is the separate
			// library-only asset of the old naming scheme.
			AssetReject: `(?i)(no-map-configs|shared)`,
			Owns: []string{
				cssharpPlugins + "/RetakesPlugin",
				"addons/counterstrikesharp/shared/RetakesPluginShared",
			},
			Homepage:   "https://github.com/B3none/cs2-retakes",
			ConfigPath: cssharpConfigs + "/RetakesPlugin/RetakesPlugin.json",
		},
		{
			ID:          "deathmatch",
			Name:        "Deathmatch",
			Description: "Free-for-all and team deathmatch with custom spawn points, gun menus, spawn protection and per-map configs.",
			Author:      "NockyCZ",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp"},
			Repo:        "NockyCZ/CS2-Deathmatch",
			AssetRegex:  `(?i)^deathmatch\.zip$`,
			// Ships Deathmatch/{plugins,shared}/… — one level above the
			// counterstrikesharp dir.
			Dest:  "addons/counterstrikesharp",
			Strip: 1,
			Owns: []string{
				cssharpPlugins + "/Deathmatch",
				"addons/counterstrikesharp/shared/DeathmatchAPI",
			},
			Homepage:   "https://github.com/NockyCZ/CS2-Deathmatch",
			ConfigPath: cssharpConfigs + "/Deathmatch/Deathmatch.json",
		},
		{
			ID:          "customvotes",
			Name:        "Custom Votes",
			Description: "Player-callable votes for server settings — !votes opens the menu; every vote is defined in the config.",
			Author:      "imi-tat0r",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp"},
			Repo:        "imi-tat0r/CS2-CustomVotes",
			AssetRegex:  `(?i)^cs2-customvotes-.*\.zip$`,
			Owns: []string{
				cssharpPlugins + "/CS2-CustomVotes",
				"addons/counterstrikesharp/shared/CS2-CustomVotes.Shared",
			},
			Homepage:   "https://github.com/imi-tat0r/CS2-CustomVotes",
			ConfigPath: cssharpConfigs + "/CS2-CustomVotes/CS2-CustomVotes.json",
		},
		{
			ID:          "cs2fixes",
			Name:        "CS2Fixes",
			Description: "Engine-level fixes and admin features (Metamod, no C# runtime needed): entity fixes, votes, admin commands, zombie-escape tooling.",
			Author:      "Source2ZE",
			Kind:        KindMetamodPlugin,
			Requires:    []string{"metamod"},
			Repo:        "Source2ZE/CS2Fixes",
			// steamrt3 matches the runtime CS2 dedicated servers ship with.
			// The steamrt3/steamrt4 split is recent; every release up to v1.19
			// published a plain "-linux" tarball, so both names are accepted
			// (steamrt4 and windows still never match).
			AssetRegex: `(?i)^cs2fixes-.*-(steamrt3|linux)\.tar\.gz$`,
			Owns: []string{
				"addons/cs2fixes",
				"addons/metamod/cs2fixes.vdf",
				"cfg/cs2fixes",
				"materials/cs2fixes",
				"particles/cs2fixes",
				"soundevents/soundevents_zr.vsndevts_c",
				"sounds/zr",
			},
			Homepage: "https://github.com/Source2ZE/CS2Fixes",
		},
	}
}

// Find returns the catalog entry with the given id.
func Find(catalog []CatalogEntry, id string) (CatalogEntry, bool) {
	for _, e := range catalog {
		if e.ID == id {
			return e, true
		}
	}
	return CatalogEntry{}, false
}
