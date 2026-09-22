package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A real git repo with a real remote: under noPushEnv, push must fail however it is invoked.
func TestNoPushEnvBlocksGitPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	must("git", "init", "-q", "--bare", remote)
	must("git", "clone", "-q", remote, work)
	for _, args := range [][]string{
		{"git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"},
	} {
		if out, err := run(nil, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	// sanity: without the env, push works
	if out, err := run(nil, "git", "push", "-q", "origin", "HEAD:refs/heads/ok"); err != nil {
		t.Fatalf("baseline push failed: %v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"git", "push", "origin", "HEAD:refs/heads/blocked"},
		{"git", "push"},
		{"sh", "-c", "git push origin HEAD:refs/heads/blocked2"},
	} {
		out, err := run(noPushEnv(), args...)
		if err == nil {
			t.Errorf("%v succeeded under noPushEnv:\n%s", args, out)
		}
	}
	// fetch still works
	if out, err := run(noPushEnv(), "git", "fetch", "-q", "origin"); err != nil {
		t.Errorf("fetch blocked: %v\n%s", err, out)
	}
	// gh has no login
	if _, err := exec.LookPath("gh"); err == nil {
		out, err := run(noPushEnv(), "gh", "auth", "status")
		if err == nil || !strings.Contains(strings.ToLower(out), "not logged") && !strings.Contains(strings.ToLower(out), "no ") {
			t.Errorf("gh still has a login under noPushEnv: %v\n%s", err, out)
		}
	}
}
