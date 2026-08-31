// Package testutil provides shared test utilities for both integration and e2e tests.
// This package has no build tags, making it usable by all test packages.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil/gitenv"
)

// PendingCheckpoint mirrors the `checkpoint list --pending --json` JSON output.
type PendingCheckpoint struct {
	ID               string    `json:"id"`
	Message          string    `json:"message"`
	MetadataDir      string    `json:"metadata_dir"`
	Date             time.Time `json:"date"`
	IsTaskCheckpoint bool      `json:"is_task_checkpoint"`
	ToolUseID        string    `json:"tool_use_id"`
	IsLogsOnly       bool      `json:"is_logs_only"`
	CondensationID   string    `json:"condensation_id"`
}

// InitRepo initializes a git repository in the given directory with test user config.
func InitRepo(t *testing.T, repoDir string) {
	t.Helper()

	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}
	defer repo.Close()

	// Configure git user for commits
	cfg, err := repo.Config()
	if err != nil {
		t.Fatalf("failed to get repo config: %v", err)
	}
	cfg.User.Name = "Test User"
	cfg.User.Email = "test@example.com"

	// Disable GPG signing for test commits
	if cfg.Raw == nil {
		cfg.Raw = config.New()
	}
	cfg.Raw.Section("commit").SetOption("gpgsign", "false")
	cfg.Core.AutoCRLF = "true"

	if err := repo.SetConfig(cfg); err != nil {
		t.Fatalf("failed to set repo config: %v", err)
	}
}

// WriteFile creates a file with the given content in the repo directory.
// It creates parent directories as needed.
func WriteFile(t *testing.T, repoDir, path, content string) {
	t.Helper()

	fullPath := filepath.Join(repoDir, path)

	// Create parent directories
	dir := filepath.Dir(fullPath)
	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("failed to create directory %s: %v", dir, err)
	}

	//nolint:gosec // test code, permissions are intentionally standard
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write file %s: %v", path, err)
	}
}

// GitAdd stages files for commit.
func GitAdd(t *testing.T, repoDir string, paths ...string) {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	for _, path := range paths {
		if _, err := worktree.Add(path); err != nil {
			t.Fatalf("failed to add file %s: %v", path, err)
		}
	}
}

// GitCommit creates a commit with all staged files.
func GitCommit(t *testing.T, repoDir, message string) {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	worktree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("failed to get worktree: %v", err)
	}

	_, err = worktree.Commit(message, &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test User",
			Email: "test@example.com",
			When:  time.Now(),
		},
	})
	if err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
}

// GitCheckoutNewBranch creates and checks out a new branch.
// Uses git CLI to work around go-git v5 bug with checkout deleting untracked files.
func GitCheckoutNewBranch(t *testing.T, repoDir, branchName string) {
	t.Helper()
	gitenv.Run(t, repoDir, "checkout", "-b", branchName)
}

// GetHeadHash returns the current HEAD commit hash.
func GetHeadHash(t *testing.T, repoDir string) string {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("failed to get HEAD: %v", err)
	}

	return head.Hash().String()
}

// GitAddForce stages paths past .gitignore. GitAdd goes through go-git's
// worktree.Add, which has no force option.
func GitAddForce(t *testing.T, repoDir string, paths ...string) {
	t.Helper()
	gitenv.Run(t, repoDir, append([]string{"add", "-f"}, paths...)...)
}

// CreateBranch creates a local branch at the current HEAD.
func CreateBranch(t *testing.T, dir string, name string) {
	t.Helper()
	gitenv.Run(t, dir, "branch", name)
}

// AddRemote adds a git remote named name pointing at url in repoDir.
func AddRemote(t *testing.T, repoDir, name, url string) {
	t.Helper()
	gitenv.Run(t, repoDir, "remote", "add", name, url)
}

// WriteCheckpointPushRemoteSetting writes .entire/settings.json configuring
// strategy_options.checkpoint_push_remote to remoteName (with enabled: true).
func WriteCheckpointPushRemoteSetting(t *testing.T, repoDir, remoteName string) {
	t.Helper()
	content := `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "` + remoteName + `"}}`
	WriteFile(t, repoDir, filepath.Join(".entire", "settings.json"), content)
}

// GitUpdateRef points ref at hash in repoDir via git update-ref.
func GitUpdateRef(t *testing.T, repoDir, ref, hash string) {
	t.Helper()
	gitenv.Run(t, repoDir, "update-ref", ref, hash)
}

// GitReset runs git reset --hard to the given ref.
func GitReset(t *testing.T, dir string, ref string) {
	t.Helper()
	gitenv.Run(t, dir, "reset", "--hard", ref)
}

// BranchExists checks if a branch exists in the repository.
func BranchExists(t *testing.T, repoDir, branchName string) bool {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("failed to open git repo: %v", err)
	}
	defer repo.Close()

	refs, err := repo.References()
	if err != nil {
		t.Fatalf("failed to get references: %v", err)
	}

	found := false
	//nolint:errcheck,gosec // ForEach callback doesn't return errors we need to handle
	refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name().Short() == branchName {
			found = true
		}
		return nil
	})

	return found
}
