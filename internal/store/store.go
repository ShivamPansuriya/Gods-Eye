// Package store keeps God's Eye data on disk as JSON files: settings,
// connected repositories, and every review ever run (record + full report).
// The CLI and the server use the same layout, so both see the same history.
//
//	<dir>/config.json                  server settings (0600: holds the GitHub token)
//	<dir>/repos/<owner>__<repo>.json   a connected repository and its review settings
//	<dir>/reviews/<id>.json            one review run: status, verdict, counts, log
//	<dir>/reviews/<id>.report.json     the full report the review page is built from
//	<dir>/state/…                      incremental-round state (see review.State)
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is the server's settings.
type Config struct {
	GitHubToken   string  `json:"githubToken,omitempty"`
	GitHubAPI     string  `json:"githubApi,omitempty"`     // default https://api.github.com
	PublicURL     string  `json:"publicUrl,omitempty"`     // where GitHub can reach this server (for webhooks)
	WebhookSecret string  `json:"webhookSecret"`           // signs every webhook this server installs
	AdminToken    string  `json:"adminToken"`              // password for the web UI
	Concurrency   int     `json:"concurrency"`             // reviews that may run at once
	Model         string  `json:"model,omitempty"`         // default claude-opus-5-5
	MonthlyBudget float64 `json:"monthlyBudget,omitempty"` // stop auto reviews above this spend (USD, 0 = no limit)
	AnthropicKey  string  `json:"anthropicKey,omitempty"`  // used when no key is set in the environment
}

// RepoSettings controls reviews for one repository.
type RepoSettings struct {
	AutoReview bool    `json:"autoReview"` // review automatically on webhook events
	OnOpen     bool    `json:"onOpen"`     // when a PR is opened, reopened or marked ready
	OnPush     bool    `json:"onPush"`     // when new commits are pushed (incremental round)
	SkipDrafts bool    `json:"skipDrafts"`
	Depth      string  `json:"depth"`   // auto | quick | standard | deep
	Effort     string  `json:"effort"`  // low … max
	Budget     float64 `json:"budget"`  // max spend per review, USD
	Comment    bool    `json:"comment"` // post a summary comment on the PR
}

// DefaultSettings are applied when a repository is connected.
func DefaultSettings() RepoSettings {
	return RepoSettings{AutoReview: false, OnOpen: true, OnPush: true, SkipDrafts: true, Depth: "auto", Effort: "high", Budget: 15, Comment: true}
}

// Repo is a connected repository.
type Repo struct {
	FullName      string       `json:"fullName"`
	CloneURL      string       `json:"cloneUrl"`
	HTMLURL       string       `json:"htmlUrl"`
	DefaultBranch string       `json:"defaultBranch"`
	Private       bool         `json:"private"`
	AddedAt       time.Time    `json:"addedAt"`
	Settings      RepoSettings `json:"settings"`
	HookID        int64        `json:"hookId,omitempty"`
	HookURL       string       `json:"hookUrl,omitempty"`
	HookError     string       `json:"hookError,omitempty"`
}

// Review is one review run.
type Review struct {
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	PR         int       `json:"pr"`
	Title      string    `json:"title"`
	Author     string    `json:"author,omitempty"`
	HeadSHA    string    `json:"headSha"`
	Trigger    string    `json:"trigger"` // manual | webhook | cli
	Status     string    `json:"status"`  // queued | running | done | failed | cancelled
	Round      int       `json:"round,omitempty"`
	Verdict    string    `json:"verdict,omitempty"`
	Bugs       int       `json:"bugs"`
	Minor      int       `json:"minor"`
	Design     int       `json:"design"`         // verified problems in the existing design
	Risk       string    `json:"risk,omitempty"` // merge risk: low | medium | high
	Units      int       `json:"units"`
	CostUSD    float64   `json:"costUsd"`
	Seconds    float64   `json:"seconds"`
	CreatedAt  time.Time `json:"createdAt"`
	StartedAt  time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	Error      string    `json:"error,omitempty"`
	CommentURL string    `json:"commentUrl,omitempty"`
	Log        []string  `json:"log,omitempty"`
}

// Store is the on-disk database. Safe for concurrent use.
type Store struct {
	Dir string
	mu  sync.Mutex
}

// Open creates the directory layout if needed.
func Open(dir string) (*Store, error) {
	for _, sub := range []string{"repos", "reviews", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{Dir: dir}, nil
}

func (s *Store) write(path string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path) // atomic: readers never see half a file
}

func read(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// LoadConfig reads the config, creating secrets on first use.
func (s *Store) LoadConfig() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Config
	err := read(filepath.Join(s.Dir, "config.json"), &c)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	changed := false
	if c.WebhookSecret == "" {
		c.WebhookSecret, changed = NewSecret(32), true
	}
	if c.AdminToken == "" {
		c.AdminToken, changed = NewSecret(16), true
	}
	if c.Concurrency <= 0 {
		c.Concurrency, changed = 2, true
	}
	if changed {
		err = s.write(filepath.Join(s.Dir, "config.json"), c, 0o600)
	}
	return c, err
}

// SaveConfig writes the config.
func (s *Store) SaveConfig(c Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(filepath.Join(s.Dir, "config.json"), c, 0o600)
}

func repoFile(dir, fullName string) string {
	return filepath.Join(dir, "repos", strings.ReplaceAll(fullName, "/", "__")+".json")
}

// Repos lists connected repositories by name.
func (s *Store) Repos() ([]Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, _ := filepath.Glob(filepath.Join(s.Dir, "repos", "*.json"))
	var rs []Repo
	for _, f := range files {
		var r Repo
		if read(f, &r) == nil {
			rs = append(rs, r)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].FullName < rs[j].FullName })
	return rs, nil
}

// Repo returns one connected repository.
func (s *Store) Repo(fullName string) (Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Repo
	if read(repoFile(s.Dir, fullName), &r) != nil {
		return r, false
	}
	return r, true
}

// SaveRepo writes a repository.
func (s *Store) SaveRepo(r Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(repoFile(s.Dir, r.FullName), r, 0o600)
}

// DeleteRepo disconnects a repository (its review history is kept).
func (s *Store) DeleteRepo(fullName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(repoFile(s.Dir, fullName))
}

// NewReviewID returns a sortable unique id: time first, then randomness.
func NewReviewID() string {
	return time.Now().UTC().Format("20060102-150405") + "-" + NewSecret(3)
}

// SaveReview writes a review record.
func (s *Store) SaveReview(r Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(filepath.Join(s.Dir, "reviews", r.ID+".json"), r, 0o600)
}

// Review returns one review record.
func (s *Store) Review(id string) (Review, bool) {
	if !validID(id) {
		return Review{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var r Review
	if read(filepath.Join(s.Dir, "reviews", id+".json"), &r) != nil {
		return r, false
	}
	return r, true
}

// Reviews lists review records, newest first, optionally for one repo and PR (pr 0 = all).
func (s *Store) Reviews(repo string, pr int, limit int) ([]Review, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, _ := filepath.Glob(filepath.Join(s.Dir, "reviews", "*.json"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	var out []Review
	for _, f := range files {
		if strings.HasSuffix(f, ".report.json") {
			continue
		}
		var r Review
		if read(f, &r) != nil {
			continue
		}
		if (repo != "" && r.Repo != repo) || (pr != 0 && r.PR != pr) {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// SaveReport writes the full report for a review.
func (s *Store) SaveReport(id string, report any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.Dir, "reviews", id+".report.json"), b, 0o600)
}

// ReportPath is where a review's report lives.
func (s *Store) ReportPath(id string) (string, bool) {
	if !validID(id) {
		return "", false
	}
	p := filepath.Join(s.Dir, "reviews", id+".report.json")
	_, err := os.Stat(p)
	return p, err == nil
}

// SpendSince sums the cost of reviews created after t.
func (s *Store) SpendSince(t time.Time) float64 {
	rs, _ := s.Reviews("", 0, 0)
	total := 0.0
	for _, r := range rs {
		if r.CreatedAt.After(t) {
			total += r.CostUSD
		}
	}
	return total
}

// NewSecret returns n random bytes as hex.
func NewSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}

func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c == '-') {
			return false
		}
	}
	return true
}
