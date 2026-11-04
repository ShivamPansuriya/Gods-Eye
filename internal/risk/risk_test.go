package risk

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{".github/workflows/**", ".github/workflows/ci.yml", true},
		{"**/*auth*/**", "src/auth/login.ts", true},
		{"**/*auth*.*", "src/oauth.go", true},
		{"**/migrations/**", "db/migrations/001.sql", true},
		{"**/*.lock", "yarn.lock", true},
		{"**/*billing*/**", "src/ui/button.tsx", false},
	}
	for _, c := range cases {
		if got := Match(c.glob, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestFloorOnlyRaises(t *testing.T) {
	tier, why := Floor(Defaults, []string{"README.md", ".github/workflows/release.yml"})
	if tier != Human || why == "" {
		t.Fatalf("got %s %q, want human", tier, why)
	}
	if tier, _ := Floor(Defaults, []string{"README.md"}); tier != Static {
		t.Fatalf("plain docs got %s", tier)
	}
}
