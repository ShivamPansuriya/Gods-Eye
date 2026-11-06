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
