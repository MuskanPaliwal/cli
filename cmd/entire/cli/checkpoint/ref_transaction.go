package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// ErrRefConflict identifies a native Git compare-and-swap failure.
var ErrRefConflict = errors.New("git ref moved during update")

const (
	refTransactionMaxAttempts = 16
	refTransactionMaxJitter   = 8 * time.Millisecond
)

// repositoryObjectLocks protects go-git's repository-local filesystem object
// cache. Native Git CAS remains the cross-process ref-publication boundary.
var repositoryObjectLocks sync.Map

func repositoryObjectLock(repo *git.Repository) *sync.Mutex {
	lock, _ := repositoryObjectLocks.LoadOrStore(repo, &sync.Mutex{})
	objectLock, ok := lock.(*sync.Mutex)
	if !ok {
		panic("checkpoint repository object lock has an unexpected type")
	}
	return objectLock
}

// RefConflictError reports the expected and observed tips for a failed ref CAS.
type RefConflictError struct {
	Ref      plumbing.ReferenceName
	Expected plumbing.Hash
	Actual   plumbing.Hash
}

func (e *RefConflictError) Error() string {
	return fmt.Sprintf("%s: %v (expected %s, found %s)", e.Ref, ErrRefConflict, e.Expected, e.Actual)
}

func (e *RefConflictError) Unwrap() error {
	return ErrRefConflict
}

// RefMutation rebuilds a ref update from its current tip. changed=false is an
// idempotent no-op and returns current without invoking git update-ref.
type RefMutation func(current plumbing.Hash) (next plumbing.Hash, changed bool, err error)

type beforeRefCASKey struct{}

func withBeforeRefCAS(ctx context.Context, hook func()) context.Context {
	return context.WithValue(ctx, beforeRefCASKey{}, hook)
}

// RunRefTransaction retries a logical ref mutation against the latest tip.
// The callback is invoked again after every CAS conflict, so it must rebuild
// trees and commits from current rather than reuse objects based on a stale tip.
func RunRefTransaction(
	ctx context.Context,
	repo *git.Repository,
	refName plumbing.ReferenceName,
	mutate RefMutation,
) (plumbing.Hash, error) {
	for attempt := range refTransactionMaxAttempts {
		if err := ctx.Err(); err != nil {
			return plumbing.ZeroHash, err //nolint:wrapcheck // canonical context cancellation
		}

		current, err := ReadRefHash(repo, refName)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		var next plumbing.Hash
		var changed bool
		objectLock := repositoryObjectLock(repo)
		func() {
			objectLock.Lock()
			defer objectLock.Unlock()
			next, changed, err = mutate(current)
		}()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if !changed {
			return current, nil
		}
		if next.IsZero() {
			return plumbing.ZeroHash, fmt.Errorf("ref transaction %s produced an empty target", refName)
		}

		if hook, ok := ctx.Value(beforeRefCASKey{}).(func()); ok {
			hook()
		}
		err = CompareAndSwapRef(ctx, repo, refName, next, current)
		if err == nil {
			return next, nil
		}
		if !errors.Is(err, ErrRefConflict) {
			return plumbing.ZeroHash, err
		}
		if attempt+1 == refTransactionMaxAttempts {
			return plumbing.ZeroHash, fmt.Errorf("update ref %s after %d attempts: %w", refName, refTransactionMaxAttempts, err)
		}
		if err := refTransactionBackoff(ctx, attempt); err != nil {
			return plumbing.ZeroHash, err
		}
	}
	panic("unreachable")
}

// ReadRefHash returns a ref's current hash, or ZeroHash when it does not exist.
func ReadRefHash(repo *git.Repository, refName plumbing.ReferenceName) (plumbing.Hash, error) {
	ref, err := repo.Reference(refName, true)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read ref %s: %w", refName, err)
	}
	return ref.Hash(), nil
}

// CompareAndSwapRef atomically updates refName through native Git. expected
// ZeroHash means the ref must not exist. Native Git is the lock interoperability
// boundary across hooks, worktrees, and other Git clients.
func CompareAndSwapRef(
	ctx context.Context,
	repo *git.Repository,
	refName plumbing.ReferenceName,
	newHash, expected plumbing.Hash,
) error {
	if newHash.IsZero() {
		return errors.New("compare-and-swap ref: new hash is required")
	}
	root, err := repositoryWorktreeRoot(repo)
	if err != nil {
		return err
	}

	oldValue := strings.Repeat("0", newHash.HexSize())
	if !expected.IsZero() {
		oldValue = expected.String()
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "update-ref", refName.String(), newHash.String(), oldValue)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, runErr := cmd.CombinedOutput()
	if runErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err //nolint:wrapcheck // canonical context cancellation
	}

	actual, readErr := ReadRefHash(repo, refName)
	detail := strings.TrimSpace(string(output))
	if readErr == nil && (actual != expected || strings.Contains(detail, "cannot lock ref") || strings.Contains(detail, "but expected")) {
		return &RefConflictError{Ref: refName, Expected: expected, Actual: actual}
	}
	if readErr != nil {
		return fmt.Errorf("git update-ref %s failed (%s), then ref reread failed: %w", refName, detail, readErr)
	}
	return fmt.Errorf("git update-ref %s: %s: %w", refName, detail, runErr)
}

func repositoryWorktreeRoot(repo *git.Repository) (string, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("open repository worktree: %w", err)
	}
	root := worktree.Filesystem().Root()
	if root == "" {
		return "", errors.New("repository worktree filesystem has no root path")
	}
	return root, nil
}

func refTransactionBackoff(ctx context.Context, attempt int) error {
	limit := refTransactionMaxJitter
	if attempt > 4 {
		limit *= 2
	}
	delay := time.Duration(rand.Int64N(int64(limit))) + time.Millisecond //nolint:gosec // scheduling jitter, not security-sensitive
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // canonical context cancellation
	}
}
