//go:build integration

package integration

// Only a commit's final trailer block links it to a checkpoint. A checkpoint ID
// anywhere else in the message (pasted, quoted, or left over from another
// commit) names a checkpoint the commit did not stamp, so the hooks must stamp
// a fresh trailer and condense the session's work there.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

// An editor message is cleaned with the repository's comment prefix. Prepare
// must write its trailer and explanation for that prefix, or the recorded
// commit carries the explanation and ends without a trailer block.
func TestTrailerAuthority_EditorCommitHonorsCommentChar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		commentChar   string
		source        string
		message       string // as git hands it to prepare-commit-msg
		cleanupPrefix string // the prefix git commit then cleans with
	}{
		{
			name:          "semicolon",
			commentChar:   ";",
			message:       "\n" + gitCommentBlock(";"),
			cleanupPrefix: ";",
		},
		{
			// The template's "#" heading makes git pick ";".
			name:          "auto",
			commentChar:   "auto",
			source:        "template",
			message:       "# Release notes\n\n" + gitCommentBlock(";"),
			cleanupPrefix: ";",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			setRepoGitConfig(env, "core.commentChar", tt.commentChar)
			authorityAgentTurn(env, "mine.txt", "my agent work\n")

			final := editorCommit(env, tt.message, tt.source, tt.cleanupPrefix, "mine.txt")

			require.NotContains(t, final, "Remove the Entire-Checkpoint trailer above", "git's cleanup must drop Entire's explanation: %q", final)
			stamped, ok := trailers.ParseCheckpointFromFinalTrailerBlock(final)
			require.True(t, ok, "the recorded commit must end with its trailer: %q", final)
			require.True(t, advCheckpointStored(env, stamped.String()), "post-commit condenses into the stamped checkpoint")
		})
	}
}

// An amend hands prepare-commit-msg the commit's message and git's comment
// block in the repository's comment prefix. The commit's own trailer must still
// be found there, or the amend restores a second copy of it.
func TestTrailerAuthority_AmendHonorsCommentChar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		commentChar string
		headMessage string
		blockPrefix string // the prefix git writes its comment block with
	}{
		{name: "semicolon", commentChar: ";", headMessage: "My change", blockPrefix: ";"},
		// The "#" line in the message makes git pick ";".
		{name: "auto", commentChar: "auto", headMessage: "My change\n\n#42 is the issue", blockPrefix: ";"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			setRepoGitConfig(env, "core.commentChar", tt.commentChar)
			authorityAgentTurn(env, "mine.txt", "my agent work\n")
			env.GitCommitWithHooks(tt.headMessage, "mine.txt")
			headMsg := env.GetCommitMessage(env.GetHeadHash())
			_, ok := trailers.ParseCheckpointFromFinalTrailerBlock(headMsg)
			require.True(t, ok, "precondition: HEAD is linked: %q", headMsg)

			amendMsg := strings.TrimRight(headMsg, "\n") + "\n\n" + gitCommentBlock(tt.blockPrefix)
			msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
			require.NoError(t, os.WriteFile(msgFile, []byte(amendMsg), 0o644))
			if out, err := env.prepareCommitMsgCmd(true, msgFile, "commit").CombinedOutput(); err != nil {
				t.Logf("prepare-commit-msg output: %s", out)
			}
			prepared, err := os.ReadFile(msgFile)
			require.NoError(t, err)
			require.Equal(t, amendMsg, string(prepared), "the amend keeps the commit's own trailer and adds none")
		})
	}
}

// Git cuts an editor message only at its exact scissors line. A body line that
// merely mentions the marker must not make an earlier body trailer look final.
func TestTrailerAuthority_ProseScissorsMarkerIsNotACut(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")

	final := editorCommit(env,
		"Fix the bug\n\nEntire-Checkpoint: "+advForeignCheckpoint+"\n\n"+
			"Explain ------------------------ >8 ------------------------ here.\n\n"+gitCommentBlock("#"),
		"", "#", "mine.txt")

	stamped := requireFreshFinalTrailer(t, final, advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the session's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a body line must not claim a checkpoint ID this clone does not have")
}

// gitCommentBlock is the comment block git appends to a message it opens in
// the editor, written with prefix.
func gitCommentBlock(prefix string) string {
	return prefix + " Please enter the commit message for your changes. Lines starting\n" +
		prefix + " with '" + prefix + "' will be ignored, and an empty message aborts the commit.\n" +
		prefix + "\n" +
		prefix + " On branch feature/test-branch\n"
}

// editorCommit runs one editor-flow commit: prepare-commit-msg gets message
// (source "" is a plain `git commit`), the user types a subject on an empty
// first line, and git's own cleanup with cleanupPrefix produces what is
// recorded before post-commit runs. Returns the recorded message.
func editorCommit(env *TestEnv, message, source, cleanupPrefix string, files ...string) string {
	env.T.Helper()
	env.GitAdd(files...)
	msgFile := filepath.Join(env.RepoDir, ".git", "COMMIT_EDITMSG")
	require.NoError(env.T, os.WriteFile(msgFile, []byte(message), 0o644))
	args := []string{msgFile}
	if source != "" {
		args = append(args, source)
	}
	if out, err := env.prepareCommitMsgCmd(true, args...).CombinedOutput(); err != nil {
		env.T.Logf("prepare-commit-msg output: %s", out)
	}
	prepared, err := os.ReadFile(msgFile)
	require.NoError(env.T, err)
	edited := string(prepared)
	if strings.HasPrefix(edited, "\n") {
		edited = "typed in editor" + edited
	}

	cleanup := exec.CommandContext(env.T.Context(), "git", "-c", "core.commentChar="+cleanupPrefix, "stripspace", "--strip-comments")
	cleanup.Dir = env.RepoDir
	cleanup.Env = testutil.GitIsolatedEnv()
	cleanup.Stdin = strings.NewReader(edited)
	recorded, err := cleanup.Output()
	require.NoError(env.T, err)

	commitWithPostCommit(env, string(recorded))
	return string(recorded)
}

// setRepoGitConfig sets one repository config value and makes it the
// .git/config guard's new baseline.
func setRepoGitConfig(env *TestEnv, key, value string) {
	env.T.Helper()
	testutil.RunGit(env.T, env.RepoDir, "config", key, value)
	require.Equal(env.T, value, strings.TrimSpace(testutil.RunGit(env.T, env.RepoDir, "config", "--get", key)))
	data, err := os.ReadFile(filepath.Join(env.RepoDir, ".git", "config"))
	require.NoError(env.T, err)
	env.AcceptGitConfigChanges(string(data))
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
