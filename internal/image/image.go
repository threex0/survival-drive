// Package image assembles the drive tree from the verified cache.
//
// Nothing here downloads. Every file is looked up by digest, copied (or
// hard-linked) into an exFAT-safe layout, and recorded in a manifest so the
// finished media can be verified later without a build machine.
package image

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"survivaldrive/internal/lock"
	"survivaldrive/internal/resolve"
)

// Options configures a build.
type Options struct {
	OutDir string
	// Lookup resolves a lock file to its cached path (injected so this package
	// does not need to know the cache's addressing scheme).
	Lookup func(resolve.File) (string, bool)
	// LicenseDir holds canonical licence texts, one file per licence id.
	LicenseDir string
	// AllowMissing builds a partial image instead of failing.
	AllowMissing bool
	// Link hard-links instead of copying (same volume only); a fallback to copy
	// happens automatically when linking fails.
	Link bool
	// Rehash re-reads every file to build _MANIFEST.sha256. Off by default: files
	// placed from the cache were already verified against the pinned digest at
	// fetch time, and re-hashing 89 GiB costs ~10 minutes per build.
	Rehash bool
	// Version is written to image_version.txt.
	Version string
	// LauncherPath, when set, is copied to the image root as
	// START_SURVIVAL_AI.exe so a buyer has one thing to double-click.
	LauncherPath string
	// Tooling ships the builder, its lock and its source under tools/.
	Tooling Tooling
	// ReadmeTemplate is rendered into README.txt at the image root.
	ReadmeTemplate string
	// RuntimeDLLs are copied next to each executable (e.g. the MSVC runtime).
	RuntimeDLLs []string
	// SystemDir is where RuntimeDLLs are read from.
	SystemDir string
	Now       time.Time
}

// FileResult is one placed file.
type FileResult struct {
	Dest    string
	Bytes   int64
	Linked  bool
	FromZip bool
	ItemID  string
}

// Report summarises a build.
type Report struct {
	Files           []FileResult
	TotalBytes      int64
	Missing         []string
	LintIssues      []string
	Licenses        []string
	MissingLicenses []string
	ManifestLines   int
	Elapsed         time.Duration
}

// Build materialises the image.
func Build(ctx context.Context, l lock.Lock, o Options) (Report, error) {
	if o.OutDir == "" {
		return Report{}, fmt.Errorf("image: out dir is required")
	}
	if o.Lookup == nil {
		return Report{}, fmt.Errorf("image: cache lookup is required")
	}
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	start := time.Now()
	rep := Report{}

	// Plan every destination first so path problems are found before any write.
	type placed struct {
		item lock.Item
		file resolve.File
		dest string
		src  string
	}
	var plan []placed
	seen := make(map[string]string) // lower-case dest -> item id that claimed it
	for _, it := range l.Items {
		for _, f := range it.Files {
			dest := path.Clean(f.Dest)
			if it.ExtractTo != "" {
				dest = path.Clean(it.ExtractTo) // the archive itself is not placed
			}
			if issues := Lint(dest); len(issues) > 0 {
				rep.LintIssues = append(rep.LintIssues, issues...)
				continue
			}
			// Two items writing the same path would silently overwrite each other.
			// This is not hypothetical: every model repo ships "mmproj-F16.gguf",
			// so a shared models/ directory lost one model's vision projector.
			lower := strings.ToLower(dest)
			if owner, dup := seen[lower]; dup {
				rep.LintIssues = append(rep.LintIssues,
					fmt.Sprintf("destination %q claimed by both %q and %q - one would overwrite the other",
						dest, owner, it.ID))
				continue
			}
			seen[lower] = it.ID

			src, ok := o.Lookup(f)
			if !ok {
				if it.Estimated {
					continue // size is an estimate; nothing to place yet
				}
				rep.Missing = append(rep.Missing, it.ID+"/"+f.Name)
				continue
			}
			plan = append(plan, placed{item: it, file: f, dest: dest, src: src})
		}
	}
	if len(rep.LintIssues) > 0 {
		return rep, fmt.Errorf("image: %d path problem(s):\n  %s",
			len(rep.LintIssues), strings.Join(rep.LintIssues, "\n  "))
	}
	if len(rep.Missing) > 0 && !o.AllowMissing {
		return rep, fmt.Errorf("image: %d file(s) not in cache (run fetch first, or use -allow-missing):\n  %s",
			len(rep.Missing), strings.Join(rep.Missing, "\n  "))
	}

	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return rep, err
	}

	for _, p := range plan {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if p.item.ExtractTo != "" {
			n, err := extractZip(p.src, filepath.Join(o.OutDir, filepath.FromSlash(p.dest)))
			if err != nil {
				return rep, fmt.Errorf("extract %s: %w", p.file.Name, err)
			}
			rep.Files = append(rep.Files, FileResult{
				Dest: p.dest, Bytes: n, FromZip: true, ItemID: p.item.ID,
			})
			rep.TotalBytes += n
			continue
		}
		n, linked, err := place(p.src, filepath.Join(o.OutDir, filepath.FromSlash(p.dest)), o.Link)
		if err != nil {
			return rep, err
		}
		rep.Files = append(rep.Files, FileResult{Dest: p.dest, Bytes: n, Linked: linked, ItemID: p.item.ID})
		rep.TotalBytes += n
	}

	// LICENSES/ + NOTICE.md
	lic, missingLic, err := writeLicenses(o, l, rep.Files)
	if err != nil {
		return rep, err
	}
	rep.Licenses = lic
	rep.MissingLicenses = missingLic

	// The launcher is the one file that is ours, not sourced content: copy it to
	// the root so the drive is a single double-click.
	if o.LauncherPath != "" {
		dst := filepath.Join(o.OutDir, "START_SURVIVAL_AI.exe")
		n, _, err := place(o.LauncherPath, dst, false)
		if err != nil {
			return rep, fmt.Errorf("launcher: %w", err)
		}
		rep.Files = append(rep.Files, FileResult{Dest: "START_SURVIVAL_AI.exe", Bytes: n, ItemID: "launcher"})
		rep.TotalBytes += n
	}

	// The MSVC runtime, so a clean Windows needs no redistributable.
	rts, err := copyRuntimeDLLs(o.OutDir, o.RuntimeDLLs, o.SystemDir)
	if err != nil {
		return rep, err
	}
	for _, r := range rts {
		rep.Files = append(rep.Files, r)
		rep.TotalBytes += r.Bytes
	}

	// The builder, its lock and its source, so the drive can be audited.
	toolFiles, lockName, err := writeTooling(o.Tooling, o.OutDir)
	if err != nil {
		return rep, err
	}
	for _, t := range toolFiles {
		rep.Files = append(rep.Files, t)
		rep.TotalBytes += t.Bytes
	}

	// Pinned digests for directly placed files, so the manifest does not have to
	// re-read 89 GiB. Anything extracted or generated is hashed for real.
	known := make(map[string]string, len(plan))
	if !o.Rehash {
		for _, p := range plan {
			// Only trust a well-formed digest; anything else gets hashed for real
			// so a malformed lock cannot poison the manifest.
			if p.item.ExtractTo != "" || !isSHA256Hex(p.file.SHA256) {
				continue
			}
			known[p.file.Dest] = p.file.SHA256
		}
	}

	// README.txt, then the manifest that covers it, then the version stamp.
	if err := writeReadme(o, l, rep, lockName); err != nil {
		return rep, err
	}

	// _MANIFEST.sha256 and image_version.txt
	lines, err := writeManifest(o.OutDir, known)
	if err != nil {
		return rep, err
	}
	rep.ManifestLines = lines
	if err := writeVersion(o, l, rep); err != nil {
		return rep, err
	}

	rep.Elapsed = time.Since(start)
	return rep, nil
}

// Lint reports exFAT hazards for a relative destination path.
//
// These are the failure modes that produce a drive that works on the build
// machine and breaks on a buyer's laptop, so they are hard errors.
func Lint(dest string) []string {
	var issues []string
	clean := path.Clean(dest)
	if clean == "." || clean == "" {
		return []string{"empty destination"}
	}
	if path.IsAbs(clean) || strings.HasPrefix(clean, "/") {
		issues = append(issues, fmt.Sprintf("%q: absolute paths are not allowed", dest))
	}
	if strings.Contains(clean, "..") {
		issues = append(issues, fmt.Sprintf("%q: parent traversal is not allowed", dest))
	}
	if len(clean) > 200 {
		issues = append(issues, fmt.Sprintf("%q: path is %d chars, exFAT targets keep it under 200", dest, len(clean)))
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" {
			continue
		}
		if strings.ContainsAny(part, `<>:"\|?*`) {
			issues = append(issues, fmt.Sprintf("%q: segment %q contains a character exFAT forbids", dest, part))
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			issues = append(issues, fmt.Sprintf("%q: segment %q ends with a dot or space", dest, part))
		}
		base := strings.ToUpper(strings.TrimSuffix(part, path.Ext(part)))
		switch base {
		case "CON", "PRN", "AUX", "NUL",
			"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
			"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
			issues = append(issues, fmt.Sprintf("%q: segment %q is a reserved Windows name", dest, part))
		}
	}
	return issues
}

// place copies or hard-links one file, returning its size.
func place(src, dst string, preferLink bool) (int64, bool, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, false, err
	}
	_ = os.Remove(dst)
	if preferLink {
		if err := os.Link(src, dst); err == nil {
			info, err := os.Stat(dst)
			if err != nil {
				return 0, true, err
			}
			return info.Size(), true, nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, false, fmt.Errorf("open cache file: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, false, err
	}
	n, err := io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, false, fmt.Errorf("copy %s: %w", dst, err)
	}
	return n, false, nil
}

// extractZip unpacks an archive into dir, refusing entries that escape it.
func extractZip(src, dir string) (int64, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return 0, err
	}
	defer zr.Close()

	var total int64
	for _, f := range zr.File {
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || strings.HasPrefix(name, "..") || path.IsAbs(name) {
			return total, fmt.Errorf("unsafe entry %q", f.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return total, err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return total, err
		}
		rc, err := f.Open()
		if err != nil {
			return total, err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return total, err
		}
		n, err := io.Copy(out, rc)
		rc.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// writeLicenses emits LICENSES/NOTICE.md plus every canonical licence text it
// can find, and returns what it wrote and what is still missing.
func writeLicenses(o Options, l lock.Lock, placed []FileResult) (found, missing []string, err error) {
	dir := filepath.Join(o.OutDir, "LICENSES")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}

	// Collect licences actually present, with their attributions.
	type entry struct{ license, attribution, item string }
	var entries []entry
	licSet := map[string]bool{}
	for _, it := range l.Items {
		if it.Estimated {
			continue
		}
		if !hasPlaced(placed, it.ID) {
			continue
		}
		entries = append(entries, entry{it.License, it.Attribution, it.ID})
		licSet[it.License] = true
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].license != entries[j].license {
			return entries[i].license < entries[j].license
		}
		return entries[i].item < entries[j].item
	})

	licenses := make([]string, 0, len(licSet))
	for lic := range licSet {
		licenses = append(licenses, lic)
	}
	sort.Strings(licenses)

	var b strings.Builder
	b.WriteString("# Licences and attribution\n\n")
	fmt.Fprintf(&b, "This image contains %d file(s) from %d item(s) under %d licence(s).\n",
		len(placed), countItems(placed), len(licenses))
	b.WriteString("Software is provided as-is; each component remains under its own licence.\n\n")

	for _, lic := range licenses {
		fmt.Fprintf(&b, "## %s\n\n", lic)
		if withBlank := strings.TrimSpace(knownLicenseNote(lic)); withBlank != "" {
			fmt.Fprintf(&b, "%s\n\n", withBlank)
		}
		b.WriteString("| Item | Attribution |\n|---|---|\n")
		for _, e := range entries {
			if e.license != lic {
				continue
			}
			fmt.Fprintf(&b, "| `%s` | %s |\n", e.item, e.attribution)
		}
		b.WriteString("\n")
		if text, ok := readLicenseText(o.LicenseDir, lic); ok {
			name := licenseFileName(lic)
			_ = os.WriteFile(filepath.Join(dir, name), text, 0o644)
			found = append(found, lic)
			fmt.Fprintf(&b, "Full text: `LICENSES/%s`\n\n", name)
		} else {
			missing = append(missing, lic)
			fmt.Fprintf(&b, "> ! Full licence text not yet vendored for **%s** - see `assets/licenses/`.\n\n", lic)
		}
	}

	// GPLv3 requires the corresponding source to be available. The image ships
	// Kiwix binaries (GPLv3) and our own executables, so name the exact source
	// and make a written offer that stays valid, rather than leaving it implied.
	shipsOurs := o.LauncherPath != "" || o.Tooling.ExePath != "" || o.Tooling.SourceDir != ""
	var gpl []entry
	for _, e := range entries {
		if strings.Contains(e.license, "GPL") {
			gpl = append(gpl, e)
		}
	}
	if shipsOurs || len(gpl) > 0 {
		b.WriteString("## Source and written offer\n\n")
		if shipsOurs {
			b.WriteString("This project's own programs are GPL-3.0-or-later.")
			if o.Tooling.SourceDir != "" {
				b.WriteString(" Their complete source is in `tools/source.zip` on this drive")
			}
			fmt.Fprintf(&b, " and at <%s>.\n\n", RepoURL)
		}
		if len(gpl) > 0 {
			fmt.Fprintf(&b, "The image also includes GPL-licensed programs. For three years from the build date, "+
				"the complete corresponding source for each component below is available from the URL shown, "+
				"and on request at <%s/issues>. This offer is made under GPLv3 section 6.\n\n", RepoURL)
			b.WriteString("| Component | Source |\n|---|---|\n")
			for _, e := range gpl {
				urls := itemSourceURLs(l, e.item)
				src := "on request"
				if len(urls) > 0 {
					src = "`" + urls[0] + "`"
				}
				fmt.Fprintf(&b, "| %s (`%s`) | %s |\n", e.attribution, e.item, src)
				for _, u := range urls[1:] {
					fmt.Fprintf(&b, "| | `%s` |\n", u)
				}
			}
			b.WriteString("\n")
		}
	}

	if err := os.WriteFile(filepath.Join(dir, "NOTICE.md"), []byte(b.String()), 0o644); err != nil {
		return found, missing, err
	}
	return found, missing, nil
}

// itemSourceURLs returns the upstream URLs recorded for an item, de-duplicated.
func itemSourceURLs(l lock.Lock, itemID string) []string {
	var out []string
	seen := map[string]bool{}
	for _, it := range l.Items {
		if it.ID != itemID {
			continue
		}
		for _, f := range it.Files {
			if f.URL == "" || seen[f.URL] {
				continue
			}
			seen[f.URL] = true
			out = append(out, f.URL)
		}
	}
	return out
}

func hasPlaced(placed []FileResult, itemID string) bool {
	for _, p := range placed {
		if p.ItemID == itemID {
			return true
		}
	}
	return false
}

// countItems returns how many distinct items actually put something on the drive.
func countItems(placed []FileResult) int {
	seen := make(map[string]bool, len(placed))
	for _, p := range placed {
		seen[p.ItemID] = true
	}
	return len(seen)
}

// licenseFileName maps a licence id to a stable filename.
func licenseFileName(lic string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ".", "-")
	return r.Replace(strings.ToLower(lic)) + ".txt"
}

// readLicenseText loads a vendored licence text, if present.
func readLicenseText(dir, lic string) ([]byte, bool) {
	if dir == "" {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(dir, licenseFileName(lic)))
	if err != nil {
		return nil, false
	}
	return b, true
}

// knownLicenseNote adds a one-line summary for licences we ship often.
func knownLicenseNote(lic string) string {
	switch lic {
	case "CC-BY-SA-4.0":
		return "Content is licensed CC BY-SA 4.0: attribution required, and derivative works must carry the same licence."
	case "CC-BY-NC-SA-3.0":
		return "NON-COMMERCIAL. Only present in the free `full` variant."
	case "CC-BY-NC-ND-4.0":
		return "NON-COMMERCIAL, no derivatives. Only present in the free `full` variant."
	case "GPL-3.0-or-later":
		return "GPLv3: the corresponding source is offered on request; see the source offer below."
	case "ODbL-1.0":
		return "Map data (c) OpenStreetMap contributors, available under the Open Database Licence."
	default:
		return ""
	}
}

// writeManifest writes _MANIFEST.sha256 over every placed file. Paths in known
// reuse a digest already verified during the fetch instead of re-reading the
// bytes.
func writeManifest(outDir string, known map[string]string) (int, error) {
	var lines []string
	err := filepath.WalkDir(outDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(outDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "_MANIFEST.sha256" || rel == "image_version.txt" {
			return nil
		}
		sum, ok := known[rel]
		if !ok || sum == "" {
			if sum, err = hashFile(p); err != nil {
				return err
			}
		}
		lines = append(lines, sum+"  "+rel)
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Strings(lines)
	body := strings.Join(lines, "\n") + "\n"
	return len(lines), os.WriteFile(filepath.Join(outDir, "_MANIFEST.sha256"), []byte(body), 0o644)
}

// isSHA256Hex reports whether s looks like a sha256 digest.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// hashFile returns the sha256 of a file as lowercase hex.
func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeVersion(o Options, l lock.Lock, rep Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "version: %s\n", o.Version)
	fmt.Fprintf(&b, "profile: %s\nsku: %s\nvariant: %s\n", l.Profile, l.SKU, l.Variant)
	fmt.Fprintf(&b, "built: %s\n", o.Now.Format(time.RFC3339))
	fmt.Fprintf(&b, "files: %d\ntotal_bytes: %d\n", len(rep.Files), rep.TotalBytes)
	fmt.Fprintf(&b, "licenses: %s\n", strings.Join(rep.Licenses, ", "))
	fmt.Fprintf(&b, "missing_licenses: %s\n", strings.Join(rep.MissingLicenses, ", "))
	fmt.Fprintf(&b, "lock_total_bytes: %d\n", l.TotalBytes)
	return os.WriteFile(filepath.Join(o.OutDir, "image_version.txt"), []byte(b.String()), 0o644)
}
