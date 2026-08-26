package checkpoint

import (
	"errors"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestCompareAndSwapRef_CreateUpdateAndConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	refName := plumbing.ReferenceName("refs/entire/checkpoints/01/example")

	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); !errors.Is(err, ErrRefConflict) {
		t.Fatalf("second create error = %v, want ErrRefConflict", err)
	}

	otherName := plumbing.ReferenceName("refs/entire/test-target")
	if err := CompareAndSwapRef(t.Context(), repo, otherName, head.Hash(), plumbing.ZeroHash); err != nil {
		t.Fatalf("create second ref: %v", err)
	}
	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), head.Hash()); err != nil {
		t.Fatalf("idempotent hash update with matching expected tip: %v", err)
	}
}

func TestCompareAndSwapRef_CreateSHA256Ref(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.RunGit(t, dir, "init", "--object-format=sha256", ".")
	testutil.RunGit(t, dir, "config", "user.name", "Test User")
	testutil.RunGit(t, dir, "config", "user.email", "test@example.com")
	testutil.RunGit(t, dir, "config", "commit.gpgsign", "false")
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.RunGit(t, dir, "add", "initial.txt")
	testutil.RunGit(t, dir, "commit", "--no-gpg-sign", "-m", "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open SHA-256 repo: %v", err)
	}
	defer repo.Close()
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	if got := head.Hash().HexSize(); got != 64 {
		t.Fatalf("HEAD hex width = %d, want 64", got)
	}
	refName := plumbing.ReferenceName("refs/entire/checkpoints/01/sha256")
	if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); err != nil {
		t.Fatalf("create SHA-256 ref: %v", err)
	}
}

func TestRunRefTransaction_RebuildsAfterConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "initial.txt", "initial")
	testutil.GitAdd(t, dir, "initial.txt")
	testutil.GitCommit(t, dir, "initial")
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	head, err := repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	refName := plumbing.ReferenceName("refs/entire/checkpoints/01/retry")
	calls := 0
	got, err := RunRefTransaction(t.Context(), repo, refName, func(current plumbing.Hash) (plumbing.Hash, bool, error) {
		calls++
		if calls == 1 {
			if err := CompareAndSwapRef(t.Context(), repo, refName, head.Hash(), plumbing.ZeroHash); err != nil {
				return plumbing.ZeroHash, false, err
			}
			return head.Hash(), true, nil
		}
		if current != head.Hash() {
			t.Fatalf("retry current = %s, want %s", current, head.Hash())
		}
		return current, false, nil
	})
	if err != nil {
		t.Fatalf("RunRefTransaction: %v", err)
	}
	if got != head.Hash() {
		t.Fatalf("result = %s, want %s", got, head.Hash())
	}
	if calls != 2 {
		t.Fatalf("mutation calls = %d, want 2", calls)
	}
}
