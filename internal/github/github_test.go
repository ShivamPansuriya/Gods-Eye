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
