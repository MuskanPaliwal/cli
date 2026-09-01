package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestCompareAndSwapRef_ReftableLockContention(t *testing.T) {
	t.Parallel()
	repoDir, initial := initReftableRepo(t, "initial.txt", "initial\n")
	newHash := reftableCommit(t, repoDir, "next.txt", "next\n")
	refName := plumbing.ReferenceName("refs/entire/reftable-lock")
	gitenv.Run(t, repoDir, "update-ref", refName.String(), initial)
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".git", "reftable", "tables.list.lock"), nil, 0o600))

	err := CompareAndSwapRef(
		context.Background(),
		repoDir,
		refName,
		plumbing.NewHash(newHash),
		plumbing.NewHash(initial),
	)

	require.ErrorIs(t, err, ErrRefLocked)
	require.Equal(t, initial, strings.TrimSpace(gitenv.Run(t, repoDir, "rev-parse", refName.String())))
}

func TestRefCASErrorClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		stderr       string
		wantConflict bool
		wantLocked   bool
	}{
		{
			name:         "expected value mismatch",
			stderr:       "cannot lock ref 'refs/heads/main': is at aaaa but expected bbbb",
			wantConflict: true,
		},
		{
			name:         "expected ref was deleted",
			stderr:       "cannot lock ref 'refs/heads/main': unable to resolve reference 'refs/heads/main'",
			wantConflict: true,
		},
		{
			name:         "create if absent conflict",
			stderr:       "cannot lock ref 'refs/heads/main': reference already exists",
			wantConflict: true,
		},
		{
			name:       "lock held by another process",
			stderr:     "unable to create '/repo/.git/refs/heads/main.lock': File exists.",
			wantLocked: true,
		},
		{
			name:       "reftable lock held by another process",
			stderr:     "fatal: update_ref failed for ref 'refs/heads/main': cannot lock references",
			wantLocked: true,
		},
		{
			name:   "permission failure",
			stderr: "cannot lock ref 'refs/heads/main': unable to create '/repo/.git/refs/heads/main.lock': Permission denied",
		},
		{
			name:   "namespace conflict",
			stderr: "cannot lock ref 'refs/heads/main/child': 'refs/heads/main' exists; cannot create 'refs/heads/main/child'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			stderr := []byte(tt.stderr)
			require.Equal(t, tt.wantConflict, isRefCASConflict(stderr))
			require.Equal(t, tt.wantLocked, isRefLockContention(stderr))
		})
	}
}
