// Package prompt builds the headless review prompt, the follow-up prompts, and the output schema.
package prompt

import (
	"fmt"
	"strings"
)

var Severities = []string{"Critical", "Important", "Suggestion", "Nit", "FYI"}

// Schema is the JSON the reviewer must write. Kept as text: it is only shown to the model.
const Schema = `{
  "verdict": "string — APPROVE or REQUEST CHANGES, then one sentence",
  "verified_locally": "string — exactly which tests/lint/build commands ran, with counts, and what could not run and why. Never fabricated.",
  "summary_body": "string — markdown review summary: Verdict, Overview, Critical Issues, Important Issues, Suggestions, What's Done Well, Verification Story. Reference inline comments by file:line; do not repeat their full text.",
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
  "lgtm": "boolean — true only when the verdict is APPROVE with no Critical or Important findings"
}`

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

func Build(pr PR, repo, baseRef, outPath, skill string) string {
	body := strings.TrimSpace(pr.Body)
	if body == "" {
		body = "(no description)"
	}
	existing := "(none)"
	if len(pr.Existing) > 0 {
		existing = strings.Join(pr.Existing, "\n")
	}
	recipe := defaultRecipe
	if skill != "" {
		recipe = fmt.Sprintf("Use the `%s` skill for the review.", skill)
	}
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
`, pr.Number, repo, pr.Title, pr.Author, pr.HeadRef, baseRef, head, baseRef, baseRef, body, existing, recipe, outPath, Schema)
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
		"Do not post to GitHub, commit, or push."
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
