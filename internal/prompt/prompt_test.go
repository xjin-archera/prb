package prompt

import (
	"strings"
	"testing"
)

func TestBuildFollowUp(t *testing.T) {
	pr := PR{Number: 7, Title: "T"}
	f := FollowUp{PrevSHA: "aaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbb", Commits: []string{"- aaa fix"}, Discussion: []string{"- x: done"},
		DeltaPath: "/wt/.pr-review/delta.patch", PreviousRes: "/wt/.pr-review/previous.json", HasSession: true}
	p := BuildFollowUp(pr, "o/r", "main", "/wt/.pr-review/result.json", "", f)
	for _, want := range []string{"aaaaaaaaaa to bbbbbbbbbb", "delta.patch", "previous.json", "- aaa fix", "- x: done", `"resolved"`, "/wt/.pr-review/result.json", "earlier in this session"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q", want)
		}
	}
	f.DeltaPath, f.HasSession = "", false
	p = BuildFollowUp(pr, "o/r", "main", "/out", "agent-skills:review", f)
	if !strings.Contains(p, "force push") || !strings.Contains(p, "Read it first") || !strings.Contains(p, "`agent-skills:review`") {
		t.Errorf("fallback prompt wrong:\n%s", p)
	}
}

func TestBuildFirstReview(t *testing.T) {
	p := Build(PR{Number: 1, Title: "x", HeadSHA: "abcdefabcdef"}, "o/r", "web", "/out", "")
	if !strings.Contains(p, "five axes") || !strings.Contains(p, "never end your turn") || !strings.Contains(p, "/out") {
		t.Errorf("prompt wrong:\n%s", p)
	}
}
