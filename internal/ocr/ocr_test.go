package ocr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fake is a stand-in ocr binary: it records its arguments and environment,
// then writes a result to the --output path.
const fake = `#!/bin/sh
dir=$(dirname "$0")
echo "$@" > "$dir/args"
env > "$dir/env"
while [ $# -gt 0 ]; do
  if [ "$1" = "--output" ]; then out="$2"; fi
  shift
done
cat > "$out" <<'JSON'
{"status":"complete","comments":[{"path":"a.go","start_line":3,"end_line":4,"content":"Nil map write panics.","existing_code":"m[k] = v","suggestion_code":"if m == nil { m = map[string]int{} }\nm[k] = v","category":"bug","severity":"high"}],
 "summary":{"files_reviewed":2,"input_tokens":1000,"output_tokens":50,"cache_read_tokens":600,"cache_write_tokens":100}}
JSON
`

func TestRunPassesTheRangeAndKeepsParentSecretsOut(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ocr")
	if err := os.WriteFile(bin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "ghp_parent_secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws_parent_secret")
	res, err := Run(context.Background(), Options{Bin: bin, Dir: dir, Base: "b1", Head: "h2", APIKey: "sk-test", Model: "claude-opus-5-5",
		Effort: "medium", MaxTokens: 1234, Background: "Fix retries", Env: []string{"GIT_TERMINAL_PROMPT=0"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Comments) != 1 || res.Files != 2 || res.Usage.CacheRead != 600 || res.Status != "complete" {
		t.Errorf("result: %+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"review --from b1 --to h2 --format json", "--audience agent", "--effort medium", "--max-tokens-budget 1234", "--background-file"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	for _, want := range []string{"OCR_LLM_TOKEN=sk-test", "OCR_LLM_MODEL=claude-opus-5-5", "OCR_LLM_PROTOCOL=anthropic", "GIT_TERMINAL_PROMPT=0"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env lacks %s", want)
		}
	}
	for _, leak := range []string{"ghp_parent_secret", "aws_parent_secret"} {
		if strings.Contains(string(env), leak) {
			t.Errorf("parent secret %s reached ocr", leak)
		}
	}
}

func TestRunReportsFailureWithoutTheAuthHeader(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ocr")
	script := "#!/bin/sh\necho 'fatal: could not read from remote, header Authorization: Basic eC1hY2Nlc3M=' >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{Bin: bin, Dir: dir, Base: "a", Head: "b", APIKey: "k"})
	if err == nil || strings.Contains(err.Error(), "eC1hY2Nlc3M=") || !strings.Contains(err.Error(), "could not read from remote") {
		t.Errorf("err = %v", err)
	}
}

// When every LLM call fails, ocr still writes a result: status "failed" and
// no comments. That is a failed run, not a clean "0 findings".
func TestRunTreatsAFailedReviewAsAnError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ocr")
	script := `#!/bin/sh
while [ $# -gt 0 ]; do if [ "$1" = "--output" ]; then out="$2"; fi; shift; done
echo '{"status":"failed","message":"Review failed: 0 finding(s); 12 of 12 selected item(s) failed.","comments":null,"summary":{"files_reviewed":12}}' > "$out"
echo 'Error: review failed: all 12 file review(s) failed' >&2
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), Options{Bin: bin, Dir: dir, Base: "a", Head: "b", APIKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "12 of 12 selected item(s) failed") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestErrorsNeverCarryTheKeyOrToken(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ocr")
	script := "#!/bin/sh\necho \"401 for key sk-live-123 via header Basic Z2hwX3NlY3JldA==\" >&2\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Options{Bin: bin, Dir: dir, Base: "a", Head: "b", APIKey: "sk-live-123",
		Env: []string{"GIT_CONFIG_VALUE_0=Authorization: Basic Z2hwX3NlY3JldA=="}})
	if err == nil || strings.Contains(err.Error(), "sk-live-123") || strings.Contains(err.Error(), "Z2hwX3NlY3JldA==") {
		t.Errorf("err = %v", err)
	}
}

func TestRunPassesExcludes(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "ocr")
	if err := os.WriteFile(bin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Options{Bin: bin, Dir: dir, Base: "a", Head: "b", APIKey: "k", Exclude: []string{"deploy/.env", "go.sum"}}); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	if !strings.Contains(string(args), "--exclude deploy/.env,go.sum") {
		t.Errorf("args %q", args)
	}
}
