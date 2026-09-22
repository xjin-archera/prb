# prb — PR Review Board

A local web app for reviewing pull requests with Claude Code, without letting a bot post for you.

- Lists the open PRs that request your review (or mention you).
- Runs a headless Claude Code review for each one, in parallel, each in its own git worktree, inside a
  Docker sandbox that has **no GitHub identity** (it cannot post, push, or read your token).
- Shows the result next to the diff: edit, drop, re-rate, or add comments inline.
- Chat with the review session ("is this N+1 real?", "drop the nit on line 42") — Claude Code keeps the
  context of everything it read, and edits the review in place when you ask.
- Posts **one** GitHub review with all inline comments when you click Post. Nothing is posted otherwise.

One static binary. Frontend is server-rendered HTML with [htmx](https://htmx.org); no JavaScript build.

## Install

```bash
go install github.com/xifengjin/prb/cmd/prb@latest   # or download a release binary
prb setup          # checks gh, git, claude, docker; stores the sandbox token
prb build-image    # builds the Docker sandbox (once)
prb                # http://127.0.0.1:8787
```

Requirements on PATH: `gh` (logged in), `git`, `claude` (Claude Code), and `docker` for the sandbox.
A Claude subscription or API access for Claude Code.

## How a review runs

1. `gh api graphql` finds open PRs with `review-requested:<you>`, plus (each switchable in the config) PRs that
   mention you, PRs you already reviewed that are still open (`reviewed-by:<you>`, so re-review requests after
   fixes show up), and PRs that request a team you belong to (`team-review-requested`, teams read from
   `gh api user/teams`). Badges in the list say which one applies.
2. **Run review**: `git fetch origin pull/N/head` into your local clone, then a worktree at
   `~/worktrees/<repo>/<TICKET>-<N>` on branch `review/<TICKET>-<N>` (reused and hard-reset if it exists).
   Fetches into one clone are serialized, so parallel reviews never race on refs.
3. The diff is written to `.pr-review/diff.patch`. `claude -p` runs headless in the sandbox with
   `--output-format stream-json`. The prompt asks for a five-axis review (correctness, readability,
   architecture, security, performance), to run the touched tests and linters, and to write
   `.pr-review/result.json`. Set `review_skill` in the config to use a Claude Code skill instead.
4. If Claude Code ends its turn without writing the file (it "waited" for a background task), the same
   session is resumed and asked to finish, up to two times. A failed run with a stored session also gets a
   **Continue session** button.
5. You curate in the UI. **Check anchors** shows which comments sit on lines GitHub accepts.
6. **Post** builds one review (`POST /repos/{o}/{r}/pulls/{n}/reviews`). Comments whose line is not in
   the diff are folded into the summary body, never dropped. Comments flagged AI-generated get a
   `🤖 AI-generated suggestion (Claude):` prefix; toggle it per comment.

Live log per PR and live chat over SSE. Up to `max_parallel` (default 3) reviews at once.

## Follow-up reviews

When a reviewed PR gets new commits or new discussion, the detail page shows a **Since your review** panel:
the commits after the head you reviewed, and remarks by others since your post (conversation comments,
review bodies, inline replies). **Review the changes** resumes the same Claude Code session with the delta
(`git diff <reviewed>...<head>` in `.pr-review/delta.patch`), the new commits, the new discussion, and your
previous result. It decides which findings were addressed (they move to a **Resolved** list), re-anchors
the open ones, reviews only the new changes for new problems, and writes a short follow-up summary.
**Post follow-up** then posts one more review. Rounds are counted per PR.

A force push makes the old head unreachable; the panel says so and the review compares by content instead.
The worktree is kept after a post by default (`cleanup_after_post: false`) so the session context survives;
if it was removed, the follow-up recreates it at the same path and the session still resumes.

## Config

`~/.pr-review-board/config.json` is created on first run (`PRB_STATE_DIR` overrides the directory).

| key | default | meaning |
|---|---|---|
| `repos` | auto-discovered from `scan_dirs` | `owner/repo` → local clone path |
| `scan_dirs` | `["~/projects"]` | dirs scanned one level deep for clones |
| `worktree_root` | `~/worktrees` | |
| `runner` | `docker` | `docker` = sandbox with no GitHub identity; `host` = run `claude` directly (tool denylist only) |
| `max_parallel` | 3 | |
| `claude_model` | `""` | CLI default |
| `max_budget_usd` | 0 | stop a run when Claude Code's cost estimate passes this (an estimate, not a bill on a subscription) |
| `max_turns` | 0 | |
| `review_skill` | `""` | a Claude Code skill to run instead of the built-in recipe, e.g. `agent-skills:review` |
| `ticket_pattern` | `(?i)\b[A-Z][A-Z0-9]+-\d+\b` | regex that names worktrees after the ticket in the branch |
| `allowed_tools` / `disallowed_tools` | all normal tools; `gh`, `git push`, `git commit` denied | headless tool policy (the sandbox is the real guard) |
| `cleanup_after_post` | false | remove the worktree, branch and PR ref after a successful post (true breaks nothing, but the follow-up must recreate the worktree) |
| `hide_bots`, `include_mentions`, `include_reviewed`, `include_teams` | true | what the list includes besides direct review requests |
| `host`, `port` | `127.0.0.1`, 8787 | local only; there is no auth |

## Sandbox

The container gets: the worktree, the main repo's `.git` (read-only), `~/.claude` and `~/.claude.json`.
It does **not** get `~/.config/gh`, `~/.ssh`, or any `GH_TOKEN`. Network stays on (Claude API, package
installs). Capabilities dropped, 8g/4cpu by default.

The Keychain login of Claude Code is not visible inside the container, so `prb setup` stores a long-lived
token from `claude setup-token`: in the macOS Keychain (`pr-review-board/oauth`), or in the config on
Linux. `CLAUDE_CODE_OAUTH_TOKEN` in the environment also works. The token is never written to logs.

## Layout

```
cmd/prb/            main: serve | setup | build-image
internal/config     config.json, repo discovery
internal/github     gh wrappers: search, detail, diff, line-anchor map, post
internal/worktree   fetch pull/N/head, worktree add/reset, per-repo fetch lock
internal/runner     jobs, docker/claude command, stream-json parsing, nudge, chat
internal/store      SQLite (pure Go) — reviews, comments, chat
internal/diff       unified diff parser + chroma highlighting
internal/web        handlers, htmx templates, static files (embedded)
```

## Development

```bash
make test    # go test ./...
make lint    # gofmt + go vet
make run
```
