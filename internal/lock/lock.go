// Package lock writes the pinned manifest produced by `resolve`.
//
// The lock is deliberately deterministic: items sorted by ID, no timestamps, no
// map iteration. Two runs against the same upstream content must produce
// byte-identical output so that the monthly refresh diff is meaningful.
package lock

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"survivaldrive/internal/budget"
	"survivaldrive/internal/manifest"
	"survivaldrive/internal/resolve"
)

// Version is the lock format version.
const Version = 1

// Lock is the on-disk artefact.
type Lock struct {
	Version       int       `json:"version"`
	Profile       string    `json:"profile"`
	SKU           string    `json:"sku"`
	Variant       string    `json:"variant"`
	CapacityBytes int64     `json:"capacity_bytes"`
	FillTarget    float64   `json:"fill_target"`
	TotalBytes    int64     `json:"total_bytes"`
	Items         []Item    `json:"items"`
	Skipped       []Skipped `json:"skipped,omitempty"`
}

// Item is one pinned entry.
type Item struct {
	ID           string         `json:"id"`
	Category     string         `json:"category"`
	Kind         manifest.Kind  `json:"kind"`
	Candidate    string         `json:"candidate"`
	Bytes        int64          `json:"bytes"`
	Estimated    bool           `json:"estimated,omitempty"`
	CommercialOK bool           `json:"commercial_ok"`
	License      string         `json:"license"`
	Attribution  string         `json:"attribution,omitempty"`
	Note         string         `json:"note,omitempty"`
	ExtractTo    string         `json:"extract_to,omitempty"`
	Files        []resolve.File `json:"files"`
}

// Skipped is an item that did not fit the drive.
type Skipped struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// FromPlan converts a budget plan into a lock, sorted for determinism.
func FromPlan(p budget.Plan, profile manifest.Profile, variant string) Lock {
	l := Lock{
		Version:       Version,
		Profile:       profile.ID,
		SKU:           profile.SKU,
		Variant:       variant,
		CapacityBytes: profile.CapacityBytes,
		FillTarget:    profile.FillTarget,
		TotalBytes:    p.TotalBytes,
	}
	for _, c := range p.Choices {
		it := Item{
			ID:           c.Item.ID,
			Category:     c.Item.Cat,
			Kind:         c.Item.Kind,
			Candidate:    c.Candidate.Label,
			Bytes:        c.Bytes(),
			Estimated:    c.Candidate.Estimated,
			CommercialOK: c.Item.CommercialOK,
			License:      c.Item.License,
			Attribution:  c.Item.Attribution,
			Note:         c.Candidate.Note,
			ExtractTo:    c.Item.ExtractTo,
			Files:        c.Candidate.Files,
		}
		sort.SliceStable(it.Files, func(i, j int) bool { return it.Files[i].Dest < it.Files[j].Dest })
		l.Items = append(l.Items, it)
	}
	sort.SliceStable(l.Items, func(i, j int) bool { return l.Items[i].ID < l.Items[j].ID })
	for _, s := range p.Skipped {
		l.Skipped = append(l.Skipped, Skipped{ID: s.Item.ID, Reason: s.Reason})
	}
	sort.SliceStable(l.Skipped, func(i, j int) bool { return l.Skipped[i].ID < l.Skipped[j].ID })
	return l
}

// Marshal renders the lock deterministically.
func (l Lock) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Read loads a lock file and checks that its shape is usable.
func Read(path string) (Lock, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Lock{}, fmt.Errorf("read lock %s: %w", path, err)
	}
	var l Lock
	if err := json.Unmarshal(b, &l); err != nil {
		return Lock{}, fmt.Errorf("parse lock %s: %w", path, err)
	}
	if l.Version != Version {
		return Lock{}, fmt.Errorf("lock %s: version %d, want %d", path, l.Version, Version)
	}
	if len(l.Items) == 0 {
		return Lock{}, fmt.Errorf("lock %s: no items", path)
	}
	return l, nil
}

// Unpinned lists "item/file" entries that have no sha256 and are not estimates.
// A build must not download these until they are pinned.
func (l Lock) Unpinned() []string {
	var out []string
	for _, it := range l.Items {
		if it.Estimated {
			continue
		}
		for _, f := range it.Files {
			if f.SHA256 == "" {
				out = append(out, it.ID+"/"+f.Name)
			}
		}
	}
	return out
}

// Estimated lists items whose payload is not sourced yet.
func (l Lock) Estimated() []string {
	var out []string
	for _, it := range l.Items {
		if it.Estimated {
			out = append(out, it.ID)
		}
	}
	return out
}

// CheckStrict reports why a build must not proceed, or nil when it may.
func (l Lock) CheckStrict() error {
	var problems []string
	if u := l.Unpinned(); len(u) > 0 {
		problems = append(problems, "no sha256: "+strings.Join(u, ", "))
	}
	if e := l.Estimated(); len(e) > 0 {
		problems = append(problems, "size estimated, payload not sourced: "+strings.Join(e, ", "))
	}
	if len(problems) > 0 {
		return fmt.Errorf("lock is not build-ready: %s", strings.Join(problems, "; "))
	}
	return nil
}

// TotalFiles counts pinned files.
func (l Lock) TotalFiles() int {
	n := 0
	for _, it := range l.Items {
		n += len(it.Files)
	}
	return n
}
