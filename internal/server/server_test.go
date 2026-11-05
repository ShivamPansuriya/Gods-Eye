package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/github"
	"github.com/ShivamPansuriya/gods-eye/internal/store"
)

// fakeGitHub answers the few endpoints the server calls.
func fakeGitHub(t *testing.T) (*httptest.Server, *[]string) {
	var hooks []string
	mux := http.NewServeMux()
	j := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghp_test" {
			w.WriteHeader(401)
			j(w, map[string]string{"message": "Bad credentials"})
			return
		}
		j(w, map[string]string{"login": "octo"})
	})
	mux.HandleFunc("GET /user/repos", func(w http.ResponseWriter, r *http.Request) {
		j(w, []map[string]any{{"full_name": "acme/shop", "clone_url": "https://github.com/acme/shop.git", "default_branch": "main"}})
	})
	mux.HandleFunc("GET /repos/acme/shop", func(w http.ResponseWriter, r *http.Request) {
		j(w, map[string]any{"full_name": "acme/shop", "clone_url": "https://github.com/acme/shop.git", "html_url": "https://github.com/acme/shop", "default_branch": "main"})
	})
	mux.HandleFunc("GET /repos/acme/shop/pulls", func(w http.ResponseWriter, r *http.Request) {
		j(w, []map[string]any{{"number": 7, "title": "Add refunds", "head": map[string]string{"sha": "abc123", "ref": "refunds"}, "base": map[string]string{"ref": "main"}, "user": map[string]string{"login": "dev"}}})
	})
	mux.HandleFunc("GET /repos/acme/shop/hooks", func(w http.ResponseWriter, r *http.Request) { j(w, []any{}) })
	mux.HandleFunc("POST /repos/acme/shop/hooks", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Config map[string]string `json:"config"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		hooks = append(hooks, in.Config["url"]+"|"+in.Config["secret"])
		w.WriteHeader(201)
		j(w, map[string]any{"id": 99, "active": true})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hooks
}

func newTestServer(t *testing.T) (*Server, http.Handler, *[]string) {
	gh, hooks := fakeGitHub(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_API_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GODSEYE_ADMIN_TOKEN", "")
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.GitHubToken, s.cfg.GitHubAPI = "ghp_test", gh.URL
	return s, s.Handler(), hooks
}

func call(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		json.NewEncoder(&b).Encode(body)
	}
	req := httptest.NewRequest(method, path, &b)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	s, h, _ := newTestServer(t)
	if c := call(t, h, "GET", "/api/repos", "", nil).Code; c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c := call(t, h, "GET", "/r/20260101-000000-abcdef", "", nil).Code; c != http.StatusSeeOther {
		t.Fatalf("review page without login should redirect: %d", c)
	}
	if c := call(t, h, "POST", "/api/login", "", map[string]string{"token": "wrong"}).Code; c != 401 {
		t.Fatalf("wrong login: %d", c)
	}
	rec := call(t, h, "POST", "/api/login", "", map[string]string{"token": s.AdminToken()})
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Set-Cookie"), "ge_admin=") || !strings.Contains(rec.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Fatalf("login: %d %s", rec.Code, rec.Header().Get("Set-Cookie"))
	}
}

func TestConnectSettingsHookAndPulls(t *testing.T) {
	s, h, hooks := newTestServer(t)
	tok := s.AdminToken()
	st := call(t, h, "GET", "/api/status", tok, nil)
	if !strings.Contains(st.Body.String(), `"githubUser":"octo"`) {
		t.Fatalf("status: %s", st.Body)
	}
	if c := call(t, h, "POST", "/api/repos", tok, map[string]string{"fullName": "acme/shop"}).Code; c != 200 {
		t.Fatalf("connect: %d", c)
	}
	// Installing a webhook needs a public URL first.
	if c := call(t, h, "POST", "/api/repos/acme/shop/webhook", tok, nil).Code; c != 400 {
		t.Fatalf("hook without public URL: %d", c)
	}
	call(t, h, "PUT", "/api/config", tok, map[string]string{"publicUrl": "https://ge.example.com/"})
	if c := call(t, h, "POST", "/api/repos/acme/shop/webhook", tok, nil).Code; c != 200 {
		t.Fatalf("hook: %d", c)
	}
	if len(*hooks) != 1 || !strings.HasPrefix((*hooks)[0], "https://ge.example.com/webhook/github|") || strings.HasSuffix((*hooks)[0], "|") {
		t.Fatalf("hook config = %v", *hooks)
	}
	rp, _ := s.Store.Repo("acme/shop")
	if rp.HookID != 99 {
		t.Fatalf("hook id = %d", rp.HookID)
	}
	// Bad settings are normalised.
	call(t, h, "PUT", "/api/repos/acme/shop/settings", tok, store.RepoSettings{AutoReview: true, OnOpen: true, Depth: "bogus", Effort: "max", Budget: -3})
	rp, _ = s.Store.Repo("acme/shop")
	if !rp.Settings.AutoReview || rp.Settings.Depth != "standard" || rp.Settings.Effort != "max" || rp.Settings.Budget != 15 {
		t.Fatalf("settings = %+v", rp.Settings)
	}
	pl := call(t, h, "GET", "/api/repos/acme/shop/pulls", tok, nil)
	if pl.Code != 200 || !strings.Contains(pl.Body.String(), `"number":7`) {
		t.Fatalf("pulls: %d %s", pl.Code, pl.Body)
	}
	// The token never leaves the server in full.
	cfg := call(t, h, "GET", "/api/config", tok, nil).Body.String()
	if strings.Contains(cfg, "ghp_test") {
		t.Fatalf("config leaked the token: %s", cfg)
	}
}

func webhook(t *testing.T, h http.Handler, secret, event string, payload any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/webhook/github", bytes.NewReader(b))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", github.Sign(secret, b))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhookDecisions(t *testing.T) {
	s, h, _ := newTestServer(t)
	secret := s.config().WebhookSecret
	ev := func(action string, draft bool) map[string]any {
		return map[string]any{"action": action, "number": 7, "repository": map[string]string{"full_name": "acme/shop"},
			"pull_request": map[string]any{"number": 7, "draft": draft}}
	}
	if c := webhook(t, h, "not-the-secret", "pull_request", ev("opened", false)).Code; c != 401 {
		t.Fatalf("forged signature accepted: %d", c)
	}
	if !strings.Contains(webhook(t, h, secret, "pull_request", ev("opened", false)).Body.String(), "not connected") {
		t.Fatal("unconnected repo should be ignored")
	}
	call(t, h, "POST", "/api/repos", s.AdminToken(), map[string]string{"fullName": "acme/shop"})
	if !strings.Contains(webhook(t, h, secret, "pull_request", ev("opened", false)).Body.String(), "auto review is off") {
		t.Fatal("auto review defaults to off")
	}
	rp, _ := s.Store.Repo("acme/shop")
	rp.Settings.AutoReview, rp.Settings.OnPush = true, false
	s.Store.SaveRepo(rp)
	if !strings.Contains(webhook(t, h, secret, "pull_request", ev("opened", true)).Body.String(), "draft") {
		t.Fatal("drafts should be skipped")
	}
	if !strings.Contains(webhook(t, h, secret, "pull_request", ev("synchronize", false)).Body.String(), "new commits are off") {
		t.Fatal("pushes should be ignored when OnPush is off")
	}
	if !strings.Contains(webhook(t, h, secret, "pull_request", ev("closed", false)).Body.String(), "does not start") {
		t.Fatal("closed should not start a review")
	}
	out := webhook(t, h, secret, "pull_request", ev("opened", false)).Body.String()
	if !strings.Contains(out, "queued review") {
		t.Fatalf("opened should queue: %s", out)
	}
	// A second event while one is queued does not queue a duplicate.
	webhook(t, h, secret, "pull_request", ev("reopened", false))
	rs, _ := s.Store.Reviews("acme/shop", 7, 0)
	if len(rs) != 1 || rs[0].Status != "queued" || rs[0].Trigger != "webhook" {
		t.Fatalf("reviews = %+v", rs)
	}
	if c := webhook(t, h, secret, "ping", map[string]string{}).Code; c != 200 {
		t.Fatalf("ping: %d", c)
	}
}
