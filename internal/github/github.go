// Package github is a small GitHub REST/GraphQL client for what God's Eye needs:
// list repos and pull requests, read a PR's metadata, post comments, and
// install webhooks. It uses net/http directly, so the server needs no gh CLI.
package github

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to GitHub (or GitHub Enterprise via BaseURL).
type Client struct {
	Token   string
	BaseURL string // default https://api.github.com
	HTTP    *http.Client
}

// New returns a client for the given token and API base URL.
func New(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &Client{Token: token, BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Error is a non-2xx response.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("github: %d %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, body any, accept string, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.BaseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gods-eye")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(data[:min(len(data), 200)]))
		}
		return &Error{Status: resp.StatusCode, Message: m.Message}
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *string:
		*o = string(data)
		return nil
	default:
		return json.Unmarshal(data, out)
	}
}

// User is the authenticated account.
type User struct {
	Login string `json:"login"`
}

// Me returns the token's user.
func (c *Client) Me(ctx context.Context) (User, error) {
	var u User
	err := c.do(ctx, "GET", "/user", nil, "", &u)
	return u, err
}

// Repo is a repository the token can see.
type Repo struct {
	FullName      string    `json:"full_name"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"default_branch"`
	CloneURL      string    `json:"clone_url"`
	HTMLURL       string    `json:"html_url"`
	UpdatedAt     time.Time `json:"updated_at"`
	Permissions   struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
	} `json:"permissions"`
}

// Repos lists repositories the token can access (most recently updated first).
func (c *Client) Repos(ctx context.Context, page int) ([]Repo, error) {
	var rs []Repo
	err := c.do(ctx, "GET", fmt.Sprintf("/user/repos?per_page=100&sort=updated&page=%d", max(page, 1)), nil, "", &rs)
	return rs, err
}

// Repo returns one repository.
func (c *Client) Repo(ctx context.Context, fullName string) (Repo, error) {
	var r Repo
	err := c.do(ctx, "GET", "/repos/"+fullName, nil, "", &r)
	return r, err
}

// Pull is a pull request.
type Pull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
}

// Pulls lists open pull requests.
func (c *Client) Pulls(ctx context.Context, fullName string) ([]Pull, error) {
	var ps []Pull
	err := c.do(ctx, "GET", "/repos/"+fullName+"/pulls?state=open&per_page=50&sort=updated&direction=desc", nil, "", &ps)
	return ps, err
}

// Pull returns one pull request.
func (c *Client) Pull(ctx context.Context, fullName string, n int) (Pull, error) {
	var p Pull
	err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", fullName, n), nil, "", &p)
	return p, err
}

// Commits returns the full messages of a PR's commits (for trailers such as Co-Authored-By).
func (c *Client) Commits(ctx context.Context, fullName string, n int) ([]string, error) {
	var cs []struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d/commits?per_page=100", fullName, n), nil, "", &cs); err != nil {
		return nil, err
	}
	out := make([]string, len(cs))
	for i, x := range cs {
		out[i] = x.Commit.Message
	}
	return out, nil
}

// Issue is a linked issue.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

// ClosingIssues returns the issues a PR closes (GraphQL). Errors are not fatal for a review.
func (c *Client) ClosingIssues(ctx context.Context, fullName string, n int) ([]Issue, error) {
	owner, name, _ := strings.Cut(fullName, "/")
	q := map[string]any{
		"query":     `query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){closingIssuesReferences(first:10){nodes{number title body url}}}}}`,
		"variables": map[string]any{"o": owner, "n": name, "p": n},
	}
	var out struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Closing struct {
						Nodes []Issue `json:"nodes"`
					} `json:"closingIssuesReferences"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	gql := c.BaseURL + "/graphql"
	if strings.HasSuffix(c.BaseURL, "/api/v3") { // GitHub Enterprise Server
		gql = strings.TrimSuffix(c.BaseURL, "/v3") + "/graphql"
	}
	if err := c.do(ctx, "POST", gql, q, "", &out); err != nil {
		return nil, err
	}
	return out.Data.Repository.PullRequest.Closing.Nodes, nil
}

// File returns a file's raw contents at a ref ("" if it does not exist).
func (c *Client) File(ctx context.Context, fullName, path, ref string) (string, error) {
	var s string
	err := c.do(ctx, "GET", "/repos/"+fullName+"/contents/"+path+"?ref="+url.QueryEscape(ref), nil, "application/vnd.github.raw", &s)
	if e, ok := err.(*Error); ok && e.Status == 404 {
		return "", nil
	}
	return s, err
}

// Comment posts a comment on a PR and returns its URL.
func (c *Client) Comment(ctx context.Context, fullName string, n int, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	err := c.do(ctx, "POST", fmt.Sprintf("/repos/%s/issues/%d/comments", fullName, n), map[string]string{"body": body}, "", &out)
	return out.HTMLURL, err
}

// ReviewComment is a comment pinned to one line of a PR's diff.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// Review posts a PR review: the summary as its body and each comment on its
// line. If GitHub refuses a line (422, e.g. the line left the diff), it posts
// the summary as a plain comment instead so nothing is lost.
func (c *Client) Review(ctx context.Context, fullName string, n int, commit, body string, comments []ReviewComment) (string, int, error) {
	if len(comments) == 0 {
		u, err := c.Comment(ctx, fullName, n, body)
		return u, 0, err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	req := map[string]any{"event": "COMMENT", "body": body, "comments": comments}
	if commit != "" {
		req["commit_id"] = commit
	}
	err := c.do(ctx, "POST", fmt.Sprintf("/repos/%s/pulls/%d/reviews", fullName, n), req, "", &out)
	if e, ok := err.(*Error); ok && e.Status == 422 {
		u, err := c.Comment(ctx, fullName, n, body)
		return u, 0, err
	}
	return out.HTMLURL, len(comments), err
}

// Hook is a repository webhook.
type Hook struct {
	ID     int64    `json:"id"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

// Hooks lists a repository's webhooks (needs admin rights).
func (c *Client) Hooks(ctx context.Context, fullName string) ([]Hook, error) {
	var hs []Hook
	err := c.do(ctx, "GET", "/repos/"+fullName+"/hooks", nil, "", &hs)
	return hs, err
}

// CreateHook installs a pull_request webhook pointing at url, signed with secret.
func (c *Client) CreateHook(ctx context.Context, fullName, hookURL, secret string) (Hook, error) {
	body := map[string]any{
		"name": "web", "active": true, "events": []string{"pull_request"},
		"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret, "insecure_ssl": "0"},
	}
	var h Hook
	err := c.do(ctx, "POST", "/repos/"+fullName+"/hooks", body, "", &h)
	return h, err
}

// DeleteHook removes a webhook.
func (c *Client) DeleteHook(ctx context.Context, fullName string, id int64) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("/repos/%s/hooks/%d", fullName, id), nil, "", nil)
}

// VerifySignature checks GitHub's X-Hub-Signature-256 header against the body.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}

// Sign returns the X-Hub-Signature-256 value for a body (used in tests and tools).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// PullRequestEvent is the part of a pull_request webhook payload we use.
type PullRequestEvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest Pull   `json:"pull_request"`
	Repository  struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}
