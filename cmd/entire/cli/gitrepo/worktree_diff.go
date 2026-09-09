package gitrepo

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/go-git/go-git/v6/plumbing"
)

// ChangedWorktreeFiles reports which paths differ between commit and the
// working tree, using native Git's clean-filter semantics. It compares against
// commit rather than the index because callers may run after the index has
// moved on to content that is not part of that commit.
//
// The paths must be repository-relative. They are passed as literal pathspecs,
// so filenames that look like pathspec magic remain filenames. Large path sets
// are split across commands because git diff has no --pathspec-from-file option
// and an oversized argv would fail before Git could inspect anything.
func ChangedWorktreeFiles(
	ctx context.Context,
	worktreeRoot string,
	commit plumbing.Hash,
	paths []string,
) (map[string]struct{}, error) {
	changed := make(map[string]struct{})
	if len(paths) == 0 {
		return changed, nil
	}

	baseArgs := []string{
		"--no-optional-locks", "-C", worktreeRoot,
		"diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv",
		commit.String(), "--",
	}
	for _, pathspecs := range chunkLiteralPathspecs(paths, gitDiffPathspecBudget) {
		args := append(append([]string(nil), baseArgs...), pathspecs...)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Env = EnvWithoutRepoOverrides()
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git diff working tree against %s: %w", commit, err)
		}

		for entry := range bytes.SplitSeq(out, []byte{0}) {
			if len(entry) != 0 {
				changed[string(entry)] = struct{}{}
			}
		}
	}
	return changed, nil
}

// Keep pathspec argv comfortably below Windows' 32 KiB command-line limit,
// including room for fixed arguments and os/exec quoting. Unix limits are much
// larger. A single repository-relative path can exceed the budget and is sent
// alone; filesystem path limits still keep that command bounded.
const gitDiffPathspecBudget = 8 * 1024

func chunkLiteralPathspecs(paths []string, budget int) [][]string {
	var chunks [][]string
	var (
		chunk []string
		size  int
	)
	for _, path := range paths {
		pathspec := ":(literal)" + path
		if len(chunk) > 0 && size+len(pathspec)+1 > budget {
			chunks = append(chunks, chunk)
			chunk = nil
			size = 0
		}
		chunk = append(chunk, pathspec)
		size += len(pathspec) + 1
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	return chunks
}
