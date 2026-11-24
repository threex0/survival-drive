// Package resolve turns manifest items into concrete, pinned download plans.
//
// Resolvers never download content: they query catalogues, parse metadata and
// return one candidate per selectable option (e.g. each Wikipedia flavour).
// The budget package then picks the candidate that fits.
package resolve

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"survivaldrive/internal/manifest"
)

// UserAgent is sent on every request.
const UserAgent = "survival-drive/0.1 (+M0 resolver)"

// Pieces holds the per-chunk hashes from a Kiwix metalink. They let the fetcher
// verify while downloading and repair a single bad 4 MiB chunk instead of
// restarting a 118 GiB transfer.
type Pieces struct {
	Size int64    `json:"size"`
	SHA1 []string `json:"sha1"`
}

// File is one concrete file to place on the drive.
type File struct {
	Dest      string `json:"dest"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256,omitempty"`
	MD5       string `json:"md5,omitempty"`
	// Descriptor is the metadata document to re-read at fetch time (a Kiwix
	// metalink). It is stable; the mirror list inside it is not, which is why
	// mirrors are deliberately not stored in the lock.
	Descriptor string  `json:"descriptor,omitempty"`
	Pieces     *Pieces `json:"pieces,omitempty"`
}

// Candidate is one selectable shape of an item.
type Candidate struct {
	Label     string `json:"label"` // e.g. "maxi"
	Files     []File `json:"files"`
	Estimated bool   `json:"estimated,omitempty"`
	// BytesEstimate carries a size when the payload is not sourced yet.
	BytesEstimate int64  `json:"bytes_estimate,omitempty"`
	Note          string `json:"note,omitempty"`
}

// Bytes is the total size of a candidate.
func (c Candidate) Bytes() int64 {
	if c.Estimated && c.BytesEstimate > 0 {
		return c.BytesEstimate
	}
	var n int64
	for _, f := range c.Files {
		n += f.SizeBytes
	}
	return n
}

// Unpinned reports files without a sha256.
func (c Candidate) Unpinned() []string {
	var out []string
	for _, f := range c.Files {
		if f.SHA256 == "" {
			out = append(out, f.Name)
		}
	}
	return out
}

// Resolver produces candidates for an item, in preference order.
type Resolver interface {
	Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error)
}

// Options configures resolver endpoints so tests can point at a local server.
type Options struct {
	KiwixBase   string
	HuggingBase string
	GitHubBase  string
	HTTP        *http.Client
	// Token is optional. GitHub rate-limits anonymous callers to 60 req/h,
	// which is enough for three assets but tight for repeated runs.
	GitHubToken string
}

// DefaultOptions returns the production endpoints.
func DefaultOptions() Options {
	return Options{
		KiwixBase:   "https://library.kiwix.org",
		HuggingBase: "https://huggingface.co",
		GitHubBase:  "https://api.github.com",
		HTTP:        &http.Client{Timeout: 120 * time.Second},
	}
}

// Registry maps resolver types to implementations.
type Registry struct {
	resolvers map[string]Resolver
}

// NewRegistry builds every resolver for the given options.
func NewRegistry(o Options) *Registry {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 120 * time.Second}
	}
	return &Registry{resolvers: map[string]Resolver{
		manifest.ResolveKiwix:    &Kiwix{BaseURL: o.KiwixBase, HTTP: o.HTTP},
		manifest.ResolveHugging:  &HuggingFace{BaseURL: o.HuggingBase, HTTP: o.HTTP},
		manifest.ResolveGitHub:   &GitHub{BaseURL: o.GitHubBase, HTTP: o.HTTP, Token: o.GitHubToken},
		manifest.ResolveStatic:   &Static{HTTP: o.HTTP},
		manifest.ResolvePMTiles:  &PMTiles{},
		manifest.ResolveEstimate: &Estimate{},
	}}
}

// Resolve dispatches to the resolver named by the item.
func (r *Registry) Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error) {
	rv, ok := r.resolvers[item.Resolver.Type]
	if !ok {
		return nil, fmt.Errorf("item %s: unknown resolver %q", item.ID, item.Resolver.Type)
	}
	cands, err := rv.Resolve(ctx, item)
	if err != nil {
		return nil, fmt.Errorf("item %s: %w", item.ID, err)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("item %s: resolver returned no candidates", item.ID)
	}
	for i := range cands {
		for j := range cands[i].Files {
			f := &cands[i].Files[j]
			if f.Name == "" {
				f.Name = path.Base(f.Dest)
			}
		}
	}
	return cands, nil
}

// destFor places a file: an item dest ending in '/' is a directory, anything
// else is an explicit file path.
func destFor(item manifest.Item, fileName string) string {
	if strings.HasSuffix(item.Dest, "/") {
		return path.Join(strings.TrimSuffix(item.Dest, "/"), fileName)
	}
	return item.Dest
}

// getXML fetches and decodes an XML document with a size cap.
func getXML(ctx context.Context, c *http.Client, url string, v any, maxBytes int64) error {
	body, err := getBytes(ctx, c, url, maxBytes)
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode xml from %s: %w", url, err)
	}
	return nil
}

// getJSON fetches and decodes a JSON document with a size cap.
func getJSON(ctx context.Context, c *http.Client, url string, v any, maxBytes int64) error {
	body, err := getBytes(ctx, c, url, maxBytes)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode json from %s: %w", url, err)
	}
	return nil
}

func unmarshalJSON(body []byte, v any, src string) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("decode json from %s: %w", src, err)
	}
	return nil
}

func getBytes(ctx context.Context, c *http.Client, url string, maxBytes int64) ([]byte, error) {
	return getBytesAuth(ctx, c, url, "", maxBytes)
}

// getBytesAuth is getBytes with an optional bearer token (GitHub rate limits).
// Transient failures (429, 5xx, connection errors) are retried with backoff:
// public mirrors do throttle, and a resolve should not fail because of it.
func getBytesAuth(ctx context.Context, c *http.Client, url, token string, maxBytes int64) ([]byte, error) {
	const attempts = 4
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * time.Second
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		body, retry, err := getOnce(ctx, c, url, token, maxBytes)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%s (after %d attempts): %w", url, attempts, lastErr)
}

// getOnce performs a single request. retry reports whether the failure looks
// transient.
func getOnce(ctx context.Context, c *http.Client, url, token string, maxBytes int64) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", UserAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("get %s: %w", url, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("get %s: unexpected status %s", url, resp.Status)
	default:
		return nil, false, fmt.Errorf("get %s: unexpected status %s", url, resp.Status)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, true, fmt.Errorf("read %s: %w", url, err)
	}
	return body, false, nil
}
