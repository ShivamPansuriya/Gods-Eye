package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShivamPansuriya/gods-eye/internal/diff"
)

func repo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(p, s string) {
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(s), 0o644)
	}
	write("a/svc.go", "package a\n\nfunc Charge(id string) error {\n\treturn nil\n}\n")
	write("b/use.go", "package b\n\nfunc x() { a.Charge(\"1\") }\n")
	write("secret.txt", "top\n")
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init")
	head := run("rev-parse", "HEAD")
	// A later uncommitted edit must not leak into reads at the commit.
	write("a/svc.go", "package a\n// dirty\n")
	return dir, head
}

func TestCleanRefusesEscapes(t *testing.T) {
	for _, p := range []string{"../x", "a/../../x", "/../etc/passwd", ".git/config", ""} {
		if _, err := clean(p); err == nil {
			t.Errorf("clean(%q) allowed", p)
		}
	}
	if c, err := clean("./a/b.go"); err != nil || c != "a/b.go" {
		t.Errorf("clean(./a/b.go) = %q, %v", c, err)
	}
}

func TestReadGrepAtCommit(t *testing.T) {
	dir, head := repo(t)
	w, err := Open(dir, head)
	if err != nil {
		t.Fatal(err)
	}
	if out := w.Read("a/svc.go", 3, 4); !strings.Contains(out, "func Charge") || strings.Contains(out, "dirty") {
		t.Fatalf("read at commit leaked working tree:\n%s", out)
	}
	if out := w.Grep(`Charge\(`, ""); !strings.Contains(out, "b/use.go:3:") || strings.Contains(out, head) {
		t.Fatalf("grep output: %s", out)
	}
	if out := w.Grep(`Charge`, "b/*"); strings.Contains(out, "a/svc.go") {
		t.Fatalf("glob not applied: %s", out)
	}
	if out := w.Read("../../etc/passwd", 1, 2); !strings.HasPrefix(out, "error:") {
		t.Fatalf("escape not refused: %s", out)
	}
	if ln, ok := w.Contains("a/svc.go", "  func   Charge(id string) error {"); !ok || ln != 3 {
		t.Fatalf("Contains = %d %v", ln, ok)
	}
	if _, ok := w.Contains("a/svc.go", "func Refund()"); ok {
		t.Fatal("Contains found invented code")
	}
	if out := w.List("**/*.go"); !strings.Contains(out, "a/svc.go") || strings.Contains(out, "secret.txt") {
		t.Fatalf("list: %s", out)
	}
}

func TestSymbolsAndCallers(t *testing.T) {
	dir, head := repo(t)
	w, _ := Open(dir, head)
	d, err := diff.Parse("diff --git a/a/svc.go b/a/svc.go\n--- a/a/svc.go\n+++ b/a/svc.go\n@@ -1,3 +1,5 @@\n package a\n \n+func Charge(id string) error {\n+\treturn nil\n+}\n")
	if err != nil {
		t.Fatal(err)
	}
	syms := w.Callers(Symbols(d), 10, 3)
	if len(syms) != 1 || syms[0].Name != "Charge" || syms[0].Total != 1 || !strings.HasPrefix(syms[0].Users[0], "b/use.go:3:") {
		t.Fatalf("callers = %+v", syms)
	}
}

func TestDefName(t *testing.T) {
	cases := map[string]string{
		"func (w *Writer) Commit(ctx context.Context) error {":        "Commit",
		"  public List<Row> sectionOf(Relation r, int n) {":           "sectionOf",
		"export async function refundOrder(id: string) {":             "refundOrder",
		"export const listOrders = async (t: string) => {":            "listOrders",
		"public record RecordRows(List<Map<String, Object>> rows) {}": "RecordRows",
		"    def compute_total(self, items):":                         "compute_total",
		"    return this.db.query<Order>(":                            "",
		"if (x) {":                                                    "",
	}
	for in, want := range cases {
		if got := defName(in); got != want {
			t.Errorf("defName(%q) = %q, want %q", in, got, want)
		}
	}
}
