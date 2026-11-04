// Package eval scores the reviewer against PRs with known bugs ("our incidents
// are the eval"). Each case lists the bugs a good review must catch; the score
// is recall, plus how many extra findings the reviewer confirmed.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/review"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
	"github.com/ShivamPansuriya/gods-eye/internal/source"
	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

// Case is one PR with known bugs. Use either Fixture (a dir with base/ and
// change.diff, built into a throwaway git repo) or Repo+Base+Head.
type Case struct {
	Name    string   `json:"name"`
	Fixture string   `json:"fixture,omitempty"`
	Repo    string   `json:"repo,omitempty"`
	Base    string   `json:"base,omitempty"`
	Head    string   `json:"head,omitempty"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Expect  []Expect `json:"expect"`
}

// Expect is one known bug: a file and a line range where a finding must land.
type Expect struct {
	File string `json:"file"`
	From int    `json:"from"`
	To   int    `json:"to"`
	Bug  string `json:"bug"`
}

// Result is one case's score.
type Result struct {
	Name       string           `json:"name"`
	Caught     []string         `json:"caught"`
	Missed     []string         `json:"missed"`
	Rejected   []string         `json:"rejectedButReal"` // a hunter found it, voters rejected it
	Extra      []review.Finding `json:"extra"`           // confirmed findings outside the expected set
	Recall     float64          `json:"recall"`
	CostUSD    float64          `json:"costUsd"`
	Seconds    float64          `json:"seconds"`
	Candidates int              `json:"candidates"`
	Error      string           `json:"error,omitempty"`
}

// Load reads a cases file; fixture and repo paths are relative to it.
func Load(path string) ([]Case, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cs []Case
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	for i := range cs {
		if cs[i].Fixture != "" && !filepath.IsAbs(cs[i].Fixture) {
			cs[i].Fixture = filepath.Join(dir, cs[i].Fixture)
		}
		if cs[i].Repo != "" && !filepath.IsAbs(cs[i].Repo) {
			cs[i].Repo = filepath.Join(dir, cs[i].Repo)
		}
	}
	return cs, nil
}

// Run reviews one case and scores it.
func Run(ctx context.Context, client anthropic.Client, c Case, opt review.Options) Result {
	res := Result{Name: c.Name}
	t0 := time.Now()
	repo, base, head, cleanup, err := prepare(c)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer cleanup()

	raw, err := gitOut(repo, "diff", "--no-color", "--no-ext-diff", base, head)
	if err != nil {
		res.Error = "diff: " + err.Error()
		return res
	}
	d, err := diff.Parse(raw)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	ws, err := workspace.Open(repo, head)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	in := &source.Input{Repo: c.Name, Title: c.Title, Body: c.Body, BaseRef: base, HeadRef: head, HeadSHA: head, LocalMode: true, Guidance: map[string]string{}}
	for _, g := range []string{"REVIEW.md", "CLAUDE.md", "AGENTS.md"} {
		if s, err := gitOut(repo, "show", head+":"+g); err == nil {
			in.Guidance[g] = s
		}
	}
	opt.Prev = nil
	r, err := review.Run(ctx, client, in, d, ws, risk.Defaults, opt)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Seconds = time.Since(t0).Seconds()
	res.CostUSD = r.Pipeline.CostUSD
	res.Candidates = r.Pipeline.Candidates
	Score(&res, c.Expect, r)
	return res
}

// Score matches findings to expected bugs. A confirmed finding (or one of its
// also_at locations) within 3 lines of an expected range catches it.
func Score(res *Result, expect []Expect, r *review.Review) {
	used := map[string]bool{}
	for _, e := range expect {
		label := fmt.Sprintf("%s:%d-%d %s", e.File, e.From, e.To, e.Bug)
		hit, rejected := false, false
		for _, f := range r.Findings {
			if !matches(f, e) {
				continue
			}
			if f.Status == "confirmed" {
				hit = true
				used[f.ID] = true
			} else if f.Status == "rejected" || f.Status == "nit" {
				rejected = true
			}
		}
		// A not_done check pointing at the bug also counts (it was verified).
		if !hit {
			for _, u := range r.Units {
				for _, c := range u.Checks {
					if c.Status == "not_done" && c.Finding != "" {
						for _, f := range r.Findings {
							if f.ID == c.Finding && matches(f, e) {
								hit = true
							}
						}
					}
				}
			}
		}
		switch {
		case hit:
			res.Caught = append(res.Caught, label)
		case rejected:
			res.Missed = append(res.Missed, label)
			res.Rejected = append(res.Rejected, label)
		default:
			res.Missed = append(res.Missed, label)
		}
	}
	for _, f := range r.Findings {
		if f.Status == "confirmed" && !used[f.ID] {
			res.Extra = append(res.Extra, f)
		}
	}
	if len(expect) > 0 {
		res.Recall = float64(len(res.Caught)) / float64(len(expect))
	}
}

func matches(f review.Finding, e Expect) bool {
	in := func(file string, line int) bool {
		return file == e.File && line >= e.From-3 && line <= e.To+3
	}
	if in(f.File, f.Line) {
		return true
	}
	for _, a := range f.AlsoAt {
		if in(a.File, a.Line) {
			return true
		}
	}
	return false
}

// prepare returns a repo with base and head commits.
func prepare(c Case) (repo, base, head string, cleanup func(), err error) {
	cleanup = func() {}
	if c.Fixture == "" {
		b, err1 := gitOut(c.Repo, "rev-parse", c.Base)
		h, err2 := gitOut(c.Repo, "rev-parse", c.Head)
		if err1 != nil || err2 != nil {
			return "", "", "", cleanup, fmt.Errorf("refs %s / %s not found in %s", c.Base, c.Head, c.Repo)
		}
		return c.Repo, strings.TrimSpace(b), strings.TrimSpace(h), cleanup, nil
	}
	fixture, err := filepath.Abs(c.Fixture)
	if err != nil {
		return "", "", "", cleanup, err
	}
	dir, err := os.MkdirTemp("", "godseye-eval-")
	if err != nil {
		return "", "", "", cleanup, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	steps := [][]string{
		{"cp", "-R", filepath.Join(fixture, "base") + "/.", dir},
		{"git", "-C", dir, "init", "-q"},
		{"git", "-C", dir, "add", "-A"},
		{"git", "-C", dir, "-c", "user.email=eval@godseye", "-c", "user.name=eval", "commit", "-qm", "base"},
		{"git", "-C", dir, "apply", filepath.Join(fixture, "change.diff")},
		{"git", "-C", dir, "add", "-A"},
		{"git", "-C", dir, "-c", "user.email=eval@godseye", "-c", "user.name=eval", "commit", "-qm", c.Title},
	}
	for _, s := range steps {
		if out, err := exec.Command(s[0], s[1:]...).CombinedOutput(); err != nil {
			return "", "", "", cleanup, fmt.Errorf("%s: %s", strings.Join(s, " "), out)
		}
	}
	b, _ := gitOut(dir, "rev-parse", "HEAD~1")
	h, _ := gitOut(dir, "rev-parse", "HEAD")
	return dir, strings.TrimSpace(b), strings.TrimSpace(h), cleanup, nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}
