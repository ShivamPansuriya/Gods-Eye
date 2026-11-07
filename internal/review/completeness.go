package review

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Gap is work the review set out to do and did not finish. Every gap is shown
// in the verdict: a review never reports a result for work it skipped.
type Gap struct {
	Stage string `json:"stage"` // hunt | design | verify
	What  string `json:"what"`  // "2 code slices not searched"
	Why   string `json:"why,omitempty"`
}

const (
	baseBudgetLines = 1500 // a PR up to this many changed code lines gets the base budget
	maxBudgetScale  = 4    // the budget never grows past base × this
	verifyReserve   = 0.45 // share of the budget kept for checking findings
)

// ScaleBudget grows the per-review budget with PR size: the base covers
// baseBudgetLines changed lines of code, then it grows in proportion, capped.
func ScaleBudget(base float64, codeLines int) (float64, string) {
	if base <= 0 {
		base = 15
	}
	scale := math.Max(1, math.Min(maxBudgetScale, float64(codeLines)/baseBudgetLines))
	b := math.Round(base*scale*100) / 100
	if scale == 1 {
		return b, fmt.Sprintf("$%.0f base for %d changed lines of code", base, codeLines)
	}
	return b, fmt.Sprintf("$%.0f base × %.1f for %d changed lines of code (base covers %d, at most ×%d)", base, scale, codeLines, baseBudgetLines, maxBudgetScale)
}

// huntCap is what mapping, design and hunting may spend together; the rest
// is held back so every finding they produce can be checked.
func huntCap(budget float64) float64 { return math.Round(budget*(1-verifyReserve)*100) / 100 }

// Finish counts every skipped check into the pipeline's gaps and marks the
// review unfinished. A verdict that would say "approve" or "needs a decision"
// becomes "incomplete"; verified bugs keep "request_changes".
func Finish(r *Review) {
	p := r.Pipeline
	if p == nil {
		return
	}
	why := ""
	if p.Budget > 0 {
		why = fmt.Sprintf("budget of $%.0f reached", p.Budget)
	}
	count := func(n int, one, many string) {
		if n == 1 {
			p.Gaps = append(p.Gaps, Gap{Stage: "verify", What: "1 " + one + " not checked", Why: why})
		} else if n > 1 {
			p.Gaps = append(p.Gaps, Gap{Stage: "verify", What: fmt.Sprintf("%d %s not checked", n, many), Why: why})
		}
	}
	nf := 0
	for _, f := range r.Findings {
		if f.Status == "skipped" {
			nf++
		}
	}
	count(nf, "finding", "findings")
	if ds := r.Design; ds != nil {
		nd := 0
		for _, f := range ds.Foundations.Items {
			if f.Status == "skipped" {
				nd++
			}
		}
		count(nd, "design claim", "design claims")
		if ri := ds.Risk; ri != nil {
			nr := 0
			for _, cs := range [][]RiskClaim{ri.Alignment.DriveBy, ri.Errors.Gaps, {ri.Rollback.RiskClaim}} {
				for _, c := range cs {
					if c.Status == "skipped" {
						nr++
					}
				}
			}
			count(nr, "risk claim", "risk claims")
		}
	}
	p.Incomplete = len(p.Gaps) > 0
	if p.Incomplete && r.Verdict != "request_changes" {
		r.Verdict = "incomplete"
	}
}

// IncompleteText is the one-line "not finished" notice, or "" when complete.
func IncompleteText(p *Pipeline) string {
	if p == nil || !p.Incomplete {
		return ""
	}
	parts := make([]string, 0, len(p.Gaps))
	for _, g := range p.Gaps {
		s := g.What
		if g.Why != "" {
			s += " (" + g.Why + ")"
		}
		parts = append(parts, s)
	}
	return "Not finished: " + strings.Join(parts, "; ") + "."
}

// decodeLenient decodes a submit tool's JSON. Tools without strict schemas
// sometimes send a nested object or array as a JSON string
// ("architecture": "{...}"); those strings are unwrapped before decoding, so
// a good result is not thrown away over its quoting.
func decodeLenient(raw []byte, v any) error {
	err := json.Unmarshal(raw, v)
	if err == nil {
		return nil
	}
	var top any
	if json.Unmarshal(raw, &top) != nil {
		return err
	}
	fixed, _ := json.Marshal(unwrapStrings(top))
	if json.Unmarshal(fixed, v) != nil {
		return err
	}
	return nil
}

func unwrapStrings(x any) any {
	switch t := x.(type) {
	case map[string]any:
		for k, v := range t {
			t[k] = unwrapStrings(v)
		}
	case []any:
		for i, v := range t {
			t[i] = unwrapStrings(v)
		}
	case string:
		s := strings.TrimSpace(t)
		if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
			var inner any
			if json.Unmarshal([]byte(s), &inner) == nil {
				return unwrapStrings(inner)
			}
		}
	}
	return x
}
