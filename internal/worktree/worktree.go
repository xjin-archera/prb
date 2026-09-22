// Package worktree fetches pull/N/head and keeps one isolated git worktree per PR.
package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", &Error{Msg: fmt.Sprintf("git %s failed: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))}
	}
	return out.String(), nil
}

// Parallel reviews share one clone. Two fetches that update the same ref at once fail with
// "cannot lock ref ... but expected ...", so fetches are serialized per repository.
var fetchLocks sync.Map // repo path -> *sync.Mutex

func fetchLock(repo string) *sync.Mutex {
	m, _ := fetchLocks.LoadOrStore(repo, &sync.Mutex{})
	return m.(*sync.Mutex)
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// ShortName names the worktree after the ticket in the branch, always with the PR number:
// several PRs can share one ticket and must not share a worktree.
func ShortName(headRef string, number int, ticketRe *regexp.Regexp) string {
	if ticketRe != nil {
		if m := ticketRe.FindString(headRef); m != "" {
			return fmt.Sprintf("%s-%d", strings.ToUpper(m), number)
		}
	}
	last := headRef
	if i := strings.LastIndex(headRef, "/"); i >= 0 {
		last = headRef[i+1:]
	}
	slug := strings.ToLower(strings.Trim(slugRe.ReplaceAllString(last, "-"), "-"))
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "pr"
	}
	return fmt.Sprintf("%s-%d", slug, number)
}

// Ensure fetches the PR head and (re)creates <root>/<repo>/<SHORT> on branch review/<SHORT>.
func Ensure(ctx context.Context, repoPath, root string, number int, headRef, baseRef, expectedSHA string, ticketRe *regexp.Regexp, log func(string)) (string, error) {
	name := ShortName(headRef, number, ticketRe)
	branch := "review/" + name
	wt := filepath.Join(root, filepath.Base(repoPath), name)
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return "", err
	}
	prRef := fmt.Sprintf("refs/pr-review/%d", number)

	mu := fetchLock(repoPath)
	mu.Lock()
	// Two fetches: with several refspecs, FETCH_HEAD would point at the first one (the base), not the PR head.
	log("$ git fetch origin " + baseRef)
	_, err := git(ctx, repoPath, "fetch", "origin", baseRef)
	if err == nil {
		log(fmt.Sprintf("$ git fetch origin +pull/%d/head:%s", number, prRef))
		_, err = git(ctx, repoPath, "fetch", "origin", fmt.Sprintf("+pull/%d/head:%s", number, prRef))
	}
	mu.Unlock()
	if err != nil {
		return "", err
	}
	head, err := git(ctx, repoPath, "rev-parse", prRef)
	if err != nil {
		return "", err
	}
	head = strings.TrimSpace(head)
	if expectedSHA != "" && head != expectedSHA {
		return "", &Error{Msg: fmt.Sprintf("fetched PR head %.10s but GitHub reports %.10s; refresh the PR list and retry", head, expectedSHA)}
	}

	if _, statErr := os.Stat(filepath.Join(wt, ".git")); statErr == nil {
		log(fmt.Sprintf("reusing worktree %s, resetting to %.10s", wt, head))
		for _, args := range [][]string{
			{"checkout", "-q", "-B", branch, head},
			{"reset", "--hard", "-q", head},
			{"clean", "-fdq", "-e", ".claude", "-e", ".pr-review"},
		} {
			if _, err := git(ctx, wt, args...); err != nil {
				return "", err
			}
		}
	} else {
		_ = os.RemoveAll(wt)
		_, _ = git(ctx, repoPath, "branch", "-D", branch) // drop a stale branch not checked out anywhere
		log(fmt.Sprintf("$ git worktree add %s -b %s %.10s", wt, branch, head))
		if _, err := git(ctx, repoPath, "worktree", "add", "-q", wt, "-b", branch, head); err != nil {
			return "", err
		}
	}

	// The agent gets the diff as a file, so it needs no network and no GitHub access.
	outDir := filepath.Join(wt, ".pr-review")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}
	patch, err := git(ctx, wt, "diff", "origin/"+baseRef+"...HEAD")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(outDir, "diff.patch"), []byte(patch), 0o644); err != nil {
		return "", err
	}
	stat, _ := git(ctx, wt, "diff", "--stat", "origin/"+baseRef+"...HEAD")
	_ = os.WriteFile(filepath.Join(outDir, "diff.stat"), []byte(stat), 0o644)
	return wt, nil
}

// Remove deletes the worktree and its review/* branch.
func Remove(ctx context.Context, repoPath, wt string) error {
	if _, err := git(ctx, repoPath, "worktree", "remove", "--force", wt); err != nil {
		return err
	}
	_, _ = git(ctx, repoPath, "branch", "-D", "review/"+filepath.Base(wt))
	return nil
}

// DropPRRef removes refs/pr-review/N.
func DropPRRef(ctx context.Context, repoPath string, number int) {
	_, _ = git(ctx, repoPath, "update-ref", "-d", fmt.Sprintf("refs/pr-review/%d", number))
}

// Diff returns `git diff from...to` run in the worktree, or "" when `from` is unknown (force push).
func Diff(ctx context.Context, wt, from, to string) (string, bool) {
	out, err := git(ctx, wt, "diff", from+"..."+to)
	if err != nil {
		return "", false
	}
	return out, true
}

// RevertChanges lists tracked files the reviewer modified or deleted and restores them from HEAD.
// Untracked files (build output, installs) are left alone; the next run cleans them.
func RevertChanges(ctx context.Context, wt string) ([]string, error) {
	out, err := git(ctx, wt, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if len(line) > 3 {
			files = append(files, strings.TrimSpace(line[3:]))
		}
	}
	if len(files) == 0 {
		return nil, nil
	}
	if _, err := git(ctx, wt, "checkout", "-q", "--", "."); err != nil {
		return files, err
	}
	return files, nil
}
