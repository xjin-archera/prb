// Package runner runs headless Claude Code reviews in parallel worktrees, streams their logs,
// parses the result, and runs chat turns in the same session.
package runner

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xifengjin/prb/internal/config"
	"github.com/xifengjin/prb/internal/github"
	"github.com/xifengjin/prb/internal/prompt"
	"github.com/xifengjin/prb/internal/store"
	"github.com/xifengjin/prb/internal/worktree"
)

const maxNudges = 2

// Mode selects how a job starts.
type Mode int

const (
	ModeFull     Mode = iota // fetch, fresh session, whole diff
	ModeResume               // continue a session that ended without result.json
	ModeFollowUp             // review the changes since the previous round in the same session
)

type job struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Manager owns the review jobs and chat turns.
type Manager struct {
	cfg      config.Config
	paths    config.Paths
	st       *store.Store
	Logs     *Broadcaster // per review key: log lines
	Events   *Broadcaster // key "*": JSON {key,status}
	ChatBus  *Broadcaster // per review key: JSON chat events
	sem      chan struct{}
	mu       sync.Mutex
	jobs     map[string]*job
	chats    map[string]*job
	ticketRe *regexp.Regexp
}

func New(cfg config.Config, paths config.Paths, st *store.Store) *Manager {
	var re *regexp.Regexp
	if cfg.TicketPattern != "" {
		re, _ = regexp.Compile(cfg.TicketPattern)
	}
	n := cfg.MaxParallel
	if n < 1 {
		n = 1
	}
	return &Manager{
		cfg: cfg, paths: paths, st: st,
		Logs: NewBroadcaster(2000), Events: NewBroadcaster(0), ChatBus: NewBroadcaster(0),
		sem: make(chan struct{}, n), jobs: map[string]*job{}, chats: map[string]*job{}, ticketRe: re,
	}
}

func (m *Manager) LogPath(repo string, number int) string {
	return filepath.Join(m.paths.Logs, strings.ReplaceAll(repo, "/", "__")+fmt.Sprintf("__%d.log", number))
}

func (m *Manager) IsRunning(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[key] != nil
}

func (m *Manager) IsChatting(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chats[key] != nil
}

func (m *Manager) notify(key, status string) {
	b, _ := json.Marshal(map[string]string{"key": key, "status": status})
	m.Events.Publish("*", string(b))
}

// Start queues a review job in the given mode.
func (m *Manager) Start(ctx context.Context, pr github.PR, mode Mode) error {
	key := pr.Key()
	if m.IsRunning(key) {
		return nil
	}
	r, err := m.st.Get(ctx, pr.Repo, pr.Number)
	if err != nil {
		return err
	}
	switch mode {
	case ModeResume:
		if r.SessionID == "" {
			return errors.New("no Claude Code session stored for this review; re-run it")
		}
		if fi, err := os.Stat(r.Worktree); r.Worktree == "" || err != nil || !fi.IsDir() {
			return errors.New("the review worktree is gone; re-run it")
		}
	case ModeFollowUp:
		if r.Result == nil || r.HeadSHA == "" {
			return errors.New("no completed review to follow up on; run a full review")
		}
		if r.HeadSHA == pr.HeadSHA {
			return errors.New("the PR head has not moved since the last review")
		}
		if r.Status == store.Done || r.Status == store.PostedS {
			r.PrevSHA = r.HeadSHA // a failed follow-up keeps the earlier PrevSHA for the retry
		}
		r.HeadSHA = pr.HeadSHA
		m.Logs.Reset(key)
	default:
		r.HeadSHA = pr.HeadSHA
		m.Logs.Reset(key)
	}
	r.Status, r.Error, r.Posted = store.Queued, "", nil
	if err := m.st.Save(ctx, r); err != nil {
		return err
	}
	jctx, cancel := context.WithCancel(context.Background())
	j := &job{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.jobs[key] = j
	m.mu.Unlock()
	m.notify(key, store.Queued)
	go func() {
		defer close(j.done)
		defer func() {
			m.mu.Lock()
			delete(m.jobs, key)
			m.mu.Unlock()
		}()
		m.run(jctx, pr, mode)
	}()
	return nil
}

func (m *Manager) Cancel(key string) {
	m.mu.Lock()
	j := m.jobs[key]
	m.mu.Unlock()
	if j != nil {
		j.cancel()
	}
}

// Shutdown stops every job so no orphan container keeps reviewing after the server exits.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	var all []*job
	for _, j := range m.jobs {
		all = append(all, j)
	}
	for _, j := range m.chats {
		all = append(all, j)
	}
	m.mu.Unlock()
	for _, j := range all {
		j.cancel()
	}
	if m.cfg.Runner == "docker" {
		out, _ := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "name=^prb-").Output()
		if ids := strings.Fields(string(out)); len(ids) > 0 {
			_ = exec.CommandContext(ctx, "docker", append([]string{"stop", "-t", "5"}, ids...)...).Run()
		}
	}
	for _, j := range all {
		select {
		case <-j.done:
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) openLog(repo string, number int, appendMode bool) (*os.File, error) {
	flag := os.O_CREATE | os.O_WRONLY
	if appendMode {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	return os.OpenFile(m.LogPath(repo, number), flag, 0o644)
}

func (m *Manager) run(ctx context.Context, pr github.PR, mode Mode) {
	key := pr.Key()
	logf, lerr := m.openLog(pr.Repo, pr.Number, mode == ModeResume)
	if lerr == nil {
		defer logf.Close()
	}
	log := func(line string) {
		msg := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), line)
		if logf != nil {
			_, _ = logf.WriteString(msg + "\n")
		}
		m.Logs.Publish(key, msg)
	}
	bg := context.Background()
	r, err := m.st.Get(bg, pr.Repo, pr.Number)
	if err != nil {
		log("FAILED: " + err.Error())
		return
	}
	finish := func(status, errMsg string) {
		r.Status, r.Error = status, errMsg
		r.FinishedAt = store.Now()
		if err := m.st.Save(bg, r); err != nil {
			log("FAILED to save: " + err.Error())
		}
		m.notify(key, status)
	}

	// wait for a slot
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		log("cancelled")
		finish(store.Failed, "cancelled")
		return
	}
	defer func() { <-m.sem }()

	r.Status, r.StartedAt = store.Running, store.Now()
	_ = m.st.Save(bg, r)
	m.notify(key, store.Running)

	err = m.review(ctx, pr, &r, mode, log)
	switch {
	case ctx.Err() != nil:
		log("cancelled")
		finish(store.Failed, "cancelled")
	case err != nil:
		log("FAILED: " + err.Error())
		finish(store.Failed, err.Error())
	default:
		finish(store.Done, "")
	}
}

func (m *Manager) review(ctx context.Context, pr github.PR, r *store.Review, mode Mode, log func(string)) error {
	resume := mode == ModeResume
	key := pr.Key()
	repoPath := m.cfg.RepoPath(pr.Repo)
	if fi, err := os.Stat(repoPath); repoPath == "" || err != nil || !fi.IsDir() {
		return fmt.Errorf("no local clone configured for %s; add it to %s under 'repos'", pr.Repo, m.paths.Config)
	}
	if m.cfg.Runner == "docker" {
		if err := m.ensureImage(ctx); err != nil {
			return err
		}
		if _, err := m.oauthToken(); err != nil {
			return err
		}
	}
	t0 := time.Now()
	var wt, outPath, sessionID string
	cost := r.CostUSD
	if resume {
		wt = r.Worktree
		outPath = filepath.Join(wt, ".pr-review", "result.json")
		sessionID = r.SessionID
		log(fmt.Sprintf("continuing session %.8s in %s", sessionID, wt))
	} else {
		log("fetching PR detail for " + key)
		detail, err := github.PRDetail(ctx, pr.Repo, pr.Number)
		if err != nil {
			return err
		}
		prevResult := r.Result // for a follow-up; nil otherwise
		root := m.cfg.WorktreeRoot
		if strings.HasPrefix(root, "~/") {
			home, _ := os.UserHomeDir()
			root = filepath.Join(home, root[2:])
		}
		wt, err = worktree.Ensure(ctx, repoPath, root, pr.Number, pr.HeadRef, pr.BaseRef, pr.HeadSHA, m.ticketRe, log)
		if err != nil {
			return err
		}
		r.Worktree = wt
		_ = m.st.Save(context.Background(), *r)

		outDir := filepath.Join(wt, ".pr-review")
		outPath = filepath.Join(outDir, "result.json")
		_ = os.Remove(outPath)
		p := prompt.PR{Number: detail.Number, Title: detail.Title, Author: detail.Author.Login, HeadRef: detail.HeadRefName,
			HeadSHA: detail.HeadRefOid, Body: detail.Body}
		var text string
		var extra []string
		if mode == ModeFollowUp && prevResult != nil {
			f, err := m.followUpContext(ctx, pr, r, wt, prevResult, log)
			if err != nil {
				return err
			}
			text = prompt.BuildFollowUp(p, pr.Repo, pr.BaseRef, outPath, m.cfg.ReviewSkill, f)
			if f.HasSession {
				extra = []string{"--resume", r.SessionID}
			}
		} else {
			for _, c := range detail.Comments {
				p.Existing = append(p.Existing, fmt.Sprintf("- %s: %.400s", c.Author.Login, c.Body))
			}
			for _, rv := range detail.Reviews {
				if rv.Body != "" {
					p.Existing = append(p.Existing, fmt.Sprintf("- review by %s (%s): %.400s", rv.Author.Login, rv.State, rv.Body))
				}
			}
			text = prompt.Build(p, pr.Repo, pr.BaseRef, outPath, m.cfg.ReviewSkill)
			r.Chat = nil
			_ = m.st.ClearChat(context.Background(), key)
		}
		_ = os.WriteFile(filepath.Join(outDir, "prompt.md"), []byte(text), 0o644)

		cmd := m.claudeCmd(wt, text, extra)
		log("$ " + redactCmd(cmd, text))
		c, _, sid, err := m.streamClaude(ctx, key, cmd, wt, log, nil)
		if err != nil {
			return err
		}
		cost, sessionID = c, sid
		if mode == ModeFollowUp && sid == "" && r.SessionID != "" {
			sessionID = r.SessionID
		}
	}
	// Headless runs sometimes end the turn to "wait" for a background task. Nudge the same session to finish
	// instead of failing the whole review. A manual Continue starts here directly.
	for n := 1; n <= maxNudges && sessionID != "" && !exists(outPath); n++ {
		log(fmt.Sprintf("Claude Code ended without writing result.json; asking it to finish (%d/%d)", n, maxNudges))
		cmd := m.claudeCmd(wt, prompt.Nudge(outPath), []string{"--resume", sessionID})
		more, _, sid, err := m.streamClaude(ctx, key, cmd, wt, log, nil)
		if err != nil {
			return err
		}
		cost += more
		if sid != "" {
			sessionID = sid
		}
	}
	if resume {
		r.DurationS += time.Since(t0).Seconds()
	} else {
		r.DurationS = time.Since(t0).Seconds()
	}
	r.DurationS = float64(int(r.DurationS*10)) / 10
	r.CostUSD = cost
	if sessionID != "" {
		r.SessionID = sessionID
	}
	if !exists(outPath) {
		return errors.New("Claude Code finished but did not write result.json — see log")
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		return err
	}
	res, err := ParseResult(data)
	if err != nil {
		return err
	}
	r.Result = &res
	switch {
	case mode == ModeFollowUp:
		r.Rounds++
		if r.Rounds < 2 {
			r.Rounds = 2
		}
	case r.Rounds == 0:
		r.Rounds = 1
	}
	log(fmt.Sprintf("done: %d comments, %d resolved, %d cut, lgtm=%v, $%.2f, %.1fs", len(res.Comments), len(res.Resolved), len(res.Cut), res.LGTM, cost, r.DurationS))
	return nil
}

// followUpContext gathers what changed since the previous round: delta patch, new commits, new discussion.
func (m *Manager) followUpContext(ctx context.Context, pr github.PR, r *store.Review, wt string, prev *store.Result, log func(string)) (prompt.FollowUp, error) {
	outDir := filepath.Join(wt, ".pr-review")
	f := prompt.FollowUp{PrevSHA: r.PrevSHA, NewSHA: pr.HeadSHA, PreviousRes: filepath.Join(outDir, "previous.json")}
	pb, _ := json.MarshalIndent(prev, "", "  ")
	if err := os.WriteFile(f.PreviousRes, pb, 0o644); err != nil {
		return f, err
	}
	if delta, ok := worktree.Diff(ctx, wt, r.PrevSHA, "HEAD"); ok {
		f.DeltaPath = filepath.Join(outDir, "delta.patch")
		if err := os.WriteFile(f.DeltaPath, []byte(delta), 0o644); err != nil {
			return f, err
		}
		log(fmt.Sprintf("delta %.10s...%.10s: %d lines", r.PrevSHA, pr.HeadSHA, strings.Count(delta, "\n")))
	} else {
		log(fmt.Sprintf("previous head %.10s unreachable (force push?); reviewing the full diff", r.PrevSHA))
	}
	commits, err := github.PRCommits(ctx, pr.Repo, pr.Number)
	if err != nil {
		return f, err
	}
	after, found := github.CommitsAfter(commits, r.PrevSHA)
	if !found {
		log("previous head not in the PR's commit list; listing all commits")
	}
	for _, c := range after {
		msg, _, _ := strings.Cut(c.Message, "\n")
		f.Commits = append(f.Commits, fmt.Sprintf("- %.10s %s (%s)", c.SHA, msg, c.Author))
	}
	since := time.Unix(r.FinishedAt, 0).UTC().Format(time.RFC3339)
	if r.Posted != nil && r.Posted.At > 0 {
		since = time.Unix(r.Posted.At, 0).UTC().Format(time.RFC3339)
	}
	login, _ := github.CurrentLogin(ctx)
	remarks, err := github.PRDiscussionSince(ctx, pr.Repo, pr.Number, since, login)
	if err != nil {
		return f, err
	}
	for _, rm := range remarks {
		switch rm.Kind {
		case "inline":
			f.Discussion = append(f.Discussion, fmt.Sprintf("- %s on %s:%d: %.500s", rm.Author, rm.Path, rm.Line, rm.Body))
		case "review":
			f.Discussion = append(f.Discussion, fmt.Sprintf("- review by %s (%s): %.500s", rm.Author, rm.State, rm.Body))
		default:
			f.Discussion = append(f.Discussion, fmt.Sprintf("- %s: %.500s", rm.Author, rm.Body))
		}
	}
	// The session file is keyed by the worktree path, so resuming works even if the worktree was recreated.
	f.HasSession = r.SessionID != ""
	log(fmt.Sprintf("follow-up: %d new commits, %d new remarks, session=%v", len(after), len(remarks), f.HasSession))
	return f, nil
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func redactCmd(cmd []string, prompt string) string {
	var parts []string
	for _, c := range cmd {
		switch {
		case c == prompt:
			continue
		case strings.HasPrefix(c, "CLAUDE_CODE_OAUTH_TOKEN="):
			parts = append(parts, "CLAUDE_CODE_OAUTH_TOKEN=***")
		case strings.ContainsAny(c, " \t\"'"):
			parts = append(parts, fmt.Sprintf("%q", c))
		default:
			parts = append(parts, c)
		}
	}
	s := strings.Join(parts, " ")
	if len(s) > 400 {
		s = s[:400]
	}
	return s + " …"
}

// claudeCmd builds the claude invocation, wrapped in docker when the sandbox runner is on.
func (m *Manager) claudeCmd(wt, promptText string, extra []string) []string {
	base := []string{"claude", "-p", promptText}
	base = append(base, extra...)
	base = append(base, "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits")
	if len(m.cfg.AllowedTools) > 0 {
		base = append(base, "--allowedTools")
		base = append(base, m.cfg.AllowedTools...)
	}
	if len(m.cfg.DisallowedTools) > 0 {
		base = append(base, "--disallowedTools")
		base = append(base, m.cfg.DisallowedTools...)
	}
	if m.cfg.ClaudeModel != "" {
		base = append(base, "--model", m.cfg.ClaudeModel)
	}
	if m.cfg.MaxBudgetUSD > 0 {
		base = append(base, "--max-budget-usd", fmt.Sprintf("%g", m.cfg.MaxBudgetUSD))
	}
	if m.cfg.MaxTurns > 0 {
		base = append(base, "--max-turns", fmt.Sprintf("%d", m.cfg.MaxTurns))
	}
	if m.cfg.Runner != "docker" {
		return base
	}
	home, _ := os.UserHomeDir()
	tok, _ := m.oauthToken()
	repoGit := repoGitDir(wt)
	cmd := []string{
		"docker", "run", "--rm", "-i", "--name", ContainerName(wt),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--memory", m.cfg.DockerMemory, "--cpus", fmt.Sprintf("%g", m.cfg.DockerCPUs),
		"-v", wt + ":" + wt,
		"-v", repoGit + ":" + repoGit + ":ro", // the worktree's .git file points here
		"-v", filepath.Join(home, ".claude") + ":" + filepath.Join(home, ".claude"),
		"-v", filepath.Join(home, ".claude.json") + ":" + filepath.Join(home, ".claude.json"),
		"-e", "HOME=" + home,
		"-e", "GH_TOKEN=", "-e", "GITHUB_TOKEN=", // no GitHub identity: cannot post or push
		"-e", "CLAUDE_CODE_OAUTH_TOKEN=" + tok,
		"-w", wt, m.cfg.DockerImage,
	}
	return append(cmd, base...)
}

func ContainerName(wt string) string {
	return strings.ToLower("prb-" + filepath.Base(filepath.Dir(wt)) + "-" + filepath.Base(wt))
}

// repoGitDir resolves the main repository's .git directory from a worktree's .git file.
func repoGitDir(wt string) string {
	gitfile := filepath.Join(wt, ".git")
	if fi, err := os.Stat(gitfile); err == nil && fi.IsDir() {
		return gitfile
	}
	b, err := os.ReadFile(gitfile)
	if err != nil {
		return gitfile
	}
	target := strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	// <repo>/.git/worktrees/<name> -> <repo>/.git
	return filepath.Dir(filepath.Dir(target))
}

func (m *Manager) ensureImage(ctx context.Context) error {
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", m.cfg.DockerImage).Run(); err != nil {
		return fmt.Errorf("docker image %s not found; build it with: prb build-image (or docker build -t %s -f Dockerfile.runner .)", m.cfg.DockerImage, m.cfg.DockerImage)
	}
	return nil
}

var hexRe = regexp.MustCompile(`^[0-9a-f]+$`)

// oauthToken returns the headless Claude Code token for the sandbox: config, then env, then the macOS Keychain
// item created with `security add-generic-password -s pr-review-board -a oauth -w <token>`.
func (m *Manager) oauthToken() (string, error) {
	if m.cfg.ClaudeOAuthToken != "" {
		return m.cfg.ClaudeOAuthToken, nil
	}
	if v := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); v != "" {
		return v, nil
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password", "-s", "pr-review-board", "-a", "oauth", "-w").Output()
		tok := strings.TrimSpace(string(out))
		// `security -w` prints some values hex-encoded.
		if tok != "" && len(tok)%2 == 0 && hexRe.MatchString(tok) {
			if b, derr := hex.DecodeString(tok); derr == nil {
				tok = strings.TrimSpace(string(b))
			}
		}
		tok = strings.Join(strings.Fields(tok), "")
		if err == nil && tok != "" {
			return tok, nil
		}
	}
	return "", errors.New("no headless Claude Code token for the sandbox. Run `claude setup-token`, then `prb setup` to store it " +
		"(or set CLAUDE_CODE_OAUTH_TOKEN, or claude_oauth_token in config.json)")
}

// streamClaude runs the command, logs a summary of each stream-json event, and returns (cost, hadCost, sessionID).
func (m *Manager) streamClaude(ctx context.Context, key string, argv []string, wt string, log func(string), onEvent func(map[string]any)) (float64, bool, string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = wt
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, false, "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return 0, false, "", err
	}
	var cost float64
	hadCost := false
	sessionID := ""
	rd := bufio.NewReaderSize(stdout, 1<<20)
	for {
		raw, err := rd.ReadBytes('\n')
		if len(raw) > 0 {
			line := strings.TrimRight(string(raw), "\r\n")
			if line != "" {
				var ev map[string]any
				if json.Unmarshal([]byte(line), &ev) != nil {
					log(line)
				} else {
					if sid, ok := ev["session_id"].(string); ok && sid != "" {
						sessionID = sid
					}
					for _, msg := range summarizeEvent(ev) {
						log(msg)
					}
					if onEvent != nil {
						onEvent(ev)
					}
					if ev["type"] == "result" {
						if c, ok := ev["total_cost_usd"].(float64); ok {
							cost, hadCost = c, true
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return cost, hadCost, sessionID, ctx.Err()
		}
		return cost, hadCost, sessionID, fmt.Errorf("claude exited: %v", err)
	}
	return cost, hadCost, sessionID, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func contentBlocks(ev map[string]any) []map[string]any {
	msg, _ := ev["message"].(map[string]any)
	raw, _ := msg["content"].([]any)
	var out []map[string]any
	for _, b := range raw {
		if bm, ok := b.(map[string]any); ok {
			out = append(out, bm)
		}
	}
	return out
}

func toolBrief(block map[string]any) string {
	inp, _ := block["input"].(map[string]any)
	for _, k := range []string{"command", "file_path", "pattern", "description"} {
		if s := str(inp[k]); s != "" {
			return s
		}
	}
	return ""
}

func summarizeEvent(ev map[string]any) []string {
	var out []string
	switch ev["type"] {
	case "assistant":
		for _, b := range contentBlocks(ev) {
			switch b["type"] {
			case "text":
				if t := strings.TrimSpace(str(b["text"])); t != "" {
					out = append(out, "🤖 "+trunc(t, 600))
				}
			case "tool_use":
				out = append(out, fmt.Sprintf("⚙ %s: %s", str(b["name"]), trunc(toolBrief(b), 200)))
			}
		}
	case "user":
		for _, b := range contentBlocks(ev) {
			if b["type"] == "tool_result" && b["is_error"] == true {
				txt := str(b["content"])
				if txt == "" {
					j, _ := json.Marshal(b["content"])
					txt = string(j)
				}
				out = append(out, "✗ tool error: "+trunc(txt, 300))
			}
		}
	case "result":
		cost, _ := ev["total_cost_usd"].(float64)
		turns, _ := ev["num_turns"].(float64)
		out = append(out, fmt.Sprintf("result: %s turns=%d cost=$%.2f", str(ev["subtype"]), int(turns), cost))
		if ev["is_error"] == true {
			out = append(out, "error: "+trunc(fmt.Sprint(ev["result"]), 500))
		}
	}
	return out
}

// ParseResult validates result.json and converts it to a store.Result.
func ParseResult(data []byte) (store.Result, error) {
	var raw struct {
		Verdict         *string `json:"verdict"`
		VerifiedLocally *string `json:"verified_locally"`
		SummaryBody     *string `json:"summary_body"`
		LGTM            *bool   `json:"lgtm"`
		Comments        *[]struct {
			Path        string  `json:"path"`
			Line        float64 `json:"line"`
			Side        string  `json:"side"`
			Severity    string  `json:"severity"`
			Body        string  `json:"body"`
			AIGenerated *bool   `json:"ai_generated"`
		} `json:"comments"`
		Cut      *[]store.Cut     `json:"cut"`
		Resolved []store.Resolved `json:"resolved"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return store.Result{}, fmt.Errorf("result.json is not valid JSON: %w", err)
	}
	var missing []string
	for k, ok := range map[string]bool{"verdict": raw.Verdict != nil, "verified_locally": raw.VerifiedLocally != nil,
		"summary_body": raw.SummaryBody != nil, "comments": raw.Comments != nil, "cut": raw.Cut != nil, "lgtm": raw.LGTM != nil} {
		if !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return store.Result{}, fmt.Errorf("result.json missing keys: %s", strings.Join(missing, ", "))
	}
	res := store.Result{Verdict: *raw.Verdict, VerifiedLocally: *raw.VerifiedLocally, SummaryBody: *raw.SummaryBody,
		LGTM: *raw.LGTM, Cut: *raw.Cut, Resolved: raw.Resolved, Comments: []store.Comment{}}
	for _, c := range *raw.Comments {
		side := c.Side
		if side != "LEFT" {
			side = "RIGHT"
		}
		sev := c.Severity
		if sev == "" {
			sev = "Nit"
		}
		ai := true
		if c.AIGenerated != nil {
			ai = *c.AIGenerated
		}
		res.Comments = append(res.Comments, store.Comment{ID: store.NewID(), Path: strings.TrimLeft(c.Path, "/"), Line: int(c.Line),
			Side: side, Severity: sev, Body: c.Body, Include: true, AIGenerated: ai})
	}
	return res, nil
}

// MergeResult keeps the reviewer's include/AI-prefix choices on comments that survived an edit by Claude Code.
func MergeResult(old, upd store.Result) store.Result {
	type k struct {
		p string
		l int
		s string
	}
	prev := map[k]store.Comment{}
	for _, c := range old.Comments {
		prev[k{c.Path, c.Line, c.Side}] = c
	}
	for i, c := range upd.Comments {
		if p, ok := prev[k{c.Path, c.Line, c.Side}]; ok {
			upd.Comments[i].ID, upd.Comments[i].Include, upd.Comments[i].AIGenerated = p.ID, p.Include, p.AIGenerated
		}
	}
	return upd
}

// ---------- chat ----------

func (m *Manager) chatPublish(key string, ev map[string]any) {
	b, _ := json.Marshal(ev)
	m.ChatBus.Publish(key, string(b))
}

// Chat sends one reviewer turn to the review's Claude Code session and streams the answer over ChatBus.
func (m *Manager) Chat(ctx context.Context, r store.Review, message string) error {
	key := r.Key()
	if m.IsRunning(key) {
		return errors.New("review is running")
	}
	if m.IsChatting(key) {
		return errors.New("a chat turn is already running")
	}
	if fi, err := os.Stat(r.Worktree); r.Worktree == "" || err != nil || !fi.IsDir() {
		return errors.New("the review worktree is gone (removed after post?); re-run the review to chat")
	}
	if err := m.st.AddChat(ctx, key, store.ChatMessage{Role: "user", Text: message, At: store.Now()}); err != nil {
		return err
	}
	jctx, cancel := context.WithCancel(context.Background())
	j := &job{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.chats[key] = j
	m.mu.Unlock()
	go func() {
		defer close(j.done)
		defer func() {
			m.mu.Lock()
			delete(m.chats, key)
			m.mu.Unlock()
		}()
		m.chatTurn(jctx, r, message)
	}()
	return nil
}

func (m *Manager) CancelChat(key string) {
	m.mu.Lock()
	j := m.chats[key]
	m.mu.Unlock()
	if j != nil {
		j.cancel()
	}
}

func (m *Manager) chatTurn(ctx context.Context, r store.Review, message string) {
	key := r.Key()
	bg := context.Background()
	outPath := filepath.Join(r.Worktree, ".pr-review", "result.json")
	var before time.Time
	if fi, err := os.Stat(outPath); err == nil {
		before = fi.ModTime()
	}
	logf, _ := m.openLog(r.Repo, r.Number, true)
	if logf != nil {
		defer logf.Close()
	}
	log := func(line string) {
		msg := fmt.Sprintf("[%s] chat: %s", time.Now().Format("15:04:05"), line)
		if logf != nil {
			_, _ = logf.WriteString(msg + "\n")
		}
		m.Logs.Publish(key, msg)
	}
	var parts []string
	onEvent := func(ev map[string]any) {
		if ev["type"] != "assistant" {
			return
		}
		for _, b := range contentBlocks(ev) {
			switch b["type"] {
			case "text":
				if t := str(b["text"]); strings.TrimSpace(t) != "" {
					parts = append(parts, t)
					m.chatPublish(key, map[string]any{"type": "text", "text": t})
				}
			case "tool_use":
				m.chatPublish(key, map[string]any{"type": "tool", "name": str(b["name"]), "brief": trunc(toolBrief(b), 200)})
			}
		}
	}
	m.chatPublish(key, map[string]any{"type": "start"})
	fail := func(msg string) {
		_ = m.st.AddChat(bg, key, store.ChatMessage{Role: "assistant", Text: msg, At: store.Now()})
		m.chatPublish(key, map[string]any{"type": "done", "updated": false, "error": msg})
	}
	var extra []string
	if r.SessionID != "" {
		extra = []string{"--resume", r.SessionID}
	}
	cmd := m.claudeCmd(r.Worktree, prompt.Chat(message, outPath, r.SessionID != ""), extra)
	log("user: " + trunc(message, 200))
	cost, hadCost, sid, err := m.streamClaude(ctx, key, cmd, r.Worktree, log, onEvent)
	if ctx.Err() != nil {
		fail("(cancelled)")
		return
	}
	if err != nil {
		log("FAILED: " + err.Error())
		fail("error: " + err.Error())
		return
	}
	cur, err := m.st.Get(bg, r.Repo, r.Number)
	if err != nil {
		fail("error: " + err.Error())
		return
	}
	if sid != "" {
		cur.SessionID = sid
	}
	if hadCost {
		cur.CostUSD += cost
	}
	var clean []string
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			clean = append(clean, t)
		}
	}
	text := strings.Join(clean, "\n\n")
	if text == "" {
		text = "(no reply)"
	}
	updated := false
	if fi, err := os.Stat(outPath); err == nil && fi.ModTime().After(before) && cur.Result != nil {
		if data, err := os.ReadFile(outPath); err == nil {
			if res, err := ParseResult(data); err == nil {
				merged := MergeResult(*cur.Result, res)
				cur.Result = &merged
				updated = true
				log("result.json changed; review reloaded")
			} else {
				log("result.json changed but does not parse: " + err.Error())
			}
		}
	}
	_ = m.st.Save(bg, cur)
	_ = m.st.AddChat(bg, key, store.ChatMessage{Role: "assistant", Text: text, At: store.Now()})
	m.chatPublish(key, map[string]any{"type": "done", "updated": updated, "cost": cost})
}
