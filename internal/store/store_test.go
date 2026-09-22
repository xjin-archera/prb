package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	r, _ := st.Get(ctx, "o/r", 1)
	if r.Status != Idle {
		t.Fatalf("status = %s", r.Status)
	}
	r.Status = Done
	r.Result = &Result{Verdict: "APPROVE", Comments: []Comment{{Path: "a.py", Line: 1, Side: "RIGHT", Severity: "Nit", Body: "b", Include: true}}, Cut: []Cut{}}
	if err := st.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	_ = st.AddChat(ctx, r.Key(), ChatMessage{Role: "user", Text: "hi", At: 1})
	got, err := st.Get(ctx, "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != Done || got.Result == nil || len(got.Result.Comments) != 1 || got.Result.Comments[0].ID == "" || len(got.Chat) != 1 {
		t.Fatalf("got = %+v", got)
	}
	all, _ := st.All(ctx)
	if len(all) != 1 {
		t.Fatalf("all = %d", len(all))
	}
	if err := st.Delete(ctx, r.Key()); err != nil {
		t.Fatal(err)
	}
	if all, _ = st.All(ctx); len(all) != 0 {
		t.Fatal("delete failed")
	}
}
