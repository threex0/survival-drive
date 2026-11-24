package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"survivaldrive/internal/media"
)

func cmdBurn(args []string) error {
	fs := flag.NewFlagSet("burn", flag.ContinueOnError)
	imageDir := fs.String("image", "", "image directory to copy from")
	drive := fs.String("drive", "", "target drive, e.g. E: or E:\\")
	force := fs.Bool("force", false, "copy into a non-empty drive")
	resume := fs.Bool("resume", true, "skip files already on the drive at the right size, repair failures")
	dryRun := fs.Bool("dry-run", false, "report what would be copied and how much space it needs")
	allowFS := fs.Bool("allow-fs", false, "proceed even if the volume is not exFAT")
	buffer := fs.Int("buffer", 4<<20, "copy buffer size in bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *imageDir == "" {
		return errors.New("burn: -image is required")
	}
	if *drive == "" {
		return errors.New("burn: -drive is required")
	}

	root, err := normalizeRoot(*drive)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(*imageDir, "_MANIFEST.sha256")); err != nil {
		return fmt.Errorf("burn: %s is not a built image (no _MANIFEST.sha256); run build first", *imageDir)
	}

	info, err := media.Inspect(root)
	if err != nil {
		return fmt.Errorf("burn: cannot inspect %s: %w", root, err)
	}
	fmt.Printf("target %s  label %q  %s  %s free of %s\n",
		root, info.Label, info.FileSystem, humanBytes(info.Free), humanBytes(info.Total))
	if !strings.EqualFold(info.FileSystem, "exFAT") && !*allowFS {
		return fmt.Errorf("burn: %s is %s, not exFAT (use -allow-fs to override)",
			root, info.FileSystem)
	}
	if info.Label != "SURVIVAL_AI" {
		fmt.Printf("note: volume label is %q; the recommended label is SURVIVAL_AI\n", info.Label)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	verifyStart := time.Now()
	last := time.Now()
	opts := media.CopyOptions{
		BufferSize:   *buffer,
		RequireEmpty: !*force && !*resume,
		Resume:       *resume,
		DryRun:       *dryRun,
		FreeBytes:    media.FreeBytes,
		VerifyProgress: func(done, total int, bytes int64) {
			if done == 0 {
				// The clock starts here, not at the beginning of the copy: the
				// verification rate should describe the read-back, not be diluted
				// by two and a half hours of writing.
				verifyStart = time.Now()
				fmt.Println("\nverifying (reading every file back off the drive) ...")
				return
			}
			if time.Since(last) < 2*time.Second && done != total {
				return
			}
			last = time.Now()
			elapsed := time.Since(verifyStart).Seconds()
			rate := 0.0
			if elapsed > 0.5 {
				rate = float64(bytes) / elapsed / (1 << 20)
			}
			fmt.Printf("\r  verified %d/%d files | %s | %.1f MB/s   ", done, total, humanBytes(bytes), rate)
			if done == total {
				fmt.Println()
			}
		},
		Progress: func(done, total int64, elapsed time.Duration) {
			if time.Since(last) < 2*time.Second {
				return
			}
			last = time.Now()
			rate := float64(done) / elapsed.Seconds()
			eta := "?"
			if rate > 0 {
				eta = (time.Duration(float64(total-done)/rate) * time.Second).Round(time.Second).String()
			}
			fmt.Printf("\r  %s / %s  %5.1f MB/s  eta %s   ",
				humanBytes(done), humanBytes(total), rate/(1<<20), eta)
		},
	}

	fmt.Println("\ncopying ...")
	verifyStart = time.Now()
	rep, err := media.Copy(ctx, *imageDir, root, opts)
	fmt.Print("\r" + strings.Repeat(" ", 70) + "\r")
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Printf("dry run: would copy %d file(s), %s | %d file(s) already present would be kept\n",
			rep.Files, humanBytes(rep.Bytes), rep.Skipped)
		return nil
	}

	fmt.Printf("copied %d files | %s in %s (%.1f MB/s)\n",
		rep.Files, humanBytes(rep.Bytes), rep.Elapsed.Round(time.Second),
		float64(rep.Bytes)/math.Max(rep.Elapsed.Seconds(), 0.5)/(1<<20))
	if rep.Skipped > 0 {
		fmt.Printf("resumed: %d file(s), %s already present, were left in place\n",
			rep.Skipped, humanBytes(rep.SkippedBytes))
	}
	if rep.Repaired > 0 {
		fmt.Printf("repaired: %d file(s) failed verification and were re-copied\n", rep.Repaired)
	}
	if rep.Verified {
		fmt.Println("verified: every file matches _MANIFEST.sha256")
	}
	fmt.Printf("\nDrive ready: %s\n", root)
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	drive := fs.String("drive", "", "drive or directory to verify")
	lockPath := fs.String("lock", "", "verify cache contents against this lock instead of a drive")
	cacheDir := fs.String("cache", defaultCache(), "cache root (with -lock)")
	quick := fs.Bool("quick", false, "check presence and size only (misses corruption)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lockPath != "" {
		return verifyCache(*lockPath, *cacheDir)
	}
	if *drive == "" {
		return errors.New("verify: -drive or -lock is required")
	}

	root, err := normalizeRoot(*drive)
	if err != nil {
		return err
	}
	entries, err := media.ReadManifest(root)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mode := "full sha256"
	if *quick {
		// Size checks need the _MANIFEST.sizes sidecar written by newer builds.
		mode = "presence only (old drive, no size index)"
		for _, e := range entries {
			if e.Size > 0 {
				mode = "presence and size"
				break
			}
		}
	}
	fmt.Printf("verifying %d entries on %s (%s) ...\n", len(entries), root, mode)
	start := time.Now()
	lastPrint := start
	bad, err := media.Verify(ctx, root, media.VerifyOptions{
		Full: !*quick,
		Progress: func(done, total int, bytes int64) {
			if done == total {
				fmt.Printf("\r  %d/%d files | %s   \n", done, total, humanBytes(bytes))
				return
			}
			if time.Since(lastPrint) < 2*time.Second {
				return
			}
			lastPrint = time.Now()
			rate := 0.0
			if secs := time.Since(start).Seconds(); secs > 0.5 {
				rate = float64(bytes) / secs / (1 << 20)
			}
			fmt.Printf("\r  %d/%d files | %s | %.1f MB/s   ", done, total, humanBytes(bytes), rate)
		},
	})
	if err != nil {
		return err
	}
	elapsed := time.Since(start)

	if len(bad) == 0 {
		fmt.Printf("OK: %d files intact in %s\n", len(entries), elapsed.Round(time.Second))
		return nil
	}
	fmt.Printf("FAILED: %d of %d file(s) do not match:\n", len(bad), len(entries))
	for i, p := range bad {
		if i >= 20 {
			fmt.Printf("  ... and %d more\n", len(bad)-i)
			break
		}
		fmt.Printf("  - %s\n", p)
	}
	return fmt.Errorf("verify: %d file(s) failed", len(bad))
}

// normalizeRoot turns "E:" or "E:\" into "E:\".
func normalizeRoot(drive string) (string, error) {
	d := strings.TrimSpace(drive)
	d = strings.TrimSuffix(d, "\"")
	if len(d) >= 2 && d[1] == ':' {
		d = d[:2]
	}
	if len(d) != 2 || d[1] != ':' {
		return "", fmt.Errorf("%q is not a drive letter (expected E: or E:\\)", drive)
	}
	root := strings.ToUpper(d[:1]) + `:\`
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("drive %s is not available: %w", root, err)
	}
	return root, nil
}
