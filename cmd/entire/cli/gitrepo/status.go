package gitrepo

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/go-git/go-git/v6"
)

// Status is the single entry point for reading go-git worktree status; the
// forbidigo rule in .golangci.yaml keeps callers off worktree.Status directly.
//
// go-git's Worktree.Status() is expensive: it walks the whole worktree twice
// (once collecting .gitignore patterns, once diffing), and gitignore.ReadPatterns
// does not thread a parent directory's patterns into its recursive walk, so it
// only prunes an ignored directory when the matching pattern was declared by
// that directory's own parent .gitignore. A rule one level too deep leaves the
// whole subtree walked: a single call cost 5.25s in this repo before e2e's
// artifacts rule moved into e2e/.gitignore.

type statusCacheKey struct{}

type statusCache struct {
	mu       sync.Mutex
	statuses map[string]git.Status
}

// WithStatusCache returns a context that memoizes Status results.
//
// Install it only across a window that neither writes tracked files nor stages
// anything — otherwise later callers observe a stale status. Staging counts:
// .git/index lives inside .git/, but the index feeds the status diff, so an
// index write invalidates a cached result just as a worktree write does. Entire
// performs no index writes today (no SetIndex calls; the git subcommands on the
// hook paths are all index-read-only), which is what makes turn start a valid
// window. A hook that runs after the agent has edited files is not.
func WithStatusCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, statusCacheKey{}, &statusCache{
		statuses: make(map[string]git.Status),
	})
}

// Status returns the worktree status for repo, reusing a cached result when ctx
// carries a cache from WithStatusCache and the same worktree was already read.
//
// The returned map is shared with other callers holding the same cached ctx, so
// callers must treat it as read-only.
func Status(ctx context.Context, repo *git.Repository) (git.Status, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}

	// Key on the worktree root rather than the repository pointer: callers on
	// the same hook path open the repository independently.
	root := worktree.Filesystem().Root()

	cache, ok := ctx.Value(statusCacheKey{}).(*statusCache)
	if !ok {
		status, err := worktree.Status() //nolint:forbidigo // the sanctioned call site
		if err != nil {
			return nil, err //nolint:wrapcheck // callers add their own context
		}
		filterNestedCheckouts(root, status)
		return status, nil
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cached, hit := cache.statuses[root]; hit {
		return cached, nil
	}

	status, err := worktree.Status() //nolint:forbidigo // the sanctioned call site
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}
	filterNestedCheckouts(root, status)
	cache.statuses[root] = status

	return status, nil
}

// filterNestedCheckouts removes untracked entries that live inside a nested
// git checkout: a directory under the worktree root containing a .git entry
// (a directory for full clones, a file for linked worktrees). git treats such
// a directory as a repository boundary and never descends into it; go-git's
// status walk has no such check, so untracked files from unrelated checkouts
// (agent worktrees, vendored clones) would otherwise be reported as if they
// were this repository's files. Tracked entries are kept regardless of
// location, matching git, which always reports index entries.
//
// Deliberately quieter than git in one respect: git reports the boundary
// itself as a single untracked entry ("?? vendor/"), while this filter drops
// it entirely. Consumers treat every status entry as a file to record, so a
// synthetic directory entry would be junk of the same kind this filter
// removes. Do not "fix" the missing boundary entry.
func filterNestedCheckouts(root string, status git.Status) {
	containsGit := make(map[string]bool)
	for relPath, fileStatus := range status {
		if fileStatus.Worktree != git.Untracked {
			continue
		}
		if insideNestedCheckout(root, relPath, containsGit) {
			delete(status, relPath)
		}
	}
}

// insideNestedCheckout reports whether any ancestor directory of relPath
// (a forward-slash path relative to root, as go-git status keys are) contains
// a .git entry. The walk stops before the root itself, so the host repo's own
// .git never counts. Lstat rather than Stat so a worktree's .git file counts
// without following it. containsGit memoizes per-directory answers across one
// status result, where thousands of paths can share a few ancestor directories.
func insideNestedCheckout(root, relPath string, containsGit map[string]bool) bool {
	for dir := path.Dir(relPath); dir != "." && dir != "/"; dir = path.Dir(dir) {
		nested, seen := containsGit[dir]
		if !seen {
			_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir), ".git"))
			nested = err == nil
			containsGit[dir] = nested
		}
		if nested {
			return true
		}
	}
	return false
}
