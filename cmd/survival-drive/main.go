// Command survival-drive builds and verifies the offline library image.
//
// M0 implements the `resolve` verb: it queries the upstream catalogues, pins
// every artefact by size and sha256, fits the result to the target drive and
// writes a deterministic lock file. Nothing is downloaded.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"survivaldrive/internal/budget"
	"survivaldrive/internal/lock"
	"survivaldrive/internal/manifest"
	"survivaldrive/internal/resolve"
)

const usage = `survival-drive - offline library builder

Usage:
  survival-drive resolve [flags]
  survival-drive fetch   [flags]

Flags for resolve:
  -profile path    profile to resolve (default profiles/sku1.json)
  -variant name    build variant: sell | full (default sell)
  -out path        write the lock file here (default: report only)
  -strict          fail if any item is unpinned or only estimated
  -jobs n          concurrent resolver workers (default 6)
  -github-token s  GitHub token (default $GITHUB_TOKEN); avoids rate limits

Flags for fetch:
  -lock path       lock file to download (required)
  -cache dir       cache root (default cache)
  -conns n         parallel connections per file (default 12)
  -only ids        comma-separated item ids (smoke tests)
  -max-bytes n     stop after n bytes (smoke tests)
  -dry-run         report what would be fetched, download nothing
  -log dir         throughput CSV directory (default <cache>\..\logs)

Flags for build:
  -lock path         lock file to build from (required)
  -cache dir         cache root (default cache)
  -out dir           image directory to create (required)
  -licenses dir      canonical licence texts (default assets\licenses)
  -allow-missing     build a partial image instead of failing
  -link              hard-link from cache when on the same volume (default true)
  -rehash            re-read every file to build _MANIFEST.sha256 (slow)
  -version s         label written to image_version.txt

Flags for burn:
  -image dir         built image to copy from (required)
  -drive X:          target drive (required)
  -force             copy into a non-empty drive
  -allow-fs          proceed even if the volume is not exFAT
  -buffer n          copy buffer in bytes (default 4 MiB)

Flags for verify:
  -drive X:          drive or directory to check
  -quick             presence and size only (misses corruption)
  -lock path         check cached files against this lock instead of a drive
  -cache dir         cache root used with -lock

Examples:
  survival-drive resolve -profile profiles/sku1.json -variant sell
  survival-drive fetch -lock cache\.ledger\sku1-sell.lock.json -dry-run
  survival-drive fetch -lock cache\.ledger\sku1-sell.lock.json -only bin-llama-cpu,bin-kiwix-tools
  survival-drive build -lock cache\.ledger\sku1-sell.lock.json -out D:\image\sku1-sell
  survival-drive burn  -image D:\image\sku1-sell -drive G: [-resume] [-dry-run]
  survival-drive verify -drive G:
  survival-drive verify -lock cache\.ledger\lite-sell.lock.json  (cache integrity)
`

// defaultCache honours SURVIVAL_CACHE and otherwise uses a predictable
// directory next to the checkout. A full SKU 1 cache is ~90 GiB, so most
// builders will point this at a large drive.
func defaultCache() string {
	if v := os.Getenv("SURVIVAL_CACHE"); v != "" {
		return v
	}
	return "cache"
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command given")
	}
	switch args[0] {
	case "resolve":
		return cmdResolve(args[1:])
	case "fetch":
		return cmdFetch(args[1:])
	case "build":
		return cmdBuild(args[1:])
	case "burn":
		return cmdBurn(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	profilePath := fs.String("profile", filepath.Join("profiles", "sku1.json"), "profile to resolve")
	variantName := fs.String("variant", "sell", "build variant")
	outPath := fs.String("out", "", "write lock file here")
	strict := fs.Bool("strict", false, "fail if any item is unpinned or estimated")
	jobs := fs.Int("jobs", 6, "concurrent resolver workers")
	token := fs.String("github-token", os.Getenv("GITHUB_TOKEN"), "GitHub token")
	if err := fs.Parse(args); err != nil {
		return err
	}

	profile, catalogue, err := manifest.LoadProfile(*profilePath)
	if err != nil {
		return err
	}
	variant, ok := profile.Variants[*variantName]
	if !ok {
		names := make([]string, 0, len(profile.Variants))
		for n := range profile.Variants {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("profile %s has no variant %q (have: %s)",
			profile.ID, *variantName, strings.Join(names, ", "))
	}

	items := manifest.ForVariant(manifest.ForSKU(catalogue, profile.SKU), variant)
	if len(items) == 0 {
		return fmt.Errorf("profile %s variant %s: no items", profile.ID, *variantName)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opts := resolve.DefaultOptions()
	opts.GitHubToken = *token
	reg := resolve.NewRegistry(opts)

	fmt.Fprintf(os.Stderr, "resolving %d items for %s/%s ...\n", len(items), profile.ID, *variantName)
	cands, err := resolveAll(ctx, reg, items, *jobs)
	if err != nil {
		return err
	}

	plan, err := budget.Build(items, cands, profile.CapacityBytes, profile.FillTarget)
	if err != nil {
		return err
	}
	l := lock.FromPlan(plan, profile, *variantName)

	printReport(profile, *variantName, plan, l, cands)

	if u := l.Unpinned(); len(u) > 0 {
		fmt.Fprintf(os.Stderr, "\nwarning: %d file(s) have no sha256 yet: %s\n", len(u), strings.Join(u, ", "))
	}
	if e := l.Estimated(); len(e) > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d item(s) are size estimates only: %s\n", len(e), strings.Join(e, ", "))
	}

	if *strict {
		if err := l.CheckStrict(); err != nil {
			return err
		}
	}

	if *outPath != "" {
		if err := writeLock(*outPath, l); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *outPath)
	}
	return nil
}

// resolveAll resolves items concurrently but returns a deterministic map.
func resolveAll(ctx context.Context, reg *resolve.Registry, items []manifest.Item, jobs int) (map[string][]resolve.Candidate, error) {
	if jobs < 1 {
		jobs = 1
	}
	sem := make(chan struct{}, jobs)
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		out   = make(map[string][]resolve.Candidate, len(items))
		errs  []error
		done  int
		total = len(items)
	)
	for _, it := range items {
		it := it
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()

			c, err := reg.Resolve(ctx, it)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			out[it.ID] = c
			done++
			fmt.Fprintf(os.Stderr, "\r  resolved %d/%d  %-28s", done, total, it.ID)
		}()
	}
	wg.Wait()
	fmt.Fprintln(os.Stderr)
	if len(errs) > 0 {
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		sort.Strings(msgs)
		return nil, errors.New(strings.Join(msgs, "\n"))
	}
	return out, nil
}

func printReport(profile manifest.Profile, variant string, plan budget.Plan, l lock.Lock, cands map[string][]resolve.Candidate) {
	fmt.Printf("\n%s - %s / %s\n", profile.Name, profile.ID, variant)
	fmt.Printf("capacity %s | fit target %.0f%% (%s) | fill %.1f%%\n\n",
		budget.Human(profile.CapacityBytes), profile.FillTarget*100,
		budget.Human(plan.LimitBytes), plan.FillFraction()*100)

	fmt.Printf("%-26s %-14s %-10s %9s  %s\n", "ITEM", "CAND", "KIND", "SIZE", "SRC")
	fmt.Println(strings.Repeat("-", 78))
	for _, c := range plan.Choices {
		src := "pinned"
		if c.Candidate.Estimated {
			src = "ESTIMATE"
		} else if len(c.Candidate.Unpinned()) > 0 {
			src = "unpinned"
		}
		label := c.Candidate.Label
		if len(cands[c.Item.ID]) > 1 {
			label = label + "*"
		}
		fmt.Printf("%-26s %-14s %-10s %9s  %s\n",
			c.Item.ID, truncate(label, 14), c.Item.Kind, budget.Human(c.Bytes()), src)
	}
	fmt.Println(strings.Repeat("-", 78))
	fmt.Printf("%-26s %-9s %-10s %9s\n", "TOTAL", "", fmt.Sprintf("%d items", len(plan.Choices)),
		budget.Human(plan.TotalBytes))
	fmt.Printf("  pinned %s + estimated %s | headroom inside target %s | free on drive %s\n",
		budget.Human(plan.ResolvedBytes), budget.Human(plan.EstimatedBytes),
		budget.Human(plan.Headroom()), budget.Human(plan.Free()))

	if len(plan.Skipped) > 0 {
		fmt.Printf("\nskipped (did not fit):\n")
		for _, s := range plan.Skipped {
			fmt.Printf("  %-24s %s\n", s.Item.ID, s.Reason)
		}
	}
	if len(cands) > 0 {
		fmt.Printf("\n* = flavour chosen from alternatives; %d files in lock\n", l.TotalFiles())
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func writeLock(path string, l lock.Lock) error {
	b, err := l.Marshal()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Write via a temp file so a crash never leaves a half-written lock.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
