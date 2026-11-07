package render

import (
	"strings"
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/review"
)

func TestInlineCommentsOnlyNewBlockingBugsOnDiffLines(t *testing.T) {
	rep := &Report{Review: &review.Review{Pipeline: &review.Pipeline{Round: 2}, Findings: []review.Finding{
		{ID: "F1", Title: "Sleep ignores ctx", File: "a.go", Line: 29, InDiff: true, Status: "confirmed", Introduced: true, Round: 2, Fix: "select on ctx.Done()",
			Votes: []review.Vote{{Real: true}, {Real: true}, {Real: false}}},
		{ID: "F2", File: "a.go", Line: 40, InDiff: true, Status: "rejected", Round: 2},
		{ID: "F3", File: "a.go", Line: 50, InDiff: false, Status: "confirmed", Round: 2},
		{ID: "F4", File: "a.go", Line: 60, InDiff: true, Status: "confirmed", Round: 1}, // commented on last round
		{ID: "F5", Title: "Off by one", File: "b.go", Line: 7, InDiff: true, Status: "confirmed", Introduced: true, Severity: "minor", Round: 2},
	}}}
	cs := InlineComments(rep)
	if len(cs) != 1 || cs[0].Path != "a.go" || cs[0].Line != 29 || cs[0].Side != "RIGHT" { // F5 is minor: summary only
		t.Fatalf("comments = %+v", cs)
	}
	if !strings.Contains(cs[0].Body, "select on ctx.Done()") || !strings.Contains(cs[0].Body, "2/3") {
		t.Fatalf("body = %q", cs[0].Body)
	}
}

func TestInlineCommentsCarryVerifiedFixesAsSuggestions(t *testing.T) {
	rep := &Report{Review: &review.Review{Pipeline: &review.Pipeline{Round: 1}, Findings: []review.Finding{
		{ID: "F1", Title: "Sleep ignores ctx", File: "a.go", Line: 29, EndLine: 31, InDiff: true, Status: "confirmed", Introduced: true, Round: 1,
			Suggestion: "select {\ncase <-ctx.Done():\n}", FixVerified: true, Source: "logic", FoundBy: []string{"logic", "ocr"}},
		{ID: "F2", Title: "Unverified fix", File: "a.go", Line: 40, InDiff: true, Status: "confirmed", Introduced: true, Round: 1, Suggestion: "x"},
		{ID: "F3", Title: "Off by one", File: "b.go", Line: 7, InDiff: true, Status: "confirmed", Introduced: true, Severity: "minor", Round: 1,
			Suggestion: "for i := 0; i < n; i++ {", FixVerified: true, Source: "ocr"},
		{ID: "F4", Title: "Minor without fix", File: "b.go", Line: 9, InDiff: true, Status: "confirmed", Introduced: true, Severity: "minor", Round: 1},
	}}}
	cs := InlineComments(rep)
	if len(cs) != 3 {
		t.Fatalf("want F1, F2 and the one-click minor F3; got %+v", cs)
	}
	if cs[0].StartLine != 29 || cs[0].Line != 31 || cs[0].StartSide != "RIGHT" || !strings.Contains(cs[0].Body, "```suggestion\nselect {") ||
		!strings.Contains(cs[0].Body, "found by God's Eye and open-code-review") {
		t.Errorf("F1 = %+v", cs[0])
	}
	if cs[1].StartLine != 0 || strings.Contains(cs[1].Body, "suggestion") {
		t.Errorf("an unverified fix is not offered: %+v", cs[1])
	}
	if cs[2].StartLine != 0 || cs[2].Line != 7 || !strings.Contains(cs[2].Body, "one-click fix") || !strings.Contains(cs[2].Body, "found by open-code-review") {
		t.Errorf("F3 = %+v", cs[2])
	}
}

// A posted bug carries a marker that survives the next round's renumbering,
// so when its code is gone the thread it opened can be found and resolved.
func TestFixedBugsCloseTheThreadsTheyOpened(t *testing.T) {
	bug := review.Finding{ID: "F1", Title: "Sleep ignores ctx", File: "a.go", Line: 29, Quote: "time.Sleep(d)", InDiff: true, Status: "confirmed", Introduced: true, Round: 1}
	round1 := &Report{Review: &review.Review{Pipeline: &review.Pipeline{Round: 1}, Findings: []review.Finding{bug}}}
	cs := InlineComments(round1)
	if len(cs) != 1 || !strings.Contains(cs[0].Body, ThreadMarker(bug)) {
		t.Fatalf("comment lacks its marker: %+v", cs)
	}

	fixed := bug
	fixed.ID, fixed.Status = "F3", "fixed" // renumbered in round 2
	open := review.Finding{ID: "F4", Title: "Other", File: "b.go", Quote: "x", Status: "confirmed", Round: 1}
	round2 := &Report{Meta: Meta{HeadSHA: "0123456789abcdef"}, Review: &review.Review{Pipeline: &review.Pipeline{Round: 2}, Findings: []review.Finding{fixed, open}}}
	got := FixedThreads(round2)
	if len(got) != 1 || !strings.Contains(got[ThreadMarker(bug)], "0123456") {
		t.Errorf("FixedThreads = %v", got)
	}
	if ThreadMarker(bug) == ThreadMarker(open) {
		t.Error("two findings share a marker")
	}
}
