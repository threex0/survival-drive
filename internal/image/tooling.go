package image

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Tooling describes the extras that make a drive auditable: the builder binary,
// the lock it was built from, and its source. Shipping these turns "trust me"
// into "check for yourself", which is the point of the checksum manifest.
type Tooling struct {
	// ExePath is the builder binary, placed at tools/survival-drive.exe.
	ExePath string
	// LockPath is the lock this image was built from, placed beside the tool.
	LockPath string
	// SourceDir is zipped to tools/source.zip so the build can be audited.
	SourceDir string
}

// sourceSkipDirs are repository-root paths that must not end up in the source
// archive. They are matched against the relative path, never the base name:
// internal/image is source code, while a top-level image/ is a build artefact.
//
// "docs" is excluded deliberately. It holds the product spec (pricing and
// marketing), the COGS analysis and the drafted licence enquiries - material
// that belongs in the private repository, never on a drive handed to a buyer.
var sourceSkipDirs = map[string]bool{
	"bin": true, "graphify-out": true, "logs": true, "docs": true,
	// Working directories a builder may create inside the checkout.
	"cache": true, "image": true, "dist": true,
}

// sourceSkipNames are directories skipped at any depth.
var sourceSkipNames = map[string]bool{".git": true, "node_modules": true}

// sourceSkipFiles are files that may hold secrets or local state.
var sourceSkipFiles = map[string]bool{".env": true}

// maxSourceFile keeps a stray build artefact from bloating the archive.
const maxSourceFile = 8 << 20

// writeTooling places the builder, the lock and a source archive, and returns
// the lock's file name (for the README) plus what it placed.
func writeTooling(t Tooling, outDir string) ([]FileResult, string, error) {
	var out []FileResult
	lockName := ""

	if t.ExePath != "" {
		dst := filepath.Join(outDir, "tools", "survival-drive.exe")
		n, _, err := place(t.ExePath, dst, false)
		if err != nil {
			return out, "", fmt.Errorf("tooling exe: %w", err)
		}
		out = append(out, FileResult{Dest: "tools/survival-drive.exe", Bytes: n, ItemID: "tooling"})
	}
	if t.LockPath != "" {
		lockName = filepath.Base(t.LockPath)
		n, _, err := place(t.LockPath, filepath.Join(outDir, "tools", lockName), false)
		if err != nil {
			return out, lockName, fmt.Errorf("tooling lock: %w", err)
		}
		out = append(out, FileResult{Dest: "tools/" + lockName, Bytes: n, ItemID: "tooling"})
	}
	if t.SourceDir != "" {
		dst := filepath.Join(outDir, "tools", "source.zip")
		n, err := ZipSource(t.SourceDir, dst)
		if err != nil {
			return out, lockName, fmt.Errorf("tooling source: %w", err)
		}
		out = append(out, FileResult{Dest: "tools/source.zip", Bytes: n, ItemID: "tooling"})
	}
	return out, lockName, nil
}

// ZipSource archives a source tree, skipping build output (bin, graphify-out),
// secrets (.env), and the private docs/ directory. Exported so the exclusion
// rules can be tested: shipping the product spec to a buyer would be a leak.
func ZipSource(srcDir, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	zw := zip.NewWriter(f)

	var files []string
	err = filepath.WalkDir(srcDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if sourceSkipDirs[strings.ToLower(rel)] || sourceSkipNames[strings.ToLower(d.Name())] {
				return filepath.SkipDir
			}
			return nil
		}
		if sourceSkipFiles[strings.ToLower(d.Name())] {
			return nil
		}
		// Locks are derived and large; a fresh one is produced by `resolve`.
		if strings.HasSuffix(strings.ToLower(rel), ".lock.json") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxSourceFile {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		zw.Close()
		f.Close()
		return 0, err
	}
	sort.Strings(files)

	for _, rel := range files {
		in, err := os.Open(filepath.Join(srcDir, filepath.FromSlash(rel)))
		if err != nil {
			zw.Close()
			f.Close()
			return 0, err
		}
		w, err := zw.Create(rel)
		if err != nil {
			in.Close()
			zw.Close()
			f.Close()
			return 0, err
		}
		_, copyErr := io.Copy(w, in)
		in.Close()
		if copyErr != nil {
			zw.Close()
			f.Close()
			return 0, copyErr
		}
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
