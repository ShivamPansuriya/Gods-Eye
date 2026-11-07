// Package remote is the command-line client for a running God's Eye server:
// everything the web UI does (repos, pull requests, reviews, live progress,
// history) from a terminal. Reviews run on the server, so they share its
// history, budget and keys.
package remote

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// Config is where the server is and how to sign in.
type Config struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// ConfigPath is ~/.config/gods-eye/remote.json (mode 0600: it holds the admin token).
func ConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "gods-eye", "remote.json")
}

// LoadConfig reads the saved config; GODSEYE_URL and GODSEYE_TOKEN override it.
func LoadConfig() Config {
	var c Config
	if b, err := os.ReadFile(ConfigPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if v := os.Getenv("GODSEYE_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("GODSEYE_TOKEN"); v != "" {
		c.Token = v
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c
}

func saveConfig(c Config) error {
	p := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, b, 0o600)
}

// Client calls the server's API.
type Client struct {
	Config
	HTTP *http.Client
}

func (c *Client) do(method, path string, body, out any) error {
	if c.URL == "" {
		return errors.New("no server set: run `gods-eye remote login <url>` first")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.URL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return fmt.Errorf("server: %s", e.Error)
		}
		return fmt.Errorf("server: HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Review mirrors the server's review record.
type Review struct {
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	PR         int       `json:"pr"`
	Title      string    `json:"title"`
	Trigger    string    `json:"trigger"`
	Status     string    `json:"status"`
	Round      int       `json:"round"`
	Verdict    string    `json:"verdict"`
	Bugs       int       `json:"bugs"`
	Minor      int       `json:"minor"`
	Design     int       `json:"design"`
	Risk       string    `json:"risk"`
	Incomplete string    `json:"incomplete"`
	CostUSD    float64   `json:"costUsd"`
	Seconds    float64   `json:"seconds"`
	CreatedAt  time.Time `json:"createdAt"`
	Error      string    `json:"error"`
	CommentURL string    `json:"commentUrl"`
	Log        []string  `json:"log"`
}

type pull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Draft     bool       `json:"draft"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Last   *Review `json:"last"`
	Rounds int     `json:"rounds"`
	Stale  bool    `json:"stale"`
}

const Usage = `gods-eye remote — drive a God's Eye server from the terminal

  login <url> [--token T]          save the server URL and admin token (prompted if not given)
  status                           server health: keys, GitHub login, queue, 30-day spend
  repos                            connected repositories
  connect <owner/repo>             connect a repository
  prs <owner/repo> [--merged]      open (or merged) pull requests and their last review
  review <owner/repo> <n> [--fresh] [--no-follow]
                                   start a review on the server and follow it live
  watch <id>                       follow a running review until it ends
  show <id>                        one review's result and log
  history [owner/repo] [--pr N] [--limit N]
                                   past reviews, newest first
  cancel <id>                      cancel a queued or running review
  open <id>                        open the full review page in the browser

GODSEYE_URL and GODSEYE_TOKEN override the saved login.
`

// Main runs one remote command. openBrowser opens a URL; out and in are the terminal.
func Main(args []string, out io.Writer, in io.Reader, openBrowser func(string)) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(out, Usage)
		return nil
	}
	c := &Client{Config: LoadConfig()}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "login":
		return login(c, rest, out, in)
	case "status":
		return status(c, out)
	case "repos":
		return repos(c, out)
	case "connect":
		if len(rest) != 1 {
			return errors.New("usage: connect <owner/repo>")
		}
		var r struct {
			FullName string `json:"fullName"`
		}
		if err := c.do("POST", "/api/repos", map[string]string{"fullName": rest[0]}, &r); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Connected %s\n", r.FullName)
		return nil
	case "prs", "pulls":
		return prs(c, rest, out)
	case "review":
		return review(c, rest, out)
	case "watch":
		if len(rest) != 1 {
			return errors.New("usage: watch <id>")
		}
		return Watch(c, rest[0], out, 2*time.Second)
	case "show":
		if len(rest) != 1 {
			return errors.New("usage: show <id>")
		}
		return show(c, rest[0], out)
	case "history":
		return history(c, rest, out)
	case "cancel":
		if len(rest) != 1 {
			return errors.New("usage: cancel <id>")
		}
		if err := c.do("POST", "/api/reviews/"+url.PathEscape(rest[0])+"/cancel", nil, nil); err != nil {
			return err
		}
		fmt.Fprintf(out, "✓ Cancelled %s\n", rest[0])
		return nil
	case "open":
		if len(rest) != 1 {
			return errors.New("usage: open <id>")
		}
		u := c.URL + "/r/" + url.PathEscape(rest[0])
		fmt.Fprintln(out, u)
		if openBrowser != nil {
			openBrowser(u)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", cmd, Usage)
}

func login(c *Client, args []string, out io.Writer, in io.Reader) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	tok := fs.String("token", "", "admin token")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: login <url> [--token T]")
	}
	u := strings.TrimRight(fs.Arg(0), "/")
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	t := *tok
	if t == "" {
		fmt.Fprint(out, "Admin token (printed when the server starts): ")
		t = readSecret(in)
		fmt.Fprintln(out)
	}
	c.Config = Config{URL: u, Token: t}
	if err := status(c, out); err != nil {
		return err
	}
	if err := saveConfig(c.Config); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ Signed in; saved to %s\n", ConfigPath())
	return nil
}

// readSecret reads one line; on a terminal it turns echo off so the token
// is not shown.
func readSecret(in io.Reader) string {
	if f, ok := in.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			stty := func(arg string) {
				c := exec.Command("stty", arg)
				c.Stdin = f
				_ = c.Run()
			}
			stty("-echo")
			defer stty("echo")
		}
	}
	line, _ := bufio.NewReader(in).ReadString('\n')
	return strings.TrimSpace(line)
}

func status(c *Client, out io.Writer) error {
	var s struct {
		Anthropic     bool    `json:"anthropic"`
		GitHubToken   bool    `json:"githubToken"`
		GitHubUser    string  `json:"githubUser"`
		PublicURL     string  `json:"publicUrl"`
		Queued        int     `json:"queued"`
		Running       int     `json:"running"`
		Spend30d      float64 `json:"spend30d"`
		MonthlyBudget float64 `json:"monthlyBudget"`
	}
	if err := c.do("GET", "/api/status", nil, &s); err != nil {
		return err
	}
	gh := "not set"
	if s.GitHubToken {
		gh = s.GitHubUser
	}
	budget := ""
	if s.MonthlyBudget > 0 {
		budget = fmt.Sprintf(" of $%.0f", s.MonthlyBudget)
	}
	fmt.Fprintf(out, "Server   %s\nClaude   %s\nGitHub   %s\nQueue    %d running, %d queued\nSpend    $%.2f%s in 30 days\n",
		c.URL, yesNo(s.Anthropic), gh, s.Running, s.Queued, s.Spend30d, budget)
	return nil
}

func repos(c *Client, out io.Writer) error {
	var rs []struct {
		FullName string `json:"fullName"`
		HookID   int64  `json:"hookId"`
		Settings struct {
			AutoReview bool    `json:"autoReview"`
			Depth      string  `json:"depth"`
			Budget     float64 `json:"budget"`
		} `json:"settings"`
	}
	if err := c.do("GET", "/api/repos", nil, &rs); err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Fprintln(out, "No repositories connected. Run: gods-eye remote connect <owner/repo>")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "REPOSITORY\tAUTO REVIEW\tWEBHOOK\tDEPTH\tBASE BUDGET")
	for _, r := range rs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t$%.0f\n", r.FullName, onOff(r.Settings.AutoReview), yesNo(r.HookID != 0), r.Settings.Depth, r.Settings.Budget)
	}
	return w.Flush()
}

func prs(c *Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("prs", flag.ContinueOnError)
	merged := fs.Bool("merged", false, "list merged pull requests")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: prs <owner/repo> [--merged]")
	}
	state := "open"
	if *merged {
		state = "merged"
	}
	var ps []pull
	if err := c.do("GET", "/api/repos/"+fs.Arg(0)+"/pulls?state="+state, nil, &ps); err != nil {
		return err
	}
	if len(ps) == 0 {
		fmt.Fprintf(out, "No %s pull requests.\n", state)
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	when := "UPDATED"
	if *merged {
		when = "MERGED"
	}
	fmt.Fprintf(w, "#\tTITLE\tAUTHOR\t%s\tLAST REVIEW\n", when)
	for _, p := range ps {
		t := p.UpdatedAt
		if p.MergedAt != nil {
			t = *p.MergedAt
		}
		title := clip(p.Title, 48)
		if p.Draft {
			title = "[draft] " + title
		}
		last := "not reviewed"
		if p.Last != nil {
			last = summary(*p.Last)
			if p.Stale {
				last += " · new commits"
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", p.Number, title, p.User.Login, ago(t), last)
	}
	return w.Flush()
}

func review(c *Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fresh := fs.Bool("fresh", false, "full re-review: ignore earlier rounds")
	noFollow := fs.Bool("no-follow", false, "queue it and return")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: review <owner/repo> <n> [--fresh] [--no-follow]")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(fs.Arg(1), "#"))
	if err != nil || n <= 0 {
		return fmt.Errorf("bad PR number %q", fs.Arg(1))
	}
	var r Review
	if err := c.do("POST", fmt.Sprintf("/api/repos/%s/pulls/%d/review", fs.Arg(0), n), map[string]bool{"fresh": *fresh}, &r); err != nil {
		return err
	}
	fmt.Fprintf(out, "Queued review %s\n", r.ID)
	if *noFollow {
		fmt.Fprintf(out, "Follow it with: gods-eye remote watch %s\n", r.ID)
		return nil
	}
	return Watch(c, r.ID, out, 2*time.Second)
}

// Watch prints a review's new log lines until it ends, then its result.
// It returns an error when the review failed, so scripts can tell.
func Watch(c *Client, id string, out io.Writer, every time.Duration) error {
	seen := 0
	for {
		var res struct {
			Review Review `json:"review"`
		}
		if err := c.do("GET", "/api/reviews/"+url.PathEscape(id), nil, &res); err != nil {
			return err
		}
		r := res.Review
		if seen > len(r.Log) {
			seen = 0 // the server swapped the live log for the saved one
		}
		for _, l := range r.Log[seen:] {
			fmt.Fprintln(out, l)
		}
		seen = len(r.Log)
		switch r.Status {
		case "done", "failed", "cancelled":
			return result(c, r, out)
		}
		time.Sleep(every)
	}
}

func result(c *Client, r Review, out io.Writer) error {
	switch r.Status {
	case "failed":
		fmt.Fprintf(out, "✗ failed: %s\n", r.Error)
		return fmt.Errorf("review %s failed", r.ID)
	case "cancelled":
		fmt.Fprintln(out, "· cancelled")
		return nil
	}
	fmt.Fprintf(out, "✓ %s · %s\n", summary(r), (time.Duration(r.Seconds) * time.Second).String())
	if r.Incomplete != "" {
		fmt.Fprintf(out, "  ⚠ %s\n", r.Incomplete)
	}
	fmt.Fprintf(out, "  %s/r/%s\n", c.URL, r.ID)
	if r.CommentURL != "" {
		fmt.Fprintf(out, "  posted on the PR: %s\n", r.CommentURL)
	}
	return nil
}

func show(c *Client, id string, out io.Writer) error {
	var res struct {
		Review Review `json:"review"`
	}
	if err := c.do("GET", "/api/reviews/"+url.PathEscape(id), nil, &res); err != nil {
		return err
	}
	r := res.Review
	fmt.Fprintf(out, "%s #%d %s\nround %d · %s · %s\n\n", r.Repo, r.PR, r.Title, r.Round, r.Trigger, r.CreatedAt.Local().Format("2006-01-02 15:04"))
	for _, l := range r.Log {
		fmt.Fprintln(out, l)
	}
	if r.Status == "queued" || r.Status == "running" {
		fmt.Fprintf(out, "… %s. Follow it with: gods-eye remote watch %s\n", r.Status, r.ID)
		return nil
	}
	return result(c, r, out)
}

func history(c *Client, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	pr := fs.Int("pr", 0, "only this pull request")
	limit := fs.Int("limit", 30, "how many")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	q := url.Values{"limit": {strconv.Itoa(*limit)}}
	if fs.NArg() > 0 {
		q.Set("repo", fs.Arg(0))
	}
	if *pr > 0 {
		q.Set("pr", strconv.Itoa(*pr))
	}
	var rs []Review
	if err := c.do("GET", "/api/reviews?"+q.Encode(), nil, &rs); err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Fprintln(out, "No reviews yet.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPULL REQUEST\tROUND\tRESULT\tWHEN")
	for _, r := range rs {
		fmt.Fprintf(w, "%s\t%s #%d %s\t%d\t%s\t%s\n", r.ID, r.Repo, r.PR, clip(r.Title, 36), r.Round, summary(r), ago(r.CreatedAt))
	}
	return w.Flush()
}

var verdicts = map[string]string{"approve": "Looks good", "request_changes": "Changes needed", "needs_decision": "Needs a decision", "incomplete": "⚠ Not finished"}

// summary is one review's result in one line; it never says "0 bugs" for an
// unfinished review.
func summary(r Review) string {
	switch r.Status {
	case "queued", "running", "failed", "cancelled":
		return r.Status
	}
	parts := []string{}
	if v := verdicts[r.Verdict]; v != "" {
		parts = append(parts, v)
	}
	bugs := fmt.Sprintf("%d bug%s", r.Bugs, map[bool]string{true: "", false: "s"}[r.Bugs == 1])
	if r.Incomplete != "" {
		bugs += " so far"
	}
	parts = append(parts, bugs)
	if r.Minor > 0 {
		parts = append(parts, fmt.Sprintf("%d minor", r.Minor))
	}
	if r.Risk != "" {
		parts = append(parts, r.Risk+" risk")
	}
	parts = append(parts, fmt.Sprintf("$%.2f", r.CostUSD))
	return strings.Join(parts, " · ")
}

// reorder puts flags before positional arguments, so `prs acme/shop --merged` works.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && takesValue(a) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func takesValue(f string) bool {
	switch strings.TrimLeft(f, "-") {
	case "token", "pr", "limit":
		return true
	}
	return false
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func yesNo(b bool) string {
	if b {
		return "✓"
	}
	return "✗ not set"
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
