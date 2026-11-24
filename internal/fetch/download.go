package fetch

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Downloader pulls one file over N parallel ranged connections.
//
// Design constraints come from the staging hardware, not from the network:
// `F:` is a shingled (SMR) disk where many interleaved writers collapse
// throughput by 5-7x. So the network side is parallel and the disk side is a
// single sequential writer fed through a small reorder buffer.
type Downloader struct {
	URLs     []string
	Size     int64
	PieceLen int64
	// PieceSHA1 may be nil; when present each chunk is verified as it lands.
	PieceSHA1 []string

	PartPath string
	Conns    int
	// ConnsPerHost caps concurrent requests to a single host (mirrors throttle).
	ConnsPerHost int
	Window       int // reorder buffer, in pieces
	Client       *http.Client

	// StallTimeout aborts a run that has received no data for this long. Without
	// it a semi-alive mirror (connects, then trickles) hangs the whole build.
	StallTimeout time.Duration
	// RequestTimeout bounds a single ranged GET.
	RequestTimeout time.Duration

	// Have reports whether a piece is already on disk (resume).
	Have func(idx int) bool
	// OnPiece, if set, is called after a piece is durably written.
	OnPiece func(idx int)
	// Progress is called periodically with bytes received.
	Progress func(bytes int64)

	// ResumeFrom is the first byte offset that is missing, used for logging.
	ResumeFrom int64
}

// Count returns the number of pieces.
func (d *Downloader) Count() int {
	if d.PieceLen <= 0 {
		return 0
	}
	return int((d.Size + d.PieceLen - 1) / d.PieceLen)
}

func (d *Downloader) offset(i int) int64 { return int64(i) * d.PieceLen }

func (d *Downloader) length(i int) int64 {
	start := d.offset(i)
	end := start + d.PieceLen
	if end > d.Size {
		end = d.Size
	}
	return end - start
}

type piece struct {
	idx  int
	data []byte
}

// mirror is one source with a running estimate of how fast it delivers.
type mirror struct {
	url      string
	host     string
	inFlight int
	requests int
	failures int
	// rate is an EWMA in bytes/second, seeded from the preflight measurements so
	// a known-slow host is not given equal share of the work.
	rate float64
}

// mirrorPool hands out mirrors by expected throughput, weighted by how many
// requests are already in flight against them.
//
// Equal round-robin was measurably wrong: Kiwix advertises some hosts at
// 0.1 MB/s alongside 12 MB/s ones, and because the output is written in order,
// the slowest piece decides when the whole file finishes.
type mirrorPool struct {
	mu   sync.Mutex
	ms   []*mirror
	best float64
	// perHost caps concurrent requests to one host: mirrors throttle, and being
	// throttled by our own fan-out is slower than using fewer connections.
	perHost int
}

// priorRate seeds the scheduler from measured preflight throughput (MB/s).
func priorRate(raw string) float64 {
	u, err := url.Parse(raw)
	if err != nil {
		return 1.0
	}
	host := u.Hostname()
	switch {
	case strings.HasSuffix(host, "mirror.driftle.ss"):
		return 1.6
	case host == "mirrors.dotsrc.org":
		return 7.0
	case host == "dumps.wikimedia.org":
		return 4.5
	case host == "ftpmirror.your.org":
		return 2.7
	case host == "ftp.nluug.nl":
		return 0.8
	case host == "mirror.download.kiwix.org":
		return 0.1
	case host == "huggingface.co" || strings.HasSuffix(host, ".huggingface.co"):
		return 19.0
	case host == "github.com" || strings.HasSuffix(host, ".github.com") ||
		strings.HasSuffix(host, "githubusercontent.com"):
		return 8.0
	default:
		return 1.5
	}
}

func newMirrorPool(urls []string, perHost int) *mirrorPool {
	if perHost <= 0 {
		perHost = 4
	}
	ms := make([]*mirror, 0, len(urls))
	for _, u := range urls {
		host := ""
		if parsed, err := url.Parse(u); err == nil {
			host = parsed.Hostname()
		}
		ms = append(ms, &mirror{url: u, host: host, rate: priorRate(u)})
	}
	// Stable sort keeps the metalink's own priority order among equal estimates.
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].rate > ms[j].rate })
	p := &mirrorPool{ms: ms, perHost: perHost}
	if len(ms) > 0 {
		p.best = ms[0].rate
	}
	return p
}

// pick returns the mirror with the best score and marks a request in flight.
func (p *mirrorPool) pick() *mirror {
	p.mu.Lock()
	defer p.mu.Unlock()

	hostLoad := make(map[string]int, len(p.ms))
	for _, m := range p.ms {
		hostLoad[m.host] += m.inFlight
	}

	var chosen *mirror
	var chosenScore float64
	var fallback *mirror
	var fallbackScore float64
	for _, m := range p.ms {
		if m.failures >= 3 {
			continue // give up on a host that has failed repeatedly
		}
		// Throughput per unit of existing load. Priors come from preflight, so a
		// known-slow host stays unused while a fast one still has capacity.
		score := m.rate / (1 + float64(m.inFlight))
		if hostLoad[m.host] < p.perHost {
			if chosen == nil || score > chosenScore {
				chosen, chosenScore = m, score
			}
		} else if fallback == nil || score > fallbackScore {
			// Every host is saturated: keep the pipeline moving rather than idle.
			fallback, fallbackScore = m, score
		}
	}
	if chosen == nil {
		chosen = fallback
	}
	if chosen == nil {
		// Everything is failing: keep going with the least-failed host so the
		// error surfaced is a real network error rather than a scheduler one.
		for _, m := range p.ms {
			if chosen == nil || m.failures < chosen.failures {
				chosen = m
			}
		}
	}
	chosen.inFlight++
	chosen.requests++
	return chosen
}

// done records a successful transfer and updates the throughput estimate.
func (p *mirrorPool) done(m *mirror, bytes int64, elapsed time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m.inFlight--
	if elapsed <= 0 {
		return
	}
	sample := float64(bytes) / elapsed.Seconds()
	// Blend generously on the first sample so one unlucky piece cannot condemn a
	// good host, then track it closely.
	if m.requests <= 1 {
		m.rate = (m.rate + sample) / 2
	} else {
		m.rate = 0.6*m.rate + 0.4*sample
	}
	if m.rate > p.best {
		p.best = m.rate
	}
}

// fail records a failed transfer and demotes the host.
func (p *mirrorPool) fail(m *mirror) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m.inFlight--
	m.failures++
	m.rate *= 0.3
}

// poolSummary renders mirror health for error messages, so a stall names the
// hosts that caused it instead of just saying "no data".
func poolSummary(p *mirrorPool) string {
	stats := p.Stats()
	parts := make([]string, 0, len(stats))
	for _, s := range stats {
		host := s.URL
		if u, err := url.Parse(s.URL); err == nil {
			host = u.Hostname()
		}
		parts = append(parts, fmt.Sprintf("%s req=%d fail=%d %.1fMB/s",
			host, s.Requests, s.Failures, s.Rate/(1<<20)))
	}
	return strings.Join(parts, ", ")
}

// Stats summarises mirror usage (for the run report).
func (p *mirrorPool) Stats() []MirrorStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]MirrorStat, 0, len(p.ms))
	for _, m := range p.ms {
		out = append(out, MirrorStat{
			URL: m.url, Requests: m.requests, Failures: m.failures, Rate: m.rate,
		})
	}
	return out
}

// MirrorStat is one mirror's contribution to a download.
type MirrorStat struct {
	URL      string  `json:"url"`
	Requests int     `json:"requests"`
	Failures int     `json:"failures"`
	Rate     float64 `json:"bytes_per_second"`
}

// Run downloads the file. It is safe to call again after a failure: completed
// pieces are reported through Have and skipped.
func (d *Downloader) Run(ctx context.Context) error {
	if d.Size <= 0 {
		return fmt.Errorf("download: size must be positive")
	}
	if d.PieceLen <= 0 {
		return fmt.Errorf("download: piece length must be positive")
	}
	if len(d.URLs) == 0 {
		return fmt.Errorf("download: no mirrors")
	}
	if d.Conns < 1 {
		d.Conns = 1
	}
	if d.Window < d.Conns {
		d.Window = d.Conns * 2
	}
	if d.StallTimeout <= 0 {
		d.StallTimeout = 90 * time.Second
	}
	if d.RequestTimeout <= 0 {
		d.RequestTimeout = 60 * time.Second
	}
	if d.ConnsPerHost <= 0 {
		d.ConnsPerHost = 4
	}

	f, err := os.OpenFile(d.PartPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open partial file: %w", err)
	}
	defer f.Close()
	if info, statErr := f.Stat(); statErr == nil && info.Size() != d.Size {
		if err := f.Truncate(d.Size); err != nil {
			return fmt.Errorf("preallocate %s: %w", d.PartPath, err)
		}
	}

	count := d.Count()
	tasks := make(chan int, d.Conns)
	results := make(chan piece, d.Conns)
	window := make(chan struct{}, d.Window)
	pool := newMirrorPool(d.URLs, d.ConnsPerHost)
	var received atomic.Int64
	var written atomic.Int64
	var awaiting atomic.Int64
	var lastActivity atomic.Int64
	lastActivity.Store(time.Now().UnixNano())
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err })
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Rate estimation for progress reporting.
	done := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if d.Progress != nil {
					d.Progress(received.Load())
				}
			}
		}
	}()

	// Stall watchdog: convert a hung mirror into a diagnosable failure instead of
	// an indefinite wait. A mirror that connects and then trickles is otherwise
	// indistinguishable from a deadlock.
	go func() {
		defer bg.Done()
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				last := time.Unix(0, lastActivity.Load())
				if time.Since(last) > d.StallTimeout {
					fail(fmt.Errorf(
						"stalled: no data received for %s (waiting for piece %d/%d, wrote %d, buffers %d/%d)",
						d.StallTimeout, awaiting.Load(), count, written.Load(), len(results), cap(results)))
					cancel()
					return
				}
			}
		}
	}()

	// Snapshot which pieces are already on disk. Later marks (made during this
	// run) must not confuse the writer, and a resumed file may have holes, so the
	// writer has to skip completed pieces rather than assume a prefix.
	donePiece := make([]bool, count)
	if d.Have != nil {
		for i := 0; i < count; i++ {
			donePiece[i] = d.Have(i)
		}
	}

	// Writer: the only goroutine that touches the file for data.
	writerDone := make(chan error, 1)
	go func() {
		defer close(writerDone)
		pending := make(map[int][]byte, d.Window)
		next := 0
		flush := func() bool {
			for {
				for next < count && donePiece[next] {
					next++
				}
				awaiting.Store(int64(next))
				if next >= count {
					return true
				}
				data, have := pending[next]
				if !have {
					return true
				}
				if _, err := f.WriteAt(data, d.offset(next)); err != nil {
					fail(fmt.Errorf("write piece %d: %w", next, err))
					return false
				}
				delete(pending, next)
				if d.OnPiece != nil {
					d.OnPiece(next)
				}
				written.Add(1)
				lastActivity.Store(time.Now().UnixNano())
				<-window
				next++
			}
		}
		for {
			p, ok := <-results
			if !ok {
				// Release credits for anything still buffered so no worker can
				// block forever on shutdown.
				for range pending {
					<-window
				}
				return
			}
			pending[p.idx] = p.data
			if !flush() {
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < d.Conns; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range tasks {
				// Take the window credit *before* fetching, never before sending.
				// Backpressure must throttle how much is fetched, not how much can
				// be delivered: a piece acquired but not deliverable deadlocks the
				// writer, which is exactly how a single slow mirror hung a build.
				select {
				case window <- struct{}{}:
				case <-ctx.Done():
					return
				}
				data, err := d.fetchPiece(ctx, pool, idx)
				if err != nil {
					<-window
					fail(err)
					cancel()
					return
				}
				received.Add(int64(len(data)))
				lastActivity.Store(time.Now().UnixNano())
				// results has room for one entry per worker, so this cannot block.
				results <- piece{idx: idx, data: data}
			}
		}()
	}

feed:
	for i := 0; i < count; i++ {
		if donePiece[i] {
			continue
		}
		select {
		case tasks <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(tasks)
	wg.Wait()
	close(results)
	if err := <-writerDone; err != nil && firstErr == nil {
		firstErr = err
	}
	close(done)
	bg.Wait()
	if d.Progress != nil {
		d.Progress(received.Load())
	}
	if firstErr != nil {
		return fmt.Errorf("%w [%s]", firstErr, poolSummary(pool))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.Sync()
}

// fetchPiece downloads one chunk, retrying across mirrors.
func (d *Downloader) fetchPiece(ctx context.Context, pool *mirrorPool, idx int) ([]byte, error) {
	start, end := d.offset(idx), d.offset(idx)+d.length(idx)-1
	attempts := len(d.URLs) * 2
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		m := pool.pick()
		began := time.Now()
		data, retry, err := d.getRange(ctx, m.url, start, end)
		if err == nil {
			if d.PieceSHA1 != nil && idx < len(d.PieceSHA1) {
				sum := sha1.Sum(data)
				if hex.EncodeToString(sum[:]) != d.PieceSHA1[idx] {
					pool.fail(m)
					lastErr = fmt.Errorf("piece %d: checksum mismatch from %s", idx, m.url)
					continue
				}
			}
			pool.done(m, int64(len(data)), time.Since(began))
			return data, nil
		}
		pool.fail(m)
		lastErr = err
		if !retry {
			return nil, err
		}
		// Honour Retry-After-style backoff, but never stall a whole build.
		delay := time.Duration(attempt+1) * 500 * time.Millisecond
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("all mirrors failed")
	}
	return nil, fmt.Errorf("piece %d: %w", idx, lastErr)
}

// getRange performs one ranged GET. retry reports whether another mirror is
// worth trying for the same chunk.
func (d *Downloader) getRange(ctx context.Context, url string, start, end int64) (data []byte, retry bool, err error) {
	// Bound each request: a mirror that accepts the connection and then stalls
	// would otherwise hold a worker until the client's global timeout.
	reqCtx, cancel := context.WithTimeout(ctx, d.RequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	// Never let a proxy or server compress a byte range.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := d.Client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		// expected
	case resp.StatusCode == http.StatusOK:
		// Server ignored the range. A whole-file 200 for a mid-file chunk would
		// corrupt the output, so refuse rather than guess.
		return nil, true, fmt.Errorf("server ignored range request (status 200)")
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusGatewayTimeout,
		resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("server returned %s", resp.Status)
	default:
		return nil, false, fmt.Errorf("server returned %s", resp.Status)
	}
	if !validContentRange(resp.Header.Get("Content-Range"), start, end) {
		return nil, true, fmt.Errorf("unexpected Content-Range %q", resp.Header.Get("Content-Range"))
	}

	want := end - start + 1
	data = make([]byte, want)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return nil, true, fmt.Errorf("short read: %w", err)
	}
	return data, false, nil
}

// validContentRange checks "bytes start-end/total".
func validContentRange(v string, start, end int64) bool {
	v = strings.TrimSpace(v)
	rest, ok := strings.CutPrefix(v, "bytes ")
	if !ok {
		return false
	}
	span, total, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	if _, err := strconv.ParseInt(total, 10, 64); err != nil {
		return false // "*" is only valid on a 416, never on a partial response
	}
	a, b, ok := strings.Cut(span, "-")
	if !ok {
		return false
	}
	gotStart, err1 := strconv.ParseInt(a, 10, 64)
	gotEnd, err2 := strconv.ParseInt(b, 10, 64)
	return err1 == nil && err2 == nil && gotStart == start && gotEnd == end
}

// Verify hashes a finished file and compares it with the expected digest.
// An empty expected digest computes and returns the digest without failing.
func Verify(path, expectedSHA256 string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if expectedSHA256 != "" && !strings.EqualFold(got, expectedSHA256) {
		return got, &DigestError{Want: expectedSHA256, Got: got}
	}
	return got, nil
}

// DigestError reports a content mismatch.
type DigestError struct{ Want, Got string }

func (e *DigestError) Error() string {
	return fmt.Sprintf("sha256 mismatch: want %s, got %s", e.Want, e.Got)
}
