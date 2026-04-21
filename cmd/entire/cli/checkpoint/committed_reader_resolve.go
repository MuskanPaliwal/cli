package checkpoint

import (
	"context"
	"errors"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// CommittedReader provides read access to committed checkpoint data.
// Both GitStore (v1) and V2GitStore (v2) implement this interface.
type CommittedReader interface {
	ReadCommitted(ctx context.Context, checkpointID id.CheckpointID) (*CheckpointSummary, error)
	ReadSessionContent(ctx context.Context, checkpointID id.CheckpointID, sessionIndex int) (*SessionContent, error)
}

// ResolveCommittedReaderForCheckpoint resolves which committed checkpoint reader
// should be used for a specific checkpoint ID.
//
// Fallback behavior:
//   - Try v2 first when preferCheckpointsV2 is true
//   - Fall back to v1 for any v2 failure except context cancellation
//   - During the v2 migration period, a valid v1 copy should never be blocked
//     by a corrupt or unreadable v2 copy
func ResolveCommittedReaderForCheckpoint(
	ctx context.Context,
	checkpointID id.CheckpointID,
	v1Store *GitStore,
	v2Store *V2GitStore,
	preferCheckpointsV2 bool,
) (CommittedReader, *CheckpointSummary, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err //nolint:wrapcheck // Propagating context cancellation
	}

	if preferCheckpointsV2 && v2Store != nil {
		summary, err := v2Store.ReadCommitted(ctx, checkpointID)
		if err == nil && summary != nil {
			return v2Store, summary, nil
		}
		if err != nil && ctx.Err() != nil {
			return nil, nil, ctx.Err() //nolint:wrapcheck // Propagating context cancellation
		}
		if err != nil && !errors.Is(err, ErrCheckpointNotFound) && !errors.Is(err, ErrNoTranscript) {
			logging.Debug(ctx, "v2 ReadCommitted failed, falling back to v1",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	if v1Store == nil {
		return nil, nil, ErrCheckpointNotFound
	}

	summary, err := v1Store.ReadCommitted(ctx, checkpointID)
	if err != nil {
		return nil, nil, err
	}
	if summary == nil {
		return nil, nil, ErrCheckpointNotFound
	}

	return v1Store, summary, nil
}

// ResolveRawSessionLogForCheckpoint resolves the raw transcript log bytes for a
// checkpoint with v2-first, v1-fallback behavior.
//
// Fallback behavior:
//   - Try v2 first when preferCheckpointsV2 is true
//   - Fall back to v1 for any v2 failure except context cancellation
func ResolveRawSessionLogForCheckpoint(
	ctx context.Context,
	checkpointID id.CheckpointID,
	v1Store *GitStore,
	v2Store *V2GitStore,
	preferCheckpointsV2 bool,
) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err //nolint:wrapcheck // Propagating context cancellation
	}

	if preferCheckpointsV2 && v2Store != nil {
		content, sessionID, err := v2Store.GetSessionLog(ctx, checkpointID)
		if err == nil && len(content) > 0 {
			return content, sessionID, nil
		}
		if err != nil && ctx.Err() != nil {
			return nil, "", ctx.Err() //nolint:wrapcheck // Propagating context cancellation
		}
		if err != nil && !errors.Is(err, ErrCheckpointNotFound) && !errors.Is(err, ErrNoTranscript) {
			logging.Debug(ctx, "v2 GetSessionLog failed, falling back to v1",
				slog.String("checkpoint_id", checkpointID.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	if v1Store == nil {
		return nil, "", ErrCheckpointNotFound
	}

	return v1Store.GetSessionLog(ctx, checkpointID)
}
