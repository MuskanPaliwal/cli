package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/antigravity" // registers the late-transcript agent under test
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/stretchr/testify/require"
)

// TestSessionLacksCondensableContent_LateTranscriptWriterPathShapes pins how
// the prepare-commit-msg fast path reads a late-transcript agent's transcript
// path. Only a non-empty REGULAR file counts as content: a directory at the
// path has a Size() too (its entry table), and a symlink is refused rather
// than followed, and neither can be condensed, so both must read as "nothing
// to condense" — otherwise the trailer is stamped and the checkpoint it names
// is never written.
func TestSessionLacksCondensableContent_LateTranscriptWriterPathShapes(t *testing.T) {
	t.Parallel()

	newState := func(transcriptPath string) *SessionState {
		return &SessionState{
			SessionID:      "agy-conv",
			AgentType:      agent.AgentTypeAntigravity,
			Phase:          session.PhaseActive,
			TranscriptPath: transcriptPath,
		}
	}

	t.Run("missing file lacks content", func(t *testing.T) {
		t.Parallel()
		require.True(t, sessionLacksCondensableContent(newState(filepath.Join(t.TempDir(), "absent.jsonl"))))
	})

	t.Run("empty file lacks content", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "empty.jsonl")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.True(t, sessionLacksCondensableContent(newState(path)))
	})

	t.Run("non-empty regular file has content", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
		require.NoError(t, os.WriteFile(path, []byte(`{"step_index":0}`+"\n"), 0o600))
		require.False(t, sessionLacksCondensableContent(newState(path)))
	})

	t.Run("directory at the path lacks content", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "transcript_full.jsonl")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "child"), 0o750))
		info, err := os.Lstat(dir)
		require.NoError(t, err)
		require.True(t, info.IsDir())
		require.True(t, sessionLacksCondensableContent(newState(dir)),
			"a directory is not a transcript, whatever Size() reports for it")
	})

	t.Run("symlinked transcript lacks content", func(t *testing.T) {
		t.Parallel()
		target := filepath.Join(t.TempDir(), "real.jsonl")
		require.NoError(t, os.WriteFile(target, []byte(`{"step_index":0}`+"\n"), 0o600))
		link := filepath.Join(t.TempDir(), "transcript_full.jsonl")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported: %v", err)
		}
		require.True(t, sessionLacksCondensableContent(newState(link)),
			"a symlink is refused, not followed to its target's size")
	})
}

// TestFinalizeAllTurnCheckpoints_LateTranscriptNotFlushedDefers pins the
// late-transcript deferral: agy writes its transcript AFTER the Stop hook, so
// the file HandleTurnEnd finds is often still the empty placeholder
// PrepareTranscript materialised. That is a transient state, not a lost
// transcript, and the mid-turn checkpoints' IDs must survive it so a later
// HandleTurnEnd finalizes them with the flushed content — the same deferral
// the degraded-scanner path uses. Nilling them abandoned the backfill for the
// whole turn on the first Stop that beat the flush.
func TestFinalizeAllTurnCheckpoints_LateTranscriptNotFlushedDefers(t *testing.T) {
	t.Parallel()

	placeholder := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	require.NoError(t, os.WriteFile(placeholder, nil, 0o600))

	state := &SessionState{
		SessionID:         "agy-mid-turn",
		AgentType:         agent.AgentTypeAntigravity,
		Phase:             session.PhaseActive,
		TranscriptPath:    placeholder,
		TurnCheckpointIDs: []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"},
	}

	errCount := NewManualCommitStrategy().finalizeAllTurnCheckpoints(context.Background(), state)
	require.Equal(t, 1, errCount, "a deferred finalize is still reported to the best-effort caller")
	require.Equal(t, []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"}, state.TurnCheckpointIDs,
		"TurnCheckpointIDs must survive an unflushed late transcript so a later turn end retries")
}

// A non-late agent's empty transcript is still the terminal condition it was:
// there is no later flush to wait for, so the IDs are cleared as before.
func TestFinalizeAllTurnCheckpoints_EmptyTranscriptClearsForOtherAgents(t *testing.T) {
	t.Parallel()

	empty := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))

	state := &SessionState{
		SessionID:         "claude-mid-turn",
		AgentType:         agent.AgentTypeClaudeCode,
		Phase:             session.PhaseActive,
		TranscriptPath:    empty,
		TurnCheckpointIDs: []string{"01ARZ3NDEKTSV4RRFFQ69G5FAV"},
	}

	errCount := NewManualCommitStrategy().finalizeAllTurnCheckpoints(context.Background(), state)
	require.Equal(t, 1, errCount)
	require.Empty(t, state.TurnCheckpointIDs)
}
