package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"survivaldrive/internal/fetch"
	"survivaldrive/internal/image"
	"survivaldrive/internal/lock"
	"survivaldrive/internal/resolve"
)

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	lockPath := fs.String("lock", "", "lock file to build from")
	cacheDir := fs.String("cache", defaultCache(), "cache root")
	outDir := fs.String("out", "", "image directory to create")
	licenseDir := fs.String("licenses", `assets\licenses`, "canonical licence texts")
	allowMissing := fs.Bool("allow-missing", false, "build a partial image instead of failing")
	link := fs.Bool("link", true, "hard-link from cache when on the same volume")
	rehash := fs.Bool("rehash", false, "re-read every file to build _MANIFEST.sha256 (slow, ~10 min for SKU 1)")
	toolsExe := fs.String("tools", filepath.Join("bin", "survival-drive.exe"), "builder binary to ship under tools/ (empty to skip)")
	sourceDir := fs.String("source", ".", "source tree to ship as tools/source.zip (empty to skip)")
	readme := fs.String("readme", filepath.Join("assets", "drive-readme.txt"), "README template rendered onto the drive (empty to skip)")
	runtimeDLLs := fs.Bool("runtime-dlls", true, "copy the MSVC runtime next to the shipped executables")
	systemDir := fs.String("system-dir", `C:\Windows\System32`, "where to read the MSVC runtime from")
	version := fs.String("version", "", "image version string")
	launcher := fs.String("launcher", filepath.Join("bin", "START_SURVIVAL_AI.exe"), "launcher executable to place at the image root (empty to skip)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lockPath == "" {
		return errors.New("build: -lock is required")
	}
	if *outDir == "" {
		return errors.New("build: -out is required")
	}

	l, err := lock.Read(*lockPath)
	if err != nil {
		return err
	}
	if *version == "" {
		*version = defaultVersion(l)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("building %s / %s  (%d items, %s expected)\n\n",
		l.Profile, l.Variant, len(l.Items), humanBytes(l.TotalBytes))

	dlls := []string(nil)
	if *runtimeDLLs {
		dlls = image.DefaultRuntimeDLLs
	}

	opts := image.Options{
		OutDir:       *outDir,
		LicenseDir:   *licenseDir,
		AllowMissing: *allowMissing,
		Link:         *link,
		Rehash:       *rehash,
		Version:      *version,
		LauncherPath: *launcher,
		Tooling: image.Tooling{
			ExePath:   *toolsExe,
			LockPath:  *lockPath,
			SourceDir: *sourceDir,
		},
		ReadmeTemplate: *readme,
		SystemDir:      *systemDir,
		RuntimeDLLs:    dlls,
		Lookup: func(f resolve.File) (string, bool) {
			return fetch.CachePath(*cacheDir, f)
		},
	}
	rep, err := image.Build(ctx, l, opts)

	for _, f := range rep.Files {
		note := ""
		if f.FromZip {
			note = "  (extracted)"
		} else if f.Linked {
			note = "  (linked)"
		}
		fmt.Printf("  %-56s %10s%s\n", truncatePath(f.Dest, 56), humanBytes(f.Bytes), note)
	}
	fmt.Println()
	if len(rep.Missing) > 0 {
		fmt.Printf("%d file(s) missing from cache:\n", len(rep.Missing))
		for _, m := range rep.Missing {
			fmt.Printf("  - %s\n", m)
		}
		fmt.Println()
	}
	if len(rep.MissingLicenses) > 0 {
		fmt.Printf("warning: %d licence text(s) not vendored: %s\n",
			len(rep.MissingLicenses), strings.Join(rep.MissingLicenses, ", "))
	}
	if err != nil {
		return err
	}

	fmt.Printf("%d file(s) | %s placed | %d manifest entries | %s\n",
		len(rep.Files), humanBytes(rep.TotalBytes), rep.ManifestLines,
		rep.Elapsed.Round(time.Second))
	fmt.Printf("image: %s\n", *outDir)
	return nil
}

// defaultVersion derives a stable label from the profile and variant.
func defaultVersion(l lock.Lock) string {
	name := l.Profile
	if name == "" {
		name = l.SKU
	}
	return fmt.Sprintf("%s-%s-%s", name, l.Variant, time.Now().UTC().Format("20060102"))
}

func truncatePath(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-(n-1):]
}
