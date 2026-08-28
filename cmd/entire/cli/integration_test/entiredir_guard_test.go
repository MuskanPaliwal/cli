//go:build integration

package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// `.entire` must be a real directory. These tests spawn the real binary so the
// whole path is exercised — cobra's hook ordering, the exit code, and the fact
// that nothing is written through the replaced path. A unit test on the
// pre-run cannot show that the process actually stops.

// replaceEntireDirWithSymlink moves the repo's `.entire` aside and symlinks it
// back. This is the realistic shape: the directory keeps working for anything
// that follows symlinks, so a Stat-based check sees a valid directory and a
// test that only checked for a regular file would pass while the hole stayed
// open. Returns the directory the symlink points at.
func replaceEntireDirWithSymlink(t *testing.T, repoDir string) string {
	t.Helper()
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink harness only runs on Unix")
	}

	entireDir := filepath.Join(repoDir, paths.EntireDir)
	target := filepath.Join(t.TempDir(), "entire-elsewhere")
	if err := os.Rename(entireDir, target); err != nil {
		t.Fatalf("move %s aside: %v", entireDir, err)
	}
	if err := os.Symlink(target, entireDir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	return target
}

// runEntireInRepo runs the real binary in repoDir and returns its exit code
// plus stdout and stderr merged, since which stream a message lands on is not
// what these tests are about. It wraps the package's runEntire to add the exit
// code, which is the assertion that matters here: the guard has to stop the
// process, not merely print something.
func runEntireInRepo(t *testing.T, repoDir string, args ...string) (exitCode int, output string) {
	t.Helper()

	stdout, stderr, err := runEntire(t, os.Environ(), repoDir, args...)
	output = stdout + stderr
	if err == nil {
		return 0, output
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run entire %v: %v\n%s", args, err, output)
	}
	return exitErr.ExitCode(), output
}

func TestEntireDirSymlink_GuardedCommandsStop(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	replaceEntireDirWithSymlink(t, env.RepoDir)

	// One per shape that matters: a status read, a checkpoint read, the setup
	// path that would otherwise MkdirAll through the symlink, and the surface
	// an agent asks for its instructions on.
	for _, args := range [][]string{
		{"status"},
		{"checkpoint", "list"},
		{"session", "list"},
		{"enable"},
		{"agent-help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			exitCode, output := runEntireInRepo(t, env.RepoDir, args...)
			if exitCode == 0 {
				t.Fatalf("`entire %s` exited 0 with a symlinked .entire:\n%s",
					strings.Join(args, " "), output)
			}
			if !strings.Contains(output, "symbolic link") {
				t.Errorf("output does not say what was found:\n%s", output)
			}
			if !strings.Contains(output, paths.EntireDir) {
				t.Errorf("output does not name %s:\n%s", paths.EntireDir, output)
			}
		})
	}
}

func TestEntireDirSymlink_ExemptCommandsStillRun(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	replaceEntireDirWithSymlink(t, env.RepoDir)

	for _, args := range [][]string{{"version"}, {"completion", "bash"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			exitCode, output := runEntireInRepo(t, env.RepoDir, args...)
			if exitCode != 0 {
				t.Fatalf("`entire %s` exited %d; exempt commands must still run:\n%s",
					strings.Join(args, " "), exitCode, output)
			}
		})
	}
}

// --help is the escape hatch that survives regardless: cobra returns
// flag.ErrHelp before it runs any PersistentPreRunE. It is worth pinning,
// because guarding `help` rests on it.
func TestEntireDirSymlink_HelpFlagStillWorks(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	replaceEntireDirWithSymlink(t, env.RepoDir)

	exitCode, output := runEntireInRepo(t, env.RepoDir, "status", "--help")
	if exitCode != 0 {
		t.Fatalf("`entire status --help` exited %d:\n%s", exitCode, output)
	}
	if !strings.Contains(output, "Usage:") {
		t.Errorf("no usage in output:\n%s", output)
	}
}

func TestEntireDirSymlink_DoctorReportsIt(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	replaceEntireDirWithSymlink(t, env.RepoDir)

	exitCode, output := runEntireInRepo(t, env.RepoDir, "doctor")
	if exitCode == 0 {
		t.Fatalf("`entire doctor` exited 0 on a broken repo:\n%s", output)
	}
	if !strings.Contains(output, "BROKEN") {
		t.Errorf("doctor did not report the problem:\n%s", output)
	}
	if !strings.Contains(output, "entire enable") {
		t.Errorf("doctor did not name the remedy:\n%s", output)
	}
}

// Refusing to run is only half of it. Nothing may be created through the
// replaced path either, or an exempt command's log sink lands in whatever
// directory the symlink points at.
func TestEntireDirSymlink_NothingIsWrittenThroughIt(t *testing.T) {
	t.Parallel()
	env := NewRepoWithCommit(t)
	target := replaceEntireDirWithSymlink(t, env.RepoDir)

	before := dirEntryNames(t, target)

	for _, args := range [][]string{{"status"}, {"version"}, {"doctor"}, {"session", "list"}} {
		runEntireInRepo(t, env.RepoDir, args...)
	}

	after := dirEntryNames(t, target)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Errorf("symlink target changed.\nbefore: %v\nafter:  %v", before, after)
	}
	for _, name := range after {
		if name == "logs" {
			t.Error("a log sink was created through the symlinked .entire")
		}
	}
}

func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
