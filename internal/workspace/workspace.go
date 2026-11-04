// Package workspace gives review agents read-only access to the repository at
// the PR's head commit: read a file range, grep, list files, and check that a
// quoted line really exists. Every path is confined to the repo root.
package workspace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Limits keep tool results small enough to be useful in a prompt.
const (
	MaxReadLines   = 250
	MaxGrepHits    = 60
	MaxListEntries = 200
	MaxLineChars   = 300
)

// Workspace is a checked-out repository at the PR head.
type Workspace struct {
	Root string
	// Ref, when set, makes reads come from this git commit (`git show ref:path`)
	// instead of the working tree, so a dirty or different checkout can't leak in.
	Ref string

	mu    sync.Mutex
	files map[string][]string // cache of file contents split into lines
}

// Open returns a workspace rooted at dir (must be inside a git repository).
func Open(dir, ref string) (*Workspace, error) {
	out, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %s", dir)
	}
	root := strings.TrimSpace(out)
	w := &Workspace{Root: root, Ref: ref, files: map[string][]string{}}
	if ref != "" {
		if _, err := git(root, "rev-parse", "--verify", "-q", ref+"^{commit}"); err != nil {
			return nil, fmt.Errorf("commit %s not found in %s", ref, root)
		}
	}
	return w, nil
}

// clean validates a repo-relative path and refuses anything that escapes the root.
func clean(p string) (string, error) {
	p = strings.TrimSpace(strings.TrimPrefix(p, "./"))
	p = strings.TrimPrefix(p, "/")
	c := path.Clean(p)
	if c == "." || c == "" {
		return "", errors.New("empty path")
	}
	if strings.HasPrefix(c, "../") || c == ".." || strings.Contains(c, "/../") {
		return "", errors.New("path escapes the repository")
	}
	if strings.HasPrefix(c, ".git/") || c == ".git" {
		return "", errors.New("path is inside .git")
	}
	return c, nil
}

// Lines returns a file's lines (cached).
func (w *Workspace) Lines(p string) ([]string, error) {
	c, err := clean(p)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	if ls, ok := w.files[c]; ok {
		w.mu.Unlock()
		return ls, nil
	}
	w.mu.Unlock()

	var data []byte
	if w.Ref != "" {
		out, err := git(w.Root, "show", w.Ref+":"+c)
		if err != nil {
			return nil, fmt.Errorf("no such file at %s: %s", short(w.Ref), c)
		}
		data = []byte(out)
	} else {
		full := filepath.Join(w.Root, filepath.FromSlash(c))
		real, err := filepath.EvalSymlinks(full)
		if err != nil {
			return nil, fmt.Errorf("no such file: %s", c)
		}
		if !strings.HasPrefix(real, w.Root+string(os.PathSeparator)) {
			return nil, errors.New("path escapes the repository")
		}
		data, err = os.ReadFile(real)
		if err != nil {
			return nil, fmt.Errorf("cannot read %s", c)
		}
	}
	if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
		return nil, fmt.Errorf("%s is a binary file", c)
	}
	ls := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	w.mu.Lock()
	w.files[c] = ls
	w.mu.Unlock()
	return ls, nil
}

// Read returns numbered lines [start, end] (1-based, inclusive), capped.
func (w *Workspace) Read(p string, start, end int) string {
	ls, err := w.Lines(p)
	if err != nil {
		return "error: " + err.Error()
	}
	if start < 1 {
		start = 1
	}
	if end < start || end == 0 {
		end = start + 150
	}
	if end-start+1 > MaxReadLines {
		end = start + MaxReadLines - 1
	}
	if start > len(ls) {
		return fmt.Sprintf("error: %s has only %d lines", p, len(ls))
	}
	if end > len(ls) {
		end = len(ls)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s lines %d-%d of %d\n", p, start, end, len(ls))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%5d  %s\n", i, clip(ls[i-1]))
	}
	if end < len(ls) {
		fmt.Fprintf(&b, "… %d more lines\n", len(ls)-end)
	}
	return b.String()
}

// Grep runs an extended regex over tracked files, optionally limited by a glob.
func (w *Workspace) Grep(pattern, glob string) string {
	if strings.TrimSpace(pattern) == "" {
		return "error: empty pattern"
	}
	args := []string{"grep", "-n", "-I", "-E", "--no-color", "-e", pattern}
	if w.Ref != "" {
		args = append(args, w.Ref)
	}
	args = append(args, "--")
	if glob != "" {
		args = append(args, ":(glob)"+glob)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = w.Root
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "no matches"
		}
		return "error: grep failed (check the regex)"
	}
	prefix := ""
	if w.Ref != "" {
		prefix = w.Ref + ":"
	}
	var b strings.Builder
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	total := 0
	for sc.Scan() {
		total++
		if n < MaxGrepHits {
			b.WriteString(clip(strings.TrimPrefix(sc.Text(), prefix)))
			b.WriteByte('\n')
			n++
		}
	}
	if total > n {
		fmt.Fprintf(&b, "… %d more matches; narrow the pattern or glob\n", total-n)
	}
	return b.String()
}

// List returns tracked files matching a glob.
func (w *Workspace) List(glob string) string {
	var out string
	var err error
	if w.Ref != "" {
		out, err = git(w.Root, "ls-tree", "-r", "--name-only", w.Ref)
	} else {
		out, err = git(w.Root, "ls-files")
	}
	if err != nil {
		return "error: cannot list files"
	}
	var b strings.Builder
	n, total := 0, 0
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		if glob != "" && !matchGlob(glob, f) {
			continue
		}
		total++
		if n < MaxListEntries {
			b.WriteString(f)
			b.WriteByte('\n')
			n++
		}
	}
	if total == 0 {
		return "no files match"
	}
	if total > n {
		fmt.Fprintf(&b, "… %d more files\n", total-n)
	}
	return b.String()
}

// Contains reports whether quote appears in the file (whitespace-insensitive),
// and the first line number where it starts. Used to drop invented findings.
func (w *Workspace) Contains(p, quote string) (int, bool) {
	q := norm(quote)
	if q == "" {
		return 0, false
	}
	ls, err := w.Lines(p)
	if err != nil {
		return 0, false
	}
	for i, l := range ls {
		if strings.Contains(norm(l), q) {
			return i + 1, true
		}
	}
	// Multi-line quotes: try joining a sliding window of 4 lines.
	for i := range ls {
		j := min(len(ls), i+4)
		if strings.Contains(norm(strings.Join(ls[i:j], " ")), q) {
			return i + 1, true
		}
	}
	return 0, false
}

// Run executes a shell command in the workspace (for --verify-cmd evidence).
func (w *Workspace) Run(ctx context.Context, command string) (string, int) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = w.Root
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	return string(out), code
}

func norm(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string) string {
	if len(s) > MaxLineChars {
		return s[:MaxLineChars] + "…"
	}
	return s
}

func short(ref string) string {
	if len(ref) > 10 {
		return ref[:10]
	}
	return ref
}

func matchGlob(glob, p string) bool {
	if ok, _ := path.Match(glob, p); ok {
		return true
	}
	// ** support: match any suffix of the path.
	if strings.Contains(glob, "**") {
		parts := strings.SplitN(glob, "**", 2)
		pre, suf := parts[0], strings.TrimPrefix(parts[1], "/")
		if !strings.HasPrefix(p, pre) {
			return false
		}
		rest := strings.TrimPrefix(p, pre)
		segs := strings.Split(rest, "/")
		for i := range segs {
			if ok, _ := path.Match(suf, strings.Join(segs[i:], "/")); ok {
				return true
			}
		}
		return false
	}
	// A bare pattern without slash matches the base name anywhere.
	if !strings.Contains(glob, "/") {
		ok, _ := path.Match(glob, path.Base(p))
		return ok
	}
	return false
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// NearestLine finds the occurrence of quote closest to line (whitespace-insensitive).
func (w *Workspace) NearestLine(p, quote string, line int) (int, bool) {
	q := norm(quote)
	if q == "" {
		return 0, false
	}
	ls, err := w.Lines(p)
	if err != nil {
		return 0, false
	}
	best, found := 0, false
	for i, l := range ls {
		if strings.Contains(norm(l), q) {
			if !found || abs(i+1-line) < abs(best-line) {
				best, found = i+1, true
			}
		}
	}
	if found {
		return best, true
	}
	return w.Contains(p, quote)
}

// IsAncestor reports whether a is an ancestor of b.
func (w *Workspace) IsAncestor(a, b string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", a, b)
	cmd.Dir = w.Root
	return cmd.Run() == nil
}

// DiffRange returns the unified diff between two commits.
func (w *Workspace) DiffRange(a, b string) (string, error) {
	return git(w.Root, "diff", "--no-color", "--no-ext-diff", a, b)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
