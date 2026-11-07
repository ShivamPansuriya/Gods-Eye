// God's Eye: a pull request reviewer that shows you what changed, whether it did
// what it claims, and what needs your decision — in as few words as possible.
package main

import (
	"bytes"
	"cmp"
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
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/eval"
	"github.com/ShivamPansuriya/gods-eye/internal/github"
	"github.com/ShivamPansuriya/gods-eye/internal/ocr"
	"github.com/ShivamPansuriya/gods-eye/internal/remote"
	"github.com/ShivamPansuriya/gods-eye/internal/render"
	"github.com/ShivamPansuriya/gods-eye/internal/review"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
	"github.com/ShivamPansuriya/gods-eye/internal/scope"
	"github.com/ShivamPansuriya/gods-eye/internal/server"
	"github.com/ShivamPansuriya/gods-eye/internal/source"
	"github.com/ShivamPansuriya/gods-eye/internal/store"
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
  gods-eye history               list reviews run from this folder; gods-eye open <id> reopens one
  gods-eye serve                 run the web app: connect repos, review from the UI, auto-review on PR events
  gods-eye remote <command>      drive a running server from the terminal (gods-eye remote help)

Depth (like Quick / Standard / Deep):
  auto      (default) quick for docs-only changes, deep for sensitive paths or > 600 lines, else standard
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
		depth    = flag.String("depth", "auto", "auto | quick | standard | deep")
		vEffort  = flag.String("verify-effort", "medium", "reasoning effort for the verifiers")
		parallel = flag.Int("parallel", 8, "max concurrent API calls")
		budget   = flag.Float64("budget", 15, "base budget in USD for a PR up to 1,500 changed lines of code; grows with size up to 4x, 45% kept for checking findings")
		verify   = flag.String("verify-cmd", "", "run this command in the checkout (tests, build) and use its output as evidence")
		ocrMode  = flag.String("ocr", "auto", "auto: also hunt with Alibaba's open-code-review when it is installed | off")
		fresh    = flag.Bool("fresh", false, "ignore the previous round; review the whole PR again")
		wsDir    = flag.String("workspace", "", "repository checkout for code access (default: current repo)")
		verbose  = flag.Bool("v", false, "log every agent")
		out      = flag.String("out", "", "write the HTML page here (default: .godseye/<pr>.html)")
		noOpen   = flag.Bool("no-open", false, "don't open the page in a browser")
		serve    = flag.String("serve", "", "serve the page over HTTP on this `addr` (e.g. :8080) instead of opening a file")
		comment  = flag.Bool("comment", false, "post the review on the PR: a summary plus a comment on each verified bug's line")
		md       = flag.Bool("md", false, "print the Markdown summary to stdout")
		save     = flag.String("save", "", "also save the raw review JSON to `file`")
		from     = flag.String("from", "", "render from a saved review JSON `file` (no API call)")
		dryRun   = flag.Bool("dry-run", false, "print the prompt that would be sent and exit")
		revFile  = flag.String("review-file", "", "use this review JSON instead of calling the API (testing)")
		showVer  = flag.Bool("version", false, "print version")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "eval":
			runEval(os.Args[2:])
			return
		case "serve":
			runServe(os.Args[2:])
			return
		case "history":
			runHistory(os.Args[2:])
			return
		case "open":
			runOpen(os.Args[2:])
			return
		case "remote":
			if err := remote.Main(os.Args[2:], os.Stdout, os.Stdin, openBrowser); err != nil {
				fail("%v", err)
			}
			return
		case "review": // `gh gods-eye review 123` reads naturally
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
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
		mb := mergeBase(in, ws)
		readBase := baseReader(ws, mb)
		d, plan := review.Scope(d, scope.Load(readBase), readBase)
		if *dryRun {
			fmt.Println(review.DryRun(in, d, ws))
			return
		}
		rules := risk.Defaults
		if s, ok := readBase(".godseye.json"); ok {
			rules = risk.Parse([]byte(s))
		}
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
				VerifyCmd: *verify, Prev: prev, Scope: plan, Log: logf, OCR: cliOCR(*ocrMode, *effort, in, ws, mb),
			})
			check(err)
			p := r.Pipeline
			step("Done in %s · %d units · %d confirmed, %d pre-existing, %d rejected, %d unverifiable · $%.2f",
				time.Since(t0).Round(time.Second), len(r.Units), p.Confirmed, p.PreExisting, p.Rejected, p.Unverifiable, p.CostUSD)
			for _, e := range p.Errors {
				step("warning: %s", e)
			}
			if t := review.IncompleteText(p); t != "" {
				step("⚠ %s", t)
			}
			if in.HeadSHA != "" {
				if err := review.SaveState(".godseye", key, in.HeadSHA, r); err != nil {
					step("warning: could not save state: %v", err)
				}
			}
		}
		rep = render.NewReport(in, d, r)
		if *revFile == "" {
			saveCLIReview(in, rep)
		}
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
		postReview(rep, &mdBuf)
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

// mergeBase is the commit the PR or branch starts from, or "" when unknown.
func mergeBase(in *source.Input, ws *workspace.Workspace) string {
	if ws == nil || in.HeadSHA == "" || in.BaseRef == "" {
		return ""
	}
	base := in.BaseRef
	if !in.LocalMode {
		base = "origin/" + base
	}
	out, err := exec.Command("git", "-C", ws.Root, "merge-base", base, in.HeadSHA).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// baseReader reads repository settings at the merge base, so a PR cannot
// change how it is reviewed. With no repository (--diff), it reads the
// current directory.
func baseReader(ws *workspace.Workspace, mb string) func(string) (string, bool) {
	if ws == nil || mb == "" {
		return func(p string) (string, bool) {
			b, err := os.ReadFile(p)
			return string(b), err == nil
		}
	}
	return func(p string) (string, bool) {
		b, err := exec.Command("git", "-C", ws.Root, "show", mb+":"+p).Output()
		return string(b), err == nil
	}
}

// cliOCR runs open-code-review over the committed range merge-base..head in
// the workspace. Uncommitted changes in --local mode are God's Eye's alone.
func cliOCR(mode, effort string, in *source.Input, ws *workspace.Workspace, mb string) review.OCRFunc {
	key := cmp.Or(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CLAUDE_API_KEY"))
	bin := ocr.Find()
	if mode == "off" || ws == nil || key == "" || bin == "" {
		return nil
	}
	if mb == "" {
		step("open-code-review skipped: no merge base with %s", in.BaseRef)
		return nil
	}
	return func(ctx context.Context, req review.OCRRequest) (*ocr.Result, error) {
		return ocr.Run(ctx, ocr.Options{Bin: bin, Dir: ws.Root, Base: cmp.Or(req.From, mb), Head: in.HeadSHA, APIKey: key, Model: req.Model,
			Effort: review.OCREffort(effort), MaxTokens: req.MaxTokens, Exclude: req.Exclude, Background: in.Title + "\n\n" + in.Body,
			Env: github.GitAuthEnv(ghToken(), "https://github.com/"), Timeout: 20 * time.Minute})
	}
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

// saveCLIReview keeps every CLI review in .godseye so `gods-eye history` can list it.
func saveCLIReview(in *source.Input, rep *render.Report) {
	st, err := store.Open(".godseye")
	if err != nil {
		return
	}
	r := rep.Review
	rec := store.Review{ID: store.NewReviewID(), Repo: in.Repo, PR: in.Number, Title: r.Title, Author: in.Author,
		HeadSHA: in.HeadSHA, Trigger: "cli", Status: "done", Verdict: r.Verdict, Units: len(r.Units),
		CreatedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}
	if p := r.Pipeline; p != nil {
		rec.Round, rec.Bugs, rec.Minor, rec.CostUSD, rec.Seconds = p.Round, p.Confirmed, p.Minor, p.CostUSD, p.Seconds
	}
	if r.MergeRisk != nil {
		rec.Risk = r.MergeRisk.Overall
	}
	rec.Incomplete = review.IncompleteText(r.Pipeline)
	if r.Design != nil {
		for _, f := range r.Design.Foundations.Items {
			if f.Status == "confirmed" {
				rec.Design++
			}
		}
	}
	if st.SaveReport(rec.ID, rep) == nil && st.SaveReview(rec) == nil {
		step("Saved as review %s (gods-eye history)", rec.ID)
	}
}

func runHistory(args []string) {
	fs := flag.NewFlagSet("history", flag.ExitOnError)
	dir := fs.String("data", ".godseye", "data folder")
	pr := fs.Int("pr", 0, "only this PR number")
	fs.Parse(args)
	st, err := store.Open(*dir)
	check(err)
	rs, _ := st.Reviews("", *pr, 50)
	if len(rs) == 0 {
		fmt.Println("No reviews saved in", *dir)
		return
	}
	fmt.Printf("%-24s  %-6s  %-5s  %-16s  %5s  %6s  %s\n", "ID", "PR", "ROUND", "VERDICT", "BUGS", "COST", "TITLE")
	for _, r := range rs {
		pr := "-"
		if r.PR > 0 {
			pr = fmt.Sprintf("#%d", r.PR)
		}
		title := r.Title
		if len(title) > 48 {
			title = title[:47] + "…"
		}
		fmt.Printf("%-24s  %-6s  %-5d  %-16s  %5d  $%5.2f  %s\n", r.ID, pr, r.Round, r.Verdict, r.Bugs, r.CostUSD, title)
	}
	fmt.Println("\nReopen one with: gods-eye open <id>")
}

func runOpen(args []string) {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	dir := fs.String("data", ".godseye", "data folder")
	serveAddr := fs.String("serve", "", "serve the page on this addr instead of opening a file")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fail("usage: gods-eye open <review id>   (see gods-eye history)")
	}
	st, err := store.Open(*dir)
	check(err)
	p, ok := st.ReportPath(fs.Arg(0))
	if !ok {
		fail("no saved review %s in %s", fs.Arg(0), *dir)
	}
	b, err := os.ReadFile(p)
	check(err)
	rep := &render.Report{}
	check(json.Unmarshal(b, rep))
	out := filepath.Join(*dir, fs.Arg(0)+".html")
	f, err := os.Create(out)
	check(err)
	check(render.HTML(f, rep))
	check(f.Close())
	abs, _ := filepath.Abs(out)
	step("Review page: %s", abs)
	if *serveAddr != "" {
		check(serveFile(*serveAddr, abs))
		return
	}
	openBrowser(abs)
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "listen address")
	home, _ := os.UserHomeDir()
	dir := fs.String("data", filepath.Join(home, ".gods-eye"), "data folder (settings, repos, every review)")
	fs.Parse(args)
	srv, err := server.New(*dir)
	check(err)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shown := *addr
	if strings.HasPrefix(shown, ":") || strings.HasPrefix(shown, "0.0.0.0:") {
		shown = "localhost:" + shown[strings.LastIndex(shown, ":")+1:]
	}
	step("God's Eye is running on http://%s", shown)
	step("Data folder: %s", *dir)
	step("Admin token (sign in with this): %s", srv.AdminToken())
	check(srv.Run(ctx, *addr))
}

// ghToken is the user's gh login token, or "".
func ghToken() string {
	b, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// resolveFixed closes the review threads God's Eye opened for bugs that this
// round found fixed, through the user's gh login.
func resolveFixed(rep *render.Report) {
	fixed := render.FixedThreads(rep)
	if len(fixed) == 0 || rep.Meta.Repo == "" {
		return
	}
	tok := ghToken()
	if tok == "" {
		step("Could not resolve the threads of fixed bugs: no gh login")
		return
	}
	n, err := github.New(tok, "").ResolveThreads(context.Background(), rep.Meta.Repo, rep.Meta.Number, fixed)
	if err != nil {
		step("Could not resolve the threads of fixed bugs: %v", err)
	} else if n > 0 {
		step("Resolved %d review threads whose bug is fixed", n)
	}
}

// postReview posts the summary as a GitHub PR review with one comment on each
// verified bug's line, through the user's gh login. If GitHub refuses the line
// comments, the summary goes up as a plain PR comment instead.
func postReview(rep *render.Report, md *bytes.Buffer) {
	repo := rep.Meta.Repo
	if repo == "" {
		repo = "{owner}/{repo}" // gh fills these in from the current clone
	}
	resolveFixed(rep)
	cs := render.InlineComments(rep)
	if len(cs) > 0 {
		req := map[string]any{"event": "COMMENT", "body": md.String(), "comments": cs}
		if rep.Meta.HeadSHA != "" {
			req["commit_id"] = rep.Meta.HeadSHA
		}
		b, _ := json.Marshal(req)
		cmd := exec.Command("gh", "api", "-X", "POST", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, rep.Meta.Number), "--input", "-", "--jq", ".html_url")
		var errBuf bytes.Buffer
		cmd.Stdin, cmd.Stderr = bytes.NewReader(b), &errBuf
		out, err := cmd.Output()
		if err == nil {
			step("Posted review on #%d with %d line comments: %s", rep.Meta.Number, len(cs), strings.TrimSpace(string(out)))
			return
		}
		step("GitHub refused the line comments (%s); posting the summary only", strings.TrimSpace(errBuf.String()))
	}
	args := []string{"pr", "comment", fmt.Sprint(rep.Meta.Number), "--body-file", "-"}
	if rep.Meta.Repo != "" {
		args = append(args, "--repo", rep.Meta.Repo)
	}
	cmd := exec.Command("gh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = md, os.Stderr, os.Stderr
	check(cmd.Run())
	step("Posted summary comment on #%d", rep.Meta.Number)
}
