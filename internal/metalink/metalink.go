// Package metalink parses the Metalink 4 documents that Kiwix publishes next to
// every .zim file.
//
// The document is the source of truth for a download: exact size, sha256, md5,
// per-piece SHA-1 hashes and the current mirror list. Because MirrorBrain
// rotates mirrors, the fetcher re-reads the descriptor at download time rather
// than trusting a snapshot.
package metalink

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// MaxBytes caps a descriptor fetch. The largest real metalink (Wikipedia maxi)
// is ~1.8 MB because it carries ~30k piece hashes.
const MaxBytes = 32 << 20

// UserAgent is sent on every request.
const UserAgent = "survival-drive/0.1 (+M1 fetcher)"

// Piece is the chunk table of a file.
type Piece struct {
	Length int64
	Hashes []string // lowercase SHA-1 hex, one per Position() chunk
}

// File is one payload described by a metalink.
type File struct {
	Name   string
	Size   int64
	SHA256 string
	MD5    string
	Pieces *Piece
	URLs   []string // publisher homepage already filtered out
}

// Position returns the byte offset of chunk i.
func (p *Piece) Position(i int) int64 { return int64(i) * p.Length }

// Len returns the byte length of chunk i (the last chunk may be short).
func (p *Piece) Len(i int, fileSize int64) int64 {
	start := p.Position(i)
	if start >= fileSize {
		return 0
	}
	end := start + p.Length
	if end > fileSize {
		end = fileSize
	}
	return end - start
}

// Count returns how many chunks the file is divided into.
func (p *Piece) Count(fileSize int64) int {
	if p == nil || p.Length <= 0 {
		return 0
	}
	n := int((fileSize + p.Length - 1) / p.Length)
	if n > len(p.Hashes) {
		return len(p.Hashes)
	}
	return n
}

type document struct {
	Files []documentFile `xml:"file"`
}

type documentFile struct {
	Name   string `xml:"name,attr"`
	Size   int64  `xml:"size"`
	Hashes []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"hash"`
	Pieces *struct {
		Length int64    `xml:"length,attr"`
		Hashes []string `xml:"hash"`
	} `xml:"pieces"`
	URLs []string `xml:"url"`
}

// Parse decodes a metalink document.
func Parse(b []byte) ([]File, error) {
	var doc document
	if err := xml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse metalink: %w", err)
	}
	out := make([]File, 0, len(doc.Files))
	for _, df := range doc.Files {
		f := File{Name: df.Name, Size: df.Size}
		for _, h := range df.Hashes {
			switch strings.ToLower(strings.TrimSpace(h.Type)) {
			case "sha-256", "sha256":
				f.SHA256 = strings.ToLower(strings.TrimSpace(h.Value))
			case "md5":
				f.MD5 = strings.ToLower(strings.TrimSpace(h.Value))
			}
		}
		if df.Pieces != nil && df.Pieces.Length > 0 && len(df.Pieces.Hashes) > 0 {
			p := &Piece{Length: df.Pieces.Length}
			for _, h := range df.Pieces.Hashes {
				p.Hashes = append(p.Hashes, strings.ToLower(strings.TrimSpace(h)))
			}
			f.Pieces = p
		}
		for _, u := range df.URLs {
			// The document also lists the publisher homepage; only keep URLs
			// that point at this exact file.
			if strings.HasSuffix(strings.TrimSuffix(u, "/"), "/"+f.Name) {
				f.URLs = append(f.URLs, u)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// Fetch downloads and parses a descriptor that must describe exactly one file.
func Fetch(ctx context.Context, c *http.Client, descriptorURL string) (File, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, descriptorURL, nil)
	if err != nil {
		return File{}, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := c.Do(req)
	if err != nil {
		return File{}, fmt.Errorf("get descriptor %s: %w", descriptorURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return File{}, fmt.Errorf("get descriptor %s: unexpected status %s", descriptorURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes))
	if err != nil {
		return File{}, fmt.Errorf("read descriptor %s: %w", descriptorURL, err)
	}
	files, err := Parse(body)
	if err != nil {
		return File{}, err
	}
	if len(files) != 1 {
		return File{}, fmt.Errorf("descriptor %s described %d files, expected 1", descriptorURL, len(files))
	}
	if files[0].Size <= 0 {
		return File{}, fmt.Errorf("descriptor %s reported size %d", descriptorURL, files[0].Size)
	}
	if len(files[0].URLs) == 0 {
		return File{}, fmt.Errorf("descriptor %s listed no mirror for %s", descriptorURL, files[0].Name)
	}
	return files[0], nil
}
