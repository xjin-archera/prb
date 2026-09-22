package runner

import (
	"testing"

	"github.com/xifengjin/prb/internal/store"
)

func TestParseResult(t *testing.T) {
	res, err := ParseResult([]byte(`{"verdict":"APPROVE","verified_locally":"ran tests","summary_body":"ok",
		"comments":[{"path":"/a.py","line":3,"side":"RIGHT","severity":"Nit","body":"x","ai_generated":true}],"cut":[],"lgtm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Comments) != 1 || res.Comments[0].Path != "a.py" || res.Comments[0].ID == "" || !res.Comments[0].Include {
		t.Fatalf("comments = %+v", res.Comments)
	}
	if _, err := ParseResult([]byte(`{"verdict":"x"}`)); err == nil {
		t.Fatal("expected missing keys error")
	}
}

func TestMergeResult(t *testing.T) {
	old := store.Result{Comments: []store.Comment{
		{ID: "keep", Path: "a.py", Line: 3, Side: "RIGHT", Body: "old", Include: false, AIGenerated: false},
		{ID: "dropped", Path: "b.py", Line: 9, Side: "RIGHT", Body: "gone"},
	}}
	upd := store.Result{Comments: []store.Comment{
		{ID: "n1", Path: "a.py", Line: 3, Side: "RIGHT", Body: "reworded", Include: true, AIGenerated: true},
		{ID: "n2", Path: "c.py", Line: 1, Side: "RIGHT", Body: "added", Include: true, AIGenerated: true},
	}}
	m := MergeResult(old, upd)
	if m.Comments[0].ID != "keep" || m.Comments[0].Include || m.Comments[0].AIGenerated || m.Comments[0].Body != "reworded" {
		t.Errorf("merged[0] = %+v", m.Comments[0])
	}
	if m.Comments[1].ID != "n2" || !m.Comments[1].Include {
		t.Errorf("merged[1] = %+v", m.Comments[1])
	}
}

func TestSummarizeEvent(t *testing.T) {
	ev := map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "ls"}},
	}}}
	out := summarizeEvent(ev)
	if len(out) != 2 || out[0] != "🤖 hello" || out[1] != "⚙ Bash: ls" {
		t.Errorf("out = %q", out)
	}
}

func TestParseResultResolved(t *testing.T) {
	res, err := ParseResult([]byte(`{"verdict":"APPROVE","verified_locally":"","summary_body":"","comments":[],"cut":[],
		"resolved":[{"finding":"N+1 in foo","note":"fixed by selectinload"}],"lgtm":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].Note != "fixed by selectinload" {
		t.Fatalf("resolved = %+v", res.Resolved)
	}
}

func TestReviewOnlyTools(t *testing.T) {
	got := reviewOnlyTools([]string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "Read", "Edit(foo/**)"})
	want := []string{"Bash", "Read", "Edit(.pr-review/**)"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestReviewedSHA(t *testing.T) {
	r := store.Review{Status: store.Done, HeadSHA: "new", PrevSHA: "old"}
	if ReviewedSHA(r) != "new" {
		t.Fatal("done review: HeadSHA is the reviewed head")
	}
	r.Status = store.Failed
	if ReviewedSHA(r) != "old" {
		t.Fatal("failed follow-up: the previous round's head is the reviewed head")
	}
	r.PrevSHA = ""
	if ReviewedSHA(r) != "new" {
		t.Fatal("failed first review: HeadSHA")
	}
}
