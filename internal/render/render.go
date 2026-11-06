// Package render turns a review into a self-contained HTML page and a short
// Markdown PR comment.
package render

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/review"
	"github.com/ShivamPansuriya/gods-eye/internal/source"
)

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// Meta is the PR info the page needs.
type Meta struct {
	Repo    string `json:"repo"`
	Number  int    `json:"number,omitempty"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title"`
	HeadSHA string `json:"headSha"`
	Issues  []int  `json:"issues"`
	// ReviewURL links the PR comment to the full review page (server mode).
	ReviewURL string `json:"reviewUrl,omitempty"`
}

// Report is everything rendered; it's also what --save writes and --from reads.
type Report struct {
	Meta   Meta           `json:"meta"`
	Review *review.Review `json:"review"`
	Files  []*diff.File   `json:"files"`
}

// NewReport assembles a report.
func NewReport(in *source.Input, d *diff.Diff, r *review.Review) *Report {
	m := Meta{Repo: in.Repo, Number: in.Number, URL: in.URL, Title: in.Title, HeadSHA: in.HeadSHA}
	for _, is := range in.Issues {
		m.Issues = append(m.Issues, is.Number)
	}
	return &Report{Meta: m, Review: r, Files: d.Files}
}

// HTML writes the review page.
func HTML(w io.Writer, rep *Report) error {
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	// Safe inside <script type="application/json">: neutralise "</" and HTML comment openers.
	s := strings.NewReplacer("</", `<\/`, "<!--", `<\!--`).Replace(string(b))
	title := "Review"
	if rep.Meta.Number > 0 {
		title = fmt.Sprintf("Review #%d", rep.Meta.Number)
	}
	return pageTmpl.Execute(w, struct {
		PageTitle string
		DataJSON  template.JS
	}{title, template.JS(s)})
}

var verdictText = map[string]string{
	"approve":         "✅ Looks good",
	"request_changes": "❌ Changes needed",
	"needs_decision":  "🟡 Needs a decision",
}

// Markdown writes a short PR comment: verdict, units by tier, failing checks, asks.
func Markdown(w io.Writer, rep *Report) {
	r := rep.Review
	fmt.Fprintf(w, "### 👁 God's Eye — %s\n\n", verdictText[r.Verdict])
	fmt.Fprintf(w, "**%s**  \n%s\n\n", r.Title, r.ELI5)
	if mr := r.MergeRisk; mr != nil {
		writeMergeRisk(w, mr)
	}

	var bugs, pre []string
	for _, f := range r.Findings {
		if f.Status != "confirmed" {
			continue
		}
		line := fmt.Sprintf("- **%s** (`%s:%d`): %s", f.Title, f.File, f.Line, f.Scenario)
		if f.Fix != "" {
			line += " *Fix:* " + f.Fix
		}
		if n := len(f.Votes); n > 0 {
			real := 0
			for _, v := range f.Votes {
				if v.Real {
					real++
				}
			}
			line += fmt.Sprintf(" <sub>%d/%d verifiers</sub>", real, n)
		}
		if f.Introduced {
			bugs = append(bugs, line)
		} else {
			pre = append(pre, line)
		}
	}
	if len(bugs) > 0 {
		fmt.Fprintf(w, "**🐞 Verified bugs (%d)**\n%s\n\n", len(bugs), strings.Join(bugs, "\n"))
	}
	if len(pre) > 0 {
		fmt.Fprintf(w, "<details><summary>Pre-existing bugs nearby (%d)</summary>\n\n%s\n</details>\n\n", len(pre), strings.Join(pre, "\n"))
	}

	tiers := []struct{ key, label string }{{"human", "🧑 Needs your decision"}, {"agent", "🤖 Agent reviewed"}, {"static", "⚙️ Static / CI"}}
	for _, t := range tiers {
		var rows []string
		for _, u := range r.Units {
			if u.Tier != t.key {
				continue
			}
			mark := ""
			nd, ne := 0, 0
			for _, c := range u.Checks {
				switch c.Status {
				case "not_done":
					nd++
				case "no_evidence":
					ne++
				}
			}
			if nd > 0 {
				mark = fmt.Sprintf(" — ❌ %d not done", nd)
			} else if ne > 0 {
				mark = fmt.Sprintf(" — ⚠️ %d unproven", ne)
			}
			rows = append(rows, fmt.Sprintf("- **%s** · %s risk%s", u.Title, u.Risk, mark))
		}
		if len(rows) > 0 {
			fmt.Fprintf(w, "**%s**\n%s\n\n", t.label, strings.Join(rows, "\n"))
		}
	}

	var asks []string
	for _, u := range r.Units {
		for _, a := range u.Ask {
			asks = append(asks, fmt.Sprintf("1. **%s:** %s", u.Title, a))
		}
	}
	if len(asks) > 0 {
		fmt.Fprintf(w, "**Requested changes**\n%s\n\n", strings.Join(asks, "\n"))
	}
	if ds := r.Design; ds != nil {
		var fo []string
		for _, f := range ds.Foundations.Items {
			if f.Status == "confirmed" {
				fo = append(fo, fmt.Sprintf("- **%s** (`%s:%d`): %s *Fix the old code:* %s *In this PR:* %s", f.Title, f.File, f.Line, f.Problem, f.FixOld, map[string]string{"now": "fix it now. " + f.AdoptHow, "follow_up": "follow-up. " + f.AdoptHow, "no": "leave it."}[f.Adopt]))
			}
		}
		if len(fo) > 0 {
			fmt.Fprintf(w, "**🏛 Existing design: %s**\n%s\n%s\n\n", ds.Foundations.Verdict, ds.Foundations.Summary, strings.Join(fo, "\n"))
		}
		var pr []string
		for _, p := range ds.Practices {
			if p.Status != "followed" && p.State != "resolved" {
				pr = append(pr, fmt.Sprintf("- %s **%s** (`%s:%d`): %s", map[string]string{"broken": "✕", "mixed": "~"}[p.Status], p.Practice, p.File, p.Line, p.Detail))
			}
		}
		if len(pr) > 0 {
			fmt.Fprintf(w, "<details><summary>Repo practices to look at (%d)</summary>\n\n%s\n</details>\n\n", len(pr), strings.Join(pr, "\n"))
		}
	}
	var proof []string
	for _, u := range r.Units {
		if u.Evidence.Status != "" && u.Evidence.Status != "provided" && u.Evidence.Ask != "" && u.Tier != "static" {
			proof = append(proof, fmt.Sprintf("- **%s**: %s", u.Title, u.Evidence.Ask))
		}
	}
	if len(proof) > 0 {
		fmt.Fprintf(w, "<details><summary>Proof to add (%d)</summary>\n\n%s\n</details>\n\n", len(proof), strings.Join(proof, "\n"))
	}
	if len(r.NotChecked) > 0 {
		fmt.Fprintf(w, "<details><summary>Not checked by anyone (%d)</summary>\n\n- %s\n</details>\n\n", len(r.NotChecked), strings.Join(r.NotChecked, "\n- "))
	}
	if len(r.WatchAfterDeploy) > 0 {
		fmt.Fprintf(w, "<details><summary>Watch after deploy</summary>\n\n- %s\n</details>\n\n", strings.Join(r.WatchAfterDeploy, "\n- "))
	}
	switch {
	case rep.Meta.ReviewURL != "":
		fmt.Fprintf(w, "**[Open the full review →](%s)** <sub>architecture map, code flows, verified bugs, existing-design check</sub>\n", rep.Meta.ReviewURL)
	case rep.Meta.Number > 0:
		fmt.Fprintf(w, "<sub>Run `gods-eye %d` for the full review with diagrams.</sub>\n", rep.Meta.Number)
	}
}

var riskIcon = map[string]string{"low": "🟢", "medium": "🟡", "high": "🔴"}
var levelIcon = map[int]string{0: "✅", 1: "⚠️", 2: "❗"}

// writeMergeRisk writes the five-signal merge-risk table.
func writeMergeRisk(w io.Writer, mr *review.MergeRisk) {
	fmt.Fprintf(w, "**%s Merge risk: %s** · %s\n\n| | Signal | Grade | Why |\n|---|---|---|---|\n", riskIcon[mr.Overall], mr.Overall, mr.Summary)
	for _, s := range mr.Signals {
		if s.Level < 0 {
			continue
		}
		why := strings.ReplaceAll(s.Reason, "|", "\\|")
		for _, c := range s.Claims {
			if c.Status == "confirmed" {
				why += fmt.Sprintf(" `%s:%d`", c.File, c.Line)
			}
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s |\n", levelIcon[s.Level], s.Name, s.Grade, why)
	}
	if len(mr.Lower) > 0 {
		fmt.Fprintf(w, "\n*To lower the risk:* %s\n", strings.Join(mr.Lower, " "))
	}
	fmt.Fprintln(w)
}
