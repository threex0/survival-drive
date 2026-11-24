package resolve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"survivaldrive/internal/manifest"
)

// HuggingFace resolves .gguf files from a model repo's file tree. The tree API
// reports the LFS object id, which is the sha256 of the file, so model files are
// pinned without downloading them.
type HuggingFace struct {
	BaseURL string
	HTTP    *http.Client
}

const hfTreeMaxBytes = 32 << 20

type hfEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
	LFS  *hfLFS `json:"lfs"`
}

type hfLFS struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

// Resolve implements Resolver.
func (h *HuggingFace) Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error) {
	repo := item.Resolver.Repo
	api := fmt.Sprintf("%s/api/models/%s/tree/main?recursive=true", h.BaseURL, repo)
	var tree []hfEntry
	if err := getJSON(ctx, h.HTTP, api, &tree, hfTreeMaxBytes); err != nil {
		return nil, fmt.Errorf("repo %s: %w", repo, err)
	}
	files := make([]hfEntry, 0, len(tree))
	for _, e := range tree {
		if e.Type == "file" {
			files = append(files, e)
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("repo %s: file tree is empty (gated or renamed repo?)", repo)
	}

	var out []File
	for _, pattern := range item.Resolver.Files {
		match, ok := firstMatch(files, pattern)
		if !ok {
			return nil, fmt.Errorf("repo %s: no file matches %q (available: %s)",
				repo, pattern, strings.Join(pathsOf(files, 8), ", "))
		}
		f := File{
			Name:      path.Base(match.Path),
			Dest:      destFor(item, path.Base(match.Path)),
			SizeBytes: match.Size,
			URL:       h.resolveURL(repo, match.Path),
		}
		if match.LFS != nil && match.LFS.Size > 0 {
			f.SizeBytes = match.LFS.Size
			if len(match.LFS.OID) == 64 {
				f.SHA256 = strings.ToLower(match.LFS.OID)
			}
		} else if len(match.OID) == 64 {
			f.SHA256 = strings.ToLower(match.OID)
		}
		out = append(out, f)
	}
	return []Candidate{{Label: "default", Files: out}}, nil
}

// resolveURL builds a direct download URL, escaping each path segment so names
// with spaces or '#' survive intact.
func (h *HuggingFace) resolveURL(repo, p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return fmt.Sprintf("%s/%s/resolve/main/%s", h.BaseURL, repo, strings.Join(segs, "/"))
}

// firstMatch returns the first file whose base name or full path matches the
// glob. Base-name matching is the common case; full-path matching allows
// targeting a subdirectory explicitly.
func firstMatch(files []hfEntry, pattern string) (hfEntry, bool) {
	for _, f := range files {
		if ok, _ := path.Match(pattern, path.Base(f.Path)); ok {
			return f, true
		}
	}
	for _, f := range files {
		if ok, _ := path.Match(pattern, f.Path); ok {
			return f, true
		}
	}
	return hfEntry{}, false
}

func pathsOf(files []hfEntry, limit int) []string {
	out := make([]string, 0, limit)
	for i, f := range files {
		if i >= limit {
			out = append(out, "...")
			break
		}
		out = append(out, path.Base(f.Path))
	}
	return out
}
