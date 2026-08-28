package paths

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// symlinkOrSkip creates a symlink, skipping the test where the platform or the
// account cannot make one (Windows without developer mode).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
}

func TestValidateEntireDirAt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T, root string)
		wantErr bool
	}{
		{
			name:  "absent is fine",
			setup: func(*testing.T, string) {},
		},
		{
			name: "real directory is fine",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(root, EntireDir), 0o750); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "regular file is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, EntireDir), []byte("nope"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
		{
			name: "symlink to a directory is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				target := filepath.Join(root, "elsewhere")
				if err := os.Mkdir(target, 0o750); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, target, filepath.Join(root, EntireDir))
			},
			wantErr: true,
		},
		{
			name: "symlink to a file is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				target := filepath.Join(root, "elsewhere")
				if err := os.WriteFile(target, []byte("nope"), 0o600); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, target, filepath.Join(root, EntireDir))
			},
			wantErr: true,
		},
		{
			name: "dangling symlink is rejected",
			setup: func(t *testing.T, root string) {
				t.Helper()
				symlinkOrSkip(t, filepath.Join(root, "missing"), filepath.Join(root, EntireDir))
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			tt.setup(t, root)

			err := ValidateEntireDirAt(root)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ValidateEntireDirAt returned nil, want error")
				}
				if !errors.Is(err, ErrEntireDirNotDirectory) {
					t.Errorf("error does not wrap ErrEntireDirNotDirectory: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateEntireDirAt: %v", err)
			}
		})
	}
}

// A directory we cannot stat through is not proof the invariant holds, so it
// must fail rather than pass. Root bypasses permission bits, and Windows does
// not honour them here at all.
func TestValidateEntireDirAt_UnreadableParentFails(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == osWindows {
		t.Skip("directory permission bits do not gate stat on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permission bits")
	}

	root := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, EntireDir), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(root, 0o750); err != nil {
			t.Logf("restore permissions on %s: %v", root, err)
		}
	})

	err := ValidateEntireDirAt(root)
	if err == nil {
		t.Fatal("ValidateEntireDirAt returned nil for an unreadable parent, want error")
	}
	if errors.Is(err, ErrEntireDirNotDirectory) {
		t.Errorf("a stat failure must not be reported as a wrong file type: %v", err)
	}
}

// The message is what a user acts on, so it must name the path and what was
// found there.
func TestValidateEntireDirAt_MessageNamesPathAndType(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	entire := filepath.Join(root, EntireDir)
	symlinkOrSkip(t, root, entire)

	err := ValidateEntireDirAt(root)
	if err == nil {
		t.Fatal("ValidateEntireDirAt returned nil, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, entire) {
		t.Errorf("message %q does not name the path %q", msg, entire)
	}
	if !strings.Contains(msg, "symbolic link") {
		t.Errorf("message %q does not say what was found", msg)
	}
}

// Outside a git repository there is no worktree root, so there is no `.entire`
// to validate and the check is skipped. Commands that need a repository report
// its absence themselves, with a message about the repository rather than one
// about `.entire`.
//
// The stray `.entire` file proves the skip is real: were the check resolving a
// root some other way, this would trip it.
func TestRequireEntireDir_OutsideRepositoryIsSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, EntireDir), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(dir)
	ClearWorktreeRootCache()
	t.Cleanup(ClearWorktreeRootCache)

	if err := RequireEntireDir(context.Background()); err != nil {
		t.Fatalf("RequireEntireDir outside a repository: %v", err)
	}
}

// withFakeGit puts a stub `git` on an otherwise-empty PATH so a specific
// discovery failure can be driven exactly. Not parallel: PATH is
// process-global. Go runs sequential tests to completion before resuming
// parallel ones, so they never overlap.
func withFakeGit(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == osWindows {
		t.Skip("shell-script git stub is Unix-only")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ClearWorktreeRootCache()
	t.Cleanup(ClearWorktreeRootCache)
}

// inDirWithSymlinkedEntireDir puts the process in a directory whose `.entire`
// is a symlink. Every fail-closed case below runs here, because the point is
// not that an error is returned — it is that this symlink is not read through.
func inDirWithSymlinkedEntireDir(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, target, filepath.Join(dir, EntireDir))
	t.Chdir(dir)
}

// Git's own verdict is the one skippable case.
func TestRequireEntireDir_GitSaysNotARepository(t *testing.T) {
	inDirWithSymlinkedEntireDir(t)
	withFakeGit(t, `echo "fatal: not a git repository (or any of the parent directories): .git" >&2
exit 128`)

	if err := RequireEntireDir(context.Background()); err != nil {
		t.Fatalf("RequireEntireDir with git's not-a-repository verdict: %v", err)
	}
}

// Everything else must fail closed. Exit code 128 is not the signal — git uses
// it for failures that happen INSIDE a repository too.
func TestRequireEntireDir_DiscoveryFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name   string
		script string
	}{
		{
			// safe.directory. Exit 128, and we are very much inside a repo.
			name: "dubious ownership",
			script: `echo "fatal: detected dubious ownership in repository at '/repo'" >&2
exit 128`,
		},
		{
			name: "permission denied",
			script: `echo "fatal: could not read '/repo/.git/config': Permission denied" >&2
exit 128`,
		},
		{
			name:   "success with no output",
			script: `exit 0`,
		},
		{
			name: "success with blank output",
			script: `echo ""
exit 0`,
		},
		{
			name:   "killed by a signal",
			script: `kill -TERM $$`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inDirWithSymlinkedEntireDir(t)
			withFakeGit(t, tt.script)

			err := RequireEntireDir(context.Background())
			if err == nil {
				t.Fatal("RequireEntireDir returned nil; a discovery failure must not be read as 'no repository'")
			}
			if errors.Is(err, ErrNotARepository) {
				t.Errorf("failure was classified as not-a-repository: %v", err)
			}
		})
	}
}

func TestRequireEntireDir_GitMissingFailsClosed(t *testing.T) {
	inDirWithSymlinkedEntireDir(t)
	t.Setenv("PATH", t.TempDir())
	ClearWorktreeRootCache()
	t.Cleanup(ClearWorktreeRootCache)

	err := RequireEntireDir(context.Background())
	if err == nil {
		t.Fatal("RequireEntireDir returned nil with no git on PATH")
	}
	if errors.Is(err, ErrNotARepository) {
		t.Errorf("a missing git was classified as not-a-repository: %v", err)
	}
}

func TestRequireEntireDir_CancelledContextFailsClosed(t *testing.T) {
	inDirWithSymlinkedEntireDir(t)
	ClearWorktreeRootCache()
	t.Cleanup(ClearWorktreeRootCache)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := RequireEntireDir(ctx)
	if err == nil {
		t.Fatal("RequireEntireDir returned nil for a cancelled context")
	}
	if errors.Is(err, ErrNotARepository) {
		t.Errorf("a cancellation was classified as not-a-repository: %v", err)
	}
}

// Git translates its messages, so the classifier pins LC_ALL/LANGUAGE. Without
// that, a localized machine would fail closed on every command run outside a
// repository.
func TestWorktreeRoot_NotARepositoryIsRecognisedUnderALocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	t.Setenv("LANGUAGE", "de")
	withFakeGit(t, `if [ "$LC_ALL" = "C" ] && [ -z "$LANGUAGE" ]; then
  echo "fatal: not a git repository (or any of the parent directories): .git" >&2
else
  echo "fatal: Kein Git-Repository (oder irgendein Elternverzeichnis): .git" >&2
fi
exit 128`)

	_, err := WorktreeRoot(context.Background())
	if !errors.Is(err, ErrNotARepository) {
		t.Fatalf("not-a-repository was not recognised with a locale set: %v", err)
	}
}

// "exit status 128" names no cause. The causes that land here are ones the user
// has to act on, and git already said what they are.
func TestWorktreeRoot_SurfacesGitsFatal(t *testing.T) {
	withFakeGit(t, `echo "fatal: detected dubious ownership in repository at '/repo'" >&2
exit 128`)

	_, err := WorktreeRoot(context.Background())
	if err == nil {
		t.Fatal("WorktreeRoot returned nil")
	}
	if !strings.Contains(err.Error(), "dubious ownership") {
		t.Errorf("git's fatal was swallowed: %v", err)
	}
}

// The three failure conditions are distinguished positively, because each has a
// different remedy and callers print it. Classification by elimination is how a
// stat error came to be reported as a git problem.
func TestEntireDirErrorsAreDistinguishable(t *testing.T) {
	t.Parallel()

	t.Run("wrong file type", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, EntireDir), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		err := ValidateEntireDirAt(root)
		if !errors.Is(err, ErrEntireDirNotDirectory) {
			t.Fatalf("want ErrEntireDirNotDirectory, got %v", err)
		}
		if errors.Is(err, ErrEntireDirUnreadable) {
			t.Errorf("a wrong file type must not read as unreadable: %v", err)
		}
	})

	t.Run("stat failure", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == osWindows {
			t.Skip("directory permission bits do not gate stat on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permission bits")
		}

		root := filepath.Join(t.TempDir(), "repo")
		if err := os.Mkdir(root, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, EntireDir), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(root, 0o750); err != nil {
				t.Logf("restore permissions on %s: %v", root, err)
			}
		})

		err := ValidateEntireDirAt(root)
		if !errors.Is(err, ErrEntireDirUnreadable) {
			t.Fatalf("want ErrEntireDirUnreadable, got %v", err)
		}
		if errors.Is(err, ErrEntireDirNotDirectory) {
			t.Errorf("a stat failure must not read as a wrong file type: %v", err)
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("the underlying cause was dropped: %v", err)
		}
	})
}

// Separate from the table above because it needs t.Chdir, which cannot run
// under a parallel parent.
func TestRequireEntireDir_UnresolvedRepositoryIsItsOwnCondition(t *testing.T) {
	inDirWithSymlinkedEntireDir(t)
	withFakeGit(t, `echo "fatal: detected dubious ownership in repository at '/repo'" >&2
exit 128`)

	err := RequireEntireDir(context.Background())
	if !errors.Is(err, ErrRepositoryUnresolved) {
		t.Fatalf("want ErrRepositoryUnresolved, got %v", err)
	}
	if errors.Is(err, ErrEntireDirNotDirectory) || errors.Is(err, ErrEntireDirUnreadable) {
		t.Errorf("an unresolved repository must not read as a problem with the path: %v", err)
	}
}
