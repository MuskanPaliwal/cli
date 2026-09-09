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
// so filenames that look like pathspec magic remain filenames.
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

	args := []string{
		"--no-optional-locks", "-C", worktreeRoot,
		"diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv",
		commit.String(), "--",
	}
	for _, path := range paths {
		args = append(args, ":(literal)"+path)
	}

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
	return changed, nil
}
