// Package media copies a built image onto removable media and verifies it.
//
// The image is the source of truth: it carries its own _MANIFEST.sha256, so a
// drive can be checked years later on a machine that has never seen this tool's
// cache or lock files.
package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one line of _MANIFEST.sha256, optionally with the size recorded in
// the _MANIFEST.sizes sidecar.
type Entry struct {
	SHA256 string
	Path   string // forward slashes, relative to the image root
	Size   int64  // 0 when the drive predates _MANIFEST.sizes
}

// ReadManifest parses _MANIFEST.sha256 from an image or drive root.
func ReadManifest(root string) ([]Entry, error) {
	p := filepath.Join(root, "_MANIFEST.sha256")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var out []Entry
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "<sha256>  <path>" - split on the first double space so paths may
		// contain single spaces.
		sum, path, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 {
			return nil, fmt.Errorf("%s:%d: malformed line", p, i+1)
		}
		out = append(out, Entry{SHA256: sum, Path: filepath.ToSlash(path)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no entries", p)
	}
	sizes, err := readSizes(root)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if n, ok := sizes[out[i].Path]; ok {
			out[i].Size = n
		}
	}
	return out, nil
}

// readSizes parses the optional _MANIFEST.sizes sidecar. A missing file is not
// an error: an older drive still verifies, but -quick can only check presence.
func readSizes(root string) (map[string]int64, error) {
	b, err := os.ReadFile(filepath.Join(root, "_MANIFEST.sizes"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make(map[string]int64, 64)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// "<size>\t<path>": tab-separated so paths may contain spaces.
		sizeStr, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			continue
		}
		out[filepath.ToSlash(path)] = n
	}
	return out, nil
}

// CopyOptions configures a copy to media.
type CopyOptions struct {
	// Progress is called at most every couple of seconds.
	Progress func(done, total int64, elapsed time.Duration)
	// BufferSize is the copy buffer; 4 MiB keeps flash writes sequential.
	BufferSize int
	// FreeBytes reports free space on a path (injected for tests).
	FreeBytes func(string) (int64, error)
	// RequireEmpty refuses a target that already has files, unless false.
	RequireEmpty bool
	// Resume skips files already present on the target at the right size and
	// repairs any that fail verification afterwards. A 49 GiB burn onto 8 MB/s
	// flash takes over an hour; losing it to a stray Ctrl-C is unacceptable.
	Resume bool
	// DryRun plans the burn (what would be copied, how much space is needed) and
	// writes nothing.
	DryRun bool
	// VerifyProgress reports post-copy verification, which reads the whole drive
	// back and is otherwise silent for a very long time.
	VerifyProgress func(done, total int, bytes int64)
	// Now supplies the clock (tests).
	Now func() time.Time
}

// CopyReport summarises a burn.
type CopyReport struct {
	Files   int
	Bytes   int64
	Elapsed time.Duration
	// Skipped counts files already present and correctly sized.
	Skipped      int
	SkippedBytes int64
	// Repaired counts files re-copied after failing verification.
	Repaired int
	// Verified reports whether the post-copy verification ran and passed.
	Verified bool
}

// CopyResult is one copied file.
type CopyResult struct {
	Path  string
	Bytes int64
}

// Copy copies every file in imageDir to targetRoot, then verifies it.
func Copy(ctx context.Context, imageDir, targetRoot string, o CopyOptions) (CopyReport, error) {
	if o.BufferSize <= 0 {
		o.BufferSize = 4 << 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.FreeBytes == nil {
		o.FreeBytes = func(string) (int64, error) { return 0, fmt.Errorf("no free-space probe configured") }
	}

	entries, err := ReadManifest(imageDir)
	if err != nil {
		return CopyReport{}, err
	}
	// Everything else that lives in the image root and is not in the manifest.
	total, files, err := measure(imageDir)
	if err != nil {
		return CopyReport{}, err
	}
	if files == 0 {
		return CopyReport{}, fmt.Errorf("image %s is empty", imageDir)
	}

	if info, err := os.Stat(targetRoot); err != nil || !info.IsDir() {
		return CopyReport{}, fmt.Errorf("target %s is not a directory", targetRoot)
	}
	if o.RequireEmpty {
		empty, err := isEmpty(targetRoot)
		if err != nil {
			return CopyReport{}, err
		}
		if !empty {
			return CopyReport{}, fmt.Errorf("target %s is not empty; format it first or pass -force", targetRoot)
		}
	}

	// Plan what actually has to be written, so the space check accounts for a
	// resumed burn instead of demanding room for the whole image again.
	type job struct {
		src, dst string
		size     int64
	}
	var jobs []job
	var skippedBytes int64
	var skipped int
	err = filepath.WalkDir(imageDir, func(src string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(imageDir, src)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		dst := filepath.Join(targetRoot, rel)
		if o.Resume {
			if dstInfo, statErr := os.Stat(dst); statErr == nil && dstInfo.Size() == info.Size() {
				skipped++
				skippedBytes += info.Size()
				return nil
			}
		}
		jobs = append(jobs, job{src: src, dst: dst, size: info.Size()})
		return nil
	})
	if err != nil {
		return CopyReport{}, err
	}

	var need int64
	for _, j := range jobs {
		need += j.size
	}
	if free, err := o.FreeBytes(targetRoot); err == nil {
		required := need + need/50 // 2% headroom
		if free < required {
			return CopyReport{}, fmt.Errorf("target has %s free, need %s", human(free), human(required))
		}
	}
	if o.DryRun {
		return CopyReport{
			Files:   len(jobs),
			Bytes:   need,
			Skipped: skipped,
		}, nil
	}

	start := o.Now()
	copied := skippedBytes
	written := int64(0)
	var fileCount int
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return CopyReport{Files: fileCount, Bytes: written, Skipped: skipped, SkippedBytes: skippedBytes, Elapsed: o.Now().Sub(start)}, err
		}
		n, err := copyFile(j.src, j.dst, o.BufferSize)
		if err != nil {
			return CopyReport{Files: fileCount, Bytes: written, Skipped: skipped, SkippedBytes: skippedBytes, Elapsed: o.Now().Sub(start)}, err
		}
		copied += n
		written += n
		fileCount++
		if o.Progress != nil {
			o.Progress(copied, total, o.Now().Sub(start))
		}
	}

	rep := CopyReport{
		Files:        fileCount,
		Bytes:        written,
		Skipped:      skipped,
		SkippedBytes: skippedBytes,
		Elapsed:      o.Now().Sub(start),
	}

	// Verify what is on the drive now, and repair anything that fails. A size
	// match is a good resume signal but not proof, so verification still decides.
	bad, err := Verify(ctx, targetRoot, VerifyOptions{Full: true, Entries: entries, Progress: o.VerifyProgress})
	if err != nil {
		return rep, fmt.Errorf("post-copy verification: %w", err)
	}
	if len(bad) > 0 {
		var repaired []Entry
		for _, rel := range bad {
			src := filepath.Join(imageDir, filepath.FromSlash(rel))
			dst := filepath.Join(targetRoot, filepath.FromSlash(rel))
			if _, err := copyFile(src, dst, o.BufferSize); err != nil {
				return rep, fmt.Errorf("repairing %s: %w", rel, err)
			}
			rep.Repaired++
			for _, e := range entries {
				if e.Path == rel {
					repaired = append(repaired, e)
					break
				}
			}
		}
		// Only the repaired files need re-checking; re-hashing the whole drive
		// would double the cost of an already half-hour verification pass.
		bad, err = Verify(ctx, targetRoot, VerifyOptions{Full: true, Entries: repaired})
		if err != nil {
			return rep, err
		}
	}
	if len(bad) > 0 {
		return rep, fmt.Errorf("verification failed for %d file(s): %s",
			len(bad), strings.Join(bad, ", "))
	}
	rep.Verified = true
	return rep, nil
}

// VerifyOptions configures verification.
type VerifyOptions struct {
	// Full hashes every file. When false only sizes are compared (fast, catches
	// truncation but not corruption).
	Full bool
	// Entries overrides reading the manifest from the root (used by tests and by
	// Copy, which already has them).
	Entries []Entry
	// Progress is called per file. Verification of a 49 GiB drive reads every
	// byte back off flash and takes half an hour; silence looks like a hang.
	// bytes is the total hashed so far.
	Progress func(done, total int, bytes int64)
}

// Verify checks a drive against its own manifest and returns failing paths.
func Verify(ctx context.Context, root string, o VerifyOptions) ([]string, error) {
	entries := o.Entries
	if entries == nil {
		var err error
		entries, err = ReadManifest(root)
		if err != nil {
			return nil, err
		}
	}
	if o.Progress != nil {
		o.Progress(0, len(entries), 0)
	}
	var bad []string
	var bytes int64
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return bad, err
		}
		p := filepath.Join(root, filepath.FromSlash(e.Path))
		if o.Full {
			got, size, err := hashFileSize(p)
			bytes += size
			if err != nil {
				bad = append(bad, e.Path)
			} else if !strings.EqualFold(got, e.SHA256) {
				bad = append(bad, e.Path)
			}
		} else {
			if info, err := os.Stat(p); err != nil {
				bad = append(bad, e.Path)
			} else {
				if e.Size > 0 && info.Size() != e.Size {
					bad = append(bad, e.Path)
				}
				bytes += info.Size()
			}
		}
		if o.Progress != nil {
			o.Progress(i+1, len(entries), bytes)
		}
	}
	sort.Strings(bad)
	return bad, nil
}

func copyFile(src, dst string, bufSize int) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, bufSize)
	n, err := io.CopyBuffer(out, in, buf)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("copy %s: %w", dst, err)
	}
	return n, nil
}

func hashFile(p string) (string, error) {
	sum, _, err := hashFileSize(p)
	return sum, err
}

// hashFileSize hashes a file and reports how many bytes it read.
func hashFileSize(p string) (string, int64, error) {
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

// measure totals the bytes and files under dir.
func measure(dir string) (int64, int, error) {
	var total int64
	var files int
	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		files++
		return nil
	})
	return total, files, err
}

func isEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	for {
		names, err := f.Readdirnames(64)
		for _, n := range names {
			// Windows creates these on every volume, including a freshly
			// formatted one; they do not make a drive "in use".
			switch strings.ToLower(n) {
			case "system volume information", "$recycle.bin", "recycler", "lost+found":
				continue
			}
			return false, nil
		}
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
