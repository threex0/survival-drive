package resolve

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"survivaldrive/internal/manifest"
)

// Static resolves a fixed URL: government PDFs and the Kiwix tools archive.
// A pinned hash may be supplied in the manifest; otherwise an md5 sidecar is
// used when available, and the entry is reported as unpinned until first fetch.
type Static struct {
	HTTP *http.Client
}

// Resolve implements Resolver.
func (s *Static) Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error) {
	raw := item.Resolver.URL

	// A versioned URL with a pinned size and digest is immutable: answer from the
	// pins and never touch the network. Kiwix's download host throttles hard.
	if item.Resolver.SizeBytes > 0 && (item.Resolver.SHA256 != "" || item.Resolver.MD5 != "") {
		name := path.Base(raw)
		return []Candidate{{Label: "pinned", Files: []File{{
			Name:      name,
			Dest:      destFor(item, name),
			SizeBytes: item.Resolver.SizeBytes,
			URL:       raw,
			SHA256:    strings.ToLower(item.Resolver.SHA256),
			MD5:       strings.ToLower(item.Resolver.MD5),
		}}}}, nil
	}

	size, finalURL, err := s.probeWithRetry(ctx, raw)
	if err != nil {
		return nil, err
	}
	name := path.Base(finalURL)
	if name == "" || name == "/" || name == "." {
		name = path.Base(raw)
	}
	f := File{
		Name:      name,
		Dest:      destFor(item, name),
		SizeBytes: size,
		URL:       finalURL,
		SHA256:    strings.ToLower(item.Resolver.SHA256),
		MD5:       strings.ToLower(item.Resolver.MD5),
	}
	if f.MD5 == "" && item.Resolver.MD5URL != "" {
		body, err := getBytes(ctx, s.HTTP, item.Resolver.MD5URL, 4096)
		if err != nil {
			return nil, fmt.Errorf("md5 sidecar: %w", err)
		}
		f.MD5 = parseMD5(string(body))
	}
	return []Candidate{{Label: "static", Files: []File{f}}}, nil
}

// probeWithRetry retries transient failures. Kiwix's download host throttles
// aggressively, and a resolve should not fail because of it.
func (s *Static) probeWithRetry(ctx context.Context, raw string) (int64, string, error) {
	const attempts = 4
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 2 * time.Second
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return 0, "", ctx.Err()
			}
		}
		size, finalURL, err := s.probe(ctx, raw)
		if err == nil {
			return size, finalURL, nil
		}
		lastErr = err
		if !isTransient(err) {
			return 0, "", err
		}
	}
	return 0, "", fmt.Errorf("%w (after %d attempts)", lastErr, attempts)
}

// isTransient reports whether an HTTP failure is worth retrying.
func isTransient(err error) bool {
	msg := err.Error()
	for _, s := range []string{"429", "500", "502", "503", "504", "Too Many Requests",
		"connection", "timeout", "EOF"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// probe determines the size and the post-redirect URL. HEAD is tried first;
// servers that reject it fall back to a one-byte ranged GET.
func (s *Static) probe(ctx context.Context, raw string) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, raw, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := s.HTTP.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
			return resp.ContentLength, resp.Request.URL.String(), nil
		}
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Range", "bytes=0-0")
	resp, err = s.HTTP.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("probe %s: %w", raw, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("probe %s: unexpected status %s", raw, resp.Status)
	}
	if total, ok := contentRangeTotal(resp.Header.Get("Content-Range")); ok {
		return total, resp.Request.URL.String(), nil
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength, resp.Request.URL.String(), nil
	}
	return 0, "", fmt.Errorf("probe %s: no usable size advertised", raw)
}

// contentRangeTotal parses "bytes 0-0/12345".
func contentRangeTotal(v string) (int64, bool) {
	_, after, ok := strings.Cut(v, "/")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(after), 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// parseMD5 pulls the first 32-hex-digit token out of a sidecar file, which may
// be "abc123...  filename" or just the digest.
func parseMD5(s string) string {
	isHex := func(b byte) bool {
		return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
	}
	fields := strings.FieldsFunc(s, func(r rune) bool { return !isHex(byte(r)) })
	for _, f := range fields {
		if len(f) == 32 {
			return strings.ToLower(f)
		}
	}
	return ""
}

// PMTiles records the offline-map extract recipe. The real payload is produced
// in M1 by `pmtiles extract` against a daily planet build using HTTP range
// requests, so the full planet is never downloaded.
type PMTiles struct{}

// Resolve implements Resolver.
func (p *PMTiles) Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error) {
	r := item.Resolver
	name := path.Base(strings.TrimSuffix(item.Dest, "/"))
	if !strings.HasSuffix(name, ".pmtiles") {
		name = "basemap.pmtiles"
	}
	return []Candidate{{
		Label:     "extract",
		Estimated: true,
		Note:      fmt.Sprintf("pmtiles extract bbox=%s planet=%s", r.BBox, r.PlanetDate),
		Files: []File{{
			Name:      name,
			Dest:      destFor(item, name),
			SizeBytes: r.EstimateBytes,
			URL:       fmt.Sprintf("https://build.protomaps.com/%s.pmtiles (bbox extract)", r.PlanetDate),
		}},
	}}, nil
}
