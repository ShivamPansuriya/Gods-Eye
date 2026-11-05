package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
