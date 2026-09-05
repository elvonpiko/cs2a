package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cs2a/internal/cs2"
)

// Values and comments used to ride through the settings API unvalidated and were
// only sanitized at render time, so a rejected value was silently rewritten
// instead of refused — the panel then displayed a setting the file did not
// contain. A newline is the dangerous case: it turns one setting into two live
// config statements.
func TestAPIPutSettingsRejectsInjection(t *testing.T) {
	client, _, base, cfg := newTestAPI(t)
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	bad := []struct {
		name     string
		settings []map[string]any
	}{
		{
			name: "newline in a value",
			settings: []map[string]any{
				{"name": "hostname", "value": "mine\nrcon_password \"leaked\""},
			},
		},
		{
			name: "newline in a comment",
			settings: []map[string]any{
				{"name": "hostname", "value": "mine", "comment": "note\nrcon_password \"leaked\""},
			},
		},
		{
			name: "control character in a value",
			settings: []map[string]any{
				{"name": "hostname", "value": "mi\x00ne"},
			},
		},
		{
			name: "invalid cvar name",
			settings: []map[string]any{
				{"name": "hostname; rm -rf /", "value": "x"},
			},
		},
		{
			name: "duplicate cvar",
			settings: []map[string]any{
				{"name": "mp_maxrounds", "value": "16"},
				{"name": "mp_maxrounds", "value": "24"},
			},
		},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := doJSON(t, client, "PUT", base, "/api/v1/settings",
				map[string]any{"settings": tc.settings})
			if resp.StatusCode != 400 {
				t.Fatalf("status = %d %v, want 400", resp.StatusCode, out)
			}
			raw, err := os.ReadFile(filepath.Join(cfg.CFGDir(), "server.cfg"))
			if err == nil && strings.Contains(string(raw), "leaked") {
				t.Fatalf("a rejected setting reached server.cfg:\n%s", raw)
			}
		})
	}

	// A long value is rejected rather than truncated.
	resp, _ := doJSON(t, client, "PUT", base, "/api/v1/settings", map[string]any{
		"settings": []map[string]any{{"name": "hostname", "value": strings.Repeat("x", 2000)}},
	})
	if resp.StatusCode != 400 {
		t.Fatalf("oversized value: %d", resp.StatusCode)
	}

	// The good case still works, and round-trips.
	resp, out := doJSON(t, client, "PUT", base, "/api/v1/settings", map[string]any{
		"settings": []map[string]any{
			{"name": "mp_maxrounds", "value": "24", "comment": "managed by cs2a"},
			{"name": "hostname", "value": "my server"},
		},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("valid settings rejected: %d %v", resp.StatusCode, out)
	}
	resp, out = doJSON(t, client, "GET", base, "/api/v1/settings", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d %v", resp.StatusCode, out)
	}
	list, _ := out["settings"].([]any)
	if len(list) != 2 {
		t.Fatalf("settings = %v", out)
	}
}

// A duplicate managed block means the panel is not showing what the server runs:
// cs2a writes the first block and the engine lets the second one win. The API has
// to say so, or a save appears to do nothing for no visible reason.
func TestAPISettingsReportsDuplicateBlock(t *testing.T) {
	client, _, base, cfg := newTestAPI(t)
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	body := cs2.ManagedBlockBegin + "\nmp_maxrounds \"24\"\n" + cs2.ManagedBlockEnd + "\n" +
		"\nhostname \"operator\"\n\n" +
		cs2.ManagedBlockBegin + "\nmp_maxrounds \"16\"\n" + cs2.ManagedBlockEnd + "\n"
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, out := doJSON(t, client, "GET", base, "/api/v1/settings", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get settings: %d %v", resp.StatusCode, out)
	}
	warn, _ := out["warning"].(string)
	if warn == "" {
		t.Fatal("a duplicate managed block must be reported")
	}
	if !strings.Contains(warn, "server.cfg") {
		t.Errorf("warning = %q", warn)
	}

	// A save still succeeds — and still reports the conflict.
	resp, out = doJSON(t, client, "PUT", base, "/api/v1/settings", map[string]any{
		"settings": []map[string]any{{"name": "mp_maxrounds", "value": "30"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("put settings: %d %v", resp.StatusCode, out)
	}
	if w, _ := out["warning"].(string); w == "" {
		t.Error("the save response must carry the same warning")
	}
	raw, err := os.ReadFile(filepath.Join(cfg.CFGDir(), "server.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	// The operator's own lines and the second block survive untouched.
	for _, want := range []string{`hostname "operator"`, `mp_maxrounds "16"`, `mp_maxrounds "30"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q:\n%s", want, raw)
		}
	}
}

// A bare cvar line inside the managed block is a query the engine answers by
// printing the value. It must survive a settings round trip: rewriting it as
// `mp_autokick ""` sets the cvar to 0 and pushes that live over RCON.
func TestAPISettingsRoundTripsABareCvar(t *testing.T) {
	client, _, base, cfg := newTestAPI(t)
	if err := os.MkdirAll(cfg.CFGDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	body := cs2.ManagedBlockBegin + "\nmp_autokick\nsv_cheats \"0\"\n" + cs2.ManagedBlockEnd + "\n"
	if err := os.WriteFile(filepath.Join(cfg.CFGDir(), "server.cfg"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, out := doJSON(t, client, "GET", base, "/api/v1/settings", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d %v", resp.StatusCode, out)
	}
	list, _ := out["settings"].([]any)
	if len(list) != 2 {
		t.Fatalf("settings = %v", out)
	}
	first, _ := list[0].(map[string]any)
	if first["name"] != "mp_autokick" || first["bare"] != true {
		t.Fatalf("bare cvar not flagged: %v", first)
	}

	// Write exactly what was read back.
	resp, out = doJSON(t, client, "PUT", base, "/api/v1/settings", map[string]any{"settings": list})
	if resp.StatusCode != 200 {
		t.Fatalf("put: %d %v", resp.StatusCode, out)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.CFGDir(), "server.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `mp_autokick ""`) {
		t.Fatalf("the bare cvar became an assignment to 0:\n%s", raw)
	}
	if !strings.Contains(string(raw), "\nmp_autokick\n") {
		t.Fatalf("the bare cvar was lost:\n%s", raw)
	}
}
