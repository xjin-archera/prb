package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

func TestShortName(t *testing.T) {
	re := regexp.MustCompile(`(?i)\b[A-Z][A-Z0-9]+-\d+\b`)
	cases := map[string]string{
		"PROJ-14813/feat/add-detail-card": "PROJ-14813-1",
		"proj-17605-thing":                "PROJ-17605-1",
		"feat/shard-tests":                "shard-tests-1",
	}
	for in, want := range cases {
		if got := ShortName(in, 1, re); got != want {
			t.Errorf("ShortName(%q) = %q, want %q", in, got, want)
		}
	}
	if ShortName("PROJ-1/fix/a", 29689, re) == ShortName("PROJ-1/fix/b", 29690, re) {
		t.Error("two PRs on one ticket must not share a worktree")
	}
	if got := ShortName("feat/x", 5, nil); got != "x-5" {
		t.Errorf("nil regexp: %q", got)
	}
}

func TestRevertChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("original\n"), 0o644)
	run("add", "a.txt")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("tampered\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "untracked.log"), []byte("x"), 0o644)
	files, err := RevertChanges(context.Background(), dir)
	if err != nil || len(files) != 1 || files[0] != "a.txt" {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "original\n" {
		t.Fatalf("not reverted: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "untracked.log")); err != nil {
		t.Fatal("untracked file must be left alone")
	}
	if files, _ := RevertChanges(context.Background(), dir); len(files) != 0 {
		t.Fatalf("clean tree reported %v", files)
	}
}
