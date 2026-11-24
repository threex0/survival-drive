package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"survivaldrive/internal/lock"
	"survivaldrive/internal/manifest"
)

// TestDriveReadmeRendersCleanly guards the buyer-facing document: it must be
// pure ASCII (older Notepad reads UTF-8 as ANSI and shows mojibake) and every
// placeholder must be filled, including the public repository URL.
func TestDriveReadmeRendersCleanly(t *testing.T) {
	const tmpl = "../../assets/drive-readme.txt"
	raw, err := os.ReadFile(tmpl)
	if err != nil {
		t.Fatalf("template: %v", err)
	}
	for i, r := range string(raw) {
		if r > 127 {
			t.Fatalf("template has non-ASCII byte at offset %d: %q", i, r)
		}
	}

	l := lock.Lock{
		Profile: "sku1", SKU: "sku1", Variant: "sell",
		Items: []lock.Item{{
			ID: "wiki", Kind: manifest.KindZIM, Bytes: 1 << 30,
			License: "CC-BY-SA-4.0", Attribution: "Wikipedia contributors",
		}},
	}
	out := t.TempDir()
	err = writeReadme(Options{
		OutDir:         out,
		ReadmeTemplate: tmpl,
		Version:        "test-20260101",
		Now:            time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}, l, Report{TotalBytes: 1 << 30, Licenses: []string{"CC-BY-SA-4.0"}}, "sku1-sell.lock.json")
	if err != nil {
		t.Fatalf("writeReadme: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(out, "README.txt"))
	if err != nil {
		t.Fatalf("read README: %v", err)
	}
	text := string(got)
	if strings.Contains(text, "{{") {
		t.Errorf("unfilled placeholder in README.txt")
	}
	if !strings.Contains(text, RepoURL) {
		t.Errorf("README.txt should point at %s", RepoURL)
	}
	for i, r := range text {
		if r > 127 {
			t.Errorf("rendered README has non-ASCII byte at offset %d: %q", i, r)
			break
		}
	}
}

// TestPublishedSourceIsASCII keeps the CLI and dashboard safe in cmd.exe and in
// browsers that ignore a UTF-8 hint. Typographic punctuation belongs in prose,
// not in a program that must print correctly on a stock Windows console.
func TestPublishedSourceIsASCII(t *testing.T) {
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			switch filepath.Ext(p) {
			case ".go", ".html", ".js":
			default:
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for i, r := range string(b) {
				if r > 127 {
					t.Errorf("%s has non-ASCII byte at offset %d: %q", p, i, r)
					return nil
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}
