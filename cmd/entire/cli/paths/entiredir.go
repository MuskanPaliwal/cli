package paths

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// The three ways validating `.entire` can fail. Each is identified positively
// and carries a different remedy, which is why they are separate sentinels
// rather than one error plus an else branch: callers print the fix, and telling
// someone to reinstall git because their filesystem returned EACCES sends them
// after the wrong thing. A caller matching none of these must offer no remedy
// rather than guess at one.
var (
	// ErrEntireDirNotDirectory reports that `.entire` exists and is not a real
	// directory. The remedy is to inspect and replace the path.
	ErrEntireDirNotDirectory = errors.New("not a directory")

	// ErrEntireDirUnreadable reports that `.entire` could not be inspected at
	// all — a permission failure, an I/O error, a dead mount. Nothing is known
	// about what is at the path. The remedy is ownership, permissions, or the
	// filesystem itself.
	ErrEntireDirUnreadable = errors.New("cannot be inspected")

	// ErrRepositoryUnresolved reports that the worktree root could not be
	// determined for a reason other than there being no repository, so there is
	// no `.entire` path to inspect yet. The remedy is git.
	ErrRepositoryUnresolved = errors.New("cannot determine which repository this directory belongs to")
)

// ValidateEntireDirAt reports whether worktreeRoot's `.entire` is safe to read
// and write through. It is safe when the path is absent (Entire is not enabled
// here yet, or `enable` is about to create it) or is a real directory. Anything
// else is a broken repo and the caller must not touch the path.
//
// The stat is Lstat, not Stat, so a symlink is rejected even when it points at
// a perfectly good directory. `.entire` holds session metadata, transcripts,
// and the settings that decide what gets redacted before it is committed, so a
// path someone else controls the far end of is not a path we write through.
//
// A stat error other than "not exist" is also a failure. It is not evidence
// that the invariant is violated, but neither is it evidence that it holds, and
// the caller's next move is to write there.
func ValidateEntireDirAt(worktreeRoot string) error {
	path := filepath.Join(worktreeRoot, EntireDir)

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("%s %w: %w", path, ErrEntireDirUnreadable, err)
	case info.Mode().IsDir():
		return nil
	}

	return fmt.Errorf("%s is %s, %w", path, describeMode(info.Mode()), ErrEntireDirNotDirectory)
}

// RequireEntireDir validates the current worktree's `.entire`.
//
// Outside a git repository there is no worktree root and so nothing to
// validate, which is not an error: commands that need a repository report its
// absence themselves, with a message about the repository rather than about
// `.entire`. That skip requires git's positive ErrNotARepository verdict.
//
// Every other discovery failure — git missing from PATH, a cancelled context, a
// permission failure, dubious ownership, malformed output — fails closed. Those
// mean "we could not find out", and the consequence of guessing "no repository"
// is not merely a skipped check: settings resolution falls back to a path
// relative to the current directory when the root will not resolve
// (settingsAbsPaths in the settings package), so a guess would read
// ./.entire/settings.json — through the very symlink this exists to reject.
// Refusing to run on a machine whose git is broken is the cheaper mistake.
//
// Deliberately not memoized. The Lstat is free next to the `git rev-parse` that
// WorktreeRoot runs, and a cached "it was fine" is a stale answer in a
// long-lived process such as `entire mcp`.
func RequireEntireDir(ctx context.Context) error {
	root, err := WorktreeRoot(ctx)
	switch {
	case err == nil:
		return ValidateEntireDirAt(root)
	case errors.Is(err, ErrNotARepository):
		return nil
	default:
		return fmt.Errorf("%w, so %s cannot be verified: %w", ErrRepositoryUnresolved, EntireDir, err)
	}
}

// describeMode names what was found. The sentinel supplies the "not a
// directory" half of the sentence, so these read as the first half of "X is a
// symbolic link, not a directory".
func describeMode(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "a symbolic link"
	case mode.IsRegular():
		return "a regular file"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeDevice != 0:
		return "a device"
	default:
		return "of an unsupported type"
	}
}
