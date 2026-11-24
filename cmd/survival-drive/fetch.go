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
	"survivaldrive/internal/lock"
)

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	lockPath := fs.String("lock", "", "lock file to download")
	cacheDir := fs.String("cache", defaultCache(), "cache root")
	conns := fs.Int("conns", fetch.DefaultConns, "parallel connections per file")
	only := fs.String("only", "", "comma-separated item ids")
	maxBytes := fs.Int64("max-bytes", 0, "stop after n bytes")
	dryRun := fs.Bool("dry-run", false, "report only, download nothing")
	logDir := fs.String("log", "", "throughput CSV directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *lockPath == "" {
		return errors.New("fetch: -lock is required")
	}

	l, err := lock.Read(*lockPath)
	if err != nil {
		return err
	}
	if *logDir == "" {
		*logDir = filepath.Join(filepath.Dir(*cacheDir), "logs")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("%s / %s  (%d items, %s)\n", l.Profile, l.Variant, len(l.Items), humanBytes(l.TotalBytes))
	if *dryRun {
		fmt.Println("dry run: nothing will be downloaded")
	}

	opts := fetch.Options{
		CacheDir:  *cacheDir,
		LogDir:    *logDir,
		Conns:     *conns,
		DryRun:    *dryRun,
		MaxBytes:  *maxBytes,
		Only:      splitList(*only),
		Progress:  func(s string) { fmt.Println(s) },
		PieceSize: fetch.DefaultPieceSize,
	}
	start := time.Now()
	sum, err := fetch.Run(ctx, l, opts)
	fmt.Println()
	fmt.Printf("%d files | %s fetched | %s reused | %s elapsed\n",
		len(sum.Files), humanBytes(sum.FetchedBytes),
		humanBytes(sum.TotalBytes-sum.FetchedBytes), time.Since(start).Round(time.Second))
	if err != nil {
		return err
	}
	if upgraded := countUpgraded(sum.Files); upgraded > 0 {
		fmt.Printf("%d file(s) had no pinned sha256; their digests are recorded in the lock report\n", upgraded)
	}
	return nil
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func countUpgraded(files []fetch.FileResult) int {
	n := 0
	for _, f := range files {
		if f.Upgraded {
			n++
		}
	}
	return n
}

// humanBytes renders sizes consistently with the resolve report.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
