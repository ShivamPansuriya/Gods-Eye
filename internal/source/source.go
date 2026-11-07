// Package source gathers everything a review needs: the diff, the PR's stated
// intent, linked issues, and the repo's review guidance files. It shells out to
// `gh` (so it uses the user's existing GitHub auth) or to `git` for local mode.
package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Issue is a linked issue (the "ticket" the change claims to address).
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

// Input is the full context for one review.
type Input struct {
	Repo      string            `json:"repo"`
	Number    int               `json:"number,omitempty"`
	URL       string            `json:"url,omitempty"`
	Title     string            `json:"title"`
	Body      string            `json:"body"`
	Author    string            `json:"author"`
	BaseRef   string            `json:"baseRef"`
	HeadRef   string            `json:"headRef"`
	HeadSHA   string            `json:"headSha"`
	Commits   []string          `json:"commits"`
	Issues    []Issue           `json:"issues"`
	Guidance  map[string]string `json:"guidance"` // CLAUDE.md / REVIEW.md / AGENTS.md contents
	RawDiff   string            `json:"-"`
	LocalMode bool              `json:"localMode"`
}

var guidanceFiles = []string{"REVIEW.md", "CLAUDE.md", "AGENTS.md", ".github/REVIEW.md"}

// FromPR loads a pull request with gh. pr may be a number, URL or branch; repo
// may be empty to use the current directory's repo.
func FromPR(pr, repo string) (*Input, error) {
	args := []string{"pr", "view", pr, "--json",
		"number,url,title,body,author,baseRefName,headRefName,headRefOid,commits,closingIssuesReferences,headRepository,headRepositoryOwner"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := run("gh", args...)
	if err != nil {
		return nil, err
	}
	var v struct {
		Number      int    `json:"number"`
		URL         string `json:"url"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		BaseRefName string `json:"baseRefName"`
		HeadRefName string `json:"headRefName"`
		HeadRefOid  string `json:"headRefOid"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
		Commits []struct {
			MessageHeadline string `json:"messageHeadline"`
		} `json:"commits"`
		ClosingIssuesReferences []struct {
			Number int    `json:"number"`
			URL    string `json:"url"`
		} `json:"closingIssuesReferences"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("parse gh pr view: %w", err)
	}
	in := &Input{
		Number: v.Number, URL: v.URL, Title: v.Title, Body: v.Body,
		Author: v.Author.Login, BaseRef: v.BaseRefName, HeadRef: v.HeadRefName, HeadSHA: v.HeadRefOid,
		Guidance: map[string]string{},
	}
	in.Repo = repo
	if in.Repo == "" {
		in.Repo = repoFromURL(v.URL)
	}
	for _, c := range v.Commits {
		in.Commits = append(in.Commits, c.MessageHeadline)
	}

	dargs := []string{"pr", "diff", pr}
	if repo != "" {
		dargs = append(dargs, "--repo", repo)
	}
	d, err := run("gh", dargs...)
	if err != nil {
		return nil, err
	}
	in.RawDiff = string(d)

	for _, ref := range v.ClosingIssuesReferences {
		iout, err := run("gh", "issue", "view", fmt.Sprint(ref.Number), "--repo", in.Repo, "--json", "number,title,body,url")
		if err != nil {
			continue // linked issue may be in another repo or inaccessible; not fatal
		}
		var is Issue
		if json.Unmarshal(iout, &is) == nil {
			in.Issues = append(in.Issues, is)
		}
	}

	for _, f := range guidanceFiles { // from the base branch: a PR cannot instruct its own review
		c, err := run("gh", "api", "-H", "Accept: application/vnd.github.raw",
			fmt.Sprintf("repos/%s/contents/%s?ref=%s", in.Repo, f, url.QueryEscape(in.BaseRef)))
		if err == nil && len(c) > 0 {
			in.Guidance[f] = string(c)
		}
	}
	return in, nil
}

// FromLocal reviews the current branch against base using git (no GitHub needed).
func FromLocal(base string) (*Input, error) {
	if base == "" {
		base = "origin/HEAD"
		if _, err := run("git", "rev-parse", "--verify", "-q", base); err != nil {
			base = "main"
		}
	}
	mb, err := run("git", "merge-base", base, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("find merge base with %s: %w", base, err)
	}
	mbs := strings.TrimSpace(string(mb))
	d, err := run("git", "diff", "--no-color", "--no-ext-diff", mbs, "HEAD")
	if err != nil {
		return nil, err
	}
	in := &Input{RawDiff: string(d), BaseRef: base, LocalMode: true, Guidance: map[string]string{}}
	if b, err := run("git", "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		in.HeadRef = strings.TrimSpace(string(b))
	}
	if b, err := run("git", "rev-parse", "HEAD"); err == nil {
		in.HeadSHA = strings.TrimSpace(string(b))
	}
	if b, err := run("git", "log", "--format=%s", mbs+"..HEAD"); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				in.Commits = append(in.Commits, l)
			}
		}
	}
	if len(in.Commits) > 0 {
		in.Title = in.Commits[len(in.Commits)-1] // oldest commit usually states the goal
	}
	if b, err := run("git", "log", "--format=%B%n---", mbs+"..HEAD"); err == nil {
		in.Body = strings.TrimSpace(string(b))
	}
	if top, err := run("git", "rev-parse", "--show-toplevel"); err == nil {
		root := strings.TrimSpace(string(top))
		in.Repo = filepath.Base(root)
		for _, f := range guidanceFiles {
			if c, err := os.ReadFile(filepath.Join(root, f)); err == nil {
				in.Guidance[f] = string(c)
			}
		}
	}
	return in, nil
}

// FromDiffFile reviews a saved diff (useful for testing and CI).
func FromDiffFile(path, title, body string) (*Input, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return &Input{RawDiff: string(b), Title: title, Body: body, LocalMode: true,
		Repo: strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)), Guidance: map[string]string{}}, nil
}

func repoFromURL(u string) string {
	// https://github.com/owner/repo/pull/12
	u = strings.TrimPrefix(u, "https://")
	parts := strings.Split(u, "/")
	if len(parts) >= 3 {
		return parts[1] + "/" + parts[2]
	}
	return ""
}

func run(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s %s: %s", name, strings.Join(args[:min(2, len(args))], " "), msg)
	}
	return out, nil
}
