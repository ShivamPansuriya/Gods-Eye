// Package ocr runs Alibaba's open-code-review (the `ocr` CLI) as one more bug
// hunter. It reviews the same commit range with its own tuned prompts, file
// grouping and language rules; God's Eye then treats each of its comments like
// any hunter's finding: placed against the code, merged with duplicates, and
// checked by the voters before anything reaches the page or the PR.
package ocr

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Options configures one run.
type Options struct {
	Bin        string // path to the ocr binary
	Dir        string // the repository (a clone without a checkout is fine)
	Base, Head string // commit range to review
	APIKey     string // Anthropic API key
	BaseURL    string // default https://api.anthropic.com
	Model      string
	Effort     string   // low | medium | high (default medium)
	MaxTokens  int64    // total token budget (input+output); 0 = no cap
	Background string   // the PR's title and description
	Exclude    []string // paths it must not review (passed as --exclude)
	Home       string   // HOME for ocr's session files (default: a temp dir)
	Env        []string
	Timeout    time.Duration
}

// Comment is one finding as ocr reports it.
type Comment struct {
	Path           string `json:"path"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	Content        string `json:"content"`
	ExistingCode   string `json:"existing_code"`
	SuggestionCode string `json:"suggestion_code"`
	Category       string `json:"category"` // bug | security | performance | maintainability | test | style | documentation | other
	Severity       string `json:"severity"` // critical | high | medium | low
}

// Usage is ocr's token count; Input includes the cached tokens.
type Usage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
}

// Result is what one run produced.
type Result struct {
	Comments []Comment
	Usage    Usage
	Files    int
	Status   string
	Message  string
	Elapsed  time.Duration
}

// Find returns the ocr binary: $GODSEYE_OCR, then ocr or opencodereview on
// PATH, then ~/.local/bin/ocr. Empty when none is installed.
func Find() string {
	if p := os.Getenv("GODSEYE_OCR"); p != "" {
		if p == "off" {
			return ""
		}
		return p
	}
	for _, n := range []string{"ocr", "opencodereview"} {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(h, ".local", "bin", "ocr")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// Run reviews Base..Head and returns ocr's comments. Errors and messages
// never carry the API key or the git credentials.
func Run(ctx context.Context, o Options) (*Result, error) {
	res, err := run(ctx, o)
	if err != nil {
		return nil, errors.New(o.redact(err.Error()))
	}
	res.Message = o.redact(res.Message)
	return res, nil
}

// redact hides the API key and any credential in o.Env.
func (o Options) redact(s string) string {
	secrets := []string{o.APIKey}
	for _, kv := range o.Env {
		if _, v, ok := strings.Cut(kv, "Authorization: "); ok {
			secrets = append(secrets, v)
			if _, cred, ok := strings.Cut(v, " "); ok {
				secrets = append(secrets, cred)
			}
		}
	}
	for _, x := range secrets {
		if len(x) >= 8 {
			s = strings.ReplaceAll(s, x, "[hidden]")
		}
	}
	return s
}

func run(ctx context.Context, o Options) (*Result, error) {
	if o.Bin == "" {
		return nil, errors.New("ocr is not installed")
	}
	if o.APIKey == "" {
		return nil, errors.New("no Anthropic API key for ocr")
	}
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	tmp, err := os.MkdirTemp("", "godseye-ocr-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	home := o.Home
	if home == "" {
		home = tmp
	}
	out := filepath.Join(tmp, "result.json")
	args := []string{"review", "--from", o.Base, "--to", o.Head, "--format", "json", "--output", out, "--audience", "agent"}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.MaxTokens > 0 {
		args = append(args, "--max-tokens-budget", fmt.Sprint(o.MaxTokens))
	}
	var ex []string
	for _, p := range o.Exclude {
		if p != "" && !strings.Contains(p, ",") {
			ex = append(ex, p)
		}
	}
	if len(ex) > 0 {
		args = append(args, "--exclude", strings.Join(ex, ","))
	}
	if b := strings.TrimSpace(o.Background); b != "" {
		bf := filepath.Join(tmp, "background.md")
		if err := os.WriteFile(bf, []byte(b), 0o600); err == nil {
			args = append(args, "--background-file", bf)
		}
	}
	url := o.BaseURL
	if url == "" {
		url = "https://api.anthropic.com"
	}
	cmd := exec.CommandContext(ctx, o.Bin, args...)
	cmd.Dir = o.Dir
	cmd.WaitDelay = 5 * time.Second // on timeout, do not wait for git children holding the pipe
	cmd.Env = append(baseEnv(), "HOME="+home,
		"OCR_LLM_URL="+url, "OCR_LLM_PROTOCOL=anthropic", "OCR_LLM_MODEL="+o.Model, "OCR_LLM_TOKEN="+o.APIKey,
		"OCR_LLM_TIMEOUT=900", "LC_ALL=C")
	cmd.Env = append(cmd.Env, o.Env...)
	t0 := time.Now()
	stderr, runErr := cmd.CombinedOutput()
	res, err := parse(out)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ocr stopped: %w", ctx.Err())
		}
		if runErr != nil {
			return nil, fmt.Errorf("ocr failed: %v: %s", runErr, lastLine(string(stderr)))
		}
		return nil, err
	}
	if res.Status == "failed" { // every LLM call failed: no review happened
		return nil, fmt.Errorf("ocr failed: %s", cmp.Or(res.Message, lastLine(string(stderr))))
	}
	res.Elapsed = time.Since(t0)
	return res, nil
}

func parse(path string) (*Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ocr wrote no result: %w", err)
	}
	var raw struct {
		Status   string    `json:"status"`
		Message  string    `json:"message"`
		Comments []Comment `json:"comments"`
		Summary  struct {
			Files int `json:"files_reviewed"`
			Usage
		} `json:"summary"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("ocr result: %w", err)
	}
	return &Result{Comments: raw.Comments, Usage: raw.Summary.Usage, Files: raw.Summary.Files, Status: raw.Status, Message: raw.Message}, nil
}

// baseEnv passes through only what ocr and git need, so the parent's other
// secrets do not leak into the child.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy", "SSH_AUTH_SOCK":
			env = append(env, kv)
		}
	}
	return env
}

func lastLine(s string) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) == 0 {
		return ""
	}
	l := ls[len(ls)-1]
	if i := strings.Index(l, "Authorization:"); i >= 0 {
		l = l[:i] + "Authorization: [hidden]"
	}
	if len(l) > 300 {
		l = l[:300]
	}
	return l
}
