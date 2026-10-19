//go:build integration

package integration

// Only a commit's final trailer block links it to a checkpoint. A checkpoint ID
// anywhere else in the message (pasted, quoted, or left over from another
// commit) names a checkpoint the commit did not stamp, so the hooks must stamp
// a fresh trailer and condense the session's work there.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func TestTrailerAuthority_BodyLineNamingUnknownCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")

	_, final := advCommit(env,
		"Fix the bug\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n\nSigned-off-by: Test User <test@example.com>\n",
		"message", keepAllTrailers, "mine.txt")

	stamped := requireFreshFinalTrailer(t, final, advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the session's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a body line must not claim a checkpoint ID this clone does not have")
}

func TestTrailerAuthority_IndentedLineNamingUnknownCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")

	_, final := advCommit(env, "Fix the bug\n\n  Entire-Checkpoint: "+advForeignCheckpoint+"\n",
		"message", keepAllTrailers, "mine.txt")

	stamped := requireFreshFinalTrailer(t, final, advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the session's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"an indented line is not a trailer, so it must not claim a checkpoint ID")
}

func TestTrailerAuthority_BodyLineNamingExistingCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "first.txt", "first change\n")
	env.GitCommitWithHooks("First change", "first.txt")
	existing := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	require.NotEmpty(t, existing, "precondition: the first commit is linked")
	before, ok := checkpointBlob(env, existing, paths.MetadataFileName)
	require.True(t, ok, "precondition: the first checkpoint is stored")

	authorityAgentTurn(env, "second.txt", "second change\n")
	_, final := advCommit(env,
		"Follow-up\n\nEntire-Checkpoint: "+existing+"\n\nSigned-off-by: Test User <test@example.com>\n",
		"message", keepAllTrailers, "second.txt")

	stamped := requireFreshFinalTrailer(t, final, existing)
	require.True(t, advCheckpointStored(env, stamped), "the follow-up commit gets a checkpoint of its own")
	after, ok := checkpointBlob(env, existing, paths.MetadataFileName)
	require.True(t, ok)
	require.Equal(t, before, after, "the checkpoint the body mentions must not change")
}

func TestTrailerAuthority_AgentCommitWithBodyLine(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	require.NoError(t, env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath))
	env.WriteFile("agent.txt", "agent work\n")
	sess.CreateTranscript("write agent.txt", []FileChange{{Path: "agent.txt", Content: "agent work\n"}})

	env.GitCommitWithHooksAsAgent(
		"Agent change\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n\nSigned-off-by: Test User <test@example.com>\n",
		"agent.txt")

	stamped := requireFreshFinalTrailer(t, env.GetCommitMessage(env.GetHeadHash()), advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the agent's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a body line must not claim a checkpoint ID this clone does not have")
}

// GUARD: the message git hands prepare-commit-msg for `commit -v --amend` ends
// with its comment block, a scissors line, and the diff. The commit's own
// trailer sits above all of that and must still be recognized, or the amend
// would gain a second trailer below the scissors line, where git discards it.
func TestTrailerAuthority_VerboseAmendKeepsItsTrailer(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	env.GitCommitWithHooks("My change", "mine.txt")
	headMsg := env.GetCommitMessage(env.GetHeadHash())
	_, ok := trailers.ParseCheckpointFromFinalTrailerBlock(headMsg)
	require.True(t, ok, "precondition: HEAD is linked: %q", headMsg)

	amendMsg := strings.TrimRight(headMsg, "\n") + "\n\n" +
		"# Please enter the commit message for your changes. Lines starting\n" +
		"# with '#' will be ignored, and an empty message aborts the commit.\n" +
		"#\n" +
		"# ------------------------ >8 ------------------------\n" +
		"# Do not modify or remove the line above.\n" +
		"# Everything below it will be ignored.\n" +
		"diff --git a/mine.txt b/mine.txt\n" +
		"--- a/mine.txt\n" +
		"+++ b/mine.txt\n" +
		"@@ -0,0 +1 @@\n" +
		"+my agent work\n"
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	require.NoError(t, os.WriteFile(msgFile, []byte(amendMsg), 0o644))
	if out, err := env.prepareCommitMsgCmd(true, msgFile, "commit").CombinedOutput(); err != nil {
		t.Logf("prepare-commit-msg output: %s", out)
	}
	prepared, err := os.ReadFile(msgFile)
	require.NoError(t, err)
	require.Equal(t, amendMsg, string(prepared), "the amend keeps the commit's own trailer and adds none")
}

func keepAllTrailers(string) bool { return true }

// authorityAgentTurn runs one complete agent turn that writes path.
func authorityAgentTurn(env *TestEnv, path, content string) {
	env.T.Helper()
	sess := env.NewSession()
	require.NoError(env.T, env.SimulateUserPromptSubmitWithPromptAndTranscriptPath(sess.ID, "change "+path, sess.TranscriptPath))
	env.WriteFile(path, content)
	sess.CreateTranscript("change "+path, []FileChange{{Path: path, Content: content}})
	require.NoError(env.T, env.SimulateStop(sess.ID, sess.TranscriptPath))
}

// requireFreshFinalTrailer returns the checkpoint the commit's final trailer
// block names, requiring it to differ from the one its body mentions.
func requireFreshFinalTrailer(t *testing.T, message, mentioned string) string {
	t.Helper()
	stamped, ok := trailers.ParseCheckpointFromFinalTrailerBlock(message)
	require.True(t, ok, "prepare-commit-msg must stamp a trailer in the final block: %q", message)
	require.NotEqual(t, mentioned, stamped.String(), "the stamped trailer must not reuse the body's ID: %q", message)
	return stamped.String()
}
