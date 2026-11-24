package media_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"survivaldrive/internal/media"
)

// makeImage creates a tiny image with a manifest.
func makeImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "archives/a.zim", "alpha")
	write(t, dir, "models/m.gguf", "beta")
	write(t, dir, "_MANIFEST.sha256", "")

	entries := []string{
		sha256Hex("alpha") + "  archives/a.zim",
		sha256Hex("beta") + "  models/m.gguf",
	}
	write(t, dir, "_MANIFEST.sha256", strings.Join(entries, "\n")+"\n")
	write(t, dir, "_MANIFEST.sizes", "5\tarchives/a.zim\n4\tmodels/m.gguf\n")
	return dir
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func opts() media.CopyOptions {
	return media.CopyOptions{
		FreeBytes:    func(string) (int64, error) { return 1 << 40, nil },
		RequireEmpty: true,
		Now:          func() time.Time { return time.Unix(0, 0) },
	}
}

func TestCopyWritesAndVerifies(t *testing.T) {
	img := makeImage(t)
	target := t.TempDir()

	rep, err := media.Copy(context.Background(), img, target, opts())
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if !rep.Verified {
		t.Error("copy should verify what it wrote")
	}
	if rep.Files != 4 {
		t.Errorf("files = %d, want 4", rep.Files)
	}
	for _, rel := range []string{"archives/a.zim", "models/m.gguf", "_MANIFEST.sha256", "_MANIFEST.sizes"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel))); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
}

func TestCopyDryRunWritesNothing(t *testing.T) {
	img := makeImage(t)
	target := t.TempDir()
	o := opts()
	o.DryRun = true
	rep, err := media.Copy(context.Background(), img, target, o)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if rep.Files != 4 {
		t.Errorf("planned files = %d, want 4", rep.Files)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Errorf("dry run wrote %d entries", len(entries))
	}
}

func TestCopyResumesAndRepairs(t *testing.T) {
	img := makeImage(t)
	target := t.TempDir()

	// Simulate an interrupted burn: one file complete, one truncated.
	write(t, target, "archives/a.zim", "alpha")
	write(t, target, "models/m.gguf", "be") // truncated, wrong size

	o := opts()
	o.RequireEmpty = false
	o.Resume = true
	rep, err := media.Copy(context.Background(), img, target, o)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if rep.Skipped != 1 { // only the complete file; the manifest was absent
		t.Errorf("skipped = %d, want 1 (already-present file)", rep.Skipped)
	}
	if !rep.Verified {
		t.Error("resume should end verified")
	}
	if got, _ := os.ReadFile(filepath.Join(target, "models", "m.gguf")); string(got) != "beta" {
		t.Errorf("truncated file not repaired: %q", got)
	}
}

func TestCopyRepairsCorruptionOnResume(t *testing.T) {
	img := makeImage(t)
	target := t.TempDir()

	// Right size, wrong content: a size check cannot see this, verification must.
	write(t, target, "archives/a.zim", "alphA")
	write(t, target, "models/m.gguf", "beta")

	o := opts()
	o.RequireEmpty = false
	o.Resume = true
	rep, err := media.Copy(context.Background(), img, target, o)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if rep.Repaired != 1 {
		t.Errorf("repaired = %d, want 1", rep.Repaired)
	}
	if got, _ := os.ReadFile(filepath.Join(target, "archives", "a.zim")); string(got) != "alpha" {
		t.Errorf("corrupt file not repaired: %q", got)
	}
	if !rep.Verified {
		t.Error("should finish verified")
	}
}

func TestCopyRefusesNonEmptyTarget(t *testing.T) {
	img := makeImage(t)
	target := t.TempDir()
	write(t, target, "leftover.txt", "x")

	if _, err := media.Copy(context.Background(), img, target, opts()); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Fatalf("error = %v, want a not-empty refusal", err)
	}
}

func TestCopyHoursSystemVolumeInformation(t *testing.T) {
	// Every freshly formatted Windows volume contains this directory; it must not
	// make the drive look "in use".
	img := makeImage(t)
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "System Volume Information"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := media.Copy(context.Background(), img, target, opts()); err != nil {
		t.Fatalf("copy should ignore System Volume Information: %v", err)
	}
}

func TestCopyRefusesWhenTargetIsSmall(t *testing.T) {
	img := makeImage(t)
	o := opts()
	o.FreeBytes = func(string) (int64, error) { return 4, nil }
	if _, err := media.Copy(context.Background(), img, t.TempDir(), o); err == nil ||
		!strings.Contains(err.Error(), "free") {
		t.Fatalf("error = %v, want a space refusal", err)
	}
}

func TestVerifyDetectsCorruptionAndTruncation(t *testing.T) {
	img := makeImage(t)
	target := t.TempDir()
	if _, err := media.Copy(context.Background(), img, target, opts()); err != nil {
		t.Fatalf("copy: %v", err)
	}

	// Corrupt one byte in place (same size, different content).
	p := filepath.Join(target, "archives", "a.zim")
	if err := os.WriteFile(p, []byte("alphA"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad, err := media.Verify(context.Background(), target, media.VerifyOptions{Full: true})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(bad) != 1 || bad[0] != "archives/a.zim" {
		t.Fatalf("bad = %v, want [archives/a.zim]", bad)
	}

	// A quick check compares sizes from _MANIFEST.sizes: same-size corruption
	// is invisible, but a truncation is not.
	if bad, _ := media.Verify(context.Background(), target, media.VerifyOptions{Full: false}); len(bad) != 0 {
		t.Errorf("quick verify should miss same-size corruption, got %v", bad)
	}
	if err := os.WriteFile(p, []byte("alp"), 0o644); err != nil {
		t.Fatal(err)
	}
	if bad, _ := media.Verify(context.Background(), target, media.VerifyOptions{Full: false}); len(bad) != 1 || bad[0] != "archives/a.zim" {
		t.Errorf("quick verify should flag a truncation, got %v", bad)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if bad, _ := media.Verify(context.Background(), target, media.VerifyOptions{Full: false}); len(bad) != 1 {
		t.Errorf("size-only verify should flag a missing file, got %v", bad)
	}
}

func TestReadManifestRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "_MANIFEST.sha256", "not-a-hash  archives/a.zim\n")
	if _, err := media.ReadManifest(dir); err == nil {
		t.Fatal("want an error for a malformed line")
	}
}

func TestReadManifestAllowsSpacesInPaths(t *testing.T) {
	dir := t.TempDir()
	sum := strings.Repeat("a", 64)
	write(t, dir, "_MANIFEST.sha256", sum+"  library_pdfs/Are You Ready.pdf\n")
	entries, err := media.ReadManifest(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "library_pdfs/Are You Ready.pdf" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestReadManifestReadsSizeSidecar(t *testing.T) {
	dir := t.TempDir()
	sum := strings.Repeat("a", 64)
	write(t, dir, "_MANIFEST.sha256", sum+"  archives/a.zim\n")
	write(t, dir, "_MANIFEST.sizes", "11\tarchives/a.zim\n")

	entries, err := media.ReadManifest(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(entries) != 1 || entries[0].Size != 11 {
		t.Fatalf("entries = %+v, want size 11", entries)
	}
}

func TestReadManifestWithoutSizeSidecar(t *testing.T) {
	dir := t.TempDir()
	sum := strings.Repeat("a", 64)
	write(t, dir, "_MANIFEST.sha256", sum+"  archives/a.zim\n")

	entries, err := media.ReadManifest(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if entries[0].Size != 0 {
		t.Fatalf("size = %d, want 0 when the sidecar is absent", entries[0].Size)
	}
}
