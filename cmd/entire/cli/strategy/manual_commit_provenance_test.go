package strategy

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func TestStampedTrailer_SkipsInheritedTrailers(t *testing.T) {
	t.Parallel()
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J30")
	fresh := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J31")
	msg := "Squashed\n\nEntire-Checkpoint: " + inherited.String() + "\nEntire-Checkpoint: " + fresh.String() + "\n"

	got, ok := stampedTrailer(context.Background(), msg, commentRules{}, []id.CheckpointID{inherited})
	assert.True(t, ok)
	assert.Equal(t, fresh, got)

	_, ok = stampedTrailer(context.Background(), "Squashed\n\nEntire-Checkpoint: "+inherited.String()+"\n", commentRules{}, []id.CheckpointID{inherited})
	assert.False(t, ok, "an inherited trailer alone is not a stamp")

	got, ok = stampedTrailer(context.Background(), msg, commentRules{}, nil)
	assert.True(t, ok)
	assert.Equal(t, fresh, got, "with nothing inherited the last trailer is the stamp, as post-commit picks it")
}

func TestPickCondensationTarget(t *testing.T) {
	t.Parallel()
	a, b, c := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3A"), id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3B"), id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3C")
	existing := map[id.CheckpointID]bool{a: true, b: true}
	exists := func(cpID id.CheckpointID) bool { return existing[cpID] }

	got, ok := pickCondensationTarget([]id.CheckpointID{a}, nil)
	assert.True(t, ok)
	assert.Equal(t, a, got, "a lone trailer is the target without consulting the store")

	got, ok = pickCondensationTarget([]id.CheckpointID{b, a, c}, exists)
	assert.True(t, ok)
	assert.Equal(t, c, got, "the stamped trailer is the one without a checkpoint")

	_, ok = pickCondensationTarget([]id.CheckpointID{b, a}, exists)
	assert.False(t, ok, "only links: nothing to condense into")

	_, ok = pickCondensationTarget(nil, exists)
	assert.False(t, ok)
}

func TestPickCondensationTargetState_RechecksSelectedTarget(t *testing.T) {
	t.Parallel()
	linked := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3A")
	target := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3B")
	targetChecks := 0
	exists := func(cpID id.CheckpointID) bool {
		if cpID == linked {
			return true
		}
		targetChecks++
		return targetChecks > 1
	}

	got, preexisting, found := pickCondensationTargetState([]id.CheckpointID{linked, target}, exists)
	assert.True(t, found)
	assert.Equal(t, target, got)
	assert.True(t, preexisting,
		"a target created after selection must be treated as preexisting before condensation")
}

// Amending a squash after its first post-commit: every trailer now has a
// checkpoint and the inherited-trailer marker is gone. The last trailer, where
// prepare stamps, is the target, preexisting, so only the session amending it
// may write there (stampedByAnotherCommit).
func TestPickCondensationTargetState_AllExistingFallsBackToTheStamp(t *testing.T) {
	t.Parallel()
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3C")
	stamp := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J3D")
	got, preexisting, found := pickCondensationTargetState([]id.CheckpointID{inherited, stamp}, func(id.CheckpointID) bool { return true })
	assert.True(t, found)
	assert.Equal(t, stamp, got)
	assert.True(t, preexisting)
}

// The inherited-trailer marker speaks only for a commit on the parent it was
// recorded against, and is consumed by the first post-commit that reads it.
func TestInheritedTrailersMarker_TiedToParentAndConsumed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	parent := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J50")
	ctx := context.Background()

	recordInheritedTrailers(ctx, []id.CheckpointID{inherited})
	require.Empty(t, takeInheritedTrailers(ctx, "0000000000000000000000000000000000000000"), "a commit on another parent is not the one prepared")
	require.Empty(t, takeInheritedTrailers(ctx, parent), "the marker is consumed by the first reader")

	recordInheritedTrailers(ctx, []id.CheckpointID{inherited})
	require.Equal(t, map[id.CheckpointID]bool{inherited: true}, takeInheritedTrailers(ctx, parent))

	recordInheritedTrailers(ctx, []id.CheckpointID{inherited})
	recordInheritedTrailers(ctx, nil)
	require.Empty(t, takeInheritedTrailers(ctx, parent), "a later prepare that inherited nothing clears the marker")
}

// Git discards everything below the scissors line of a `commit -v` message; an
// inherited trailer must land above git's comment block to survive.
func TestAddInheritedCheckpointTrailer_StaysAboveGitComments(t *testing.T) {
	t.Parallel()
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J51")
	msg := "Subject\n\n# Please enter the commit message.\n# ------------------------ >8 ------------------------\ndiff --git a/f b/f\n"
	got := addCheckpointTrailerAboveComments(msg, inherited, editorRules("#"))
	trailerAt := strings.Index(got, "Entire-Checkpoint: "+inherited.String())
	require.GreaterOrEqual(t, trailerAt, 0, "%q", got)
	require.Less(t, trailerAt, strings.Index(got, "# Please enter"), "%q", got)
	require.True(t, strings.HasSuffix(got, "# ------------------------ >8 ------------------------\ndiff --git a/f b/f\n"), "git's block is kept intact: %q", got)

	require.Equal(t, addCheckpointTrailer("Subject\n", inherited), addCheckpointTrailerAboveComments("Subject\n", inherited, editorRules("#")),
		"a message without git comments is unchanged in behaviour")

	// With another comment prefix, "#" lines are content and git's block starts
	// at the first line with that prefix.
	semicolon := "# Release notes\n\n; Please enter the commit message.\n; ------------------------ >8 ------------------------\n"
	got = addCheckpointTrailerAboveComments(semicolon, inherited, editorRules(";"))
	trailerAt = strings.Index(got, "Entire-Checkpoint: "+inherited.String())
	require.Greater(t, trailerAt, strings.Index(got, "# Release notes"), "%q", got)
	require.Less(t, trailerAt, strings.Index(got, "; Please enter"), "%q", got)
}

// An amend that inherits nothing clears a marker an aborted amend left, so the
// next commit on the same parent is not mistaken for the one prepared.
func TestRecordInheritedTrailersOnAmend_EmptyClearsStaleMarker(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, "f.txt", "x\n")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "second")
	t.Chdir(dir)
	parent := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD~1"))
	ctx := context.Background()

	recordInheritedTrailers(ctx, []id.CheckpointID{id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J52")})
	recordInheritedTrailers(ctx, nil)
	require.Empty(t, takeInheritedTrailers(ctx, parent))
}

// git reports source "message" for `commit --amend -m` and "commit" for
// `commit -C <rev>`, so the source word does not say whether the commit will
// sit on HEAD or on HEAD's parent. The marker must be found either way, or
// post-commit stops filtering inherited trailers and may condense into one.
func TestInheritedTrailersMarker_FoundOnEitherParent(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := resolvedTempDir(t)
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "README.md", "base\n")
	testutil.GitAdd(t, dir, "README.md")
	testutil.GitCommit(t, dir, "init")
	testutil.WriteFile(t, dir, "f.txt", "x\n")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "second")
	t.Chdir(dir)
	head := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD"))
	parent := strings.TrimSpace(testutil.RunGit(t, dir, "rev-parse", "HEAD~1"))
	ctx := context.Background()
	cpID := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J53")

	// `commit --amend -m`: prepared as an ordinary commit, lands on HEAD's parent.
	recordInheritedTrailers(ctx, []id.CheckpointID{cpID})
	require.True(t, takeInheritedTrailers(ctx, parent)[cpID], "amend with -m")

	// `commit -C <rev>`: prepared as an amend, lands on HEAD.
	recordInheritedTrailers(ctx, []id.CheckpointID{cpID})
	require.True(t, takeInheritedTrailers(ctx, head)[cpID], "commit -C")

	// Any other parent is a commit the marker was not prepared for.
	recordInheritedTrailers(ctx, []id.CheckpointID{cpID})
	require.Empty(t, takeInheritedTrailers(ctx, strings.Repeat("0", 40)))
}

// Git's comment block is the run of comment lines that ends the message. A
// comment-prefixed line inside it (a body line git will strip) must not pull
// the trailer above the lines after it, or they would follow the trailer and
// it would not end the cleaned message.
func TestAddCheckpointTrailerAboveComments_AnchorsToTheEndingBlock(t *testing.T) {
	t.Parallel()
	cpID := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J55")
	for _, msg := range []string{
		"Subject\n\nIntro\n#42 is fixed\nMore details\n\n# Please enter the commit message.\n#\n",
		"Subject\n\n#42 is fixed\n\nMore details\n\n# Please enter the commit message.\n# ------------------------ >8 ------------------------\ndiff --git a/f b/f\n",
	} {
		got := addCheckpointTrailerAboveComments(msg, cpID, editorRules("#"))
		require.Equal(t, []id.CheckpointID{cpID}, trailers.ParseAllCheckpointsFromFinalTrailerBlock(trailers.StripEditorComments(got, "#"), "#"), "%q", got)
	}
}

// A message git does not open in the editor (-m, --no-edit) has no comment
// block and keeps its `#` lines as written: "#42 fix login" is the subject, and
// "#123" or a "#42 is the issue" paragraph that ends it is the user's, so the
// inherited trailer follows them; git's trailer grammar skips comment lines
// that end a message.
func TestAddInheritedCheckpointTrailer_MessageSourceKeepsHashLines(t *testing.T) {
	t.Parallel()
	inherited := id.CheckpointID("01M2VBJBJQZ2BP1W2PBWDF3J53")

	for _, msg := range []string{"#42 fix login\n", "Fix login\n#123\n", "Fix login\n\n#42 is the issue\n"} {
		got := addCheckpointTrailerAboveComments(msg, inherited, commentRules{prefix: "#", trailerPrefix: "#"})
		require.Equal(t, msg+"\nEntire-Checkpoint: "+inherited.String()+"\n", got, "the user's lines come first")
		require.Equal(t, []id.CheckpointID{inherited}, trailers.ParseAllCheckpointsFromFinalTrailerBlock(got, "#"), "%q", got)
	}
}

// editorRules are the comment rules of a message git opens in the editor,
// under git's default cleanup.
func editorRules(prefix string) commentRules {
	return commentRules{prefix: prefix, trailerPrefix: prefix, edited: true, stripped: true}
}
