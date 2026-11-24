// Package budget decides which resolved candidates actually go on the drive.
//
// Items are considered in priority order and each one takes the best candidate
// that still fits inside the fill target. That gives a real downgrade path: if
// Wikipedia maxi no longer fits, nopic then mini are tried before the item is
// dropped.
package budget

import (
	"fmt"
	"sort"

	"survivaldrive/internal/manifest"
	"survivaldrive/internal/resolve"
)

// Choice is an item plus the candidate selected for it.
type Choice struct {
	Item      manifest.Item
	Candidate resolve.Candidate
}

// Bytes is the selected candidate's size.
func (c Choice) Bytes() int64 { return c.Candidate.Bytes() }

// Skip records an item that did not fit, and why.
type Skip struct {
	Item   manifest.Item
	Reason string
}

// Plan is the outcome of fitting a catalogue to a drive.
type Plan struct {
	CapacityBytes int64
	LimitBytes    int64
	Choices       []Choice
	Skipped       []Skip

	TotalBytes     int64
	ResolvedBytes  int64
	EstimatedBytes int64
}

// Build fits items into capacity*fillTarget. candidates must contain at least
// one entry per item and is expected to be in preference order.
func Build(items []manifest.Item, candidates map[string][]resolve.Candidate, capacity int64, fill float64) (Plan, error) {
	if capacity <= 0 {
		return Plan{}, fmt.Errorf("budget: capacity must be positive")
	}
	if fill <= 0 || fill > 1 {
		return Plan{}, fmt.Errorf("budget: fill target %v must be in (0,1]", fill)
	}
	limit := int64(float64(capacity) * fill)

	ordered := make([]manifest.Item, len(items))
	copy(ordered, items)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority > ordered[j].Priority
		}
		return ordered[i].ID < ordered[j].ID
	})

	p := Plan{CapacityBytes: capacity, LimitBytes: limit}
	for _, it := range ordered {
		cands, ok := candidates[it.ID]
		if !ok || len(cands) == 0 {
			return Plan{}, fmt.Errorf("budget: item %s has no candidates", it.ID)
		}

		var chosen *resolve.Candidate
		var smallest int64 = -1
		for i := range cands {
			size := cands[i].Bytes()
			if smallest < 0 || size < smallest {
				smallest = size
			}
			if p.TotalBytes+size <= limit {
				chosen = &cands[i]
				break
			}
		}
		if chosen == nil {
			p.Skipped = append(p.Skipped, Skip{
				Item: it,
				Reason: fmt.Sprintf("smallest candidate needs %s, only %s left",
					Human(smallest), Human(limit-p.TotalBytes)),
			})
			continue
		}

		c := Choice{Item: it, Candidate: *chosen}
		p.Choices = append(p.Choices, c)
		size := c.Bytes()
		p.TotalBytes += size
		if c.Candidate.Estimated {
			p.EstimatedBytes += size
		} else {
			p.ResolvedBytes += size
		}
	}
	return p, nil
}

// Headroom is the unused space inside the fill target.
func (p Plan) Headroom() int64 { return p.LimitBytes - p.TotalBytes }

// Free is the unused space on the whole drive.
func (p Plan) Free() int64 { return p.CapacityBytes - p.TotalBytes }

// FillFraction is the fraction of the raw capacity used.
func (p Plan) FillFraction() float64 {
	if p.CapacityBytes == 0 {
		return 0
	}
	return float64(p.TotalBytes) / float64(p.CapacityBytes)
}

// Human renders a byte count the way the planning docs do: GiB.
func Human(n int64) string {
	const gib = 1 << 30
	if n < gib {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/gib)
}
