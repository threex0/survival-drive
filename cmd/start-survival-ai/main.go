// Command start-survival-ai is the buyer-facing launcher: one double-click that
// brings up the AI assistant and the offline library, then opens the dashboard.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"survivaldrive/internal/launcher"
)

func main() {
	root := flag.String("root", "", "drive root (default: directory of this executable)")
	uiPort := flag.Int("port", 0, "dashboard port (default: pick a free one)")
	noAI := flag.Bool("no-ai", false, "serve the library only")
	noKiwix := flag.Bool("no-kiwix", false, "serve the AI only")
	noOpen := flag.Bool("no-open", false, "do not open a browser")
	cpu := flag.Bool("cpu", false, "force the CPU backend")
	planOnly := flag.Bool("print-plan", false, "print what would be started, then exit")
	logDir := flag.String("log", "", "write child process logs here")
	assumeRAM := flag.Int("assume-ram-gib", 0, "pretend this much RAM (support/testing)")
	assumeFree := flag.Int("assume-free-gib", 0, "pretend this much free RAM (support/testing)")
	flag.Parse()

	cfg := launcher.Config{
		Root:          *root,
		OpenBrowser:   !*noOpen,
		NoAI:          *noAI,
		NoKiwix:       *noKiwix,
		PreferCPU:     *cpu,
		LogDir:        *logDir,
		UIPort:        *uiPort,
		AssumeRAMGiB:  *assumeRAM,
		AssumeFreeGiB: *assumeFree,
	}
	l, err := launcher.New(cfg)
	if err != nil {
		fail(err)
	}

	p := l.Plan()
	if *planOnly {
		fmt.Printf("root     : %s\n", p.Root)
		fmt.Printf("RAM      : %.1f GiB (%.1f GiB free at start)\n", float64(p.RAM)/(1<<30), float64(p.Avail)/(1<<30))
		fmt.Printf("model    : %s (%.2f GiB)\n", p.Model.ID, float64(p.Model.Size)/(1<<30))
		fmt.Printf("mmproj   : %s\n", p.Model.MMProj)
		fmt.Printf("backend  : %s\n", p.Backend)
		fmt.Printf("server   : %s\n", p.Server)
		fmt.Printf("context  : %d\n", p.Context)
		fmt.Printf("ports    : ui=%d llama=%d kiwix=%d\n", p.UIPort, p.LLAMAPort, p.KiwixPort)
		fmt.Printf("archives : %d\n", len(p.ZIMs))
		for _, w := range p.Warnings {
			fmt.Printf("warning  : %s\n", w)
		}
		return
	}

	fmt.Printf("SURVIVAL AI\n")
	fmt.Printf("  model   %s (%.2f GiB, %s)\n", p.Model.ID, float64(p.Model.Size)/(1<<30), p.Backend)
	fmt.Printf("  library %d archives\n", len(p.ZIMs))
	fmt.Printf("  opening http://127.0.0.1:%d\n\n", p.UIPort)
	for _, w := range p.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
	fmt.Println("  close this window or press Ctrl-C to stop")
	fmt.Println("  NOTE: the library and assistant stop with this window - keep it open while browsing")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := l.Run(ctx); err != nil {
		fail(err)
	}
	fmt.Println("stopped")
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "\nSTARTUP FAILED: %v\n\n", err)
	fmt.Println("Press Enter to close (the window would close instantly otherwise).")
	time.Sleep(200 * time.Millisecond)
	_, _ = fmt.Scanln()
	os.Exit(1)
}
