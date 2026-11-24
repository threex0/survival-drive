package lock_test

import (
	"bytes"
	"strings"
	"testing"

	"survivaldrive/internal/budget"
	"survivaldrive/internal/lock"
	"survivaldrive/internal/manifest"
	"survivaldrive/internal/resolve"
)

const gib = int64(1) << 30

func profile() manifest.Profile {
	return manifest.Profile{
		ID:            "sku1",
		SKU:           "sku1",
		CapacityBytes: 128 * gib,
		FillTarget:    0.85,
	}
}

func plan() budget.Plan {
	return budget.Plan{
		CapacityBytes: 128 * gib,
		LimitBytes:    108 * gib,
		TotalBytes:    12 * gib,
		Choices: []budget.Choice{
			{
				Item: manifest.Item{
					ID: "zeta", Cat: "books", Kind: manifest.KindZIM, Dest: "archives/",
					CommercialOK: true, License: "CC-BY-SA-4.0", Attribution: "Zeta",
					Priority: 10,
				},
				Candidate: resolve.Candidate{
					Label: "maxi",
					Files: []resolve.File{
						{Name: "zeta.b.zim", Dest: "archives/zeta.b.zim", SizeBytes: gib, SHA256: strings.Repeat("b", 64)},
						{Name: "zeta.a.zim", Dest: "archives/zeta.a.zim", SizeBytes: gib, SHA256: strings.Repeat("a", 64)},
					},
				},
			},
			{
				Item: manifest.Item{
					ID: "alpha", Cat: "wiki", Kind: manifest.KindZIM, Dest: "archives/",
					CommercialOK: true, License: "CC-BY-SA-4.0",
					Priority: 10,
				},
				Candidate: resolve.Candidate{
					Label: "nopic",
					Files: []resolve.File{
						{Name: "alpha.zim", Dest: "archives/alpha.zim", SizeBytes: 10 * gib, SHA256: strings.Repeat("c", 64)},
					},
				},
			},
		},
	}
}

func TestFromPlanSortsForDeterminism(t *testing.T) {
	l := lock.FromPlan(plan(), profile(), "sell")
	if len(l.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(l.Items))
	}
	if l.Items[0].ID != "alpha" || l.Items[1].ID != "zeta" {
		t.Fatalf("order = %s, %s; want alpha, zeta", l.Items[0].ID, l.Items[1].ID)
	}
	files := l.Items[1].Files
	if files[0].Dest != "archives/zeta.a.zim" || files[1].Dest != "archives/zeta.b.zim" {
		t.Errorf("files = %s, %s; want dest-sorted", files[0].Dest, files[1].Dest)
	}
	if l.TotalBytes != 12*gib {
		t.Errorf("total = %d, want 12 GiB", l.TotalBytes)
	}
	if l.TotalFiles() != 3 {
		t.Errorf("files = %d, want 3", l.TotalFiles())
	}
}

func TestMarshalIsByteIdentical(t *testing.T) {
	a, err := lock.FromPlan(plan(), profile(), "sell").Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := lock.FromPlan(plan(), profile(), "sell").Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two marshals of the same plan differ; the lock must be deterministic")
	}
	if !bytes.HasSuffix(a, []byte("\n")) {
		t.Error("lock should end with a newline")
	}
	if bytes.Contains(a, []byte("generated")) {
		t.Error("lock must not contain timestamps; they break diffing")
	}
}

func TestUnpinnedAndEstimated(t *testing.T) {
	p := plan()
	p.Choices[1].Candidate.Files[0].SHA256 = ""
	p.Choices = append(p.Choices, budget.Choice{
		Item:      manifest.Item{ID: "pdfs", License: "Public-Domain-US-Gov"},
		Candidate: resolve.Candidate{Label: "estimate", Estimated: true, BytesEstimate: gib},
	})
	l := lock.FromPlan(p, profile(), "sell")

	if got := l.Unpinned(); len(got) != 1 || got[0] != "alpha/alpha.zim" {
		t.Errorf("unpinned = %v, want [alpha/alpha.zim]", got)
	}
	if got := l.Estimated(); len(got) != 1 || got[0] != "pdfs" {
		t.Errorf("estimated = %v, want [pdfs]", got)
	}
	if err := l.CheckStrict(); err == nil {
		t.Fatal("strict check must fail with unpinned and estimated items")
	} else {
		if !strings.Contains(err.Error(), "no sha256") || !strings.Contains(err.Error(), "not sourced") {
			t.Errorf("strict error = %q, want both problems named", err)
		}
	}
}

func TestCheckStrictPassesWhenFullyPinned(t *testing.T) {
	l := lock.FromPlan(plan(), profile(), "sell")
	if err := l.CheckStrict(); err != nil {
		t.Fatalf("fully pinned lock must pass strict: %v", err)
	}
}

func TestSkippedItemsAreRecorded(t *testing.T) {
	p := plan()
	p.Skipped = []budget.Skip{{Item: manifest.Item{ID: "too-big"}, Reason: "no room"}}
	l := lock.FromPlan(p, profile(), "sell")
	if len(l.Skipped) != 1 || l.Skipped[0].ID != "too-big" {
		t.Fatalf("skipped = %+v", l.Skipped)
	}
}
