package resolve

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"survivaldrive/internal/manifest"
)

// GitHub resolves release assets. Two behaviours matter:
//
//   - llama.cpp's tagged "latest" release may carry no Windows assets at all
//     (the nightlies publish those), so the resolver scans releases until one
//     actually contains every requested asset.
//   - Modern release assets expose a "sha256:" digest, which gives us a pinned
//     hash without downloading.
type GitHub struct {
	BaseURL string
	HTTP    *http.Client
	Token   string
}

const githubMaxBytes = 16 << 20

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
	// Digest is "sha256:<hex>" when GitHub has computed one.
	Digest string `json:"digest"`
}

// Resolve implements Resolver.
func (g *GitHub) Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error) {
	repo := item.Resolver.Repo
	if repo == "" {
		return nil, fmt.Errorf("github resolver needs repo")
	}
	patterns := item.Resolver.Assets
	res := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("bad asset pattern %q: %w", p, err)
		}
		res[i] = re
	}

	releases, err := g.releases(ctx, repo)
	if err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("repo %s: no releases", repo)
	}

	var (
		matched *ghRelease
		picked  []ghAsset
	)
	// llama.cpp publishes its Windows builds as nightly *prereleases*, so a
	// stable-only search finds nothing. Prefer a stable release, then fall back
	// to prereleases rather than failing.
	for _, allowPrerelease := range []bool{false, true} {
		for i := range releases {
			rel := &releases[i]
			if rel.Draft || (rel.Prerelease && !allowPrerelease) {
				continue
			}
			if item.Resolver.Tag != "" && rel.TagName != item.Resolver.Tag {
				continue
			}
			assets, ok := matchAll(rel.Assets, res)
			if !ok {
				continue
			}
			matched, picked = rel, assets
			break
		}
		if matched != nil {
			break
		}
	}
	if matched == nil {
		if item.Resolver.Tag != "" {
			return nil, fmt.Errorf("repo %s: release %s has no assets matching %v",
				repo, item.Resolver.Tag, patterns)
		}
		return nil, fmt.Errorf("repo %s: no release among the last %d contains all of %v",
			repo, len(releases), patterns)
	}

	files := make([]File, 0, len(picked))
	for _, a := range picked {
		f := File{
			Name:      a.Name,
			Dest:      destFor(item, a.Name),
			SizeBytes: a.Size,
			URL:       a.URL,
		}
		if hex, ok := strings.CutPrefix(a.Digest, "sha256:"); ok && len(hex) == 64 {
			f.SHA256 = strings.ToLower(hex)
		}
		files = append(files, f)
	}
	c := Candidate{Label: matched.TagName, Files: files}
	if matched.Prerelease {
		c.Note = "prerelease (nightly) build"
	}
	return []Candidate{c}, nil
}

func (g *GitHub) releases(ctx context.Context, repo string) ([]ghRelease, error) {
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=20", g.BaseURL, repo)
	body, err := getBytesAuth(ctx, g.HTTP, u, g.Token, githubMaxBytes)
	if err != nil {
		return nil, err
	}
	var out []ghRelease
	if err := unmarshalJSON(body, &out, u); err != nil {
		return nil, err
	}
	return out, nil
}

// matchAll returns one asset per pattern, in pattern order. Every pattern must
// match, otherwise the release is unusable (we will not ship a partial toolkit).
func matchAll(assets []ghAsset, res []*regexp.Regexp) ([]ghAsset, bool) {
	out := make([]ghAsset, 0, len(res))
	for _, re := range res {
		var found *ghAsset
		for i := range assets {
			if re.MatchString(assets[i].Name) {
				found = &assets[i]
				break
			}
		}
		if found == nil {
			return nil, false
		}
		out = append(out, *found)
	}
	return out, true
}
