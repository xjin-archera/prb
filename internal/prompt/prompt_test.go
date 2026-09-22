package prompt

import (
	"strings"
	"testing"
)

func TestBuildFollowUp(t *testing.T) {
	pr := PR{Number: 7, Title: "T"}
	f := FollowUp{PrevSHA: "aaaaaaaaaaaa", NewSHA: "bbbbbbbbbbbb", Commits: []string{"- aaa fix"}, Discussion: []string{"- x: done"},
		DeltaPath: "/wt/.pr-review/delta.patch", PreviousRes: "/wt/.pr-review/previous.json", HasSession: true}
	p := BuildFollowUp(pr, "o/r", "main", "/wt/.pr-review/result.json", Options{}, f)
	for _, want := range []string{"aaaaaaaaaa to bbbbbbbbbb", "delta.patch", "previous.json", "- aaa fix", "- x: done", `"resolved"`, "/wt/.pr-review/result.json", "earlier in this session"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q", want)
		}
	}
	f.DeltaPath, f.HasSession = "", false
	p = BuildFollowUp(pr, "o/r", "main", "/out", Options{Skill: "agent-skills:review"}, f)
	if !strings.Contains(p, "force push") || !strings.Contains(p, "Read it first") || !strings.Contains(p, "`agent-skills:review`") {
		t.Errorf("fallback prompt wrong:\n%s", p)
	}
}

func TestBuildFirstReview(t *testing.T) {
	p := Build(PR{Number: 1, Title: "x", HeadSHA: "abcdefabcdef"}, "o/r", "web", "/out", Options{})
	if !strings.Contains(p, "five axes") || !strings.Contains(p, "never end your turn") || !strings.Contains(p, "/out") {
		t.Errorf("prompt wrong:\n%s", p)
	}
}

func TestOptionsShapeThePrompt(t *testing.T) {
	o := Options{Instructions: "Be terse. Never comment on formatting.", SummaryFormat: "two sections only: Blockers, Notes"}
	p := Build(PR{Number: 1}, "o/r", "main", "/out", o)
	for _, want := range []string{"Additional instructions from the reviewer:", "Be terse.", `"summary_body": "string — two sections only: Blockers, Notes"`} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(p, "What's Done Well") {
		t.Error("default summary format must be replaced")
	}
	f := BuildFollowUp(PR{Number: 1}, "o/r", "main", "/out", o, FollowUp{PrevSHA: "a", NewSHA: "b"})
	if !strings.Contains(f, "Be terse.") || !strings.Contains(f, "Blockers, Notes") {
		t.Error("follow-up prompt must carry the options too")
	}
}
