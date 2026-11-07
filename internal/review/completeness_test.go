package review

import (
	"strings"
	"testing"
)

func TestScaleBudget(t *testing.T) {
	cases := []struct {
		base  float64
		lines int
		want  float64
	}{
		{15, 300, 15},   // small PR: base
		{15, 1500, 15},  // up to the base size
		{15, 3000, 30},  // twice the size, twice the budget
		{15, 40000, 60}, // capped at 4x
		{0, 3000, 30},   // no base set: default 15
	}
	for _, c := range cases {
		got, why := ScaleBudget(c.base, c.lines)
		if got != c.want || why == "" {
			t.Errorf("ScaleBudget(%v, %d) = %v %q, want %v", c.base, c.lines, got, why, c.want)
		}
	}
}

func TestFinishMarksSkippedWorkAndNeverReportsZeroBugs(t *testing.T) {
	r := &Review{Verdict: "approve", Pipeline: &Pipeline{Budget: 15},
		Findings: []Finding{{Status: "skipped"}, {Status: "skipped"}, {Status: "rejected"}},
		Design:   &Design{Foundations: Foundations{Items: []Foundation{{Status: "skipped"}}}, Risk: &RiskInput{}},
	}
	r.Design.Risk.Errors.Gaps = []RiskClaim{{Status: "skipped"}}
	r.Pipeline.Gaps = []Gap{{Stage: "design", What: "design step failed", Why: "parse error"}}
	Finish(r)
	p := r.Pipeline
	if !p.Incomplete || r.Verdict != "incomplete" {
		t.Fatalf("incomplete=%v verdict=%s", p.Incomplete, r.Verdict)
	}
	s := IncompleteText(p)
	for _, want := range []string{"2 findings not checked", "1 design claim not checked", "1 risk claim not checked", "design step failed", "$15"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary %q lacks %q", s, want)
		}
	}

	// Verified bugs keep request_changes, but the review is still marked unfinished.
	r2 := &Review{Verdict: "request_changes", Pipeline: &Pipeline{}, Findings: []Finding{{Status: "confirmed"}, {Status: "skipped"}}}
	Finish(r2)
	if r2.Verdict != "request_changes" || !r2.Pipeline.Incomplete {
		t.Fatalf("verdict=%s incomplete=%v", r2.Verdict, r2.Pipeline.Incomplete)
	}

	// Nothing skipped: complete, verdict untouched.
	r3 := &Review{Verdict: "approve", Pipeline: &Pipeline{}, Findings: []Finding{{Status: "rejected"}}}
	Finish(r3)
	if r3.Pipeline.Incomplete || r3.Verdict != "approve" || IncompleteText(r3.Pipeline) != "" {
		t.Fatalf("complete review changed: %+v %s", r3.Pipeline, r3.Verdict)
	}
}

func TestVerifyReserve(t *testing.T) {
	if got := huntCap(60); got != 33 {
		t.Fatalf("huntCap(60) = %v, want 33 (45%% kept for checking)", got)
	}
}

func TestDecodeLenientUnwrapsStringifiedObjects(t *testing.T) {
	raw := []byte(`{"architecture":"{\"summary\":\"s\",\"components\":[{\"id\":\"a\"}]}","flows":"[]","practices":[],"foundations":{"verdict":"sound","items":[]}}`)
	var ds Design
	if err := decodeLenient(raw, &ds); err != nil {
		t.Fatal(err)
	}
	if ds.Architecture.Summary != "s" || len(ds.Architecture.Components) != 1 || ds.Foundations.Verdict != "sound" {
		t.Fatalf("decoded = %+v", ds)
	}
	var bad Design
	if err := decodeLenient([]byte(`{"architecture":"not json"}`), &bad); err == nil {
		t.Fatal("a plain string where an object belongs must still fail")
	}
}
