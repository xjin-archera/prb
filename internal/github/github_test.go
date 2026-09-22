package github

import "testing"

const sample = `diff --git a/server/src/app/foo.py b/server/src/app/foo.py
--- a/server/src/app/foo.py
+++ b/server/src/app/foo.py
@@ -10,4 +10,5 @@ def f():
     a = 1
-    b = 2
+    b = 3
+    c = 4
     return a
diff --git a/new.py b/new.py
--- /dev/null
+++ b/new.py
@@ -0,0 +1,2 @@
+x = 1
+y = 2
`

func TestDiffLineMap(t *testing.T) {
	m := DiffLineMap(sample)
	for _, n := range []int{10, 11, 12, 13} {
		if !m.Has("server/src/app/foo.py", "RIGHT", n) {
			t.Errorf("RIGHT %d missing", n)
		}
	}
	if m.Has("server/src/app/foo.py", "RIGHT", 14) || !m.Has("server/src/app/foo.py", "LEFT", 11) || m.Has("server/src/app/foo.py", "LEFT", 13) {
		t.Errorf("bad map: %v", m["server/src/app/foo.py"])
	}
	if !m.Has("new.py", "RIGHT", 2) || m.Has("new.py", "LEFT", 1) {
		t.Errorf("new file: %v", m["new.py"])
	}
}
