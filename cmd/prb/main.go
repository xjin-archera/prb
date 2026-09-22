// prb: a local PR review board. Lists PRs that want your review, runs headless Claude Code reviews in
// parallel worktrees (sandboxed in Docker), lets you curate the result next to the diff, chat with the
// review session, and posts one review to GitHub when you click Post.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/xjin-archera/prb/internal/config"
	"github.com/xjin-archera/prb/internal/runner"
	"github.com/xjin-archera/prb/internal/store"
	"github.com/xjin-archera/prb/internal/web"
)

//go:embed Dockerfile.runner
var dockerfile string

var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "setup":
		err = setup()
	case "build-image":
		err = buildImage()
	case "version":
		fmt.Println("prb", version)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`prb — PR review board (local web app)

  prb              start the web app (same as: prb serve)
  prb setup        check gh, git, claude, docker; store the sandbox token
  prb build-image  build the Docker sandbox image
  prb version

Config: ~/.pr-review-board/config.json (override the dir with PRB_STATE_DIR)
`)
}

func serve() error {
	paths := config.DefaultPaths()
	cfg, err := config.Load(paths)
	if err != nil {
		return err
	}
	st, err := store.Open(paths.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.FailOrphans(context.Background()); err != nil {
		return err
	}
	jobs := runner.New(cfg, paths, st)
	srv, err := web.New(cfg, paths, st, jobs)
	if err != nil {
		return err
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	hs := &http.Server{Addr: addr, Handler: srv.Handler()}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Println("shutting down…")
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		jobs.Shutdown(sctx)
		_ = hs.Shutdown(sctx)
	}()
	log.Printf("prb %s listening on http://%s (runner=%s, %d repos)", version, addr, cfg.Runner, len(cfg.Repos))
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func setup() error {
	paths := config.DefaultPaths()
	cfg, err := config.Load(paths)
	if err != nil {
		return err
	}
	fmt.Println("config:", paths.Config)
	ok := true
	for _, tool := range []string{"gh", "git", "claude"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Printf("  ✗ %s not found on PATH\n", tool)
			ok = false
		} else {
			fmt.Printf("  ✓ %s\n", tool)
		}
	}
	if out, err := exec.Command("gh", "auth", "status").CombinedOutput(); err != nil {
		fmt.Printf("  ✗ gh is not logged in: %s\n", strings.TrimSpace(string(out)))
		ok = false
	} else {
		fmt.Println("  ✓ gh logged in")
	}
	fmt.Printf("  repos: %d discovered under %v\n", len(cfg.Repos), cfg.ScanDirs)
	if cfg.Runner == "docker" {
		if _, err := exec.LookPath("docker"); err != nil {
			fmt.Println("  ✗ docker not found (set \"runner\": \"host\" to run without the sandbox)")
			ok = false
		} else if err := exec.Command("docker", "image", "inspect", cfg.DockerImage).Run(); err != nil {
			fmt.Printf("  ✗ image %s missing — run: prb build-image\n", cfg.DockerImage)
		} else {
			fmt.Printf("  ✓ docker image %s\n", cfg.DockerImage)
		}
		fmt.Println()
		fmt.Println("The sandbox needs a long-lived Claude Code token (your Keychain login is not visible in the container).")
		fmt.Println("Run `claude setup-token`, then paste the token here (leave empty to skip):")
		fmt.Print("token> ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		tok := strings.Join(strings.Fields(line), "") // a pasted token can carry a wrapped line break
		if tok != "" {
			if err := runner.CheckToken(tok); err != nil {
				return fmt.Errorf("token not stored: %w", err)
			}
			if runtime.GOOS == "darwin" {
				_ = exec.Command("security", "delete-generic-password", "-s", "pr-review-board", "-a", "oauth").Run()
				if err := exec.Command("security", "add-generic-password", "-s", "pr-review-board", "-a", "oauth", "-w", tok).Run(); err != nil {
					return fmt.Errorf("store token in Keychain: %w", err)
				}
				fmt.Println("  ✓ token stored in the macOS Keychain (pr-review-board/oauth)")
			} else {
				cfg.ClaudeOAuthToken = tok
				if err := config.Save(paths, cfg); err != nil {
					return err
				}
				fmt.Println("  ✓ token stored in config.json (claude_oauth_token); or export CLAUDE_CODE_OAUTH_TOKEN instead")
			}
		}
	}
	if !ok {
		return errors.New("some checks failed")
	}
	fmt.Println("\nready: run `prb` and open http://" + fmt.Sprintf("%s:%d", cfg.Host, cfg.Port))
	return nil
}

func buildImage() error {
	paths := config.DefaultPaths()
	cfg, err := config.Load(paths)
	if err != nil {
		return err
	}
	cmd := exec.Command("docker", "build", "-t", cfg.DockerImage, "-f", "-", ".")
	cmd.Stdin = strings.NewReader(dockerfile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
