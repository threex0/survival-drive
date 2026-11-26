package launcher

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Model is one loadable model on the drive.
type Model struct {
	ID     string // directory name, e.g. "gemma-4-E4B"
	Path   string // absolute path to the .gguf
	Size   int64
	MMProj string // absolute path to the vision projector, may be empty
}

// Plan is what the launcher decided to run.
type Plan struct {
	Root      string
	Model     Model
	Backend   string // "llama-vulkan" or "llama-cpu"
	Server    string // absolute path to llama-server
	Context   int
	LLAMAPort int
	KiwixPort int
	UIPort    int
	ZIMs      []string
	RAM       int64
	// Avail is the free physical memory seen at startup. The AI budget is derived
	// from this, not from RAM: kiwix already holds ~900 MB before a model loads.
	Avail int64
	// AICanStart reports whether the assistant could run on this machine.
	AICanStart bool
	// AIOnDemand means the machine can host it but it was not started unasked.
	AIOnDemand bool
	// AIModel reports whether a model exists on the drive at all.
	AIModel bool
	// AIReason explains an unavailable or deferred assistant.
	AIReason string
	// Warnings are shown in the dashboard and printed at start.
	Warnings []string
}

// Discover finds every model directory containing a loadable .gguf, pairing each
// with a vision projector when one is present.
//
// Layout expected (as built by `survival-drive build`):
//
//	models/<id>/<name>.gguf
//	models/<id>/mmproj-*.gguf
func Discover(root string) ([]Model, error) {
	modelsDir := filepath.Join(root, "models")
	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		return nil, fmt.Errorf("no models directory under %s: %w", root, err)
	}
	var models []Model
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(modelsDir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		m := Model{ID: e.Name()}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(strings.ToLower(f.Name()), ".gguf") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if isProjector(f.Name()) {
				if m.MMProj == "" {
					m.MMProj = filepath.Join(dir, f.Name())
				}
				continue
			}
			// Prefer the largest non-projector .gguf as the model file.
			if info.Size() > m.Size {
				m.Path = filepath.Join(dir, f.Name())
				m.Size = info.Size()
			}
		}
		if m.Path != "" {
			models = append(models, m)
		}
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("no .gguf models found under %s", modelsDir)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Size < models[j].Size })
	return models, nil
}

// isProjector reports whether a gguf is a vision/audio projector rather than a
// model. Both `mmproj-F16.gguf` and `mmproj-gemma-4-E4B-it-Q8_0.gguf` occur.
func isProjector(name string) bool {
	base := strings.ToLower(path.Base(name))
	return strings.HasPrefix(base, "mmproj")
}

// Memory the launcher must leave alone before it will load a model.
const (
	// The library is not free: measured ~919 MB resident with 25 archives open.
	// Taking that much RAM away from the machine while the model loads is what
	// turns "the AI started" into "the encyclopedia died".
	libraryReserve = 1400 << 20
	// Headroom for Windows itself, the browser tabs and file cache.
	systemReserve = 1500 << 20
	// autoAIThreshold is the machine size at which the assistant may start by
	// itself. Below it the library is the product and the AI is on request: a
	// 4.6 GiB model plus archive indexes plus browser tabs on a 16 GB laptop does
	// not crash, it thrashes - pages stop loading and the drive looks broken.
	autoAIThreshold = 24 << 30
)

// AIStartMode decides whether the assistant may start unasked.
//
// auto=true means the launcher starts it; otherwise reason explains why it is
// waiting for an explicit request. Manual start is the default: the library is
// the part that must always work, and on a USB stick the model load competes
// with the archives for the same device. allowAuto is the explicit opt-in.
func AIStartMode(allowAuto bool, totalRAM, availRAM, smallestNeed int64) (auto bool, reason string) {
	if smallestNeed <= 0 {
		return false, "no model on this drive"
	}
	budget := ModelBudget(availRAM)
	if budget < smallestNeed {
		return false, fmt.Sprintf(
			"only %.1f GiB of memory is free and the smallest model needs %.1f GiB",
			float64(availRAM)/(1<<30), float64(smallestNeed)/(1<<30))
	}
	if !allowAuto {
		return false, "the assistant is started on request so the library always loads first"
	}
	if totalRAM < autoAIThreshold {
		return false, fmt.Sprintf(
			"this machine has %.1f GiB of RAM, so the assistant is started on request to keep the library fast",
			float64(totalRAM)/(1<<30))
	}
	return true, ""
}

// ModelBudget is how much memory a model may use, given what is actually free.
func ModelBudget(available int64) int64 {
	b := available - libraryReserve - systemReserve
	if b < 0 {
		return 0
	}
	return b
}

// SelectModel picks the largest model that plausibly fits in the budget, else the
// smallest. GGUF weights are memory-mapped, but the KV cache and compute buffers
// are real allocations, so the estimate is deliberately conservative.
func SelectModel(models []Model, budgetBytes int64) (Model, error) {
	if len(models) == 0 {
		return Model{}, fmt.Errorf("no models available")
	}
	const overhead = 1 << 30 // runtime + context overhead
	for i := len(models) - 1; i >= 0; i-- {
		need := int64(float64(models[i].Size)*1.25) + overhead
		if need <= budgetBytes {
			return models[i], nil
		}
	}
	return Model{}, fmt.Errorf("no model fits in %d MiB of free memory", budgetBytes>>20)
}

// SmallestModelNeed is what the smallest model on the drive would require.
func SmallestModelNeed(models []Model) int64 {
	if len(models) == 0 {
		return 0
	}
	const overhead = 1 << 30
	return int64(float64(models[0].Size)*1.25) + overhead
}

// ContextFor sizes the context window from available RAM: bigger machines get
// more working memory for conversation, small ones stay usable.
func ContextFor(ramBytes int64) int {
	switch {
	case ramBytes >= 24<<30:
		return 16384
	case ramBytes >= 12<<30:
		return 8192
	default:
		return 4096
	}
}

// DiscoverZIMs lists the archives to serve.
func DiscoverZIMs(root string) ([]string, error) {
	dir := filepath.Join(root, "archives")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("no archives directory under %s: %w", root, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".zim") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Backend picks the llama.cpp build to try first, with its fallback.
//
// The Vulkan build uses any GPU (Intel/AMD/NVIDIA) and falls back to CPU inside
// llama.cpp when no device is found, but it can also fail to start at all on a
// machine with a broken driver - hence the explicit CPU fallback.
func Backend(root string) (primary, fallback, primaryExe, fallbackExe string) {
	vulkanDir := filepath.Join(root, "bin", "llama-vulkan")
	cpuDir := filepath.Join(root, "bin", "llama-cpu")
	vulkanExe := filepath.Join(vulkanDir, exeName("llama-server"))
	cpuExe := filepath.Join(cpuDir, exeName("llama-server"))
	if fileExists(vulkanExe) {
		primary, primaryExe = vulkanDir, vulkanExe
	}
	if fileExists(cpuExe) {
		if primary == "" {
			primary, primaryExe = cpuDir, cpuExe
		} else {
			fallback, fallbackExe = cpuDir, cpuExe
		}
	}
	return primary, fallback, primaryExe, fallbackExe
}

// ServerArgs builds the llama-server command line. Only flags that exist in
// every recent llama.cpp build are used: an unknown flag is a failed start.
func ServerArgs(m Model, context, port int) []string {
	args := []string{
		"-m", m.Path,
		"-c", fmt.Sprint(context),
		"--host", "127.0.0.1",
		"--port", fmt.Sprint(port),
	}
	if m.MMProj != "" {
		args = append(args, "--mmproj", m.MMProj)
	}
	return args
}

// KiwixArgs builds the kiwix-serve command line. It takes explicit paths; a
// wildcard is not expanded by the program.
func KiwixArgs(zims []string, port int) []string {
	return append([]string{"--port", fmt.Sprint(port)}, zims...)
}

// BookName finds a served archive by prefix and returns the name kiwix-serve
// uses in URLs (the file name without ".zim").
func BookName(zims []string, prefixes ...string) string {
	for _, z := range zims {
		base := filepath.Base(z)
		lower := strings.ToLower(base)
		for _, p := range prefixes {
			if strings.HasPrefix(lower, p) {
				return strings.TrimSuffix(base, filepath.Ext(base))
			}
		}
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
