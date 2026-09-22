package diff

import "testing"

const sample = `diff --git a/server/src/app/foo.py b/server/src/app/foo.py
index 1..2 100644
--- a/server/src/app/foo.py
+++ b/server/src/app/foo.py
@@ -10,4 +10,5 @@ def f():
     a = 1
-    b = 2
+    b = 3
+    c = 4
     return a
diff --git a/new.py b/new.py
new file mode 100644
--- /dev/null
+++ b/new.py
@@ -0,0 +1,2 @@
+x = 1
+y = 2
`

func TestParse(t *testing.T) {
	files := Parse(sample)
	if len(files) != 2 {
		t.Fatalf("files = %d", len(files))
	}
	f := files[0]
	if f.Path != "server/src/app/foo.py" || f.Adds != 2 || f.Dels != 1 {
		t.Fatalf("file0 = %+v", f)
	}
	l := f.Hunks[0].Lines
	if l[0].Type != "ctx" || l[0].OldN != 10 || l[0].NewN != 10 || l[0].Side != "RIGHT" || l[0].Line != 10 {
		t.Errorf("ctx line = %+v", l[0])
	}
	if l[1].Type != "del" || l[1].OldN != 11 || l[1].Side != "LEFT" || l[1].Line != 11 {
		t.Errorf("del line = %+v", l[1])
	}
	if l[2].Type != "add" || l[2].NewN != 11 || l[3].NewN != 12 || l[4].NewN != 13 || l[4].OldN != 12 {
		t.Errorf("add/ctx numbering = %+v %+v %+v", l[2], l[3], l[4])
	}
	if files[1].Status != "new" || files[1].Path != "new.py" {
		t.Errorf("file1 = %+v", files[1])
	}
}

func TestHighlightKeepsLineCount(t *testing.T) {
	files := Parse(sample)
	Highlight(files)
	for _, f := range files {
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				if l.HTML == "" && l.Text != "" {
					t.Errorf("%s: line %q not highlighted", f.Path, l.Text)
				}
			}
		}
	}
	if got := string(files[0].Hunks[0].Lines[1].HTML); got == "" || got == "    b = 2" {
		t.Errorf("expected spans in %q", got)
	}
}

func TestCSS(t *testing.T) {
	css := CSS("github", ".hl")
	if css == "" || !contains(css, ".hl .k") {
		t.Fatalf("css = %.200s", css)
	}
	if contains(css, ".chroma") || contains(css, "/*") {
		t.Fatalf("css not scoped: %.200s", css)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
