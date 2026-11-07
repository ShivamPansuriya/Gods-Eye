package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/review"
)

// InlineComment is one comment pinned to a line of the PR diff.
type InlineComment struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"` // first line of a multi-line comment (a suggested fix)
	StartSide string `json:"start_side,omitempty"`
	Line      int    `json:"line"`
	Side      string `json:"side"`
	Body      string `json:"body"`
}

// MaxInline caps how many line comments one review posts.
const MaxInline = 25

// InlineComments returns a line comment for every blocking verified bug found
// in this round on a line the PR adds or changes. Bugs from earlier rounds were already
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
		// Only what needs a human: a verified bug this PR introduces. Minor and
		// pre-existing bugs stay in the summary, so the thread is not noisy.
		// A minor bug is posted too when it comes with a fix the voters
		// accepted: one click resolves it, so it costs the author no attention.
		oneClick := f.FixVerified && f.Suggestion != ""
		if f.Status != "confirmed" || !f.Introduced || (f.Severity == "minor" && !oneClick) || !f.InDiff || f.File == "" || f.Line <= 0 {
			continue
		}
		if f.Round != 0 && round != 0 && f.Round != round {
			continue
		}
		label := "🐞 **Bug**"
		if f.Severity == "minor" {
			label = "🔧 **Minor bug, one-click fix**"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s\n\n", label, f.Title)
		if f.Scenario != "" {
			fmt.Fprintf(&b, "%s\n\n", f.Scenario)
		}
		if f.Fix != "" {
			fmt.Fprintf(&b, "**Fix:** %s\n\n", f.Fix)
		}
		c := InlineComment{Path: f.File, Line: f.Line, Side: "RIGHT"}
		if f.FixVerified && f.Suggestion != "" {
			fence := "```"
			for strings.Contains(f.Suggestion, fence) {
				fence += "`"
			}
			fmt.Fprintf(&b, "%ssuggestion\n%s\n%s\n\n", fence, f.Suggestion, fence)
			if f.EndLine > f.Line {
				c.StartLine, c.StartSide, c.Line = f.Line, "RIGHT", f.EndLine
			}
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
		if f.FixVerified {
			b.WriteString(" · fix checked by the same reviewers")
		}
		if by := foundBy(f.Source, f.FoundBy); by != "" {
			b.WriteString(" · found by " + by)
		}
		b.WriteString("</sub>\n" + ThreadMarker(f))
		c.Body = b.String()
		out = append(out, c)
		if len(out) == MaxInline {
			break
		}
	}
	return out
}

// foundBy names who reported a finding when an outside engine was involved.
func foundBy(source string, all []string) string {
	if len(all) == 0 {
		all = []string{source}
	}
	ocr, own := false, false
	for _, s := range all {
		switch s {
		case "ocr":
			ocr = true
		case "check":
		default:
			own = true
		}
	}
	switch {
	case ocr && own:
		return "God's Eye and open-code-review"
	case ocr:
		return "open-code-review"
	}
	return ""
}

// ThreadMarker is a hidden tag in a line comment that names its finding by
// what it is (file, quoted code, title), not by its ID: IDs are renumbered
// every round, the marker is not.
func ThreadMarker(f review.Finding) string {
	h := sha256.Sum256([]byte(f.File + "\n" + f.Quote + "\n" + f.Title))
	return "<!-- godseye:finding=" + hex.EncodeToString(h[:6]) + " -->"
}

// FixedThreads maps the marker of every finding this round found fixed to the
// reply that closes its thread.
func FixedThreads(rep *Report) map[string]string {
	sha := rep.Meta.HeadSHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	out := map[string]string{}
	for _, f := range rep.Review.Findings {
		if f.Status == "fixed" {
			out[ThreadMarker(f)] = fmt.Sprintf("✅ Fixed in `%s`: the flagged code is gone. Resolved by God's Eye.", sha)
		}
	}
	return out
}
