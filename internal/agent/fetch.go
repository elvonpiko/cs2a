package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// The agent downloads from upstreams it does not control — GitHub's API and
// object store, and the AlliedModders drop behind Cloudflare. Those hosts
// intermittently kill a connection mid-response, which the standard library
// surfaces as "unexpected EOF" from the body read. A single such blip used to
// abort a whole plugin install *and* every install that depended on it, so
// every outbound GET goes through httpGet: bounded, retried, and downgraded to
// HTTP/1.1 on the last attempt.
const fetchAttempts = 4

// fetchBackoff is the base delay between attempts (multiplied by the attempt
// number). Tests set it to zero so an intentionally offline transport does not
// make the suite sleep.
var fetchBackoff = 750 * time.Millisecond

// statusErr is a non-200 HTTP response. Keeping it typed lets the retry logic
// tell "the server said no" (404: stop) from "the server is unwell"
// (429/5xx: worth another try).
type statusErr struct {
	code int
	url  string
}

func (e *statusErr) Error() string {
	return fmt.Sprintf("HTTP %d %s from %s", e.code, http.StatusText(e.code), hostOf(e.url))
}

func (e *statusErr) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}

// asStatus is errors.As for *statusErr, kept here so callers do not need to
// import errors just to inspect an HTTP status.
func asStatus(err error, target **statusErr) bool {
	return errors.As(err, target)
}

// hostOf returns just the host of a URL for error text (full asset URLs are
// long and add nothing an operator can act on).
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// newTransport builds the transport shared by every agent HTTP client. The
// response-header timeout matters most: without it a stalled upstream would
// hold the whole 15-minute download budget before the first retry.
func newTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 15 * time.Second
	tr.ResponseHeaderTimeout = 45 * time.Second
	return tr
}

// http11Client clones a client with HTTP/2 disabled. Cloudflare-fronted hosts
// (mms.alliedmods.net) sometimes reset an h2 stream mid-body while plain
// HTTP/1.1 completes fine, so the final attempt drops to h1 rather than
// failing an install for a protocol-level hiccup.
func http11Client(base *http.Client) *http.Client {
	tr, ok := base.Transport.(*http.Transport)
	if !ok {
		if base.Transport != nil {
			return base // custom RoundTripper (tests): nothing to downgrade
		}
		tr = newTransport()
	}
	h1 := tr.Clone()
	h1.ForceAttemptHTTP2 = false
	// A non-nil (even empty) TLSNextProto is what disables h2 in net/http.
	h1.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if h1.TLSClientConfig == nil {
		h1.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		h1.TLSClientConfig = h1.TLSClientConfig.Clone()
	}
	h1.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{Transport: h1, Timeout: base.Timeout, CheckRedirect: base.CheckRedirect}
}

// retryable reports whether another attempt could plausibly succeed. Local
// filesystem failures and definitive HTTP rejections are permanent; anything
// transport-shaped (truncated body, reset connection, refused dial, DNS blip)
// is worth retrying.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	var se *statusErr
	if errors.As(err, &se) {
		return se.retryable()
	}
	// The caller's deadline or cancellation will not heal by waiting.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A write into the plugin cache that failed (no space, permissions) fails
	// again identically.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return false
	}
	return true
}

// httpGet performs a GET and hands a 200 response to sink, retrying transient
// failures. sink may be called more than once and must reset any partial
// output it produced.
func httpGet(ctx context.Context, client *http.Client, url string, headers map[string]string, sink func(*http.Response) error) error {
	if client == nil {
		client = newDownloadClient()
	}
	var lastErr error
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		use := client
		if attempt == fetchAttempts {
			use = http11Client(client)
		}
		err := httpGetOnce(ctx, use, url, headers, sink)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retryable(err) {
			break
		}
		if ctx.Err() != nil {
			break
		}
		if fetchBackoff > 0 {
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(time.Duration(attempt) * fetchBackoff):
			}
		}
	}
	return lastErr
}

func httpGetOnce(ctx context.Context, client *http.Client, url string, headers map[string]string, sink func(*http.Response) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	// Some CDNs answer differently (or not at all) without an Accept header.
	req.Header.Set("Accept", "*/*")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// Drain a little so a keep-alive connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return &statusErr{code: resp.StatusCode, url: url}
	}
	return sink(resp)
}

// userAgent identifies the agent to upstreams. GitHub requires one, and a
// descriptive value makes cs2a traffic recognisable in upstream logs.
const userAgent = "cs2a-agent (+https://github.com/elvonpiko/cs2a)"

// copyCapped streams src into dst, enforcing a byte cap, and verifies the
// transfer against Content-Length when the server supplied one. Without that
// check a body truncated exactly at a chunk boundary would be written to disk
// as a valid-looking but corrupt archive.
func copyCapped(dst io.Writer, resp *http.Response, max int64) (int64, error) {
	n, err := io.Copy(dst, io.LimitReader(resp.Body, max+1))
	if err != nil {
		return n, err
	}
	if n > max {
		return n, fmt.Errorf("response exceeds %d byte cap", max)
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return n, fmt.Errorf("%w: got %d of %d bytes", io.ErrUnexpectedEOF, n, resp.ContentLength)
	}
	return n, nil
}
