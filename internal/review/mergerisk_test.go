package review

import (
	"strings"
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
)

func riskInput() *RiskInput {
	ri := &RiskInput{}
	ri.Alignment.Grade = "some_drift"
	ri.Alignment.DriveBy = []RiskClaim{
		{Title: "Renamed backoff", File: "internal/index/writer.go", Quote: "time.Sleep(backoff(attempt))", Why: "not needed"},
		{Title: "Invented", File: "internal/index/writer.go", Quote: "os.Exit(3)"},
	}
	ri.Rollback.Grade = "one_way"
	ri.Rollback.RiskClaim = RiskClaim{Title: "Drops a column", File: "db.sql", Quote: "DROP COLUMN x"} // not in the diff
	ri.Errors.Grade = "poor"
	ri.Errors.Gaps = []RiskClaim{{Title: "Busy error swallowed", File: "internal/index/writer.go", Quote: "return ErrIndexBusy", Why: "x"}}
	return ri
}

func TestGroundRiskDropsInventedClaimsAndDowngradesUnplacedOneWay(t *testing.T) {
	d := exampleDiff(t)
	ri := riskInput()
	cs := GroundRisk(ri, d, nil)
	if len(cs) != 2 || cs[0].ID != "R1" || cs[0].Kind != "drive_by" || cs[1].Kind != "error_gap" || cs[0].Line != 29 {
		t.Fatalf("claims = %+v", cs)
	}
	if ri.Rollback.Grade != "costly" || ri.Rollback.Kind != "" {
		t.Fatalf("rollback = %+v", ri.Rollback)
	}
}

func TestBuildMergeRiskScoresFiveSignals(t *testing.T) {
	d := exampleDiff(t)
	ri := riskInput()
	cs := GroundRisk(ri, d, nil)
	yes := []Vote{{Real: true, Severity: "important"}, {Real: true, Severity: "important"}, {Real: false}}
	no := []Vote{{Real: false, Reason: "the description mentions it"}, {Real: false}, {Real: true}}
	cs[0].Votes, cs[1].Votes = no, yes
	for _, c := range cs {
		TallyClaim(c, 3)
	}
	if cs[0].Status != "rejected" || cs[0].Reject == "" || cs[1].Status != "confirmed" {
		t.Fatalf("tally = %+v %+v", cs[0], cs[1])
	}
	r := &Review{Design: &Design{Risk: ri}, Units: []Unit{
		{ID: "U1", Tier: "human", Evidence: UnitEvidence{Status: "missing"}},
		{ID: "U2", Tier: "agent", Evidence: UnitEvidence{Status: "provided"}},
		{ID: "U3", Tier: "static"},
	}}
	mr := BuildMergeRisk(r, d, risk.Defaults, nil, 0, "")
	got := map[string]Signal{}
	for _, s := range mr.Signals {
		got[s.Key] = s
	}
	if len(mr.Signals) != 5 {
		t.Fatalf("signals = %d", len(mr.Signals))
	}
	// Drive-by rejected → model's "some_drift" is capped at watch.
	if got["alignment"].Level != 1 || got["errors"].Level != 1 || got["rollback"].Level != 1 || got["verification"].Level != 2 || got["impact"].Level != 0 {
		t.Fatalf("levels = %+v", got)
	}
	if mr.Overall != "medium" || len(mr.Lower) == 0 || !strings.Contains(mr.Summary, "verification") {
		t.Fatalf("overall = %+v", mr)
	}

	// No architect (quick depth): rollback and errors are not assessed and do not count.
	q := BuildMergeRisk(&Review{}, d, nil, nil, 1, "go test ./...")
	for _, s := range q.Signals {
		if (s.Key == "rollback" || s.Key == "errors" || s.Key == "alignment") && s.Level != -1 {
			t.Fatalf("quick %s = %d", s.Key, s.Level)
		}
		if s.Key == "verification" && s.Level != 2 {
			t.Fatal("failing verify command should make verification poor")
		}
	}
}

func TestAutoDepth(t *testing.T) {
	doc, _ := diff.Parse("diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1,1 +1,2 @@\n+doc\n")
	if dp, _ := AutoDepth(doc, risk.Defaults, nil, nil); dp != "quick" {
		t.Fatal("docs-only:", dp)
	}
	d := exampleDiff(t)
	if dp, _ := AutoDepth(d, risk.Defaults, nil, nil); dp != "standard" {
		t.Fatal("small code change:", dp)
	}
	auth, _ := diff.Parse("diff --git a/src/auth/login.go b/src/auth/login.go\n--- a/src/auth/login.go\n+++ b/src/auth/login.go\n@@ -1,1 +1,2 @@\n+x\n")
	if dp, why := AutoDepth(auth, risk.Defaults, nil, nil); dp != "deep" || !strings.Contains(why, "auth") {
		t.Fatal("auth change:", dp, why)
	}
	if dp, _ := AutoDepth(d, risk.Defaults, map[string]bool{"README.md": true}, nil); dp != "quick" {
		t.Fatal("re-review with no code changes:", dp)
	}
}

func TestAutoDepthHuntsATestsOnlyPR(t *testing.T) {
	d, _ := diff.Parse("diff --git a/a/x_test.go b/a/x_test.go\n--- a/a/x_test.go\n+++ b/a/x_test.go\n@@ -1,1 +1,2 @@\n+func TestX(t *testing.T) {}\n")
	if dp, why := AutoDepth(d, risk.Defaults, nil, nil); dp == "quick" {
		t.Fatalf("a tests-only PR still gets the tests lens: %s (%s)", dp, why)
	}
}
