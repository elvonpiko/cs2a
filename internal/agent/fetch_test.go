package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
