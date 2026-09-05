package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Config is the panel's on-disk configuration (/etc/cs2a/panel.json).
type Config struct {
	Listen     string `json:"listen"` // web bind, e.g. 0.0.0.0:8080
	AgentURL   string `json:"agent_url"`
	AgentToken string `json:"agent_token"`
	DBPath     string `json:"db_path"`
	// SetupTokenFile is the path of the one-time first-admin token. When the
	// file exists and no users exist, /setup accepts it.
	SetupTokenFile string `json:"setup_token_file,omitempty"`
	// PublicURL is the address users reach the panel on (e.g.
	// https://panel.example.com). Set it when a reverse proxy fronts the panel
	// so cross-origin checks and secure cookies behave correctly.
	PublicURL string `json:"public_url,omitempty"`
}

// Defaults matching the bootstrap installer.
const (
	DefaultListen       = "0.0.0.0:8080"
	DefaultSetupTokenFp = "/etc/cs2a/panel-setup-token"
)

// DefaultConfig returns bootstrap-equivalent defaults.
func DefaultConfig() Config {
	return Config{
		Listen:         DefaultListen,
		AgentURL:       "http://127.0.0.1:8100",
		DBPath:         "/var/lib/cs2a/panel.db",
		SetupTokenFile: DefaultSetupTokenFp,
	}
}

// LoadConfig reads panel config; env overrides (CS2A_PANEL_AGENT_TOKEN, ...).
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("panel: read config: %w", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("panel: parse config %s: %w", path, err)
		}
	}
	if v := os.Getenv("CS2A_PANEL_AGENT_TOKEN"); v != "" {
		cfg.AgentToken = v
	}
	if v := os.Getenv("CS2A_PANEL_AGENT_URL"); v != "" {
		cfg.AgentURL = v
	}
	if v := os.Getenv("CS2A_PANEL_DB"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("CS2A_PANEL_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// TrustedOrigins is the list of origins allowed to submit forms to the panel
// in addition to its own. Only the configured public URL qualifies.
func (c *Config) TrustedOrigins() []string {
	if c.PublicURL == "" {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(c.PublicURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return []string{u.Scheme + "://" + u.Host}
}

// SecureCookies reports whether session cookies may carry the Secure flag
// (only when the panel is served over HTTPS, otherwise login would break).
func (c *Config) SecureCookies() bool {
	u, err := url.Parse(strings.TrimSpace(c.PublicURL))
	return err == nil && u.Scheme == "https"
}

// Validate checks required fields.
func (c *Config) Validate() error {
	var errs []error
	if c.AgentToken == "" {
		errs = append(errs, errors.New("agent_token is required"))
	}
	if c.AgentURL == "" {
		errs = append(errs, errors.New("agent_url is required"))
	}
	if strings.Contains(c.Listen, "..") {
		errs = append(errs, errors.New("invalid listen address"))
	}
	return errors.Join(errs...)
}

// SetupToken reads the one-time setup token, if the file exists.
func (c *Config) SetupToken() (string, bool) {
	if c.SetupTokenFile == "" {
		return "", false
	}
	b, err := os.ReadFile(c.SetupTokenFile)
	if err != nil {
		return "", false
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", false
	}
	return tok, true
}

// ConsumeSetupToken deletes the setup token file (first admin created).
func (c *Config) ConsumeSetupToken() {
	if c.SetupTokenFile == "" {
		return
	}
	_ = os.Remove(c.SetupTokenFile)
	_ = os.Remove(dirOfFile(c.SetupTokenFile) + "/panel-setup-token.pub")
}

func dirOfFile(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "."
	}
	return p[:i]
}
