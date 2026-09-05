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
		HTTP:  &http.Client{Timeout: 30 * time.Second, Transport: newTransport()},
		Token: token,
	}
}

// LatestRelease returns the latest published release of a repo.
func (g *GHClient) LatestRelease(ctx context.Context, repo string) (*GHRelease, error) {
	return g.release(ctx, repo, "latest")
}

func (g *GHClient) release(ctx context.Context, repo, ref string) (*GHRelease, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/" + ref
	headers := map[string]string{"Accept": "application/vnd.github+json"}
	if g.Token != "" {
		headers["Authorization"] = "Bearer " + g.Token
	}
	var rel GHRelease
	// The API is retried on transport errors and 5xx/429 like every other
	// outbound call: a rate-limit blip or a dropped connection should not fail
	// an install that a second attempt completes.
	err := httpGet(ctx, g.HTTP, url, headers, func(resp *http.Response) error {
		rel = GHRelease{}
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel)
	})
	if err != nil {
		return nil, ghError(repo, err)
	}
	return &rel, nil
}

// ghError turns transport and status failures into text an operator can act on.
func ghError(repo string, err error) error {
	var se *statusErr
	if asStatus(err, &se) {
		switch se.code {
		case http.StatusNotFound:
			return fmt.Errorf("github: %s has no published release", repo)
		case http.StatusForbidden, http.StatusTooManyRequests:
			// Unauthenticated GitHub API calls share a 60/hour/IP budget, which
			// a few installs on a busy host can exhaust.
			return fmt.Errorf("github: rate limited while reading %s — set GITHUB_TOKEN in the agent config to raise the limit, or retry in an hour", repo)
		}
	}
	return fmt.Errorf("github: could not read the latest release of %s: %w", repo, err)
}
