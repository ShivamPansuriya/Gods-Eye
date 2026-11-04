package review

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
)

func load(t *testing.T) (*Review, *diff.Diff) {
	b, _ := os.ReadFile("../../examples/retry.diff")
	d, err := diff.Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := os.ReadFile("../../examples/retry.review.json")
	var r Review
	if err := json.Unmarshal(rb, &r); err != nil {
		t.Fatal(err)
	}
	return &r, d
}

func TestSchemaIsValidJSON(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(Schema(), &v); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeCatchesOrphansAndBadIDs(t *testing.T) {
	r, d := load(t)
	r.Units = r.Units[:1]                                    // drop the README unit → H1 is orphaned
	r.Units[0].Hunks = append(r.Units[0].Hunks, "H99", "H2") // bogus + duplicate
	r.Units[0].Notes[0].Hunk = "H42"
	Finalize(r, d, risk.Defaults)
	if got := strings.Join(r.Units[0].Hunks, ","); got != "H2,H3" {
		t.Fatalf("hunks = %s", got)
	}
	if r.Units[0].Notes[0].Hunk != "" {
		t.Fatal("invalid note hunk kept")
	}
	last := r.Units[len(r.Units)-1]
	if len(last.Hunks) != 1 || last.Hunks[0] != "H1" {
		t.Fatalf("orphan unit = %+v", last)
	}
}

func TestFinalizeRaisesTierForFailingCheck(t *testing.T) {
	r, d := load(t)
	r.Units[0].Tier = "agent"
	Finalize(r, d, risk.Defaults)
	if r.Units[0].Tier != "human" {
		t.Fatalf("tier = %s, want human (has not_done check)", r.Units[0].Tier)
	}
}

func TestClampWords(t *testing.T) {
	if got := clampWords("one two three four", 2); got != "one two…" {
		t.Fatalf("got %q", got)
	}
}
