package metalink_test

import (
	"strings"
	"testing"

	"survivaldrive/internal/metalink"
)

const sample = `<?xml version="1.0" encoding="UTF-8"?>
<metalink xmlns="urn:ietf:params:xml:ns:metalink">
  <publisher><name>Kiwix project</name><url>https://kiwix.org</url></publisher>
  <file name="wikipedia_en_all_maxi_2026-08.zim">
    <size>127418087648</size>
    <hash type="md5">ad1d4aeda3963a9a5dd6d9b73d6b0126</hash>
    <hash type="sha-256">34162D18B9F96F494EAC48A73A309C72E0BC95D85F76BE405D0F5DE795854AA6</hash>
    <pieces length="4194304" type="sha-1">
      <hash>b67cba440a5502b9fb4c816dc6c867a8feb76a49</hash>
      <hash>177c9f4ba6fa03cf4a1a6e9601a409b897870e6f</hash>
    </pieces>
    <url>https://kiwix.org</url>
    <url>https://dumps.wikimedia.org/kiwix/zim/wikipedia/wikipedia_en_all_maxi_2026-08.zim</url>
    <url>https://mirrors.dotsrc.org/kiwix/zim/wikipedia/wikipedia_en_all_maxi_2026-08.zim</url>
  </file>
</metalink>`

func TestParse(t *testing.T) {
	files, err := metalink.Parse([]byte(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	f := files[0]
	if f.Name != "wikipedia_en_all_maxi_2026-08.zim" {
		t.Errorf("name = %q", f.Name)
	}
	if f.Size != 127418087648 {
		t.Errorf("size = %d", f.Size)
	}
	if f.SHA256 != strings.ToLower(f.SHA256) || len(f.SHA256) != 64 {
		t.Errorf("sha256 = %q, want lowercase hex", f.SHA256)
	}
	if f.MD5 != "ad1d4aeda3963a9a5dd6d9b73d6b0126" {
		t.Errorf("md5 = %q", f.MD5)
	}
	// The publisher homepage must not be treated as a mirror.
	if len(f.URLs) != 2 {
		t.Fatalf("urls = %v, want 2 (homepage filtered)", f.URLs)
	}
	for _, u := range f.URLs {
		if strings.Contains(u, "://kiwix.org") {
			t.Errorf("publisher url leaked into mirrors: %s", u)
		}
	}
	if f.Pieces == nil || f.Pieces.Length != 4194304 || len(f.Pieces.Hashes) != 2 {
		t.Fatalf("pieces = %+v", f.Pieces)
	}
}

func TestPieceGeometry(t *testing.T) {
	p := &metalink.Piece{Length: 100}
	if got := p.Position(0); got != 0 {
		t.Errorf("position(0) = %d", got)
	}
	if got := p.Position(3); got != 300 {
		t.Errorf("position(3) = %d", got)
	}
	if got := p.Len(0, 250); got != 100 {
		t.Errorf("len(0) = %d", got)
	}
	if got := p.Len(2, 250); got != 50 {
		t.Errorf("last len = %d, want 50", got)
	}
	if got := p.Len(3, 250); got != 0 {
		t.Errorf("len beyond eof = %d, want 0", got)
	}
}

func TestPieceCountClampsToHashTable(t *testing.T) {
	// Two hashes but room for four chunks: trust the hashes, not the arithmetic.
	p := &metalink.Piece{Length: 100, Hashes: []string{"a", "b"}}
	if got := p.Count(400); got != 2 {
		t.Errorf("count = %d, want 2 (hash table is authoritative)", got)
	}
	if got := p.Count(150); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
	var nilPieces *metalink.Piece
	if got := nilPieces.Count(100); got != 0 {
		t.Errorf("nil piece table count = %d, want 0", got)
	}
}
