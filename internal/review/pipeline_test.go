package review

import (
	"os"
	"strings"
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
)

func exampleDiff(t *testing.T) *diff.Diff {
	b, _ := os.ReadFile("../../examples/retry.diff")
	d, err := diff.Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPlaceFindingFromDiffWithoutWorkspace(t *testing.T) {
	d := exampleDiff(t)
	f := Finding{File: "./internal/index/writer.go", Line: 5, Quote: "time.Sleep(backoff(attempt))"}
	if !placeFinding(&f, d, nil) {
		t.Fatal("quote from the diff was not placed")
	}
	if f.Line != 29 || f.Hunk != "H2" || !f.InDiff || f.File != "internal/index/writer.go" {
		t.Fatalf("placed = %+v", f)
	}
	bad := Finding{File: "internal/index/writer.go", Line: 29, Quote: "os.Exit(1)"}
	if placeFinding(&bad, d, nil) {
		t.Fatal("invented quote was accepted")
	}
}

func TestDedupePrefersHunterAndKeepsCheckLink(t *testing.T) {
	fs := []Finding{
		{File: "x.go", Line: 10, Quote: "a", Source: "check", CheckRef: "0:1", Confidence: "medium"},
		{File: "x.go", Line: 12, Quote: "b", Source: "all", Confidence: "high", Title: "real"},
		{File: "x.go", Line: 40, Quote: "c", Source: "all"},
		{File: "y.go", Line: 10, Quote: "a", Source: "all"},
	}
	out, dups := dedupe(fs)
	if dups != 1 || len(out) != 3 {
		t.Fatalf("dups=%d out=%d", dups, len(out))
	}
	if out[0].Title != "real" || out[0].CheckRef != "0:1" {
		t.Fatalf("merged = %+v", out[0])
	}
}

func TestTallyMajority(t *testing.T) {
	v := func(real, intro bool, sev string) Vote { return Vote{Real: real, Introduced: intro, Severity: sev} }
	cases := []struct {
		votes  []Vote
		status string
		intro  bool
	}{
		{[]Vote{v(true, true, "important"), v(true, true, "important"), v(false, false, "nit")}, "confirmed", true},
		{[]Vote{v(true, true, "important"), v(false, false, "nit"), v(false, false, "nit")}, "rejected", false},
		{[]Vote{v(true, true, "important"), v(true, true, "important")}, "confirmed", true}, // one voter errored: 2/3 still a majority
		{[]Vote{v(true, true, "important")}, "rejected", false},                             // two abstained: 1/3 is not
		{[]Vote{v(true, false, "important"), v(true, false, "important"), v(true, true, "important")}, "confirmed", false},
		{[]Vote{v(true, true, "nit"), v(true, true, "nit"), v(true, true, "important")}, "nit", true},
		{[]Vote{v(true, true, "minor"), v(true, true, "minor"), v(true, true, "nit")}, "confirmed", true}, // minor bugs stay visible
	}
	sec := Finding{Category: "security", Votes: []Vote{v(true, true, "minor"), v(true, true, "important"), v(true, true, "minor")}}
	tally(&sec, 3)
	if sec.Status != "confirmed" || sec.Severity != "important" {
		t.Errorf("security finding = %s/%s, want confirmed/important", sec.Status, sec.Severity)
	}
	for i, c := range cases {
		f := Finding{Votes: c.votes}
		tally(&f, 3)
		if f.Status != c.status || f.Introduced != c.intro {
			t.Errorf("case %d: got %s/%v want %s/%v", i, f.Status, f.Introduced, c.status, c.intro)
		}
	}
}

func TestSlicesBalanceAndSkipDocs(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 12; i++ {
		name := "f" + string(rune('a'+i)) + ".go"
		b.WriteString("diff --git a/" + name + " b/" + name + "\n--- a/" + name + "\n+++ b/" + name + "\n@@ -1,1 +1,200 @@\n")
		for j := 0; j < 200; j++ {
			b.WriteString("+x\n")
		}
	}
	b.WriteString("diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1,1 +1,2 @@\n+doc\n")
	d, _ := diff.Parse(b.String())
	sl, files := slices(d, nil, 10)
	if len(files) != len(sl) {
		t.Fatal("file sets do not match slices")
	}
	if len(sl) == 0 || len(sl) > 10 {
		t.Fatalf("slices = %d", len(sl))
	}
	n := 0
	for _, s := range sl {
		for _, x := range s {
			if strings.HasPrefix(x, "README.md") {
				t.Fatal("docs were sent to a hunter")
			}
			n++
		}
	}
	if n != 12 {
		t.Fatalf("files covered = %d, want 12", n)
	}
	only, _ := slices(d, map[string]bool{"fa.go": true}, 10)
	if len(only) != 1 || len(only[0]) != 1 {
		t.Fatalf("delta slices = %v", only)
	}
}

func TestMergeDrivesTierVerdictAndChecks(t *testing.T) {
	d := exampleDiff(t)
	r := &Review{Verdict: "approve", Units: []Unit{
		{ID: "U1", Hunks: []string{"H2", "H3"}, Tier: "agent", Risk: "low",
			Checks: []Check{{Status: "not_done", Evidence: Evidence{Hunk: "H2", Line: 29}}}},
		{ID: "U2", Hunks: []string{"H1"}, Tier: "static", Risk: "low"},
	}}
	fs := []Finding{
		{ID: "F1", File: "internal/index/writer.go", Line: 29, Hunk: "H2", Status: "confirmed", Introduced: true, Title: "Sleep ignores ctx", Fix: "select on ctx.Done()", CheckRef: "0:0"},
		{ID: "F2", File: "internal/index/README.md", Line: 5, Hunk: "H1", Status: "rejected"},
	}
	Merge(r, d, fs)
	u := r.Units[0]
	if u.Tier != "human" || u.Risk != "medium" || len(u.Findings) != 1 || len(u.Ask) != 1 {
		t.Fatalf("unit = %+v", u)
	}
	if u.Checks[0].Finding != "F1" || r.Verdict != "request_changes" {
		t.Fatalf("check/verdict = %+v %s", u.Checks[0], r.Verdict)
	}

	// A rejected not_done claim is downgraded and no longer blocks.
	r2 := &Review{Verdict: "request_changes", Units: []Unit{{ID: "U1", Hunks: []string{"H2"}, Checks: []Check{{Status: "not_done"}}}}}
	Merge(r2, d, []Finding{{ID: "F1", Status: "rejected", CheckRef: "0:0", Hunk: "H2"}})
	if c := r2.Units[0].Checks[0]; c.Status != "no_evidence" || !c.Disputed || r2.Verdict != "needs_decision" {
		t.Fatalf("disputed check = %+v verdict %s", c, r2.Verdict)
	}
}

func TestGroundDesignDropsInventedClaimsAndCleansDiagram(t *testing.T) {
	d := exampleDiff(t)
	ds := &Design{
		Architecture: Architecture{
			Components: []Component{
				{ID: "Writer", Name: "Writer", Layer: "data", Kind: "changed", Files: []string{"internal/index/writer.go", "nope.go"}},
				{ID: "writer", Name: "dup"},
				{ID: "sync", Name: "Syncer", Layer: "weird", Kind: "same"},
			},
			Links: []Link{{From: "sync", To: "writer", Label: "calls Commit"}, {From: "sync", To: "ghost"}},
		},
		Flows: []CodeFlow{{Title: "Busy write", Lanes: []Lane{{ID: "sync", Name: "Syncer"}}, Steps: []FlowStep{
			{From: "sync", To: "writer", Action: "Commit", File: "internal/index/writer.go", Quote: "time.Sleep(backoff(attempt))", Kind: "added"},
			{From: "writer", To: "db", Action: "made up", File: "internal/index/writer.go", Quote: "db.Explode()"},
		}}},
		Practices: []Practice{
			{Practice: "Errors are wrapped", Status: "broken", File: "internal/index/writer.go", Quote: "return ErrIndexBusy"},
			{Practice: "Invented", Status: "broken", File: "internal/index/writer.go", Quote: "panic(\"x\")"},
		},
		Foundations: Foundations{Verdict: "bogus", Items: []Foundation{
			{Title: "Real", File: "internal/index/writer.go", Quote: "time.Sleep(backoff(attempt))", Adopt: "now"},
			{Title: "Invented", File: "internal/index/writer.go", Quote: "go func() { leak() }()"},
		}},
	}
	st := GroundDesign(ds, d, nil, 1)
	a := ds.Architecture
	if len(a.Components) != 2 || len(a.Components[0].Files) != 1 || a.Components[1].Layer != "service" {
		t.Fatalf("components = %+v", a.Components)
	}
	if len(a.Links) != 1 {
		t.Fatalf("links = %+v", a.Links)
	}
	f := ds.Flows[0]
	if !f.Steps[0].Grounded || f.Steps[0].Line != 29 || f.Steps[0].Hunk != "H2" || f.Steps[1].Grounded || len(f.Lanes) != 3 {
		t.Fatalf("flow = %+v", f)
	}
	if st.PracticesKept != 1 || st.PracticesDrop != 1 || st.FoundationsKept != 1 || st.FoundationsDrop != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if ds.Foundations.Verdict != "questionable" || ds.Foundations.Items[0].ID != "D1" || ds.Practices[0].ID != "P1" {
		t.Fatalf("ids/verdict = %+v", ds.Foundations)
	}
}

func TestDetectAttribution(t *testing.T) {
	a := DetectAttribution("feat: x\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>")
	if !a.Agent || a.Tool != "claude" || a.Reviewed != "unknown" {
		t.Fatalf("got %+v", a)
	}
	b := DetectAttribution("Written by an AI agent on behalf of @x, who has not reviewed it yet.")
	if !b.Agent || b.Reviewed != "no" {
		t.Fatalf("got %+v", b)
	}
	if c := DetectAttribution("plain human commit"); c.Agent {
		t.Fatalf("got %+v", c)
	}
}

func TestTallyFoundation(t *testing.T) {
	v := func(real bool, sev string) Vote { return Vote{Real: real, Severity: sev} }
	f := Foundation{Votes: []Vote{v(true, "important"), v(true, "minor"), v(false, "nit")}}
	TallyFoundation(&f, 3)
	if f.Status != "confirmed" {
		t.Fatal(f.Status)
	}
	g := Foundation{Votes: []Vote{v(true, "nit"), v(true, "nit"), v(true, "minor")}}
	TallyFoundation(&g, 3)
	if g.Status != "rejected" {
		t.Fatal("taste-only claim should be rejected:", g.Status)
	}
}

func TestMergeDropsAsksTheFindingRestates(t *testing.T) {
	d := exampleDiff(t)
	r := &Review{Units: []Unit{{ID: "U1", Hunks: []string{"H2"}, Tier: "agent", Risk: "low",
		Ask: []string{"Replace time.Sleep with a select on ctx.Done() and a timer.", "Add a test that forces ErrBusy."}}}}
	Merge(r, d, []Finding{{ID: "F1", Hunk: "H2", File: "internal/index/writer.go", Line: 29, Status: "confirmed", Introduced: true,
		Severity: "important", Title: "Sleep ignores cancellation", Fix: "Select on ctx.Done() and a timer instead of time.Sleep."}})
	if got := r.Units[0].Ask; len(got) != 2 || !strings.Contains(got[0], "ErrBusy") || !strings.HasPrefix(got[1], "Sleep ignores cancellation") {
		t.Fatalf("asks = %q", got)
	}
}
