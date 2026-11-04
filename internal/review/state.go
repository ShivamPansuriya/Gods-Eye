package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// State is what a review remembers between rounds, so the next round reviews
// only what changed, skips what was already reported, and notices fixes.
type State struct {
	Key      string    `json:"key"`
	Round    int       `json:"round"`
	HeadSHA  string    `json:"headSha"`
	Findings []Finding `json:"findings"`
	SavedAt  time.Time `json:"savedAt"`
}

var keyClean = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// StatePath is where the state for a key lives.
func StatePath(dir, key string) string {
	return filepath.Join(dir, "state", keyClean.ReplaceAllString(key, "_")+".json")
}

// LoadState reads a saved state (nil if none).
func LoadState(dir, key string) *State {
	b, err := os.ReadFile(StatePath(dir, key))
	if err != nil {
		return nil
	}
	var s State
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// SaveState writes the state after a review.
func SaveState(dir, key, head string, r *Review) error {
	round := 1
	if r.Pipeline != nil {
		round = r.Pipeline.Round
	}
	s := State{Key: key, Round: round, HeadSHA: head, SavedAt: time.Now().UTC()}
	for _, f := range r.Findings {
		if f.Status == "confirmed" {
			s.Findings = append(s.Findings, f)
		}
	}
	p := StatePath(dir, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(p, b, 0o644)
}
