// God's Eye: a pull request reviewer that shows you what changed, whether it did
// what it claims, and what needs your decision — in as few words as possible.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/eval"
	"github.com/ShivamPansuriya/gods-eye/internal/render"
	"github.com/ShivamPansuriya/gods-eye/internal/review"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
	"github.com/ShivamPansuriya/gods-eye/internal/source"
	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

var version = "dev"

const usage = `God's Eye — PR review you can read in two minutes.

Usage:
  gods-eye [PR] [flags]          review a pull request (number, URL or branch; default: current branch's PR)
  gods-eye --local [--base main] review the current branch against a base, no GitHub needed
  gods-eye --diff x.diff         review a saved diff
  gods-eye --from report.json    re-render a saved review without calling the API
  gods-eye eval evals/cases.json score the reviewer against known bugs

Depth (like Quick / Standard / Deep):
  quick     one mapping agent: units, tiers, flows, checks
  standard  + bug hunters over slices of the diff, each finding verified by 3 independent voters
  deep      + two hunter lenses per slice (logic, contracts)

Flags:
`

func main() {
	var (
		repo     = flag.String("repo", "", "GitHub repo `owner/name` (default: from the current directory)")
		local    = flag.Bool("local", false, "review the local branch with git instead of a GitHub PR")
		base     = flag.String("base", "", "base ref for --local (default: origin/HEAD or main)")
		diffFile = flag.String("diff", "", "review this unified diff `file`")
		title    = flag.String("title", "", "PR title for --diff / --local (default: from commits)")
		model    = flag.String("model", "claude-opus-5-5", "Claude model")
		effort   = flag.String("effort", "high", "reasoning effort: low | medium | high | xhigh | max")
		depth    = flag.String("depth", "standard", "quick | standard | deep")
		vEffort  = flag.String("verify-effort", "medium", "reasoning effort for the verifiers")
		parallel = flag.Int("parallel", 8, "max concurrent API calls")
		budget   = flag.Float64("budget", 15, "stop starting new verifications above this spend, in USD")
		verify   = flag.String("verify-cmd", "", "run this command in the checkout (tests, build) and use its output as evidence")
		fresh    = flag.Bool("fresh", false, "ignore the previous round; review the whole PR again")
		wsDir    = flag.String("workspace", "", "repository checkout for code access (default: current repo)")
		verbose  = flag.Bool("v", false, "log every agent")
		out      = flag.String("out", "", "write the HTML page here (default: .godseye/<pr>.html)")
		noOpen   = flag.Bool("no-open", false, "don't open the page in a browser")
		serve    = flag.String("serve", "", "serve the page over HTTP on this `addr` (e.g. :8080) instead of opening a file")
		comment  = flag.Bool("comment", false, "post a short summary comment on the PR")
		md       = flag.Bool("md", false, "print the Markdown summary to stdout")
		save     = flag.String("save", "", "also save the raw review JSON to `file`")
		from     = flag.String("from", "", "render from a saved review JSON `file` (no API call)")
		dryRun   = flag.Bool("dry-run", false, "print the prompt that would be sent and exit")
		revFile  = flag.String("review-file", "", "use this review JSON instead of calling the API (testing)")
		showVer  = flag.Bool("version", false, "print version")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	if len(os.Args) > 1 && os.Args[1] == "eval" {
		runEval(os.Args[2:])
		return
	}
	flag.Parse()
	if *showVer {
		fmt.Println("gods-eye", version)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var rep *render.Report
	if *from != "" {
		b, err := os.ReadFile(*from)
		check(err)
		rep = &render.Report{}
		check(json.Unmarshal(b, rep))
	} else {
		in, err := load(*local, *base, *diffFile, *title, *repo, flag.Arg(0))
		check(err)
		d, err := diff.Parse(in.RawDiff)
		check(err)
		if len(d.Hunks()) == 0 {
			fail("no changes found in the diff")
		}
		ws := openWorkspace(*wsDir, in, *diffFile != "")
		if *dryRun {
			fmt.Println(review.DryRun(in, d, ws))
			return
		}
		rules := risk.Load(".godseye.json")
		var r *review.Review
		if *revFile != "" {
			b, err := os.ReadFile(*revFile)
			check(err)
			r = &review.Review{}
			check(json.Unmarshal(b, r))
			review.Finalize(r, d, rules)
			review.Merge(r, d, r.Findings)
		} else {
			key := stateKey(in)
			var prev *review.State
			if !*fresh {
				prev = review.LoadState(".godseye", key)
			}
			step("Reviewing %d files, %d hunks · %s · depth %s · effort %s", len(d.Files), len(d.Hunks()), *model, *depth, *effort)
			logf := func(s string) { step("%s", s) }
			if !*verbose {
				logf = func(s string) {
					if !strings.HasPrefix(s, "hunt-") && !strings.HasPrefix(s, "verify-") && !strings.Contains(s, "submitted after") {
						step("%s", s)
					}
				}
			}
			t0 := time.Now()
			r, err = review.Run(ctx, newClient(), in, d, ws, rules, review.Options{
				Model: *model, Effort: *effort, VerifyEff: *vEffort, Depth: *depth, Parallel: *parallel, Budget: *budget,
				VerifyCmd: *verify, Prev: prev, Log: logf,
			})
			check(err)
			p := r.Pipeline
			step("Done in %s · %d units · %d confirmed, %d pre-existing, %d rejected, %d unverifiable · $%.2f",
				time.Since(t0).Round(time.Second), len(r.Units), p.Confirmed, p.PreExisting, p.Rejected, p.Unverifiable, p.CostUSD)
			for _, e := range p.Errors {
				step("warning: %s", e)
			}
			if in.HeadSHA != "" {
				if err := review.SaveState(".godseye", key, in.HeadSHA, r); err != nil {
					step("warning: could not save state: %v", err)
				}
			}
		}
		rep = render.NewReport(in, d, r)
	}

	if *save != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		check(os.WriteFile(*save, b, 0o644))
		step("Saved %s", *save)
	}

	path := *out
	if path == "" {
		name := "local"
		if rep.Meta.Number > 0 {
			name = fmt.Sprintf("pr-%d", rep.Meta.Number)
		} else if rep.Meta.HeadSHA != "" {
			name = "local-" + rep.Meta.HeadSHA[:min(7, len(rep.Meta.HeadSHA))]
		}
		path = filepath.Join(".godseye", name+".html")
	}
	check(os.MkdirAll(filepath.Dir(path), 0o755))
	f, err := os.Create(path)
	check(err)
	check(render.HTML(f, rep))
	check(f.Close())
	abs, _ := filepath.Abs(path)
	step("Review page: %s", abs)

	var mdBuf bytes.Buffer
	render.Markdown(&mdBuf, rep)
	if *md {
		fmt.Print(mdBuf.String())
	}
	if *comment {
		if rep.Meta.Number == 0 {
			fail("--comment needs a GitHub PR")
		}
		args := []string{"pr", "comment", fmt.Sprint(rep.Meta.Number), "--body-file", "-"}
		if rep.Meta.Repo != "" {
			args = append(args, "--repo", rep.Meta.Repo)
		}
		cmd := exec.Command("gh", args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = &mdBuf, os.Stderr, os.Stderr
		check(cmd.Run())
		step("Posted summary comment on #%d", rep.Meta.Number)
	}
	if *serve != "" {
		check(serveFile(*serve, abs))
		return
	}
	if !*noOpen {
		openBrowser(abs)
	}
}

func load(local bool, base, diffFile, title, repo, pr string) (*source.Input, error) {
	switch {
	case diffFile != "":
		return source.FromDiffFile(diffFile, title, "")
	case local:
		in, err := source.FromLocal(base)
		if err == nil && title != "" {
			in.Title = title
		}
		return in, err
	default:
		if _, err := exec.LookPath("gh"); err != nil {
			return nil, errors.New("gh CLI not found; install it from https://cli.github.com or use --local")
		}
		return source.FromPR(pr, repo)
	}
}

// openWorkspace gives agents read access to the code at the PR head.
func openWorkspace(dir string, in *source.Input, diffOnly bool) *workspace.Workspace {
	if dir == "" && diffOnly {
		return nil
	}
	if dir == "" {
		dir = "."
	}
	ref := in.HeadSHA
	if ref != "" && !in.LocalMode {
		// PR mode: make sure the head commit is present locally.
		if _, err := workspace.Open(dir, ref); err != nil && in.Number > 0 {
			_ = exec.Command("git", "-C", dir, "fetch", "--depth=50", "origin", fmt.Sprintf("pull/%d/head", in.Number)).Run()
		}
	}
	ws, err := workspace.Open(dir, ref)
	if err != nil {
		step("No code access (%v); reviewing from the diff only", err)
		return nil
	}
	return ws
}

func stateKey(in *source.Input) string {
	if in.Number > 0 {
		return fmt.Sprintf("%s#%d", in.Repo, in.Number)
	}
	return in.Repo + "@" + in.HeadRef
}

func openBrowser(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		cmd = exec.Command("xdg-open", path)
	}
	_ = cmd.Start()
}

func step(format string, a ...any) { fmt.Fprintf(os.Stderr, "› "+format+"\n", a...) }

func check(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if strings.Contains(msg, "401") || strings.Contains(strings.ToLower(msg), "api key") {
		msg += "\n  hint: set ANTHROPIC_API_KEY (or run `ant auth login`)"
	}
	fmt.Fprintln(os.Stderr, "✕ "+msg)
	os.Exit(1)
}

// newClient uses ANTHROPIC_API_KEY (the SDK default) and falls back to CLAUDE_API_KEY.
func newClient() anthropic.Client {
	opts := []option.RequestOption{option.WithMaxRetries(5)}
	if os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		if k := os.Getenv("CLAUDE_API_KEY"); k != "" {
			opts = append(opts, option.WithAPIKey(k))
		}
	}
	return anthropic.NewClient(opts...)
}

// serveFile serves the review page at / until interrupted.
func serveFile(addr, file string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFile(w, r, file)
	})
	step("Serving review at http://localhost:%d (Ctrl-C to stop)", ln.Addr().(*net.TCPAddr).Port)
	return http.Serve(ln, mux)
}

func runEval(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	model := fs.String("model", "claude-opus-5-5", "Claude model")
	effort := fs.String("effort", "high", "reasoning effort")
	depth := fs.String("depth", "standard", "quick | standard | deep")
	parallel := fs.Int("parallel", 8, "max concurrent API calls")
	only := fs.String("case", "", "run only this case")
	out := fs.String("out", "", "write results JSON here")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fail("usage: gods-eye eval [flags] cases.json")
	}
	cases, err := eval.Load(fs.Arg(0))
	check(err)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	client := newClient()
	var results []eval.Result
	var cost float64
	caught, total := 0, 0
	for _, c := range cases {
		if *only != "" && c.Name != *only {
			continue
		}
		step("Case %s (%d known bugs)…", c.Name, len(c.Expect))
		res := eval.Run(ctx, client, c, review.Options{Model: *model, Effort: *effort, Depth: *depth, Parallel: *parallel,
			Log: func(s string) {
				if !strings.HasPrefix(s, "hunt-") && !strings.HasPrefix(s, "verify-") && !strings.Contains(s, "submitted after") {
					step("  %s", s)
				}
			}})
		if res.Error != "" {
			step("  ✕ %s", res.Error)
		}
		for _, x := range res.Caught {
			step("  ✓ caught   %s", x)
		}
		for _, x := range res.Missed {
			tag := ""
			for _, r := range res.Rejected {
				if r == x {
					tag = " (found, then rejected by voters)"
				}
			}
			step("  ✕ missed   %s%s", x, tag)
		}
		for _, f := range res.Extra {
			step("  + extra    %s:%d %s", f.File, f.Line, f.Title)
		}
		step("  recall %.0f%% · %d candidates · $%.2f · %.0fs", res.Recall*100, res.Candidates, res.CostUSD, res.Seconds)
		caught += len(res.Caught)
		total += len(res.Caught) + len(res.Missed)
		cost += res.CostUSD
		results = append(results, res)
	}
	if total > 0 {
		step("TOTAL recall %d/%d (%.0f%%) · $%.2f", caught, total, float64(caught)/float64(total)*100, cost)
	}
	if *out != "" {
		b, _ := json.MarshalIndent(results, "", "  ")
		check(os.WriteFile(*out, b, 0o644))
	}
}
