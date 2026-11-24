package fetch

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"survivaldrive/internal/lock"
	"survivaldrive/internal/resolve"
)

// descriptorServer serves a payload plus a metalink that describes it.
type descriptorServer struct {
	data     []byte
	size     int64 // advertised size, defaults to len(data)
	sha256   string
	urls     []string // defaults to the payload URL
	requests int
}

func newDescriptorServer(t *testing.T, data []byte) (*descriptorServer, *httptest.Server) {
	t.Helper()
	ds := &descriptorServer{data: data}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/file.zim", func(w http.ResponseWriter, r *http.Request) {
		ds.requests++
		start, end := int64(0), int64(len(ds.data))-1
		if rng := r.Header.Get("Range"); rng != "" {
			a, b, err := parseRange(rng, int64(len(ds.data)))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			start, end = a, b
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(ds.data)))
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(ds.data[start : end+1])
	})
	mux.HandleFunc("/file.zim.meta4", func(w http.ResponseWriter, r *http.Request) {
		ds.requests++
		size := ds.size
		if size == 0 {
			size = int64(len(ds.data))
		}
		sha := ds.sha256
		if sha == "" {
			sha = sha256Hex(ds.data)
		}
		urls := ds.urls
		if len(urls) == 0 {
			urls = []string{srv.URL + "/file.zim"}
		}
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<metalink xmlns="urn:ietf:params:xml:ns:metalink">
  <file name="file.zim">
    <size>%d</size>
    <hash type="sha-256">%s</hash>
    %s
  </file>
</metalink>`, size, sha, metalinkURLs(urls))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ds, srv
}

func metalinkURLs(urls []string) string {
	var b strings.Builder
	for _, u := range urls {
		fmt.Fprintf(&b, "<url>%s</url>", u)
	}
	return b.String()
}

func testLock(files ...resolve.File) lock.Lock {
	return lock.Lock{
		Version: lock.Version,
		Profile: "test", SKU: "test", Variant: "sell",
		CapacityBytes: 1 << 30,
		Items: []lock.Item{{
			ID: "item", Category: "test", Kind: "zim",
			CommercialOK: true, License: "CC-BY-SA-4.0",
			Files: files,
		}},
	}
}

func testOptions(cacheDir string) Options {
	return Options{
		CacheDir:  cacheDir,
		Conns:     4,
		PieceSize: 64 << 10,
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
}
func TestRunFetchesThenReusesCache(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	data := payload(256 << 10)
	ds, srv := newDescriptorServer(t, data)

	l := testLock(resolve.File{
		Name: "file.zim", Dest: "archives/file.zim",
		URL: srv.URL + "/file.zim", Descriptor: srv.URL + "/file.zim.meta4",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	cache := t.TempDir()

	sum, err := Run(t.Context(), l, testOptions(cache))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(sum.Files) != 1 || sum.Files[0].Reused {
		t.Fatalf("summary = %+v, want one fetched file", sum.Files)
	}
	cached := filepath.Join(cache, "sha256", sha256Hex(data))
	got, err := os.ReadFile(cached)
	if err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
	if string(got) != string(data) {
		t.Fatal("cached bytes differ from payload")
	}

	before := ds.requests
	sum2, err := Run(t.Context(), l, testOptions(cache))
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !sum2.Files[0].Reused {
		t.Error("second run should report the file as cached")
	}
	if ds.requests != before {
		t.Errorf("second run made %d requests, want 0", ds.requests-before)
	}
}

func TestRunDryRunTouchesNothing(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	data := payload(64 << 10)
	ds, srv := newDescriptorServer(t, data)
	l := testLock(resolve.File{
		Name: "file.zim", URL: srv.URL + "/file.zim", Descriptor: srv.URL + "/file.zim.meta4",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	cache := t.TempDir()
	o := testOptions(cache)
	o.DryRun = true

	sum, err := Run(t.Context(), l, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ds.requests != 0 {
		t.Errorf("dry run made %d requests", ds.requests)
	}
	if sum.FetchedBytes != int64(len(data)) {
		t.Errorf("fetched bytes = %d, want the full size reported", sum.FetchedBytes)
	}
	if entries, _ := os.ReadDir(filepath.Join(cache, "sha256")); len(entries) != 0 {
		t.Error("dry run must not write cache files")
	}
}

func TestRunRefusesWhenDiskIsTooSmall(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 20, nil } // 1 MiB free
	data := payload(64 << 10)
	_, srv := newDescriptorServer(t, data)
	l := testLock(resolve.File{
		Name: "file.zim", URL: srv.URL + "/file.zim", Descriptor: srv.URL + "/file.zim.meta4",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	_, err := Run(t.Context(), l, testOptions(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "need") {
		t.Fatalf("error = %v, want a free-space refusal", err)
	}
}

func TestRunDetectsUpstreamSizeChange(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	data := payload(64 << 10)
	ds, srv := newDescriptorServer(t, data)
	ds.size = int64(len(data)) + 1 // upstream grew since resolve

	l := testLock(resolve.File{
		Name: "file.zim", URL: srv.URL + "/file.zim", Descriptor: srv.URL + "/file.zim.meta4",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	_, err := Run(t.Context(), l, testOptions(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "re-run resolve") {
		t.Fatalf("error = %v, want a size-change refusal naming resolve", err)
	}
}

func TestRunDetectsUpstreamDigestChange(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	data := payload(64 << 10)
	ds, srv := newDescriptorServer(t, data)
	ds.sha256 = strings.Repeat("0", 64)

	l := testLock(resolve.File{
		Name: "file.zim", URL: srv.URL + "/file.zim", Descriptor: srv.URL + "/file.zim.meta4",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	_, err := Run(t.Context(), l, testOptions(t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "re-run resolve") {
		t.Fatalf("error = %v, want a digest-change refusal naming resolve", err)
	}
}

func TestRunRejectsCorruptPayload(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	data := payload(64 << 10)
	corrupt := append([]byte(nil), data...)
	corrupt[10] ^= 0xFF
	ds, srv := newDescriptorServer(t, corrupt)
	// The descriptor still advertises the correct digest: the bytes on the wire
	// are wrong, which is exactly what the final hash must catch.
	ds.sha256 = sha256Hex(data)

	l := testLock(resolve.File{
		Name: "file.zim", URL: srv.URL + "/file.zim", Descriptor: srv.URL + "/file.zim.meta4",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	cache := t.TempDir()
	_, err := Run(t.Context(), l, testOptions(cache))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("error = %v, want a sha256 mismatch", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(cache, "sha256")); len(entries) != 0 {
		t.Error("a corrupt download must not be placed in the cache")
	}
	if leftover := countFiles(t, filepath.Join(cache, ".partial")); leftover != 0 {
		t.Errorf("%d partial file(s) left behind", leftover)
	}
}

// countFiles counts regular files below dir.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func TestRunRecordsDigestForUnpinnedFile(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	data := payload(64 << 10)
	_, srv := newDescriptorServer(t, data)

	// No sha256 in the lock: the fetcher must hash the result itself.
	l := testLock(resolve.File{
		Name: "tools.zip", URL: srv.URL + "/file.zim", SizeBytes: int64(len(data)),
	})
	cache := t.TempDir()
	sum, err := Run(t.Context(), l, testOptions(cache))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	f := sum.Files[0]
	if !f.Upgraded {
		t.Error("want Upgraded=true for a file with no pinned digest")
	}
	if f.SHA256 != sha256Hex(data) {
		t.Errorf("recorded digest = %q, want the computed sha256", f.SHA256)
	}
	if _, err := os.Stat(filepath.Join(cache, "adhoc", "tools.zip")); err != nil {
		t.Errorf("unpinned file should be cached by name: %v", err)
	}
}

func TestRunWritesThroughputLog(t *testing.T) {
	freeSpace = func(string) (int64, error) { return 1 << 40, nil }
	data := payload(32 << 10)
	_, srv := newDescriptorServer(t, data)
	l := testLock(resolve.File{
		Name: "file.zim", URL: srv.URL + "/file.zim", Descriptor: srv.URL + "/file.zim.meta4",
		SizeBytes: int64(len(data)), SHA256: sha256Hex(data),
	})
	cache := t.TempDir()
	logs := t.TempDir()
	o := testOptions(cache)
	o.LogDir = logs
	if _, err := Run(t.Context(), l, o); err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(logs, "throughput.csv"))
	if err != nil {
		t.Fatalf("log missing: %v", err)
	}
	if !strings.Contains(string(b), "item,file,bytes,status") {
		t.Errorf("csv header missing: %q", string(b))
	}
}

func TestCacheKeyIsContentAddressed(t *testing.T) {
	sha := strings.Repeat("a", 64)
	if got := cacheKey(fileRef{File: resolve.File{SHA256: sha, Name: "x.zim"}}); got != filepath.Join("sha256", sha) {
		t.Errorf("key = %q", got)
	}
	if got := cacheKey(fileRef{File: resolve.File{MD5: strings.Repeat("b", 32), Name: "x.zim"}}); got != filepath.Join("md5", strings.Repeat("b", 32)) {
		t.Errorf("key = %q", got)
	}
	if got := cacheKey(fileRef{File: resolve.File{Name: "a/b:c.zip"}}); got != filepath.Join("adhoc", "a_b_c.zip") {
		t.Errorf("key = %q, want slashes and colons flattened", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	st := loadState(path, 1000, 100)
	if st.Have(3) {
		t.Fatal("fresh state must be empty")
	}
	for i := 0; i < flushEvery+5; i++ {
		st.Mark(i)
	}
	if err := st.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	reloaded := loadState(path, 1000, 100)
	if n := reloaded.Count(); n != flushEvery+5 {
		t.Errorf("reloaded %d pieces, want %d", n, flushEvery+5)
	}
	if !reloaded.Have(flushEvery + 4) {
		t.Error("last marked piece missing after reload")
	}

	// A different size means the state is stale and must be ignored.
	stale := loadState(path, 2000, 100)
	if stale.Count() != 0 {
		t.Error("state from a different file size must be discarded")
	}
}
