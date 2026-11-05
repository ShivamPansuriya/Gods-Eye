// Package review runs the review pipeline and cleans its result.
package review

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
)

// Finalize enforces the rules the model can't be trusted with: every hunk is in
// exactly one unit, path rules raise tiers, ids are valid, text stays short.
func Finalize(r *Review, d *diff.Diff, rules []risk.Rule) {
	byID := d.HunkByID()
	seen := map[string]bool{}

	for i := range r.Units {
		u := &r.Units[i]
		if u.ID == "" || strings.ContainsAny(u.ID, " /#") {
			u.ID = fmt.Sprintf("U%d", i+1)
		}
		var hs []string
		var paths []string
		for _, h := range u.Hunks {
			hk, ok := byID[h]
			if !ok || seen[h] {
				continue
			}
			seen[h] = true
			hs = append(hs, h)
			paths = append(paths, hk.File)
		}
		u.Hunks = hs

		// Path rules can only raise the tier.
		if floor, why := risk.Floor(rules, paths); risk.Rank(floor) > risk.Rank(risk.Tier(u.Tier)) {
			u.Tier = string(floor)
			u.RiskReason = "Always needs review: " + why
			if floor == risk.Human && u.Risk == "low" {
				u.Risk = "medium"
			}
		}
		for _, c := range u.Checks {
			if c.Status == "not_done" {
				u.Tier = string(risk.Human)
			}
		}

		u.Title = clampWords(u.Title, 10)
		u.Brief = clampWords(u.Brief, 45)
		u.Question = clampWords(u.Question, 26)
		u.Evidence.Have, u.Evidence.Ask = clampWords(u.Evidence.Have, 26), clampWords(u.Evidence.Ask, 36)
		u.Evidence.Status = oneOf(u.Evidence.Status, "missing", "provided", "partial", "missing")
		u.RiskReason = clampWords(u.RiskReason, 16)
		for j := range u.Notes {
			u.Notes[j].Note = clampWords(u.Notes[j].Note, 26)
			if !validHunk(byID, u.Notes[j].Hunk) {
				u.Notes[j].Hunk, u.Notes[j].Line = "", 0
			}
		}
		for j := range u.Checks {
			u.Checks[j].Detail = clampWords(u.Checks[j].Detail, 32)
			if !validHunk(byID, u.Checks[j].Evidence.Hunk) {
				u.Checks[j].Evidence.Hunk, u.Checks[j].Evidence.Line = "", 0
			}
		}
		for j := range u.Blast {
			u.Blast[j].Detail = clampWords(u.Blast[j].Detail, 32)
		}
		for j := range u.Ask {
			u.Ask[j] = clampWords(u.Ask[j], 32)
		}
		fixSteps(byID, u.Flow.Before)
		fixSteps(byID, u.Flow.After)
	}

	// Any hunk the model forgot goes into a catch-all unit so nothing is hidden.
	var orphans []string
	for _, h := range d.Hunks() {
		if !seen[h.ID] {
			orphans = append(orphans, h.ID)
		}
	}
	if len(orphans) > 0 {
		r.Units = append(r.Units, Unit{
			ID: fmt.Sprintf("U%d", len(r.Units)+1), Title: "Other changes (not grouped)",
			Hunks: orphans, Tier: "agent", Risk: "low",
			RiskReason: "Not grouped by the reviewer; skim them",
			Brief:      "These hunks were not placed in any unit. Skim them yourself.",
		})
	}

	kept := r.Units[:0]
	ids := map[string]bool{}
	for _, u := range r.Units {
		if len(u.Hunks) == 0 {
			continue
		}
		for ids[u.ID] {
			u.ID += "b"
		}
		ids[u.ID] = true
		kept = append(kept, u)
	}
	r.Units = kept

	st := &r.Story
	st.Why, st.What = clampWords(st.Why, 50), clampWords(st.What, 60)
	st.Before, st.After = clampWords(st.Before, 30), clampWords(st.After, 30)
	for i := range st.Impact {
		st.Impact[i].Area, st.Impact[i].Change = clampWords(st.Impact[i].Area, 5), clampWords(st.Impact[i].Change, 24)
	}
	for i := range st.Focus {
		st.Focus[i] = clampWords(st.Focus[i], 18)
	}

	if r.Verdict == "approve" {
		for _, u := range r.Units {
			for _, c := range u.Checks {
				if c.Status == "not_done" {
					r.Verdict = "request_changes"
				}
			}
		}
	}
}

// Merge attaches verified findings to units and lets them drive tiers, asks
// and the verdict. A not_done check the verifiers rejected is downgraded.
func Merge(r *Review, d *diff.Diff, fs []Finding) {
	unitOfHunk := map[string]int{}
	unitOfFile := map[string]int{}
	byID := d.HunkByID()
	for i, u := range r.Units {
		for _, h := range u.Hunks {
			unitOfHunk[h] = i
			if hk := byID[h]; hk != nil {
				if _, ok := unitOfFile[hk.File]; !ok {
					unitOfFile[hk.File] = i
				}
			}
		}
	}
	anyImportant := false
	for k := range fs {
		f := &fs[k]
		f.Title = clampWords(f.Title, 12)
		f.Scenario = clampWords(f.Scenario, 50)
		f.Fix = clampWords(f.Fix, 32)
		ui, ok := unitOfHunk[f.Hunk]
		if !ok {
			ui, ok = unitOfFile[f.File]
		}
		if ok {
			f.Unit = r.Units[ui].ID
		}

		// Link back to the check this came from (or was merged with).
		if f.CheckRef != "" {
			parts := strings.SplitN(f.CheckRef, ":", 2)
			a, _ := strconv.Atoi(parts[0])
			b, _ := strconv.Atoi(parts[1])
			if a < len(r.Units) && b < len(r.Units[a].Checks) {
				c := &r.Units[a].Checks[b]
				switch f.Status {
				case "confirmed":
					c.Finding = f.ID
					if f.Severity == "minor" {
						c.Detail = clampWords(c.Detail, 28) + " (minor impact)"
					}
				case "rejected", "nit":
					c.Status, c.Disputed = "no_evidence", true
					c.Detail = clampWords(c.Detail, 24) + " Verifiers could not confirm this."
				}
			}
		}

		if f.Status != "confirmed" || !ok {
			continue
		}
		u := &r.Units[ui]
		u.Findings = append(u.Findings, f.ID)
		if f.Introduced {
			if f.Severity != "minor" {
				anyImportant = true
				u.Tier = "human"
				if u.Risk == "low" {
					u.Risk = "medium"
				}
			}
			if f.Fix != "" {
				// The verified finding's fix replaces the mapper's own wording of the same ask.
				kept := u.Ask[:0]
				for _, a := range u.Ask {
					if overlap(a, f.Title+" "+f.Fix) < 0.4 {
						kept = append(kept, a)
					}
				}
				u.Ask = kept
				ask := f.Title + ": " + f.Fix
				dup := false
				for _, a := range u.Ask {
					if strings.EqualFold(a, ask) {
						dup = true
					}
				}
				if !dup {
					u.Ask = append(u.Ask, ask)
				}
			}
		}
	}
	r.Findings = fs

	// The verdict follows the evidence.
	notDone := false
	for _, u := range r.Units {
		for _, c := range u.Checks {
			if c.Status == "not_done" {
				notDone = true
			}
		}
	}
	switch {
	case anyImportant || notDone:
		r.Verdict = "request_changes"
	case r.Verdict == "request_changes":
		r.Verdict = "needs_decision" // the model asked for changes but nothing survived verification
	}
}

func fixSteps(byID map[string]*diff.Hunk, steps []Step) {
	for i := range steps {
		steps[i].Label = clampWords(steps[i].Label, 5)
		if !validHunk(byID, steps[i].Hunk) {
			steps[i].Hunk, steps[i].Line = "", 0
		}
	}
}

func validHunk(byID map[string]*diff.Hunk, h string) bool {
	if h == "" {
		return false
	}
	_, ok := byID[h]
	return ok
}

// clampWords is the last line of defence against long AI prose.
func clampWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) <= n {
		return strings.Join(f, " ")
	}
	return strings.TrimRight(strings.Join(f[:n], " "), ",;:") + "…"
}

// overlap is the share of the shorter text's meaningful words found in the other.
func overlap(a, b string) float64 {
	words := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
		}) {
			if len(w) >= 4 && !stopWords[w] {
				m[w] = true
			}
		}
		return m
	}
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	if len(wb) < len(wa) {
		wa, wb = wb, wa
	}
	n := 0
	for w := range wa {
		if wb[w] {
			n++
		}
	}
	return float64(n) / float64(len(wa))
}

var stopWords = map[string]bool{"that": true, "this": true, "with": true, "when": true, "then": true, "from": true, "into": true, "only": true, "after": true, "before": true, "should": true, "would": true, "make": true, "pass": true, "return": true, "returns": true}
