package remote

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShivamPansuriya/gods-eye/internal/server"
)

func isolate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GODSEYE_URL", "")
	t.Setenv("GODSEYE_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_API_KEY", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GODSEYE_ADMIN_TOKEN", "")
}

func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Main(args, &out, strings.NewReader(stdin), nil)
	return out.String(), err
}

// Against the real server: login checks the token, saves it 0600, and the
// read commands work on an empty server.
func TestLoginAndReadCommandsAgainstRealServer(t *testing.T) {
	isolate(t)
	s, err := server.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if _, err := run(t, "wrong\n", "login", ts.URL); err == nil || !strings.Contains(err.Error(), "Sign in") {
		t.Fatalf("wrong token: %v", err)
	}
	if _, err := os.Stat(ConfigPath()); err == nil {
		t.Fatal("a failed login must not save anything")
	}
	out, err := run(t, s.AdminToken()+"\n", "login", ts.URL)
	if err != nil || !strings.Contains(out, "Signed in") || !strings.Contains(out, "Claude   ✗ not set") {
		t.Fatalf("login: %v\n%s", err, out)
	}
	if fi, err := os.Stat(ConfigPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("config file: %v %v", err, fi)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"status"}, "0 running, 0 queued"},
		{[]string{"repos"}, "No repositories connected"},
		{[]string{"history"}, "No reviews yet"},
		{[]string{"history", "acme/shop", "--pr", "3"}, "No reviews yet"},
	} {
		out, err := run(t, "", c.args...)
		if err != nil || !strings.Contains(out, c.want) {
			t.Errorf("%v: %v\n%s", c.args, err, out)
		}
	}
	if _, err := run(t, "", "prs", "acme/shop"); err == nil || !strings.Contains(err.Error(), "Not connected") {
		t.Fatalf("prs on unconnected repo: %v", err)
	}
	if _, err := run(t, "", "bogus"); err == nil {
		t.Fatal("unknown command accepted")
	}
}

// review starts a review, then follows it: each log line printed once, the
// result line at the end, and an unfinished review never reads as "0 bugs".
func TestReviewFollowsLogUntilDone(t *testing.T) {
	isolate(t)
	var mu sync.Mutex
	polls := 0
	var started map[string]bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"Sign in first."}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/repos/acme/shop/pulls/7/review":
			json.NewDecoder(r.Body).Decode(&started)
			w.WriteHeader(202)
			w.Write([]byte(`{"id":"20261107-000000-aaaaaa","status":"queued"}`))
		case r.URL.Path == "/api/reviews/20261107-000000-aaaaaa":
			polls++
			rv := map[string]any{"id": "20261107-000000-aaaaaa", "status": "running", "log": []string{"one"}}
			if polls >= 2 {
				rv["log"] = []string{"one", "two"}
			}
			if polls >= 3 {
				rv = map[string]any{"id": "20261107-000000-aaaaaa", "status": "done", "verdict": "incomplete", "bugs": 0,
					"incomplete": "Not finished: 3 findings not checked.", "costUsd": 61.5, "seconds": 600, "log": []string{"one", "two", "three"}}
			}
			json.NewEncoder(w).Encode(map[string]any{"review": rv})
		default:
			w.WriteHeader(404)
		}
	}))
	defer ts.Close()
	t.Setenv("GODSEYE_URL", ts.URL)
	t.Setenv("GODSEYE_TOKEN", "tok")

	var out bytes.Buffer
	c := &Client{Config: LoadConfig()}
	if err := c.do("POST", "/api/repos/acme/shop/pulls/7/review", map[string]bool{"fresh": true}, nil); err != nil {
		t.Fatal(err)
	}
	if !started["fresh"] {
		t.Fatal("--fresh not sent")
	}
	if err := Watch(c, "20261107-000000-aaaaaa", &out, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "one\n") != 1 || strings.Count(got, "two\n") != 1 || !strings.Contains(got, "three\n") {
		t.Fatalf("log lines repeated or lost:\n%s", got)
	}
	if !strings.Contains(got, "⚠ Not finished · 0 bugs so far") || !strings.Contains(got, "3 findings not checked") || !strings.Contains(got, ts.URL+"/r/20261107-000000-aaaaaa") {
		t.Fatalf("result:\n%s", got)
	}
}

func TestReorderLetsFlagsFollowArgs(t *testing.T) {
	got := strings.Join(reorder([]string{"acme/shop", "--merged", "--pr", "4", "x"}), " ")
	if got != "--merged --pr 4 acme/shop x" {
		t.Fatal(got)
	}
}
