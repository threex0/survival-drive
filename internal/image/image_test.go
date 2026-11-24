package image_test

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"survivaldrive/internal/image"
	"survivaldrive/internal/lock"
	"survivaldrive/internal/manifest"
	"survivaldrive/internal/resolve"
)

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeZip(t *testing.T, dir, name string, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for n, body := range entries {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLintRejectsExFATHazards(t *testing.T) {
	bad := []string{
		"archives/CON.zim",
		"bin/nul",
		"lib/foo.",
		"lib/foo ",
		"lib/a:b.zim",
		"lib/a?b.zim",
		"/abs/path.zim",
		"../escape.zim",
		"lib/" + strings.Repeat("d", 210) + ".zim",
	}
	for _, p := range bad {
		if got := image.Lint(p); len(got) == 0 {
			t.Errorf("Lint(%q) = ok, want issues", p)
		}
	}

	good := []string{
		"archives/wikipedia_en_all_nopic_2026-06.zim",
		"models/gemma-4-E4B-it-Q4_K_M.gguf",
		"library_pdfs/fema/are-you-ready.pdf",
		"bin/kiwix/kiwix-serve.exe",
	}
	for _, p := range good {
		if got := image.Lint(p); len(got) != 0 {
			t.Errorf("Lint(%q) = %v, want clean", p, got)
		}
	}
}

// fixture builds a lock backed by a temp cache and returns the lookup function
// the builder needs (mirroring how cmd wires fetch.CachePath).
func fixture(t *testing.T) (lock.Lock, func(resolve.File) (string, bool)) {
	t.Helper()
	cache := t.TempDir()

	writeFile(t, cache, filepath.Join("sha256", "aaaazim"), []byte("zim-content"))
	writeZip(t, cache, filepath.Join("md5", "md5kiwix"), map[string]string{
		"kiwix-serve.exe":  "MZfake",
		"libkiwix.dll":     "dll",
		"docs/license.txt": "gpl",
	})

	l := lock.Lock{
		Version: lock.Version, Profile: "sku1", SKU: "sku1", Variant: "sell",
		TotalBytes: 1000,
		Items: []lock.Item{
			{
				ID: "wiki", Category: "wiki", Kind: manifest.KindZIM,
				CommercialOK: true, License: "CC-BY-SA-4.0", Attribution: "Wikipedia contributors",
				Files: []resolve.File{{Dest: "archives/wiki.zim", Name: "wiki.zim", SizeBytes: 11, SHA256: "aaaazim"}},
			},
			{
				ID: "bin-kiwix-tools", Category: "tools", Kind: manifest.KindZip,
				CommercialOK: true, License: "GPL-3.0-or-later", Attribution: "Kiwix",
				ExtractTo: "bin/kiwix/",
				Files:     []resolve.File{{Dest: "bin/kiwix.zip", Name: "kiwix.zip", URL: "https://example.org/release/kiwix-tools.zip", SizeBytes: 100, MD5: "md5kiwix"}},
			},
			{
				ID: "pdfs", Category: "pdf", Kind: manifest.KindPDF, Estimated: true,
				CommercialOK: true, License: "Public-Domain-US-Gov", Attribution: "FEMA",
			},
		},
	}
	lookup := func(f resolve.File) (string, bool) {
		var p string
		switch {
		case f.SHA256 != "":
			p = filepath.Join(cache, "sha256", f.SHA256)
		case f.MD5 != "":
			p = filepath.Join(cache, "md5", f.MD5)
		default:
			return "", false
		}
		_, err := os.Stat(p)
		return p, err == nil
	}
	return l, lookup
}

func TestBuildPlacesFilesExtractsZipsAndWritesManifest(t *testing.T) {
	l, lookup := fixture(t)
	out := t.TempDir()
	licDir := t.TempDir()
	writeFile(t, licDir, "cc-by-sa-4-0.txt", []byte("CC BY-SA 4.0 legalcode"))

	rep, err := image.Build(context.Background(), l, image.Options{
		OutDir:     out,
		LicenseDir: licDir,
		Lookup:     lookup,
		Version:    "test-1",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rep.Files) != 2 {
		t.Fatalf("placed %d files, want 2 (zim + extracted zip)", len(rep.Files))
	}

	if _, err := os.Stat(filepath.Join(out, "archives", "wiki.zim")); err != nil {
		t.Errorf("zim not placed: %v", err)
	}
	for _, f := range []string{"kiwix-serve.exe", "libkiwix.dll", filepath.Join("docs", "license.txt")} {
		if _, err := os.Stat(filepath.Join(out, "bin", "kiwix", f)); err != nil {
			t.Errorf("extracted file missing: %s (%v)", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "bin", "kiwix.zip")); err == nil {
		t.Error("the archive itself must not be placed when extract_to is set")
	}

	man, err := os.ReadFile(filepath.Join(out, "_MANIFEST.sha256"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	body := string(man)
	for _, want := range []string{"archives/wiki.zim", "bin/kiwix/kiwix-serve.exe", "LICENSES/NOTICE.md"} {
		if !strings.Contains(body, want) {
			t.Errorf("manifest missing %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "_MANIFEST.sha256") || strings.Contains(body, "image_version.txt") {
		t.Error("manifest must not list itself or image_version.txt")
	}
	if n := len(strings.Split(strings.TrimSpace(body), "\n")); n != rep.ManifestLines {
		t.Errorf("manifest has %d lines, report says %d", n, rep.ManifestLines)
	}

	ver, err := os.ReadFile(filepath.Join(out, "image_version.txt"))
	if err != nil {
		t.Fatalf("image_version.txt: %v", err)
	}
	if !strings.Contains(string(ver), "version: test-1") {
		t.Errorf("version file = %q", string(ver))
	}

	notice, err := os.ReadFile(filepath.Join(out, "LICENSES", "NOTICE.md"))
	if err != nil {
		t.Fatalf("notice: %v", err)
	}
	ns := string(notice)
	if !strings.Contains(ns, "CC-BY-SA-4.0") || !strings.Contains(ns, "Kiwix") {
		t.Errorf("notice missing licences/attribution:\n%s", ns)
	}
	if !strings.Contains(ns, "LICENSES/cc-by-sa-4-0.txt") {
		t.Errorf("notice should point at the vendored text:\n%s", ns)
	}
	// GPLv3 obliges us to make the corresponding source available, not implied.
	if !strings.Contains(ns, "Source and written offer") || !strings.Contains(ns, "example.org/release/kiwix-tools.zip") {
		t.Errorf("notice should include the GPL source offer and URL:\n%s", ns)
	}
	if _, err := os.Stat(filepath.Join(out, "LICENSES", "cc-by-sa-4-0.txt")); err != nil {
		t.Errorf("vendored licence text not copied: %v", err)
	}
	if len(rep.MissingLicenses) != 1 || rep.MissingLicenses[0] != "GPL-3.0-or-later" {
		t.Errorf("missing licences = %v, want GPL only", rep.MissingLicenses)
	}
}

func TestBuildRefusesMissingFiles(t *testing.T) {
	l, lookup := fixture(t)
	l.Items[0].Files[0].SHA256 = "not-in-cache"
	_, err := image.Build(context.Background(), l, image.Options{OutDir: t.TempDir(), Lookup: lookup})
	if err == nil || !strings.Contains(err.Error(), "not in cache") {
		t.Fatalf("error = %v, want a missing-cache refusal", err)
	}
	if !strings.Contains(err.Error(), "wiki.zim") {
		t.Errorf("error should name the file: %v", err)
	}
}

func TestBuildAllowMissingPlacesWhatItCan(t *testing.T) {
	l, lookup := fixture(t)
	l.Items[0].Files[0].SHA256 = "not-in-cache"
	rep, err := image.Build(context.Background(), l, image.Options{
		OutDir: t.TempDir(), Lookup: lookup, AllowMissing: true,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rep.Missing) != 1 {
		t.Errorf("missing = %v, want one", rep.Missing)
	}
	if len(rep.Files) != 1 {
		t.Errorf("placed %d files, want 1", len(rep.Files))
	}
}

func TestBuildSkipsEstimatedItems(t *testing.T) {
	l, lookup := fixture(t)
	rep, err := image.Build(context.Background(), l, image.Options{OutDir: t.TempDir(), Lookup: lookup})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, f := range rep.Files {
		if f.ItemID == "pdfs" {
			t.Error("an estimated item must not appear in the image")
		}
	}
}

func TestBuildRejectsCaseCollisions(t *testing.T) {
	l, lookup := fixture(t)
	l.Items = append(l.Items, lock.Item{
		ID: "wiki2", Kind: manifest.KindZIM, License: "CC-BY-SA-4.0",
		Files: []resolve.File{{Dest: "ARCHIVES/WIKI.ZIM", Name: "x", SizeBytes: 1, SHA256: "aaaazim"}},
	})
	_, err := image.Build(context.Background(), l, image.Options{OutDir: t.TempDir(), Lookup: lookup})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "claimed by both") {
		t.Fatalf("error = %v, want a case-collision refusal", err)
	}
}

func TestBuildRejectsDuplicateDestinations(t *testing.T) {
	// Regression: every GGUF repo ships "mmproj-F16.gguf", so a shared models/
	// directory silently lost one model's vision projector.
	l, lookup := fixture(t)
	l.Items = append(l.Items, lock.Item{
		ID: "model-two", Kind: manifest.KindGGUF, License: "Apache-2.0",
		Files: []resolve.File{{Dest: "archives/wiki.zim", Name: "mmproj-F16.gguf", SizeBytes: 11, SHA256: "aaaazim"}},
	})
	_, err := image.Build(context.Background(), l, image.Options{OutDir: t.TempDir(), Lookup: lookup})
	if err == nil {
		t.Fatal("want a duplicate-destination refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "claimed by both") || !strings.Contains(msg, "model-two") {
		t.Fatalf("error = %v, want it to name both items", msg)
	}
}

func TestBuildRejectsZipSlip(t *testing.T) {
	cache := t.TempDir()
	writeZip(t, cache, filepath.Join("sha256", "evilzip"), map[string]string{"../escape.txt": "nope"})

	l := lock.Lock{
		Version: lock.Version, Profile: "p", SKU: "s", Variant: "v",
		Items: []lock.Item{{
			ID: "evil", Kind: manifest.KindZip, License: "MIT", ExtractTo: "bin/evil/",
			Files: []resolve.File{{Dest: "bin/evil.zip", Name: "evil.zip", SizeBytes: 4, SHA256: "evilzip"}},
		}},
	}
	out := t.TempDir()
	_, err := image.Build(context.Background(), l, image.Options{
		OutDir: out,
		Lookup: func(f resolve.File) (string, bool) {
			return filepath.Join(cache, "sha256", f.SHA256), true
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe entry") {
		t.Fatalf("error = %v, want a zip-slip refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "..", "escape.txt")); statErr == nil {
		t.Fatal("zip slip wrote outside the image")
	}
}

func TestBuildLinksWhenAsked(t *testing.T) {
	l, lookup := fixture(t)
	out := filepath.Join(t.TempDir(), "img")
	rep, err := image.Build(context.Background(), l, image.Options{
		OutDir: out, Lookup: lookup, Link: true,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	linked := false
	for _, f := range rep.Files {
		if f.Linked {
			linked = true
		}
	}
	if !linked {
		t.Log("hard links unavailable on this volume; copy fallback used (acceptable)")
	}
	if _, err := os.Stat(filepath.Join(out, "archives", "wiki.zim")); err != nil {
		t.Errorf("file missing after link attempt: %v", err)
	}
}

func TestZipSourceExcludesPrivateMaterial(t *testing.T) {
	src := t.TempDir()
	// Code that must ship; docs and secrets that must not.
	for _, p := range []string{
		"go.mod", "cmd/app/main.go", "internal/x/x.go", "internal/image/image.go",
		"docs/spec.md", "docs/plan.md",
		".env", "bin/app.exe", "GRAPHIFY-OUT/graph.json",
		"dist/app.exe", "cache/big.bin", "image/sku1/archives/x.zim",
		"sku1-sell.lock.json",
	} {
		full := filepath.Join(src, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(t.TempDir(), "source.zip")
	if _, err := image.ZipSource(src, dst); err != nil {
		t.Fatalf("zip: %v", err)
	}

	zr, err := zip.OpenReader(dst)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	got := map[string]bool{}
	for _, f := range zr.File {
		got[f.Name] = true
	}
	for _, want := range []string{"go.mod", "cmd/app/main.go", "internal/x/x.go", "internal/image/image.go"} {
		if !got[want] {
			t.Errorf("missing %s from the archive", want)
		}
	}
	// The product spec (pricing, COGS) and drafts must never reach a buyer.
	for _, forbidden := range []string{
		"docs/spec.md", "docs/plan.md", ".env", "bin/app.exe", "GRAPHIFY-OUT/graph.json",
		"dist/app.exe", "cache/big.bin", "image/sku1/archives/x.zim", "sku1-sell.lock.json",
	} {
		if got[forbidden] {
			t.Errorf("%s must not be in the shipped source archive", forbidden)
		}
	}
}
