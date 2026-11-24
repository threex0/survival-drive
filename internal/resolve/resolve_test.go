package resolve_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"survivaldrive/internal/manifest"
	"survivaldrive/internal/resolve"
)

// fixtures serves the frozen testdata plus synthetic entries for anything the
// tests do not care about, so no test ever touches the network.
func fixtures(t *testing.T) *httptest.Server {
	t.Helper()
	td := "testdata"

	mux := http.NewServeMux()
	mux.HandleFunc("/catalog/v2/entries", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		switch {
		case name == "wikipedia_en_all":
			http.ServeFile(w, r, filepath.Join(td, "kiwix_entries_wikipedia.xml"))
		case name == "ted_mul_health":
			http.ServeFile(w, r, filepath.Join(td, "kiwix_entries_ted.xml"))
		case name == "" || strings.HasPrefix(name, "wikihow"):
			http.ServeFile(w, r, filepath.Join(td, "kiwix_entries_empty.xml"))
		default:
			writeSyntheticFeed(w, name)
		}
	})
	mux.HandleFunc("/zim/", func(w http.ResponseWriter, r *http.Request) {
		base := filepath.Base(r.URL.Path)
		candidate := filepath.Join(td, base)
		if _, err := os.Stat(candidate); err == nil {
			http.ServeFile(w, r, candidate)
			return
		}
		writeSyntheticMetalink(w, strings.TrimSuffix(base, ".meta4"))
	})
	mux.HandleFunc("/api/models/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/tree/main") {
			http.ServeFile(w, r, filepath.Join(td, "hf_tree_gemma_e4b.json"))
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(r.URL.Path, "/stable/") {
			http.ServeFile(w, r, filepath.Join(td, "github_releases_stable.json"))
			return
		}
		http.ServeFile(w, r, filepath.Join(td, "github_releases_llama.json"))
	})
	mux.HandleFunc("/files/tools.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2048")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(make([]byte, 2048))
		}
	})
	mux.HandleFunc("/files/tools.zip.md5", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "d41d8cd98f00b204e9800998ecf8427e  tools.zip\n")
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writeSyntheticFeed(w http.ResponseWriter, name string) {
	type link struct {
		Rel    string `xml:"rel,attr"`
		Type   string `xml:"type,attr"`
		Href   string `xml:"href,attr"`
		Length int64  `xml:"length,attr"`
	}
	type entry struct {
		ID      string `xml:"id"`
		Title   string `xml:"title"`
		Name    string `xml:"name"`
		Flavour string `xml:"flavour"`
		Cat     string `xml:"category"`
		Link    link   `xml:"link"`
	}
	feed := struct {
		XMLName xml.Name `xml:"feed"`
		Xmlns   string   `xml:"xmlns,attr"`
		ID      string   `xml:"id"`
		Title   string   `xml:"title"`
		Entries []entry  `xml:"entry"`
	}{
		Xmlns: "http://www.w3.org/2005/Atom",
		ID:    "synthetic-" + name,
		Title: "synthetic feed for " + name,
	}
	e := entry{ID: "urn:uuid:synthetic-" + name, Title: name, Name: name, Cat: "synthetic"}
	e.Link = link{
		Rel:    "http://opds-spec.org/acquisition/open-access",
		Type:   "application/x-zim",
		Href:   "/zim/synthetic/" + name + ".zim.meta4",
		Length: syntheticSize(name),
	}
	feed.Entries = append(feed.Entries, e)

	w.Header().Set("Content-Type", "application/atom+xml")
	_ = xml.NewEncoder(w).Encode(feed)
}

func writeSyntheticMetalink(w http.ResponseWriter, fileName string) {
	size := syntheticSize(fileName)
	feed := struct {
		XMLName xml.Name `xml:"metalink"`
		Xmlns   string   `xml:"xmlns,attr"`
		File    struct {
			Name   string `xml:"name,attr"`
			Size   int64  `xml:"size"`
			Hashes []struct {
				Type  string `xml:"type,attr"`
				Value string `xml:",chardata"`
			} `xml:"hash"`
			URLs []string `xml:"url"`
		} `xml:"file"`
	}{Xmlns: "urn:ietf:params:xml:ns:metalink"}
	feed.File.Name = fileName
	feed.File.Size = size
	feed.File.Hashes = append(feed.File.Hashes, struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	}{Type: "sha-256", Value: syntheticSHA(fileName)})
	feed.File.URLs = append(feed.File.URLs, "https://kiwix.org", "https://mirror.example/zim/"+fileName)

	w.Header().Set("Content-Type", "application/metalink4+xml")
	_ = xml.NewEncoder(w).Encode(feed)
}

// syntheticSize gives a stable pseudo-size in [8 MiB, 2 GiB) per name.
func syntheticSize(name string) int64 {
	var h uint32 = 2166136261
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	return int64(8<<20) + int64(h%(1900<<20))
}

func syntheticSHA(name string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(name); i++ {
		h ^= uint32(name[i])
		h *= 16777619
	}
	s := fmt.Sprintf("%08x", h)
	return strings.Repeat(s, 8)
}

func opts(t *testing.T) resolve.Options {
	srv := fixtures(t)
	o := resolve.DefaultOptions()
	o.KiwixBase = srv.URL
	o.HuggingBase = srv.URL
	o.GitHubBase = srv.URL
	return o
}

func TestKiwixFlavourPreference(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "wiki",
		Kind: manifest.KindZIM,
		Dest: "archives/",
		Resolver: manifest.Resolver{
			Type:     manifest.ResolveKiwix,
			Name:     "wikipedia_en_all",
			Flavours: []string{"nopic", "mini"},
		},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("want 2 candidates (nopic, mini), got %d", len(cands))
	}
	if cands[0].Label != "nopic" || cands[1].Label != "mini" {
		t.Fatalf("candidate order = [%s %s], want [nopic mini]", cands[0].Label, cands[1].Label)
	}
	f := cands[0].Files[0]
	if f.Dest != "archives/wikipedia_en_all_nopic_2026-06.zim" {
		t.Errorf("dest = %q", f.Dest)
	}
	if f.SizeBytes != 52685168640 {
		t.Errorf("size = %d, want 52685168640", f.SizeBytes)
	}
	if len(f.SHA256) != 64 {
		t.Errorf("sha256 = %q, want 64 hex chars", f.SHA256)
	}
	if !strings.HasSuffix(f.Descriptor, "wikipedia_en_all_nopic_2026-06.zim.meta4") {
		t.Errorf("descriptor = %q, want the metalink pinned", f.Descriptor)
	}
	if !strings.HasSuffix(f.URL, "wikipedia_en_all_nopic_2026-06.zim") {
		t.Errorf("url = %q, want the canonical .zim path", f.URL)
	}
	if strings.Contains(f.URL, "meta4") {
		t.Errorf("url = %q must not be the descriptor", f.URL)
	}
	if f.Pieces == nil || len(f.Pieces.SHA1) != 1 || f.Pieces.Size != 4194304 {
		t.Errorf("pieces = %+v, want one 4 MiB chunk hash", f.Pieces)
	}
}

func TestKiwixPrefersExactName(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:       "ted",
		Kind:     manifest.KindZIM,
		Dest:     "archives/",
		Resolver: manifest.Resolver{Type: manifest.ResolveKiwix, Name: "ted_mul_health"},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want 1 candidate (exact name match), got %d: %+v", len(cands), cands)
	}
	if got := cands[0].Files[0].Name; got != "ted_mul_health_2026-09.zim" {
		t.Errorf("file = %q, want the exact-name archive", got)
	}
}

func TestKiwixMissingContentFailsLoudly(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:       "wikihow",
		Kind:     manifest.KindZIM,
		Dest:     "archives/",
		Resolver: manifest.Resolver{Type: manifest.ResolveKiwix, Name: "wikihow_en_maxi"},
	}
	_, err := reg.Resolve(context.Background(), item)
	if err == nil {
		t.Fatal("expected an error for withdrawn content")
	}
	if !strings.Contains(err.Error(), "wikihow_en_maxi") {
		t.Errorf("error should name the slug, got: %v", err)
	}
}

func TestHuggingFacePinsLFSObjects(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "model",
		Kind: manifest.KindGGUF,
		Dest: "models/",
		Resolver: manifest.Resolver{
			Type:  manifest.ResolveHugging,
			Repo:  "unsloth/gemma-4-E4B-it-GGUF",
			Files: []string{"gemma-4-E4B-it-Q4_K_M.gguf", "mmproj-F16.gguf"},
		},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	files := cands[0].Files
	if len(files) != 2 {
		t.Fatalf("want model + mmproj, got %d files", len(files))
	}
	if files[0].Name != "gemma-4-E4B-it-Q4_K_M.gguf" || files[1].Name != "mmproj-F16.gguf" {
		t.Errorf("order = %q, %q (pattern order must be preserved)", files[0].Name, files[1].Name)
	}
	if len(files[0].SHA256) != 64 {
		t.Errorf("model sha256 = %q, want 64 hex chars from lfs.oid", files[0].SHA256)
	}
	if files[0].SizeBytes != 4984194560 {
		t.Errorf("size = %d, want lfs size 4984194560", files[0].SizeBytes)
	}
	wantURL := "/unsloth/gemma-4-E4B-it-GGUF/resolve/main/gemma-4-E4B-it-Q4_K_M.gguf"
	if !strings.HasSuffix(files[0].URL, wantURL) {
		t.Errorf("url = %q, want suffix %q", files[0].URL, wantURL)
	}
}

func TestHuggingFaceMissingPatternIsAnError(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "model",
		Kind: manifest.KindGGUF,
		Dest: "models/",
		Resolver: manifest.Resolver{
			Type:  manifest.ResolveHugging,
			Repo:  "unsloth/gemma-4-E4B-it-GGUF",
			Files: []string{"Q4_K_XL.gguf"},
		},
	}
	_, err := reg.Resolve(context.Background(), item)
	if err == nil || !strings.Contains(err.Error(), "Q4_K_XL.gguf") {
		t.Fatalf("want error naming the missing pattern, got: %v", err)
	}
}

func TestGitHubSkipsReleaseWithoutMatchingAssets(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "llama",
		Kind: manifest.KindZip,
		Dest: "bin/",
		Resolver: manifest.Resolver{
			Type:   manifest.ResolveGitHub,
			Repo:   "ggml-org/llama.cpp",
			Assets: []string{"bin-win-vulkan-x64\\.zip$", "bin-win-cpu-x64\\.zip$"},
		},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cands[0].Label != "b12503" {
		t.Errorf("picked release %q, want b12503 (v1.0.0 has no win assets)", cands[0].Label)
	}
	if cands[0].Note != "prerelease (nightly) build" {
		t.Errorf("note = %q, want the nightly flagged", cands[0].Note)
	}
	files := cands[0].Files
	if len(files) != 2 {
		t.Fatalf("want 2 assets, got %d", len(files))
	}
	if !strings.Contains(files[0].Name, "vulkan") {
		t.Errorf("first asset = %q, want vulkan (pattern order)", files[0].Name)
	}
	if len(files[0].SHA256) != 64 {
		t.Errorf("sha256 = %q, want digest from the API", files[0].SHA256)
	}
	if files[0].Dest != "bin/llama-b12503-bin-win-vulkan-x64.zip" {
		t.Errorf("dest = %q", files[0].Dest)
	}
}

func TestGitHubPrefersStableOverNightly(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "llama",
		Kind: manifest.KindZip,
		Dest: "bin/",
		Resolver: manifest.Resolver{
			Type:   manifest.ResolveGitHub,
			Repo:   "test/stable",
			Assets: []string{"bin-win-vulkan-x64\\.zip$"},
		},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cands[0].Label != "v1.2.0" {
		t.Errorf("picked %q, want the stable v1.2.0 over the newer prerelease", cands[0].Label)
	}
	if cands[0].Note != "" {
		t.Errorf("note = %q, stable release should not be flagged", cands[0].Note)
	}
}

func TestGitHubPinnedTagMissingFails(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "llama",
		Kind: manifest.KindZip,
		Dest: "bin/",
		Resolver: manifest.Resolver{
			Type:   manifest.ResolveGitHub,
			Repo:   "ggml-org/llama.cpp",
			Tag:    "v1.0.0",
			Assets: []string{"bin-win-vulkan-x64\\.zip$"},
		},
	}
	_, err := reg.Resolve(context.Background(), item)
	if err == nil {
		t.Fatal("want an error: v1.0.0 has no matching assets")
	}
}

func TestStaticUsesMD5Sidecar(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	base := lastServerURL(t, reg)
	item := manifest.Item{
		ID:   "tools",
		Kind: manifest.KindZip,
		Dest: "bin/tools.zip",
		Resolver: manifest.Resolver{
			Type:   manifest.ResolveStatic,
			URL:    base + "/files/tools.zip",
			MD5URL: base + "/files/tools.zip.md5",
		},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	f := cands[0].Files[0]
	if f.SizeBytes != 2048 {
		t.Errorf("size = %d, want 2048 from Content-Length", f.SizeBytes)
	}
	if f.MD5 != "d41d8cd98f00b204e9800998ecf8427e" {
		t.Errorf("md5 = %q, want the sidecar digest", f.MD5)
	}
	if f.Dest != "bin/tools.zip" {
		t.Errorf("dest = %q, want explicit file path preserved", f.Dest)
	}
	if len(cands[0].Unpinned()) != 1 {
		t.Errorf("static item should report as unpinned until first fetch")
	}
}

// lastServerURL is a helper: the registry has no public accessor, so tests that
// need the fixture base URL build their own server.
func lastServerURL(t *testing.T, _ *resolve.Registry) string {
	t.Helper()
	srv := fixtures(t)
	return srv.URL
}

func TestEstimateResolver(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "pdfs",
		Kind: manifest.KindPDF,
		Dest: "library_pdfs/",
		Note: "M1 pending",
		Resolver: manifest.Resolver{
			Type:          manifest.ResolveEstimate,
			EstimateBytes: 1 << 30,
		},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	c := cands[0]
	if !c.Estimated || c.Bytes() != 1<<30 {
		t.Errorf("candidate = %+v, want estimated at 1 GiB", c)
	}
	if c.Note != "M1 pending" {
		t.Errorf("note = %q, want the item note carried through", c.Note)
	}
}

func TestPMTilesEstimate(t *testing.T) {
	reg := resolve.NewRegistry(opts(t))
	item := manifest.Item{
		ID:   "maps",
		Kind: manifest.KindPMTiles,
		Dest: "maps/basemap.pmtiles",
		Resolver: manifest.Resolver{
			Type:          manifest.ResolvePMTiles,
			BBox:          "north-america",
			PlanetDate:    "20261118",
			EstimateBytes: 6 << 30,
		},
	}
	cands, err := reg.Resolve(context.Background(), item)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	c := cands[0]
	if !c.Estimated || c.Bytes() != 6<<30 {
		t.Fatalf("candidate = %+v", c)
	}
	if c.Files[0].Dest != "maps/basemap.pmtiles" {
		t.Errorf("dest = %q", c.Files[0].Dest)
	}
	if !strings.Contains(c.Note, "north-america") || !strings.Contains(c.Note, "20261118") {
		t.Errorf("note = %q, want bbox and planet date recorded", c.Note)
	}
}
