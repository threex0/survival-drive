// Package fetch downloads lock entries into a content-addressed cache.
//
// Guarantees:
//   - resumable: completed pieces are recorded in a side state file, so a crash
//     or Ctrl-C costs at most a few pieces;
//   - verified: every finished file is hashed against the lock's sha256, and
//     Kiwix files are additionally checked per 4 MiB piece while downloading;
//   - resumable mirrors: the descriptor is re-read for current mirrors, so a
//     dead or throttled mirror is rotated away rather than retried forever;
//   - single sequential writer per file, because the staging volume is SMR.
package fetch

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"survivaldrive/internal/lock"
	"survivaldrive/internal/metalink"
	"survivaldrive/internal/resolve"
)

// UserAgent is sent on every request.
const UserAgent = "survival-drive/0.1 (+M1 fetcher)"

// Defaults chosen from the preflight throughput measurements.
const (
	DefaultConns     = 12
	DefaultPieceSize = 8 << 20
	// SpaceFactor leaves room for the lock/state files and the filesystem's own
	// appetite; a build that just barely fits is a build that fails halfway.
	SpaceFactor = 1.1
	spaceSlack  = 512 << 20
)

// newClient builds an HTTP client tuned for many parallel ranged requests to the
// same host.
//
// HTTP/2 is deliberately disabled: with a dozen ranged streams over a single TCP
// connection, a single lost packet stalls every stream (head-of-line blocking),
// which measured ~10x slower than one connection here. HTTP/1.1 gives each worker
// its own connection, which is what aria2 and curl do.
func newClient(conns int) *http.Client {
	if conns < 4 {
		conns = 4
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          conns * 2,
		MaxIdleConnsPerHost:   conns * 2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Minute}
}

// freeSpace is a variable so tests can simulate a full disk.
var freeSpace = FreeBytes

// Options configures a fetch run.
type Options struct {
	CacheDir  string
	LogDir    string
	Conns     int
	PieceSize int64
	Client    *http.Client
	DryRun    bool
	// Only limits the run to specific item ids (smoke tests).
	Only []string
	// MaxBytes stops scheduling new files after this many bytes (smoke tests).
	MaxBytes int64
	Progress func(string)
}

// FileResult records what happened to one file.
type FileResult struct {
	ID       string
	Dest     string
	Name     string
	Size     int64
	Reused   bool
	Elapsed  time.Duration
	SHA256   string
	Upgraded bool // had no pinned hash; digest recorded for later
	Mirrors  int
}

// Summary is the outcome of a run.
type Summary struct {
	Files        []FileResult
	TotalBytes   int64
	FetchedBytes int64
	Elapsed      time.Duration
}

// Run fetches every file in the lock that is not already cached.
func Run(ctx context.Context, l lock.Lock, o Options) (Summary, error) {
	if o.CacheDir == "" {
		return Summary{}, fmt.Errorf("fetch: cache dir is required")
	}
	if o.Conns <= 0 {
		o.Conns = DefaultConns
	}
	if o.PieceSize <= 0 {
		o.PieceSize = DefaultPieceSize
	}
	if o.Client == nil {
		o.Client = newClient(o.Conns)
	}
	for _, dir := range []string{
		o.CacheDir,
		filepath.Join(o.CacheDir, "sha256"),
		filepath.Join(o.CacheDir, "md5"),
		filepath.Join(o.CacheDir, "adhoc"),
		filepath.Join(o.CacheDir, ".partial"),
		filepath.Join(o.CacheDir, ".state"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Summary{}, err
		}
	}

	type job struct {
		item lock.Item
		file fileRef
	}
	var jobs []job
	only := toSet(o.Only)
	for _, it := range l.Items {
		if len(only) > 0 && !only[it.ID] {
			continue
		}
		for _, f := range it.Files {
			if f.SizeBytes <= 0 {
				continue
			}
			jobs = append(jobs, job{item: it, file: fileRef{ItemID: it.ID, File: f}})
		}
	}
	if len(jobs) == 0 {
		return Summary{}, fmt.Errorf("fetch: nothing to do (no matching files)")
	}

	// Space preflight: refuse to start a run that cannot finish.
	var need int64
	for _, j := range jobs {
		if _, ok := cachedPath(o.CacheDir, j.file); ok {
			continue
		}
		need += j.file.SizeBytes
	}
	if !o.DryRun && need > 0 {
		if free, err := freeSpace(o.CacheDir); err == nil {
			required := int64(float64(need)*SpaceFactor) + spaceSlack
			if free < required {
				return Summary{}, fmt.Errorf(
					"fetch: need %s free for %s of downloads (with %.0f%% headroom), only %s available",
					human(required), human(need), (SpaceFactor-1)*100, human(free))
			}
		}
	}

	sum := Summary{}
	start := time.Now()
	var rowBuf [][]string

	for i := range jobs {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		if o.MaxBytes > 0 && sum.FetchedBytes >= o.MaxBytes {
			break
		}
		res, err := fetchOne(ctx, o, jobs[i].file)
		if err != nil {
			if !o.DryRun {
				writeLog(o.LogDir, rowBuf, sum)
			}
			return sum, err
		}
		sum.Files = append(sum.Files, res)
		sum.TotalBytes += res.Size
		if !res.Reused {
			sum.FetchedBytes += res.Size
		}
		rowBuf = append(rowBuf, []string{
			res.ID,
			res.Name,
			strconv.FormatInt(res.Size, 10),
			boolString(res.Reused),
			strconv.FormatFloat(res.Elapsed.Seconds(), 'f', 3, 64),
			speed(res.Size, res.Elapsed),
			fmt.Sprint(res.Mirrors),
			res.SHA256,
		})
		if o.Progress != nil {
			o.Progress(summaryLine(res, len(sum.Files), len(jobs)))
		}
	}
	sum.Elapsed = time.Since(start)
	if !o.DryRun {
		writeLog(o.LogDir, rowBuf, sum)
	}
	return sum, nil
}

// fileRef pairs a lock item id with one of its files.
type fileRef struct {
	ItemID string
	resolve.File
}

// fetchOne downloads a single file into the cache.
func fetchOne(ctx context.Context, o Options, ref fileRef) (FileResult, error) {
	res := FileResult{ID: ref.ItemID, Dest: ref.Dest, Name: ref.Name, Size: ref.SizeBytes}
	start := time.Now()

	dst, existed := cachedPath(o.CacheDir, ref)
	if existed {
		res.Reused = true
		res.SHA256 = ref.SHA256
		res.Elapsed = time.Since(start)
		if o.DryRun {
			return res, nil
		}
		return res, nil
	}
	if o.DryRun {
		res.Elapsed = time.Since(start)
		return res, nil
	}

	mirrors, pieceLen, pieceHashes, err := resolveMirrors(ctx, o, ref)
	if err != nil {
		return res, err
	}
	res.Mirrors = len(mirrors)

	key := cacheKey(ref)
	part := filepath.Join(o.CacheDir, ".partial", key+".part")
	statePath := filepath.Join(o.CacheDir, ".state", key+".json")
	for _, dir := range []string{filepath.Dir(part), filepath.Dir(statePath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return res, err
		}
	}

	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			// Second pass: the digest disagreed, so start clean.
			_ = os.Remove(part)
			_ = os.Remove(statePath)
		}
		st := loadState(statePath, ref.SizeBytes, pieceLen)
		st.SHA256 = ref.SHA256

		// Persist progress at least every couple of seconds, so a hard kill costs
		// seconds of download rather than a whole window of pieces.
		var lastFlush atomic.Int64
		lastFlush.Store(time.Now().UnixNano())
		onPiece := func(idx int) {
			st.Mark(idx)
			if time.Since(time.Unix(0, lastFlush.Load())) > 2*time.Second {
				lastFlush.Store(time.Now().UnixNano())
				_ = st.Flush()
			}
		}

		d := &Downloader{
			URLs:      mirrors,
			Size:      ref.SizeBytes,
			PieceLen:  pieceLen,
			PieceSHA1: pieceHashes,
			PartPath:  part,
			Conns:     o.Conns,
			Client:    o.Client,
			Have:      st.Have,
			OnPiece:   onPiece,
		}
		if err := d.Run(ctx); err != nil {
			_ = st.Flush()
			return res, fmt.Errorf("%s: %w", ref.Name, err)
		}
		if err := st.Flush(); err != nil {
			return res, err
		}

		got, err := Verify(part, ref.SHA256)
		if err != nil {
			var de *DigestError
			if errors.As(err, &de) && attempt == 0 {
				continue
			}
			_ = os.Remove(part)
			st.Remove()
			return res, fmt.Errorf("%s: %w", ref.Name, err)
		}
		res.SHA256 = got
		if ref.SHA256 == "" {
			res.Upgraded = true
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return res, err
		}
		if err := os.Rename(part, dst); err != nil {
			return res, fmt.Errorf("place %s: %w", ref.Name, err)
		}
		st.Remove()
		res.Elapsed = time.Since(start)
		return res, nil
	}
	return res, fmt.Errorf("%s: failed after two attempts", ref.Name)
}

// resolveMirrors re-reads the descriptor (when there is one) so the fetcher uses
// current mirrors, and validates that upstream still serves the pinned content.
func resolveMirrors(ctx context.Context, o Options, ref fileRef) (urls []string, pieceLen int64, hashes []string, err error) {
	pieceLen = o.PieceSize
	if ref.Pieces != nil && ref.Pieces.Size > 0 {
		pieceLen = ref.Pieces.Size
		hashes = ref.Pieces.SHA1
	}
	if pieceLen > ref.SizeBytes {
		pieceLen = ref.SizeBytes
	}

	if ref.Descriptor == "" {
		if ref.URL == "" {
			return nil, 0, nil, fmt.Errorf("%s: no url and no descriptor", ref.Name)
		}
		return []string{ref.URL}, pieceLen, hashes, nil
	}

	mf, err := metalink.Fetch(ctx, o.Client, ref.Descriptor)
	if err != nil {
		return nil, 0, nil, err
	}
	if mf.Size != ref.SizeBytes {
		return nil, 0, nil, fmt.Errorf(
			"%s: upstream size changed (%d -> %d); re-run resolve", ref.Name, ref.SizeBytes, mf.Size)
	}
	if ref.SHA256 != "" && mf.SHA256 != "" && !strings.EqualFold(ref.SHA256, mf.SHA256) {
		return nil, 0, nil, fmt.Errorf(
			"%s: upstream sha256 changed; re-run resolve", ref.Name)
	}
	if mf.Pieces != nil && mf.Pieces.Length > 0 && len(mf.Pieces.Hashes) > 0 && hashes == nil {
		pieceLen = mf.Pieces.Length
		hashes = mf.Pieces.Hashes
	}
	return mf.URLs, pieceLen, hashes, nil
}

// CachePath returns the content-addressed location for a lock file and whether
// the exact expected size is already present. Exported so the image builder uses
// the same addressing scheme as the fetcher.
func CachePath(cacheDir string, f resolve.File) (string, bool) {
	return cachedPath(cacheDir, fileRef{File: f})
}

// cachedPath returns the cache location for a file and whether it is present.
// The key is content-addressed when a digest is known, so several items that
// share bytes share one copy.
func cachedPath(cacheDir string, ref fileRef) (string, bool) {
	key := cacheKey(ref)
	dst := filepath.Join(cacheDir, key)
	info, err := os.Stat(dst)
	if err != nil || info.Size() != ref.SizeBytes {
		return dst, false
	}
	return dst, true
}

func cacheKey(ref fileRef) string {
	switch {
	case ref.SHA256 != "":
		return filepath.Join("sha256", ref.SHA256)
	case ref.MD5 != "":
		return filepath.Join("md5", ref.MD5)
	default:
		safe := strings.Map(func(r rune) rune {
			if r == '/' || r == '\\' || r == ':' {
				return '_'
			}
			return r
		}, ref.Name)
		return filepath.Join("adhoc", safe)
	}
}

func toSet(ss []string) map[string]bool {
	if len(ss) == 0 {
		return nil
	}
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				m[part] = true
			}
		}
	}
	return m
}

func boolString(b bool) string {
	if b {
		return "reused"
	}
	return "fetched"
}

func speed(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "0"
	}
	return strconv.FormatFloat(float64(bytes)/(1<<20)/d.Seconds(), 'f', 2, 64)
}

func summaryLine(r FileResult, n, total int) string {
	if r.Reused {
		return fmt.Sprintf("  [%d/%d] %-26s cached  %s", n, total, r.ID, human(r.Size))
	}
	if r.Elapsed <= 0 {
		return fmt.Sprintf("  [%d/%d] %-26s %s (dry run)", n, total, r.ID, human(r.Size))
	}
	return fmt.Sprintf("  [%d/%d] %-26s fetched %s in %s (%s/s)",
		n, total, r.ID, human(r.Size), r.Elapsed.Round(time.Second),
		human(int64(float64(r.Size)/r.Elapsed.Seconds())))
}

// writeLog appends a throughput CSV so mirror quality is measured, not guessed.
func writeLog(dir string, rows [][]string, sum Summary) {
	if dir == "" || len(rows) == 0 {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "throughput.csv")
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if statErr != nil {
		_ = w.Write([]string{"item", "file", "bytes", "status", "seconds", "MBps", "mirrors", "sha256"})
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
	for _, r := range rows {
		_ = w.Write(r)
	}
	w.Flush()
}

// human renders GiB/MiB the way the docs do.
func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
