package fetch

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// rangedServer serves a synthetic payload with Range support and enough knobs to
// exercise failover, corruption and throttling.
type rangedServer struct {
	data []byte

	mu       sync.Mutex
	requests int
	served   int64
	// alwaysFail makes every request return 429.
	alwaysFail bool
	// corruptAt holds byte offsets whose first byte gets flipped.
	corruptAt map[int64]bool
	// slowAt delays a specific offset.
	slowAt map[int64]time.Duration
	// ignoreRange makes the server answer 200 with the whole body.
	ignoreRange bool
	// block makes the handler hang until the client gives up.
	block bool
}

func newRangedServer(t *testing.T, data []byte) (*rangedServer, *httptest.Server) {
	t.Helper()
	s := &rangedServer{data: data, corruptAt: map[int64]bool{}, slowAt: map[int64]time.Duration{}}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *rangedServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	alwaysFail := s.alwaysFail
	ignoreRange := s.ignoreRange
	block := s.block
	s.mu.Unlock()
	if block {
		<-r.Context().Done()
		return
	}
	if alwaysFail {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	start, end := int64(0), int64(len(s.data))-1
	if rng := r.Header.Get("Range"); rng != "" && !ignoreRange {
		var err error
		start, end, err = parseRange(rng, int64(len(s.data)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	s.mu.Lock()
	if d, ok := s.slowAt[start]; ok {
		s.mu.Unlock()
		time.Sleep(d)
		s.mu.Lock()
	}
	corrupt := s.corruptAt[start]
	s.mu.Unlock()

	chunk := make([]byte, end-start+1)
	copy(chunk, s.data[start:end+1])
	if corrupt {
		chunk[0] ^= 0xFF
	}

	if ignoreRange {
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		w.WriteHeader(http.StatusOK)
		n, _ := w.Write(s.data)
		s.mu.Lock()
		s.served += int64(n)
		s.mu.Unlock()
		return
	}

	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(http.StatusPartialContent)
	n, _ := w.Write(chunk)
	s.mu.Lock()
	s.served += int64(n)
	s.mu.Unlock()
}

func (s *rangedServer) stats() (requests int, served int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests, s.served
}

func parseRange(v string, size int64) (int64, int64, error) {
	rest, ok := strings.CutPrefix(v, "bytes=")
	if !ok {
		return 0, 0, fmt.Errorf("bad range %q", v)
	}
	a, b, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, 0, fmt.Errorf("bad range %q", v)
	}
	start, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	end := size - 1
	if b != "" {
		end, err = strconv.ParseInt(b, 10, 64)
		if err != nil {
			return 0, 0, err
		}
	}
	if start < 0 || start >= size || end >= size || end < start {
		return 0, 0, fmt.Errorf("range out of bounds %q", v)
	}
	return start, end, nil
}

// payload returns deterministic test bytes.
func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func pieceHashes(b []byte, pieceLen int64) []string {
	var out []string
	for off := int64(0); off < int64(len(b)); off += pieceLen {
		end := off + pieceLen
		if end > int64(len(b)) {
			end = int64(len(b))
		}
		sum := sha1.Sum(b[off:end])
		out = append(out, hex.EncodeToString(sum[:]))
	}
	return out
}

func TestDownloaderWritesWholeFile(t *testing.T) {
	data := payload(512 << 10)
	srvState, srv := newRangedServer(t, data)

	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	d := &Downloader{
		URLs:      []string{srv.URL + "/file"},
		Size:      int64(len(data)),
		PieceLen:  64 << 10,
		PieceSHA1: pieceHashes(data, 64<<10),
		PartPath:  part,
		Conns:     6,
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(part)
	if err != nil {
		t.Fatalf("read part: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded bytes differ from payload")
	}
	if reqs, served := srvState.stats(); served != int64(len(data)) || reqs == 0 {
		t.Errorf("served %d bytes over %d requests, want %d", served, reqs, len(data))
	}
}

func TestDownloaderResumesWithoutRefetching(t *testing.T) {
	data := payload(256 << 10)
	srvState, srv := newRangedServer(t, data)
	pieceLen := int64(64 << 10)
	done := 2 // first two pieces already on disk

	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	if err := os.WriteFile(part, data[:int64(done)*pieceLen], 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Downloader{
		URLs:      []string{srv.URL + "/file"},
		Size:      int64(len(data)),
		PieceLen:  pieceLen,
		PieceSHA1: pieceHashes(data, pieceLen),
		PartPath:  part,
		Conns:     4,
		Client:    &http.Client{Timeout: 10 * time.Second},
		Have:      func(i int) bool { return i < done },
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := os.ReadFile(part)
	if !bytes.Equal(got, data) {
		t.Fatal("resumed file differs from payload")
	}
	// Only the missing pieces may have been fetched.
	if _, served := srvState.stats(); served != int64(len(data))-int64(done)*pieceLen {
		t.Errorf("served %d bytes, want %d (only the missing pieces)", served, int64(len(data))-int64(done)*pieceLen)
	}
}

func TestDownloaderFailsOverOnThrottling(t *testing.T) {
	data := payload(128 << 10)
	bad, badSrv := newRangedServer(t, data)
	bad.alwaysFail = true
	_, goodSrv := newRangedServer(t, data)

	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	d := &Downloader{
		URLs:      []string{badSrv.URL + "/file", goodSrv.URL + "/file"},
		Size:      int64(len(data)),
		PieceLen:  64 << 10,
		PieceSHA1: pieceHashes(data, 64<<10),
		PartPath:  part,
		Conns:     2,
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := os.ReadFile(part)
	if !bytes.Equal(got, data) {
		t.Fatal("content mismatch after failover")
	}
	if reqs, _ := bad.stats(); reqs == 0 {
		t.Error("expected the throttled mirror to have been tried first")
	}
}

func TestDownloaderRepairsCorruptPieceFromAnotherMirror(t *testing.T) {
	data := payload(256 << 10)
	pieceLen := int64(64 << 10)
	// Piece 2 starts at 128 KiB.
	corrupt, corruptSrv := newRangedServer(t, data)
	corrupt.corruptAt[2*pieceLen] = true
	_, goodSrv := newRangedServer(t, data)

	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	d := &Downloader{
		URLs:      []string{corruptSrv.URL + "/file", goodSrv.URL + "/file"},
		Size:      int64(len(data)),
		PieceLen:  pieceLen,
		PieceSHA1: pieceHashes(data, pieceLen),
		PartPath:  part,
		Conns:     1,
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := os.ReadFile(part)
	if !bytes.Equal(got, data) {
		t.Fatal("corrupt piece was not repaired")
	}
}

func TestDownloaderRejectsIgnoredRange(t *testing.T) {
	data := payload(128 << 10)
	bad, badSrv := newRangedServer(t, data)
	bad.ignoreRange = true

	dir := t.TempDir()
	d := &Downloader{
		URLs:     []string{badSrv.URL + "/file"},
		Size:     int64(len(data)),
		PieceLen: 64 << 10,
		PartPath: filepath.Join(dir, "f.part"),
		Conns:    1,
		Client:   &http.Client{Timeout: 10 * time.Second},
	}
	err := d.Run(t.Context())
	if err == nil {
		t.Fatal("want an error when the server ignores Range")
	}
	if !strings.Contains(err.Error(), "ignored range") {
		t.Errorf("error = %q, want it to explain the ignored range", err)
	}
}

func TestDownloaderWritesInOrderWhenAPieceIsSlow(t *testing.T) {
	data := payload(256 << 10)
	pieceLen := int64(64 << 10)
	s, srv := newRangedServer(t, data)
	// Make the first piece slow so later pieces finish first and must be
	// buffered instead of written out of order.
	s.slowAt[0] = 150 * time.Millisecond

	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	d := &Downloader{
		URLs:      []string{srv.URL + "/file"},
		Size:      int64(len(data)),
		PieceLen:  pieceLen,
		PieceSHA1: pieceHashes(data, pieceLen),
		PartPath:  part,
		Conns:     4,
		Client:    &http.Client{Timeout: 10 * time.Second},
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatalf("run: %v", err)
	}
	got, _ := os.ReadFile(part)
	if !bytes.Equal(got, data) {
		t.Fatal("reorder buffer produced a corrupt file")
	}
}

func TestDownloaderResumesWithHoles(t *testing.T) {
	data := payload(256 << 10)
	pieceLen := int64(64 << 10)
	srvState, srv := newRangedServer(t, data)

	// A resumed file can have holes: pieces 0 and 2 present, 1 and 3 missing.
	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	full := make([]byte, len(data))
	copy(full[0:pieceLen], data[0:pieceLen])
	copy(full[2*pieceLen:3*pieceLen], data[2*pieceLen:3*pieceLen])
	if err := os.WriteFile(part, full, 0o644); err != nil {
		t.Fatal(err)
	}
	have := map[int]bool{0: true, 2: true}

	d := &Downloader{
		URLs:         []string{srv.URL + "/file"},
		Size:         int64(len(data)),
		PieceLen:     pieceLen,
		PieceSHA1:    pieceHashes(data, pieceLen),
		PartPath:     part,
		Conns:        4,
		Client:       &http.Client{Timeout: 5 * time.Second},
		StallTimeout: 2 * time.Second,
		Have:         func(i int) bool { return have[i] },
	}
	// Guard against a writer that stalls waiting for a piece it will never see.
	errc := make(chan error, 1)
	go func() { errc <- d.Run(t.Context()) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("download deadlocked on a resumed file with holes")
	}

	got, _ := os.ReadFile(part)
	if !bytes.Equal(got, data) {
		t.Fatal("file with holes was not completed correctly")
	}
	if _, served := srvState.stats(); served != 2*pieceLen {
		t.Errorf("served %d bytes, want %d (only the two missing pieces)", served, 2*pieceLen)
	}
}

func TestDownloaderAbortsOnStall(t *testing.T) {
	data := payload(128 << 10)
	s, srv := newRangedServer(t, data)
	s.block = true

	dir := t.TempDir()
	d := &Downloader{
		URLs:         []string{srv.URL + "/file"},
		Size:         int64(len(data)),
		PieceLen:     64 << 10,
		PartPath:     filepath.Join(dir, "f.part"),
		Conns:        2,
		Client:       &http.Client{Timeout: 30 * time.Second},
		StallTimeout: 300 * time.Millisecond,
	}
	start := time.Now()
	err := d.Run(t.Context())
	if err == nil {
		t.Fatal("want a stall error")
	}
	if !strings.Contains(err.Error(), "stalled") {
		t.Errorf("error = %q, want it to say stalled", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("stall took %s to detect, want a couple of seconds", elapsed)
	}
}

func TestDownloaderCompletesWhenOnePieceIsVerySlow(t *testing.T) {
	// Regression: a single slow piece used to deadlock the reorder window once
	// more than `window` later pieces had completed, because a worker could not
	// hand over the piece the writer was waiting for.
	const n = 48
	data := payload(n * 64 << 10)
	pieceLen := int64(64 << 10)
	s, srv := newRangedServer(t, data)
	s.slowAt[0] = 400 * time.Millisecond // the piece everyone waits for

	dir := t.TempDir()
	part := filepath.Join(dir, "f.part")
	d := &Downloader{
		URLs:         []string{srv.URL + "/file"},
		Size:         int64(len(data)),
		PieceLen:     pieceLen,
		PieceSHA1:    pieceHashes(data, pieceLen),
		PartPath:     part,
		Conns:        8, // window defaults to 16, far below the 48 pieces
		Client:       &http.Client{Timeout: 5 * time.Second},
		StallTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- d.Run(t.Context()) }()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("deadlocked waiting for a slow piece")
	}

	got, _ := os.ReadFile(part)
	if !bytes.Equal(got, data) {
		t.Fatal("output corrupt")
	}
}

func TestVerifyDetectsMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.bin")
	_ = os.WriteFile(p, []byte("hello"), 0o644)

	if _, err := Verify(p, sha256Hex([]byte("hello"))); err != nil {
		t.Errorf("matching digest must pass: %v", err)
	}
	_, err := Verify(p, strings.Repeat("0", 64))
	if err == nil {
		t.Fatal("want a mismatch error")
	}
	var de *DigestError
	if !errors.As(err, &de) {
		t.Errorf("error = %v, want DigestError", err)
	}
}

func TestValidContentRange(t *testing.T) {
	tests := []struct {
		v     string
		start int64
		end   int64
		want  bool
	}{
		{"bytes 0-99/1000", 0, 99, true},
		{"bytes 100-199/1000", 100, 199, true},
		{"bytes 0-99/*", 0, 99, false},
		{"bytes 1-99/1000", 0, 99, false},
		{"", 0, 99, false},
	}
	for _, tc := range tests {
		if got := validContentRange(tc.v, tc.start, tc.end); got != tc.want {
			t.Errorf("validContentRange(%q,%d,%d) = %v, want %v", tc.v, tc.start, tc.end, got, tc.want)
		}
	}
}

func TestPieceCountAndLengths(t *testing.T) {
	d := &Downloader{Size: 10, PieceLen: 4}
	if got := d.Count(); got != 3 {
		t.Errorf("count = %d, want 3", got)
	}
	if got := d.length(2); got != 2 {
		t.Errorf("last piece length = %d, want 2", got)
	}
}
