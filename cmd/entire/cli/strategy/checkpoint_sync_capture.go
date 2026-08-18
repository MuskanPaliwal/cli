package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// capturedSyncRemotesFileName is the per-clone captured-election state, stored
// in the git common dir (worktree-shared, like the push queue). List-shaped
// from day one: phase 1 caps membership at one remote, and lifting the cap
// (per-remote push-queue tracking) must not need a state migration.
const capturedSyncRemotesFileName = "entire-checkpoint-sync-remotes.json"

// capturedSyncRemotesLockName serializes the read-decide-write in
// maybeCaptureCheckpointSyncRemote. Paired with the state file exactly as
// checkpoint/pushqueue.go pairs its queue and lock in this same directory: the
// atomic write below keeps readers safe, but without the lock two concurrent
// pre-push hooks in different worktrees both observe "nothing captured", both
// write, and last-rename-wins — so each announces a different remote and "first
// capture sticks" becomes a coin flip.
const capturedSyncRemotesLockName = "entire-checkpoint-sync-remotes.lock"

type capturedSyncRemotesFile struct {
	Remotes []string `json:"remotes"`
}

func capturedSyncRemotesPath(ctx context.Context) (string, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, capturedSyncRemotesFileName), nil
}

func capturedSyncRemotesLockPath(ctx context.Context) (string, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, capturedSyncRemotesLockName), nil
}

// loadCapturedSyncRemotes reads the captured election. Fail-soft: a missing,
// unreadable, or corrupt file reads as "nothing captured" — capture is
// automatic state, so unlike the explicit checkpoint_push_remote setting it
// must never fail sync closed.
func loadCapturedSyncRemotes(ctx context.Context) []string {
	path, err := capturedSyncRemotesPath(ctx)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the git common dir resolved from the repo itself, not user input.
	if err != nil {
		return nil
	}
	var f capturedSyncRemotesFile
	if err := json.Unmarshal(data, &f); err != nil {
		logging.Debug(ctx, "captured sync remotes file unreadable; ignoring",
			slog.String("error", err.Error()))
		return nil
	}
	return f.Remotes
}

// saveCapturedSyncRemote persists the one captured remote. Singular on purpose:
// phase 1 caps membership at one, and a plural writer left that cap resting on a
// single call site passing a one-element literal. The on-disk shape stays
// list-shaped, so lifting the cap in phase 2 needs a new writer rather than a
// migration.
func saveCapturedSyncRemote(ctx context.Context, name string) error {
	path, err := capturedSyncRemotesPath(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(capturedSyncRemotesFile{Remotes: []string{name}})
	if err != nil {
		return fmt.Errorf("encode captured sync remotes: %w", err)
	}
	if err := jsonutil.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write captured sync remotes: %w", err)
	}
	return nil
}

// maybeCaptureCheckpointSyncRemote elects pushRemote as the checkpoint sync
// remote when this push is consent-grade evidence that it is the user's own
// remote: the push target agrees with the branch's declared push destination
// (the config a bare `git push` resolves through). Declaration alone was the
// bug that got the tracking tier dropped from the election (74e239a9 — it
// elected remotes that never receive pushes); behavior alone is the pre-
// single-remote transcript leak. Capture acts only on their intersection,
// and announces itself — an election change must never be silent.
//
// Phase-1 rules: at most one captured remote, and the first capture sticks
// (a mixed-habit repo whose branches push two remotes must not flip the
// election on every push). The default-elected seed is displaceable once;
// after that, re-routing takes the explicit setting until the multi-remote
// set ships.
func maybeCaptureCheckpointSyncRemote(ctx context.Context, pushRemote string) {
	if !isConfiguredRemote(ctx, pushRemote) {
		return
	}

	// The whole read-decide-write runs under the lock, not just the write: the
	// decision is what has to be exclusive, or two hooks both see "nothing
	// captured" and race. A lock we cannot take is not a reason to skip capture
	// forever, but it is a reason to skip it this once.
	lockPath, err := capturedSyncRemotesLockPath(ctx)
	if err != nil {
		logging.Debug(ctx, "capture skipped: cannot resolve lock path",
			slog.String("error", err.Error()))
		return
	}
	release, err := flock.Acquire(lockPath)
	if err != nil {
		logging.Debug(ctx, "capture skipped: cannot acquire lock",
			slog.String("error", err.Error()))
		return
	}
	defer release()

	// One election answers every remaining gate, so there is no second copy of
	// its rules to drift: err covers unreadable settings and a fail-closed
	// checkpoint_push_remote, Source covers an explicit override and a capture
	// already in force, and Name covers "already elected, nothing to capture".
	// A separate validity predicate beside the resolver was how the two came to
	// disagree about a dead captured entry in the first place.
	elected, err := ResolveCheckpointSyncRemote(ctx)
	if err != nil {
		return
	}
	switch elected.Source {
	case SyncRemoteSourceConfig, SyncRemoteSourceObserved:
		// An explicit override, or a capture already in force: nothing to displace.
		return
	case SyncRemoteSourceDefault, SyncRemoteSourceSole, SyncRemoteSourceFirst:
		// Exactly the tiers a capture may displace. Enumerated rather than left to
		// a default so `exhaustive` turns a new tier into a decision here instead
		// of silently letting capture override it.
	}
	if elected.Name == pushRemote {
		return
	}
	if declaredPushDestination(ctx) != pushRemote {
		return
	}
	if saveErr := saveCapturedSyncRemote(ctx, pushRemote); saveErr != nil {
		logging.Warn(ctx, "failed to persist captured checkpoint sync remote",
			slog.String("remote", pushRemote),
			slog.String("error", saveErr.Error()))
		return
	}
	// Announced only after the state landed, so the message can never claim a
	// change that was not persisted.
	fmt.Fprintf(stderrWriter,
		"[entire] Checkpoints now sync to %q — the remote your branch pushes to. Override with strategy_options.checkpoint_push_remote in .entire/settings.local.json.\n",
		pushRemote)
	logging.Info(ctx, "checkpoint sync remote captured",
		slog.String("remote", pushRemote),
		slog.String("previously_elected", elected.Name))
}

// declaredPushDestination resolves where a bare `git push` on the current
// branch would go, through git's own precedence: branch.<name>.pushRemote,
// then remote.pushDefault, then branch.<name>.remote. Empty when HEAD is
// detached or nothing is declared.
//
// Phase-1 simplification: the pre-push hook receives only the remote name
// (refspecs are not plumbed through), so the declaration is read from HEAD's
// branch rather than the branches actually being pushed. A miss is
// conservative — no capture happens and the gate behaves as before.
func declaredPushDestination(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "symbolic-ref", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return ""
	}
	if v := gitConfigValue(ctx, "branch."+branch+".pushRemote"); v != "" {
		return v
	}
	if v := gitConfigValue(ctx, "remote.pushDefault"); v != "" {
		return v
	}
	return gitConfigValue(ctx, "branch."+branch+".remote")
}

// gitConfigValue returns a single git config value, or "" when unset or on
// any error.
func gitConfigValue(ctx context.Context, key string) string {
	out, err := exec.CommandContext(ctx, "git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
