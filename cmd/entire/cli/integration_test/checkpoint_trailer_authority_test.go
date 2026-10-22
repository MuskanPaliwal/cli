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
		"message", keepAll, "mine.txt")

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
		"message", keepAll, "mine.txt")

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
		"message", keepAll, "second.txt")

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
	require.NotEmpty(t, trailers.ParseAllCheckpointsFromFinalTrailerBlock(headMsg, ""), "precondition: HEAD is linked: %q", headMsg)

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
	_, prepared := runPrepareCommitMsg(env, amendMsg, "commit")
	require.Equal(t, amendMsg, prepared, "the amend keeps the commit's own trailer and adds none")
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

			final := editorCommit(env, tt.message, tt.source, tt.cleanupPrefix, cleanupStrip, "mine.txt")

			require.NotContains(t, final, "Remove the Entire-Checkpoint trailer", "git's cleanup must drop Entire's explanation: %q", final)
			stamped := trailers.ParseAllCheckpointsFromFinalTrailerBlock(final, "")
			require.Len(t, stamped, 1, "the recorded commit must end with its trailer: %q", final)
			require.True(t, advCheckpointStored(env, stamped[0].String()), "post-commit condenses into the stamped checkpoint")
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
			require.NotEmpty(t, trailers.ParseAllCheckpointsFromFinalTrailerBlock(headMsg, ""), "precondition: HEAD is linked: %q", headMsg)

			amendMsg := strings.TrimRight(headMsg, "\n") + "\n\n" + gitCommentBlock(tt.blockPrefix)
			_, prepared := runPrepareCommitMsg(env, amendMsg, "commit")
			require.Equal(t, amendMsg, prepared, "the amend keeps the commit's own trailer and adds none")
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
		"", "#", cleanupStrip, "mine.txt")

	stamped := requireFreshFinalTrailer(t, final, advForeignCheckpoint)
	require.True(t, advCheckpointStored(env, stamped), "the session's work belongs in the checkpoint prepare stamped")
	require.False(t, advCheckpointStored(env, advForeignCheckpoint),
		"a body line must not claim a checkpoint ID this clone does not have")
}

// commit.cleanup=scissors and whitespace keep comment lines, so the recorded
// commit still carries Entire's explanation (whitespace keeps git's comment
// block too). The trailer must still link the commit, by git's grammar and for
// post-commit, also when core.commentChar=auto picks another prefix because the
// message has a "#" line.
func TestTrailerAuthority_CleanupKeepingCommentsLinksTheCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		commentChar string
		cleanup     string
		source      string
		gitPrefix   string // the prefix git writes its block with
		message     string // as git hands it to prepare-commit-msg
	}{
		{name: "scissors", cleanup: cleanupScissors, gitPrefix: "#", message: "\n" + gitScissorsBlock("#")},
		{name: "scissors with a configured prefix", commentChar: ";", cleanup: cleanupScissors, gitPrefix: ";", message: "\n" + gitScissorsBlock(";")},
		{
			// The template's "#" heading makes git pick ";".
			name: "scissors with auto", commentChar: "auto", cleanup: cleanupScissors, source: "template", gitPrefix: ";",
			message: "# Release notes\n\n" + gitScissorsBlock(";"),
		},
		{name: "whitespace", cleanup: cleanupWhitespace, gitPrefix: "#", message: "\n" + gitCommentBlock("#")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			setRepoGitConfig(env, "commit.cleanup", tt.cleanup)
			if tt.commentChar != "" {
				setRepoGitConfig(env, "core.commentChar", tt.commentChar)
			}
			authorityAgentTurn(env, "mine.txt", "my agent work\n")

			final := editorCommit(env, tt.message, tt.source, tt.gitPrefix, tt.cleanup, "mine.txt")

			stamped := trailers.ParseAllCheckpoints(final)
			require.Len(t, stamped, 1, "precondition: prepare stamped one trailer: %q", final)
			parse := exec.CommandContext(t.Context(), "git", "interpret-trailers", "--parse")
			parse.Dir = env.RepoDir
			parse.Env = testutil.GitIsolatedEnv()
			parse.Stdin = strings.NewReader(final)
			gitTrailers, err := parse.Output()
			require.NoError(t, err)
			require.Contains(t, string(gitTrailers), stamped[0].String(), "precondition: git reads the trailer: %q", final)
			require.True(t, advCheckpointStored(env, stamped[0].String()),
				"post-commit must condense into the checkpoint the commit's trailer names: %q", final)
		})
	}
}

// Typing the subject on the template's second blank line must not run it into
// the trailer: git reads a trailer in the subject paragraph as no trailer, and
// post-commit would never condense into it.
func TestTrailerAuthority_SubjectOnTheTemplatesSecondLineLinks(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	env.GitAdd("mine.txt")
	_, prepared := runPrepareCommitMsg(env, "\n"+gitCommentBlock("#"), "")
	lines := strings.Split(prepared, "\n")
	require.Equal(t, []string{"", ""}, lines[:2], "precondition: the template opens with two blank lines: %q", prepared)
	lines[1] = "typed on the second line"

	final := recordEditorCommit(env, strings.Join(lines, "\n"), "#", cleanupStrip)

	stamped := trailers.ParseAllCheckpointsFromFinalTrailerBlock(final, "")
	require.Len(t, stamped, 1, "the recorded commit must end with its trailer: %q", final)
	require.True(t, advCheckpointStored(env, stamped[0].String()), "post-commit condenses into the stamped checkpoint")
}

// A trailer prepare adds to an amend or an agent's editor commit goes above
// git's comment block: with `commit -v` everything below the scissors line is
// the diff, which git discards with the trailer in it.
func TestTrailerAuthority_AddedTrailerSurvivesVerboseCleanup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(env *TestEnv) (prepared string, notID string)
	}{
		{
			name: "amend restores the session's last checkpoint",
			prepare: func(env *TestEnv) (string, string) {
				authorityAgentTurn(env, "mine.txt", "my agent work\n")
				env.GitCommitWithHooks("My change", "mine.txt")
				_, prepared := runPrepareCommitMsg(env, "My change\n\n"+gitCommentBlock("#")+gitVerboseTail, "commit")
				return prepared, ""
			},
		},
		{
			name: "amend of a deleted checkpoint gets a fresh one",
			prepare: func(env *TestEnv) (string, string) {
				deleted := createCheckpointedCommit(env.T, env, "Add beta", "beta.go", "package beta\n", "Add beta")
				env.RunCLI("checkpoint", "delete", deleted, "--force")
				authorityAgentTurn(env, "beta2.go", "package beta\n")
				env.GitAdd("beta2.go")
				headMsg := strings.TrimRight(env.GetCommitMessage(env.GetHeadHash()), "\n")
				_, prepared := runPrepareCommitMsg(env, headMsg+"\n\n"+gitCommentBlock("#")+gitVerboseTail, "commit")
				return prepared, deleted
			},
		},
		{
			name: "agent commit through the editor",
			prepare: func(env *TestEnv) (string, string) {
				sess := env.NewSession()
				require.NoError(env.T, env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath))
				env.WriteFile("agent.txt", "agent work\n")
				sess.CreateTranscript("write agent.txt", []FileChange{{Path: "agent.txt", Content: "agent work\n"}})
				env.GitAdd("agent.txt")
				msgFile := env.commitMsgFile()
				require.NoError(env.T, os.WriteFile(msgFile, []byte("Agent change\n\n"+gitCommentBlock("#")+gitVerboseTail), 0o644))
				if out, err := env.prepareCommitMsgCmd(false, msgFile).CombinedOutput(); err != nil {
					env.T.Logf("prepare-commit-msg output: %s", out)
				}
				prepared, err := os.ReadFile(msgFile)
				require.NoError(env.T, err)
				return string(prepared), ""
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)

			prepared, notID := tt.prepare(env)

			kept := trailers.ParseAllCheckpointsFromFinalTrailerBlock(trailers.StripEditorComments(prepared, "#"), "#")
			require.Len(t, kept, 1, "the added trailer must survive git's cleanup: %q", prepared)
			if notID != "" {
				require.NotEqual(t, notID, kept[0].String(), "never the deleted checkpoint")
			}
		})
	}
}

// The user clears a restored amend's message to abort it. Git's comment block
// still ends the message, so commit-msg must remove the trailer and let git
// abort, not record a commit holding only the trailer.
func TestTrailerAuthority_ClearedAmendMessageAborts(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	env.GitCommitWithHooks("My change", "mine.txt")
	msgFile, prepared := runPrepareCommitMsg(env, "My change\n\n"+gitCommentBlock("#"), "commit")
	require.NotEmpty(t, trailers.ParseAllCheckpoints(prepared), "precondition: the amend restored a trailer: %q", prepared)
	require.NoError(t, os.WriteFile(msgFile, []byte(strings.Replace(prepared, "My change\n", "", 1)), 0o644))

	edited := runCommitMsgHook(env, msgFile)

	require.Empty(t, trailers.ParseAllCheckpoints(edited),
		"commit-msg must remove the trailer so git aborts the empty amend: %q", edited)
}

// commit-msg removes Entire's trailer from an editor message the user left
// empty, so git aborts the commit. It must judge "empty" the way git's cleanup
// will: with the repository's comment prefix, and ignoring the `commit -v`
// scissors line and the diff below it.
func TestTrailerAuthority_UntouchedEditorMessageAborts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		commentChar string
		message     string // as git hands it to prepare-commit-msg
	}{
		// GUARD: the default prefix.
		{name: "default prefix", message: "\n" + gitCommentBlock("#")},
		{name: "semicolon", commentChar: ";", message: "\n" + gitCommentBlock(";")},
		{name: "verbose", message: "\n" + gitCommentBlock("#") + gitVerboseTail},
		// GUARD: commit.status=false hands prepare-commit-msg an empty message, so
		// only Entire's own explanation marks it as an editor message.
		{name: "no comment block", message: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := NewFeatureBranchEnv(t)
			if tt.commentChar != "" {
				setRepoGitConfig(env, "core.commentChar", tt.commentChar)
			}
			authorityAgentTurn(env, "mine.txt", "my agent work\n")
			env.GitAdd("mine.txt")
			msgFile, prepared := runPrepareCommitMsg(env, tt.message, "")
			require.NotEmpty(t, trailers.ParseAllCheckpoints(prepared), "precondition: prepare stamped a trailer: %q", prepared)

			// The user saves without typing anything.
			edited := runCommitMsgHook(env, msgFile)

			require.Empty(t, trailers.ParseAllCheckpoints(edited),
				"commit-msg must remove the trailer so git aborts the empty commit: %q", edited)
		})
	}
}

// A -m message keeps every line, so "#42 fix login" is the commit's subject,
// not a comment: commit-msg must leave the commit's trailer in.
func TestTrailerAuthority_MessageSubjectStartingWithHashKeepsTrailer(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	authorityAgentTurn(env, "mine.txt", "my agent work\n")
	env.GitAdd("mine.txt")
	msgFile, prepared := runPrepareCommitMsg(env, "#42 fix login\n", "message")
	stamped := trailers.ParseAllCheckpointsFromFinalTrailerBlock(prepared, "")
	require.Len(t, stamped, 1, "precondition: prepare stamped a trailer: %q", prepared)

	edited := runCommitMsgHook(env, msgFile)

	require.Equal(t, stamped, trailers.ParseAllCheckpointsFromFinalTrailerBlock(edited, ""),
		"commit-msg must keep the trailer of a message with content: %q", edited)
}

// runCommitMsgHook runs the real commit-msg hook on msgFile, as git does after
// the editor closes, and returns the message it leaves for git to record.
func runCommitMsgHook(env *TestEnv, msgFile string) string {
	env.T.Helper()
	cmd := exec.CommandContext(env.T.Context(), getTestBinary(), "hooks", "git", "commit-msg", msgFile)
	cmd.Dir = env.RepoDir
	cmd.Env = env.gitHookEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		env.T.Logf("commit-msg output: %s", out)
	}
	edited, err := os.ReadFile(msgFile)
	require.NoError(env.T, err)
	return string(edited)
}

// runPrepareCommitMsg runs the real prepare-commit-msg hook, as a human at a
// TTY, on message written to the commit message file. source "" is a plain
// `git commit`. Returns the file and the message the hook leaves in it.
func runPrepareCommitMsg(env *TestEnv, message, source string) (string, string) {
	env.T.Helper()
	msgFile := env.commitMsgFile()
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
	return msgFile, string(prepared)
}

// gitCommentBlock is the comment block git appends to a message it opens in
// the editor, written with prefix.
func gitCommentBlock(prefix string) string {
	return prefix + " Please enter the commit message for your changes. Lines starting\n" +
		prefix + " with '" + prefix + "' will be ignored, and an empty message aborts the commit.\n" +
		prefix + "\n" +
		prefix + " On branch feature/test-branch\n"
}

// gitScissorsBlock is the block git appends to a message it opens in the
// editor under commit.cleanup=scissors, written with prefix.
func gitScissorsBlock(prefix string) string {
	return prefix + " " + scissorsLine + "\n" +
		prefix + " Do not modify or remove the line above.\n" +
		prefix + " Everything below it will be ignored.\n" +
		prefix + "\n" +
		prefix + " On branch feature/test-branch\n"
}

// gitVerboseTail is what `commit -v` adds below git's comment block: the
// scissors line and the diff, which git discards.
const gitVerboseTail = "# " + scissorsLine + "\n" +
	"# Do not modify or remove the line above.\n" +
	"# Everything below it will be ignored.\n" +
	"diff --git a/mine.txt b/mine.txt\n" +
	"+my agent work\n"

const scissorsLine = "------------------------ >8 ------------------------"

// git's commit.cleanup modes for an editor message.
const (
	cleanupStrip      = "strip"      // drops comment lines (the default)
	cleanupScissors   = "scissors"   // drops the scissors line and below, keeps comment lines
	cleanupWhitespace = "whitespace" // keeps comment lines
)

// editorCommit runs one editor-flow commit: prepare-commit-msg gets message
// (source "" is a plain `git commit`), the user types a subject on an empty
// first line, and git's own cleanup (cleanupMode, with commentPrefix) produces
// what is recorded before post-commit runs. Returns the recorded message.
func editorCommit(env *TestEnv, message, source, commentPrefix, cleanupMode string, files ...string) string {
	env.T.Helper()
	env.GitAdd(files...)
	_, edited := runPrepareCommitMsg(env, message, source)
	if strings.HasPrefix(edited, "\n") {
		edited = "typed in editor" + edited
	}
	return recordEditorCommit(env, edited, commentPrefix, cleanupMode)
}

// recordEditorCommit applies git's cleanup (cleanupMode, with commentPrefix) to
// the message the user saved in the editor, records the commit, and runs
// post-commit. Returns the recorded message.
func recordEditorCommit(env *TestEnv, edited, commentPrefix, cleanupMode string) string {
	env.T.Helper()
	args := []string{"-c", "core.commentChar=" + commentPrefix, "stripspace"}
	switch cleanupMode {
	case cleanupStrip:
		args = append(args, "--strip-comments")
	case cleanupScissors:
		if before, _, found := strings.Cut(edited, "\n"+commentPrefix+" "+scissorsLine+"\n"); found {
			edited = before + "\n"
		}
	}
	cleanup := exec.CommandContext(env.T.Context(), "git", args...)
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
	stamped := trailers.ParseAllCheckpointsFromFinalTrailerBlock(message, "")
	require.Len(t, stamped, 1, "prepare-commit-msg must stamp a trailer in the final block: %q", message)
	require.NotEqual(t, mentioned, stamped[0].String(), "the stamped trailer must not reuse the body's ID: %q", message)
	return stamped[0].String()
}
