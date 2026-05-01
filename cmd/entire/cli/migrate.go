package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/spf13/cobra"
)

func newMigrateCmd() *cobra.Command {
	var checkpointsFlag string
	var forceFlag bool

	cmd := &cobra.Command{
		Use:    "migrate",
		Short:  "Migrate Entire data to newer formats",
		Long:   `Migrate Entire data to newer formats. Currently supports migrating v1 checkpoints to v2.`,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if checkpointsFlag == "" {
				return cmd.Help()
			}
			if checkpointsFlag != "v2" {
				return fmt.Errorf("unsupported checkpoints version: %q (only \"v2\" is supported)", checkpointsFlag)
			}

			ctx := cmd.Context()

			if _, err := paths.WorktreeRoot(ctx); err != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
				return NewSilentError(errors.New("not a git repository"))
			}

			logging.SetLogLevelGetter(GetLogLevel)
			if initErr := logging.Init(ctx, ""); initErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: could not initialize logging: %v\n", initErr)
			} else {
				defer logging.Close()
			}
			return runMigrateCheckpointsV2(ctx, cmd, forceFlag)
		},
	}

	cmd.Flags().StringVar(&checkpointsFlag, "checkpoints", "", "Target checkpoint format version (e.g., \"v2\")")
	cmd.Flags().BoolVar(&forceFlag, "force", false, "Force re-migration of all checkpoints, overwriting existing v2 data")

	return cmd
}

type migrateResult struct {
	total                        int
	migrated                     int
	skipped                      int
	failed                       int
	missingSessions              int
	compactTranscriptSkipped     int
	backfilledCompactTranscripts int
	repaired                     int
}

func runMigrateCheckpointsV2(ctx context.Context, cmd *cobra.Command, force bool) error {
	repo, err := strategy.OpenRepository(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Please run from within a git repository.")
		return NewSilentError(err)
	}

	v1Store := checkpoint.NewGitStore(repo)
	v2Store := checkpoint.NewV2GitStore(repo, migrateRemoteName)
	out := cmd.OutOrStdout()
	progressOut := cmd.ErrOrStderr()

	result, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, progressOut, force)
	if err != nil {
		return err
	}

	printMigrateCompletion(out, result)
	fmt.Fprintln(out, "Note: V2 checkpoints are stored as custom refs under refs/entire/checkpoints/v2/*, not as a branch visible in the GitHub UI.")
	fmt.Fprintf(out, "To inspect pushed v2 checkpoint refs locally, run: git ls-remote %s \"refs/entire/checkpoints/v2/*\"\n", migrateRemoteName)
	fmt.Fprintln(out, `You may also open a checkpoint's details in the Entire web app and click the "session logs" link to view the log files and metadata.`)

	if result.failed > 0 {
		return NewSilentError(fmt.Errorf("%d checkpoint(s) failed to migrate", result.failed))
	}

	return nil
}

const migrationLogFile = logging.LogsDir + "/entire.log"

func printMigrateCompletion(out io.Writer, result *migrateResult) {
	if result.total == 0 {
		fmt.Fprintln(out, "Nothing to migrate: no v1 checkpoints found")
		fmt.Fprintln(out)
		return
	}

	fmt.Fprintf(out, "Migration complete: %d migrated, %d skipped, %d failed\n",
		result.migrated, result.skipped, result.failed)

	if result.hasLoggedDetails() {
		fmt.Fprintf(out, "Details for skipped, missing, incomplete, or failed checkpoints were logged to %s.\n", migrationLogFile)
	}

	fmt.Fprintln(out)
}

func (r *migrateResult) hasLoggedDetails() bool {
	return r.skipped > 0 || r.failed > 0 || r.missingSessions > 0 || r.compactTranscriptSkipped > 0
}

var (
	errAlreadyMigrated          = errors.New("already migrated")
	errTranscriptNotGeneratable = errors.New("transcript.jsonl could not be generated")
	errNoMigratableSessions     = errors.New("no migratable v1 sessions")
)

const migrateRemoteName = "origin"

func migrateCheckpointsV2(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, progressOut io.Writer, force bool) (*migrateResult, error) {
	v1List, err := v1Store.ListCommitted(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list v1 checkpoints: %w", err)
	}

	if len(v1List) == 0 {
		return &migrateResult{}, nil
	}

	total := len(v1List)
	result := &migrateResult{total: total}
	progress := startProgressBar(progressOut, "Migrating checkpoints", total)
	defer progress.Finish()

	for _, info := range v1List {
		outcome, migrateErr := migrateOneCheckpoint(ctx, repo, v1Store, v2Store, info, force)
		result.missingSessions += outcome.missingSessions
		result.backfilledCompactTranscripts += outcome.backfilledCompactTranscripts
		if outcome.compactTranscriptSkipped {
			result.compactTranscriptSkipped++
		}
		if outcome.repaired {
			result.repaired++
		}

		if migrateErr != nil {
			switch {
			case errors.Is(migrateErr, errAlreadyMigrated):
				logCheckpointMigrationSkip(ctx, info.CheckpointID, "already in v2", migrateErr)
				result.skipped++
			case errors.Is(migrateErr, errTranscriptNotGeneratable):
				logCheckpointMigrationSkip(ctx, info.CheckpointID, "transcript.jsonl could not be generated", migrateErr)
				result.skipped++
			case errors.Is(migrateErr, errNoMigratableSessions):
				logCheckpointMigrationSkip(ctx, info.CheckpointID, "no migratable v1 sessions", migrateErr)
				result.skipped++
			default:
				logging.Error(ctx, "checkpoint migration failed",
					slog.String("checkpoint_id", string(info.CheckpointID)),
					slog.String("error", migrateErr.Error()),
				)
				result.failed++
			}
			progress.Increment()
			continue
		}

		result.migrated++
		progress.Increment()
	}

	return result, nil
}

func logCheckpointMigrationSkip(ctx context.Context, checkpointID id.CheckpointID, reason string, err error) {
	logging.Info(ctx, "checkpoint migration skipped",
		slog.String("checkpoint_id", string(checkpointID)),
		slog.String("reason", reason),
		slog.String("error", err.Error()),
	)
}

type migrateCheckpointOutcome struct {
	missingSessions              int
	compactTranscriptSkipped     bool
	backfilledCompactTranscripts int
	repaired                     bool
}

func migrateOneCheckpoint(ctx context.Context, repo *git.Repository, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, force bool) (migrateCheckpointOutcome, error) {
	var outcome migrateCheckpointOutcome

	existing, err := v2Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return outcome, fmt.Errorf("failed to check v2 for checkpoint %s: %w", info.CheckpointID, err)
	}

	// Already in v2 — when not forcing, check if any aspect of sessions are missing and backfill
	if existing != nil && !force {
		repaired, repairErr := repairPartialV2Checkpoint(ctx, v1Store, v2Store, info, existing)
		if repairErr != nil {
			return outcome, repairErr
		}
		outcome.repaired = repaired

		currentV2, readCurrentErr := v2Store.ReadCommitted(ctx, info.CheckpointID)
		if readCurrentErr != nil {
			return outcome, fmt.Errorf("failed to re-read v2 checkpoint %s: %w", info.CheckpointID, readCurrentErr)
		}
		if currentV2 == nil {
			return outcome, fmt.Errorf("v2 checkpoint %s disappeared during migration", info.CheckpointID)
		}

		// Clean up v1-named transcript files (full.jsonl, content_hash.txt) that older
		// CLI versions may have written to /full/current before the rename to raw_transcript.
		cleanupV1TranscriptFiles(ctx, repo, v2Store, info.CheckpointID, len(currentV2.Sessions))

		backfilled, backfillErr := backfillCompactTranscripts(ctx, v1Store, v2Store, info, currentV2)
		outcome.backfilledCompactTranscripts = backfilled
		if errors.Is(backfillErr, errAlreadyMigrated) && repaired {
			return outcome, nil
		}
		if errors.Is(backfillErr, errTranscriptNotGeneratable) && repaired {
			outcome.compactTranscriptSkipped = true
			return outcome, nil
		}
		return outcome, backfillErr
	}

	if existing != nil && force {
		if pruneErr := pruneV2CheckpointForForce(ctx, repo, v2Store, info.CheckpointID); pruneErr != nil {
			return outcome, fmt.Errorf("failed to reset existing v2 checkpoint %s before force migration: %w", info.CheckpointID, pruneErr)
		}
	}

	summary, err := v1Store.ReadCommitted(ctx, info.CheckpointID)
	if err != nil {
		return outcome, fmt.Errorf("failed to read v1 summary: %w", err)
	}
	if summary == nil {
		return outcome, fmt.Errorf("v1 checkpoint %s has no summary", info.CheckpointID)
	}

	compactFailed := false
	shouldCopyTaskMetadata := false
	skippedMissingSessions := 0
	migratedSessions := 0
	v1ToV2SessionIdx := make(map[int]int, len(summary.Sessions))

	for sessionIdx := range len(summary.Sessions) {
		content, skipped, readErr := readV1SessionForMigration(ctx, v1Store, info.CheckpointID, sessionIdx)
		if skipped {
			skippedMissingSessions++
			outcome.missingSessions++
			continue
		}
		if readErr != nil {
			return outcome, fmt.Errorf("failed to read v1 session %d: %w", sessionIdx, readErr)
		}
		if content.Metadata.IsTask {
			shouldCopyTaskMetadata = true
		}

		opts := buildMigrateWriteOpts(content, info, summary.CombinedAttribution)

		compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata)
		if compacted != nil {
			opts.CompactTranscript = compacted
			opts.CompactTranscriptStart = computeCompactOffset(ctx, content.Transcript, compacted, content.Metadata)
		} else if len(content.Transcript) > 0 {
			compactFailed = true
		}

		v2SessionIdx, writeErr := v2Store.WriteCommittedWithSessionIndex(ctx, opts)
		if writeErr != nil {
			return outcome, fmt.Errorf("failed to write v2 session %d: %w", sessionIdx, writeErr)
		}
		v1ToV2SessionIdx[sessionIdx] = v2SessionIdx
		migratedSessions++
	}

	if migratedSessions == 0 {
		return outcome, fmt.Errorf("%w: v1 metadata lists %d session(s), but no transcript/session content exists for any of them", errNoMigratableSessions, len(summary.Sessions))
	}

	// Copy task metadata trees from v1 to v2 /full/current
	if shouldCopyTaskMetadata {
		if taskErr := copyTaskMetadataToV2(ctx, repo, v1Store, v2Store, info.CheckpointID, summary, v1ToV2SessionIdx); taskErr != nil {
			logging.Warn(ctx, "failed to copy task metadata to v2",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.String("error", taskErr.Error()),
			)
		}
	}

	if compactFailed {
		outcome.compactTranscriptSkipped = true
		logging.Warn(ctx, "compact transcript not generated during checkpoint migration",
			slog.String("checkpoint_id", string(info.CheckpointID)),
			slog.Int("migrated_sessions", migratedSessions),
		)
	}
	if skippedMissingSessions > 0 {
		logging.Warn(ctx, "checkpoint migration skipped v1 sessions with missing transcript/session content",
			slog.String("checkpoint_id", string(info.CheckpointID)),
			slog.Int("missing_sessions", skippedMissingSessions),
		)
	}

	return outcome, nil
}

func readV1SessionForMigration(ctx context.Context, v1Store *checkpoint.GitStore, checkpointID id.CheckpointID, sessionIdx int) (*checkpoint.SessionContent, bool, error) {
	content, readErr := v1Store.ReadSessionContent(ctx, checkpointID, sessionIdx)
	if readErr != nil {
		if errors.Is(readErr, checkpoint.ErrNoTranscript) || errors.Is(readErr, checkpoint.ErrCheckpointNotFound) {
			warnMissingV1Session(ctx, checkpointID, sessionIdx, readErr)
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("read v1 session content: %w", readErr)
	}
	return content, false, nil
}

func warnMissingV1Session(ctx context.Context, checkpointID id.CheckpointID, sessionIdx int, err error) {
	logging.Warn(ctx, "skipping v1 session with missing transcript during checkpoint migration",
		slog.String("checkpoint_id", checkpointID.String()),
		slog.Int("session_index", sessionIdx),
		slog.String("error", err.Error()),
	)
}

func pruneV2CheckpointForForce(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID) error {
	for _, refName := range []plumbing.ReferenceName{
		plumbing.ReferenceName(paths.V2MainRefName),
		plumbing.ReferenceName(paths.V2FullCurrentRefName),
	} {
		if err := pruneV2CheckpointRef(ctx, repo, v2Store, refName, cpID); err != nil {
			return err
		}
	}
	return nil
}

func pruneV2CheckpointRef(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, refName plumbing.ReferenceName, cpID id.CheckpointID) error {
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			return nil
		}
		return fmt.Errorf("failed to get v2 ref state for %s: %w", refName, err)
	}

	rootTree, err := repo.TreeObject(rootTreeHash)
	if err != nil {
		return fmt.Errorf("failed to read v2 tree for %s: %w", refName, err)
	}
	if _, err := rootTree.Tree(cpID.Path()); err != nil {
		return nil //nolint:nilerr // Checkpoint is absent from this ref, so there is nothing to prune.
	}

	shardPrefix := string(cpID[:2])
	shardSuffix := string(cpID[2:])
	newRoot, err := pruneCheckpointFromRoot(repo, rootTreeHash, shardPrefix, shardSuffix)
	if err != nil {
		return fmt.Errorf("failed to remove checkpoint subtree from %s: %w", refName, err)
	}
	if newRoot == rootTreeHash {
		return nil
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, newRoot, parentHash,
		fmt.Sprintf("Reset checkpoint before force migration: %s\n", cpID),
		"Entire Migration", "migration@entire.dev")
	if err != nil {
		return fmt.Errorf("failed to create v2 prune commit for %s: %w", refName, err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("failed to update ref %s: %w", refName, err)
	}
	return nil
}

func pruneCheckpointFromRoot(repo *git.Repository, rootTreeHash plumbing.Hash, shardPrefix, shardSuffix string) (plumbing.Hash, error) {
	newRoot, err := checkpoint.UpdateSubtree(repo, rootTreeHash,
		[]string{shardPrefix},
		nil,
		checkpoint.UpdateSubtreeOptions{
			MergeMode:   checkpoint.MergeKeepExisting,
			DeleteNames: []string{shardSuffix},
		},
	)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to prune checkpoint from shard: %w", err)
	}
	if newRoot == rootTreeHash {
		return newRoot, nil
	}

	newRootTree, err := repo.TreeObject(newRoot)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to read pruned root tree: %w", err)
	}
	shardTree, err := newRootTree.Tree(shardPrefix)
	if err != nil {
		return newRoot, nil //nolint:nilerr // The shard prefix was already absent after pruning.
	}
	if len(shardTree.Entries) > 0 {
		return newRoot, nil
	}

	prunedRoot, err := checkpoint.UpdateSubtree(repo, rootTreeHash,
		nil,
		nil,
		checkpoint.UpdateSubtreeOptions{
			MergeMode:   checkpoint.MergeKeepExisting,
			DeleteNames: []string{shardPrefix},
		},
	)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("failed to prune empty shard prefix: %w", err)
	}
	return prunedRoot, nil
}

func repairPartialV2Checkpoint(ctx context.Context, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, v2Summary *checkpoint.CheckpointSummary) (bool, error) {
	repaired := false

	// Spot-check already present sessions: ensure required /full/* artifacts exist.
	existingSessionCount := len(v2Summary.Sessions)
	for sessionIdx := range existingSessionCount {
		ok, checkErr := hasFullSessionArtifacts(v2Store, info.CheckpointID, sessionIdx)
		if checkErr != nil {
			return false, fmt.Errorf("failed to check v2 session %d artifacts: %w", sessionIdx, checkErr)
		}
		if ok {
			continue
		}

		content, readErr := v1Store.ReadSessionContent(ctx, info.CheckpointID, sessionIdx)
		if readErr != nil {
			return false, fmt.Errorf("failed to read v1 session %d while repairing v2: %w", sessionIdx, readErr)
		}

		updateOpts := checkpoint.UpdateCommittedOptions{
			CheckpointID: info.CheckpointID,
			SessionID:    content.Metadata.SessionID,
			// content.Transcript was read from v1 checkpoint storage and is
			// already redacted at write time.
			Transcript: redact.AlreadyRedacted(content.Transcript),
			Prompts:    checkpoint.SplitPromptContent(content.Prompts),
			Agent:      content.Metadata.Agent,
		}
		if compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata); compacted != nil {
			updateOpts.CompactTranscript = compacted
		}

		if updateErr := v2Store.UpdateCommitted(ctx, updateOpts); updateErr != nil {
			return false, fmt.Errorf("failed to repair v2 session %d: %w", sessionIdx, updateErr)
		}
		repaired = true
	}

	return repaired, nil
}

func hasFullSessionArtifacts(v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int) (bool, error) {
	ok, err := v2Store.HasFullSessionArtifacts(cpID, sessionIdx)
	if err != nil {
		return false, fmt.Errorf("failed to check v2 full artifacts for session %d: %w", sessionIdx, err)
	}
	return ok, nil
}

// backfillCompactTranscripts checks sessions in an already-migrated v2 checkpoint
// for missing transcript.jsonl and attempts to generate + write them from v1 data.
// Returns errAlreadyMigrated if all sessions already have compact transcripts.
func backfillCompactTranscripts(ctx context.Context, v1Store *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, info checkpoint.CommittedInfo, v2Summary *checkpoint.CheckpointSummary) (int, error) {
	// Find sessions missing transcript.jsonl
	var needsBackfill []int
	for i, session := range v2Summary.Sessions {
		if session.Transcript == "" {
			needsBackfill = append(needsBackfill, i)
		}
	}

	if len(needsBackfill) == 0 {
		return 0, errAlreadyMigrated
	}

	backfilled := 0
	var lastAgent string

	for _, sessionIdx := range needsBackfill {
		content, readErr := v1Store.ReadSessionContent(ctx, info.CheckpointID, sessionIdx)
		if readErr != nil {
			logging.Warn(ctx, "transcript.jsonl backfill: could not read v1 session",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.Int("session_index", sessionIdx),
				slog.String("error", readErr.Error()),
			)
			continue
		}

		if content.Metadata.Agent != "" {
			lastAgent = string(content.Metadata.Agent)
		}

		compacted := tryCompactTranscript(ctx, content.Transcript, content.Metadata)
		if compacted == nil {
			// tryCompactTranscript already logs for no-agent and compact-error cases;
			// log the empty-transcript case here.
			if len(content.Transcript) == 0 {
				logging.Warn(ctx, "transcript.jsonl backfill: empty transcript in v1",
					slog.String("checkpoint_id", string(info.CheckpointID)),
					slog.Int("session_index", sessionIdx),
				)
			}
			continue
		}

		updateErr := v2Store.UpdateCommitted(ctx, checkpoint.UpdateCommittedOptions{
			CheckpointID:      info.CheckpointID,
			SessionID:         content.Metadata.SessionID,
			CompactTranscript: compacted,
		})
		if updateErr != nil {
			logging.Warn(ctx, "transcript.jsonl backfill: failed to write to v2",
				slog.String("checkpoint_id", string(info.CheckpointID)),
				slog.Int("session_index", sessionIdx),
				slog.String("error", updateErr.Error()),
			)
			continue
		}

		backfilled++
	}

	if backfilled == 0 {
		if lastAgent != "" {
			return 0, fmt.Errorf("%w: agent %q", errTranscriptNotGeneratable, lastAgent)
		}
		return 0, fmt.Errorf("%w: no agent type in metadata", errTranscriptNotGeneratable)
	}

	return backfilled, nil
}

func buildMigrateWriteOpts(content *checkpoint.SessionContent, info checkpoint.CommittedInfo, combinedAttribution *checkpoint.InitialAttribution) checkpoint.WriteCommittedOptions {
	m := content.Metadata

	prompts := checkpoint.SplitPromptContent(content.Prompts)

	return checkpoint.WriteCommittedOptions{
		CheckpointID: info.CheckpointID,
		SessionID:    m.SessionID,
		CreatedAt:    m.CreatedAt,
		Strategy:     m.Strategy,
		Branch:       m.Branch,
		// content.Transcript comes from persisted checkpoint storage and is
		// already redacted.
		Transcript:                  redact.AlreadyRedacted(content.Transcript),
		Prompts:                     prompts,
		FilesTouched:                m.FilesTouched,
		CheckpointsCount:            m.CheckpointsCount,
		Agent:                       m.Agent,
		Model:                       m.Model,
		TurnID:                      m.TurnID,
		TokenUsage:                  m.TokenUsage,
		SessionMetrics:              m.SessionMetrics,
		InitialAttribution:          m.InitialAttribution,
		PromptAttributionsJSON:      m.PromptAttributions,
		CombinedAttribution:         combinedAttribution,
		Summary:                     m.Summary,
		CheckpointTranscriptStart:   m.GetTranscriptStart(),
		TranscriptIdentifierAtStart: m.TranscriptIdentifierAtStart,
		IsTask:                      m.IsTask,
		ToolUseID:                   m.ToolUseID,
		AuthorName:                  "Entire Migration",
		AuthorEmail:                 "migration@entire.dev",
	}
}

func tryCompactTranscript(ctx context.Context, transcript []byte, m checkpoint.CommittedMetadata) []byte {
	return compactTranscriptForStartLine(ctx, transcript, m, 0)
}

func compactTranscriptForStartLine(ctx context.Context, transcript []byte, m checkpoint.CommittedMetadata, startLine int) []byte {
	if len(transcript) == 0 {
		return nil
	}
	if m.Agent == "" {
		logging.Warn(ctx, "compact transcript skipped: no agent type in checkpoint metadata",
			slog.String("checkpoint_id", string(m.CheckpointID)),
		)
		return nil
	}

	// transcript is read from persisted checkpoint storage and already redacted.
	compacted, err := compact.Compact(redact.AlreadyRedacted(transcript), compact.MetadataFields{
		Agent:      string(m.Agent),
		CLIVersion: versioninfo.Version,
		StartLine:  startLine,
	})
	if err != nil {
		logging.Warn(ctx, "compact transcript generation failed during migration",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.String("agent", string(m.Agent)),
			slog.String("error", err.Error()),
		)
		return nil
	}
	if len(compacted) == 0 {
		logging.Warn(ctx, "transcript.jsonl generation produced no output",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.String("agent", string(m.Agent)),
			slog.Int("input_bytes", len(transcript)),
		)
		return nil
	}
	return compacted
}

// computeCompactOffset determines the transcript.jsonl line offset for a checkpoint
// by comparing a full compact (startLine=0) against the scoped compact. The difference
// is the number of compact lines before this checkpoint's data.
func computeCompactOffset(ctx context.Context, fullTranscript, fullCompact []byte, m checkpoint.CommittedMetadata) int {
	startLine := m.GetTranscriptStart()
	if startLine == 0 || len(fullTranscript) == 0 || m.Agent == "" {
		return 0
	}

	if len(fullCompact) == 0 {
		return 0
	}

	// fullTranscript is read from persisted checkpoint storage and already redacted.
	scopedCompact, err := compact.Compact(redact.AlreadyRedacted(fullTranscript), compact.MetadataFields{
		Agent:      string(m.Agent),
		CLIVersion: versioninfo.Version,
		StartLine:  startLine,
	})
	if err != nil {
		logging.Warn(ctx, "compact transcript offset calculation failed during migration",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.String("agent", string(m.Agent)),
			slog.String("error", err.Error()),
		)
		return 0
	}
	if len(scopedCompact) == 0 {
		return 0
	}

	fullLines := bytes.Count(fullCompact, []byte{'\n'})
	scopedLines := bytes.Count(scopedCompact, []byte{'\n'})
	offset := fullLines - scopedLines
	if offset < 0 {
		logging.Warn(ctx, "compact transcript offset was negative during migration, defaulting to 0",
			slog.String("checkpoint_id", string(m.CheckpointID)),
			slog.Int("full_lines", fullLines),
			slog.Int("scoped_lines", scopedLines),
		)
		return 0
	}
	return offset
}

// copyTaskMetadataToV2 copies task metadata files (subagent transcripts, checkpoint JSONs)
// from the v1 branch to the v2 /full/current ref via tree surgery.
func copyTaskMetadataToV2(ctx context.Context, repo *git.Repository, _ *checkpoint.GitStore, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, summary *checkpoint.CheckpointSummary, v1ToV2SessionIdx map[int]int) error {
	// Resolve the v1 branch tree
	v1Tree, err := resolveV1CheckpointTree(repo, cpID)
	if err != nil {
		return err
	}

	// Legacy v1 layout stores task metadata at checkpoint root: <cp>/tasks/<tool-use-id>/...
	// Prefer attaching this tree to the latest session in v2.
	if rootTasksTree, rootTasksErr := v1Tree.Tree("tasks"); rootTasksErr == nil {
		if latestSessionIdx, ok := latestMigratedV2SessionIndex(v1ToV2SessionIdx); ok {
			if spliceErr := spliceTasksTreeToV2(ctx, repo, v2Store, cpID, latestSessionIdx, rootTasksTree.Hash); spliceErr != nil {
				return fmt.Errorf("latest session task tree splice failed: %w", spliceErr)
			}
		}
	}

	for sessionIdx := range len(summary.Sessions) {
		sessionDir := strconv.Itoa(sessionIdx)
		sessionTree, sessionErr := v1Tree.Tree(sessionDir)
		if sessionErr != nil {
			continue
		}

		tasksTree, tasksErr := sessionTree.Tree("tasks")
		if tasksErr != nil {
			continue // No tasks directory in this session
		}

		v2SessionIdx, ok := v1ToV2SessionIdx[sessionIdx]
		if !ok {
			continue
		}

		if spliceErr := spliceTasksTreeToV2(ctx, repo, v2Store, cpID, v2SessionIdx, tasksTree.Hash); spliceErr != nil {
			return fmt.Errorf("session %d task tree splice failed: %w", sessionIdx, spliceErr)
		}
	}

	return nil
}

func latestMigratedV2SessionIndex(v1ToV2SessionIdx map[int]int) (int, bool) {
	latest := -1
	for _, v2SessionIdx := range v1ToV2SessionIdx {
		if v2SessionIdx > latest {
			latest = v2SessionIdx
		}
	}
	if latest < 0 {
		return -1, false
	}
	return latest, true
}

// resolveV1CheckpointTree reads the checkpoint subtree from the v1 branch.
func resolveV1CheckpointTree(repo *git.Repository, cpID id.CheckpointID) (*object.Tree, error) {
	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	if err != nil {
		// Try remote tracking branch
		remoteRefName := plumbing.NewRemoteReferenceName(migrateRemoteName, paths.MetadataBranchName)
		ref, err = repo.Reference(remoteRefName, true)
		if err != nil {
			return nil, fmt.Errorf("v1 branch not found: %w", err)
		}
	}

	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("failed to get v1 commit: %w", err)
	}

	rootTree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("failed to get v1 tree: %w", err)
	}

	cpTree, err := rootTree.Tree(cpID.Path())
	if err != nil {
		return nil, fmt.Errorf("checkpoint %s not found in v1 tree: %w", cpID, err)
	}

	return cpTree, nil
}

// cleanupV1TranscriptFiles removes legacy v1-named transcript files (full.jsonl,
// full.jsonl.*, content_hash.txt) from /full/current. Older CLI versions wrote
// these before the rename to raw_transcript; they are inert but waste space.
// Best-effort: failures are logged and do not block migration.
func cleanupV1TranscriptFiles(ctx context.Context, _ *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionCount int) {
	if err := v2Store.CleanupV1TranscriptFiles(ctx, cpID, sessionCount); err != nil {
		logging.Warn(ctx, "v1 transcript cleanup failed",
			slog.String("checkpoint_id", string(cpID)),
			slog.String("error", err.Error()),
		)
	}
}

func spliceTasksTreeToV2(ctx context.Context, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int, tasksTreeHash plumbing.Hash) error {
	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	if err != nil {
		return fmt.Errorf("failed to get v2 ref state: %w", err)
	}
	incomingTasksTree, err := repo.TreeObject(tasksTreeHash)
	if err != nil {
		return fmt.Errorf("failed to read tasks tree: %w", err)
	}

	shardPrefix := string(cpID[:2])
	shardSuffix := string(cpID[2:])
	sessionDir := strconv.Itoa(sessionIdx)

	newRoot, err := checkpoint.UpdateSubtree(repo, rootTreeHash,
		[]string{shardPrefix, shardSuffix, sessionDir, "tasks"},
		incomingTasksTree.Entries,
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	if err != nil {
		return fmt.Errorf("tree surgery failed: %w", err)
	}

	commitHash, err := checkpoint.CreateCommit(ctx, repo, newRoot, parentHash,
		fmt.Sprintf("Add task metadata for %s\n", cpID),
		"Entire Migration", "migration@entire.dev")
	if err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}

	if err := repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)); err != nil {
		return fmt.Errorf("failed to update ref %s: %w", refName, err)
	}
	return nil
}
