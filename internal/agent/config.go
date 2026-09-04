// Package agent implements the cs2a agent runtime: a small loopback HTTP API
// that executes server operations (service control, RCON commands, config and
// plugin management) against a local CS2 dedicated server.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the agent's on-disk configuration (/etc/cs2a/agent.json).
type Config struct {
	Listen       string `json:"listen"`        // loopback API bind, e.g. 127.0.0.1:8100
	Token        string `json:"token"`         // bearer token the panel must present
	CS2Dir       string `json:"cs2_dir"`       // CS2 install root (contains game/)
	ServiceName  string `json:"service_name"`  // systemd unit running the game server
	RCONAddr     string `json:"rcon_addr"`     // 127.0.0.1:27015
	RCONPassword string `json:"rcon_password"` // rcon_password of the game server
	A2SAddr      string `json:"a2s_addr"`      // 127.0.0.1:27015
	DBPath       string `json:"db_path"`       // agent sqlite db
	PluginCache  string `json:"plugin_cache"`  // download cache dir
}

// Defaults matching the bootstrap installer.
const (
	DefaultListen      = "127.0.0.1:8100"
	DefaultServiceName = "cs2-server"
	DefaultDBPath      = "/var/lib/cs2a/agent.db"
	DefaultCachePath   = "/var/cache/cs2a/plugins"
)

// DefaultConfig returns a config with bootstrap-equivalent defaults.
func DefaultConfig() Config {
	return Config{
		Listen:      DefaultListen,
		ServiceName: DefaultServiceName,
		RCONAddr:    "127.0.0.1:27015",
		A2SAddr:     "127.0.0.1:27015",
		DBPath:      DefaultDBPath,
		PluginCache: DefaultCachePath,
	}
}

// LoadConfig reads a JSON config file. Environment variables override file
// values (CS2A_AGENT_TOKEN, CS2A_AGENT_CS2_DIR, CS2A_AGENT_RCON_PASSWORD).
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("agent: read config: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("agent: parse config %s: %w", path, err)
		}
	}
	if v := os.Getenv("CS2A_AGENT_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("CS2A_AGENT_CS2_DIR"); v != "" {
		cfg.CS2Dir = v
	}
	if v := os.Getenv("CS2A_AGENT_RCON_PASSWORD"); v != "" {
		cfg.RCONPassword = v
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// Validate checks the minimal set of required fields.
func (c *Config) Validate() error {
	var errs []error
	if c.Token == "" {
		errs = append(errs, errors.New("token is required"))
	}
	if c.CS2Dir == "" {
		errs = append(errs, errors.New("cs2_dir is required"))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// CSGODir is the csgo content root (cfg/, maps/, addons/ live under it).
func (c *Config) CSGODir() string {
	return filepath.Join(c.CS2Dir, "game", "csgo")
}

// CFGDir is the server config directory containing server.cfg.
func (c *Config) CFGDir() string {
	return filepath.Join(c.CSGODir(), "cfg")
}
