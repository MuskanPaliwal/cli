package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
)

var (
	// ErrRefCASConflict means the ref no longer has the expected old value.
	ErrRefCASConflict = errors.New("git reference changed during compare-and-swap")
	// ErrRefLocked means native Git could not acquire the ref lock.
	ErrRefLocked = errors.New("git reference lock is unavailable")
)

// CompareAndSwapRef updates refName through native Git without dereferencing a
// symbolic ref. ZeroHash as expectedHash requires that refName does not exist.
func CompareAndSwapRef(
	ctx context.Context,
	repoRoot string,
	refName plumbing.ReferenceName,
	newHash, expectedHash plumbing.Hash,
) error {
	newValue := newHash.String()
	oldValue := strings.Repeat("0", newHash.HexSize())
	if expectedHash != plumbing.ZeroHash {
		oldValue = expectedHash.String()
	}

	cmd := exec.CommandContext(ctx, "git", "update-ref", "--no-deref", "--end-of-options", refName.String(), newValue, oldValue)
	cmd.Dir = repoRoot
	cmd.Env = gitPlumbingEnv()
	stderr, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("git update-ref %s: %w", refName, ctxErr)
	}

	detail := strings.TrimSpace(string(stderr))
	switch {
	case isRefCASConflict(stderr):
		return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, ErrRefCASConflict)
	case isRefLockContention(stderr):
		return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, ErrRefLocked)
	default:
		return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, err)
	}
}

// isRefCASConflict reports that git rejected the expected old value, including
// the cases where a required ref disappeared or a create-only ref appeared.
func isRefCASConflict(stderr []byte) bool {
	msg := strings.ToLower(string(stderr))
	return strings.Contains(msg, "but expected") ||
		strings.Contains(msg, "unable to resolve reference") ||
		strings.Contains(msg, "reference already exists")
}

func isRefLockContention(stderr []byte) bool {
	msg := strings.ToLower(string(stderr))
	return strings.Contains(msg, "cannot lock references") ||
		strings.Contains(msg, ".lock") && strings.Contains(msg, "file exists")
}
