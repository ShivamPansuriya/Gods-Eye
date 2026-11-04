package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

// Price is per 1M tokens.
type Price struct{ In, Out float64 }

// Prices for current models. Cache writes (5-minute TTL) cost 1.25x input; cache
// reads cost 0.05x input on Opus 5.5 / Sonnet 5.5 / Haiku 5.5 and 0.025x on Fable 5.1.
var Prices = map[string]Price{
	"claude-opus-5-5":   {4, 20},
	"claude-sonnet-5-5": {2, 10},
	"claude-haiku-5-5":  {0.10, 0.50},
	"claude-fable-5-1":  {10, 50},
}

func readFactor(model string) float64 {
	if strings.HasPrefix(model, "claude-fable") || strings.HasPrefix(model, "claude-mythos") {
		return 0.025
	}
	return 0.05
}

// StageUsage is token use for one pipeline stage.
type StageUsage struct {
	Calls      int     `json:"calls"`
	Input      int64   `json:"input"`      // uncached input
	CacheWrite int64   `json:"cacheWrite"` // tokens written to cache
	CacheRead  int64   `json:"cacheRead"`  // tokens read from cache
	Output     int64   `json:"output"`
	CostUSD    float64 `json:"costUsd"`
	Seconds    float64 `json:"seconds"` // summed call time (calls overlap)
}

// Meter sums usage across concurrent agents.
type Meter struct {
	mu     sync.Mutex
	Model  string
	Stages map[string]*StageUsage
}

// NewMeter returns an empty meter.
func NewMeter() *Meter { return &Meter{Stages: map[string]*StageUsage{}} }

// Add records one API response.
func (m *Meter) Add(stage, model string, u anthropic.BetaUsage, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Model == "" {
		m.Model = model
	}
	s := m.Stages[stage]
	if s == nil {
		s = &StageUsage{}
		m.Stages[stage] = s
	}
	s.Calls++
	s.Input += u.InputTokens
	s.CacheWrite += u.CacheCreationInputTokens
	s.CacheRead += u.CacheReadInputTokens
	s.Output += u.OutputTokens
	s.Seconds += d.Seconds()
	if p, ok := Prices[model]; ok {
		s.CostUSD += float64(u.InputTokens)/1e6*p.In +
			float64(u.CacheCreationInputTokens)/1e6*p.In*1.25 +
			float64(u.CacheReadInputTokens)/1e6*p.In*readFactor(model) +
			float64(u.OutputTokens)/1e6*p.Out
	}
}

// Total sums all stages.
func (m *Meter) Total() StageUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t StageUsage
	for _, s := range m.Stages {
		t.Calls += s.Calls
		t.Input += s.Input
		t.CacheWrite += s.CacheWrite
		t.CacheRead += s.CacheRead
		t.Output += s.Output
		t.CostUSD += s.CostUSD
		t.Seconds += s.Seconds
	}
	return t
}

// Snapshot copies the per-stage usage.
func (m *Meter) Snapshot() map[string]StageUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]StageUsage{}
	for k, v := range m.Stages {
		out[k] = *v
	}
	return out
}

// String is a one-line summary per stage.
func (m *Meter) String() string {
	snap := m.Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		s := snap[k]
		in := s.Input + s.CacheWrite + s.CacheRead
		hit := 0.0
		if in > 0 {
			hit = float64(s.CacheRead) / float64(in) * 100
		}
		fmt.Fprintf(&b, "  %-7s %3d calls · %7dk in (%2.0f%% cached) · %5dk out · $%.2f\n", k, s.Calls, in/1000, hit, s.Output/1000, s.CostUSD)
	}
	return b.String()
}
