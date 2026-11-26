// Package launcher starts the two offline services on a drive and serves the
// dashboard that ties them together.
//
// It is the only piece of this project a buyer sees, so it optimises for "one
// double-click, then it works": detect the machine, choose a model that fits,
// start both servers, wait until they answer, open a browser, and shut
// everything down when the window closes.
package launcher

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"survivaldrive/internal/media"
)

//go:embed ui/index.html
var uiFS embed.FS

// Config is what the caller can change.
type Config struct {
	Root         string
	OpenBrowser  bool
	NoAI         bool
	NoKiwix      bool
	PreferCPU    bool
	ReadyTimeout time.Duration
	LogDir       string
	// UIPort pins the dashboard port (tests and support); 0 picks a free one.
	UIPort int
	// AssumeRAMGiB / AssumeFreeGiB override the memory probes so the
	// small-machine behaviour can be demonstrated on any hardware. 0 = measure.
	AssumeRAMGiB  int
	AssumeFreeGiB int
	// AutoAI lets the assistant start without a click on a machine that meets
	// the memory budget. Off by default: the library is the product that must
	// always come up, and a multi-gigabyte model loading from the same USB
	// device can make every page slow. Owners start it from the dashboard.
	AutoAI bool
}

// child is one supervised process.
type child struct {
	name  string
	path  string
	args  []string
	port  int
	cmd   *exec.Cmd
	up    bool
	err   string
	start time.Time
}

// normalizeRootPath turns a bare drive letter into a drive root. On Windows a
// path like "G:" is drive-relative; joining "bin" to it yields "G:bin", which
// does not exist. Children must always be resolved from "G:\".
func normalizeRootPath(root string) string {
	if len(root) == 2 && root[1] == ':' {
		return root + string(os.PathSeparator)
	}
	return root
}

// Launcher owns the children and the dashboard.
type Launcher struct {
	cfg      Config
	plan     Plan
	mu       sync.Mutex
	llama    *child
	kiwix    *child
	start    time.Time
	logs     map[string]*os.File
	cancel   context.CancelFunc
	starting bool
	logmu    sync.Mutex
	// aiStarting guards the on-demand start so two clicks cannot load the model
	// twice; runCtx is the lifetime used for the child process.
	aiStarting bool
	runCtx     context.Context
}

// New resolves the drive layout and decides what to run. It starts nothing.
func New(cfg Config) (*Launcher, error) {
	if cfg.Root == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		cfg.Root = filepath.Dir(exe)
	}
	cfg.Root = normalizeRootPath(cfg.Root)
	if cfg.ReadyTimeout <= 0 {
		// Loading a 5 GB model off a USB stick is not fast; llama-server answers
		// 503 until it is done, so this budget is "model loaded", not "port open".
		cfg.ReadyTimeout = 4 * time.Minute
	}
	if cfg.LogDir != "" {
		_ = os.MkdirAll(cfg.LogDir, 0o755)
	}
	// Always keep child output somewhere. Without this a buyer's "it didn't
	// start" report contains nothing to diagnose, and the launcher's own reason
	// for a fallback is invisible.
	if cfg.LogDir == "" {
		dir := filepath.Join(cfg.Root, "logs")
		if err := os.MkdirAll(dir, 0o755); err == nil {
			cfg.LogDir = dir
		}
	}
	root := cfg.Root

	ram, ramErr := TotalRAM()
	avail := int64(0)
	if total, a, err := RAM(); err == nil {
		ram, avail = total, a
	}
	plan := Plan{Root: root, RAM: ram, Avail: avail}
	if ramErr != nil && ram == 0 {
		plan.Warnings = append(plan.Warnings,
			"could not detect RAM ("+ramErr.Error()+"); assuming 8 GB")
		ram = 8 << 30
		avail = ram
		plan.RAM = ram
	}
	if avail <= 0 {
		// No reliable free-memory figure: be conservative and treat half of the
		// installed RAM as available.
		avail = ram / 2
	}
	if cfg.AssumeRAMGiB > 0 {
		ram = int64(cfg.AssumeRAMGiB) << 30
		avail = int64(cfg.AssumeFreeGiB) << 30
		if cfg.AssumeFreeGiB <= 0 {
			avail = ram / 2
		}
		plan.RAM, plan.Avail = ram, avail
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"memory probes overridden: pretending %.0f GiB installed, %.0f GiB free",
			float64(ram)/(1<<30), float64(avail)/(1<<30)))
	}

	if !cfg.NoAI {
		models, err := Discover(root)
		if err != nil {
			plan.Warnings = append(plan.Warnings, "no AI model: "+err.Error())
			cfg.NoAI = true
			plan.AIReason = "no model on this drive"
		} else {
			plan.AIModel = true

			// Configure everything an on-demand start needs, whether or not it is
			// auto-started. Deferring the AI must not leave the assistant
			// unstartable - that was a real bug: the button returned
			// "started on request" and then had no server to launch.
			if m, selErr := SelectModel(models, ModelBudget(avail)); selErr == nil {
				plan.Model = m
				plan.Context = ContextFor(ram)
				primaryDir, _, primaryExe, _ := Backend(root)
				if cfg.PreferCPU {
					_, _, _, cpuExe := Backend(root)
					if cpuExe != "" {
						primaryExe, primaryDir = cpuExe, filepath.Dir(cpuExe)
					}
				}
				plan.Server = primaryExe
				plan.Backend = filepath.Base(primaryDir)
				plan.AICanStart = plan.Server != ""
			} else {
				plan.AIReason = selErr.Error()
			}
			if plan.AICanStart && plan.Server == "" {
				plan.AICanStart = false
			}
			if plan.Server == "" && plan.AIReason == "" {
				plan.AIReason = "the assistant binary is missing from this drive"
				plan.Warnings = append(plan.Warnings,
					"no llama-server binary under bin\\llama-vulkan or bin\\llama-cpu; library unaffected")
			}

			auto, reason := AIStartMode(cfg.AutoAI, ram, avail, SmallestModelNeed(models))
			if !auto {
				if reason != "" {
					plan.AIReason = reason
					plan.Warnings = append(plan.Warnings, reason)
				}
				cfg.NoAI = true
			}
			// Keep the on-demand path available even if an auto-start failed:
			// the card can offer a retry. Never offer it when the machine cannot
			// host a model at all.
			plan.AIOnDemand = plan.AICanStart
			if !plan.AICanStart {
				cfg.NoAI = true
				plan.AIOnDemand = false
			}
		}
	}

	if !cfg.NoKiwix {
		zims, err := DiscoverZIMs(root)
		if err != nil {
			plan.Warnings = append(plan.Warnings, "no archives: "+err.Error())
			cfg.NoKiwix = true
		} else {
			plan.ZIMs = zims
		}
	}

	var ports []int
	for i := 0; i < 3; i++ {
		p, err := freePort()
		if err != nil {
			return nil, err
		}
		ports = append(ports, p)
	}
	plan.UIPort, plan.LLAMAPort, plan.KiwixPort = ports[0], ports[1], ports[2]
	if cfg.UIPort != 0 {
		plan.UIPort = cfg.UIPort
	}

	l := &Launcher{cfg: cfg, plan: plan, start: time.Now(), logs: map[string]*os.File{}}
	l.logf("start: root=%s ram=%.1fGiB model=%s backend=%s ports ui=%d llama=%d kiwix=%d archives=%d",
		plan.Root, float64(plan.RAM)/(1<<30), plan.Model.ID, plan.Backend,
		plan.UIPort, plan.LLAMAPort, plan.KiwixPort, len(plan.ZIMs))
	for _, w := range plan.Warnings {
		l.logf("warning: %s", w)
	}
	return l, nil
}

// Plan exposes the decisions (for -print-plan and tests).
func (l *Launcher) Plan() Plan { return l.plan }

// Run starts the dashboard first, then the services behind it, and blocks until
// ctx is done.
//
// Serving the UI before the children matters: a cold start reads a 4.6 GiB model
// off USB (~2.5 minutes on real media) and a dead port for that long looks
// exactly like a broken program.
func (l *Launcher) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l.cancel = cancel

	dash, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", l.plan.UIPort))
	if err != nil {
		return fmt.Errorf("dashboard port %d unavailable: %w", l.plan.UIPort, err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", l.dashboardHandler())
	mux.HandleFunc("/api/status", l.handleStatus)
	mux.HandleFunc("/api/verify", l.handleVerify)
	mux.HandleFunc("/api/ai/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !l.StartAI() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": l.plan.AIReason,
			})
			return
		}
		fmt.Fprintln(w, "starting")
	})
	mux.HandleFunc("/api/ai/stop", func(w http.ResponseWriter, r *http.Request) {
		l.StopAI()
		fmt.Fprintln(w, "stopped")
	})
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "stopping")
		// Cancelling the run context is what actually ends the server; stopping
		// only the children would leave the dashboard answering on a dead app.
		if l.cancel != nil {
			go func() {
				time.Sleep(300 * time.Millisecond)
				l.cancel()
			}()
		}
	})

	srv := &http.Server{Handler: mux}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(dash) }()

	l.mu.Lock()
	l.starting = true
	l.mu.Unlock()

	go func() {
		<-ctx.Done()
		shutdownCtx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if l.cfg.OpenBrowser {
		go openBrowser(fmt.Sprintf("http://127.0.0.1:%d", l.plan.UIPort))
	}

	go l.startServices(ctx)

	err = <-serveErr
	l.Stop()
	if err == http.ErrServerClosed {
		l.logf("shutdown: %v", ctx.Err())
		return nil
	}
	l.logf("dashboard stopped unexpectedly: %v (children stopped)", err)
	return err
}

// startServices brings the children up, reporting progress.
//
// The library is started and confirmed first: it is two orders of magnitude
// smaller to load than the model, it is what a user reaches for in a hurry, and
// running both at once on a USB stick thrashes the device (measured: 17 s for the
// library alone, 5+ minutes when the model loads beside it).
func (l *Launcher) startServices(ctx context.Context) {
	defer func() {
		l.mu.Lock()
		l.starting = false
		l.mu.Unlock()
	}()

	var wg sync.WaitGroup

	// The library goes first and is allowed to finish before the AI starts.
	// Starting them together looked attractive (max instead of sum) but on a USB
	// stick the model load and the archive indexing fight over the same device:
	// measured 17 s for the library when run alone versus over five minutes when
	// run concurrently, which leaves the user staring at "Starting...".
	if !l.cfg.NoKiwix && l.kiwix == nil {
		l.startLibrary(ctx)
	}
	if !l.cfg.NoAI && l.plan.Server != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Crucially, an AI panic must not kill the process that is serving the
			// library. A GPU driver crashing inside llama.cpp is exactly the kind
			// of failure that must not take the encyclopedia with it.
			defer l.recoverFrom("AI")
			l.startAIReported(ctx)
		}()
		wg.Wait()
	}
	l.logf("startup complete: kiwix up=%v llama up=%v", l.kiwixUp(), l.llamaUp())
	fmt.Println("  ready")
}

// recoverFrom turns a panic in one service goroutine into a logged warning
// instead of a crashed launcher.
func (l *Launcher) recoverFrom(which string) {
	if r := recover(); r != nil {
		msg := fmt.Sprintf("%s panicked: %v", which, r)
		l.logf("%s", msg)
		l.mu.Lock()
		l.plan.Warnings = append(l.plan.Warnings, msg)
		l.mu.Unlock()
		fmt.Printf("  %s\n", msg)
	}
}

func (l *Launcher) kiwixUp() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.kiwix != nil && l.kiwix.up
}

func (l *Launcher) llamaUp() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.llama != nil && l.llama.up
}

// startLibrary starts kiwix-serve. Failure is contained: it is reported, and the
// AI (if any) is unaffected.
func (l *Launcher) startLibrary(ctx context.Context) {
	fmt.Println("  starting library ...")
	l.logf("starting library: %d archives", len(l.plan.ZIMs))
	c := &child{
		name: "kiwix-serve",
		path: filepath.Join(l.plan.Root, "bin", "kiwix", exeName("kiwix-serve")),
		args: append(KiwixArgs(l.plan.ZIMs, l.plan.KiwixPort),
			// kiwix-serve can watch a PID and exit when it disappears, so a
			// crashed launcher does not leave a server holding a port.
			fmt.Sprintf("--attachToProcess=%d", os.Getpid())),
		port: l.plan.KiwixPort,
	}
	l.mu.Lock()
	l.kiwix = c
	l.mu.Unlock()

	if err := l.startChild(ctx, c); err != nil {
		l.note(c, err)
		l.logf("library FAILED: %v", err)
		fmt.Printf("  library failed: %v\n", err)
		return
	}
	l.logf("library ready in %s on port %d", time.Since(c.start).Round(time.Second), c.port)
	fmt.Printf("  library ready (%d archives)\n", len(l.plan.ZIMs))
}

// StartAI starts the assistant on request. It reports false when the machine
// cannot host it or it is already running/starting.
func (l *Launcher) StartAI() bool {
	if !l.plan.AICanStart || l.plan.Server == "" {
		return false
	}
	l.mu.Lock()
	if l.aiStarting || (l.llama != nil && l.llama.up) {
		l.mu.Unlock()
		return false
	}
	l.aiStarting = true
	ctx := l.runCtx
	l.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}

	go func() {
		defer func() {
			l.mu.Lock()
			l.aiStarting = false
			l.mu.Unlock()
		}()
		defer l.recoverFrom("AI")
		l.logf("assistant requested by the user")
		l.startAIReported(ctx)
	}()
	return true
}

// StopAI unloads the model, freeing its memory immediately. The library is
// untouched.
func (l *Launcher) StopAI() {
	l.mu.Lock()
	c := l.llama
	l.llama = nil
	l.mu.Unlock()
	if c != nil && c.cmd != nil && c.cmd.Process != nil {
		l.logf("assistant stopped on request")
		_ = c.cmd.Process.Kill()
	}
}

// startAIReported logs and prints the outcome of an assistant start.
func (l *Launcher) startAIReported(ctx context.Context) {
	fmt.Println("  loading model ... (a cold start from USB can take 2-3 minutes)")
	l.logf("starting AI: %s (%s, ctx %d)", l.plan.Model.ID, l.plan.Backend, l.plan.Context)
	begin := time.Now()
	if err := l.startAI(ctx); err != nil {
		l.note(nil, err)
		l.logf("AI FAILED after %s: %v", time.Since(begin).Round(time.Second), err)
		fmt.Printf("  AI failed: %v\n", err)
		fmt.Println("  the library is unaffected and fully usable")
		return
	}
	l.logf("AI ready in %s (%s, port %d)", time.Since(begin).Round(time.Second), l.plan.Backend, l.plan.LLAMAPort)
	fmt.Printf("  AI ready (%s)\n", l.plan.Model.ID)
}

// logf appends a line to <logs>\launcher.log. Support reports from another
// machine are otherwise just "it didn't work".
func (l *Launcher) logf(format string, args ...any) {
	if l.cfg.LogDir == "" {
		return
	}
	l.logmu.Lock()
	defer l.logmu.Unlock()
	f, err := os.OpenFile(filepath.Join(l.cfg.LogDir, "launcher.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// startAI starts llama-server, falling back to the CPU build if the GPU build
// dies before answering.
func (l *Launcher) startAI(ctx context.Context) error {
	primaryDir, _, primaryExe, fallbackExe := Backend(l.plan.Root)
	if l.cfg.PreferCPU && fallbackExe != "" {
		cpuDir := filepath.Dir(fallbackExe)
		return l.tryAI(ctx, cpuDir, fallbackExe, "")
	}

	c := &child{
		name: "llama-server",
		path: primaryExe,
		args: ServerArgs(l.plan.Model, l.plan.Context, l.plan.LLAMAPort),
		port: l.plan.LLAMAPort,
	}
	l.llama = c
	err := l.startChild(ctx, c)
	if err == nil {
		return nil
	}
	if fallbackExe == "" {
		return fmt.Errorf("AI server failed: %w", err)
	}
	// The GPU build did not come up: retry on the CPU build.
	path := filepath.Join(primaryDir, "..", "x")
	_ = path
	l.plan.Warnings = append(l.plan.Warnings,
		"GPU backend failed to start; falling back to CPU")
	return l.tryAI(ctx, filepath.Dir(fallbackExe), fallbackExe, err.Error())
}

func (l *Launcher) tryAI(ctx context.Context, dir, exe, previous string) error {
	c := &child{
		name: "llama-server",
		path: exe,
		args: ServerArgs(l.plan.Model, l.plan.Context, l.plan.LLAMAPort),
		port: l.plan.LLAMAPort,
		err:  previous,
	}
	l.llama = c
	l.plan.Backend = filepath.Base(dir)
	l.plan.Server = exe
	return l.startChild(ctx, c)
}

// startChild launches a process and waits until its HTTP endpoint answers.
func (l *Launcher) startChild(ctx context.Context, c *child) error {
	if _, err := os.Stat(c.path); err != nil {
		return fmt.Errorf("%s not found at %s", c.name, c.path)
	}
	cmd := exec.CommandContext(ctx, c.path, c.args...)
	cmd.Dir = filepath.Dir(c.path)
	if l.cfg.LogDir != "" {
		if f, err := os.Create(filepath.Join(l.cfg.LogDir, c.name+".log")); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
			l.logs[c.name] = f
		}
	} else {
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", c.name, err)
	}
	c.cmd = cmd
	c.start = time.Now()

	// Watch for early exit while we poll for readiness.
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		exited <- err
		l.mu.Lock()
		c.up = false
		c.err = fmt.Sprintf("%s exited", c.name)
		l.mu.Unlock()
		if c.name == "llama-server" {
			// A model or GPU crash is not allowed to look like a dead drive.
			l.logf("llama-server exited (%v); library unaffected and still serving", err)
		} else {
			l.logf("%s exited (%v)", c.name, err)
		}
	}()

	deadline := time.Now().Add(l.cfg.ReadyTimeout)
	nextNotice := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			c.err = fmt.Sprintf("%s exited during startup: %v", c.name, err)
			return fmt.Errorf("%s", c.err)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if httpReady(c.port, c.name) {
			l.mu.Lock()
			c.up = true
			c.err = ""
			l.mu.Unlock()
			return nil
		}
		// A cold model load reads ~5 GB off the drive; silence for minutes looks
		// like a hang, so say something periodically.
		if time.Now().After(nextNotice) {
			nextNotice = time.Now().Add(15 * time.Second)
			elapsed := time.Since(c.start).Round(time.Second)
			l.logf("%s still starting after %s", c.name, elapsed)
			if c.name == "llama-server" {
				fmt.Printf("\r  loading model ... %s   ", elapsed)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.err = fmt.Sprintf("%s did not answer within %s", c.name, l.cfg.ReadyTimeout)
	return fmt.Errorf("%s", c.err)
}

// httpReady reports whether a service is actually usable.
//
// For llama-server that means /health answering **200**: while the model loads it
// answers 503 {"status":"loading model"}, and every inference request is refused.
// Accepting anything under 500 made the launcher announce "AI ready" twelve
// seconds in, and the first prompt failed with 503.
func httpReady(port int, name string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	if name == "llama-server" {
		resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (l *Launcher) note(c *child, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c != nil {
		c.err = err.Error()
	}
	l.plan.Warnings = append(l.plan.Warnings, err.Error())
}

// Stop terminates the children.
func (l *Launcher) Stop() {
	l.logf("stopping children")
	for _, c := range []*child{l.llama, l.kiwix} {
		if c != nil && c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
	for _, f := range l.logs {
		_ = f.Close()
	}
}

type statusChild struct {
	Name    string `json:"name"`
	Up      bool   `json:"up"`
	Port    int    `json:"port"`
	URL     string `json:"url"`
	Error   string `json:"error,omitempty"`
	UptimeS int    `json:"uptime_s,omitempty"`
}

type status struct {
	Root       string            `json:"root"`
	Model      string            `json:"model"`
	ModelGiB   float64           `json:"model_gib"`
	Context    int               `json:"context"`
	Backend    string            `json:"backend"`
	RAMGiB     float64           `json:"ram_gib"`
	AvailGiB   float64           `json:"avail_gib"`
	ZIMs       int               `json:"zims"`
	UptimeS    int               `json:"uptime_s"`
	Starting   bool              `json:"starting"`
	AICanStart bool              `json:"ai_can_start"`
	AIOnDemand bool              `json:"ai_on_demand"`
	AIReason   string            `json:"ai_reason,omitempty"`
	Books      map[string]string `json:"books"`
	Kiwix      statusChild       `json:"kiwix"`
	LLAMA      statusChild       `json:"llama"`
	Warnings   []string          `json:"warnings,omitempty"`
}

func (l *Launcher) snapshot() status {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := status{
		Root:       l.plan.Root,
		Model:      l.plan.Model.ID,
		ModelGiB:   float64(l.plan.Model.Size) / (1 << 30),
		Context:    l.plan.Context,
		Backend:    l.plan.Backend,
		RAMGiB:     float64(l.plan.RAM) / (1 << 30),
		AvailGiB:   float64(l.plan.Avail) / (1 << 30),
		ZIMs:       len(l.plan.ZIMs),
		UptimeS:    int(time.Since(l.start).Seconds()),
		Starting:   l.starting,
		AICanStart: l.plan.AICanStart,
		AIOnDemand: l.plan.AIOnDemand,
		AIReason:   l.plan.AIReason,
		Warnings:   l.plan.Warnings,
		Books: map[string]string{
			"wikipedia":   BookName(l.plan.ZIMs, "wikipedia_en"),
			"medlineplus": BookName(l.plan.ZIMs, "medlineplus"),
			"diy":         BookName(l.plan.ZIMs, "diy."),
		},
	}
	if l.kiwix != nil {
		s.Kiwix = statusChild{
			Name: l.kiwix.name, Up: l.kiwix.up, Port: l.kiwix.port,
			URL:   fmt.Sprintf("http://127.0.0.1:%d", l.kiwix.port),
			Error: l.kiwix.err, UptimeS: uptime(l.kiwix),
		}
	}
	if l.llama != nil {
		s.LLAMA = statusChild{
			Name: l.llama.name, Up: l.llama.up, Port: l.llama.port,
			URL:   fmt.Sprintf("http://127.0.0.1:%d", l.llama.port),
			Error: l.llama.err, UptimeS: uptime(l.llama),
		}
	}
	return s
}

func uptime(c *child) int {
	if c == nil || c.start.IsZero() {
		return 0
	}
	return int(time.Since(c.start).Seconds())
}

func (l *Launcher) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(l.snapshot())
}

// handleVerify runs the fast (size-only) manifest check. It is the buyer-facing
// "is my drive still good?" button: seconds, not the half-hour full check.
func (l *Launcher) handleVerify(w http.ResponseWriter, r *http.Request) {
	entries, err := media.ReadManifest(l.plan.Root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bad, err := media.Verify(r.Context(), l.plan.Root, media.VerifyOptions{
		Full: false, Entries: entries,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"checked": len(entries),
		"missing": bad,
		"ok":      len(bad) == 0,
	})
}

func (l *Launcher) dashboardHandler() http.Handler {
	page, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "dashboard asset missing", http.StatusInternalServerError)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	})
}

// freePort asks the OS for an unused localhost port.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
