// Package manifest defines the declarative item catalogue and per-SKU profiles.
//
// A profile says "this drive has N bytes and should be filled to X%" and points
// at a catalogue of items. The catalogue is the single source of truth for what
// can be shipped; each item is tagged with the SKUs that want it and whether it
// is legal to sell.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Kind identifies what sort of payload an item produces.
type Kind string

const (
	KindZIM     Kind = "zim"
	KindGGUF    Kind = "gguf"
	KindZip     Kind = "zip"
	KindPDF     Kind = "pdf"
	KindPMTiles Kind = "pmtiles"
)

var kinds = map[Kind]bool{
	KindZIM: true, KindGGUF: true, KindZip: true, KindPDF: true, KindPMTiles: true,
}

// Resolver types.
const (
	ResolveKiwix    = "kiwix"
	ResolveHugging  = "huggingface"
	ResolveGitHub   = "github"
	ResolveStatic   = "static"
	ResolvePMTiles  = "pmtiles"
	ResolveEstimate = "estimate"
)

// Resolver is a flat tagged union: only the fields relevant to Type are read.
// Flat rather than nested so profile JSON stays readable.
type Resolver struct {
	Type string `json:"type"`

	// kiwix: catalogue slug plus an ordered flavour preference list.
	Name     string   `json:"name,omitempty"`
	Flavours []string `json:"flavours,omitempty"`

	// huggingface: repo plus glob patterns matched against file paths.
	Repo  string   `json:"repo,omitempty"`
	Files []string `json:"files,omitempty"`

	// github: asset regexps, all of which must match within one release.
	// Tag pins a release; empty means "newest release that has matching assets".
	Assets []string `json:"assets,omitempty"`
	Tag    string   `json:"tag,omitempty"`

	// static: a direct URL, optionally with a known hash or a hash sidecar.
	URL    string `json:"url,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	MD5    string `json:"md5,omitempty"`
	MD5URL string `json:"md5_url,omitempty"`
	// SizeBytes pins the size of an immutable (versioned) static asset so that
	// resolving needs no network at all. Publishing hosts throttle.
	SizeBytes int64 `json:"size_bytes,omitempty"`

	// pmtiles: the extract recipe. Resolved in M1; M0 records the plan only.
	BBox          string `json:"bbox,omitempty"`
	PlanetDate    string `json:"planet_date,omitempty"`
	EstimateBytes int64  `json:"estimate_bytes,omitempty"`
}

// Item is one shippable unit of content.
type Item struct {
	ID   string   `json:"id"`
	Kind Kind     `json:"kind"`
	Cat  string   `json:"category"`
	Dest string   `json:"dest"` // directory (trailing '/') or explicit file path
	SKUs []string `json:"skus"`

	Resolver Resolver `json:"resolver"`

	CommercialOK bool   `json:"commercial_ok"`
	License      string `json:"license"`
	Attribution  string `json:"attribution"`

	// Priority drives budget order: higher wins when the drive is full.
	Priority int `json:"priority"`

	// ExtractTo unpacks a zip archive into this directory on the drive instead
	// of placing the archive itself (e.g. "bin/kiwix/").
	ExtractTo string `json:"extract_to,omitempty"`

	// Pending marks items whose payload is not yet sourced (M1 work).
	Pending bool   `json:"pending,omitempty"`
	Note    string `json:"note,omitempty"`
}

// Catalogue is the full item list.
type Catalogue struct {
	Items []Item `json:"items"`
}

// Variant is a build flavour: sell excludes non-commercial content.
type Variant struct {
	IncludeNonCommercial bool `json:"include_noncommercial"`
}

// Profile describes one physical SKU.
type Profile struct {
	ID            string             `json:"id"`
	SKU           string             `json:"sku"`
	Name          string             `json:"name"`
	CapacityBytes int64              `json:"capacity_bytes"`
	FillTarget    float64            `json:"fill_target"`
	Catalogue     string             `json:"catalogue"`
	Variants      map[string]Variant `json:"variants"`
}

// LoadProfile reads a profile and the catalogue it references, resolving the
// catalogue path relative to the profile file.
func LoadProfile(path string) (Profile, []Item, error) {
	var p Profile
	if err := readJSON(path, &p); err != nil {
		return p, nil, err
	}
	catPath := p.Catalogue
	if !filepath.IsAbs(catPath) {
		catPath = filepath.Join(filepath.Dir(path), catPath)
	}
	var c Catalogue
	if err := readJSON(catPath, &c); err != nil {
		return p, nil, err
	}
	if err := p.Validate(); err != nil {
		return p, nil, err
	}
	if err := ValidateItems(c.Items); err != nil {
		return p, nil, err
	}
	return p, c.Items, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

// Validate checks the profile itself.
func (p *Profile) Validate() error {
	switch {
	case p.ID == "":
		return fmt.Errorf("profile: id is required")
	case p.SKU == "":
		return fmt.Errorf("profile %s: sku is required", p.ID)
	case p.CapacityBytes <= 0:
		return fmt.Errorf("profile %s: capacity_bytes must be positive", p.ID)
	case p.FillTarget <= 0 || p.FillTarget > 1:
		return fmt.Errorf("profile %s: fill_target must be in (0,1]", p.ID)
	case len(p.Variants) == 0:
		return fmt.Errorf("profile %s: at least one variant is required", p.ID)
	}
	for name := range p.Variants {
		if name == "" {
			return fmt.Errorf("profile %s: variant name must not be empty", p.ID)
		}
	}
	return nil
}

// ValidateItems checks every item in a catalogue.
func ValidateItems(items []Item) error {
	if len(items) == 0 {
		return fmt.Errorf("catalogue: no items")
	}
	seen := make(map[string]bool, len(items))
	for i, it := range items {
		where := fmt.Sprintf("catalogue item %d", i)
		if it.ID == "" {
			return fmt.Errorf("%s: id is required", where)
		}
		where = "item " + it.ID
		if seen[it.ID] {
			return fmt.Errorf("%s: duplicate id", where)
		}
		seen[it.ID] = true

		if !kinds[it.Kind] {
			return fmt.Errorf("%s: unknown kind %q", where, it.Kind)
		}
		if it.Dest == "" {
			return fmt.Errorf("%s: dest is required", where)
		}
		if len(it.SKUs) == 0 {
			return fmt.Errorf("%s: at least one sku is required", where)
		}
		if it.License == "" {
			return fmt.Errorf("%s: license is required", where)
		}
		if it.Resolver.Type == "" {
			return fmt.Errorf("%s: resolver.type is required", where)
		}
		if err := it.Resolver.Validate(it.ID); err != nil {
			return err
		}
		if it.CommercialOK && isNonCommercialLicense(it.License) {
			return fmt.Errorf("%s: commercial_ok conflicts with license %q", where, it.License)
		}
		if it.ExtractTo != "" {
			if it.Kind != KindZip {
				return fmt.Errorf("%s: extract_to is only valid for zip items", where)
			}
			if !strings.HasSuffix(it.ExtractTo, "/") {
				return fmt.Errorf("%s: extract_to must end with '/'", where)
			}
			if strings.HasPrefix(it.ExtractTo, "/") || strings.Contains(it.ExtractTo, "..") {
				return fmt.Errorf("%s: extract_to must be a relative path without '..'", where)
			}
		}
	}
	return nil
}

// Validate checks that the fields the resolver type needs are present.
func (r *Resolver) Validate(itemID string) error {
	fail := func(msg string) error { return fmt.Errorf("item %s: %s", itemID, msg) }
	switch r.Type {
	case ResolveKiwix:
		if r.Name == "" {
			return fail("kiwix resolver needs name")
		}
	case ResolveHugging:
		if r.Repo == "" {
			return fail("huggingface resolver needs repo")
		}
		if len(r.Files) == 0 {
			return fail("huggingface resolver needs files")
		}
	case ResolveGitHub:
		if r.Repo == "" {
			return fail("github resolver needs repo")
		}
		if len(r.Assets) == 0 {
			return fail("github resolver needs assets")
		}
	case ResolveStatic:
		if r.URL == "" {
			return fail("static resolver needs url")
		}
	case ResolvePMTiles:
		if r.BBox == "" {
			return fail("pmtiles resolver needs bbox")
		}
		if r.EstimateBytes <= 0 {
			return fail("pmtiles resolver needs estimate_bytes while pending")
		}
	case ResolveEstimate:
		if r.EstimateBytes <= 0 {
			return fail("estimate resolver needs estimate_bytes")
		}
	default:
		return fail(fmt.Sprintf("unknown resolver type %q", r.Type))
	}
	return nil
}

// isNonCommercialLicense reports whether a licence forbids commercial
// distribution. Only the NC clause matters here; attribution does not.
func isNonCommercialLicense(license string) bool {
	l := strings.ToUpper(strings.ReplaceAll(license, " ", ""))
	return strings.Contains(l, "-NC-") || strings.Contains(l, "-NC") || strings.Contains(l, "NONCOMMERCIAL")
}

// ForSKU returns the items wanted by this profile's SKU, in catalogue order.
func ForSKU(items []Item, sku string) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		for _, s := range it.SKUs {
			if s == sku {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

// ForVariant drops items that may not ship in this variant.
func ForVariant(items []Item, v Variant) []Item {
	if v.IncludeNonCommercial {
		return items
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if it.CommercialOK {
			out = append(out, it)
		}
	}
	return out
}
