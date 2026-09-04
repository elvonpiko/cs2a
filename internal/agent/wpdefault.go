package agent

import (
	"os"
	"path/filepath"
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
func (in *Installer) writeWeaponPaintsDefaultConfig() error {
	path := filepath.Join(in.cfg.CSGODir(),
		"addons", "counterstrikesharp", "configs", "plugins", "WeaponPaints", "WeaponPaints.json")
	if _, err := os.Stat(path); err == nil {
		return nil // never clobber an existing admin-edited config
	}
	if err := ensureDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := atomicWrite(path, []byte(weaponPaintsDefaultConfig), 0o644); err != nil {
		return err
	}
	return nil
}
