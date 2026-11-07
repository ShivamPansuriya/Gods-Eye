package scope

import (
	"strings"
	"testing"
)

func TestGlob(t *testing.T) {
	for _, c := range []struct {
		pat, path string
		want      bool
	}{
		{"**/*_test.go", "foo_test.go", true},
		{"**/*_test.go", "a/b/c_test.go", true},
		{"*_test.go", "foo_test.go", true},
		{"*_test.go", "pkg/foo_test.go", false},
		{"**/*.{js,ts}", "src/app.TS", true},
		{"**/*.{js,ts}", "src/app.go", false},
		{"legacy/**", "legacy", true},
		{"legacy/**", "legacy/a/b.go", true},
		{"legacy/**", "legacyx/a.go", false},
		{"**/src/test/java/**/*.java", "m/src/test/java/a/BTest.java", true},
		{"**/.yarn/{cache,sdks}/**", "w/.yarn/sdks/x.js", true},
		{"src/?.go", "src/a.go", true},
		{"src/?.go", "src/ab.go", false},
	} {
		if got := Compile(c.pat).Match(c.path); got != c.want {
			t.Errorf("%s ~ %s = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestDecide(t *testing.T) {
	cfg := ParseGodsEye([]byte(`{"rules":[{"glob":"x","tier":"human"}],"exclude":["legacy/**"],"include":["keep/**"],
		"checklists":[{"path":"api/**/*.go","rule":"Every handler checks the tenant.","merge_system_rule":true},{"path":"web/**","rule":"docs/web-review.md"}]}`))
	cfg = cfg.Merge(ParseOCR([]byte(`{"rules":[{"path":"**/*.sql","rule":"Migrations are append-only."}],"exclude":["tmp/**"]}`)))
	read := func(p string) (string, bool) {
		if p == "docs/web-review.md" {
			return "Escape every user string.", true
		}
		return "", false
	}
	bin := func(p string) bool { return p == "logo.png" }
	p := Decide([]string{
		".env", ".env.example", "deploy/id_rsa", ".npmrc", "cert/server.pem",
		"svc/main.go", "svc/main_test.go", "keep/main_test.go", "m/src/test/java/a/ATest.java",
		"package-lock.json", "web/yarn.lock", "README.md", "logo.png", "gen/api.pb.go", "node_modules/x/i.js",
		"legacy/old.go", "tmp/a.go", "api/v1/h.go", "web/app.ts", "db/V2__x.sql", "Dockerfile",
	}, bin, cfg, read)

	want := map[string]struct {
		kind      Kind
		reason    string
		checklist string
	}{
		".env":                         {Secret, "credential file, never sent to the model", ""},
		".env.example":                 {Skip, "not code (.example)", ""},
		"deploy/id_rsa":                {Secret, "credential file, never sent to the model", ""},
		".npmrc":                       {Secret, "credential file, never sent to the model", ""},
		"cert/server.pem":              {Secret, "credential file, never sent to the model", ""},
		"svc/main.go":                  {Code, "", "go"},
		"svc/main_test.go":             {Test, "test", "go"},
		"keep/main_test.go":            {Code, "", "go"},
		"m/src/test/java/a/ATest.java": {Test, "test", "java"},
		"package-lock.json":            {Skip, "lockfile", ""},
		"web/yarn.lock":                {Skip, "lockfile", ""},
		"README.md":                    {Skip, "not code (.md)", ""},
		"logo.png":                     {Skip, "binary", ""},
		"gen/api.pb.go":                {Skip, "generated", ""},
		"node_modules/x/i.js":          {Skip, "dependency", ""},
		"legacy/old.go":                {Skip, "repo rule: exclude legacy/**", ""},
		"tmp/a.go":                     {Skip, "repo rule: exclude tmp/**", ""},
		"api/v1/h.go":                  {Code, "", "repo: api/**/*.go + go"},
		"web/app.ts":                   {Code, "", "repo: web/**"},
		"db/V2__x.sql":                 {Code, "", "repo: **/*.sql"},
		"Dockerfile":                   {Code, "", ""},
	}
	for path, w := range want {
		f := p.Of(path)
		if f.Kind != w.kind || f.Reason != w.reason || f.Checklist != w.checklist {
			t.Errorf("%s = %+v, want %+v", path, f, w)
		}
	}
	if s := p.Checklists["repo: api/**/*.go + go"]; !strings.Contains(s, "Go Review Principles") || !strings.HasSuffix(s, "Every handler checks the tenant.") {
		t.Errorf("merged checklist: %.80q", s)
	}
	if p.Checklists["repo: web/**"] != "Escape every user string." {
		t.Errorf("checklist from a repo file: %q", p.Checklists["repo: web/**"])
	}
	s := p.Summary()
	if s.Secret != 4 || s.Test != 2 || s.Skipped != 9 || s.Code != 6 || s.Reasons["lockfile"] != 2 || s.Reasons["repo rule"] != 2 {
		t.Errorf("summary %+v", s)
	}
	if s.Files[0].Kind != Secret {
		t.Errorf("secrets are listed first: %+v", s.Files[0])
	}
}

func TestSystemRulesKeepFileOrder(t *testing.T) {
	if len(systemRules) < 60 {
		t.Fatalf("only %d path rules loaded", len(systemRules))
	}
	// .github/workflows/x.yml matches both github_workflows and yaml; the first wins.
	if name, _ := systemChecklist(".github/workflows/ci.yml"); name != "github_workflows" {
		t.Errorf("workflow file got %q", name)
	}
	if name, text := systemChecklist("a/b/Svc.java"); name != "java" || text == "" {
		t.Errorf("java got %q", name)
	}
	if name, _ := systemChecklist("x.unknown"); name != "" {
		t.Errorf("unknown got %q", name)
	}
}

func TestMoreCredentialFilesAndSafeRulePaths(t *testing.T) {
	for _, p := range []string{".git-credentials", "ops/.envrc", "conf/prod.env", "home/.aws/credentials", "web/.htpasswd", "infra/prod.tfvars", "vault.kdbx"} {
		if !IsSecret(p) {
			t.Errorf("%s is a credential file", p)
		}
	}
	for _, p := range []string{".env.example", "src/env.go", "docs/environment.md"} {
		if IsSecret(p) {
			t.Errorf("%s is not a credential file", p)
		}
	}
	if k, _ := Classify("vendor/github.com/x/y_test.go"); k != Skip {
		t.Errorf("a vendored test is skipped, got %s", k)
	}
	read := func(p string) (string, bool) {
		t.Errorf("read %q: a rule path must stay inside the repo", p)
		return "x", true
	}
	for _, rule := range []string{"../../home/u/notes.txt", "/etc/motd.txt", "docs/../../x.md"} {
		cfg := ParseGodsEye([]byte(`{"checklists":[{"path":"**","rule":"` + rule + `"}]}`))
		Decide([]string{"a.go"}, func(string) bool { return false }, cfg, read)
	}
}
