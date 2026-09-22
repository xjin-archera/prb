// Package config loads ~/.pr-review-board/config.json with defaults and repo auto-discovery.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Config is the user configuration. Missing keys keep their defaults.
type Config struct {
	Repos            map[string]string `json:"repos"`     // owner/repo -> local clone path
	ScanDirs         []string          `json:"scan_dirs"` // scanned one level deep for clones
	WorktreeRoot     string            `json:"worktree_root"`
	Runner           string            `json:"runner"` // "docker" (sandbox, no GitHub identity) | "host"
	DockerImage      string            `json:"docker_image"`
	DockerMemory     string            `json:"docker_memory"`
	DockerCPUs       float64           `json:"docker_cpus"`
	ClaudeOAuthToken string            `json:"claude_oauth_token"` // optional; env and Keychain are preferred
	MaxParallel      int               `json:"max_parallel"`
	ClaudeModel      string            `json:"claude_model"`
	MaxBudgetUSD     float64           `json:"max_budget_usd"`
	MaxTurns         int               `json:"max_turns"`
	AllowedTools     []string          `json:"allowed_tools"`
	DisallowedTools  []string          `json:"disallowed_tools"`
	ReviewSkill      string            `json:"review_skill"`   // optional Claude Code skill to run, e.g. "agent-skills:review"
	TicketPattern    string            `json:"ticket_pattern"` // regex that names worktrees after a ticket in the branch
	CleanupAfterPost bool              `json:"cleanup_after_post"`
	HideBots         bool              `json:"hide_bots"`
	IncludeMentions  bool              `json:"include_mentions"` // also list open PRs that mention you
	IncludeReviewed  bool              `json:"include_reviewed"` // also list open PRs you already reviewed (waiting for re-review)
	IncludeTeams     bool              `json:"include_teams"`    // also list PRs that request a team you belong to
	Host             string            `json:"host"`
	Port             int               `json:"port"`
}

// Paths under the state directory.
type Paths struct {
	StateDir string
	Config   string
	DB       string
	Logs     string
}

func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	if v := os.Getenv("PRB_STATE_DIR"); v != "" {
		home = "" // absolute override
		return Paths{StateDir: v, Config: filepath.Join(v, "config.json"), DB: filepath.Join(v, "prb.db"), Logs: filepath.Join(v, "logs")}
	}
	d := filepath.Join(home, ".pr-review-board")
	return Paths{StateDir: d, Config: filepath.Join(d, "config.json"), DB: filepath.Join(d, "prb.db"), Logs: filepath.Join(d, "logs")}
}

func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Repos:        map[string]string{},
		ScanDirs:     []string{filepath.Join(home, "projects")},
		WorktreeRoot: filepath.Join(home, "worktrees"),
		Runner:       "docker",
		DockerImage:  "pr-review-runner:latest",
		DockerMemory: "8g",
		DockerCPUs:   4,
		MaxParallel:  3,
		AllowedTools: []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "LS", "Skill", "Agent", "TodoWrite", "WebFetch", "WebSearch"},
		// The sandbox is the real guard; these stay denied as a belt-and-braces for the host runner.
		DisallowedTools:  []string{"Bash(gh:*)", "Bash(git push:*)", "Bash(git commit:*)"},
		TicketPattern:    `(?i)\b[A-Z][A-Z0-9]+-\d+\b`,
		CleanupAfterPost: false, // keep the worktree so follow-up reviews and chat can resume the session
		HideBots:         true,
		IncludeMentions:  true,
		IncludeReviewed:  true,
		IncludeTeams:     true,
		Host:             "127.0.0.1",
		Port:             8787,
	}
}

// Load reads the config file (creating it with defaults on first run) and fills in discovered repos.
func Load(p Paths) (Config, error) {
	for _, d := range []string{p.StateDir, p.Logs} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Config{}, err
		}
	}
	cfg := Default()
	data, err := os.ReadFile(p.Config)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", p.Config, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// created below
	default:
		return Config{}, err
	}
	if cfg.Repos == nil {
		cfg.Repos = map[string]string{}
	}
	for name, path := range DiscoverRepos(cfg.ScanDirs) {
		if _, ok := cfg.Repos[name]; !ok {
			cfg.Repos[name] = path
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := Save(p, cfg); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func Save(p Paths, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.Config, append(data, '\n'), 0o600)
}

// RepoPath returns the local clone for owner/repo, or "".
func (c Config) RepoPath(nameWithOwner string) string {
	p := c.Repos[nameWithOwner]
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, p[2:])
	}
	return p
}

// RemoteToName maps a git remote URL to owner/repo.
func RemoteToName(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), ".git")
	var tail string
	switch {
	case strings.HasPrefix(url, "git@"):
		_, tail, _ = strings.Cut(url, ":")
	case strings.Contains(url, "://"):
		parts := strings.Split(url, "/")
		if len(parts) < 2 {
			return ""
		}
		tail = strings.Join(parts[len(parts)-2:], "/")
	default:
		return ""
	}
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

// DiscoverRepos scans one level under each dir for git clones.
func DiscoverRepos(scanDirs []string) map[string]string {
	found := map[string]string{}
	for _, d := range scanDirs {
		if strings.HasPrefix(d, "~/") {
			home, _ := os.UserHomeDir()
			d = filepath.Join(home, d[2:])
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, n := range names {
			child := filepath.Join(d, n)
			if _, err := os.Stat(filepath.Join(child, ".git")); err != nil {
				continue
			}
			cmd := exec.Command("git", "-C", child, "remote", "get-url", "origin")
			done := make(chan []byte, 1)
			go func() { out, _ := cmd.Output(); done <- out }()
			var out []byte
			select {
			case out = <-done:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				continue
			}
			if name := RemoteToName(string(out)); name != "" {
				if _, ok := found[name]; !ok {
					found[name] = child
				}
			}
		}
	}
	return found
}
