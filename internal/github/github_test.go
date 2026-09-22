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

func TestCommitsAfterAndFilterRemarks(t *testing.T) {
	cs := []Commit{{SHA: "a"}, {SHA: "b"}, {SHA: "c"}}
	after, found := CommitsAfter(cs, "a")
	if !found || len(after) != 2 || after[0].SHA != "b" {
		t.Errorf("after a = %v %v", after, found)
	}
	if after, found = CommitsAfter(cs, "zz"); found || len(after) != 3 {
		t.Errorf("unknown sha: %v %v", after, found)
	}
	rs := []Remark{
		{Author: "me", Body: "mine", CreatedAt: "2026-09-22T12:00:00Z"},
		{Author: "x", Body: "old", CreatedAt: "2026-09-21T00:00:00Z"},
		{Author: "y", Body: "late", CreatedAt: "2026-09-23T00:00:00Z"},
		{Author: "z", Body: "mid", CreatedAt: "2026-09-22T13:00:00Z"},
	}
	got := FilterRemarks(rs, "2026-09-22T11:00:00Z", "me")
	if len(got) != 2 || got[0].Author != "z" || got[1].Author != "y" {
		t.Errorf("filtered = %+v", got)
	}
}
