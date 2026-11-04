// Package review asks Claude for a structured review and cleans the result.
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/shared/constant"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
	"github.com/ShivamPansuriya/gods-eye/internal/source"
)

// Options controls the model call.
type Options struct {
	Model  string // default claude-opus-5-5
	Effort string // low | medium | high | xhigh | max
}

// Usage is what the call cost.
type Usage struct {
	Model        string  `json:"model"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	CacheRead    int64   `json:"cacheRead"`
	CostUSD      float64 `json:"costUsd"`
}

// pricing per 1M tokens (input, output). Unknown models report cost 0.
var pricing = map[string][2]float64{
	"claude-opus-5-5":   {4, 20},
	"claude-sonnet-5-5": {2, 10},
	"claude-haiku-5-5":  {0.10, 0.50},
	"claude-fable-5-1":  {10, 50},
}

// MaxDiffChars guards against sending something huge by accident (~250k tokens).
const MaxDiffChars = 1_000_000

// Prompt returns the user prompt (for --dry-run).
func Prompt(in *source.Input, d *diff.Diff) string { return userPrompt(in, d.Annotated()) }

// Run calls Claude and returns a cleaned review.
func Run(ctx context.Context, client anthropic.Client, in *source.Input, d *diff.Diff, opt Options) (*Review, *Usage, error) {
	if opt.Model == "" {
		opt.Model = "claude-opus-5-5"
	}
	if opt.Effort == "" {
		opt.Effort = "high"
	}
	annotated := d.Annotated()
	if len(annotated) > MaxDiffChars {
		return nil, nil, fmt.Errorf("diff is %d chars (limit %d); split the PR or review a narrower range", len(annotated), MaxDiffChars)
	}

	params := anthropic.BetaMessageNewParams{
		Model:     opt.Model,
		MaxTokens: 64000,
		System: []anthropic.BetaTextBlockParam{{
			Text:         systemPrompt,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}},
		Messages: []anthropic.BetaMessageParam{
			anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userPrompt(in, annotated))),
		},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Effort: anthropic.BetaOutputConfigEffort(opt.Effort),
			Format: anthropic.BetaJSONOutputFormatParam{Schema: Schema()},
		},
		// If a safety classifier declines, the server retries on a fallback model
		// inside the same call instead of failing the review.
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}

	stream := client.Beta.Messages.NewStreaming(ctx, params)
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		if err := msg.Accumulate(stream.Current()); err != nil {
			return nil, nil, fmt.Errorf("stream: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, nil, fmt.Errorf("claude: %w", err)
	}

	switch msg.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return nil, nil, fmt.Errorf("model declined to review this diff (%s)", msg.StopDetails.Category)
	case anthropic.BetaStopReasonMaxTokens:
		return nil, nil, fmt.Errorf("review hit the output limit; try a smaller PR")
	}

	var text strings.Builder
	for _, b := range msg.Content {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	var r Review
	if err := json.Unmarshal([]byte(text.String()), &r); err != nil {
		return nil, nil, fmt.Errorf("parse review JSON: %w", err)
	}

	u := &Usage{
		Model:        string(msg.Model),
		InputTokens:  msg.Usage.InputTokens + msg.Usage.CacheCreationInputTokens + msg.Usage.CacheReadInputTokens,
		OutputTokens: msg.Usage.OutputTokens,
		CacheRead:    msg.Usage.CacheReadInputTokens,
	}
	if p, ok := pricing[u.Model]; ok {
		u.CostUSD = float64(u.InputTokens)/1e6*p[0] + float64(u.OutputTokens)/1e6*p[1]
	}
	return &r, u, nil
}

// Finalize enforces the rules the model can't be trusted with: every hunk is in
// exactly one unit, path rules raise tiers, ids are valid, text stays short.
func Finalize(r *Review, d *diff.Diff, rules []risk.Rule) {
	byID := d.HunkByID()
	seen := map[string]bool{}

	for i := range r.Units {
		u := &r.Units[i]
		if u.ID == "" {
			u.ID = fmt.Sprintf("U%d", i+1)
		}
		var hs []string
		var paths []string
		for _, h := range u.Hunks {
			hk, ok := byID[h]
			if !ok || seen[h] {
				continue
			}
			seen[h] = true
			hs = append(hs, h)
			paths = append(paths, hk.File)
		}
		u.Hunks = hs

		// Path rules can only raise the tier.
		if floor, why := risk.Floor(rules, paths); risk.Rank(floor) > risk.Rank(risk.Tier(u.Tier)) {
			u.Tier = string(floor)
			u.RiskReason = "Always needs review: " + why
			if floor == risk.Human && u.Risk == "low" {
				u.Risk = "medium"
			}
		}
		// A failing check always needs a person.
		for _, c := range u.Checks {
			if c.Status == "not_done" {
				u.Tier = string(risk.Human)
			}
		}

		u.Title = clampWords(u.Title, 10)
		u.Brief = clampWords(u.Brief, 45)
		u.RiskReason = clampWords(u.RiskReason, 16)
		for j := range u.Notes {
			u.Notes[j].Note = clampWords(u.Notes[j].Note, 26)
			if !validHunk(byID, u.Notes[j].Hunk) {
				u.Notes[j].Hunk, u.Notes[j].Line = "", 0
			}
		}
		for j := range u.Checks {
			u.Checks[j].Detail = clampWords(u.Checks[j].Detail, 32)
			if !validHunk(byID, u.Checks[j].Evidence.Hunk) {
				u.Checks[j].Evidence.Hunk, u.Checks[j].Evidence.Line = "", 0
			}
		}
		for j := range u.Blast {
			u.Blast[j].Detail = clampWords(u.Blast[j].Detail, 32)
		}
		for j := range u.Ask {
			u.Ask[j] = clampWords(u.Ask[j], 32)
		}
		fixSteps(byID, u.Flow.Before)
		fixSteps(byID, u.Flow.After)
	}

	// Any hunk the model forgot goes into a catch-all unit so nothing is hidden.
	var orphans []string
	for _, h := range d.Hunks() {
		if !seen[h.ID] {
			orphans = append(orphans, h.ID)
		}
	}
	if len(orphans) > 0 {
		r.Units = append(r.Units, Unit{
			ID: fmt.Sprintf("U%d", len(r.Units)+1), Title: "Other changes (not grouped)",
			Hunks: orphans, Tier: "agent", Risk: "low",
			RiskReason: "Not grouped by the reviewer; skim them",
			Brief:      "These hunks were not placed in any unit. Skim them yourself.",
		})
	}

	// Drop units that ended with no hunks.
	kept := r.Units[:0]
	for _, u := range r.Units {
		if len(u.Hunks) > 0 {
			kept = append(kept, u)
		}
	}
	r.Units = kept

	if r.Verdict == "approve" {
		for _, u := range r.Units {
			for _, c := range u.Checks {
				if c.Status == "not_done" {
					r.Verdict = "request_changes"
				}
			}
		}
	}
}

func fixSteps(byID map[string]*diff.Hunk, steps []Step) {
	for i := range steps {
		steps[i].Label = clampWords(steps[i].Label, 5)
		if !validHunk(byID, steps[i].Hunk) {
			steps[i].Hunk, steps[i].Line = "", 0
		}
	}
}

func validHunk(byID map[string]*diff.Hunk, h string) bool {
	if h == "" {
		return false
	}
	_, ok := byID[h]
	return ok
}

// clampWords is the last line of defence against long AI prose.
func clampWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return strings.Join(f, " ")
	}
	return strings.TrimRight(strings.Join(f[:n], " "), ",;:") + "…"
}
