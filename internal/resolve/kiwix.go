package resolve

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"survivaldrive/internal/manifest"
	"survivaldrive/internal/metalink"
)

// Kiwix resolves .zim files through the Kiwix OPDS catalogue and each file's
// metalink. The metalink is the source of truth: it carries the exact sha256,
// the per-4MiB piece hashes and the mirror list, so nothing has to be guessed.
type Kiwix struct {
	BaseURL string
	HTTP    *http.Client
}

const opdsMaxBytes = 8 << 20

type opdsFeed struct {
	Entries []opdsEntry `xml:"entry"`
}

type opdsEntry struct {
	Name    string     `xml:"name"`
	Title   string     `xml:"title"`
	Flavour string     `xml:"flavour"`
	Cat     string     `xml:"category"`
	Links   []opdsLink `xml:"link"`
}

type opdsLink struct {
	Rel    string `xml:"rel,attr"`
	Type   string `xml:"type,attr"`
	Href   string `xml:"href,attr"`
	Length int64  `xml:"length,attr"`
}

// Resolve implements Resolver.
func (k *Kiwix) Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error) {
	entries, err := k.entries(ctx, item.Resolver.Name)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("kiwix: no catalogue entry for %q (content may have been withdrawn)", item.Resolver.Name)
	}

	ordered := orderByFlavour(entries, item.Resolver.Flavours)
	var cands []Candidate
	for _, e := range ordered {
		href := e.metalinkURL()
		if href == "" {
			continue
		}
		abs, err := k.abs(href)
		if err != nil {
			return nil, err
		}
		file, err := k.metalinkFile(ctx, abs, item)
		if err != nil {
			return nil, fmt.Errorf("flavour %q: %w", e.flavourLabel(), err)
		}
		cands = append(cands, Candidate{
			Label: candidateLabel(e),
			Files: []File{file},
		})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("kiwix: no usable flavour for %q (available: %s)",
			item.Resolver.Name, strings.Join(flavourLabels(entries), ", "))
	}
	return cands, nil
}

func (k *Kiwix) entries(ctx context.Context, name string) ([]opdsEntry, error) {
	u := fmt.Sprintf("%s/catalog/v2/entries?name=%s&count=25", k.BaseURL, url.QueryEscape(name))
	var feed opdsFeed
	if err := getXML(ctx, k.HTTP, u, &feed, opdsMaxBytes); err != nil {
		return nil, err
	}
	// The catalogue's name filter is a prefix search, so "ted_mul_health" also
	// returns "ted_mul_health-care". Prefer entries whose name is an exact match;
	// only fall back to the broader set when nothing matches exactly.
	var exact []opdsEntry
	for _, e := range feed.Entries {
		if e.Name == name {
			exact = append(exact, e)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	return feed.Entries, nil
}

// metalinkFile downloads a metalink and pins the payload from it.
func (k *Kiwix) metalinkFile(ctx context.Context, href string, item manifest.Item) (File, error) {
	mf, err := metalink.Fetch(ctx, k.HTTP, href)
	if err != nil {
		return File{}, err
	}
	f := File{
		Name:      mf.Name,
		Dest:      destFor(item, mf.Name),
		SizeBytes: mf.Size,
		SHA256:    mf.SHA256,
		MD5:       mf.MD5,
	}
	// The mirror list itself is volatile - MirrorBrain re-shuffles and
	// re-selects mirrors over time - so the lock pins the stable descriptor
	// instead and the fetcher re-reads it for current mirrors.
	f.Descriptor = href
	// The canonical download location is the descriptor path without ".meta4";
	// it is byte-stable across runs, unlike the mirror list.
	f.URL = strings.TrimSuffix(href, ".meta4")
	if mf.Pieces != nil {
		f.Pieces = &Pieces{Size: mf.Pieces.Length, SHA1: mf.Pieces.Hashes}
	}
	return f, nil
}

func (k *Kiwix) abs(href string) (string, error) {
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		return href, nil
	}
	base, err := url.Parse(k.BaseURL)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

// metalinkURL returns the acquisition link for the .zim.meta4 descriptor.
func (e opdsEntry) metalinkURL() string {
	for _, l := range e.Links {
		if strings.HasSuffix(l.Href, ".meta4") {
			return l.Href
		}
	}
	for _, l := range e.Links {
		if l.Type == "application/x-zim" {
			return l.Href
		}
	}
	return ""
}

func (e opdsEntry) flavourLabel() string {
	if e.Flavour == "" {
		return path.Base(strings.TrimSuffix(e.Name, path.Ext(e.Name)))
	}
	return e.Flavour
}

// candidateLabel is what the report and the lock show: the flavour when the ZIM
// has one, otherwise "default" (the slug is already the item id).
func candidateLabel(e opdsEntry) string {
	if e.Flavour == "" {
		return "default"
	}
	return e.Flavour
}

func flavourLabels(es []opdsEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.flavourLabel())
	}
	return out
}

// orderByFlavour reorders catalogue entries to match the requested preference
// list. When a preference list is given, only those flavours are candidates:
// an explicit preference is a decision, not a hint. With no preference, entries
// are ordered largest-first so resolution is still deterministic.
func orderByFlavour(entries []opdsEntry, prefer []string) []opdsEntry {
	if len(prefer) == 0 {
		out := make([]opdsEntry, len(entries))
		copy(out, entries)
		sort.SliceStable(out, func(i, j int) bool { return entrySize(out[i]) > entrySize(out[j]) })
		return out
	}
	remaining := make([]opdsEntry, len(entries))
	copy(remaining, entries)
	var out []opdsEntry
	for _, want := range prefer {
		for i, e := range remaining {
			if e.Flavour == want || e.flavourLabel() == want {
				out = append(out, e)
				remaining = append(remaining[:i], remaining[i+1:]...)
				break
			}
		}
	}
	return out
}

func entrySize(e opdsEntry) int64 {
	var n int64
	for _, l := range e.Links {
		if l.Type == "application/x-zim" && l.Length > n {
			n = l.Length
		}
	}
	return n
}
