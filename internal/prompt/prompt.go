// Package prompt builds the headless review prompt, the follow-up prompts, and the output schema.
package prompt

import (
	"fmt"
	"strings"
)

var Severities = []string{"Critical", "Important", "Suggestion", "Nit", "FYI"}

// DefaultSummaryFormat is what summary_body must contain unless the config overrides it.
const DefaultSummaryFormat = "markdown review summary with these sections: Verdict, Overview, Critical Issues, " +
	"Important Issues, Suggestions, What's Done Well, Verification Story. Reference inline comments by file:line; " +
	"do not repeat their full text."

// Options are the configurable parts of the prompt.
type Options struct {
	Skill         string // Claude Code skill to run instead of the built-in recipe
	Instructions  string // appended verbatim
	SummaryFormat string // replaces DefaultSummaryFormat
}

func (o Options) summary() string {
	if strings.TrimSpace(o.SummaryFormat) != "" {
		return strings.TrimSpace(o.SummaryFormat)
	}
	return DefaultSummaryFormat
}

func (o Options) extra() string {
	if strings.TrimSpace(o.Instructions) == "" {
		return ""
	}
	return "\nAdditional instructions from the reviewer:\n" + strings.TrimSpace(o.Instructions) + "\n"
}

// Schema is the JSON the reviewer must write. Kept as text: it is only shown to the model.
// %s is the summary_body format.
const schemaTemplate = `{
  "verdict": "string — APPROVE or REQUEST CHANGES, then one sentence",
  "verified_locally": "string — exactly which tests/lint/build commands ran, with counts, and what could not run and why. Never fabricated.",
  "summary_body": "string — %s",
  "comments": [
    {
      "path": "string — repo-relative path",
      "line": "integer — a line that appears in the PR diff",
      "side": "RIGHT | LEFT",
      "severity": "Critical | Important | Suggestion | Nit | FYI",
      "body": "string — markdown starting with the bold severity label, e.g. **Important:** ..., with a specific fix for Critical/Important",
      "ai_generated": true
    }
  ],
  "cut": [{"finding": "string", "reason": "string"}],
  "resolved": [{"finding": "string", "note": "string"}],
  "lgtm": "boolean — true only when the verdict is APPROVE with no Critical or Important findings"
}`

// Schema is the default schema text (built-in summary format).
var Schema = fmt.Sprintf(schemaTemplate, strings.ReplaceAll(DefaultSummaryFormat, `"`, `\"`))

func (o Options) schema() string {
	return fmt.Sprintf(schemaTemplate, strings.ReplaceAll(o.summary(), `"`, `\"`))
}

// The built-in recipe, used when no Claude Code skill is configured.
const defaultRecipe = `Review the diff on five axes, in this order: correctness (bugs, edge cases, error handling, concurrency,
data loss), readability (naming, structure, dead code), architecture (boundaries, coupling, consistency with the
surrounding code), security (injection, authz, secrets, unsafe defaults), performance (N+1 queries, needless work
on hot paths). Prefer a few findings you are sure about over many guesses. Verify a claim by reading the code it
depends on before you make it. Every Critical or Important comment needs a concrete fix. Findings you considered
and dropped go in "cut" with the reason.`

// PR is the subset of PR detail the prompt uses.
type PR struct {
	Number   int
	Title    string
	Author   string
	HeadRef  string
	HeadSHA  string
	Body     string
	Existing []string // things already said on the PR
}

func Build(pr PR, repo, baseRef, outPath string, o Options) string {
	body := strings.TrimSpace(pr.Body)
	if body == "" {
		body = "(no description)"
	}
	existing := "(none)"
	if len(pr.Existing) > 0 {
		existing = strings.Join(pr.Existing, "\n")
	}
	recipe := defaultRecipe
	if o.Skill != "" {
		recipe = fmt.Sprintf("Use the `%s` skill for the review.", o.Skill)
	}
	recipe += o.extra()
	head := pr.HeadSHA
	if len(head) > 10 {
		head = head[:10]
	}
	return fmt.Sprintf(`Review pull request #%d of %s: %s
Author: %s. Branch `+"`%s`"+` into `+"`%s`"+`. Head %s.
This worktree is checked out at the PR head. The diff against `+"`origin/%s`"+` is in `+"`.pr-review/diff.patch`"+`
(`+"`git diff origin/%s...HEAD`"+` gives the same). Review only that diff.

PR description:
%s

Already said on the PR (do not repeat):
%s

%s
Run the tests and linters that the diff touches when the environment allows it; if something cannot run (no
database, missing service), say so instead of guessing. Do not post anything to GitHub and do not commit or push.

This is a headless run: nobody will wake you when a background task finishes. Never run commands in the
background and never end your turn to "wait" for something. Run long commands (installs, builds, schema
generation) in the foreground with a timeout; if one would take more than ~10 minutes, skip it and report it
under verified_locally as not run. Your turn must end only after result.json exists.

When done, write the review as JSON to exactly this path, then reply DONE:
    %s
Schema (types and meaning of each key):
%s
Every `+"`line`"+` must be a line that appears in the diff (RIGHT side unless the point is about deleted code).
`+"`lgtm`"+` is true only for APPROVE with no Critical or Important findings. `+"`ai_generated`"+` is true on every comment.
Put findings you dropped, and why, in `+"`cut`"+`.
`, pr.Number, repo, pr.Title, pr.Author, pr.HeadRef, baseRef, head, baseRef, baseRef, body, existing, recipe, outPath, o.schema())
}

// Nudge asks a session that ended without writing the file to finish.
func Nudge(outPath string) string {
	return fmt.Sprintf("Your turn ended but `%s` does not exist. This is a headless run: no one will wake you when a "+
		"background task finishes, so do not wait for one and do not start another. Use what you have verified so "+
		"far; list anything unfinished under verified_locally as not run. Write the result JSON to that exact path "+
		"now, following the schema you were given, then reply DONE.", outPath)
}

// Chat wraps a reviewer message for a turn in the review session.
func Chat(message, outPath string, hasSession bool) string {
	head := "You are continuing the code review of this pull request with the reviewer, who is reading " +
		"your review in a UI next to the diff. Answer briefly and concretely; cite file:line. " +
		"Never modify repository files: this is a review, not a fix. The only file you may write is the review " +
		"JSON under .pr-review/. Do not post to GitHub, commit, or push."
	var edit string
	if hasSession {
		edit = fmt.Sprintf("If the reviewer asks you to change the review (reword, drop, add, or re-rate a comment, "+
			"or change the summary), edit `%s` in place, keeping its schema, then say what you changed.", outPath)
	} else {
		edit = fmt.Sprintf("The previous review session is not available, so start from the stored review at `%s` "+
			"and the diff at `.pr-review/diff.patch`. If asked to change the review, edit that JSON file in "+
			"place, keeping its schema, then say what you changed.", outPath)
	}
	return head + "\n" + edit + "\n\nReviewer: " + message
}

// FollowUp is the context of an incremental re-review.
type FollowUp struct {
	PrevSHA     string
	NewSHA      string
	Commits     []string // "sha message (author)"
	Discussion  []string // remarks since the last review
	DeltaPath   string   // .pr-review/delta.patch, "" when the old head is unreachable
	PreviousRes string   // path of the previous result JSON
	HasSession  bool
}

func code(s string) string { return "`" + s + "`" }

func short10(s string) string {
	if len(s) > 10 {
		return s[:10]
	}
	return s
}

// BuildFollowUp asks for a review of the changes since the previous round.
func BuildFollowUp(pr PR, repo, baseRef, outPath string, o Options, f FollowUp) string {
	commits := "(none listed)"
	if len(f.Commits) > 0 {
		commits = strings.Join(f.Commits, "\n")
	}
	discussion := "(none)"
	if len(f.Discussion) > 0 {
		discussion = strings.Join(f.Discussion, "\n")
	}
	var delta string
	if f.DeltaPath != "" {
		delta = fmt.Sprintf("The changes since your review are in %s (%s).", code(f.DeltaPath),
			code(fmt.Sprintf("git diff %s...%s", short10(f.PrevSHA), short10(f.NewSHA))))
	} else {
		delta = fmt.Sprintf("The previous head %s is no longer reachable (force push), so treat the whole diff as changed and "+
			"compare against your previous findings by content.", short10(f.PrevSHA))
	}
	var memory string
	if f.HasSession {
		memory = "You reviewed this pull request earlier in this session; your findings are also saved in " + code(f.PreviousRes) + "."
	} else {
		memory = "Your previous review of this pull request is saved in " + code(f.PreviousRes) + " (same schema as below). Read it first."
	}
	recipe := ""
	if o.Skill != "" {
		recipe = fmt.Sprintf("Use the %s skill for the review of the new changes.", code(o.Skill))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Follow-up review of pull request #%d of %s: %s\n", pr.Number, repo, pr.Title)
	fmt.Fprintf(&b, "The PR was updated since your review. Head moved from %s to %s. The worktree is at the new head.\n", short10(f.PrevSHA), short10(f.NewSHA))
	fmt.Fprintf(&b, "The full diff against %s is in %s. %s\n%s\n\n", code("origin/"+baseRef), code(".pr-review/diff.patch"), delta, memory)
	fmt.Fprintf(&b, "New commits:\n%s\n\nNew discussion on the PR since your review (replies to you included):\n%s\n\n", commits, discussion)
	b.WriteString(`Do three things:
1. For each finding of your previous review, decide whether the new changes address it. Addressed findings go
   in "resolved" with a short note (what fixed it, or why it no longer applies). Findings still open stay in
   "comments", re-anchored to lines that exist in the current diff; reword them only if the code changed.
   If the author pushed back in the discussion and is right, resolve the finding and say so in the note.
2. Review only the new changes for new problems, with the same care as a first review. ` + recipe + `
3. Write the summary_body as a follow-up: what was addressed, what is still open, what is new. Keep it short.
` + o.extra() + `
Run the tests and linters the new changes touch when the environment allows it. This is a review, not a
fix: never modify, create, or delete repository files; the only file you write is the result JSON. Do not
post anything to GitHub and do not commit or push. This is a headless run: never run commands in the background and never
end your turn to "wait" for something; end only after result.json exists.

Write the review as JSON to exactly this path, then reply DONE:
    ` + outPath + `
Schema:
` + o.schema() + "\n")
	return b.String()
}
