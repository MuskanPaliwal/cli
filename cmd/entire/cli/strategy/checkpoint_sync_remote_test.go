package strategy

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// currentBranchName returns the short name of the current branch in repoDir.
func currentBranchName(t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// setGitConfig sets a git config key to value in repoDir.
func setGitConfig(t *testing.T, repoDir, key, value string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "config", key, value)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSetting(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "private", "https://example.com/private.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "private")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "private", Source: SyncRemoteSourceConfig}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSettingMissingRemote_FailsClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gone")
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_DefaultsToOrigin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_SoleRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "upstream", Source: SyncRemoteSourceSole}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_FirstInConfigOrder(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// No "origin" remote. Add zeta before alpha; config-file order should win
	// over alphabetical order.
	testutil.AddRemote(t, tmpDir, "zeta", "https://example.com/zeta.git")
	testutil.AddRemote(t, tmpDir, "alpha", "https://example.com/alpha.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "zeta", Source: SyncRemoteSourceFirst}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_SettingsLoadErrorFailsClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

	// Corrupt settings.json: the file may contain a checkpoint_push_remote
	// we cannot read, so election must not proceed.
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte("{not valid json"), 0o644))

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot read settings")
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_NoRemotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_PushurlOnlyRemoteIsInvisible(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// A remote configured with only a pushurl (no url) added first. If it
	// were counted, it would sort first in .git/config order and get elected.
	cmd := exec.CommandContext(ctx, "git", "config", "remote.pushonly.pushurl", "https://example.com/pushonly.git")
	cmd.Dir = tmpDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Two real remotes added after it, no "origin" — this keeps the visible
	// remote count at 2 so the resolver exercises the "first" precedence
	// path (not "sole"), proving the pushurl-only entry is excluded from
	// both the count and the ordering.
	testutil.AddRemote(t, tmpDir, "first-real", "https://example.com/first.git")
	testutil.AddRemote(t, tmpDir, "second-real", "https://example.com/second.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "first-real", Source: SyncRemoteSourceFirst}, got)
}

// Not parallel: uses t.Chdir()
// Regression guard for the tracking tier that was removed before merge: the
// branch's tracking config must NOT decide the election.
//
// Election is compared against the remote of the push being made, so electing
// the tracking remote turns every push to a different remote into a silent
// no-op — the failure TestAlternates_RelativeObjectAlternate_CheckpointSync
// caught (clone with `-o base`, push checkpoints to a separately added
// origin). It also elects a remote the read paths cannot see, since resume and
// explain resolve checkpoints through origin's remote-tracking refs.
//
// The fork setup this tier was meant to serve (origin unpushable, push to your
// own fork) is served explicitly by checkpoint_push_remote, covered above.
func TestResolveCheckpointSyncRemote_TrackingConfigDoesNotDecide(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		keys map[string]string
	}{
		{"branch.<name>.remote", map[string]string{"branch.%s.remote": "upstream"}},
		{"remote.pushDefault", map[string]string{"remote.pushDefault": "upstream"}},
		{"branch.<name>.pushRemote", map[string]string{"branch.%s.pushRemote": "upstream"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			testutil.InitRepo(t, tmpDir)
			testutil.WriteFile(t, tmpDir, "f.txt", "init")
			testutil.GitAdd(t, tmpDir, "f.txt")
			testutil.GitCommit(t, tmpDir, "init")

			testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
			testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

			branch := currentBranchName(t, tmpDir)
			for key, val := range tt.keys {
				if strings.Contains(key, "%s") {
					key = fmt.Sprintf(key, branch)
				}
				setGitConfig(t, tmpDir, key, val)
			}

			t.Chdir(tmpDir)

			got, err := ResolveCheckpointSyncRemote(ctx)
			require.NoError(t, err)
			assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got,
				"tracking config must not outrank origin")
		})
	}
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSettingBeatsTracking(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.AddRemote(t, tmpDir, "private", "https://example.com/private.git")

	branch := currentBranchName(t, tmpDir)
	setGitConfig(t, tmpDir, "branch."+branch+".remote", "upstream")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "private")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "private", Source: SyncRemoteSourceConfig}, got)
}

// Not parallel: uses t.Chdir()
func TestCheckpointSyncAllowedForRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("no setting: allowed only for the elected default remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

		t.Chdir(tmpDir)

		assert.True(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("misconfigured setting fails closed for every remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")
		testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("unreadable settings fails closed for every remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

		// Corrupt settings.json, not a misconfigured setting: the gate must
		// fail closed here too, not just when the resolver itself detects a
		// bad checkpoint_push_remote value.
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte("{not valid json"), 0o644))

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("raw URL push argument is never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "https://github.com/o/r.git"))
	})

	t.Run("no remotes configured: never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
	})
}

// newCaptureTestRepo builds a repo with one commit and an origin+fork remote
// pair — the fork topology: origin wins the default election, fork is where
// the user's branches actually push.
// gitInRepo runs a git subcommand in repoDir with isolated config, for setup the
// testutil helpers do not cover (remote rename).
func gitInRepo(t *testing.T, repoDir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func newCaptureTestRepo(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "fork", "https://example.com/fork.git")
	return tmpDir
}

// captureStderrWriter redirects the strategy package's user-facing stderr into
// a buffer for the duration of the test.
func captureStderrWriter(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })
	return &buf
}

// Regression: the default election guesses from config at rest (origin bias),
// so a user whose branches push a non-origin remote had checkpoints strand
// locally until they hand-configured checkpoint_push_remote (first user report
// the day after v0.10.0 shipped ENT-1451). A captured election — written when
// an actual push agrees with the branch's declared push destination — must
// outrank origin, while an explicit setting still outranks the capture.
//
// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_CapturedTier(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("captured remote beats origin", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceObserved}, got)
	})

	t.Run("explicit checkpoint_push_remote beats captured", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "origin")
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceConfig}, got)
	})

	t.Run("captured remote no longer configured falls through to origin", func(t *testing.T) {
		// Fail-soft, unlike the fail-closed explicit setting: capture is
		// automatic, so a renamed/removed remote must not disable sync.
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "gone"))

		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
	})

	t.Run("corrupt capture state falls through to origin", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		path, err := capturedSyncRemotesPath(ctx)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

		got, resolveErr := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, resolveErr)
		assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
	})
}

// Not parallel: uses t.Chdir()
func TestMaybeCaptureCheckpointSyncRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("push agreeing with the branch's declared destination captures and announces", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		maybeCaptureCheckpointSyncRemote(ctx, "fork")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx), "capture must persist the remote")
		assert.Contains(t, buf.String(), `"fork"`, "capture must announce itself")
		assert.Contains(t, buf.String(), "checkpoint_push_remote", "announcement must name the override")
		got, err := ResolveCheckpointSyncRemote(ctx)
		require.NoError(t, err)
		assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceObserved}, got)
	})

	t.Run("push to the already-elected remote does not capture", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "origin")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		maybeCaptureCheckpointSyncRemote(ctx, "origin")

		assert.Empty(t, loadCapturedSyncRemotes(ctx), "the seed election needs no capture; persisting it would block a later real capture")
		assert.Empty(t, buf.String())
	})

	t.Run("one-off push to a non-declared remote does not capture", func(t *testing.T) {
		// The upstream-PR push: behavior without declaration is not consent.
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "origin")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		maybeCaptureCheckpointSyncRemote(ctx, "fork")

		assert.Empty(t, loadCapturedSyncRemotes(ctx))
		assert.Empty(t, buf.String())
	})

	t.Run("existing capture sticks", func(t *testing.T) {
		// Phase-1 no-ping-pong rule: a mixed-habit repo (branches pushing two
		// remotes) must not flip the election back and forth on every push.
		dir := newCaptureTestRepo(t)
		testutil.AddRemote(t, dir, "work", "https://example.com/work.git")
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "work")
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))
		buf := captureStderrWriter(t)

		maybeCaptureCheckpointSyncRemote(ctx, "work")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx), "first capture sticks until config or phase 2")
		assert.Empty(t, buf.String())
	})

	t.Run("a dead capture does not block a fresh one", func(t *testing.T) {
		// The sticky rule asks whether a capture is IN FORCE, not whether the file
		// is non-empty. Gating on presence stranded the election: after
		// `git remote rename fork myfork` the resolver skipped the dead entry and
		// fell back to origin, while capture refused to ever elect myfork — so for
		// a user who only pushes to myfork every push was gated and checkpoints
		// reached nowhere, recoverable only by deleting the state file by hand.
		dir := newCaptureTestRepo(t)
		t.Chdir(dir)
		require.NoError(t, saveCapturedSyncRemote(ctx, "fork"))
		// The rename is what kills the captured entry: fork stops existing, the
		// state file still names it, and myfork becomes the declared destination.
		gitInRepo(t, dir, "remote", "rename", "fork", "myfork")
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "myfork")
		buf := captureStderrWriter(t)

		maybeCaptureCheckpointSyncRemote(ctx, "myfork")

		assert.Equal(t, []string{"myfork"}, loadCapturedSyncRemotes(ctx),
			"a captured remote that no longer exists must not veto the next capture")
		assert.Contains(t, buf.String(), `"myfork"`, "the new election must be announced")
	})

	t.Run("explicit checkpoint_push_remote disables capture", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		testutil.WriteCheckpointPushRemoteSetting(t, dir, "origin")
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		maybeCaptureCheckpointSyncRemote(ctx, "fork")

		assert.Empty(t, loadCapturedSyncRemotes(ctx), "an explicit setting is a decision already made")
		assert.Empty(t, buf.String())
	})

	t.Run("branch pushRemote outranks branch remote in the declaration", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		branch := currentBranchName(t, dir)
		setGitConfig(t, dir, "branch."+branch+".remote", "origin")
		setGitConfig(t, dir, "branch."+branch+".pushRemote", "fork")
		t.Chdir(dir)

		maybeCaptureCheckpointSyncRemote(ctx, "fork")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx), "pushRemote is git's own push-resolution winner")
	})

	t.Run("remote.pushDefault declares when the branch has no tracking", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "remote.pushDefault", "fork")
		t.Chdir(dir)

		maybeCaptureCheckpointSyncRemote(ctx, "fork")

		assert.Equal(t, []string{"fork"}, loadCapturedSyncRemotes(ctx))
	})

	t.Run("raw URL push never captures", func(t *testing.T) {
		dir := newCaptureTestRepo(t)
		setGitConfig(t, dir, "branch."+currentBranchName(t, dir)+".remote", "fork")
		t.Chdir(dir)
		buf := captureStderrWriter(t)

		maybeCaptureCheckpointSyncRemote(ctx, "https://example.com/elsewhere.git")

		assert.Empty(t, loadCapturedSyncRemotes(ctx))
		assert.Empty(t, buf.String())
	})
}
