package strategy

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func newShadowOnlyCommit(t *testing.T, env *shadowCleanupEnv, shadow string) plumbing.Hash {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "mktree")
	output, treeErr := cmd.CombinedOutput()
	require.NoError(t, treeErr, string(output))
	hash, err := checkpoint.CreateCommit(t.Context(), env.repo, plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904"), env.baseHash, "shadow-only history", "test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, env.repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(shadow), hash)))
	return hash
}

func requireShadowReachable(t *testing.T, hash plumbing.Hash) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "fsck", "--unreachable", "--no-reflogs")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	require.NotContains(t, string(output), "unreachable commit "+hash.String())
	t.Log("shadow commit remains reachable:", hash.String())
}

func TestCleanupPushedShadowBranches_PreservesExpiredUncondensed(t *testing.T) {
	for _, phase := range []session.Phase{session.PhaseActive, session.PhaseEnded} {
		t.Run(string(phase), func(t *testing.T) {
			env := newShadowCleanupEnv(t)
			ctx := t.Context()
			shadow := env.addShadowBranch(env.baseHash.String(), "")
			hash := newShadowOnlyCommit(t, env, shadow)
			recent := time.Now().Add(-time.Hour)
			state := &SessionState{SessionID: "pending-session", BaseCommit: env.baseHash.String(), StartedAt: recent, LastInteractionTime: &recent, Phase: phase, StepCount: 1, FullyCondensed: false}
			if phase == session.PhaseEnded {
				state.EndedAt = &recent
			}
			require.NoError(t, SaveSessionState(ctx, state))
			deleted, err := CleanupPushedShadowBranches(ctx)
			require.NoError(t, err)
			require.Zero(t, deleted)
			require.True(t, env.branchExists(shadow))
			old := time.Now().Add(-8 * 24 * time.Hour)
			state.StartedAt = old
			state.LastInteractionTime = &old
			if phase == session.PhaseEnded {
				state.EndedAt = &old
			}
			require.NoError(t, SaveSessionState(ctx, state))
			deleted, err = CleanupPushedShadowBranches(ctx)
			require.NoError(t, err)
			require.Zero(t, deleted)
			require.True(t, env.branchExists(shadow))
			_, err = os.Stat(filepath.Join(env.dir, ".git", "entire-sessions", state.SessionID+".json"))
			require.NoError(t, err)
			t.Logf("phase=%s control_deleted=0 aged_deleted=%d state_preserved=true", phase, deleted)
			requireShadowReachable(t, hash)
		})
	}
}

func TestResetSession_PreservesCorruptSiblingShadow(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		name := "valid"
		if corrupt {
			name = "corrupt"
		}
		t.Run(name, func(t *testing.T) {
			env := newShadowCleanupEnv(t)
			ctx := t.Context()
			shadow := env.addShadowBranch(env.baseHash.String(), "")
			hash := newShadowOnlyCommit(t, env, shadow)
			for _, id := range []string{"reset-me", "keep-me"} {
				require.NoError(t, SaveSessionState(ctx, &SessionState{SessionID: id, BaseCommit: env.baseHash.String(), StartedAt: time.Now(), Phase: session.PhaseActive, StepCount: 1}))
			}
			sibling := filepath.Join(env.dir, ".git", "entire-sessions", "keep-me.json")
			if corrupt {
				require.NoError(t, os.WriteFile(sibling, []byte(`{"session_id":`), 0600))
			}
			var output strings.Builder
			resetErr := NewManualCommitStrategy().ResetSession(ctx, &output, io.Discard, "reset-me")
			if corrupt {
				require.Error(t, resetErr)
			} else {
				require.NoError(t, resetErr)
			}
			require.True(t, env.branchExists(shadow))
			_, err := os.Stat(sibling)
			require.NoError(t, err)
			t.Logf("corrupt=%v shared_branch_exists=%v sibling_state_exists=true output=%q", corrupt, env.branchExists(shadow), output.String())
			requireShadowReachable(t, hash)
			if corrupt {
				require.NoError(t, SaveSessionState(ctx, &SessionState{SessionID: "keep-me", BaseCommit: env.baseHash.String(), StartedAt: time.Now(), Phase: session.PhaseActive, StepCount: 1}))
				require.NoError(t, NewManualCommitStrategy().ResetSession(ctx, &output, io.Discard, "keep-me"))
				require.False(t, env.branchExists(shadow))
			}
		})
	}
}

func TestListAllSessionStates_PreservesUnreadableShadow(t *testing.T) {
	env := newShadowCleanupEnv(t)
	ctx := t.Context()
	shadow := env.addShadowBranch(env.baseHash.String(), "")
	hash := newShadowOnlyCommit(t, env, shadow)
	state := &SessionState{SessionID: "idle-pending", BaseCommit: env.baseHash.String(), StartedAt: time.Now(), Phase: session.PhaseIdle, StepCount: 1}
	require.NoError(t, SaveSessionState(ctx, state))
	strat := NewManualCommitStrategy()
	states, err := strat.listAllSessionStates(ctx)
	require.NoError(t, err)
	require.Len(t, states, 1)
	refPath := filepath.Join(env.dir, ".git", "refs", "heads", shadow)
	require.NoError(t, os.Chmod(refPath, 0000))
	t.Cleanup(func() { require.NoError(t, os.Chmod(refPath, 0o600)) })
	_, rawErr := os.ReadFile(refPath)
	if !os.IsPermission(rawErr) {
		t.Skip("filesystem does not enforce unreadable file permissions")
	}
	_, readErr := env.repo.Reference(plumbing.NewBranchReferenceName(shadow), true)
	require.Error(t, readErr)
	require.ErrorIs(t, readErr, plumbing.ErrReferenceNotFound)
	states, err = strat.listAllSessionStates(ctx)
	require.Error(t, err)
	require.Nil(t, states)
	_, statErr := os.Stat(filepath.Join(env.dir, ".git", "entire-sessions", state.SessionID+".json"))
	require.NoError(t, statErr)
	t.Logf("ref_error=%v list_error=%v state_preserved=true", readErr, err)
	require.NoError(t, os.Chmod(refPath, 0600))
	deleted, err := CleanupPushedShadowBranches(ctx)
	require.NoError(t, err)
	require.Zero(t, deleted)
	requireShadowReachable(t, hash)
}

func TestCleanupPushedShadowBranches_PreservesCorruptState(t *testing.T) {
	env := newShadowCleanupEnv(t)
	ctx := t.Context()
	shadow := env.addShadowBranch(env.baseHash.String(), "")
	hash := newShadowOnlyCommit(t, env, shadow)
	env.addSessionState("pending", env.baseHash.String(), "", nil, nil, false)
	path := filepath.Join(env.dir, ".git", "entire-sessions", "pending.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"session_id":`), 0o600))
	deleted, err := CleanupPushedShadowBranches(ctx)
	require.Error(t, err)
	require.Zero(t, deleted)
	require.True(t, env.branchExists(shadow))
	requireShadowReachable(t, hash)
	env.addSessionState("pending", env.baseHash.String(), "", nil, nil, false)
	deleted, err = CleanupPushedShadowBranches(ctx)
	require.NoError(t, err)
	require.Zero(t, deleted)
}
