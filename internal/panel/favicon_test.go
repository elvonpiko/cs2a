package panel

import (
	"strings"
	"testing"
)

// The panel used to ship about:invalid#TemplFailedSanitizationURL as its icon
// href: templ's URL sanitizer rejects data: URIs, so every deployed panel
// showed the browser's default favicon while the landing page showed the
// mark. The icon is now a real static file.
func TestPanelFavicon(t *testing.T) {
	client, _, base := newPanelTest(t)
	body := getBody(t, client, base+"/login")
	if strings.Contains(body, "about:invalid") {
		t.Fatal("templ rejected the favicon href again")
	}
	if !strings.Contains(body, `href="/static/favicon.svg"`) {
		t.Fatalf("favicon link missing:\n%s", body[:min(600, len(body))])
	}
	resp := get(t, client, base+"/static/favicon.svg")
	if resp.StatusCode != 200 {
		t.Fatalf("favicon.svg: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("favicon content-type = %q", ct)
	}
}
