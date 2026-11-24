package fetch

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
)

// state tracks which pieces of an in-progress file are already on disk, so a
// reboot or Ctrl-C costs at most a few pieces of re-download.
type state struct {
	Version  int    `json:"version"`
	Size     int64  `json:"size"`
	PieceLen int64  `json:"piece_len"`
	Done     []int  `json:"done"`
	SHA256   string `json:"sha256,omitempty"`

	path string
	mu   sync.Mutex
	set  map[int]bool
	// pending counts pieces written since the last flush.
	pending int
}

const stateVersion = 1

// flushEvery bounds how many pieces may be lost to an unclean shutdown.
const flushEvery = 32

func loadState(path string, size, pieceLen int64) *state {
	s := &state{
		Version:  stateVersion,
		Size:     size,
		PieceLen: pieceLen,
		path:     path,
		set:      make(map[int]bool),
	}
	b, err := os.ReadFile(path)
	if err == nil {
		var disk state
		if json.Unmarshal(b, &disk) == nil &&
			disk.Version == stateVersion &&
			disk.Size == size &&
			disk.PieceLen == pieceLen {
			for _, i := range disk.Done {
				s.set[i] = true
			}
			s.Done = disk.Done
		}
	}
	return s
}

// Have reports whether piece i is already complete.
func (s *state) Have(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set[i]
}

// Mark records a piece as complete, flushing to disk periodically.
func (s *state) Mark(i int) {
	s.mu.Lock()
	if s.set[i] {
		s.mu.Unlock()
		return
	}
	s.set[i] = true
	s.Done = append(s.Done, i)
	s.pending++
	flush := s.pending >= flushEvery
	if flush {
		s.pending = 0
	}
	s.mu.Unlock()
	if flush {
		_ = s.Flush()
	}
}

// Count returns how many pieces are recorded complete.
func (s *state) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.set)
}

// Flush writes the state atomically.
func (s *state) Flush() error {
	s.mu.Lock()
	done := make([]int, len(s.Done))
	copy(done, s.Done)
	s.mu.Unlock()
	sort.Ints(done)

	payload := struct {
		Version  int    `json:"version"`
		Size     int64  `json:"size"`
		PieceLen int64  `json:"piece_len"`
		Done     []int  `json:"done"`
		SHA256   string `json:"sha256,omitempty"`
	}{stateVersion, s.Size, s.PieceLen, done, s.SHA256}

	b, err := json.Marshal(&payload)
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write state: %w", err)
	}
	return os.Rename(tmp, s.path)
}

// Remove deletes the state file.
func (s *state) Remove() {
	_ = os.Remove(s.path)
}
