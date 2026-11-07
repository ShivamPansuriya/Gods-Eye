package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSignature(t *testing.T) {
	body := []byte(`{"action":"opened"}`)
	sig := Sign("s3cret", body)
	if !VerifySignature("s3cret", body, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifySignature("other", body, sig) || VerifySignature("s3cret", []byte(`{}`), sig) || VerifySignature("", body, sig) || VerifySignature("s3cret", body, "sha1=abc") {
		t.Fatal("bad signature accepted")
	}
}

func TestReviewPostsLineCommentsAndFallsBack(t *testing.T) {
	var paths []string
	refuse := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			var body struct {
				Event    string          `json:"event"`
				CommitID string          `json:"commit_id"`
				Comments []ReviewComment `json:"comments"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if refuse {
				w.WriteHeader(422)
				w.Write([]byte(`{"message":"Line could not be resolved"}`))
				return
			}
			if body.Event != "COMMENT" || body.CommitID != "abc" || len(body.Comments) != 1 || body.Comments[0].Line != 9 {
				t.Errorf("review body = %+v", body)
			}
		}
		w.Write([]byte(`{"html_url":"https://x/1"}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	cs := []ReviewComment{{Path: "a.go", Line: 9, Side: "RIGHT", Body: "bug"}}
	if _, n, err := c.Review(context.Background(), "o/r", 3, "abc", "summary", cs); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	refuse = true
	if u, n, err := c.Review(context.Background(), "o/r", 3, "abc", "summary", cs); err != nil || n != 0 || u == "" {
		t.Fatalf("fallback n=%d err=%v", n, err)
	}
	if want := "/repos/o/r/issues/3/comments"; paths[len(paths)-1] != want {
		t.Fatalf("paths = %v", paths)
	}
}

func TestPullsMergedSkipsClosedUnmerged(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write([]byte(`[{"number":1,"merged_at":"2026-11-01T10:00:00Z"},{"number":2,"merged_at":null}]`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	ps, err := c.Pulls(context.Background(), "o/r", "merged")
	if err != nil || len(ps) != 1 || ps[0].Number != 1 || ps[0].MergedAt == nil || !strings.Contains(query, "state=closed") {
		t.Fatalf("ps=%+v err=%v query=%s", ps, err, query)
	}
	if _, err := c.Pulls(context.Background(), "o/r", "open"); err != nil || !strings.Contains(query, "state=open") {
		t.Fatalf("open query=%s err=%v", query, err)
	}
}

func TestDiffBase(t *testing.T) {
	var open, merged Pull
	open.Base.Ref, open.Base.SHA = "main", "aaa"
	merged.Base.Ref, merged.Base.SHA = "main", "bbb"
	now := time.Now()
	merged.MergedAt = &now
	if got := open.DiffBase(); got != "refs/remotes/origin/main" {
		t.Fatalf("open base = %s", got)
	}
	if got := merged.DiffBase(); got != "bbb" {
		t.Fatalf("merged base = %s", got)
	}
}

func TestResolveThreadsClosesOnlyOurOpenThreadsForFixedBugs(t *testing.T) {
	var resolved []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if r.URL.Path != "/graphql" {
			t.Errorf("path %s", r.URL.Path)
		}
		if strings.HasPrefix(req.Query, "mutation") {
			if !strings.Contains(req.Variables["b"].(string), "Fixed") {
				t.Errorf("reply = %v", req.Variables["b"])
			}
			resolved = append(resolved, req.Variables["t"].(string))
			w.Write([]byte(`{"data":{}}`))
			return
		}
		w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
			{"id":"T1","isResolved":false,"comments":{"nodes":[{"body":"bug <!-- m1 -->","viewerDidAuthor":true}]}},
			{"id":"T2","isResolved":false,"comments":{"nodes":[{"body":"quoting <!-- m1 -->","viewerDidAuthor":false}]}},
			{"id":"T3","isResolved":true,"comments":{"nodes":[{"body":"bug <!-- m2 -->","viewerDidAuthor":true}]}},
			{"id":"T4","isResolved":false,"comments":{"nodes":[{"body":"bug <!-- m3 -->","viewerDidAuthor":true}]}}]}}}}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	n, err := c.ResolveThreads(context.Background(), "o/r", 3, map[string]string{"<!-- m1 -->": "Fixed in abc", "<!-- m2 -->": "Fixed in abc"})
	if err != nil || n != 1 || len(resolved) != 1 || resolved[0] != "T1" {
		t.Fatalf("n=%d err=%v resolved=%v", n, err, resolved)
	}
	if n, err := c.ResolveThreads(context.Background(), "o/r", 3, nil); n != 0 || err != nil {
		t.Errorf("nothing fixed must not call GitHub: n=%d err=%v", n, err)
	}
}

func TestGitAuthEnvIsScopedToTheGitHubHost(t *testing.T) {
	env := strings.Join(GitAuthEnv("tok", "https://github.com/"), "\n")
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader", "GIT_CONFIG_VALUE_0=" + AuthHeader("tok")} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
	if got := GitAuthEnv("", "https://github.com/"); len(got) != 1 {
		t.Errorf("no token adds no header: %v", got)
	}
	if AuthHeader("") != "" {
		t.Error("no token, no header")
	}
}
