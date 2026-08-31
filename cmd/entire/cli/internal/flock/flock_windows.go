//go:build windows

package flock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// pollInterval is how often the bounded AcquireContext path retries a
// non-blocking lock while waiting for a deadline.
const pollInterval = 25 * time.Millisecond

// Acquire takes an exclusive lock on path via Windows LockFileEx. The
// returned release unlocks and closes the file. Callers must invoke release
// exactly once. Acquire blocks indefinitely until the lock is available; use
// AcquireContext with a deadline to bound the wait.
func Acquire(path string) (release func(), err error) {
	return AcquireContext(context.Background(), path)
}

// AcquireContext behaves like Acquire but honors ctx. When ctx carries a
// deadline it polls a fail-immediately lock until acquired or the deadline
// fires; otherwise it blocks like Acquire. See the unix implementation for the
// rationale.
func AcquireContext(ctx context.Context, path string) (release func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // caller is responsible for path validation
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}
	return lockFile(ctx, f)
}

// AcquireIn is Acquire for a lock file named inside root. It is the form the
// .git-resident locks use: their names carry agent-supplied session IDs, so
// resolving them through the git common dir's root keeps a name that escaped
// validation from naming a file outside the clone.
func AcquireIn(root *os.Root, name string) (release func(), err error) {
	return AcquireContextIn(context.Background(), root, name)
}

// AcquireContextIn is AcquireContext for a lock file named inside root.
func AcquireContextIn(ctx context.Context, root *os.Root, name string) (release func(), err error) {
	f, err := openLockFileIn(root, name)
	if err != nil {
		return nil, fmt.Errorf("open flock: %w", err)
	}
	return lockFile(ctx, f)
}

// openLockFileIn opens the lock file, creating it if needed, without ever
// asking for O_CREATE *without* O_EXCL.
//
// That combination is the one operation macOS gets wrong: when several callers
// race the first creation of a single name through openat(2), most of them get
// a spurious ENOENT instead of a descriptor — measured at 25-75% under
// contention, on macOS 26.6 (25G72) and 26.6.2 (25G83), reproducible in plain C
// with no Go involved. Nothing else races: O_CREATE|O_EXCL, a plain open of an
// existing file, mkdirat/symlinkat/linkat/renameat, and full-path open(2) are
// all correct, and Linux is unaffected entirely. Only openat's create-or-open
// fallback is broken.
//
// This matters here more than anywhere else in the tree, because a lock nobody
// can take is a lock that silently stops serializing: callers saw
// "acquire state lock: ... no such file or directory" and concurrent session
// state merges were lost. The pre-os.Root code used a full path, so this is a
// regression the root migration would otherwise introduce on the platform we
// develop on.
//
// Splitting create-or-open into an exclusive create plus a plain open avoids
// the broken path entirely, and is deterministic rather than a retry: measured
// 0 failures in 6000 racing opens where the single-call form failed 4806 times.
// The loop covers only the vanishingly rare case of the file being removed
// between the two steps; lock files are never unlinked (see ClearSessionState),
// so in practice it runs once.
func openLockFileIn(root *os.Root, name string) (*os.File, error) {
	var err error
	for range 3 {
		var f *os.File
		f, err = root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err //nolint:wrapcheck // AcquireContextIn wraps this; wrapping twice would bury the cause
		}
		// It exists now, so open it without O_CREATE.
		f, err = root.OpenFile(name, os.O_RDWR, 0)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err //nolint:wrapcheck // AcquireContextIn wraps this; wrapping twice would bury the cause
		}
		// Removed between the two steps; start over.
	}
	return nil, err //nolint:wrapcheck // AcquireContextIn wraps this; wrapping twice would bury the cause
}

// lockFile holds the locking logic shared by the path- and root-based entry
// points, which differ only in how the file was opened.
func lockFile(ctx context.Context, f *os.File) (release func(), err error) {
	overlapped := new(windows.Overlapped)
	releaseFn := func() {
		_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
		_ = f.Close()
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, overlapped); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", err)
		}
		return releaseFn, nil
	}

	for {
		lockErr := windows.LockFileEx(windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
		if lockErr == nil {
			return releaseFn, nil
		}
		// Only lock contention is retryable. LOCKFILE_FAIL_IMMEDIATELY reports a
		// held lock as ERROR_LOCK_VIOLATION (or ERROR_IO_PENDING); any other error
		// is a genuine failure (I/O, bad handle) that must fail fast rather than
		// polling until the deadline and masking the real cause as a timeout —
		// mirroring the unix path, which only retries on EWOULDBLOCK.
		if !errors.Is(lockErr, windows.ERROR_LOCK_VIOLATION) && !errors.Is(lockErr, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", lockErr)
		}
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("lock flock: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
