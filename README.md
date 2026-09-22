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

Download a binary from the [releases page](https://github.com/xjin-archera/prb/releases) (macOS and Linux,
amd64 and arm64), put it on your PATH, or build from source:

```bash
go install github.com/xjin-archera/prb/cmd/prb@latest
```

Then:

```bash
prb setup          # checks gh, git, claude, docker; stores the sandbox token
prb build-image    # builds the Docker sandbox image (once; needs Docker running)
prb                # http://127.0.0.1:8787
```

Requirements on PATH: `gh` (logged in with `gh auth login`), `git`, `claude` (Claude Code, logged in), and
`docker` for the sandbox. Without Docker set `"runner": "host"` in the config: reviews then run `claude`
directly on your machine, guarded only by the tool denylist (no `gh`, `git push`, `git commit`).

The review recipe is built in. If you have a Claude Code review skill you prefer, name it in the config as
`review_skill` and the prompt tells Claude Code to use it; the app itself does not depend on any skill,
plugin, or `CLAUDE.md`. Inside the sandbox Claude Code sees your own `~/.claude` (skills, plugins, settings),
so it behaves like your shell does.

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
| `review_instructions` | `""` | free text appended to every review and follow-up prompt: house rules, focus areas, tone |
| `summary_format` | built-in section list | what `summary_body` must contain, e.g. `"two sections: Blockers, Notes; one line each"` |
| `ticket_pattern` | `(?i)\b[A-Z][A-Z0-9]+-\d+\b` | regex that names worktrees after the ticket in the branch |
| `allowed_tools` / `disallowed_tools` | read/run tools; edit tools are always stripped; `gh`, `git push`, `git commit` denied | headless tool policy (the sandbox and the git environment are the real guards) |
| `readonly_worktree` | false | docker: mount the worktree read-only |
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

## Security notes

- **Local only.** The server binds to `127.0.0.1` and has no login. Every request must carry the header
  htmx sends and a loopback `Host`, so another website open in your browser cannot trigger a post through
  the app, and a DNS-rebinding page is rejected.
- **Review only, never a fix.** The reviewer gets no edit tools (`Write`, `Edit` and friends are stripped
  from the allowlist; only `.pr-review/` is writable through them), git config passed through the
  environment makes `git push` fail however it is invoked, and `gh` has no login in either runner. After
  every run and every chat turn the app runs `git status` in the worktree, reverts any tracked file the
  reviewer changed, and shows a warning with the file names. `readonly_worktree: true` additionally mounts
  the worktree read-only in Docker (then installs and builds cannot run).
- **The sandbox has no GitHub identity.** It gets no `gh` config, no SSH keys, no `GH_TOKEN`. Posting to
  GitHub happens only on the host, only when you click Post, with your own `gh` login.
- **What the sandbox does see:** the PR worktree (read-write), the main clone's `.git` (read-only), and your
  `~/.claude` and `~/.claude.json` (read-write, so sessions persist and can be resumed). On Linux
  `~/.claude` can hold Claude Code's credentials file; the Claude Code token is needed there anyway.
- **The token** for the sandbox is read from the macOS Keychain, `CLAUDE_CODE_OAUTH_TOKEN`, or the config
  file (mode 0600), and is passed to `docker` through the process environment, never on the command line
  and never into logs.
- **Rendering.** Markdown from GitHub and from the model is rendered with raw HTML disabled; log lines and
  diff text are escaped. The diff shown is the one the review ran on.
- **What leaves your machine:** `gh` calls to GitHub, and the Claude Code session to Anthropic (the diff,
  the files it reads in the worktree, and your chat messages). Nothing else.

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
