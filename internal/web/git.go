package web

import (
	"context"

	"github.com/xifengjin/prb/internal/worktree"
)

func removeWorktree(ctx context.Context, repoPath, wt string) error {
	return worktree.Remove(ctx, repoPath, wt)
}
func dropPRRef(ctx context.Context, repoPath string, n int) { worktree.DropPRRef(ctx, repoPath, n) }
