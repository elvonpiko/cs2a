package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain removes the retry backoff and the unit settle window for the whole
// package: several tests deliberately point the installer at an offline or
// failing transport, and the production delays would otherwise add seconds of
// sleeping per test.
func TestMain(m *testing.M) {
	fetchBackoff = 0
	settleWindow = 20 * time.Millisecond
	settlePoll = 5 * time.Millisecond
	// Several tests point the agent at an unroutable address on purpose; the
	// production probe timeout would spend seconds waiting for each one.
	rconProbeTimeout = 150 * time.Millisecond
	os.Exit(m.Run())
}

// A body that dies mid-response is the exact failure the VPS hit ("unexpected
// EOF" reading a 35-byte pointer file). One blip must not fail the install.
func TestHTTPGetRetriesTruncatedBody(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Length", "20")
		if n == 1 {
			// Announce 20 bytes, send 5, hang up: net/http reports
			// io.ErrUnexpectedEOF to the body reader.
			io.WriteString(w, "short")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler) // kill the connection
		}
		io.WriteString(w, "12345678901234567890")
	}))
	defer srv.Close()

	var got string
	err := httpGet(context.Background(), srv.Client(), srv.URL, nil, func(resp *http.Response) error {
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		got = string(b)
		return nil
	})
	if err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	if got != "12345678901234567890" {
		t.Fatalf("body = %q", got)
	}
	if hits < 2 {
		t.Fatalf("expected a retry, got %d request(s)", hits)
	}
}

// A 404 is a definitive answer: retrying only delays the error the operator
// needs to see.
func TestHTTPGetDoesNotRetryNotFound(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	err := httpGet(context.Background(), srv.Client(), srv.URL, nil, func(*http.Response) error {
		t.Error("sink must not run for a non-200 response")
		return nil
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	var se *statusErr
	if !errors.As(err, &se) || se.code != http.StatusNotFound {
		t.Fatalf("err = %v", err)
	}
	if hits != 1 {
		t.Fatalf("404 was retried %d times", hits)
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("unhelpful error text: %v", err)
	}
}

// 5xx and 429 are transient by definition.
func TestHTTPGetRetriesServerError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	if err := httpGet(context.Background(), srv.Client(), srv.URL, nil, func(*http.Response) error {
		return nil
	}); err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want 2", hits)
	}
}

// A truncated download must not be written to disk as a valid-looking archive:
// Content-Length is the only signal available before extraction.
func TestCopyCappedDetectsShortBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "only-ten!!")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if _, err := copyCapped(io.Discard, resp, maxFileBytes); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF", err)
	}
}

// The pointer file is the root of every install. When its primary host is
// broken, the official mirrors must carry the install through — and the
// resolved artifact URL must follow the mirror that answered.
func TestResolvePointerFallsBackToMirror(t *testing.T) {
	var primaryHits int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.Header().Set("Content-Length", "35")
		io.WriteString(w, "mms")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer broken.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, mmArtifact+"\n")
	}))
	defer mirror.Close()

	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	entry := CatalogEntry{
		ID:           "metamod",
		URL:          broken.URL + "/mmsdrop/2.0/mmsource-latest-linux",
		URLMirrors:   []string{mirror.URL + "/mmsdrop/2.0/mmsource-latest-linux"},
		URLIsPointer: true,
	}
	name, urls, version, err := in.resolveArtifact(context.Background(), entry)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if name != mmArtifact || version != "2.0.0-git1411" {
		t.Fatalf("name=%q version=%q", name, version)
	}
	if len(urls) != 2 {
		t.Fatalf("want the artifact on both hosts, got %v", urls)
	}
	// The host that actually answered is tried first for the tarball too.
	if !strings.HasPrefix(urls[0], mirror.URL) {
		t.Fatalf("first url = %q, want the working mirror", urls[0])
	}
	if primaryHits < 2 {
		t.Fatalf("primary was not retried before failing over (%d hits)", primaryHits)
	}
}

// When every host is down the error must name the component and the hosts
// tried, not leak a bare "unexpected EOF".
func TestResolvePointerReportsAllFailures(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
	defer dead.Close()

	cfg := testConfig(t)
	store, _ := OpenStore(cfg.DBPath)
	defer store.Close()
	in := NewInstaller(cfg, store, DefaultCatalog(), nil)

	_, _, _, err := in.resolveArtifact(context.Background(), CatalogEntry{
		ID:           "metamod",
		URL:          dead.URL + "/mmsdrop/2.0/mmsource-latest-linux",
		URLIsPointer: true,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"metamod", "latest-build pointer", "HTTP 503"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q is missing %q", msg, want)
		}
	}
}

// Nested dependency failures used to read
// "plugins: dependency cssharp: plugins: dependency metamod: plugins: metamod:
// …". Only the component that actually failed should be named.
func TestDependencyErrorNamesTheRootCause(t *testing.T) {
	root := errors.New("could not read the latest-build pointer")
	inner := depError("Metamod:Source", root)
	outer := depError("CounterStrikeSharp", inner)

	if !errors.Is(outer, root) {
		t.Fatal("root cause lost")
	}
	msg := outer.Error()
	if strings.Contains(msg, "CounterStrikeSharp") {
		t.Fatalf("intermediate dependency should not be named: %s", msg)
	}
	if !strings.Contains(msg, "Metamod:Source is required first") {
		t.Fatalf("error text = %q", msg)
	}
	if strings.Count(msg, "required first") != 1 {
		t.Fatalf("nested prefixes not collapsed: %s", msg)
	}
}

// The final retry attempt must drop the post-quantum key share
// (X25519MLKEM768) from the TLS handshake. Go 1.24+ advertises it by default,
// which grows the ClientHello past 1.5 KB; path middleboxes that cannot relay
// the oversized hello silently drop it, and the download dies with a bare
// timeout. This server records the curve list of every ClientHello and fails
// the first requests, so the loop is driven to its last attempt; the last
// hello must offer no hybrid group, while an earlier one must have it —
// proving the test can tell the two clients apart.
func TestFinalAttemptDropsPostQuantumKeyShare(t *testing.T) {
	var mu sync.Mutex
	var hellos [][]tls.CurveID
	var hits int32

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) < fetchAttempts {
			w.WriteHeader(http.StatusInternalServerError) // retryable
			return
		}
		io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS12, // h1 only: no h2 negotiation to lean on
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			hellos = append(hellos, append([]tls.CurveID(nil), hello.SupportedCurves...))
			mu.Unlock()
			return nil, nil // proceed with the server config above
		},
	}
	srv.StartTLS()
	defer srv.Close()

	err := httpGet(context.Background(), srv.Client(), srv.URL, nil, func(*http.Response) error {
		return nil
	})
	if err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	if int(hits) != fetchAttempts {
		t.Fatalf("hits = %d, want %d (loop must reach the final attempt)", hits, fetchAttempts)
	}

	hasHybrid := func(list []tls.CurveID) bool {
		for _, c := range list {
			if c == tls.X25519MLKEM768 {
				return true
			}
		}
		return false
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hellos) == 0 {
		t.Fatal("no TLS handshakes recorded")
	}
	// Sensitivity: the ordinary attempts must have offered the hybrid. If a
	// future Go removes it from the defaults, that is fine — but then this
	// test stops proving anything and should be revisited, so fail loudly.
	if !hasHybrid(hellos[0]) {
		t.Skip("default client no longer offers X25519MLKEM768; the workaround is moot in this Go version")
	}
	last := hellos[len(hellos)-1]
	if hasHybrid(last) {
		t.Fatal("final attempt still offered X25519MLKEM768 — the compat downgrade did not apply")
	}
}

// compatCurvePreferences must never grow the hybrid back: it is the whole
// point of the downgrade, and a future edit to "modernise" the list would
// silently reintroduce the middlebox failure on the last retry.
func TestCompatCurveListHasNoHybrid(t *testing.T) {
	for _, c := range compatCurvePreferences {
		if c == tls.X25519MLKEM768 {
			t.Fatal("compat curve list must not contain X25519MLKEM768")
		}
	}
	if len(compatCurvePreferences) == 0 {
		t.Fatal("compat curve list is empty")
	}
}
