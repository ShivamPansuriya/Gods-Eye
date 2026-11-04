package eval

import (
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/review"
)

func TestScore(t *testing.T) {
	r := &review.Review{
		Findings: []review.Finding{
			{ID: "F1", File: "a.ts", Line: 32, Status: "confirmed"},                                                 // within 3 lines of 31
			{ID: "F2", File: "a.ts", Line: 90, Status: "rejected"},                                                  // found but voted down
			{ID: "F3", File: "b.ts", Line: 5, Status: "confirmed", AlsoAt: []review.Loc{{File: "a.ts", Line: 200}}}, // also_at counts
			{ID: "F4", File: "c.ts", Line: 1, Status: "confirmed"},                                                  // extra
		},
	}
	exp := []Expect{
		{File: "a.ts", From: 31, To: 31, Bug: "offset"},
		{File: "a.ts", From: 88, To: 92, Bug: "rejected one"},
		{File: "a.ts", From: 199, To: 201, Bug: "class"},
		{File: "d.ts", From: 1, To: 1, Bug: "never found"},
	}
	var res Result
	Score(&res, exp, r)
	if len(res.Caught) != 2 || len(res.Missed) != 2 || len(res.Rejected) != 1 || len(res.Extra) != 1 || res.Recall != 0.5 {
		t.Fatalf("score = %+v", res)
	}
}
