package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GHAsset is one release artifact.
type GHAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// GHRelease is the subset of a GitHub release the installer needs.
type GHRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []GHAsset `json:"assets"`
}

// GHClient fetches release metadata from the GitHub API.
type GHClient struct {
	HTTP  *http.Client
	Token string // optional GITHUB_TOKEN for higher rate limits
}

// NewGHClient builds a client with sane timeouts.
func NewGHClient(token string) *GHClient {
	return &GHClient{
		HTTP:  &http.Client{Timeout: 20 * time.Second},
		Token: token,
	}
}

// LatestRelease returns the latest published release of a repo.
func (g *GHClient) LatestRelease(ctx context.Context, repo string) (*GHRelease, error) {
	return g.release(ctx, repo, "latest")
}

// ReleaseByTag returns a specific release by tag.
func (g *GHClient) ReleaseByTag(ctx context.Context, repo, tag string) (*GHRelease, error) {
	return g.release(ctx, repo, "tags/"+tag)
}

func (g *GHClient) release(ctx context.Context, repo, ref string) (*GHRelease, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/" + ref
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "cs2a-agent")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// fallthrough
	case http.StatusNotFound:
		return nil, fmt.Errorf("github: release not found for %s", repo)
	case http.StatusForbidden:
		return nil, fmt.Errorf("github: rate limited or forbidden (consider GITHUB_TOKEN)")
	default:
		return nil, fmt.Errorf("github: unexpected status %d for %s", resp.StatusCode, repo)
	}
	var rel GHRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("github: decode release: %w", err)
	}
	return &rel, nil
}

// Download streams a release asset to w with a hard size cap.
func (g *GHClient) Download(ctx context.Context, assetURL string, w io.Writer, maxBytes int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "cs2a-agent")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("github: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github: download status %d", resp.StatusCode)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("github: download: %w", err)
	}
	if n > maxBytes {
		return fmt.Errorf("github: asset exceeds %d byte cap", maxBytes)
	}
	return nil
}
