package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.LoadConfig()
	if len(c.WebhookSecret) != 64 || len(c.AdminToken) != 32 || c.Concurrency != 2 {
		t.Fatalf("config = %+v", c)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "config.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("config.json perm = %v", fi.Mode().Perm())
	}
	c2, _ := s.LoadConfig()
	if c2.AdminToken != c.AdminToken {
		t.Fatal("secrets must be stable across loads")
	}
	s.SaveRepo(Repo{FullName: "a/b", Settings: DefaultSettings()})
	if r, ok := s.Repo("a/b"); !ok || r.Settings.Depth != "auto" {
		t.Fatalf("repo = %+v %v", r, ok)
	}
	old := Review{ID: "20250101-000000-aaaaaa", Repo: "a/b", PR: 1, Status: "done", CostUSD: 5, CreatedAt: time.Now().AddDate(0, -2, 0)}
	newer := Review{ID: NewReviewID(), Repo: "a/b", PR: 2, Status: "done", CostUSD: 2, CreatedAt: time.Now()}
	s.SaveReview(old)
	s.SaveReview(newer)
	s.SaveReport(newer.ID, map[string]int{"x": 1})
	rs, _ := s.Reviews("a/b", 0, 0)
	if len(rs) != 2 || rs[0].ID != newer.ID {
		t.Fatalf("newest first: %+v", rs)
	}
	if rs, _ := s.Reviews("a/b", 1, 0); len(rs) != 1 {
		t.Fatal("pr filter")
	}
	if got := s.SpendSince(time.Now().AddDate(0, -1, 0)); got != 2 {
		t.Fatalf("spend = %v", got)
	}
	if _, ok := s.ReportPath(newer.ID); !ok {
		t.Fatal("report missing")
	}
	if _, ok := s.Review("../../etc/passwd"); ok {
		t.Fatal("path traversal in id")
	}
}
