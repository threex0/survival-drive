package image

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"survivaldrive/internal/lock"
)

// readmeData is what the template is filled with. Every number comes from the
// lock and the build, so the file on the drive cannot drift from its contents.
type readmeData struct {
	Product   string
	Version   string
	SKU       string
	Built     string
	Files     int
	GiB       string
	Books     int
	BooksGiB  string
	Models    int
	ModelsGiB string
	Licenses  string
	LockFile  string
	Repo      string
}

// writeReadme renders assets/drive-readme.txt into the image root as README.txt.
func writeReadme(o Options, l lock.Lock, rep Report, lockName string) error {
	tmpl := o.ReadmeTemplate
	if tmpl == "" {
		return nil
	}
	raw, err := os.ReadFile(tmpl)
	if err != nil {
		return fmt.Errorf("readme template: %w", err)
	}

	var books, models int
	var booksBytes, modelsBytes int64
	for _, it := range l.Items {
		switch it.Kind {
		case "zim":
			books++
			booksBytes += it.Bytes
		case "gguf":
			models++
			modelsBytes += it.Bytes
		}
	}

	// Count what is actually on the drive, not the items placed: an extracted
	// archive is one item but dozens of files, and the README must agree with
	// _MANIFEST.sha256 (+2 for README.txt and image_version.txt).
	files, err := countFiles(o.OutDir)
	if err != nil {
		return err
	}

	d := readmeData{
		Product:   productName(l),
		Version:   o.Version,
		SKU:       l.SKU,
		Built:     o.Now.Format("2006-01-02"),
		Files:     files + 2,
		GiB:       gib(rep.TotalBytes),
		Books:     books,
		BooksGiB:  gib(booksBytes),
		Models:    models,
		ModelsGiB: gib(modelsBytes),
		Licenses:  strings.Join(rep.Licenses, ", "),
		LockFile:  lockName,
		Repo:      RepoURL,
	}
	if d.LockFile == "" {
		d.LockFile = "sku.lock.json"
	}

	out := string(raw)
	for _, kv := range [][2]string{
		{"{{PRODUCT}}", d.Product},
		{"{{VERSION}}", d.Version},
		{"{{SKU}}", d.SKU},
		{"{{BUILT}}", d.Built},
		{"{{FILES}}", fmt.Sprint(d.Files)},
		{"{{GIB}}", d.GiB},
		{"{{BOOKS}}", fmt.Sprint(d.Books)},
		{"{{BOOKS_GIB}}", d.BooksGiB},
		{"{{MODELS}}", fmt.Sprint(d.Models)},
		{"{{MODELS_GIB}}", d.ModelsGiB},
		{"{{LICENSES}}", d.Licenses},
		{"{{LOCKFILE}}", d.LockFile},
		{"{{REPO}}", d.Repo},
	} {
		out = strings.ReplaceAll(out, kv[0], kv[1])
	}

	return os.WriteFile(filepath.Join(o.OutDir, "README.txt"), []byte(out), 0o644)
}

// productName is a human label for the build.
func productName(l lock.Lock) string {
	switch {
	case l.Profile == "lite":
		return "64 GB Field Edition"
	case l.SKU == "sku1":
		return "128 GB Edition"
	case l.SKU == "sku2":
		return "Bunker Edition"
	default:
		return l.SKU
	}
}

func gib(n int64) string {
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20))
}

// copyRuntimeDLLs places the MSVC runtime next to each executable that needs it.
//
// Measured on the shipped binaries: kiwix-serve.exe imports VCRUNTIME140,
// VCRUNTIME140_1 and MSVCP140; llama-server.exe imports VCRUNTIME140. A clean
// Windows install without the VC++ redistributable cannot start either of them,
// and the failure looks like "the encyclopedia does not work" rather than a
// missing dependency. Microsoft permits redistributing these files with an
// application, so the drive carries them and has no prerequisites at all.
func copyRuntimeDLLs(outDir string, names []string, sysDir string) ([]FileResult, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var dirs []string
	binRoot := filepath.Join(outDir, "bin")
	err := filepath.WalkDir(binRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a missing bin/ is not an error here
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".exe") {
			dir := filepath.Dir(p)
			for _, existing := range dirs {
				if existing == dir {
					return nil
				}
			}
			dirs = append(dirs, dir)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(dirs)

	var out []FileResult
	for _, dir := range dirs {
		for _, name := range names {
			src := filepath.Join(sysDir, name)
			if _, err := os.Stat(src); err != nil {
				continue // not on this machine; documented as a requirement
			}
			dst := filepath.Join(dir, name)
			if _, err := os.Stat(dst); err == nil {
				continue // already shipped with the archive
			}
			n, _, err := place(src, dst, false)
			if err != nil {
				return out, fmt.Errorf("runtime %s: %w", name, err)
			}
			rel, _ := filepath.Rel(outDir, dst)
			out = append(out, FileResult{Dest: filepath.ToSlash(rel), Bytes: n, ItemID: "runtime"})
		}
	}
	return out, nil
}

// DefaultRuntimeDLLs are the MSVC runtime files the shipped binaries import.
var DefaultRuntimeDLLs = []string{
	"vcruntime140.dll",
	"vcruntime140_1.dll",
	"msvcp140.dll",
}

// countFiles counts regular files under dir.
func countFiles(dir string) (int, error) {
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n, err
}
