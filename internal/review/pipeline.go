package review

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/ShivamPansuriya/gods-eye/internal/agent"
	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
	"github.com/ShivamPansuriya/gods-eye/internal/source"
	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

// Options controls the pipeline.
type Options struct {
	Model     string
	Effort    string
	VerifyEff string  // effort for verifiers (their prompts share only the small header, so a different effort costs little cache)
	Depth     string  // quick | standard | deep
	Parallel  int     // concurrent API calls
	Budget    float64 // base spend limit (USD); scaled with PR size, 45% held back for checking
	VerifyCmd string  // optional command whose output is evidence (tests, build)
	Prev      *State  // previous round, for incremental re-review
	Log       func(string)
}

// No agent reads more than a bounded view of the diff, so a PR of any size is
// reviewed: the mapper and architect get an Overview of at most WholeViewChars
// (every file and hunk listed, bodies by priority), and hunters get slices of
// about SliceChars each, as many slices as the size needs (up to MaxSlices).
const (
	WholeViewChars = 400_000
	SliceChars     = 120_000
	MaxSlices      = 40
)

// Run reviews a PR end to end.
func Run(ctx context.Context, client anthropic.Client, in *source.Input, d *diff.Diff, ws *workspace.Workspace, rules []risk.Rule, opt Options) (*Review, error) {
	t0 := time.Now()
	opt.defaults()
	log := opt.Log
	pipe := &Pipeline{Depth: opt.Depth, Round: 1, Workspace: ws != nil}
	fullChars := len(d.Annotated())
	annotated, summarized := d.Overview(WholeViewChars)
	if summarized > 0 {
		pipe.Summarized = append(pipe.Summarized, fmt.Sprintf("The diff is %d characters, so the mapper and architect saw %d of %d files as a summary (every hunk listed, body read on demand with read_file). Every file is still searched by a bug hunter.", fullChars, summarized, len(d.Files)))
		log(pipe.Summarized[0])
	}

	// 1. Deterministic context: who uses the symbols this PR touches.
	callers := ""
	if ws != nil {
		syms := ws.Callers(workspace.Symbols(d), 40, 4)
		pipe.Symbols = len(syms)
		callers = workspace.FormatCallers(syms)
		log(fmt.Sprintf("Indexed callers of %d changed symbols", len(syms)))
	}

	// 2. Optional empirical evidence: run the project's tests or build.
	verifyOut := ""
	if opt.VerifyCmd != "" && ws != nil {
		log("Running " + opt.VerifyCmd + " …")
		out, code := ws.Run(ctx, opt.VerifyCmd)
		pipe.VerifyCmd, pipe.VerifyExit = opt.VerifyCmd, code
		verifyOut = fmt.Sprintf("$ %s\nexit code: %d\n%s", opt.VerifyCmd, code, tail(out, 150))
	}

	// 3. Incremental round: what changed since the last review, what is still open.
	var carried []Finding
	var deltaText string
	var deltaFiles map[string]bool
	var deltaRanges map[string][][2]int
	if p := opt.Prev; p != nil && ws != nil && p.HeadSHA != "" && p.HeadSHA != in.HeadSHA && ws.IsAncestor(p.HeadSHA, in.HeadSHA) {
		pipe.Round, pipe.Since = p.Round+1, p.HeadSHA
		if raw, err := ws.DiffRange(p.HeadSHA, in.HeadSHA); err == nil {
			if dd, err := diff.Parse(raw); err == nil {
				deltaFiles = map[string]bool{}
				deltaRanges = map[string][][2]int{}
				for _, f := range dd.Files {
					deltaFiles[f.Path] = true
					for _, h := range f.Hunks {
						deltaRanges[f.Path] = append(deltaRanges[f.Path], [2]int{h.NewStart - 5, h.NewStart + h.NewLines + 5})
					}
				}
				deltaText = clip(dd.Annotated(), 30000)
			}
		}
		for _, f := range p.Findings {
			if f.Status != "confirmed" {
				continue
			}
			if _, ok := ws.Contains(f.File, f.Quote); ok {
				carried = append(carried, f) // still there
			} else {
				f.Status, f.Why = "fixed", "the quoted code is gone at the new head"
				carried = append(carried, f)
				pipe.Fixed++
			}
		}
		log(fmt.Sprintf("Round %d: %d files changed since %s; %d earlier findings fixed", pipe.Round, len(deltaFiles), shortSHA(p.HeadSHA), pipe.Fixed))
	}

	// 3b. Depth: "auto" spends review effort where the change needs it.
	if opt.Depth == "auto" {
		opt.Depth, pipe.DepthWhy = AutoDepth(d, rules, deltaFiles)
		log(fmt.Sprintf("Depth %s: %s", opt.Depth, pipe.DepthWhy))
	}
	pipe.Depth = opt.Depth
	opt.Budget, pipe.BudgetWhy = ScaleBudget(opt.Budget, codeLines(d, deltaFiles))
	pipe.Budget = opt.Budget
	preCap := huntCap(opt.Budget)
	log(fmt.Sprintf("Budget $%.2f (%s); mapping, design and hunting may use $%.2f, the rest is kept for checking", opt.Budget, pipe.BudgetWhy, preCap))

	tools := append(agent.ReadTools(),
		agent.Tool{Name: "submit_review", Description: "Submit the structured review map. Ends the mapping task.", Schema: MapSchema(), Submit: true},
		agent.Tool{Name: "submit_findings", Description: "Submit the bugs you found (possibly none). Ends the hunting task.", Schema: FindingsSchema(), Submit: true, Strict: true},
		agent.Tool{Name: "submit_verdict", Description: "Submit your verdict on one reported bug or design claim. Ends the verification task.", Schema: VerdictSchema(), Submit: true, Strict: true},
		agent.Tool{Name: "submit_design", Description: "Submit the architecture, code flows, practices and foundations. Ends the architect task.", Schema: DesignSchema(), Submit: true},
	)
	meter := agent.NewMeter()
	overPre := func() bool { return meter.Total().CostUSD > preCap }
	gap := func(stage, what, why string) { pipe.Gaps = append(pipe.Gaps, Gap{Stage: stage, What: what, Why: why}) }
	runner := &agent.Runner{
		Client: client, Model: opt.Model, Effort: opt.Effort, System: systemPrompt,
		Shared: sharedBlock(in, d.Summary(), callers, verifyOut), WS: ws, Tools: tools,
		Meter: meter, Sem: make(chan struct{}, opt.Parallel),
	}

	// 4. Map stage first: its first streamed token means the shared prefix is cached.
	var (
		mapRes *Review
		mapErr error
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []string
	)
	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		raw, err := runner.Run(ctx, agent.Task{Stage: "map", Label: "map", Context: diffBlock(annotated, "whole PR"), Prompt: mapTask, Submit: "submit_review", MaxTurns: 8, Started: started})
		if err != nil {
			mapErr = err
			return
		}
		var r Review
		if err := decodeLenient(raw, &r); err != nil {
			mapErr = fmt.Errorf("map: %w", err)
			return
		}
		mapRes = &r
		log(fmt.Sprintf("Mapped %d units", len(r.Units)))
	}()
	<-started

	// 4b. Architect: architecture, step-by-step flows, practices, existing-design check.
	// It reads the same whole-PR block as the mapper, so its prefix is a cache read.
	var design *Design
	if opt.Depth != "quick" {
		var knownDesign []string
		if opt.Prev != nil {
			for _, f := range opt.Prev.Foundations {
				knownDesign = append(knownDesign, fmt.Sprintf("%s (%s:%d)", f.Title, f.File, f.Line))
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, err := runner.Run(ctx, agent.Task{Stage: "design", Label: "design", Context: diffBlock(annotated, "whole PR"),
				Prompt: designTaskWith(knownDesign), Submit: "submit_design", MaxTurns: 18, Over: overPre})
			var ds Design
			if err == nil {
				err = decodeLenient(raw, &ds)
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, "design: "+err.Error())
				gap("design", "design step failed: no architecture, practices, existing-design check or merge-risk claims", clampWords(err.Error(), 20))
				mu.Unlock()
				log("Design step failed: " + err.Error())
				return
			}
			design = &ds
			log(fmt.Sprintf("Design: %d components, %d flows, %d practices, %d foundation claims",
				len(ds.Architecture.Components), len(ds.Flows), len(ds.Practices), len(ds.Foundations.Items)))
		}()
	}

	// 5. Hunters, in parallel, over slices of the diff.
	var (
		cands     []Finding
		lenses    = []Lens{LensAll}
		known     []string
		huntSlcs  [][]string
		huntFiles []map[string]bool
	)
	if opt.Depth == "deep" {
		lenses = []Lens{LensLogic, LensContract}
	}
	for _, f := range carried {
		if f.Status == "confirmed" {
			known = append(known, fmt.Sprintf("%s (%s:%d)", f.Title, f.File, f.Line))
		}
	}
	if opt.Depth != "quick" {
		nSlices := min(MaxSlices, max(10, (fullChars+SliceChars-1)/SliceChars))
		huntSlcs, huntFiles = slices(d, deltaFiles, nSlices)
		pipe.Hunters = len(huntSlcs) * len(lenses)
		log(fmt.Sprintf("Hunting with %d agents (%d slices × %d lens)", pipe.Hunters, len(huntSlcs), len(lenses)))
		for si, sl := range huntSlcs {
			for _, l := range lenses {
				wg.Add(1)
				go func(si int, sl []string, l Lens) {
					defer wg.Done()
					label := fmt.Sprintf("hunt-%d-%s", si+1, l.Name)
					files := fmt.Sprintf("%d file(s)", len(huntFiles[si]))
					if overPre() {
						mu.Lock()
						gap("hunt", fmt.Sprintf("slice %d (%s, %s lens) not searched for bugs", si+1, files, l.Name), "hunting share of the budget spent")
						mu.Unlock()
						return
					}
					view, partial := d.Only(huntFiles[si]).Overview(SliceChars)
					if partial > 0 {
						mu.Lock()
						pipe.Summarized = append(pipe.Summarized, fmt.Sprintf("Hunter slice %d (%s lens) saw %d large file(s) as a summary and read them on demand.", si+1, l.Name, partial))
						mu.Unlock()
					}
					raw, err := runner.Run(ctx, agent.Task{Stage: "hunt", Label: label, Context: diffBlock(view, "your slice"),
						Prompt: huntTask(l, sl, deltaText, known), Submit: "submit_findings", MaxTurns: 14, Over: overPre})
					var out struct {
						Findings []Finding `json:"findings"`
					}
					if err == nil {
						err = json.Unmarshal(raw, &out)
					}
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, label+": "+err.Error())
						gap("hunt", fmt.Sprintf("slice %d (%s, %s lens) not searched: the hunter failed", si+1, files, l.Name), clampWords(err.Error(), 20))
						return
					}
					for _, f := range out.Findings {
						f.Source = l.Name
						f.Round = pipe.Round
						cands = append(cands, f)
					}
					log(fmt.Sprintf("%s: %d findings", label, len(out.Findings)))
				}(si, sl, l)
			}
		}
	}
	wg.Wait()
	if mapErr != nil {
		return nil, mapErr
	}
	r := mapRes

	// 6. A not_done check is a claim of a defect too: verify it the same way.
	byID := d.HunkByID()
	if opt.Depth != "quick" {
		for ui, u := range r.Units {
			for ci, c := range u.Checks {
				if c.Status != "not_done" {
					continue
				}
				h := byID[c.Evidence.Hunk]
				if h == nil {
					continue
				}
				cands = append(cands, Finding{
					Title: c.Criterion, File: h.File, Line: c.Evidence.Line, Quote: lineText(h, c.Evidence.Line),
					Severity: "important", Category: "logic", Scenario: c.Detail, Source: "check",
					Confidence: "medium", Round: pipe.Round, BugClass: "unmet requirement",
					CheckRef: fmt.Sprintf("%d:%d", ui, ci),
				})
			}
		}
	}
	pipe.Candidates = len(cands)

	// 7. Discard findings whose quoted code does not exist; place the rest.
	var kept []Finding
	for _, f := range cands {
		if !placeFinding(&f, d, ws) {
			pipe.Unverifiable++
			continue
		}
		// Re-review rounds only take findings in code that changed since the
		// last round; otherwise every round "finds" a few more old issues and
		// the review never converges.
		if deltaRanges != nil && !inRanges(deltaRanges[f.File], f.Line) {
			pipe.OutsideDelta++
			continue
		}
		kept = append(kept, f)
	}
	kept, dups := dedupe(kept)
	pipe.Duplicates = dups
	for i := range kept {
		kept[i].ID = fmt.Sprintf("F%d", i+1)
	}

	// 7b. Ground the design: every cited line must exist; carry earlier rounds.
	var riskClaims []*RiskClaim
	if design != nil {
		st := GroundDesign(design, d, ws, pipe.Round)
		pipe.DesignStats = &st
		if pipe.Round > 1 {
			CarryDesign(design, opt.Prev, ws)
		}
		if design.Risk != nil {
			riskClaims = GroundRisk(design.Risk, d, ws)
			pipe.RiskClaims = len(riskClaims)
		}
	}

	// 8. Independent verification by vote.
	voters, voteFails := 0, 0
	if opt.Depth != "quick" {
		voters = len(VerifyLenses)
	}
	pipe.Voters = voters
	nDesign := 0
	if design != nil {
		nDesign = len(design.Foundations.Items)
	}
	if voters > 0 && len(kept)+nDesign+len(riskClaims) > 0 {
		log(fmt.Sprintf("Verifying %d findings, %d design claims and %d merge-risk claims with %d independent voters each", len(kept), nDesign, len(riskClaims), voters))
		sort.SliceStable(kept, func(a, b int) bool { return confRank(kept[a].Confidence) > confRank(kept[b].Confidence) })
		var vwg sync.WaitGroup
		for i := range kept {
			if meter.Total().CostUSD > opt.Budget {
				kept[i].Status, kept[i].Why = "skipped", fmt.Sprintf("budget of $%.0f reached", opt.Budget)
				continue
			}
			for _, l := range VerifyLenses[:voters] {
				vwg.Add(1)
				go func(f *Finding, l Lens) {
					defer vwg.Done()
					raw, err := runner.Run(ctx, agent.Task{Stage: "verify", Label: "verify-" + f.ID + "-" + l.Name,
						Prompt: verifyTask(l, *f, hunkText(byID[f.Hunk])), Submit: "submit_verdict", MaxTurns: 6, Effort: opt.VerifyEff})
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err.Error())
						voteFails++
						return
					}
					var v Vote
					if err := json.Unmarshal(raw, &v); err != nil {
						errs = append(errs, err.Error())
						voteFails++
						return
					}
					v.Lens = l.Name
					f.Votes = append(f.Votes, v)
				}(&kept[i], l)
			}
		}
		// Claims about the existing design go through the same vote.
		if design != nil {
			for i := range design.Foundations.Items {
				f := &design.Foundations.Items[i]
				if f.Status == "confirmed" || f.Status == "resolved" {
					continue // carried from an earlier round
				}
				if meter.Total().CostUSD > opt.Budget {
					f.Status, f.Why = "skipped", fmt.Sprintf("budget of $%.0f reached", opt.Budget)
					continue
				}
				for _, l := range VerifyLenses[:voters] {
					vwg.Add(1)
					go func(f *Foundation, l Lens) {
						defer vwg.Done()
						raw, err := runner.Run(ctx, agent.Task{Stage: "verify", Label: "verify-" + f.ID + "-" + l.Name,
							Prompt: verifyFoundationTask(l, *f), Submit: "submit_verdict", MaxTurns: 6, Effort: opt.VerifyEff})
						mu.Lock()
						defer mu.Unlock()
						if err != nil {
							errs = append(errs, err.Error())
							voteFails++
							return
						}
						var v Vote
						if json.Unmarshal(raw, &v) == nil {
							v.Lens = l.Name
							f.Votes = append(f.Votes, v)
						}
					}(f, l)
				}
			}
		}
		// Merge-risk claims (drive-by edits, one-way doors, swallowed errors) too.
		for _, c := range riskClaims {
			if meter.Total().CostUSD > opt.Budget {
				c.Status = "skipped"
				continue
			}
			for _, l := range VerifyLenses[:voters] {
				vwg.Add(1)
				go func(c *RiskClaim, l Lens) {
					defer vwg.Done()
					raw, err := runner.Run(ctx, agent.Task{Stage: "verify", Label: "verify-" + c.ID + "-" + l.Name,
						Prompt: verifyRiskTask(l, *c), Submit: "submit_verdict", MaxTurns: 6, Effort: opt.VerifyEff})
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err.Error())
						voteFails++
						return
					}
					var v Vote
					if json.Unmarshal(raw, &v) == nil {
						v.Lens = l.Name
						c.Votes = append(c.Votes, v)
					}
				}(c, l)
			}
		}
		vwg.Wait()
		if voteFails > 0 {
			gap("verify", fmt.Sprintf("%d verifier call(s) failed", voteFails), "those claims were judged on fewer votes, and a missing vote counts against them")
		}
	}
	for _, c := range riskClaims {
		TallyClaim(c, voters)
	}
	if design != nil {
		for i := range design.Foundations.Items {
			TallyFoundation(&design.Foundations.Items[i], voters)
		}
	}
	for i := range kept {
		tally(&kept[i], voters)
	}

	// 9. Put it together.
	Finalize(r, d, rules)
	all := append(kept, carriedOpen(carried, len(kept))...)
	Merge(r, d, all)
	r.Design = design
	AttachDesignUnits(r)
	r.MergeRisk = BuildMergeRisk(r, d, rules, pipe.VerifyExit, pipe.VerifyCmd)
	att := DetectAttribution(in.Body + "\n" + strings.Join(in.Commits, "\n"))
	r.Attribution = &att
	for _, f := range r.Findings {
		switch f.Status {
		case "confirmed":
			switch {
			case !f.Introduced:
				pipe.PreExisting++
			case f.Severity == "minor":
				pipe.Minor++
			default:
				pipe.Confirmed++
			}
		case "rejected":
			pipe.Rejected++
		case "nit":
			pipe.Nits++
		}
	}
	if pipe.Round >= 3 {
		open := 0
		for _, f := range carried {
			if f.Status == "confirmed" {
				open++
			}
		}
		if open > 0 {
			pipe.Notice = fmt.Sprintf("Review round %d with %d findings still open from earlier rounds. Step back and rethink the approach instead of patching line by line.", pipe.Round, open)
		}
	}
	pipe.Errors = errs
	pipe.Seconds = time.Since(t0).Seconds()
	pipe.Usage = map[string]StageUsage{}
	for k, v := range meter.Snapshot() {
		pipe.Usage[k] = StageUsage(v)
	}
	pipe.CostUSD = meter.Total().CostUSD
	r.Pipeline = pipe
	Finish(r)
	if t := IncompleteText(pipe); t != "" {
		log(t)
	}
	log("Cost by stage:\n" + strings.TrimRight(meter.String(), "\n"))
	return r, nil
}

func (o *Options) defaults() {
	if o.Model == "" {
		o.Model = "claude-opus-5-5"
	}
	if o.Effort == "" {
		o.Effort = "high"
	}
	if o.VerifyEff == "" {
		o.VerifyEff = "medium"
	}
	if o.Depth == "" {
		o.Depth = "auto"
	}
	if o.Parallel <= 0 {
		o.Parallel = 8
	}
	if o.Budget <= 0 {
		o.Budget = 15
	}
	if o.Log == nil {
		o.Log = func(string) {}
	}
}

// slices groups reviewable files into at most n balanced slices, keeping diff
// order so related files stay together. With only set, files outside it are skipped.
func slices(d *diff.Diff, only map[string]bool, n int) ([][]string, []map[string]bool) {
	type item struct {
		desc  string
		path  string
		lines int
	}
	var items []item
	total := 0
	for _, f := range d.Files {
		if skipHunt(f.Path) || (only != nil && !only[f.Path]) || f.Status == "binary" {
			continue
		}
		ids := make([]string, len(f.Hunks))
		for i, h := range f.Hunks {
			ids[i] = h.ID
		}
		c := f.Adds + f.Dels
		items = append(items, item{fmt.Sprintf("%s (%s, +%d -%d)", f.Path, strings.Join(ids, " "), f.Adds, f.Dels), f.Path, c})
		total += c
	}
	if len(items) == 0 {
		return nil, nil
	}
	target := max(350, total/n+1)
	var out [][]string
	var files []map[string]bool
	var cur []string
	curFiles := map[string]bool{}
	size := 0
	for _, it := range items {
		if size > 0 && size+it.lines > target && len(out) < n-1 {
			out, files = append(out, cur), append(files, curFiles)
			cur, curFiles, size = nil, map[string]bool{}, 0
		}
		cur = append(cur, it.desc)
		curFiles[it.path] = true
		size += it.lines
	}
	if len(cur) > 0 {
		out, files = append(out, cur), append(files, curFiles)
	}
	return out, files
}

func skipHunt(p string) bool {
	l := strings.ToLower(p)
	for _, s := range []string{".md", ".txt", ".lock", ".sum", ".svg", ".png", ".jpg", ".gif", ".snap", "package-lock.json", "yarn.lock", "pnpm-lock.yaml"} {
		if strings.HasSuffix(l, s) {
			return true
		}
	}
	return strings.Contains(l, "/generated/") || strings.Contains(l, "/vendor/") || strings.HasPrefix(l, "docs/")
}

// placeFinding checks the quote exists, corrects the line, and finds its hunk.
func placeFinding(f *Finding, d *diff.Diff, ws *workspace.Workspace) bool {
	f.File = strings.TrimPrefix(strings.TrimSpace(f.File), "./")
	if strings.TrimSpace(f.Quote) == "" {
		f.Why = "no code quoted"
		return false
	}
	ok := false
	if ws != nil {
		if ln, found := ws.NearestLine(f.File, f.Quote, f.Line); found {
			f.Line, ok = ln, true
		}
	}
	if !ok {
		// The quote may be a deleted line (old code) or ws may be missing.
		for _, file := range d.Files {
			if file.Path != f.File {
				continue
			}
			for _, h := range file.Hunks {
				for _, l := range h.Lines {
					if strings.Contains(normWS(l.Text), normWS(f.Quote)) {
						ok = true
						if l.New > 0 {
							f.Line = l.New
						}
					}
				}
			}
		}
	}
	if !ok {
		f.Why = "quoted code not found in " + f.File
		return false
	}
	for _, file := range d.Files {
		if file.Path != f.File {
			continue
		}
		for _, h := range file.Hunks {
			if f.Line >= h.NewStart && f.Line < h.NewStart+max(h.NewLines, 1) {
				f.Hunk = h.ID
				for _, l := range h.Lines {
					if l.Kind == diff.Add && l.New == f.Line {
						f.InDiff = true
					}
				}
			}
		}
	}
	return true
}

// dedupe merges findings that point at the same place (same file, within 3
// lines, or the same quoted code). The most confident report wins.
func dedupe(fs []Finding) ([]Finding, int) {
	sort.SliceStable(fs, func(a, b int) bool {
		if fs[a].File != fs[b].File {
			return fs[a].File < fs[b].File
		}
		return fs[a].Line < fs[b].Line
	})
	var out []Finding
	dups := 0
	for _, f := range fs {
		merged := false
		for i := range out {
			o := &out[i]
			if o.File == f.File && (absInt(o.Line-f.Line) <= 3 || normWS(o.Quote) == normWS(f.Quote)) {
				dups++
				merged = true
				// Keep the hunter's finding over a check, and the more confident one.
				if (o.Source == "check" && f.Source != "check") || (o.Source == f.Source && confRank(f.Confidence) > confRank(o.Confidence)) {
					ref := o.CheckRef
					f.AlsoAt = append(f.AlsoAt, o.AlsoAt...)
					*o = f
					if o.CheckRef == "" {
						o.CheckRef = ref
					}
				} else {
					o.AlsoAt = append(o.AlsoAt, f.AlsoAt...)
					if o.CheckRef == "" {
						o.CheckRef = f.CheckRef
					}
				}
				break
			}
		}
		if !merged {
			out = append(out, f)
		}
	}
	return out, dups
}

// tally turns votes into a status. A finding needs a strict majority of all
// voters (abstentions count against it) to be confirmed.
func tally(f *Finding, voters int) {
	if f.Status == "skipped" {
		return
	}
	if voters == 0 {
		f.Status, f.Introduced = "unverified", true
		return
	}
	real, intro, important, minor := 0, 0, 0, 0
	for _, v := range f.Votes {
		if v.Real {
			real++
			if v.Introduced {
				intro++
			}
			switch v.Severity {
			case "important":
				important++
			case "minor":
				minor++
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
	case important*2 > real:
		f.Status, f.Severity = "confirmed", "important"
	case (important+minor)*2 > real:
		f.Status, f.Severity = "confirmed", "minor"
		// A verified security hole stays blocking when any voter calls it
		// important: hiding an auth bypass as "minor" costs far more than a false alarm.
		if f.Category == "security" && important > 0 {
			f.Severity = "important"
		}
	default:
		f.Status, f.Severity = "nit", "nit"
	}
	if f.Status == "rejected" {
		f.Introduced = false
		return
	}
	f.Introduced = intro*2 > real || (intro*2 == real && f.InDiff)
}

func carriedOpen(carried []Finding, offset int) []Finding {
	var out []Finding
	for i, f := range carried {
		f.ID = fmt.Sprintf("F%d", offset+i+1)
		out = append(out, f)
	}
	return out
}

func lineText(h *diff.Hunk, line int) string {
	for _, l := range h.Lines {
		if l.New == line && l.Kind != diff.Del {
			return strings.TrimSpace(l.Text)
		}
	}
	for _, l := range h.Lines {
		if l.Kind == diff.Add {
			return strings.TrimSpace(l.Text)
		}
	}
	return ""
}

func hunkText(h *diff.Hunk) string {
	if h == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s @@ +%d,%d @@\n", h.ID, h.File, h.NewStart, h.NewLines)
	for _, l := range h.Lines {
		switch l.Kind {
		case diff.Add:
			fmt.Fprintf(&b, "%5d + %s\n", l.New, l.Text)
		case diff.Del:
			fmt.Fprintf(&b, "%5s - %s\n", fmt.Sprintf("o%d", l.Old), l.Text)
		default:
			fmt.Fprintf(&b, "%5d   %s\n", l.New, l.Text)
		}
	}
	return clip(b.String(), 8000)
}

func confRank(c string) int {
	switch c {
	case "high":
		return 2
	case "medium":
		return 1
	}
	return 0
}

func normWS(s string) string { return strings.Join(strings.Fields(s), " ") }

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func tail(s string, n int) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > n {
		ls = append([]string{fmt.Sprintf("… %d earlier lines omitted", len(ls)-n)}, ls[len(ls)-n:]...)
	}
	return strings.Join(ls, "\n")
}

// DryRun returns what the agents would be sent (shared context + map task).
func DryRun(in *source.Input, d *diff.Diff, ws *workspace.Workspace) string {
	callers := ""
	if ws != nil {
		callers = workspace.FormatCallers(ws.Callers(workspace.Symbols(d), 40, 4))
	}
	return systemPrompt + "\n\n---\n\n" + sharedBlock(in, d.Summary(), callers, "") + "\n\n---\n\n" + diffBlock(d.Annotated(), "whole PR") + "\n\n---\n\n" + mapTask
}

func inRanges(rs [][2]int, line int) bool {
	for _, r := range rs {
		if line >= r[0] && line <= r[1] {
			return true
		}
	}
	return false
}
