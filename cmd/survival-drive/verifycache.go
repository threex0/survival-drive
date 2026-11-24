package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"survivaldrive/internal/fetch"
	"survivaldrive/internal/lock"
)

// verifyCache re-hashes every cached artefact against the lock.
//
// This exists because the staging volume is not implicitly trustworthy: an SMR
// HDD produced exactly one silently corrupt file in a 33-file cache, and the
// only reason it was caught is the post-copy verification on a USB stick. A
// cache check turns that discovery into a 10-minute pre-flight instead.
func verifyCache(lockPath, cacheDir string) error {
	l, err := lock.Read(lockPath)
	if err != nil {
		return err
	}

	type job struct {
		id, name, path, want string
		size                 int64
	}
	var jobs []job
	var unpinned int
	for _, it := range l.Items {
		if it.Estimated {
			continue
		}
		for _, f := range it.Files {
			path, ok := fetch.CachePath(cacheDir, f)
			if !ok {
				jobs = append(jobs, job{id: it.ID, name: f.Name, want: "<missing>"})
				continue
			}
			if f.SHA256 == "" {
				unpinned++
				continue
			}
			jobs = append(jobs, job{id: it.ID, name: f.Name, path: path, want: f.SHA256, size: f.SizeBytes})
		}
	}
	if len(jobs) == 0 {
		return fmt.Errorf("verify: lock has no files to check")
	}

	fmt.Printf("verifying %d cached file(s) against %s ...\n", len(jobs), lockPath)
	start := time.Now()
	var bad []string
	var bytes int64
	lastPrint := start
	for i, j := range jobs {
		if j.path == "" {
			bad = append(bad, j.id+"/"+j.name+" (not in cache)")
		} else {
			got, size, err := hashPath(j.path)
			bytes += size
			switch {
			case err != nil:
				bad = append(bad, j.id+"/"+j.name+" ("+err.Error()+")")
			case !strings.EqualFold(got, j.want):
				bad = append(bad, j.id+"/"+j.name)
			}
		}
		if time.Since(lastPrint) > 2*time.Second || i+1 == len(jobs) {
			lastPrint = time.Now()
			rate := 0.0
			if secs := time.Since(start).Seconds(); secs > 0.5 {
				rate = float64(bytes) / secs / (1 << 20)
			}
			fmt.Printf("\r  %d/%d files | %s | %.0f MB/s   ", i+1, len(jobs), humanBytes(bytes), rate)
		}
	}
	fmt.Println()

	if len(bad) > 0 {
		sort.Strings(bad)
		fmt.Printf("FAILED: %d of %d file(s) do not match the pin:\n", len(bad), len(jobs))
		for _, b := range bad {
			fmt.Printf("  - %s\n", b)
		}
		fmt.Println("\nre-fetch each one with:  survival-drive fetch -lock ... -only <item-id>")
		return fmt.Errorf("verify: %d cached file(s) failed", len(bad))
	}
	fmt.Printf("OK: %d cached files intact in %s\n", len(jobs), time.Since(start).Round(time.Second))
	if unpinned > 0 {
		fmt.Printf("note: %d file(s) have no pinned sha256 and were not checked\n", unpinned)
	}
	return nil
}

func hashPath(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
