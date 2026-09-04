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
	// URL is a direct download (used for metamod's stable latest build).
	URL string `json:"url,omitempty"`
	// PostInstall runs after extraction ("gameinfo-metamod", "guidelines-off").
	PostInstall []string `json:"post_install,omitempty"`
	// Homepage for the UI "docs" link.
	Homepage string `json:"homepage,omitempty"`
	// ConfigPath is the cssharp config file (relative to configs/) that the
	// panel may edit, if any.
	ConfigPath string `json:"config_path,omitempty"`
}

// DefaultCatalog is the shipped MVP catalog. Entries are resolved at install
// time against the live release lists; no versions are hardcoded.
func DefaultCatalog() []CatalogEntry {
	return []CatalogEntry{
		{
			ID:          "metamod",
			Name:        "Metamod:Source",
			Description: "Plugin loader for the Source engine. Required by every other component.",
			Author:      "AlliedModders",
			Kind:        KindRuntime,
			URL:         "https://mms.alliedmods.net/mmsdrop/2.0/mmsource-latest-linux.tar.gz",
			PostInstall: []string{"gameinfo-metamod"},
			Homepage:    "https://cs2.poggu.me/metamod/installation/",
		},
		{
			ID:          "cssharp",
			Name:        "CounterStrikeSharp",
			Description: "The dominant CS2 plugin runtime. Required by all cssharp-based plugins.",
			Author:      "roflmuffin",
			Kind:        KindRuntime,
			Requires:    []string{"metamod"},
			Repo:        "roflmuffin/CounterStrikeSharp",
			AssetRegex:  `(?i)with-runtime.*linux.*\.zip`,
			Homepage:    "https://docs.cssharp.dev",
		},
		{
			ID:          "weaponpaints",
			Name:        "WeaponPaints",
			Description: "Player-selected weapon & knife skins applied server-side. Backed by a MySQL database (configure after install).",
			Author:      "Nereziel",
			Kind:        KindCSSharpPlugin,
			Requires:    []string{"cssharp"},
			Repo:        "Nereziel/cs2-WeaponPaints",
			AssetRegex:  `(?i)weaponpaints.*\.zip`,
			PostInstall: []string{"guidelines-off"},
			Homepage:    "https://github.com/Nereziel/cs2-WeaponPaints",
			ConfigPath:  "plugins/WeaponPaints/WeaponPaints.json",
		},
		{
			ID:          "cs2whitelist",
			Name:        "CS2 Whitelist",
			Description: "Restrict server access to whitelisted SteamIDs (Metamod plugin). cs2a manages the whitelist file.",
			Author:      "jvnipers / FemboyKZ",
			Kind:        KindMetamodPlugin,
			Requires:    []string{"metamod"},
			Repo:        "FemboyKZ/mm-cs2whitelist",
			AssetRegex:  `(?i)whitelist.*(linux|win).*\.(zip|tar\.gz|tgz)`,
			Homepage:    "https://github.com/jvnipers/mm-cs2whitelist",
			ConfigPath:  "cs2whitelist/whitelist.txt",
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
