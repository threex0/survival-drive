package resolve

import (
	"context"
	"fmt"

	"survivaldrive/internal/manifest"
)

// Estimate records an item whose payload has not been sourced yet. It keeps the
// budget honest: the size is counted, the item is marked ESTIMATE, and it will
// block a strict build until a real resolver replaces it.
type Estimate struct{}

// Resolve implements Resolver.
func (e *Estimate) Resolve(ctx context.Context, item manifest.Item) ([]Candidate, error) {
	n := item.Resolver.EstimateBytes
	if n <= 0 {
		return nil, fmt.Errorf("estimate resolver needs estimate_bytes")
	}
	note := item.Note
	if note == "" {
		note = "size estimated; payload not sourced"
	}
	return []Candidate{{
		Label:         "estimate",
		Estimated:     true,
		BytesEstimate: n,
		Note:          note,
	}}, nil
}
