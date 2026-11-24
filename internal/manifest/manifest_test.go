package manifest_test

import (
	"path/filepath"
	"strings"
	"testing"

	"survivaldrive/internal/manifest"
)

func validItem() manifest.Item {
	return manifest.Item{
		ID:           "test",
		Kind:         manifest.KindZIM,
		Cat:          "wiki",
		Dest:         "archives/",
		SKUs:         []string{"sku1"},
		CommercialOK: true,
		License:      "CC-BY-SA-4.0",
		Resolver:     manifest.Resolver{Type: manifest.ResolveKiwix, Name: "wikipedia_en_all"},
	}
}

func TestValidateItemsAcceptsMinimalItem(t *testing.T) {
	if err := manifest.ValidateItems([]manifest.Item{validItem()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateItemsRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*manifest.Item)
		wantSub string
	}{
		{"missing id", func(i *manifest.Item) { i.ID = "" }, "id is required"},
		{"unknown kind", func(i *manifest.Item) { i.Kind = "rar" }, "unknown kind"},
		{"missing dest", func(i *manifest.Item) { i.Dest = "" }, "dest is required"},
		{"missing sku", func(i *manifest.Item) { i.SKUs = nil }, "at least one sku"},
		{"missing licence", func(i *manifest.Item) { i.License = "" }, "license is required"},
		{"kiwix without name", func(i *manifest.Item) { i.Resolver.Name = "" }, "needs name"},
		{"hf without files", func(i *manifest.Item) {
			i.Resolver = manifest.Resolver{Type: manifest.ResolveHugging, Repo: "a/b"}
		}, "needs files"},
		{"github without repo", func(i *manifest.Item) {
			i.Resolver = manifest.Resolver{Type: manifest.ResolveGitHub, Assets: []string{"x"}}
		}, "needs repo"},
		{"static without url", func(i *manifest.Item) {
			i.Resolver = manifest.Resolver{Type: manifest.ResolveStatic}
		}, "needs url"},
		{"pmtiles without bbox", func(i *manifest.Item) {
			i.Resolver = manifest.Resolver{Type: manifest.ResolvePMTiles, EstimateBytes: 1}
		}, "needs bbox"},
		{"unknown resolver", func(i *manifest.Item) {
			i.Resolver = manifest.Resolver{Type: "carrier-pigeon"}
		}, "unknown resolver type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			it := validItem()
			tc.mutate(&it)
			err := manifest.ValidateItems([]manifest.Item{it})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateRejectsCommercialNonCommercialConflict(t *testing.T) {
	it := validItem()
	it.License = "CC-BY-NC-SA-3.0"
	it.CommercialOK = true
	err := manifest.ValidateItems([]manifest.Item{it})
	if err == nil || !strings.Contains(err.Error(), "conflicts with license") {
		t.Fatalf("want a conflict error, got: %v", err)
	}

	// Flipping commercial_ok is allowed and is the normal case for NC content.
	it.CommercialOK = false
	if err := manifest.ValidateItems([]manifest.Item{it}); err != nil {
		t.Fatalf("NC content with commercial_ok=false must validate: %v", err)
	}
}

func TestValidateRejectsDuplicateIDs(t *testing.T) {
	a, b := validItem(), validItem()
	err := manifest.ValidateItems([]manifest.Item{a, b})
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("want a duplicate id error, got: %v", err)
	}
}

func TestForVariantSellExcludesNonCommercial(t *testing.T) {
	sell := manifest.ForVariant([]manifest.Item{
		{ID: "a", CommercialOK: true},
		{ID: "b", CommercialOK: false},
	}, manifest.Variant{IncludeNonCommercial: false})
	if len(sell) != 1 || sell[0].ID != "a" {
		t.Fatalf("sell = %+v, want only item a", sell)
	}

	full := manifest.ForVariant([]manifest.Item{
		{ID: "a", CommercialOK: true},
		{ID: "b", CommercialOK: false},
	}, manifest.Variant{IncludeNonCommercial: true})
	if len(full) != 2 {
		t.Fatalf("full = %d items, want 2", len(full))
	}
}

func TestForSKU(t *testing.T) {
	items := []manifest.Item{
		{ID: "both", SKUs: []string{"sku1", "sku2"}},
		{ID: "one", SKUs: []string{"sku1"}},
		{ID: "two", SKUs: []string{"sku2"}},
	}
	if got := manifest.ForSKU(items, "sku1"); len(got) != 2 {
		t.Errorf("sku1 = %d items, want 2", len(got))
	}
	if got := manifest.ForSKU(items, "sku2"); len(got) != 2 {
		t.Errorf("sku2 = %d items, want 2", len(got))
	}
}

// TestRealProfilesLoad guards the shipped profile files: they must parse, pass
// validation, and produce the expected split between variants.
func TestRealProfilesLoad(t *testing.T) {
	for _, tc := range []struct {
		file     string
		wantSell int
		wantFull int
	}{
		{file: "sku1.json", wantSell: 31, wantFull: 36},
		{file: "sku2.json", wantSell: 44, wantFull: 57},
	} {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join("..", "..", "profiles", tc.file)
			profile, catalogue, err := manifest.LoadProfile(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			items := manifest.ForSKU(catalogue, profile.SKU)
			sell := manifest.ForVariant(items, profile.Variants["sell"])
			full := manifest.ForVariant(items, profile.Variants["full"])
			if len(sell) != tc.wantSell {
				t.Errorf("sell = %d items, want %d", len(sell), tc.wantSell)
			}
			if len(full) != tc.wantFull {
				t.Errorf("full = %d items, want %d", len(full), tc.wantFull)
			}
			if len(sell) > len(full) {
				t.Error("sell must be a subset of full")
			}
			for _, it := range sell {
				if !it.CommercialOK {
					t.Errorf("sell contains non-commercial item %q", it.ID)
				}
			}
		})
	}
}

func TestProfileValidation(t *testing.T) {
	base := manifest.Profile{
		ID:            "p",
		SKU:           "p",
		CapacityBytes: 1 << 30,
		FillTarget:    0.9,
		Variants:      map[string]manifest.Variant{"sell": {}},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("baseline profile must validate: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*manifest.Profile)
		want   string
	}{
		{"no capacity", func(p *manifest.Profile) { p.CapacityBytes = 0 }, "capacity_bytes"},
		{"bad fill", func(p *manifest.Profile) { p.FillTarget = 1.5 }, "fill_target"},
		{"no variants", func(p *manifest.Profile) { p.Variants = nil }, "at least one variant"},
		{"no sku", func(p *manifest.Profile) { p.SKU = "" }, "sku is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			err := p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
