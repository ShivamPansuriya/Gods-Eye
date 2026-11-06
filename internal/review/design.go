package review

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

// Design is the architect agent's answer to three reviewer questions:
// how is this built, what happens step by step, and is the ground it stands on
// (existing code, conventions) right.
type Design struct {
	Architecture Architecture `json:"architecture"`
	Flows        []CodeFlow   `json:"flows"`
	Practices    []Practice   `json:"practices"`
	Foundations  Foundations  `json:"foundations"`
	Risk         *RiskInput   `json:"risk,omitempty"` // merge-risk claims; grounded, voted, then scored into Review.MergeRisk
}

// Architecture is the component map of the change.
type Architecture struct {
	Summary    string      `json:"summary"`  // how the change is structured
	Approach   string      `json:"approach"` // how the author implemented it
	Components []Component `json:"components"`
	Links      []Link      `json:"links"`
}

// Component is one box on the architecture map.
type Component struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Layer   string   `json:"layer"` // ui | api | service | domain | data | infra | external | test
	Kind    string   `json:"kind"`  // added | changed | removed | same
	Files   []string `json:"files"`
	Role    string   `json:"role"`
	Members []string `json:"members"` // UML-style: "+ sectionOf(relation, records)"
}

// Link is a dependency or call between components.
type Link struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// CodeFlow is a step-by-step sequence through the code, played back on the page.
type CodeFlow struct {
	Title   string     `json:"title"`
	Trigger string     `json:"trigger"`
	Lanes   []Lane     `json:"lanes"`
	Steps   []FlowStep `json:"steps"`
}

// Lane is one participant in a flow.
type Lane struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// FlowStep is one arrow in a flow.
type FlowStep struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Action   string `json:"action"`
	Detail   string `json:"detail"`
	Kind     string `json:"kind"` // added | changed | removed | same
	File     string `json:"file"`
	Line     int    `json:"line"`
	Quote    string `json:"quote"`
	Hunk     string `json:"hunk,omitempty"`
	Grounded bool   `json:"grounded"`
}

// Practice is a repo convention the PR follows or breaks.
type Practice struct {
	ID        string `json:"id"`
	Practice  string `json:"practice"`
	Status    string `json:"status"` // followed | broken | mixed
	File      string `json:"file"`
	Line      int    `json:"line"`
	Quote     string `json:"quote"`
	Reference string `json:"reference"` // where the convention is written down or shown
	Detail    string `json:"detail"`
	Hunk      string `json:"hunk,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Round     int    `json:"round"`
	State     string `json:"state"` // open | resolved
}

// Foundations is the verdict on the existing design the PR builds on.
type Foundations struct {
	Verdict string       `json:"verdict"` // sound | questionable | flawed
	Summary string       `json:"summary"`
	Items   []Foundation `json:"items"`
}

// Foundation is one problem in the existing code or design.
type Foundation struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Problem  string `json:"problem"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Quote    string `json:"quote"`
	FixOld   string `json:"fix_old"`
	Adopt    string `json:"adopt"` // now | follow_up | no
	AdoptHow string `json:"adopt_how"`
	Effort   string `json:"effort"` // small | medium | large
	Risk     string `json:"risk_if_ignored"`

	Votes  []Vote `json:"votes,omitempty"`
	Status string `json:"status"` // confirmed | rejected | unverifiable | resolved | skipped
	Round  int    `json:"round"`
	Why    string `json:"why,omitempty"`
}

// Attribution says who wrote the change, from commit trailers and the description.
type Attribution struct {
	Agent    bool   `json:"agent"`
	Tool     string `json:"tool,omitempty"`
	Reviewed string `json:"reviewed"` // yes | no | unknown: did the human author say they reviewed it
	Evidence string `json:"evidence,omitempty"`
}

var (
	agentRe    = regexp.MustCompile(`(?i)(co-authored-by:\s*(claude|copilot|cursor|codex|devin|gemini|chatgpt|gpt|aider|windsurf|amp|jules)[^\n]*|generated with \[?(claude code|cursor|copilot|codex)|written by an ai agent)`)
	reviewedRe = regexp.MustCompile(`(?i)has reviewed it|i reviewed|reviewed by me|self-reviewed`)
	notRevRe   = regexp.MustCompile(`(?i)has not reviewed it|not reviewed yet|unreviewed`)
)

// DetectAttribution looks for agent co-authorship and a review statement.
func DetectAttribution(text string) Attribution {
	a := Attribution{Reviewed: "unknown"}
	if m := agentRe.FindStringSubmatch(text); m != nil {
		a.Agent = true
		a.Evidence = strings.TrimSpace(m[0])
		for _, g := range m[2:] {
			if g != "" {
				a.Tool = strings.ToLower(g)
				break
			}
		}
	}
	switch {
	case notRevRe.MatchString(text):
		a.Reviewed = "no"
	case reviewedRe.MatchString(text):
		a.Reviewed = "yes"
	}
	return a
}

const designTask = `## Your task: explain how this PR is built, and judge the ground it stands on
You are the architect on the team. The reviewer asks four questions; answer each from code you read (use the tools: read the changed files in full, the code they call, and one or two existing places that do the same kind of thing).

### 1. How is it built? (architecture)
- summary ≤ 40 words: how the change is structured.
- approach ≤ 40 words: how the author implemented it: which pattern, where the logic lives, what they reused or added.
- components: 4-14 boxes for a diagram. Each: id (short slug), name (class, module or endpoint, ≤ 4 words), layer (ui, api, service, domain, data, infra, external, test), kind (added, changed, removed, or same for unchanged neighbours that matter), files (repo paths), role ≤ 15 words, members: up to 6 UML-style lines for the important fields/methods this PR adds or changes ("+ sectionOf(relation, records): SectionRest", "- maxReads: int").
- links: calls or dependencies between components: from, to, label ≤ 5 words, kind (added, changed, removed, same).

### 2. What happens, step by step? (code flows)
1-3 flows for the behaviours a reviewer most needs to understand, as sequences: title ≤ 8 words, trigger ≤ 15 words (what starts it: "A user opens a Summary with sections"), lanes (participants: browser, controller, service, database, …; 2-7), steps (4-12): from lane, to lane (same lane = internal work), action ≤ 10 words, detail ≤ 25 words, kind (added = new in this PR, removed = happened before but no longer, changed, same), and where it happens: file, line, quote (the exact code line at that location; it is checked). Include removed steps so the page can replay before and after.

### 3. Does it follow this repo's practices?
3-8 practices that matter for this change, judged against how THIS repo does things: its guidance files (CLAUDE.md, AGENTS.md, CODING_STANDARDS.md, REVIEW.md, docs) and existing code that solves similar problems. Each: practice ≤ 10 words, status (followed, broken, mixed), file + line + quote in the PR's code that shows it, reference = where the convention lives ("CODING_STANDARDS.md: single owner" or "same as ExportJobService.submit"), detail ≤ 30 words. Prefer real conventions over generic advice; no style nits.

### 4. Is the existing design right?
Look at the existing code and design this PR builds on or extends (not the PR's own new code). Is it correct? Report real problems in it: wrong abstractions, duplicated ownership, missing checks, unsafe patterns the PR copies or depends on. For each: title ≤ 10 words, problem ≤ 40 words, file + line + quote of the EXISTING code (must exist at the PR head), fix_old ≤ 30 words (how to fix the old code), adopt (now = this PR should include the fix because it touches or copies the problem; follow_up = fix separately; no = not worth it), adopt_how ≤ 30 words (how the new code could incorporate the fix), effort (small, medium, large), risk_if_ignored ≤ 20 words. Give foundations.verdict (sound, questionable, flawed) and summary ≤ 40 words. Zero items is a valid answer when the base is sound. At most 6 items; each is checked by independent verifiers.

### 5. How risky is it to merge? (risk)
- alignment: does the code do what the title, description and linked issues say, and only that? grade (aligned, some_drift, drifted), reason ≤ 25 words, drive_by: changes the PR's purpose does not need and the description does not mention (renames, unrelated fixes, reformatting of untouched logic): title ≤ 10 words, file + line + quote, why ≤ 30 words. At most 4; zero is normal.
- rollback: if this is deployed and goes wrong, can a plain revert undo it? grade: two_way (revert and redeploy fully undoes it), costly (revert works but needs extra steps: cache flush, re-run a job, config change), one_way (destructive migration, data rewritten or deleted, a published API or message format others will depend on, irreversible external calls). reason ≤ 25 words, steps ≤ 30 words (how to roll back), and for costly/one_way the line that makes it so: title, file, line, quote, why. For two_way leave title/file/quote empty and line 0.
- errors: if the new code fails in production, will anyone see it? grade (good: failures are returned, logged or reported; decent: mostly; poor: failures are swallowed or invisible), reason ≤ 25 words, gaps: places in the PR's NEW code where a failure is swallowed, ignored, or turned into a default without a log or report: title, file + line + quote, why. At most 4.
Every quoted line is checked against the code, and every drive_by, gap and costly/one_way rollback claim is checked by independent verifiers.

Plain words, short sentences. Finish by calling submit_design.`

func designTaskWith(known []string) string {
	if len(known) == 0 {
		return designTask
	}
	return designTask + "\n\nAlready reported in earlier review rounds (do not repeat; report only new problems):\n- " + strings.Join(known, "\n- ")
}

func verifyFoundationTask(l Lens, f Foundation) string {
	var b strings.Builder
	b.WriteString("## Your task: verify one claim about the EXISTING design, independently\n")
	b.WriteString("Another agent claims the code this PR builds on has a design problem. Such claims are often wrong or exaggerated; decide for yourself from the code.\n")
	fmt.Fprintf(&b, "Your method: %s\n\n", l.Focus)
	fmt.Fprintf(&b, "<claim>\ntitle: %s\nlocation: %s:%d\ncode: %s\nproblem: %s\nsuggested fix: %s\n</claim>\n", f.Title, f.File, f.Line, f.Quote, f.Problem, f.FixOld)
	b.WriteString(`
Read the code and decide:
- real: true only if the problem is genuine: it causes bugs, inconsistency, duplicated ownership, or makes changes like this PR riskier. Matters of taste → false.
- introduced: false (this is about existing code); set true only if the PR itself created the problem.
- severity: important (worth fixing soon), minor (worth a follow-up), nit (not worth it).
- reason: ≤ 40 words citing what you read (file:line).
Finish by calling submit_verdict.`)
	return b.String()
}

const designSchemaJSON = `{
  "type": "object",
  "required": ["architecture","flows","practices","foundations","risk"],
  "properties": {
    "architecture": {
      "type": "object",
      "required": ["summary","approach","components","links"],
      "properties": {
        "summary": {"type": "string"},
        "approach": {"type": "string"},
        "components": {"type": "array", "items": {"type": "object", "required": ["id","name","layer","kind","files","role","members"],
          "properties": {"id": {"type": "string"}, "name": {"type": "string"},
            "layer": {"type": "string", "enum": ["ui","api","service","domain","data","infra","external","test"]},
            "kind": {"type": "string", "enum": ["added","changed","removed","same"]},
            "files": {"type": "array", "items": {"type": "string"}}, "role": {"type": "string"},
            "members": {"type": "array", "items": {"type": "string"}}}}},
        "links": {"type": "array", "items": {"type": "object", "required": ["from","to","label","kind"],
          "properties": {"from": {"type": "string"}, "to": {"type": "string"}, "label": {"type": "string"},
            "kind": {"type": "string", "enum": ["added","changed","removed","same"]}}}}
      }
    },
    "flows": {"type": "array", "items": {"type": "object", "required": ["title","trigger","lanes","steps"],
      "properties": {"title": {"type": "string"}, "trigger": {"type": "string"},
        "lanes": {"type": "array", "items": {"type": "object", "required": ["id","name"], "properties": {"id": {"type": "string"}, "name": {"type": "string"}}}},
        "steps": {"type": "array", "items": {"type": "object", "required": ["from","to","action","detail","kind","file","line","quote"],
          "properties": {"from": {"type": "string"}, "to": {"type": "string"}, "action": {"type": "string"}, "detail": {"type": "string"},
            "kind": {"type": "string", "enum": ["added","changed","removed","same"]},
            "file": {"type": "string"}, "line": {"type": "integer"}, "quote": {"type": "string"}}}}}}},
    "practices": {"type": "array", "items": {"type": "object", "required": ["practice","status","file","line","quote","reference","detail"],
      "properties": {"practice": {"type": "string"}, "status": {"type": "string", "enum": ["followed","broken","mixed"]},
        "file": {"type": "string"}, "line": {"type": "integer"}, "quote": {"type": "string"},
        "reference": {"type": "string"}, "detail": {"type": "string"}}}},
    "foundations": {"type": "object", "required": ["verdict","summary","items"],
      "properties": {"verdict": {"type": "string", "enum": ["sound","questionable","flawed"]}, "summary": {"type": "string"},
        "items": {"type": "array", "items": {"type": "object",
          "required": ["title","problem","file","line","quote","fix_old","adopt","adopt_how","effort","risk_if_ignored"],
          "properties": {"title": {"type": "string"}, "problem": {"type": "string"}, "file": {"type": "string"}, "line": {"type": "integer"},
            "quote": {"type": "string"}, "fix_old": {"type": "string"},
            "adopt": {"type": "string", "enum": ["now","follow_up","no"]}, "adopt_how": {"type": "string"},
            "effort": {"type": "string", "enum": ["small","medium","large"]}, "risk_if_ignored": {"type": "string"}}}}}},
    "risk": {"type": "object", "required": ["alignment","rollback","errors"],
      "properties": {
        "alignment": {"type": "object", "required": ["grade","reason","drive_by"], "properties": {
          "grade": {"type": "string", "enum": ["aligned","some_drift","drifted"]}, "reason": {"type": "string"},
          "drive_by": {"type": "array", "items": {"type": "object", "required": ["title","file","line","quote","why"], "properties": {"title": {"type": "string"}, "file": {"type": "string"}, "line": {"type": "integer"}, "quote": {"type": "string"}, "why": {"type": "string"}}}}}},
        "rollback": {"type": "object", "required": ["grade","reason","steps","title","file","line","quote","why"], "properties": {
          "grade": {"type": "string", "enum": ["two_way","costly","one_way"]}, "reason": {"type": "string"}, "steps": {"type": "string"},
          "title": {"type": "string"}, "file": {"type": "string"}, "line": {"type": "integer"}, "quote": {"type": "string"}, "why": {"type": "string"}}},
        "errors": {"type": "object", "required": ["grade","reason","gaps"], "properties": {
          "grade": {"type": "string", "enum": ["good","decent","poor"]}, "reason": {"type": "string"},
          "gaps": {"type": "array", "items": {"type": "object", "required": ["title","file","line","quote","why"], "properties": {"title": {"type": "string"}, "file": {"type": "string"}, "line": {"type": "integer"}, "quote": {"type": "string"}, "why": {"type": "string"}}}}}}}}
  }
}`

// DesignSchema is the input schema of submit_design.
func DesignSchema() map[string]any { return mustMap(designSchemaJSON) }

// DesignStats counts what grounding removed.
type DesignStats struct {
	StepsGrounded   int `json:"stepsGrounded"`
	StepsTotal      int `json:"stepsTotal"`
	PracticesKept   int `json:"practicesKept"`
	PracticesDrop   int `json:"practicesDropped"`
	FoundationsKept int `json:"foundationsKept"`
	FoundationsDrop int `json:"foundationsDropped"`
}

// GroundDesign checks every cited line against the code, drops claims whose
// quoted code does not exist, repairs line numbers, and cleans the diagram so
// it never points at a box that is not there.
func GroundDesign(ds *Design, d *diff.Diff, ws *workspace.Workspace, round int) DesignStats {
	var st DesignStats
	a := &ds.Architecture
	a.Summary, a.Approach = clampWords(a.Summary, 50), clampWords(a.Approach, 50)

	// Components: unique ids, files that exist, short text.
	ids := map[string]bool{}
	comps := a.Components[:0]
	for _, c := range a.Components {
		c.ID = slug(c.ID, c.Name)
		if ids[c.ID] || c.Name == "" {
			continue
		}
		ids[c.ID] = true
		c.Name, c.Role = clampWords(c.Name, 6), clampWords(c.Role, 18)
		var files []string
		for _, f := range c.Files {
			if fileExists(f, d, ws) {
				files = append(files, f)
			}
		}
		c.Files = files
		if len(c.Members) > 8 {
			c.Members = c.Members[:8]
		}
		c.Kind = oneOf(c.Kind, "same", "added", "changed", "removed", "same")
		c.Layer = oneOf(c.Layer, "service", "ui", "api", "service", "domain", "data", "infra", "external", "test")
		comps = append(comps, c)
	}
	a.Components = comps
	links := a.Links[:0]
	for _, l := range a.Links {
		l.From, l.To = slug(l.From, ""), slug(l.To, "")
		if ids[l.From] && ids[l.To] && l.From != l.To {
			l.Label = clampWords(l.Label, 6)
			l.Kind = oneOf(l.Kind, "same", "added", "changed", "removed", "same")
			links = append(links, l)
		}
	}
	a.Links = links

	// Flows: lanes exist for every step; steps grounded when their quote is real.
	flows := ds.Flows[:0]
	for _, f := range ds.Flows {
		if len(f.Steps) == 0 {
			continue
		}
		f.Title, f.Trigger = clampWords(f.Title, 10), clampWords(f.Trigger, 20)
		lane := map[string]bool{}
		lanes := f.Lanes[:0]
		for _, l := range f.Lanes {
			l.ID = slug(l.ID, l.Name)
			if !lane[l.ID] {
				lane[l.ID] = true
				lanes = append(lanes, l)
			}
		}
		for i := range f.Steps {
			s := &f.Steps[i]
			s.From, s.To = slug(s.From, ""), slug(s.To, "")
			for _, id := range []string{s.From, s.To} {
				if id != "" && !lane[id] {
					lane[id] = true
					lanes = append(lanes, Lane{ID: id, Name: id})
				}
			}
			s.Action, s.Detail = clampWords(s.Action, 12), clampWords(s.Detail, 30)
			s.Kind = oneOf(s.Kind, "same", "added", "changed", "removed", "same")
			st.StepsTotal++
			if s.File != "" && s.Quote != "" {
				if ln, ok := locate(s.File, s.Quote, s.Line, d, ws); ok {
					s.Line, s.Grounded = ln, true
					s.Hunk = hunkAt(d, s.File, ln)
					st.StepsGrounded++
				}
			}
		}
		f.Lanes = lanes
		if len(f.Lanes) > 8 {
			continue
		}
		flows = append(flows, f)
	}
	if len(flows) > 3 {
		flows = flows[:3]
	}
	ds.Flows = flows

	// Practices and foundations are claims: they need real code behind them.
	ps := ds.Practices[:0]
	for _, p := range ds.Practices {
		ln, ok := locate(p.File, p.Quote, p.Line, d, ws)
		if !ok {
			st.PracticesDrop++
			continue
		}
		p.Line, p.Hunk = ln, hunkAt(d, p.File, ln)
		p.Practice, p.Detail, p.Reference = clampWords(p.Practice, 12), clampWords(p.Detail, 36), clampWords(p.Reference, 14)
		p.Status = oneOf(p.Status, "mixed", "followed", "broken", "mixed")
		p.Round, p.State = round, "open"
		ps = append(ps, p)
	}
	ds.Practices = ps
	for i := range ds.Practices {
		ds.Practices[i].ID = fmt.Sprintf("P%d", i+1)
	}
	st.PracticesKept = len(ps)

	fo := &ds.Foundations
	fo.Summary = clampWords(fo.Summary, 50)
	fo.Verdict = oneOf(fo.Verdict, "questionable", "sound", "questionable", "flawed")
	items := fo.Items[:0]
	for _, f := range fo.Items {
		ln, ok := locate(f.File, f.Quote, f.Line, d, ws)
		if !ok {
			st.FoundationsDrop++
			continue
		}
		f.Line = ln
		f.Title, f.Problem = clampWords(f.Title, 12), clampWords(f.Problem, 50)
		f.FixOld, f.AdoptHow, f.Risk = clampWords(f.FixOld, 36), clampWords(f.AdoptHow, 36), clampWords(f.Risk, 24)
		f.Adopt = oneOf(f.Adopt, "follow_up", "now", "follow_up", "no")
		f.Effort = oneOf(f.Effort, "medium", "small", "medium", "large")
		f.Round = round
		items = append(items, f)
	}
	if len(items) > 6 {
		items = items[:6]
	}
	fo.Items = items
	for i := range fo.Items {
		fo.Items[i].ID = fmt.Sprintf("D%d", i+1)
	}
	st.FoundationsKept = len(items)
	return st
}

// TallyFoundation turns votes into a status (majority of all voters must agree).
func TallyFoundation(f *Foundation, voters int) {
	if f.Status == "skipped" || f.Status == "resolved" {
		return
	}
	if voters == 0 {
		f.Status = "unverified"
		return
	}
	real, worth := 0, 0
	for _, v := range f.Votes {
		if v.Real {
			real++
			if v.Severity != "nit" {
				worth++
			}
		}
	}
	switch {
	case real*2 <= voters:
		f.Status = "rejected"
		for _, v := range f.Votes {
			if !v.Real {
				f.Why = v.Reason
				break
			}
		}
	case worth*2 <= real:
		f.Status, f.Why = "rejected", "real but not worth changing"
	default:
		f.Status = "confirmed"
	}
}

// CarryDesign brings earlier rounds' design items forward: still present →
// kept open, quoted code gone → resolved. New items at the same place are dropped.
func CarryDesign(ds *Design, prev *State, ws *workspace.Workspace) {
	if prev == nil || ws == nil {
		return
	}
	var carriedF []Foundation
	for _, f := range prev.Foundations {
		if _, ok := ws.Contains(f.File, f.Quote); ok {
			carriedF = append(carriedF, f)
		} else {
			f.Status, f.Why = "resolved", "the old code was changed"
			carriedF = append(carriedF, f)
		}
	}
	var fresh []Foundation
	for _, f := range ds.Foundations.Items {
		dup := false
		for _, c := range carriedF {
			if c.File == f.File && absInt(c.Line-f.Line) <= 3 {
				dup = true
			}
		}
		if !dup {
			fresh = append(fresh, f)
		}
	}
	ds.Foundations.Items = append(carriedF, fresh...)
	for i := range ds.Foundations.Items {
		ds.Foundations.Items[i].ID = fmt.Sprintf("D%d", i+1)
	}

	var carriedP []Practice
	for _, p := range prev.Practices {
		if _, ok := ws.Contains(p.File, p.Quote); ok {
			carriedP = append(carriedP, p)
		} else {
			p.State = "resolved"
			carriedP = append(carriedP, p)
		}
	}
	var freshP []Practice
	for _, p := range ds.Practices {
		dup := false
		for _, c := range carriedP {
			if c.File == p.File && absInt(c.Line-p.Line) <= 3 {
				dup = true
			}
		}
		if !dup {
			freshP = append(freshP, p)
		}
	}
	ds.Practices = append(carriedP, freshP...)
	for i := range ds.Practices {
		ds.Practices[i].ID = fmt.Sprintf("P%d", i+1)
	}
}

// AttachDesignUnits links practices to the unit that owns their hunk.
func AttachDesignUnits(r *Review) {
	if r.Design == nil {
		return
	}
	unitOf := map[string]string{}
	for _, u := range r.Units {
		for _, h := range u.Hunks {
			unitOf[h] = u.ID
		}
	}
	for i := range r.Design.Practices {
		r.Design.Practices[i].Unit = unitOf[r.Design.Practices[i].Hunk]
	}
}

func locate(file, quote string, line int, d *diff.Diff, ws *workspace.Workspace) (int, bool) {
	file = strings.TrimPrefix(strings.TrimSpace(file), "./")
	if strings.TrimSpace(quote) == "" || file == "" {
		return 0, false
	}
	if ws != nil {
		if ln, ok := ws.NearestLine(file, quote, line); ok {
			return ln, true
		}
	}
	for _, f := range d.Files {
		if f.Path != file {
			continue
		}
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if strings.Contains(normWS(l.Text), normWS(quote)) {
					if l.New > 0 {
						return l.New, true
					}
					return line, true
				}
			}
		}
	}
	return 0, false
}

func hunkAt(d *diff.Diff, file string, line int) string {
	for _, f := range d.Files {
		if f.Path != file {
			continue
		}
		for _, h := range f.Hunks {
			if line >= h.NewStart && line < h.NewStart+max(h.NewLines, 1) {
				return h.ID
			}
		}
	}
	return ""
}

func fileExists(p string, d *diff.Diff, ws *workspace.Workspace) bool {
	for _, f := range d.Files {
		if f.Path == p {
			return true
		}
	}
	if ws != nil {
		_, err := ws.Lines(p)
		return err == nil
	}
	return false
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(id, name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(id)), "-"), "-")
	if s == "" {
		s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	}
	return s
}

func oneOf(v, def string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return def
}
