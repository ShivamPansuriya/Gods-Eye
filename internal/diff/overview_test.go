package diff

import (
	"fmt"
	"strings"
	"testing"
)

func bigDiff(t *testing.T) *Diff {
	var b strings.Builder
	file := func(name string, lines int) {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n@@ -1,2 +1,%d @@ func x()\n ctx\n", name, name, name, name, lines+1)
		for i := 0; i < lines; i++ {
			fmt.Fprintf(&b, "+line %d of %s with some padding text\n", i, name)
		}
	}
	file("src/A.java", 40)
	file("seed/data.json", 3000)
	file("src/B.java", 40)
	d, err := Parse(b.String())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOverviewSmallIsTheFullDiff(t *testing.T) {
	d := bigDiff(t)
	got, n := d.Overview(10_000_000)
	if got != d.Annotated() || n != 0 {
		t.Fatalf("small diff changed, summarized=%d", n)
	}
}

func TestOverviewStaysUnderLimitAndKeepsEveryHunk(t *testing.T) {
	d := bigDiff(t)
	limit := 12_000
	got, n := d.Overview(limit)
	if len(got) > limit {
		t.Fatalf("overview %d chars, limit %d", len(got), limit)
	}
	for _, h := range d.Hunks() {
		if !strings.Contains(got, "--- HUNK "+h.ID+" ") {
			t.Errorf("hunk %s missing", h.ID)
		}
	}
	// Code is kept, the big data file is summarized.
	if n != 1 || !strings.Contains(got, "line 39 of src/B.java") || strings.Contains(got, "line 2999 of seed/data.json") || !strings.Contains(got, "seed/data.json (modified, +3000 -0) [summarized") {
		t.Fatalf("summarized=%d\n%s", n, got[:min(len(got), 600)])
	}
	// Original file order is kept.
	if strings.Index(got, "src/A.java") > strings.Index(got, "seed/data.json") || strings.Index(got, "seed/data.json") > strings.Index(got, "src/B.java") {
		t.Fatal("file order changed")
	}
}

func TestOnly(t *testing.T) {
	d := bigDiff(t)
	o := d.Only(map[string]bool{"src/B.java": true})
	if len(o.Files) != 1 || o.Files[0].Path != "src/B.java" || o.Files[0].Hunks[0].ID != d.Files[2].Hunks[0].ID {
		t.Fatalf("only = %+v", o.Files)
	}
}
