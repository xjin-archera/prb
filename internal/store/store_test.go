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

func TestMigrationAndRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// simulate a database created before prev_sha/rounds existed
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.Exec(`CREATE TABLE legacy AS SELECT 1`); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"prev_sha", "rounds"} {
		if _, err := old.db.Exec(`ALTER TABLE reviews DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	st, err := Open(path) // must add the columns back
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	r := Review{Repo: "o/r", Number: 2, Status: Done, HeadSHA: "new", PrevSHA: "old", Rounds: 2,
		Result: &Result{Resolved: []Resolved{{Finding: "f", Note: "n"}}, Comments: []Comment{}, Cut: []Cut{}}}
	if err := st.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(ctx, "o/r", 2)
	if got.PrevSHA != "old" || got.Rounds != 2 || len(got.Result.Resolved) != 1 {
		t.Fatalf("got = %+v", got)
	}
}
