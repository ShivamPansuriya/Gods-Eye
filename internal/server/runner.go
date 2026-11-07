package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
	"github.com/ShivamPansuriya/gods-eye/internal/github"
	"github.com/ShivamPansuriya/gods-eye/internal/render"
	"github.com/ShivamPansuriya/gods-eye/internal/review"
	"github.com/ShivamPansuriya/gods-eye/internal/risk"
	"github.com/ShivamPansuriya/gods-eye/internal/source"
	"github.com/ShivamPansuriya/gods-eye/internal/store"
	"github.com/ShivamPansuriya/gods-eye/internal/workspace"
)

// Runner queues and runs reviews in the background.
type Runner struct {
	Store     *store.Store
	Anthropic func() (anthropic.Client, bool)
	Config    func() store.Config

	queue   chan string
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	logs    map[string][]string // live log of running reviews
	repoMu  map[string]*sync.Mutex
}

// NewRunner creates a runner; call Start to begin processing.
func NewRunner(st *store.Store, cfg func() store.Config, ac func() (anthropic.Client, bool)) *Runner {
	return &Runner{Store: st, Config: cfg, Anthropic: ac, queue: make(chan string, 1000),
		cancels: map[string]context.CancelFunc{}, logs: map[string][]string{}, repoMu: map[string]*sync.Mutex{}}
}

// Start launches the workers and re-queues reviews interrupted by a restart.
func (r *Runner) Start(ctx context.Context, workers int) {
	for i := 0; i < max(workers, 1); i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-r.queue:
					r.process(ctx, id)
				}
			}
		}()
	}
	rs, _ := r.Store.Reviews("", 0, 500)
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Status == "queued" || rs[i].Status == "running" {
			rs[i].Status = "queued"
			rs[i].Log = append(rs[i].Log, "Re-queued after a server restart")
			_ = r.Store.SaveReview(rs[i])
			r.queue <- rs[i].ID
		}
	}
}

// Enqueue adds a review for a PR. A PR that already has a queued review is not queued twice.
func (r *Runner) Enqueue(repo string, pr int, trigger string, fresh bool) (store.Review, error) {
	existing, _ := r.Store.Reviews(repo, pr, 20)
	for _, e := range existing {
		if e.Status == "queued" {
			return e, nil
		}
	}
	rec := store.Review{ID: store.NewReviewID(), Repo: repo, PR: pr, Trigger: trigger, Status: "queued", CreatedAt: time.Now().UTC()}
	if fresh {
		rec.Log = []string{"Full review requested: earlier rounds are ignored"}
	}
	if err := r.Store.SaveReview(rec); err != nil {
		return rec, err
	}
	select {
	case r.queue <- rec.ID:
	default:
		return rec, errors.New("the review queue is full")
	}
	return rec, nil
}

// Cancel stops a queued or running review.
func (r *Runner) Cancel(id string) bool {
	r.mu.Lock()
	cancel, running := r.cancels[id]
	r.mu.Unlock()
	if running {
		cancel()
		return true
	}
	rec, ok := r.Store.Review(id)
	if ok && rec.Status == "queued" {
		rec.Status, rec.FinishedAt = "cancelled", time.Now().UTC()
		_ = r.Store.SaveReview(rec)
		return true
	}
	return false
}

// Live returns the in-memory log of a running review.
func (r *Runner) Live(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.logs[id]...)
}

func (r *Runner) logf(id, format string, a ...any) {
	line := time.Now().UTC().Format("15:04:05") + "  " + fmt.Sprintf(format, a...)
	r.mu.Lock()
	r.logs[id] = append(r.logs[id], line)
	r.mu.Unlock()
}

func (r *Runner) process(parent context.Context, id string) {
	rec, ok := r.Store.Review(id)
	if !ok || rec.Status != "queued" {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.cancels[id] = cancel
	r.logs[id] = append([]string(nil), rec.Log...)
	r.mu.Unlock()
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.cancels, id)
		delete(r.logs, id)
		r.mu.Unlock()
	}()

	rec.Status, rec.StartedAt = "running", time.Now().UTC()
	_ = r.Store.SaveReview(rec)
	// Persist the live log every few seconds so a page reload or restart keeps it.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				cur, ok := r.Store.Review(id)
				if ok && cur.Status == "running" {
					cur.Log = r.Live(id)
					_ = r.Store.SaveReview(cur)
				}
			}
		}
	}()

	err := r.run(ctx, &rec)
	close(stop)
	rec.Log = r.Live(id)
	rec.FinishedAt = time.Now().UTC()
	switch {
	case errors.Is(ctx.Err(), context.Canceled) && parent.Err() == nil:
		rec.Status = "cancelled"
		rec.Log = append(rec.Log, "Cancelled")
	case err != nil:
		rec.Status, rec.Error = "failed", err.Error()
		rec.Log = append(rec.Log, "Failed: "+err.Error())
	default:
		rec.Status = "done"
	}
	_ = r.Store.SaveReview(rec)
}

func (r *Runner) run(ctx context.Context, rec *store.Review) error {
	cfg := r.Config()
	repo, ok := r.Store.Repo(rec.Repo)
	if !ok {
		return fmt.Errorf("%s is not connected", rec.Repo)
	}
	client, ok := r.Anthropic()
	if !ok {
		return errors.New("no Anthropic API key: set ANTHROPIC_API_KEY or CLAUDE_API_KEY, or add a key in Settings")
	}
	if cfg.MonthlyBudget > 0 && rec.Trigger == "webhook" {
		if spent := r.Store.SpendSince(time.Now().AddDate(0, -1, 0)); spent >= cfg.MonthlyBudget {
			return fmt.Errorf("monthly budget of $%.0f reached ($%.2f spent in the last 30 days); automatic reviews are paused", cfg.MonthlyBudget, spent)
		}
	}
	gh := github.New(cfg.GitHubToken, cfg.GitHubAPI)

	r.logf(rec.ID, "Reading PR #%d from GitHub", rec.PR)
	pull, err := gh.Pull(ctx, rec.Repo, rec.PR)
	if err != nil {
		return fmt.Errorf("read PR: %w", err)
	}
	rec.Title, rec.Author, rec.HeadSHA = pull.Title, pull.User.Login, pull.Head.SHA
	_ = r.Store.SaveReview(*rec)

	dir, err := r.sync(ctx, rec, repo, cfg.GitHubToken, pull)
	if err != nil {
		return err
	}
	mb, err := git(ctx, dir, "merge-base", pull.DiffBase(), pull.Head.SHA)
	if err != nil {
		return fmt.Errorf("find merge base: %w", err)
	}
	raw, err := git(ctx, dir, "diff", "--no-color", "--no-ext-diff", strings.TrimSpace(mb), pull.Head.SHA)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	d, err := diff.Parse(raw)
	if err != nil {
		return err
	}
	if len(d.Hunks()) == 0 {
		return errors.New("the PR has no changes to review")
	}

	in := &source.Input{Repo: rec.Repo, Number: rec.PR, URL: pull.HTMLURL, Title: pull.Title, Body: pull.Body,
		Author: pull.User.Login, BaseRef: pull.Base.Ref, HeadRef: pull.Head.Ref, HeadSHA: pull.Head.SHA, Guidance: map[string]string{}}
	if msgs, err := gh.Commits(ctx, rec.Repo, rec.PR); err == nil {
		for _, m := range msgs {
			in.Commits = append(in.Commits, strings.TrimSpace(m))
		}
	}
	if iss, err := gh.ClosingIssues(ctx, rec.Repo, rec.PR); err == nil {
		for _, i := range iss {
			in.Issues = append(in.Issues, source.Issue{Number: i.Number, Title: i.Title, Body: i.Body, URL: i.URL})
		}
	}
	for _, g := range []string{"REVIEW.md", "CLAUDE.md", "AGENTS.md", ".github/REVIEW.md"} {
		if s, err := git(ctx, dir, "show", pull.Head.SHA+":"+g); err == nil {
			in.Guidance[g] = s
		}
	}
	rules := risk.Defaults
	if s, err := git(ctx, dir, "show", pull.Head.SHA+":.godseye.json"); err == nil {
		rules = risk.Parse([]byte(s))
	}
	ws, err := workspace.Open(dir, pull.Head.SHA)
	if err != nil {
		return err
	}

	key := fmt.Sprintf("%s#%d", rec.Repo, rec.PR)
	var prev *review.State
	fresh := len(rec.Log) > 0 && strings.HasPrefix(rec.Log[0], "Full review requested")
	if !fresh {
		prev = review.LoadState(r.Store.Dir, key)
	}
	s := repo.Settings
	r.logf(rec.ID, "Reviewing %d files, %d hunks · depth %s · effort %s", len(d.Files), len(d.Hunks()), s.Depth, s.Effort)
	rv, err := review.Run(ctx, client, in, d, ws, rules, review.Options{
		Model: cfg.Model, Effort: s.Effort, Depth: s.Depth, Budget: s.Budget, Prev: prev,
		Log: func(line string) {
			if !strings.Contains(line, "submitted after") {
				r.logf(rec.ID, "%s", strings.ReplaceAll(line, "\n", " · "))
			}
		},
	})
	if err != nil {
		return err
	}
	rep := render.NewReport(in, d, rv)
	if cfg.PublicURL != "" {
		rep.Meta.ReviewURL = strings.TrimRight(cfg.PublicURL, "/") + "/r/" + rec.ID
	}
	if err := r.Store.SaveReport(rec.ID, rep); err != nil {
		return err
	}
	_ = review.SaveState(r.Store.Dir, key, pull.Head.SHA, rv)

	p := rv.Pipeline
	rec.Round, rec.Verdict, rec.Units = p.Round, rv.Verdict, len(rv.Units)
	rec.Bugs, rec.Minor, rec.CostUSD, rec.Seconds = p.Confirmed, p.Minor, p.CostUSD, p.Seconds
	if rv.MergeRisk != nil {
		rec.Risk = rv.MergeRisk.Overall
	}
	rec.Incomplete = review.IncompleteText(p)
	if rv.Design != nil {
		for _, f := range rv.Design.Foundations.Items {
			if f.Status == "confirmed" {
				rec.Design++
			}
		}
	}
	if rec.Incomplete != "" {
		r.logf(rec.ID, "%s", rec.Incomplete)
	}
	r.logf(rec.ID, "Done: %d bugs, %d minor, %d design problems · %s merge risk · $%.2f", rec.Bugs, rec.Minor, rec.Design, rec.Risk, rec.CostUSD)

	if s.Comment {
		var b bytes.Buffer
		render.Markdown(&b, rep)
		var cs []github.ReviewComment
		for _, c := range render.InlineComments(rep) {
			cs = append(cs, github.ReviewComment(c))
		}
		if u, n, err := gh.Review(ctx, rec.Repo, rec.PR, rec.HeadSHA, b.String(), cs); err != nil {
			r.logf(rec.ID, "Could not post the PR review: %v", err)
		} else {
			rec.CommentURL = u
			r.logf(rec.ID, "Posted the review on the PR (%d line comments)", n)
		}
	}
	return nil
}

// sync makes sure a blobless clone of the repo has the PR head and base branch.
// The token is passed per command as a header, never written to .git/config.
func (r *Runner) sync(ctx context.Context, rec *store.Review, repo store.Repo, token string, pull github.Pull) (string, error) {
	r.mu.Lock()
	m := r.repoMu[repo.FullName]
	if m == nil {
		m = &sync.Mutex{}
		r.repoMu[repo.FullName] = m
	}
	r.mu.Unlock()
	m.Lock()
	defer m.Unlock()

	dir := filepath.Join(r.Store.Dir, "clones", strings.ReplaceAll(repo.FullName, "/", "__"))
	auth := []string{}
	if token != "" && strings.HasPrefix(repo.CloneURL, "https://") {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		auth = []string{"-c", "http.extraHeader=Authorization: Basic " + basic}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		r.logf(rec.ID, "Cloning %s (first review of this repo)", repo.FullName)
		_ = os.MkdirAll(filepath.Dir(dir), 0o700)
		args := append(append([]string{}, auth...), "clone", "--filter=blob:none", "--no-checkout", "--quiet", repo.CloneURL, dir)
		if _, err := git(ctx, "", args...); err != nil {
			return "", fmt.Errorf("clone: %w", err)
		}
	}
	r.logf(rec.ID, "Fetching %s and PR head %s", pull.Base.Ref, short(pull.Head.SHA))
	args := append(append([]string{}, auth...), "fetch", "--quiet", "origin",
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", pull.Base.Ref, pull.Base.Ref),
		fmt.Sprintf("+refs/pull/%d/head:refs/remotes/pr/%d", rec.PR, rec.PR))
	if _, err := git(ctx, dir, args...); err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	if _, err := git(ctx, dir, "cat-file", "-e", pull.Head.SHA+"^{commit}"); err != nil {
		return "", fmt.Errorf("PR head %s is not available after fetch", short(pull.Head.SHA))
	}
	return dir, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if i := strings.Index(msg, "Authorization: Basic"); i >= 0 { // never echo the token
			msg = msg[:i] + "Authorization: [hidden]"
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return out.String(), nil
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}
