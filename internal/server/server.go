// Package server is God's Eye as a self-hosted app: connect GitHub repos,
// review PRs from the UI or automatically from webhooks, and keep every review.
package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/ShivamPansuriya/gods-eye/internal/github"
	"github.com/ShivamPansuriya/gods-eye/internal/render"
	"github.com/ShivamPansuriya/gods-eye/internal/store"
)

//go:embed app.html
var appHTML []byte

// Server is the web app.
type Server struct {
	Store  *store.Store
	Runner *Runner

	mu  sync.RWMutex
	cfg store.Config
	// AnthropicKey from the environment; a key saved in Settings is used when this is empty.
	envKey string
}

// New opens the store and prepares the server.
func New(dir string) (*Server, error) {
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return nil, err
	}
	if t := os.Getenv("GODSEYE_ADMIN_TOKEN"); t != "" {
		cfg.AdminToken = t
	}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" && cfg.GitHubToken == "" {
		cfg.GitHubToken = t
	}
	s := &Server{Store: st, cfg: cfg}
	s.envKey = os.Getenv("ANTHROPIC_API_KEY")
	if s.envKey == "" {
		s.envKey = os.Getenv("CLAUDE_API_KEY")
	}
	s.Runner = NewRunner(st, s.config, s.anthropic)
	return s, nil
}

// AdminToken is the password for the web UI.
func (s *Server) AdminToken() string { return s.config().AdminToken }

func (s *Server) config() store.Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Server) anthropic() (anthropic.Client, bool) {
	key := s.envKey
	if key == "" {
		key = s.config().AnthropicKey
	}
	if key == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		return anthropic.Client{}, false
	}
	opts := []option.RequestOption{option.WithMaxRetries(5)}
	if key != "" {
		opts = append(opts, option.WithAPIKey(key))
	}
	return anthropic.NewClient(opts...), true
}

func (s *Server) gh() *github.Client {
	c := s.config()
	return github.New(c.GitHubToken, c.GitHubAPI)
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(appHTML)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ge_admin", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /webhook/github", s.webhook)

	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("GET /api/status", s.status)
	api("GET /api/config", s.getConfig)
	api("PUT /api/config", s.putConfig)
	api("GET /api/github/repos", s.githubRepos)
	api("GET /api/repos", s.listRepos)
	api("POST /api/repos", s.connectRepo)
	api("DELETE /api/repos/{owner}/{repo}", s.disconnectRepo)
	api("PUT /api/repos/{owner}/{repo}/settings", s.putSettings)
	api("POST /api/repos/{owner}/{repo}/webhook", s.installHook)
	api("DELETE /api/repos/{owner}/{repo}/webhook", s.removeHook)
	api("GET /api/repos/{owner}/{repo}/pulls", s.pulls)
	api("POST /api/repos/{owner}/{repo}/pulls/{n}/review", s.startReview)
	api("GET /api/reviews", s.listReviews)
	api("GET /api/reviews/{id}", s.getReview)
	api("POST /api/reviews/{id}/cancel", s.cancelReview)
	api("GET /r/{id}", s.reviewPage)
	return secureHeaders(mux)
}

// Run starts the workers and serves until ctx is done.
func (s *Server) Run(ctx context.Context, addr string) error {
	s.Runner.Start(ctx, s.config().Concurrency)
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
	}()
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// ---- auth ----

func (s *Server) authorized(r *http.Request) bool {
	want := s.config().AdminToken
	got := ""
	if c, err := r.Cookie("ge_admin"); err == nil {
		got = c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	}
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) auth(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			if strings.HasPrefix(r.URL.Path, "/r/") {
				http.Redirect(w, r, "/#login", http.StatusSeeOther)
				return
			}
			writeJSON(w, 401, map[string]string{"error": "Sign in first."})
			return
		}
		h(w, r)
	})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	want := s.config().AdminToken
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Token)), []byte(want)) != 1 {
		time.Sleep(400 * time.Millisecond) // slow down guessing
		writeJSON(w, 401, map[string]string{"error": "That admin token is not right. It is printed when the server starts."})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "ge_admin", Value: want, Path: "/", MaxAge: 30 * 24 * 3600, HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || strings.HasPrefix(s.config().PublicURL, "https://")})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- status and config ----

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	out := map[string]any{"githubToken": cfg.GitHubToken != "", "publicUrl": cfg.PublicURL}
	if cfg.GitHubToken != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if u, err := s.gh().Me(ctx); err == nil {
			out["githubUser"] = u.Login
		} else {
			out["githubError"] = err.Error()
		}
	}
	_, ok := s.anthropic()
	out["anthropic"] = ok
	out["anthropicFromEnv"] = s.envKey != ""
	if cfg.PublicURL != "" {
		out["webhookUrl"] = strings.TrimRight(cfg.PublicURL, "/") + "/webhook/github"
	}
	out["spend30d"] = s.Store.SpendSince(time.Now().AddDate(0, -1, 0))
	out["monthlyBudget"] = cfg.MonthlyBudget
	rs, _ := s.Store.Reviews("", 0, 300)
	q, run := 0, 0
	for _, x := range rs {
		switch x.Status {
		case "queued":
			q++
		case "running":
			run++
		}
	}
	out["queued"], out["running"] = q, run
	writeJSON(w, 200, out)
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	c := s.config()
	writeJSON(w, 200, map[string]any{
		"githubToken": mask(c.GitHubToken), "githubApi": c.GitHubAPI, "publicUrl": c.PublicURL,
		"concurrency": c.Concurrency, "model": c.Model, "monthlyBudget": c.MonthlyBudget,
		"anthropicKey": mask(c.AnthropicKey), "anthropicFromEnv": s.envKey != "",
	})
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GitHubToken   *string  `json:"githubToken"`
		GitHubAPI     *string  `json:"githubApi"`
		PublicURL     *string  `json:"publicUrl"`
		Concurrency   *int     `json:"concurrency"`
		Model         *string  `json:"model"`
		MonthlyBudget *float64 `json:"monthlyBudget"`
		AnthropicKey  *string  `json:"anthropicKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Send JSON."})
		return
	}
	s.mu.Lock()
	c := s.cfg
	if in.GitHubToken != nil && !strings.Contains(*in.GitHubToken, "•") {
		c.GitHubToken = strings.TrimSpace(*in.GitHubToken)
	}
	if in.AnthropicKey != nil && !strings.Contains(*in.AnthropicKey, "•") {
		c.AnthropicKey = strings.TrimSpace(*in.AnthropicKey)
	}
	if in.GitHubAPI != nil {
		c.GitHubAPI = strings.TrimRight(strings.TrimSpace(*in.GitHubAPI), "/")
	}
	if in.PublicURL != nil {
		c.PublicURL = strings.TrimRight(strings.TrimSpace(*in.PublicURL), "/")
	}
	if in.Concurrency != nil && *in.Concurrency > 0 && *in.Concurrency <= 8 {
		c.Concurrency = *in.Concurrency
	}
	if in.Model != nil {
		c.Model = strings.TrimSpace(*in.Model)
	}
	if in.MonthlyBudget != nil && *in.MonthlyBudget >= 0 {
		c.MonthlyBudget = *in.MonthlyBudget
	}
	s.cfg = c
	s.mu.Unlock()
	if err := s.Store.SaveConfig(c); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.getConfig(w, r)
}

// ---- repositories ----

func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request) {
	if s.config().GitHubToken == "" {
		writeJSON(w, 400, map[string]string{"error": "Add a GitHub token in Settings first."})
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	rs, err := s.gh().Repos(r.Context(), page)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": err.Error()})
		return
	}
	connected := map[string]bool{}
	mine, _ := s.Store.Repos()
	for _, m := range mine {
		connected[m.FullName] = true
	}
	type row struct {
		github.Repo
		Connected bool `json:"connected"`
	}
	out := make([]row, len(rs))
	for i, x := range rs {
		out[i] = row{x, connected[x.FullName]}
	}
	writeJSON(w, 200, out)
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	rs, _ := s.Store.Repos()
	type row struct {
		store.Repo
		Last    *store.Review `json:"last,omitempty"`
		Reviews int           `json:"reviews"`
	}
	out := make([]row, 0, len(rs))
	for _, x := range rs {
		h, _ := s.Store.Reviews(x.FullName, 0, 0)
		rw := row{Repo: x, Reviews: len(h)}
		if len(h) > 0 {
			l := h[0]
			l.Log = nil
			rw.Last = &l
		}
		out = append(out, rw)
	}
	writeJSON(w, 200, out)
}

func (s *Server) connectRepo(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FullName string `json:"fullName"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	name := strings.Trim(strings.TrimSpace(in.FullName), "/")
	if strings.Count(name, "/") != 1 {
		writeJSON(w, 400, map[string]string{"error": "Use owner/name."})
		return
	}
	gr, err := s.gh().Repo(r.Context(), name)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "GitHub: " + err.Error()})
		return
	}
	rp, exists := s.Store.Repo(gr.FullName)
	if !exists {
		rp = store.Repo{FullName: gr.FullName, AddedAt: time.Now().UTC(), Settings: store.DefaultSettings()}
	}
	rp.CloneURL, rp.HTMLURL, rp.DefaultBranch, rp.Private = gr.CloneURL, gr.HTMLURL, gr.DefaultBranch, gr.Private
	if err := s.Store.SaveRepo(rp); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, rp)
}

func repoName(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("repo") }

func (s *Server) disconnectRepo(w http.ResponseWriter, r *http.Request) {
	name := repoName(r)
	if rp, ok := s.Store.Repo(name); ok && rp.HookID != 0 {
		_ = s.gh().DeleteHook(r.Context(), name, rp.HookID)
	}
	_ = s.Store.DeleteRepo(name)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.Store.Repo(repoName(r))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "Not connected."})
		return
	}
	var st store.RepoSettings
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&st); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Send JSON."})
		return
	}
	if st.Depth != "auto" && st.Depth != "quick" && st.Depth != "standard" && st.Depth != "deep" {
		st.Depth = "auto"
	}
	switch st.Effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		st.Effort = "high"
	}
	if st.Budget <= 0 || st.Budget > 200 {
		st.Budget = 15
	}
	rp.Settings = st
	_ = s.Store.SaveRepo(rp)
	writeJSON(w, 200, rp)
}

func (s *Server) installHook(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	rp, ok := s.Store.Repo(repoName(r))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "Not connected."})
		return
	}
	if cfg.PublicURL == "" {
		writeJSON(w, 400, map[string]string{"error": "Set the public URL in Settings first: GitHub must be able to reach this server."})
		return
	}
	url := cfg.PublicURL + "/webhook/github"
	gh := s.gh()
	if hs, err := gh.Hooks(r.Context(), rp.FullName); err == nil {
		for _, h := range hs {
			if h.Config.URL == url { // already installed (e.g. by an earlier connect)
				rp.HookID, rp.HookURL, rp.HookError = h.ID, url, ""
				_ = s.Store.SaveRepo(rp)
				writeJSON(w, 200, rp)
				return
			}
		}
	}
	h, err := gh.CreateHook(r.Context(), rp.FullName, url, cfg.WebhookSecret)
	if err != nil {
		rp.HookError = err.Error()
		_ = s.Store.SaveRepo(rp)
		writeJSON(w, 502, map[string]string{"error": "GitHub refused the webhook: " + err.Error() + ". The token needs admin rights on the repo (or webhook write permission)."})
		return
	}
	rp.HookID, rp.HookURL, rp.HookError = h.ID, url, ""
	_ = s.Store.SaveRepo(rp)
	writeJSON(w, 200, rp)
}

func (s *Server) removeHook(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.Store.Repo(repoName(r))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "Not connected."})
		return
	}
	if rp.HookID != 0 {
		if err := s.gh().DeleteHook(r.Context(), rp.FullName, rp.HookID); err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
	}
	rp.HookID, rp.HookURL, rp.HookError = 0, "", ""
	_ = s.Store.SaveRepo(rp)
	writeJSON(w, 200, rp)
}

func (s *Server) pulls(w http.ResponseWriter, r *http.Request) {
	name := repoName(r)
	if _, ok := s.Store.Repo(name); !ok {
		writeJSON(w, 404, map[string]string{"error": "Not connected."})
		return
	}
	ps, err := s.gh().Pulls(r.Context(), name)
	if err != nil {
		writeJSON(w, 502, map[string]string{"error": "GitHub: " + err.Error()})
		return
	}
	hist, _ := s.Store.Reviews(name, 0, 0)
	type row struct {
		github.Pull
		Last   *store.Review `json:"last,omitempty"`
		Rounds int           `json:"rounds"`
		Stale  bool          `json:"stale"` // new commits since the last finished review
	}
	out := make([]row, 0, len(ps))
	for _, p := range ps {
		rw := row{Pull: p}
		for i := range hist {
			if hist[i].PR != p.Number {
				continue
			}
			if rw.Last == nil {
				h := hist[i]
				h.Log = nil
				rw.Last = &h
			}
			if hist[i].Status == "done" {
				rw.Rounds++
			}
		}
		if rw.Last != nil && rw.Last.Status == "done" && rw.Last.HeadSHA != p.Head.SHA {
			rw.Stale = true
		}
		out = append(out, rw)
	}
	writeJSON(w, 200, out)
}

func (s *Server) startReview(w http.ResponseWriter, r *http.Request) {
	name := repoName(r)
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n <= 0 {
		writeJSON(w, 400, map[string]string{"error": "Bad PR number."})
		return
	}
	if _, ok := s.Store.Repo(name); !ok {
		writeJSON(w, 404, map[string]string{"error": "Not connected."})
		return
	}
	var in struct {
		Fresh bool `json:"fresh"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&in)
	rec, err := s.Runner.Enqueue(name, n, "manual", in.Fresh)
	if err != nil {
		writeJSON(w, 503, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 202, rec)
}

// ---- reviews ----

func (s *Server) listReviews(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pr, _ := strconv.Atoi(q.Get("pr"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rs, _ := s.Store.Reviews(q.Get("repo"), pr, limit)
	for i := range rs {
		rs[i].Log = nil
	}
	writeJSON(w, 200, rs)
}

func (s *Server) getReview(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.Store.Review(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "No such review."})
		return
	}
	if rec.Status == "running" {
		if live := s.Runner.Live(rec.ID); len(live) > 0 {
			rec.Log = live
		}
	}
	_, hasReport := s.Store.ReportPath(rec.ID)
	writeJSON(w, 200, map[string]any{"review": rec, "hasReport": hasReport})
}

func (s *Server) cancelReview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"cancelled": s.Runner.Cancel(r.PathValue("id"))})
}

func (s *Server) reviewPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.Store.ReportPath(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var rep render.Report
	if err := json.Unmarshal(b, &rep); err != nil {
		http.Error(w, "bad report: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = render.HTML(w, &rep)
}

// ---- webhook ----

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 25<<20))
	if err != nil {
		http.Error(w, "read", 400)
		return
	}
	if !github.VerifySignature(s.config().WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "bad signature", 401)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		writeJSON(w, 200, map[string]string{"result": "pong"})
		return
	case "pull_request":
	default:
		writeJSON(w, 202, map[string]string{"result": "ignored event"})
		return
	}
	var ev github.PullRequestEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad payload", 400)
		return
	}
	result := s.HandlePullRequest(ev)
	writeJSON(w, 202, map[string]string{"result": result})
}

// HandlePullRequest decides whether a pull_request event starts a review.
func (s *Server) HandlePullRequest(ev github.PullRequestEvent) string {
	rp, ok := s.Store.Repo(ev.Repository.FullName)
	if !ok {
		return "repository not connected"
	}
	st := rp.Settings
	if !st.AutoReview {
		return "auto review is off for this repository"
	}
	switch ev.Action {
	case "opened", "reopened", "ready_for_review":
		if !st.OnOpen {
			return "reviews on open are off"
		}
	case "synchronize":
		if !st.OnPush {
			return "reviews on new commits are off"
		}
	default:
		return "action " + ev.Action + " does not start a review"
	}
	if ev.PullRequest.Draft && st.SkipDrafts {
		return "draft pull requests are skipped"
	}
	rec, err := s.Runner.Enqueue(rp.FullName, ev.Number, "webhook", false)
	if err != nil {
		log.Printf("webhook: %v", err)
		return "could not queue: " + err.Error()
	}
	return "queued review " + rec.ID
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func mask(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "••••"
	}
	return s[:4] + "••••" + s[len(s)-4:]
}

func secureHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		h.ServeHTTP(w, r)
	})
}
