package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func TestFinalizeAllTurnCheckpointsStopsAtTotalBudget(t *testing.T) {
	workDir := setupGitRepo(t)
	t.Chdir(workDir)
	paths.ClearWorktreeRootCache()

	require.NoError(t, os.MkdirAll(filepath.Join(workDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, ".entire", "settings.json"),
		[]byte(`{"enabled":true,"checkpoints":{"primary":{"type":"git-refs"}}}`),
		0o644,
	))

	repo, err := git.PlainOpen(workDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repo.Close()) })

	sessionID := "slow-finalize"
	checkpointIDs := []id.CheckpointID{
		id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAV"),
		id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAW"),
		id.MustCheckpointID("01ARZ3NDEKTSV4RRFFQ69G5FAX"),
	}
	stores, err := checkpoint.Open(context.Background(), repo, checkpoint.OpenOptions{})
	require.NoError(t, err)
	refHashes := make(map[plumbing.ReferenceName]plumbing.Hash, len(checkpointIDs))
	for _, checkpointID := range checkpointIDs {
		require.NoError(t, stores.Persistent.Write(context.Background(), checkpoint.Session{
			CheckpointID: checkpointID,
			SessionID:    sessionID,
			Strategy:     StrategyNameManualCommit,
			Transcript:   redact.AlreadyRedacted([]byte("provisional transcript\n")),
			AuthorName:   "Test",
			AuthorEmail:  "test@example.com",
			Agent:        "Claude Code",
		}))
		refName, refErr := checkpoint.RefName(checkpointID)
		require.NoError(t, refErr)
		ref, refErr := repo.Reference(refName, true)
		require.NoError(t, refErr)
		refHashes[refName] = ref.Hash()
		require.NoError(t, repo.Storer.RemoveReference(refName))
	}

	transcriptPath := filepath.Join(workDir, ".entire", "metadata", sessionID, paths.TranscriptFileName)
	require.NoError(t, os.MkdirAll(filepath.Dir(transcriptPath), 0o755))
	require.NoError(t, os.WriteFile(transcriptPath, []byte(testTranscriptPromptResponse), 0o644))
	state := &SessionState{
		SessionID:         sessionID,
		AgentType:         "Claude Code",
		TranscriptPath:    transcriptPath,
		TurnCheckpointIDs: []string{checkpointIDs[0].String(), checkpointIDs[1].String(), checkpointIDs[2].String()},
	}

	s := NewManualCommitStrategy()
	s.turnCheckpointFinalizeBudget = 25 * time.Millisecond
	fetchCalls := 0
	s.checkpointRefFetcher = func(ctx context.Context, refName plumbing.ReferenceName) error {
		fetchCalls++
		timer := time.NewTimer(15 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return repo.Storer.SetReference(plumbing.NewHashReference(refName, refHashes[refName]))
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	started := time.Now()
	errCount := s.finalizeAllTurnCheckpoints(context.Background(), state)
	elapsed := time.Since(started)

	finalized := 0
	for _, checkpointID := range checkpointIDs {
		refName, refErr := checkpoint.RefName(checkpointID)
		require.NoError(t, refErr)
		if _, refErr = repo.Reference(refName, true); refErr == nil {
			finalized++
		}
	}

	require.Less(t, elapsed, 40*time.Millisecond)
	require.Less(t, finalized, len(checkpointIDs), "the pass must stop before every slow fetch completes")
	require.Equal(t, len(checkpointIDs)-finalized, errCount, "every provisional checkpoint must count as an error")
	require.LessOrEqual(t, fetchCalls, 2, "no checkpoint fetch may start after the total budget expires")
	require.Empty(t, state.TurnCheckpointIDs, "a best-effort pass has no durable retry path")
	require.NotNil(t, state.CaptureDegradedAt, "the incomplete finalize must be visible through `entire status`")
}
