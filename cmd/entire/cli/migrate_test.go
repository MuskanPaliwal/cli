package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/transcript/compact"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initMigrateTestRepo creates a repo with an initial commit.
func initMigrateTestRepo(t *testing.T) *git.Repository {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "init")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "initial")

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	return repo
}

// writeV1Checkpoint writes a checkpoint to the v1 branch for testing.
func writeV1Checkpoint(t *testing.T, store *checkpoint.GitStore, cpID id.CheckpointID, sessionID string, transcript []byte, prompts []string) {
	t.Helper()
	err := store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(transcript),
		Prompts:      prompts,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)
}

func newMigrateStores(repo *git.Repository) (*checkpoint.GitStore, *checkpoint.V2GitStore) {
	return checkpoint.NewGitStore(repo), checkpoint.NewV2GitStore(repo, migrateRemoteName)
}

func buildTasksTreeHash(t *testing.T, repo *git.Repository, toolUseID string) plumbing.Hash {
	t.Helper()

	blobHash, err := checkpoint.CreateBlobFromContent(repo, []byte(`{"tool_use_id":"`+toolUseID+`"}`))
	require.NoError(t, err)

	treeHash, err := checkpoint.BuildTreeFromEntries(context.Background(), repo, map[string]object.TreeEntry{
		toolUseID + "/checkpoint.json": {Mode: filemode.Regular, Hash: blobHash},
	})
	require.NoError(t, err)

	return treeHash
}

func addV1SessionTasksTree(t *testing.T, repo *git.Repository, cpID id.CheckpointID, sessionIdx int, toolUseID string) {
	t.Helper()

	tasksTreeHash := buildTasksTreeHash(t, repo, toolUseID)
	tasksTree, err := repo.TreeObject(tasksTreeHash)
	require.NoError(t, err)

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)

	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	newRoot, err := checkpoint.UpdateSubtree(repo, commit.TreeHash,
		[]string{string(cpID[:2]), string(cpID[2:]), strconv.Itoa(sessionIdx), "tasks"},
		tasksTree.Entries,
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	require.NoError(t, err)

	commitHash, err := checkpoint.CreateCommit(context.Background(), repo, newRoot, ref.Hash(),
		"Add test session task metadata\n",
		"Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func TestMigrateCheckpointsV2_Basic(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("a1b2c3d4e5f6")
	writeV1Checkpoint(t, v1Store, cpID, "session-001",
		[]byte("{\"type\":\"assistant\",\"message\":\"hello\"}\n"),
		[]string{"test prompt"},
	)

	var stdout bytes.Buffer

	result, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)

	// Verify checkpoint exists in v2
	summary, err := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary, "checkpoint should exist in v2 after migration")
	assert.Equal(t, cpID, summary.CheckpointID)
}

func TestMigrateCheckpointsV2_PreservesCreatedAt(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cpID := id.MustCheckpointID("b1c2d3e4f5a6")
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-created-at",
		CreatedAt:    createdAt,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"hello\"}\n")),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	content, err := v2Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, err)
	assert.True(t, content.Metadata.CreatedAt.Equal(createdAt))
}

func TestMigrateCheckpointsV2_Idempotent(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("c3d4e5f6a1b2")
	writeV1Checkpoint(t, v1Store, cpID, "session-idem",
		[]byte("{\"type\":\"assistant\",\"message\":\"idempotent test\"}\n"),
		[]string{"idem prompt"},
	)

	var stdout bytes.Buffer

	// First run: should migrate
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)
	assert.Equal(t, 0, result1.skipped)

	// Second run: should skip (no agent type means backfill also can't produce compact transcript)
	stdout.Reset()
	result2, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)
}

func TestMigrateCheckpointsV2_ForceOverwritesExisting(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("f0f1f2f3f4f5")
	writeV1Checkpoint(t, v1Store, cpID, "session-force",
		[]byte("{\"type\":\"assistant\",\"message\":\"original\"}\n"),
		[]string{"original prompt"},
	)

	var stdout bytes.Buffer

	// First run: normal migration
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	// Second run without force: should skip
	stdout.Reset()
	result2, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)

	// Third run with force: should re-migrate
	stdout.Reset()
	result3, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result3.migrated)
	assert.Equal(t, 0, result3.skipped)
	assert.Empty(t, stdout.String())

	// Verify checkpoint still readable in v2
	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	assert.Equal(t, cpID, summary.CheckpointID)
}

func TestMigrateCheckpointsV2_ForceMultipleCheckpoints(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID1 := id.MustCheckpointID("a0a1a2a3a4a5")
	cpID2 := id.MustCheckpointID("b0b1b2b3b4b5")
	writeV1Checkpoint(t, v1Store, cpID1, "session-force-1",
		[]byte("{\"type\":\"assistant\",\"message\":\"first\"}\n"),
		[]string{"prompt 1"},
	)
	writeV1Checkpoint(t, v1Store, cpID2, "session-force-2",
		[]byte("{\"type\":\"assistant\",\"message\":\"second\"}\n"),
		[]string{"prompt 2"},
	)

	// First run: migrates both
	var discard bytes.Buffer
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &discard, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result1.migrated)

	// Force re-migrate: should re-migrate both (0 skipped)
	var stdout bytes.Buffer
	result2, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, err)
	assert.Equal(t, 2, result2.migrated)
	assert.Equal(t, 0, result2.skipped)
}

func TestMigrateCmd_ForceFlag(t *testing.T) {
	t.Parallel()
	cmd := newMigrateCmd()

	// Verify --force flag exists
	flag := cmd.Flags().Lookup("force")
	require.NotNil(t, flag, "--force flag should be registered")
	assert.Equal(t, "false", flag.DefValue)
}

func TestMigrateCheckpointsV2_MultiSession(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("d4e5f6a1b2c3")

	// Write first session
	writeV1Checkpoint(t, v1Store, cpID, "session-multi-1",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 1\"}\n"),
		[]string{"prompt 1"},
	)

	// Write second session to same checkpoint
	writeV1Checkpoint(t, v1Store, cpID, "session-multi-2",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 2\"}\n"),
		[]string{"prompt 2"},
	)

	var stdout bytes.Buffer

	result, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	// Verify both sessions are in v2
	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	assert.GreaterOrEqual(t, len(summary.Sessions), 2, "should have at least 2 sessions")
}

func TestMigrateCheckpointsV2_SkipsV1SessionWithoutTranscript(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("445566778899")

	writeV1Checkpoint(t, v1Store, cpID, "session-real",
		[]byte("{\"type\":\"assistant\",\"message\":\"real session\"}\n"),
		[]string{"real prompt"},
	)

	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-without-transcript",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)
	assert.Equal(t, 1, result.missingSessions)

	output := stdout.String()
	assert.NotContains(t, output, "warning: skipping v1 session 1")
	assert.NotContains(t, output, "Migrating checkpoint")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	assert.Equal(t, "/"+cpID.Path()+"/0/metadata.json", summary.Sessions[0].Metadata)
}

func TestMigrateCheckpointsV2_SkipsV1SessionWithMissingDirectory(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("4455667788aa")
	writeV1Checkpoint(t, v1Store, cpID, "session-real",
		[]byte("{\"type\":\"assistant\",\"message\":\"real session\"}\n"),
		[]string{"real prompt"},
	)
	appendMissingV1SessionReference(t, repo, v1Store, cpID)

	var stdout bytes.Buffer
	result, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 0, result.failed)
	assert.Equal(t, 1, result.missingSessions)

	output := stdout.String()
	assert.NotContains(t, output, "warning: skipping v1 session 1")
	assert.NotContains(t, output, "skipped 1 session(s) with missing transcript/session content")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	assert.Equal(t, "/"+cpID.Path()+"/0/metadata.json", summary.Sessions[0].Metadata)
}

func TestMigrateCheckpointsV2_TaskMetadataUsesMigratedSessionIndexAfterSkip(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("66778899aabb")

	writeV1Checkpoint(t, v1Store, cpID, "session-real",
		[]byte("{\"type\":\"assistant\",\"message\":\"real session\"}\n"),
		[]string{"real prompt"},
	)

	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-without-transcript",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	err = v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-task",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"task session\"}\n")),
		Prompts:      []string{"task prompt"},
		IsTask:       true,
		ToolUseID:    "toolu_root_shifted",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)
	addV1SessionTasksTree(t, repo, cpID, 2, "toolu_session_shifted")

	var stdout bytes.Buffer
	result, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 2)
	assert.Equal(t, "/"+cpID.Path()+"/1/metadata.json", summary.Sessions[1].Metadata)

	_, rootTreeHash, refErr := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, refErr)
	rootTree, treeErr := repo.TreeObject(rootTreeHash)
	require.NoError(t, treeErr)

	_, err = rootTree.File(cpID.Path() + "/1/tasks/toolu_root_shifted/checkpoint.json")
	require.NoError(t, err, "root task metadata should follow the shifted v2 session index")
	_, err = rootTree.File(cpID.Path() + "/1/tasks/toolu_session_shifted/checkpoint.json")
	require.NoError(t, err, "session task metadata should follow the shifted v2 session index")
	_, err = rootTree.File(cpID.Path() + "/2/tasks/toolu_root_shifted/checkpoint.json")
	require.Error(t, err, "task metadata must not be written under a non-existent v2 session")
}

func TestMigrateCheckpointsV2_SkipsCheckpointWhenAllV1SessionsMissingTranscript(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("5566778899bb")
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "metadata-only-session",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 0, result.migrated)
	assert.Equal(t, 1, result.skipped)
	assert.Equal(t, 0, result.failed)
	assert.Equal(t, 1, result.missingSessions)

	output := stdout.String()
	assert.NotContains(t, output, "warning: skipping v1 session 0")
	assert.NotContains(t, output, "skipped (no migratable v1 sessions")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	assert.Nil(t, summary)
}

func TestMigrateCheckpointsV2_ForcePrunesSkippedV2Sessions(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("778899aabbcc")
	writeV1Checkpoint(t, v1Store, cpID, "session-keep",
		[]byte("{\"type\":\"assistant\",\"message\":\"keep\"}\n"),
		[]string{"keep prompt"},
	)
	writeV1Checkpoint(t, v1Store, cpID, "session-stale",
		[]byte("{\"type\":\"assistant\",\"message\":\"stale\"}\n"),
		[]string{"stale prompt"},
	)

	var initialRun bytes.Buffer
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	initialSummary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, initialSummary)
	require.Len(t, initialSummary.Sessions, 2)

	err = v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-stale",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only stale prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result2, rerunErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, rerunErr)
	assert.Equal(t, 1, result2.migrated)
	assert.Equal(t, 0, result2.skipped)
	assert.Equal(t, 1, result2.missingSessions)
	assert.NotContains(t, stdout.String(), "warning: skipping v1 session 1")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)
	require.Len(t, summary.Sessions, 1)
	assert.Equal(t, "/"+cpID.Path()+"/0/metadata.json", summary.Sessions[0].Metadata)

	_, rootTreeHash, refErr := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, refErr)
	rootTree, treeErr := repo.TreeObject(rootTreeHash)
	require.NoError(t, treeErr)
	_, err = rootTree.File(cpID.Path() + "/1/" + paths.V2RawTranscriptHashFileName)
	require.Error(t, err, "force migration should remove stale full transcript data for skipped sessions")
}

func TestMigrateCheckpointsV2_ForcePruneRemovesEmptyShardWhenAllSessionsSkipped(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("8899aabbccdd")
	writeV1Checkpoint(t, v1Store, cpID, "session-stale-only",
		[]byte("{\"type\":\"assistant\",\"message\":\"stale only\"}\n"),
		[]string{"stale prompt"},
	)

	var initialRun bytes.Buffer
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	err = v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-stale-only",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(nil),
		Prompts:      []string{"metadata-only stale prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer
	result2, rerunErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, true)
	require.NoError(t, rerunErr)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)
	assert.Equal(t, 1, result2.missingSessions)
	assert.NotContains(t, stdout.String(), "no migratable v1 sessions")

	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	assert.Nil(t, summary)

	assertNoV2ShardPrefix(t, repo, v2Store, plumbing.ReferenceName(paths.V2MainRefName), cpID)
	assertNoV2ShardPrefix(t, repo, v2Store, plumbing.ReferenceName(paths.V2FullCurrentRefName), cpID)
}

func assertNoV2ShardPrefix(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, refName plumbing.ReferenceName, cpID id.CheckpointID) {
	t.Helper()

	_, rootTreeHash, err := v2Store.GetRefState(refName)
	require.NoError(t, err)

	rootTree, err := repo.TreeObject(rootTreeHash)
	require.NoError(t, err)

	_, err = rootTree.Tree(string(cpID[:2]))
	require.Error(t, err, "force prune should remove an empty shard prefix from %s", refName)
}

func appendMissingV1SessionReference(t *testing.T, repo *git.Repository, v1Store *checkpoint.GitStore, cpID id.CheckpointID) {
	t.Helper()

	ctx := context.Background()
	summary, err := v1Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)

	missingIndex := len(summary.Sessions)
	missingBase := "/" + cpID.Path() + "/" + strconv.Itoa(missingIndex) + "/"
	summary.Sessions = append(summary.Sessions, checkpoint.SessionFilePaths{
		Metadata:    missingBase + paths.MetadataFileName,
		Transcript:  missingBase + paths.TranscriptFileName,
		ContentHash: missingBase + paths.ContentHashFileName,
		Prompt:      missingBase + paths.PromptFileName,
	})

	metadataJSON, err := json.MarshalIndent(summary, "", "  ")
	require.NoError(t, err)
	metadataJSON = append(metadataJSON, '\n')

	metadataHash, err := checkpoint.CreateBlobFromContent(repo, metadataJSON)
	require.NoError(t, err)

	refName := plumbing.NewBranchReferenceName(paths.MetadataBranchName)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)

	newTreeHash, err := checkpoint.UpdateSubtree(
		repo,
		commit.TreeHash,
		[]string{string(cpID[:2]), string(cpID[2:])},
		[]object.TreeEntry{{
			Name: paths.MetadataFileName,
			Mode: filemode.Regular,
			Hash: metadataHash,
		}},
		checkpoint.UpdateSubtreeOptions{MergeMode: checkpoint.MergeKeepExisting},
	)
	require.NoError(t, err)

	newCommitHash, err := checkpoint.CreateCommit(ctx, repo, newTreeHash, ref.Hash(), "test: stale v1 session reference\n", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, newCommitHash)))
}

func TestMigrateCheckpointsV2_NoV1Branch(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	var stdout bytes.Buffer

	// No v1 data written — ListCommitted returns empty
	result, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result.migrated)
	assert.Empty(t, stdout.String())
}

func TestPrintMigrateCompletion_LogPathForSkippedOrMissing(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	printMigrateCompletion(&stdout, &migrateResult{
		total:           2,
		migrated:        1,
		skipped:         1,
		missingSessions: 1,
	})

	output := stdout.String()
	assert.Contains(t, output, "Migration complete: 1 migrated, 1 skipped, 0 failed")
	assert.Contains(t, output, ".entire/logs/entire.log")
}

func TestPrintMigrateCompletion_CleanRunOmitsLogPath(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	printMigrateCompletion(&stdout, &migrateResult{
		total:    1,
		migrated: 1,
	})

	output := stdout.String()
	assert.Contains(t, output, "Migration complete: 1 migrated, 0 skipped, 0 failed")
	assert.NotContains(t, output, ".entire/logs/entire.log")
}

func TestMigrateCmd_InvalidFlag(t *testing.T) {
	t.Parallel()
	cmd := newMigrateCmd()
	cmd.SetArgs([]string{"--checkpoints", "v3"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported checkpoints version")
}

func TestMigrateCheckpointsV2_CompactionSkipped(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("e5f6a1b2c3d4")
	// Write checkpoint with no agent type — compaction will be skipped
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-noagent",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"no agent\"}\n")),
		Prompts:      []string{"compact fail prompt"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer

	result, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)
	assert.Equal(t, 1, result.compactTranscriptSkipped)
	assert.Empty(t, stdout.String())
}

func TestMigrateCheckpointsV2_TaskCheckpoint(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("b2c3d4e5f6a1")
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-task-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"task work\"}\n")),
		Prompts:      []string{"task prompt"},
		IsTask:       true,
		ToolUseID:    "toolu_01ABC",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	var stdout bytes.Buffer

	result, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)

	// Verify task checkpoint exists in v2
	summary, readErr := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, readErr)
	require.NotNil(t, summary)

	// Verify task metadata tree was copied into v2 /full/current.
	_, rootTreeHash, refErr := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, refErr)
	rootTree, treeErr := repo.TreeObject(rootTreeHash)
	require.NoError(t, treeErr)
	_, taskFileErr := rootTree.File(cpID.Path() + "/0/tasks/toolu_01ABC/checkpoint.json")
	require.NoError(t, taskFileErr, "expected migrated task checkpoint metadata in /full/current")
}

func TestMigrateCheckpointsV2_AllSkippedOnRerun(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID1 := id.MustCheckpointID("f6a1b2c3d4e5")
	cpID2 := id.MustCheckpointID("a1b2c3d4e5f7")

	writeV1Checkpoint(t, v1Store, cpID1, "session-p1",
		[]byte("{\"type\":\"assistant\",\"message\":\"first\"}\n"),
		[]string{"prompt 1"},
	)
	writeV1Checkpoint(t, v1Store, cpID2, "session-p2",
		[]byte("{\"type\":\"assistant\",\"message\":\"second\"}\n"),
		[]string{"prompt 2"},
	)

	// First run: migrates both
	var discard bytes.Buffer
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &discard, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result1.migrated)

	// Second run: skips both
	var stdout bytes.Buffer
	result2, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 2, result2.skipped)
}

func TestMigrateCheckpointsV2_BackfillCompactTranscript(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("aabb11223344")

	// Write v1 checkpoint with agent type (so compaction can succeed)
	err := v1Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-backfill",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\n")),
		Prompts:      []string{"hello"},
		Agent:        "Claude Code",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	// Write to v2 WITHOUT compact transcript (simulating earlier migration)
	err = v2Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-backfill",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n")),
		Prompts:      []string{"hello"},
		Agent:        "Claude Code",
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
		// CompactTranscript intentionally nil
	})
	require.NoError(t, err)

	// Verify no transcript.jsonl on /main yet
	summary, err := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary)
	assert.Empty(t, summary.Sessions[0].Transcript, "should have no compact transcript before backfill")

	// Run migration — should backfill the compact transcript
	var stdout bytes.Buffer
	result, migrateErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated, "backfill should count as migrated")
	assert.Equal(t, 0, result.skipped)
	assert.Equal(t, 1, result.backfilledCompactTranscripts)
	assert.Empty(t, stdout.String())

	// Verify transcript.jsonl now exists
	summary2, err := v2Store.ReadCommitted(context.Background(), cpID)
	require.NoError(t, err)
	require.NotNil(t, summary2)
	assert.NotEmpty(t, summary2.Sessions[0].Transcript, "should have compact transcript after backfill")
}

func TestMigrateCheckpointsV2_UsesComputedCompactTranscriptStart(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("5566778899aa")
	transcript := []byte(
		"{\"type\":\"human\",\"message\":{\"content\":\"prompt 1\"}}\n" +
			"{\"type\":\"assistant\",\"message\":{\"content\":\"reply 1\"}}\n" +
			"{\"type\":\"human\",\"message\":{\"content\":\"prompt 2\"}}\n" +
			"{\"type\":\"assistant\",\"message\":{\"content\":\"reply 2\"}}\n",
	)
	err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID:              cpID,
		SessionID:                 "session-compact-start-migrate",
		Strategy:                  "manual-commit",
		Transcript:                redact.AlreadyRedacted(transcript),
		Prompts:                   []string{"prompt 2"},
		Agent:                     agent.AgentTypeClaudeCode,
		CheckpointTranscriptStart: 2, // full transcript line domain
		AuthorName:                "Test",
		AuthorEmail:               "test@test.com",
	})
	require.NoError(t, err)

	v1Content, err := v1Store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	fullCompacted := tryCompactTranscript(ctx, v1Content.Transcript, v1Content.Metadata)
	require.NotNil(t, fullCompacted)
	scopedCompacted, err := compact.Compact(redact.AlreadyRedacted(v1Content.Transcript), compact.MetadataFields{
		Agent:      string(v1Content.Metadata.Agent),
		CLIVersion: versioninfo.Version,
		StartLine:  v1Content.Metadata.GetTranscriptStart(),
	})
	require.NoError(t, err)
	require.NotNil(t, scopedCompacted)
	require.Greater(t, bytes.Count(fullCompacted, []byte{'\n'}), bytes.Count(scopedCompacted, []byte{'\n'}))
	expectedOffset := computeCompactOffset(ctx, v1Content.Transcript, fullCompacted, v1Content.Metadata)
	require.Positive(t, expectedOffset, "expected non-zero compact transcript start")

	var stdout bytes.Buffer
	result, migrateErr := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, migrateErr)
	assert.Equal(t, 1, result.migrated)

	v2MainRef, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	v2MainCommit, err := repo.CommitObject(v2MainRef.Hash())
	require.NoError(t, err)
	v2MainTree, err := v2MainCommit.Tree()
	require.NoError(t, err)

	metadataFile, err := v2MainTree.File(cpID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err)
	metadataContent, err := metadataFile.Contents()
	require.NoError(t, err)

	var metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent), &metadata))
	assert.Equal(t, expectedOffset, metadata.CheckpointTranscriptStart)

	storedCompact, err := v2Store.ReadSessionCompactTranscript(ctx, cpID, 0)
	require.NoError(t, err)
	assert.Equal(t, fullCompacted, storedCompact, "migration should persist cumulative compact transcript")
}

func TestMigrateCheckpointsV2_RepairsMissingFullTranscriptBeforeBackfill(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("112233aabbcc")
	writeV1Checkpoint(t, v1Store, cpID, "session-repair-001",
		[]byte("{\"type\":\"assistant\",\"message\":\"repair me\"}\n"),
		[]string{"repair prompt"},
	)

	// Initial migration to create v2 state.
	var initialRun bytes.Buffer
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	// Simulate interrupted migration by removing raw transcript files from /full/current.
	removeV2SessionTranscriptFiles(t, repo, v2Store, cpID, 0)

	// Re-run migration: should repair /full/current and count as migrated (not skipped).
	var rerun bytes.Buffer
	result2, rerunErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &rerun, false)
	require.NoError(t, rerunErr)
	assert.Equal(t, 1, result2.migrated)
	assert.Equal(t, 0, result2.failed)
	assert.Equal(t, 1, result2.repaired)
	assert.Empty(t, rerun.String())

	content, readErr := v2Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, readErr)
	assert.NotEmpty(t, content.Transcript, "raw full transcript should be restored in /full/current")
}

func TestMigrateCheckpointsV2_SkipsRepairWhenArchivedFullExists(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)

	cpID := id.MustCheckpointID("334455ddeeff")
	writeV1Checkpoint(t, v1Store, cpID, "session-repair-archive-001",
		[]byte("{\"type\":\"assistant\",\"message\":\"repair from archive fallback\"}\n"),
		[]string{"repair archive prompt"},
	)

	// Initial migration to seed v2.
	var initialRun bytes.Buffer
	result1, err := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &initialRun, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result1.migrated)

	// Preserve current generation as an archived ref to simulate fallback availability.
	currentCommitHash, _, refErr := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, refErr)
	archiveRef := plumbing.ReferenceName(paths.V2FullRefPrefix + "0000000000001")
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(archiveRef, currentCommitHash)))

	// Remove current /full/current transcript artifacts.
	removeV2SessionTranscriptFiles(t, repo, v2Store, cpID, 0)

	// Sanity-check fallback exists: ReadSessionContent can still read from archive.
	archivedRead, archivedReadErr := v2Store.ReadSessionContent(context.Background(), cpID, 0)
	require.NoError(t, archivedReadErr)
	assert.NotEmpty(t, archivedRead.Transcript)

	// Re-run migration: archived /full/* artifacts are sufficient, so it should
	// not rehydrate old raw transcripts into /full/current.
	var rerun bytes.Buffer
	result2, rerunErr := migrateCheckpointsV2(context.Background(), repo, v1Store, v2Store, &rerun, false)
	require.NoError(t, rerunErr)
	assert.Equal(t, 0, result2.migrated)
	assert.Equal(t, 1, result2.skipped)
	assert.NotContains(t, rerun.String(), "repaired partial v2 checkpoint state")

	ok, checkErr := hasFullSessionArtifacts(v2Store, cpID, 0)
	require.NoError(t, checkErr)
	assert.True(t, ok, "expected archived /full/* artifacts to count as present")
	assert.False(t, hasCurrentFullSessionArtifactsForTest(t, repo, v2Store, cpID, 0),
		"migration rerun must not copy archived artifacts back into /full/current")
}

func removeV2SessionTranscriptFiles(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int) {
	t.Helper()

	refName := plumbing.ReferenceName(paths.V2FullCurrentRefName)
	parentHash, rootTreeHash, err := v2Store.GetRefState(refName)
	require.NoError(t, err)

	newRootHash, updateErr := checkpoint.UpdateSubtree(
		repo,
		rootTreeHash,
		[]string{string(cpID[:2]), string(cpID[2:]), strconv.Itoa(sessionIdx)},
		nil,
		checkpoint.UpdateSubtreeOptions{
			MergeMode: checkpoint.MergeKeepExisting,
			DeleteNames: []string{
				paths.V2RawTranscriptFileName,
				paths.V2RawTranscriptFileName + ".001",
				paths.V2RawTranscriptFileName + ".002",
				paths.V2RawTranscriptHashFileName,
			},
		},
	)
	require.NoError(t, updateErr)

	commitHash, commitErr := checkpoint.CreateCommit(context.Background(), repo, newRootHash, parentHash, "test: remove full transcript\n", "Test", "test@test.com")
	require.NoError(t, commitErr)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(refName, commitHash)))
}

func hasCurrentFullSessionArtifactsForTest(t *testing.T, repo *git.Repository, v2Store *checkpoint.V2GitStore, cpID id.CheckpointID, sessionIdx int) bool {
	t.Helper()

	_, rootTreeHash, err := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, err)

	rootTree, err := repo.TreeObject(rootTreeHash)
	require.NoError(t, err)

	sessionPath := cpID.Path() + "/" + strconv.Itoa(sessionIdx)
	sessionTree, err := rootTree.Tree(sessionPath)
	if err != nil {
		return false
	}

	hasTranscript := false
	for _, entry := range sessionTree.Entries {
		if entry.Name == paths.V2RawTranscriptFileName || strings.HasPrefix(entry.Name, paths.V2RawTranscriptFileName+".") {
			hasTranscript = true
			break
		}
	}
	if !hasTranscript {
		return false
	}

	_, err = sessionTree.File(paths.V2RawTranscriptHashFileName)
	return err == nil
}

func TestBuildMigrateWriteOpts_PromptSeparatorRoundTrip(t *testing.T) {
	t.Parallel()

	cpID := id.MustCheckpointID("123456abcdef")
	rawPrompts := strings.Join([]string{
		"first line\nwith newline",
		"second prompt",
	}, checkpoint.PromptSeparator)

	opts := buildMigrateWriteOpts(&checkpoint.SessionContent{
		Metadata: checkpoint.CommittedMetadata{
			SessionID: "session-prompts-001",
			Strategy:  "manual-commit",
		},
		Prompts: rawPrompts,
	}, checkpoint.CommittedInfo{
		CheckpointID: cpID,
	}, nil)

	require.Len(t, opts.Prompts, 2)
	assert.Equal(t, "first line\nwith newline", opts.Prompts[0])
	assert.Equal(t, "second prompt", opts.Prompts[1])
}

func TestLatestMigratedV2SessionIndex_Empty(t *testing.T) {
	t.Parallel()

	latest, ok := latestMigratedV2SessionIndex(nil)
	assert.Equal(t, -1, latest)
	assert.False(t, ok)
}

func TestSpliceTasksTreeToV2_MergesTaskDirectories(t *testing.T) {
	t.Parallel()

	repo := initMigrateTestRepo(t)
	_, v2Store := newMigrateStores(repo)
	cpID := id.MustCheckpointID("123abc456def")

	err := v2Store.WriteCommitted(context.Background(), checkpoint.WriteCommittedOptions{
		CheckpointID: cpID,
		SessionID:    "session-001",
		Strategy:     "manual-commit",
		Agent:        "Cursor",
		Transcript:   redact.AlreadyRedacted([]byte(`{"type":"assistant","message":"seed"}`)),
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	})
	require.NoError(t, err)

	rootTasksHash := buildTasksTreeHash(t, repo, "toolu_root")
	sessionTasksHash := buildTasksTreeHash(t, repo, "toolu_session")

	require.NoError(t, spliceTasksTreeToV2(context.Background(), repo, v2Store, cpID, 0, rootTasksHash))
	require.NoError(t, spliceTasksTreeToV2(context.Background(), repo, v2Store, cpID, 0, sessionTasksHash))

	_, rootTreeHash, refErr := v2Store.GetRefState(plumbing.ReferenceName(paths.V2FullCurrentRefName))
	require.NoError(t, refErr)
	rootTree, treeErr := repo.TreeObject(rootTreeHash)
	require.NoError(t, treeErr)

	_, err = rootTree.File(cpID.Path() + "/0/tasks/toolu_root/checkpoint.json")
	require.NoError(t, err, "root task metadata should be preserved")
	_, err = rootTree.File(cpID.Path() + "/0/tasks/toolu_session/checkpoint.json")
	require.NoError(t, err, "session task metadata should be preserved")
}

func TestMigrateCheckpointsV2_PreservesPromptAttributions(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("aabb22334455")
	promptAttrs := json.RawMessage(`[{"prompt_index":0,"user_lines":["main.go:10"]}]`)

	err := v1Store.WriteCommitted(ctx, checkpoint.WriteCommittedOptions{
		CheckpointID:           cpID,
		SessionID:              "session-pa-001",
		Strategy:               "manual-commit",
		Transcript:             redact.AlreadyRedacted([]byte("{\"type\":\"assistant\",\"message\":\"pa test\"}\n")),
		Prompts:                []string{"test prompt"},
		PromptAttributionsJSON: promptAttrs,
		AuthorName:             "Test",
		AuthorEmail:            "test@test.com",
	})
	require.NoError(t, err)

	// Verify v1 has prompt_attributions
	v1Content, err := v1Store.ReadSessionContent(ctx, cpID, 0)
	require.NoError(t, err)
	require.NotNil(t, v1Content.Metadata.PromptAttributions, "v1 should have prompt_attributions")

	// Migrate
	var stdout bytes.Buffer
	result, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	// Read v2 session metadata from /main ref and verify prompt_attributions preserved
	v2MainRef, err := repo.Reference(plumbing.ReferenceName(paths.V2MainRefName), true)
	require.NoError(t, err)
	v2MainCommit, err := repo.CommitObject(v2MainRef.Hash())
	require.NoError(t, err)
	v2MainTree, err := v2MainCommit.Tree()
	require.NoError(t, err)

	metadataFile, err := v2MainTree.File(cpID.Path() + "/0/" + paths.MetadataFileName)
	require.NoError(t, err)
	metadataContent, err := metadataFile.Contents()
	require.NoError(t, err)

	var metadata checkpoint.CommittedMetadata
	require.NoError(t, json.Unmarshal([]byte(metadataContent), &metadata))
	assert.JSONEq(t, string(promptAttrs), string(metadata.PromptAttributions),
		"v2 session metadata should preserve prompt_attributions from v1")
}

func TestMigrateCheckpointsV2_PreservesCombinedAttribution(t *testing.T) {
	t.Parallel()
	repo := initMigrateTestRepo(t)
	v1Store, v2Store := newMigrateStores(repo)
	ctx := context.Background()

	cpID := id.MustCheckpointID("ccdd55667788")

	// Write two sessions so combined attribution is meaningful
	writeV1Checkpoint(t, v1Store, cpID, "session-ca-001",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 1\"}\n"),
		[]string{"prompt 1"},
	)
	writeV1Checkpoint(t, v1Store, cpID, "session-ca-002",
		[]byte("{\"type\":\"assistant\",\"message\":\"session 2\"}\n"),
		[]string{"prompt 2"},
	)

	// Inject CombinedAttribution into v1 root summary
	combined := &checkpoint.InitialAttribution{
		CalculatedAt:      time.Date(2026, 4, 15, 0, 18, 47, 0, time.UTC),
		AgentLines:        119,
		AgentRemoved:      94,
		HumanAdded:        3,
		HumanModified:     0,
		HumanRemoved:      1,
		TotalCommitted:    122,
		TotalLinesChanged: 217,
		AgentPercentage:   98.15668202764977,
		MetricVersion:     2,
	}
	err := v1Store.UpdateCheckpointSummary(ctx, cpID, combined)
	require.NoError(t, err)

	// Verify v1 root summary has CombinedAttribution
	v1Summary, err := v1Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, v1Summary.CombinedAttribution, "v1 should have combined_attribution")

	// Migrate
	var stdout bytes.Buffer
	result, err := migrateCheckpointsV2(ctx, repo, v1Store, v2Store, &stdout, false)
	require.NoError(t, err)
	assert.Equal(t, 1, result.migrated)

	// Read v2 root summary and verify CombinedAttribution preserved
	v2Summary, err := v2Store.ReadCommitted(ctx, cpID)
	require.NoError(t, err)
	require.NotNil(t, v2Summary)
	require.NotNil(t, v2Summary.CombinedAttribution,
		"v2 root summary should preserve combined_attribution from v1")
	assert.Equal(t, combined.CalculatedAt, v2Summary.CombinedAttribution.CalculatedAt)
	assert.Equal(t, combined.AgentLines, v2Summary.CombinedAttribution.AgentLines)
	assert.Equal(t, combined.AgentRemoved, v2Summary.CombinedAttribution.AgentRemoved)
	assert.Equal(t, combined.HumanAdded, v2Summary.CombinedAttribution.HumanAdded)
	assert.Equal(t, combined.HumanModified, v2Summary.CombinedAttribution.HumanModified)
	assert.Equal(t, combined.HumanRemoved, v2Summary.CombinedAttribution.HumanRemoved)
	assert.Equal(t, combined.TotalCommitted, v2Summary.CombinedAttribution.TotalCommitted)
	assert.Equal(t, combined.TotalLinesChanged, v2Summary.CombinedAttribution.TotalLinesChanged)
	assert.InDelta(t, combined.AgentPercentage, v2Summary.CombinedAttribution.AgentPercentage, 0.001)
	assert.Equal(t, combined.MetricVersion, v2Summary.CombinedAttribution.MetricVersion)
}
