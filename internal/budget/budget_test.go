package budget_test

import (
	"strings"
	"testing"

	"survivaldrive/internal/budget"
	"survivaldrive/internal/manifest"
	"survivaldrive/internal/resolve"
)

const gib = int64(1) << 30

func item(id string, priority int) manifest.Item {
	return manifest.Item{ID: id, Priority: priority, Kind: manifest.KindZIM, Dest: "archives/"}
}

func cand(label string, size int64) resolve.Candidate {
	return resolve.Candidate{Label: label, Files: []resolve.File{{Name: label, SizeBytes: size}}}
}

func TestBuildFitsEverything(t *testing.T) {
	items := []manifest.Item{item("a", 10), item("b", 5)}
	cands := map[string][]resolve.Candidate{
		"a": {cand("maxi", 10*gib)},
		"b": {cand("all", 5*gib)},
	}
	p, err := budget.Build(items, cands, 100*gib, 0.9)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(p.Choices) != 2 || len(p.Skipped) != 0 {
		t.Fatalf("choices=%d skipped=%d, want 2/0", len(p.Choices), len(p.Skipped))
	}
	if p.TotalBytes != 15*gib {
		t.Errorf("total = %d, want 15 GiB", p.TotalBytes)
	}
	if p.LimitBytes != 90*gib || p.Headroom() != 75*gib {
		t.Errorf("limit=%d headroom=%d", p.LimitBytes, p.Headroom())
	}
}

func TestBuildDowngradesFlavourWhenFull(t *testing.T) {
	items := []manifest.Item{item("wiki", 10)}
	cands := map[string][]resolve.Candidate{
		"wiki": {
			cand("maxi", 80*gib),
			cand("nopic", 40*gib),
			cand("mini", 10*gib),
		},
	}
	// 100 GiB at 60% = 60 GiB: maxi (80) does not fit, nopic (40) does.
	p, err := budget.Build(items, cands, 100*gib, 0.6)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(p.Choices) != 1 || p.Choices[0].Candidate.Label != "nopic" {
		t.Fatalf("picked %+v, want nopic", p.Choices)
	}
	if p.TotalBytes != 40*gib {
		t.Errorf("total = %d, want 40 GiB", p.TotalBytes)
	}
}

func TestBuildSkipsItemThatCannotFit(t *testing.T) {
	items := []manifest.Item{item("big", 1)}
	cands := map[string][]resolve.Candidate{"big": {cand("only", 500*gib)}}
	p, err := budget.Build(items, cands, 100*gib, 0.9)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(p.Choices) != 0 || len(p.Skipped) != 1 {
		t.Fatalf("choices=%d skipped=%d, want 0/1", len(p.Choices), len(p.Skipped))
	}
	if !strings.Contains(p.Skipped[0].Reason, "smallest candidate") {
		t.Errorf("reason = %q", p.Skipped[0].Reason)
	}
}

func TestBuildHonoursPriority(t *testing.T) {
	items := []manifest.Item{
		item("low", 1),
		item("high", 100),
	}
	cands := map[string][]resolve.Candidate{
		"low":  {cand("x", 50*gib)},
		"high": {cand("y", 50*gib)},
	}
	// Only 60 GiB of room: the high-priority item must win regardless of order.
	p, err := budget.Build(items, cands, 100*gib, 0.6)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(p.Choices) != 1 || p.Choices[0].Item.ID != "high" {
		t.Fatalf("choices = %+v, want only 'high'", p.Choices)
	}
	if len(p.Skipped) != 1 || p.Skipped[0].Item.ID != "low" {
		t.Fatalf("skipped = %+v, want 'low'", p.Skipped)
	}
}

func TestBuildSeparatesEstimatedBytes(t *testing.T) {
	items := []manifest.Item{item("a", 1), item("pdf", 0)}
	cands := map[string][]resolve.Candidate{
		"a":   {cand("x", 10*gib)},
		"pdf": {{Label: "estimate", Estimated: true, BytesEstimate: 2 * gib}},
	}
	p, err := budget.Build(items, cands, 100*gib, 0.9)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if p.ResolvedBytes != 10*gib || p.EstimatedBytes != 2*gib {
		t.Errorf("resolved=%d estimated=%d", p.ResolvedBytes, p.EstimatedBytes)
	}
	if p.TotalBytes != 12*gib {
		t.Errorf("total = %d, want 12 GiB", p.TotalBytes)
	}
}

func TestBuildRejectsMissingCandidates(t *testing.T) {
	items := []manifest.Item{item("ghost", 1)}
	if _, err := budget.Build(items, map[string][]resolve.Candidate{}, 10*gib, 0.9); err == nil {
		t.Fatal("want an error for an unresolved item")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	items := []manifest.Item{item("a", 1)}
	cands := map[string][]resolve.Candidate{"a": {cand("x", gib)}}
	if _, err := budget.Build(items, cands, 0, 0.9); err == nil {
		t.Error("want error for zero capacity")
	}
	if _, err := budget.Build(items, cands, 10*gib, 0); err == nil {
		t.Error("want error for zero fill target")
	}
	if _, err := budget.Build(items, cands, 10*gib, 1.1); err == nil {
		t.Error("want error for fill target above 1")
	}
}
