package worktree

import (
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
