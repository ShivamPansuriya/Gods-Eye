package review

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/ocr"
	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

// The third voter is skipped only when two "not real" votes already decide
// the claim: for every way the first two votes and the third can fall, the
// status with the third vote skipped equals the status with it cast.
func TestSkippingTheThirdVoteNeverChangesTheOutcome(t *testing.T) {
	sevs := []string{"important", "minor", "nit"}
	var opts []Vote
	for _, real := range []bool{true, false} {
		for _, sev := range sevs {
			opts = append(opts, Vote{Real: real, Severity: sev, Introduced: true, Reason: "r" + sev})
		}
	}
	opts = append(opts, Vote{}) // a failed vote: nothing recorded
	cast := func(vs ...Vote) []Vote {
		var out []Vote
		for _, v := range vs {
			if v != (Vote{}) {
				out = append(out, v)
			}
		}
		return out
	}
	checked := 0
	for _, a := range opts {
		for _, b := range opts {
			first := cast(a, b)
			realSoFar := 0
			for _, v := range first {
				if v.Real {
					realSoFar++
				}
			}
			if (realSoFar+1)*2 > 3 {
				continue // the third vote runs
			}
			skipped := Finding{Votes: first}
			tally(&skipped, 3)
			for _, c := range opts {
				full := Finding{Votes: cast(a, b, c)}
				tally(&full, 3)
				if full.Status != skipped.Status || full.Severity != skipped.Severity || full.Introduced != skipped.Introduced {
					t.Fatalf("votes %v,%v,%v: with third %s/%s, without %s/%s", a, b, c, full.Status, full.Severity, skipped.Status, skipped.Severity)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no case skipped the third vote")
	}
}

func TestFixIsVerifiedByMostRealVotes(t *testing.T) {
	for _, c := range []struct {
		fixOK []bool
		want  bool
	}{{[]bool{true, true, false}, true}, {[]bool{true, false, false}, false}, {[]bool{true, true, true}, true}} {
		f := Finding{Suggestion: "x := 1"}
		for _, ok := range c.fixOK {
			f.Votes = append(f.Votes, Vote{Real: true, Severity: "important", Introduced: true, FixOK: ok})
		}
		tally(&f, 3)
		if f.Status != "confirmed" || f.FixVerified != c.want {
			t.Errorf("fix_ok %v: status %s, verified %v", c.fixOK, f.Status, f.FixVerified)
		}
	}
	noFix := Finding{Votes: []Vote{{Real: true, Severity: "important", FixOK: true}, {Real: true, Severity: "important", FixOK: true}}}
	tally(&noFix, 3)
	if noFix.FixVerified {
		t.Error("a finding without a suggestion cannot have a verified fix")
	}
}

func TestCheckSuggestion(t *testing.T) {
	d := exampleDiff(t)
	const file = "internal/index/writer.go"
	for _, c := range []struct {
		name          string
		line, endLine int
		suggestion    string
		want          string
		wantEnd       int
	}{
		{"one added line", 29, 29, "\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\treturn ctx.Err()\n\t\tcase <-time.After(backoff(attempt)):\n\t\t}\n", "\t\tselect {\n\t\tcase <-ctx.Done():\n\t\t\treturn ctx.Err()\n\t\tcase <-time.After(backoff(attempt)):\n\t\t}", 29},
		{"no end line means one line", 29, 0, "\t\tctxSleep(ctx, backoff(attempt))", "\t\tctxSleep(ctx, backoff(attempt))", 29},
		{"context lines in the hunk are fine", 35, 37, "\ttx, err := w.db.BeginTx(ctx, nil)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"begin: %w\", err)", "\ttx, err := w.db.BeginTx(ctx, nil)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"begin: %w\", err)", 37},
		{"range leaves the diff", 36, 40, "x", "", 0},
		{"same code is no fix", 29, 29, "  time.Sleep(backoff(attempt))", "", 0},
		{"too long", 3, 25, "x", "", 0},
		{"empty", 29, 29, "  ", "", 0},
	} {
		f := Finding{File: file, Line: c.line, EndLine: c.endLine, Suggestion: c.suggestion}
		checkSuggestion(&f, d)
		if f.Suggestion != c.want || f.EndLine != c.wantEnd {
			t.Errorf("%s: got %q end %d, want %q end %d", c.name, f.Suggestion, f.EndLine, c.want, c.wantEnd)
		}
	}
}

func TestFromOCR(t *testing.T) {
	fs := fromOCR([]ocr.Comment{
		{Path: "a/Svc.java", StartLine: 60, EndLine: 62, Severity: "high", Category: "bug",
			Content: "Every job is counted, including `CANCELLED` ones. That moves the end date.", ExistingCode: "+  .max(a.end())\n  .orElse(null);", SuggestionCode: "  .filter(Job::live)\n  .max(a.end())"},
		{Path: "a/Svc.java", StartLine: 10, EndLine: 10, Severity: "low", Category: "style", Content: "Rename x."},
		{Path: "db/V1.sql", StartLine: 146, EndLine: 147, Severity: "medium", Category: "performance", Content: "Full table scan on every write.", SuggestionCode: "CREATE INDEX i ON t(c);"},
	}, 2)
	if len(fs) != 2 {
		t.Fatalf("style comments must be dropped: %d findings", len(fs))
	}
	a, b := fs[0], fs[1]
	if a.Title != "Every job is counted, including `CANCELLED` ones" || a.Quote != ".max(a.end())" || a.Severity != "important" ||
		a.Confidence != "high" || a.Source != "ocr" || a.Round != 2 || a.Line != 60 || a.EndLine != 62 || a.Suggestion == "" {
		t.Errorf("first: %+v", a)
	}
	if b.Category != "performance" || b.Severity != "minor" || b.Suggestion != "" {
		t.Errorf("a suggestion without the code it replaces is dropped; got %+v", b)
	}
}

func TestDedupeRecordsEverySourceAndPrefersGodsEyesHunter(t *testing.T) {
	fs, dups := dedupe([]Finding{
		{File: "a.go", Line: 10, Quote: "x := y", Source: "ocr", Title: "long ocr text", Confidence: "high"},
		{File: "a.go", Line: 11, Quote: "x := y", Source: "logic", Title: "Nil y panics", Confidence: "medium"},
		{File: "a.go", Line: 12, Quote: "z", Source: "check"},
		{File: "b.go", Line: 1, Quote: "q", Source: "ocr"},
	})
	if dups != 2 || len(fs) != 2 {
		t.Fatalf("dups %d, kept %d", dups, len(fs))
	}
	if fs[0].Title != "Nil y panics" || !reflect.DeepEqual(fs[0].FoundBy, []string{"check", "logic", "ocr"}) {
		t.Errorf("merged: %+v", fs[0])
	}
	if fs[1].FoundBy != nil {
		t.Errorf("a single source records no FoundBy: %v", fs[1].FoundBy)
	}
}

func TestWithoutKnownDropsBugsStillOpenFromEarlierRounds(t *testing.T) {
	carried := []Finding{{File: "a.go", Line: 40, Quote: "q", Status: "confirmed"}, {File: "a.go", Line: 90, Quote: "r", Status: "fixed"}}
	got, n := withoutKnown([]Finding{{File: "a.go", Line: 42, Quote: "other"}, {File: "a.go", Line: 90, Quote: "r"}, {File: "b.go", Line: 40}}, carried)
	if n != 1 || len(got) != 2 || fmt.Sprint(got[0].Line) != "90" {
		t.Errorf("dropped %d, kept %+v", n, got)
	}
}

// A finding counts as fixed only when its file is still there and the quoted
// code is gone. A renamed file keeps the bug open: resolving its thread
// would tell the author a bug is fixed that is not.
func TestCarryForwardDoesNotCallAMovedFileFixed(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
	}
	write("a.go", "package a\nfunc f() { time.Sleep(d) }\n")
	write("b.go", "package a\nfunc g() { x := 1 }\n")
	write("c.go", "package a\nfunc h() { y := 2 }\n")
	git("init", "-q")
	git("add", "-A")
	git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "1")
	git("mv", "a.go", "moved.go")                     // bug moved, not fixed
	write("b.go", "package a\nfunc g() { x := 2 }\n") // quoted code gone: fixed
	git("add", "-A")
	git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "2")
	ws, err := workspace.Open(dir, git("rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	prev := []Finding{
		{ID: "F1", File: "a.go", Quote: "time.Sleep(d)", Status: "confirmed"},
		{ID: "F2", File: "b.go", Quote: "x := 1", Status: "confirmed"},
		{ID: "F3", File: "c.go", Quote: "y := 2", Status: "confirmed"},
		{ID: "F4", File: "c.go", Quote: "z", Status: "rejected"},
	}
	carried, fixed := carryForward(ws, prev)
	got := map[string]string{}
	for _, f := range carried {
		got[f.ID] = f.Status
	}
	if fixed != 1 || got["F1"] != "confirmed" || got["F2"] != "fixed" || got["F3"] != "confirmed" || len(carried) != 3 {
		t.Errorf("fixed %d, carried %v", fixed, got)
	}
}
