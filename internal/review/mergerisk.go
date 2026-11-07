package review

import (
	"fmt"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
	"github.com/ShivamPansuriya/gods-eye/internal/scope"
	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

// RiskInput is what the architect says about three merge-risk signals that need
// judgement: alignment, rollback cost and error detection. Every claim with a
// code line is grounded and then voted on like a bug.
type RiskInput struct {
	Alignment struct {
		Grade   string      `json:"grade"` // aligned | some_drift | drifted
		Reason  string      `json:"reason"`
		DriveBy []RiskClaim `json:"drive_by"`
	} `json:"alignment"`
	Rollback struct {
		Grade  string `json:"grade"` // two_way | costly | one_way
		Reason string `json:"reason"`
		Steps  string `json:"steps"` // how to roll back
		RiskClaim
	} `json:"rollback"`
	Errors struct {
		Grade  string      `json:"grade"` // good | decent | poor
		Reason string      `json:"reason"`
		Gaps   []RiskClaim `json:"gaps"`
	} `json:"errors"`
}

// RiskClaim is one line-level claim behind a risk signal.
type RiskClaim struct {
	ID     string `json:"id,omitempty"`
	Kind   string `json:"kind,omitempty"` // drive_by | rollback | error_gap
	Title  string `json:"title"`
	File   string `json:"file"`
	Line   int    `json:"line"`
	Quote  string `json:"quote"`
	Why    string `json:"why"`
	Hunk   string `json:"hunk,omitempty"`
	Votes  []Vote `json:"votes,omitempty"`
	Status string `json:"status,omitempty"` // confirmed | rejected | unverified | skipped
	Reject string `json:"reject,omitempty"` // why the verifiers disagreed
}

// MergeRisk is the five-signal score shown at the top of a review: how much
// human attention this PR needs, and what would lower it.
type MergeRisk struct {
	Overall string   `json:"overall"` // low | medium | high
	Score   int      `json:"score"`   // 0-100
	Summary string   `json:"summary"`
	Signals []Signal `json:"signals"`
	Lower   []string `json:"lower,omitempty"` // steps that would lower the risk
}

// Signal is one of alignment, impact, verification, rollback, errors.
type Signal struct {
	Key    string      `json:"key"`
	Name   string      `json:"name"`
	Grade  string      `json:"grade"`
	Level  int         `json:"level"` // 0 fine, 1 watch, 2 risk, -1 not assessed
	Reason string      `json:"reason"`
	Claims []RiskClaim `json:"claims,omitempty"`
}

// GroundRisk drops claims whose quoted code does not exist and numbers the rest.
// A rollback claim that cannot be placed is downgraded from one_way to costly.
func GroundRisk(ri *RiskInput, d *diff.Diff, ws *workspace.Workspace) []*RiskClaim {
	ri.Alignment.Grade = oneOf(ri.Alignment.Grade, "aligned", "aligned", "some_drift", "drifted")
	ri.Rollback.Grade = oneOf(ri.Rollback.Grade, "two_way", "two_way", "costly", "one_way")
	ri.Errors.Grade = oneOf(ri.Errors.Grade, "decent", "good", "decent", "poor")
	ri.Alignment.Reason = clampWords(ri.Alignment.Reason, 30)
	ri.Rollback.Reason, ri.Rollback.Steps = clampWords(ri.Rollback.Reason, 30), clampWords(ri.Rollback.Steps, 36)
	ri.Errors.Reason = clampWords(ri.Errors.Reason, 30)

	ground := func(cs []RiskClaim, kind string, limit int) []RiskClaim {
		var out []RiskClaim
		for _, c := range cs {
			c.File = strings.TrimPrefix(strings.TrimSpace(c.File), "./")
			ln, ok := locate(c.File, c.Quote, c.Line, d, ws)
			if !ok {
				continue
			}
			c.Line, c.Hunk, c.Kind = ln, hunkAt(d, c.File, ln), kind
			c.Title, c.Why = clampWords(c.Title, 12), clampWords(c.Why, 40)
			out = append(out, c)
			if len(out) == limit {
				break
			}
		}
		return out
	}
	ri.Alignment.DriveBy = ground(ri.Alignment.DriveBy, "drive_by", 4)
	ri.Errors.Gaps = ground(ri.Errors.Gaps, "error_gap", 4)
	rb := &ri.Rollback
	if rb.Grade != "two_way" {
		g := ground([]RiskClaim{rb.RiskClaim}, "rollback", 1)
		if len(g) == 1 {
			rb.RiskClaim = g[0]
			if rb.Title == "" {
				rb.Title = "Hard to roll back"
			}
		} else {
			rb.RiskClaim = RiskClaim{}
			if rb.Grade == "one_way" {
				rb.Grade = "costly"
			}
		}
	} else {
		rb.RiskClaim = RiskClaim{}
	}

	var all []*RiskClaim
	for i := range ri.Alignment.DriveBy {
		all = append(all, &ri.Alignment.DriveBy[i])
	}
	if rb.Kind == "rollback" {
		all = append(all, &rb.RiskClaim)
	}
	for i := range ri.Errors.Gaps {
		all = append(all, &ri.Errors.Gaps[i])
	}
	for i, c := range all {
		c.ID = fmt.Sprintf("R%d", i+1)
	}
	return all
}

// TallyClaim turns votes into a status: a majority of all voters must find the
// claim real, and most of those must find it worth acting on.
func TallyClaim(c *RiskClaim, voters int) {
	if c.Status == "skipped" {
		return
	}
	if voters == 0 {
		c.Status = "unverified"
		return
	}
	real, worth := 0, 0
	for _, v := range c.Votes {
		if v.Real {
			real++
			if v.Severity != "nit" {
				worth++
			}
		}
	}
	switch {
	case real*2 <= voters:
		c.Status = "rejected"
		for _, v := range c.Votes {
			if !v.Real {
				c.Reject = clampWords(v.Reason, 30)
				break
			}
		}
	case worth*2 <= real:
		c.Status, c.Reject = "rejected", "real but not worth acting on"
	default:
		c.Status = "confirmed"
	}
}

var riskVerifyText = map[string]string{
	"drive_by":  "This change is unrelated to the PR's stated purpose (title, description, linked issues): a drive-by edit. real = true only if the change is not needed for the PR's purpose AND the description does not mention it.",
	"rollback":  "This change is hard to undo: reverting the code would not restore the previous state (data destroyed or rewritten, a schema change old code cannot read, a published API or message format others now depend on, an irreversible external call). real = true only if a plain revert-and-redeploy would leave lasting damage.",
	"error_gap": "At this line a failure would go unseen in production: an error is swallowed, ignored, or turned into a default value without being logged, reported or returned. real = true only if a real failure here would leave no trace an operator could find.",
}

func verifyRiskTask(l Lens, c RiskClaim) string {
	var b strings.Builder
	b.WriteString("## Your task: verify one merge-risk claim, independently\n")
	b.WriteString("Another agent made this claim about the PR. Such claims are often wrong or exaggerated; decide for yourself from the code and the PR description.\n")
	fmt.Fprintf(&b, "Your method: %s\n\n", l.Focus)
	fmt.Fprintf(&b, "<claim>\nkind: %s\nwhat it means: %s\ntitle: %s\nlocation: %s:%d\ncode: %s\nwhy: %s\n</claim>\n", c.Kind, riskVerifyText[c.Kind], c.Title, c.File, c.Line, c.Quote, c.Why)
	b.WriteString(`
Read the code and decide:
- real: as defined above.
- introduced: true if this PR's own change causes it.
- severity: important (a reviewer must act), minor (worth noting), nit (not worth it).
- reason: ≤ 40 words citing what you read (file:line).
Finish by calling submit_verdict.`)
	return b.String()
}

// BuildMergeRisk scores the PR on five signals. Alignment, rollback and error
// detection come from the architect's voted claims; impact and verification are
// computed from the diff, the path rules and the units' proof.
func BuildMergeRisk(r *Review, d *diff.Diff, rules []risk.Rule, plan *scope.Plan, verifyExit int, verifyCmd string) *MergeRisk {
	var ri *RiskInput
	if r.Design != nil && r.Design.Risk != nil {
		ri = r.Design.Risk
	}
	confirmed := func(cs []RiskClaim) (n int) {
		for _, c := range cs {
			if c.Status == "confirmed" || c.Status == "unverified" {
				n++
			}
		}
		return
	}
	var sigs []Signal

	// 1. Alignment: drive-by edits plus "done when" claims the PR does not meet.
	notDone := 0
	for _, u := range r.Units {
		for _, c := range u.Checks {
			if c.Status == "not_done" {
				notDone++
			}
		}
	}
	al := Signal{Key: "alignment", Name: "Alignment", Level: -1, Grade: "not assessed"}
	if ri != nil {
		n := confirmed(ri.Alignment.DriveBy)
		model := map[string]int{"aligned": 0, "some_drift": 1, "drifted": 2}[ri.Alignment.Grade]
		al.Level = min(model, 1)
		if n > 0 {
			al.Level = min(2, n)
		}
		al.Reason, al.Claims = ri.Alignment.Reason, ri.Alignment.DriveBy
	}
	if notDone > 0 {
		al.Level = max(al.Level, min(2, notDone))
		al.Reason = strings.TrimSpace(fmt.Sprintf("%d stated requirement%s not met. %s", notDone, plural(notDone), al.Reason))
	}
	// Without the architect nobody looked for drive-by edits: say so, never "aligned".
	if al.Level >= 0 {
		al.Grade = []string{"aligned", "some drift", "drifted"}[al.Level]
		if al.Reason == "" {
			al.Reason = "Matches the PR's stated purpose."
		}
	}
	sigs = append(sigs, al)

	// 2. Impact: sensitive paths, size, and reach outside the diff.
	lines, files := 0, 0
	var paths []string
	for _, f := range d.Files {
		if plan.Of(f.Path).Kind != scope.Code {
			continue
		}
		lines += f.Adds + f.Dels
		files++
		paths = append(paths, f.Path)
	}
	blast := 0
	for _, u := range r.Units {
		blast += len(u.Blast)
	}
	im := Signal{Key: "impact", Name: "Impact"}
	var why []string
	if floor, reason := risk.Floor(rules, paths); floor == risk.Human {
		im.Level = 2
		why = append(why, "touches "+shortPaths(reason))
	}
	if lines > 400 || files > 15 {
		im.Level = max(im.Level, 1)
	}
	for _, u := range r.Units {
		if u.Risk == "high" {
			im.Level = max(im.Level, 1)
		}
	}
	why = append(why, fmt.Sprintf("%d changed lines in %d files", lines, files))
	if blast > 0 {
		why = append(why, fmt.Sprintf("%d effects outside the diff", blast))
	}
	im.Grade = []string{"small", "medium", "large"}[im.Level]
	im.Reason = capFirst(strings.Join(why, " · ")) + "."
	sigs = append(sigs, im)

	// 3. Verification: does each behaviour change come with the proof its kind needs.
	ve := Signal{Key: "verification", Name: "Verification"}
	prov, part, miss, humanMiss := 0, 0, 0, 0
	for _, u := range r.Units {
		if u.Tier == "static" {
			continue
		}
		switch u.Evidence.Status {
		case "provided":
			prov++
		case "partial":
			part++
		default:
			miss++
			if u.Tier == "human" {
				humanMiss++
			}
		}
	}
	tot := prov + part + miss
	switch {
	case tot == 0 || miss+part == 0:
		ve.Level = 0
	case humanMiss > 0 || miss*2 > tot:
		ve.Level = 2
	default:
		ve.Level = 1
	}
	ve.Reason = fmt.Sprintf("%d of %d changes have proof", prov, tot)
	if part > 0 {
		ve.Reason += fmt.Sprintf(", %d partial", part)
	}
	ve.Reason += "."
	if verifyCmd != "" && verifyExit != 0 {
		ve.Level = 2
		ve.Reason += fmt.Sprintf(" `%s` failed (exit %d).", verifyCmd, verifyExit)
	}
	ve.Grade = []string{"good", "partial", "poor"}[ve.Level]
	sigs = append(sigs, ve)

	// 4. Rollback cost: a two-way door or a one-way door.
	rb := Signal{Key: "rollback", Name: "Rollback cost", Level: -1, Grade: "not assessed"}
	if ri != nil {
		x := ri.Rollback
		rb.Reason = x.Reason
		switch {
		case x.Grade == "two_way":
			rb.Level = 0
		case x.Kind != "rollback": // costly, no line to point at
			rb.Level = 1
		case x.Status == "rejected":
			rb.Level, rb.Reason = 0, "A plain revert is enough: the verifiers rejected the one-way-door claim."
		case x.Grade == "one_way":
			rb.Level = 2
		default:
			rb.Level = 1
		}
		if x.Kind == "rollback" {
			rb.Claims = []RiskClaim{x.RiskClaim}
		}
		if x.Steps != "" && rb.Level > 0 {
			rb.Reason += " Roll back: " + x.Steps
		}
		rb.Grade = []string{"low", "medium", "high"}[rb.Level]
		if rb.Reason == "" {
			rb.Reason = "A plain revert undoes it."
		}
	}
	sigs = append(sigs, rb)

	// 5. Error detection: would a failure on the new paths be seen?
	er := Signal{Key: "errors", Name: "Error detection", Level: -1, Grade: "not assessed"}
	if ri != nil {
		n := confirmed(ri.Errors.Gaps)
		model := map[string]int{"good": 0, "decent": 1, "poor": 2}[ri.Errors.Grade]
		er.Level = min(model, 1)
		if n > 0 {
			er.Level = min(2, n)
		}
		er.Reason, er.Claims = ri.Errors.Reason, ri.Errors.Gaps
		er.Grade = []string{"good", "decent", "poor"}[er.Level]
	}
	sigs = append(sigs, er)

	// Overall: weighted, with floors for one-way doors and sensitive code without proof.
	w := map[string]int{"impact": 3, "rollback": 3, "verification": 2, "alignment": 2, "errors": 1}
	num, den := 0, 0
	lv := map[string]int{}
	for _, s := range sigs {
		lv[s.Key] = s.Level
		if s.Level < 0 {
			continue
		}
		num += w[s.Key] * s.Level
		den += w[s.Key] * 2
	}
	mr := &MergeRisk{Signals: sigs}
	if den > 0 {
		mr.Score = num * 100 / den
	}
	switch {
	case mr.Score >= 55:
		mr.Overall = "high"
	case mr.Score >= 25:
		mr.Overall = "medium"
	default:
		mr.Overall = "low"
	}
	if (lv["impact"] == 2 || lv["rollback"] == 2) && mr.Overall == "low" {
		mr.Overall = "medium"
	}
	if (lv["impact"] == 2 || lv["rollback"] == 2) && lv["verification"] == 2 {
		mr.Overall = "high"
	}

	// What would lower it, in the article's order of payoff.
	if lv["alignment"] >= 1 && ri != nil && confirmed(ri.Alignment.DriveBy) > 0 {
		mr.Lower = append(mr.Lower, "Move the unrelated changes to their own PR.")
	}
	if lv["verification"] >= 1 {
		mr.Lower = append(mr.Lower, "Add the missing proof: tests, or before/after output for user-facing changes.")
	}
	if lv["errors"] >= 1 {
		mr.Lower = append(mr.Lower, "Log or report failures on the new paths instead of swallowing them.")
	}
	if lv["rollback"] == 2 {
		mr.Lower = append(mr.Lower, "Make it reversible (e.g. expand first, remove later) or write down the rollback steps.")
	}
	if lines > 400 && lv["impact"] >= 1 {
		mr.Lower = append(mr.Lower, "Split it into smaller stacked PRs.")
	}

	var worst []string
	for _, s := range sigs {
		if s.Level == 2 {
			worst = append(worst, strings.ToLower(s.Name))
		}
	}
	switch {
	case len(worst) > 0:
		mr.Summary = "Look closest at " + strings.Join(worst, " and ") + "."
	case mr.Overall == "low":
		mr.Summary = "Low risk: a normal review is enough."
	default:
		mr.Summary = "Some risk: check the signals marked to watch."
	}
	return mr
}

// shortPaths turns "auth code (src/a/b/login.go)" into "auth code (login.go)".
func shortPaths(s string) string {
	i, j := strings.LastIndex(s, "("), strings.LastIndex(s, ")")
	if i < 0 || j < i {
		return s
	}
	p := s[i+1 : j]
	if k := strings.LastIndex(p, "/"); k >= 0 {
		p = p[k+1:]
	}
	return s[:i+1] + p + s[j:]
}

func plural(n int) string {
	if n == 1 {
		return " is"
	}
	return "s are"
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// AutoDepth picks the review depth from what changed: no reviewable code →
// quick; sensitive paths or a large change → deep; otherwise standard.
// In a re-review round only the files changed since the last round count.
func AutoDepth(d *diff.Diff, rules []risk.Rule, only map[string]bool, plan *scope.Plan) (string, string) {
	lines := codeLines(d, only, plan, scope.Code)
	var paths []string
	for _, f := range d.Files {
		if k := plan.Of(f.Path).Kind; (k != scope.Code && k != scope.Test) || f.Status == "binary" || (only != nil && !only[f.Path]) {
			continue
		}
		paths = append(paths, f.Path)
	}
	if len(paths) == 0 {
		return "quick", "no code to hunt bugs in (docs, lockfiles or assets only)"
	}
	if floor, why := risk.Floor(rules, paths); floor == risk.Human {
		return "deep", "touches " + shortPaths(why)
	}
	if lines > 600 {
		return "deep", fmt.Sprintf("%d changed lines of code", lines)
	}
	return "standard", fmt.Sprintf("%d changed lines of code, no sensitive paths", lines)
}

// codeLines counts changed lines in files of the given kinds (no docs,
// lockfiles or binaries); with only set, just those files (a re-review round).
func codeLines(d *diff.Diff, only map[string]bool, plan *scope.Plan, kinds ...scope.Kind) int {
	n := 0
	for _, f := range d.Files {
		if !slicesContains(kinds, plan.Of(f.Path).Kind) || f.Status == "binary" || (only != nil && !only[f.Path]) {
			continue
		}
		n += f.Adds + f.Dels
	}
	return n
}

func slicesContains[T comparable](xs []T, x T) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
