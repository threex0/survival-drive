package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel string, size int) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverPairsProjectorsAndSizes(t *testing.T) {
	root := t.TempDir()
	write(t, root, "models/gemma-4-E4B/gemma-4-E4B-it-Q4_K_M.gguf", 4096)
	write(t, root, "models/gemma-4-E4B/mmproj-F16.gguf", 512)
	write(t, root, "models/qwen3.5-4B/Qwen3.5-4B-Q4_K_M.gguf", 2048)
	write(t, root, "models/qwen3.5-4B/mmproj-F16.gguf", 256)
	write(t, root, "models/notes/README.txt", 10) // no gguf: ignored

	models, err := Discover(root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("found %d models, want 2", len(models))
	}
	// Ascending by size, so the smallest is first.
	if models[0].ID != "qwen3.5-4B" || models[1].ID != "gemma-4-E4B" {
		t.Fatalf("order = %s, %s", models[0].ID, models[1].ID)
	}
	for _, m := range models {
		if m.MMProj == "" {
			t.Errorf("%s: projector not paired", m.ID)
		}
		if filepath.Base(m.Path) == "mmproj-F16.gguf" {
			t.Errorf("%s: projector was chosen as the model file", m.ID)
		}
		if m.Size != int64(4096) && m.Size != int64(2048) {
			t.Errorf("%s: size = %d, want the model file size", m.ID, m.Size)
		}
	}
}

func TestDiscoverErrorsWithoutModels(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(root); err == nil {
		t.Fatal("want an error when no .gguf exists")
	}
}

func TestSelectModelByRAM(t *testing.T) {
	small := Model{ID: "small", Size: 2 << 30}
	mid := Model{ID: "mid", Size: 6 << 30}
	large := Model{ID: "large", Size: 16 << 30}
	all := []Model{small, mid, large}

	if got, _ := SelectModel(all, 8<<30); got.ID != "small" {
		t.Errorf("8 GB picked %s, want small (mid needs 8.5)", got.ID)
	}
	if got, _ := SelectModel(all, 16<<30); got.ID != "mid" {
		t.Errorf("16 GB picked %s, want mid", got.ID)
	}
	if got, _ := SelectModel(all, 32<<30); got.ID != "large" {
		t.Errorf("32 GB picked %s, want large", got.ID)
	}
	// Nothing fits: refuse rather than load a model that will not fit. Loading it
	// anyway is how the assistant ends up taking the library down with it.
	if _, err := SelectModel(all, 2<<30); err == nil {
		t.Error("want an error when no model fits the budget")
	}
	if _, err := SelectModel(nil, 8<<30); err == nil {
		t.Error("want an error with no models")
	}
}

func TestAIStartMode(t *testing.T) {
	// 4.64 GiB weights x 1.25 + 1 GiB, computed the way SelectModel does.
	smallest := int64(float64(4984194560)*1.25) + (1 << 30)

	// Default: never auto-start, even on a big machine. The library must always
	// come up before the model can compete for the same USB device.
	if auto, why := AIStartMode(false, 32<<30, 20<<30, smallest); auto {
		t.Error("the default must be on-request, not auto-start")
	} else if !strings.Contains(why, "on request") {
		t.Errorf("reason = %q, want it to mention on-request", why)
	}
	// Explicit opt-in on a big machine with room: allowed.
	if auto, why := AIStartMode(true, 32<<30, 20<<30, smallest); !auto {
		t.Errorf("32 GiB with 20 free and -auto-ai should start, got %q", why)
	}
	// Opt-in does not override the 24 GiB threshold.
	if auto, why := AIStartMode(true, 16<<30, 12<<30, smallest); auto || !strings.Contains(why, "on request") {
		t.Errorf("16 GiB with -auto-ai: auto=%v why=%q", auto, why)
	}
	// Small machine: not enough free memory even to offer it.
	if auto, why := AIStartMode(true, 8<<30, 3<<30, smallest); auto || !strings.Contains(why, "memory") {
		t.Errorf("8 GiB with 3 free: auto=%v why=%q", auto, why)
	}
	// No model on the drive at all.
	if auto, why := AIStartMode(true, 64<<30, 60<<30, 0); auto || why == "" {
		t.Error("no model must never auto-start")
	}
	// A 400 GiB Bunker model on a 32 GiB machine: no.
	huge := int64(float64(15430000000) * 1.25)
	if auto, _ := AIStartMode(true, 32<<30, 20<<30, huge); auto {
		t.Error("a 27B model should not auto-start on 32 GiB")
	}
}

func TestNormalizeRootPath(t *testing.T) {
	sep := string(os.PathSeparator)
	if got := normalizeRootPath("G:"); got != "G:"+sep {
		t.Errorf("normalizeRootPath(G:) = %q, want a drive root", got)
	}
	if got := normalizeRootPath("G:" + sep); got != "G:"+sep {
		t.Errorf("an already-root path changed to %q", got)
	}
	if got := normalizeRootPath(`C:\data`); got != `C:\data` {
		t.Errorf("a directory path changed to %q", got)
	}
}

func TestModelBudgetReservesForLibraryAndOS(t *testing.T) {
	// 8 GiB free: minus 1.4 GiB library, minus 1.5 GiB system.
	got := ModelBudget(8 << 30)
	want := int64(8<<30) - libraryReserve - systemReserve
	if got != want {
		t.Errorf("ModelBudget(8GiB) = %d, want %d", got, want)
	}
	if ModelBudget(1<<30) != 0 {
		t.Error("a nearly-full machine must report no budget, not a negative one")
	}

	small := Model{ID: "small", Size: 2 << 30}
	mid := Model{ID: "mid", Size: 6 << 30}
	// 2 GiB weights x 1.25 + 1 GiB runtime overhead.
	if need := SmallestModelNeed([]Model{small, mid}); need != int64(2<<30)*5/4+(1<<30) {
		t.Errorf("SmallestModelNeed = %d, want 3.5 GiB", need)
	}
	if SmallestModelNeed(nil) != 0 {
		t.Error("SmallestModelNeed(nil) = 0 expected")
	}

	// A 4 GiB machine cannot host even the 2 GiB model alongside the library.
	if b := ModelBudget(4 << 30); b >= SmallestModelNeed([]Model{small}) {
		t.Error("a 4 GiB machine should not be offered a model")
	}
}

func TestContextFor(t *testing.T) {
	cases := []struct {
		ram  int64
		want int
	}{
		{4 << 30, 4096},
		{8 << 30, 4096},
		{12 << 30, 8192},
		{23 << 30, 8192},
		{24 << 30, 16384},
		{64 << 30, 16384},
	}
	for _, c := range cases {
		if got := ContextFor(c.ram); got != c.want {
			t.Errorf("ContextFor(%d GiB) = %d, want %d", c.ram>>30, got, c.want)
		}
	}
}

func TestBookName(t *testing.T) {
	zims := []string{
		`C:\x\archives\wikipedia_en_all_nopic_2026-06.zim`,
		`C:\x\archives\medlineplus.gov_en_all_2025-01.zim`,
		`C:\x\archives\diy.stackexchange.com_en_all_2026-10.zim`,
	}
	if got := BookName(zims, "wikipedia_en"); got != "wikipedia_en_all_nopic_2026-06" {
		t.Errorf("wikipedia = %q", got)
	}
	if got := BookName(zims, "medlineplus"); got != "medlineplus.gov_en_all_2025-01" {
		t.Errorf("medlineplus = %q", got)
	}
	if got := BookName(zims, "nope"); got != "" {
		t.Errorf("missing prefix = %q, want empty", got)
	}
}

func TestArgsAvoidUnknownFlags(t *testing.T) {
	m := Model{Path: `C:\m\a.gguf`, MMProj: `C:\m\mmproj.gguf`}
	args := ServerArgs(m, 8192, 8080)
	want := []string{"-m", `C:\m\a.gguf`, "-c", "8192", "--host", "127.0.0.1", "--port", "8080",
		"--mmproj", `C:\m\mmproj.gguf`}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}

	// Without a projector the --mmproj pair is omitted entirely.
	plain := ServerArgs(Model{Path: "a.gguf"}, 4096, 1)
	for _, a := range plain {
		if a == "--mmproj" {
			t.Fatal("--mmproj must not appear without a projector")
		}
	}

	kz := KiwixArgs([]string{"a.zim", "b.zim"}, 8081)
	if kz[0] != "--port" || kz[1] != "8081" || len(kz) != 4 {
		t.Fatalf("kiwix args = %v", kz)
	}
}

func TestDiscoverZIMsAndPlan(t *testing.T) {
	root := t.TempDir()
	write(t, root, "archives/b.zim", 10)
	write(t, root, "archives/a.zim", 10)
	write(t, root, "archives/ignore.txt", 10)
	write(t, root, "models/m/x.gguf", 1024)
	write(t, root, "models/m/mmproj-F16.gguf", 100)

	zims, err := DiscoverZIMs(root)
	if err != nil {
		t.Fatalf("zims: %v", err)
	}
	if len(zims) != 2 || filepath.Base(zims[0]) != "a.zim" {
		t.Fatalf("zims = %v, want a.zim then b.zim", zims)
	}

	l, err := New(Config{Root: root, NoAI: false, NoKiwix: false})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := l.Plan()
	if len(p.ZIMs) != 2 || p.Model.ID != "m" {
		t.Fatalf("plan = %+v", p)
	}
	if p.UIPort == 0 || p.UIPort == p.KiwixPort || p.LLAMAPort == p.KiwixPort {
		t.Fatalf("ports not distinct: ui=%d llama=%d kiwix=%d", p.UIPort, p.LLAMAPort, p.KiwixPort)
	}
	if p.Context != ContextFor(p.RAM) {
		t.Errorf("context %d not derived from RAM %d", p.Context, p.RAM)
	}
}

func TestAIUnavailableDoesNotBlockLibrary(t *testing.T) {
	// The guarantee that matters on an old machine: a missing/unusable AI must
	// never stop kiwix-serve from starting.
	root := t.TempDir()
	write(t, root, "archives/a.zim", 10)
	// Deliberately no models directory at all.
	l, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !l.cfg.NoAI {
		t.Fatal("AI should be disabled when no model exists")
	}

	// startServices must still attempt the library (the binary is absent here, so
	// it fails - but it must have been attempted and configured).
	l.startServices(context.Background())
	if l.kiwix == nil {
		t.Fatal("library was never attempted")
	}
	if l.kiwix.port == 0 || len(l.kiwix.args) == 0 {
		t.Error("library child was not configured with port/args")
	}
	if l.kiwix.err == "" {
		t.Error("expected a library error for the missing binary")
	}
	if l.llama != nil {
		t.Error("AI child must not exist when no model is available")
	}
}

func TestMissingAIBinaryIsReportedNotSilent(t *testing.T) {
	// A model but no llama-server binary is a broken image. The user must be told
	// why there is no AI, and the library must still be attempted.
	root := t.TempDir()
	write(t, root, "archives/a.zim", 10)
	write(t, root, "models/m/x.gguf", 1024)
	l, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !l.cfg.NoAI {
		t.Fatal("AI should be disabled when no server binary exists")
	}
	warnings := strings.Join(l.Plan().Warnings, " | ")
	if !strings.Contains(warnings, "llama-server") {
		t.Errorf("warnings = %q, want one naming the missing binary", warnings)
	}

	l.startServices(context.Background())
	if l.kiwix == nil {
		t.Fatal("library was never attempted")
	}
	if l.kiwix.port == 0 || len(l.kiwix.args) == 0 {
		t.Error("library child was not configured with port/args")
	}
}

func TestNewDegradesWithoutContent(t *testing.T) {
	root := t.TempDir()
	l, err := New(Config{Root: root})
	if err != nil {
		t.Fatalf("New must not fail on an empty drive: %v", err)
	}
	// Both services unavailable, but the launcher still has to run and explain.
	if len(l.Plan().Warnings) == 0 {
		t.Error("want warnings explaining the missing model and archives")
	}
}
