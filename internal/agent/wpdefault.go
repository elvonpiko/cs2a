package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// weaponPaintsDefaultConfig mirrors cs2-WeaponPaints' WeaponPaintsConfig
// (ConfigVersion 10, verified against upstream Config.cs) with every
// cosmetic category enabled so players can use knives, gloves, agents,
// skins, music and pins out of the box. Only the DB credentials need to be
// filled in (by the bootstrap installer or the panel config editor).
const weaponPaintsDefaultConfig = `{
  "ConfigVersion": 10,
  "SkinsLanguage": "en",
  "DatabaseHost": "",
  "DatabasePort": 3306,
  "DatabaseUser": "",
  "DatabasePassword": "",
  "DatabaseName": "",
  "CmdRefreshCooldownSeconds": 3,
  "Website": "",
  "MenuType": "selectable",
  "Additional": {
    "KnifeEnabled": true,
    "GloveEnabled": true,
    "MusicEnabled": true,
    "AgentEnabled": true,
    "SkinEnabled": true,
    "PinsEnabled": true,
    "CommandWpEnabled": true,
    "CommandKillEnabled": false,
    "CommandKnife": ["knife"],
    "CommandMusic": ["music"],
    "CommandPin": ["pin", "pins", "coin", "coins"],
    "CommandGlove": ["gloves"],
    "CommandAgent": ["agents"],
    "CommandStattrak": ["stattrak", "st"],
    "CommandSkin": ["ws"],
    "CommandSkinSelection": ["skins"],
    "CommandRefresh": ["wp"],
    "CommandKill": ["kill"],
    "GiveRandomKnife": false,
    "GiveRandomSkin": false,
    "ShowSkinImage": true
  }
}
`

// writeWeaponPaintsDefaultConfig writes the default WeaponPaints.json after
// install. If a config already exists (re-install), it is left untouched.
//
// When the agent already knows the WeaponPaints database (wp_dsn, set up by the
// installer), the credentials are filled in here. They used to be printed once
// by the installer and then had to be retyped into the panel's config editor by
// hand — miss that and the plugin loads with an empty DatabaseHost and no skin
// ever applies.
func (in *Installer) writeWeaponPaintsDefaultConfig() error {
	path := filepath.Join(in.cfg.CSGODir(),
		"addons", "counterstrikesharp", "configs", "plugins", "WeaponPaints", "WeaponPaints.json")
	if _, err := os.Stat(path); err == nil {
		return nil // never clobber an existing admin-edited config
	}
	body := []byte(weaponPaintsDefaultConfig)
	var dsnWarning error
	if in.cfg.WPDsn != "" {
		filled, err := weaponPaintsConfigWithDSN(weaponPaintsDefaultConfig, in.cfg.WPDsn)
		if err != nil {
			// Still write the config with empty credentials: the plugin needs
			// the file to exist, and the operator can fill them in from the
			// panel's config editor.
			dsnWarning = warnf("WeaponPaints config written without database credentials: %v", err)
		} else {
			body = filled
		}
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := atomicWrite(path, body, 0o644); err != nil {
		return err
	}
	return dsnWarning
}

// weaponPaintsConfigWithDSN copies the default config with the Database* fields
// taken from a Go MySQL DSN ("user:pass@tcp(host:port)/dbname").
func weaponPaintsConfigWithDSN(base, dsn string) ([]byte, error) {
	c, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("wp_dsn is not a valid MySQL DSN: %w", err)
	}
	host, port := c.Addr, 3306
	if h, p, err := net.SplitHostPort(c.Addr); err == nil {
		host = h
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(base), &doc); err != nil {
		return nil, err
	}
	doc["DatabaseHost"] = host
	doc["DatabasePort"] = port
	doc["DatabaseUser"] = c.User
	doc["DatabasePassword"] = c.Passwd
	doc["DatabaseName"] = strings.TrimPrefix(c.DBName, "/")
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// placeWeaponPaintsGamedata moves the gamedata file WeaponPaints.zip ships at
// its archive root into the location the plugin actually reads.
//
// WeaponPaints.zip has two top-level entries — WeaponPaints/ and gamedata/ —
// so extracting it into the plugins directory puts the file at
// addons/counterstrikesharp/plugins/gamedata/weaponpaints.json. Upstream
// (WeaponPaints.cs) resolves it two directories above its module directory,
// i.e. addons/counterstrikesharp/gamedata/weaponpaints.json, and calls
// Unload(false) when it is missing: the plugin used to install "successfully"
// and then disable itself on every boot, with the reason buried in the game
// server's console log.
func (in *Installer) placeWeaponPaintsGamedata() error {
	cssharp := filepath.Join(in.cfg.CSGODir(), "addons", "counterstrikesharp")
	want := filepath.Join(cssharp, "gamedata", "weaponpaints.json")
	// The archive ships the same file in two places; prefer the copy inside the
	// plugin folder, which survives even if the stray root copy is ever dropped.
	sources := []string{
		filepath.Join(cssharp, "plugins", "WeaponPaints", "gamedata", "weaponpaints.json"),
		filepath.Join(cssharp, "plugins", "gamedata", "weaponpaints.json"),
	}
	var data []byte
	for _, src := range sources {
		b, err := os.ReadFile(src)
		if err == nil {
			data = b
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("weaponpaints: read gamedata: %w", err)
		}
	}
	if data == nil {
		// Not fatal: everything else is installed and recorded (so uninstall
		// still works), but the plugin will refuse to load, and that is what
		// the operator needs to be told.
		return warnf("WeaponPaints will not load: gamedata/weaponpaints.json was not in the release archive")
	}
	if err := ensureDir(filepath.Dir(want)); err != nil {
		return err
	}
	if err := atomicWrite(want, data, 0o644); err != nil {
		return fmt.Errorf("weaponpaints: write gamedata: %w", err)
	}
	// Remove the misplaced copy so the plugins directory holds only plugins.
	stray := filepath.Join(cssharp, "plugins", "gamedata")
	if safeSubPath(in.cfg.CSGODir(), stray) {
		if err := os.RemoveAll(stray); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("weaponpaints: remove stray gamedata dir: %w", err)
		}
	}
	return nil
}
