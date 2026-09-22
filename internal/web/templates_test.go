package web

import (
	"bytes"
	"html/template"
	"testing"

	"github.com/xifengjin/prb/internal/config"
	"github.com/xifengjin/prb/internal/github"
	"github.com/xifengjin/prb/internal/store"
)

// Every template must execute against representative data; html/template only fails at execution time.
func TestTemplatesExecute(t *testing.T) {
	s, err := New(config.Default(), config.Paths{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := store.Comment{ID: "c1", Path: "a.py", Line: 3, Side: "RIGHT", Severity: "Important", Body: "**Important:** x", Include: true, AIGenerated: true}
	rv := store.Review{Repo: "o/r", Number: 1, Status: store.Done, HeadSHA: "abcdef1234", Worktree: "/tmp/wt", SessionID: "s",
		Result: &store.Result{Verdict: "APPROVE", SummaryBody: "ok", Comments: []store.Comment{c}, Cut: []store.Cut{{Finding: "f", Reason: "r"}}},
		Chat:   []store.ChatMessage{{Role: "user", Text: "hi", At: 1}, {Role: "assistant", Text: "yo `x`", At: 2}}}
	pr := github.PR{Repo: "o/r", Number: 1, Title: "T", Author: "a", HeadRef: "h", BaseRef: "b", HeadSHA: "abcdef1234", UpdatedAt: "2026-01-01T00:00:00Z", Reasons: []string{"mentioned"}}
	d := detailData{PR: pr, Key: "o/r#1", HasClone: true, Review: rv, Tab: "review", Body: template.HTML("x")}
	cases := map[string]any{
		"index":       map[string]any{"Filter": filter{HideBots: true}, "ClaudeMissing": true},
		"prlist":      listData{Rows: []prRow{{PR: pr, Key: "o/r#1", HasClone: true, Review: &rv, Stale: true}}, Authors: []authorCount{{Name: "a", Count: 1}}, Login: "me"},
		"detail":      d,
		"tabs":        d,
		"log":         d,
		"review":      d,
		"comment":     commentView{Key: "o/r#1", C: c},
		"comment_row": commentView{Key: "o/r#1", C: c, Editing: true, Diff: true},
		"chat":        chatData{Key: "o/r#1", Review: rv, Chatting: true, Prefill: "p"},
		"preflight":   map[string]any{"Key": "o/r#1", "Inline": 1, "Unanchored": []store.Comment{c}, "HeadMoved": true},
		"toast":       map[string]any{"Msg": "m", "Err": true, "OOB": true},
	}
	for name, data := range cases {
		var buf bytes.Buffer
		if err := s.tpl.ExecuteTemplate(&buf, name, data); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSinceTemplate(t *testing.T) {
	s, err := New(config.Default(), config.Paths{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	d := sinceData{Key: "o/r#1", Stale: true, Found: false, SinceLabel: "Jan 2 15:04",
		Commits: []github.Commit{{SHA: "abcdef1234", Message: "fix\n\nbody", Author: "a"}},
		Remarks: []github.Remark{{Kind: "inline", Author: "b", Path: "a.py", Line: 3, Body: "done"}, {Kind: "review", Author: "c", State: "APPROVED", Body: "lgtm"}}}
	if err := s.tpl.ExecuteTemplate(&buf, "since", d); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte("Review the changes")) || !bytes.Contains(buf.Bytes(), []byte("force push")) {
		t.Fatalf("since = %s", buf.String())
	}
}

// The page loads these by URL; a missing file would fail silently in the browser.
func TestStaticFilesEmbedded(t *testing.T) {
	for _, f := range []string{"static/app.js", "static/style.css", "static/vendor/htmx.min.js", "static/vendor/sse.js"} {
		if b, err := staticFS.ReadFile(f); err != nil || len(b) == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
}
