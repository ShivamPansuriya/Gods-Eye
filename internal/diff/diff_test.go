package diff

import (
	"os"
	"strings"
	"testing"
)

func TestParseExample(t *testing.T) {
	b, err := os.ReadFile("../../examples/retry.diff")
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(d.Files))
	}
	hs := d.Hunks()
	if len(hs) != 3 || hs[0].ID != "H1" || hs[2].ID != "H3" {
		t.Fatalf("unexpected hunks: %d", len(hs))
	}
	w := d.Files[1]
	if w.Path != "internal/index/writer.go" || w.Adds != 25 || w.Dels != 1 {
		t.Fatalf("writer.go = %+v", w)
	}
	// New-file line numbers must line up with the source.
	for _, l := range hs[1].Lines {
		if l.Kind == Add && strings.Contains(l.Text, "time.Sleep(backoff(attempt))") && l.New != 29 {
			t.Fatalf("sleep line = %d, want 29", l.New)
		}
	}
	if !strings.Contains(d.Annotated(), "--- HUNK H2 ") {
		t.Fatal("annotated diff missing hunk label")
	}
}

func TestParseNewDeletedRenamed(t *testing.T) {
	src := `diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+a
+b
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-x
diff --git a/old name.go b/new name.go
similarity index 100%
rename from old name.go
rename to new name.go
`
	d, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, f := range d.Files {
		got = append(got, f.Status+":"+f.Path)
	}
	want := "added:new.txt deleted:gone.txt renamed:new name.go"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	if d.Files[1].Path != "gone.txt" {
		t.Fatalf("deleted file path = %q", d.Files[1].Path)
	}
}
