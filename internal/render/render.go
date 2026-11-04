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
}

// Report is everything rendered; it's also what --save writes and --from reads.
type Report struct {
	Meta   Meta           `json:"meta"`
	Review *review.Review `json:"review"`
	Files  []*diff.File   `json:"files"`
	Usage  *review.Usage  `json:"usage,omitempty"`
}

// NewReport assembles a report.
func NewReport(in *source.Input, d *diff.Diff, r *review.Review, u *review.Usage) *Report {
	m := Meta{Repo: in.Repo, Number: in.Number, URL: in.URL, Title: in.Title, HeadSHA: in.HeadSHA}
	for _, is := range in.Issues {
		m.Issues = append(m.Issues, is.Number)
	}
	return &Report{Meta: m, Review: r, Files: d.Files, Usage: u}
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
	if len(r.NotChecked) > 0 {
		fmt.Fprintf(w, "<details><summary>Not checked by anyone (%d)</summary>\n\n- %s\n</details>\n\n", len(r.NotChecked), strings.Join(r.NotChecked, "\n- "))
	}
	if len(r.WatchAfterDeploy) > 0 {
		fmt.Fprintf(w, "<details><summary>Watch after deploy</summary>\n\n- %s\n</details>\n\n", strings.Join(r.WatchAfterDeploy, "\n- "))
	}
	if rep.Meta.Number > 0 {
		fmt.Fprintf(w, "<sub>Run `gods-eye %d` for the full review with diagrams.</sub>\n", rep.Meta.Number)
	}
}
