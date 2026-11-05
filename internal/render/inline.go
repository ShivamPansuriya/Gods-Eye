package render

import (
	"fmt"
	"strings"
)

// InlineComment is one comment pinned to a line of the PR diff.
type InlineComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// MaxInline caps how many line comments one review posts.
const MaxInline = 25

// InlineComments returns a line comment for every verified bug found in this
// round on a line the PR adds or changes. Bugs from earlier rounds were already
// commented on, and lines outside the diff cannot carry a GitHub line comment
// (they stay in the summary).
func InlineComments(rep *Report) []InlineComment {
	r := rep.Review
	round := 0
	if r.Pipeline != nil {
		round = r.Pipeline.Round
	}
	var out []InlineComment
	for _, f := range r.Findings {
		if f.Status != "confirmed" || !f.InDiff || f.File == "" || f.Line <= 0 {
			continue
		}
		if f.Round != 0 && round != 0 && f.Round != round {
			continue
		}
		label := "🐞 **Bug**"
		switch {
		case !f.Introduced:
			label = "🕰 **Pre-existing bug**"
		case f.Severity == "minor":
			label = "⚠️ **Minor**"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s\n\n", label, f.Title)
		if f.Scenario != "" {
			fmt.Fprintf(&b, "%s\n\n", f.Scenario)
		}
		if f.Fix != "" {
			fmt.Fprintf(&b, "**Fix:** %s\n\n", f.Fix)
		}
		real := 0
		for _, v := range f.Votes {
			if v.Real {
				real++
			}
		}
		fmt.Fprintf(&b, "<sub>👁 God's Eye · %s", f.ID)
		if len(f.Votes) > 0 {
			fmt.Fprintf(&b, " · verified by %d/%d reviewers", real, len(f.Votes))
		}
		b.WriteString("</sub>")
		out = append(out, InlineComment{Path: f.File, Line: f.Line, Side: "RIGHT", Body: b.String()})
		if len(out) == MaxInline {
			break
		}
	}
	return out
}
