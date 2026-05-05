package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testPluginName is the bare plugin name used across managed-store tests.
const testPluginName = "pgr"

// withPluginDir points $ENTIRE_PLUGIN_DIR at a fresh temp dir so the managed
// helpers operate in isolation. Mutates process state, so the calling test
// must not be t.Parallel.
func withPluginDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(pluginEnvPluginDir, dir)
	return dir
}

func TestPluginParentDir_HonorsOverride(t *testing.T) { //nolint:paralleltest // mutates env
	dir := withPluginDir(t)
	got, err := pluginParentDir()
	if err != nil {
		t.Fatalf("pluginParentDir: %v", err)
	}
	if got != dir {
		t.Errorf("pluginParentDir = %q, want %q", got, dir)
	}
}

func TestPluginBinDir_AndDataDir(t *testing.T) { //nolint:paralleltest // mutates env
	root := withPluginDir(t)
	bin, err := PluginBinDir()
	if err != nil {
		t.Fatalf("PluginBinDir: %v", err)
	}
	if want := filepath.Join(root, "bin"); bin != want {
		t.Errorf("PluginBinDir = %q, want %q", bin, want)
	}
	data, err := PluginDataDir("pgr")
	if err != nil {
		t.Fatalf("PluginDataDir: %v", err)
	}
	if want := filepath.Join(root, "data", "pgr"); data != want {
		t.Errorf("PluginDataDir(pgr) = %q, want %q", data, want)
	}
}

func TestPrependPluginBinDirToPATH(t *testing.T) { //nolint:paralleltest // mutates env
	root := withPluginDir(t)
	t.Setenv("PATH", "/usr/bin:/bin")
	PrependPluginBinDirToPATH()
	bin := filepath.Join(root, "bin")
	got := os.Getenv("PATH")
	if !strings.HasPrefix(got, bin+string(os.PathListSeparator)) {
		t.Errorf("PATH does not start with managed bin dir: %q", got)
	}
	// Idempotent: a second call must not double-prepend.
	PrependPluginBinDirToPATH()
	if strings.Count(os.Getenv("PATH"), bin) != 1 {
		t.Errorf("PATH contains managed bin dir %d times after second prepend; want 1: %q", strings.Count(os.Getenv("PATH"), bin), os.Getenv("PATH"))
	}
}

func TestInstallPluginFromPath_SymlinksAndLists(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink path is Unix-only here")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "entire-pgr")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}

	p, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src})
	if err != nil {
		t.Fatalf("InstallPluginFromPath: %v", err)
	}
	if p.Name != testPluginName {
		t.Errorf("Name = %q, want %s", p.Name, testPluginName)
	}
	if !p.Symlink {
		t.Errorf("expected Symlink=true; got %+v", p)
	}

	plugins, err := ListInstalledPlugins()
	if err != nil {
		t.Fatalf("ListInstalledPlugins: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Name != testPluginName {
		t.Errorf("ListInstalledPlugins = %+v; want one entry named %s", plugins, testPluginName)
	}

	// Re-install without --force fails.
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error on re-install without --force")
	}
	// With --force succeeds.
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src, Force: true}); err != nil {
		t.Errorf("InstallPluginFromPath --force: %v", err)
	}

	// Remove unlinks the managed entry without disturbing the source.
	if err := RemoveInstalledPlugin(testPluginName); err != nil {
		t.Fatalf("RemoveInstalledPlugin: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source %s was disturbed by RemoveInstalledPlugin: %v", src, err)
	}
	if err := RemoveInstalledPlugin(testPluginName); err == nil {
		t.Errorf("expected error removing already-removed plugin")
	}
}

func TestInstallPluginFromPath_RejectsBadBasename(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix permissions checks")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "not-prefixed")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error for non-prefixed basename")
	}
}

func TestInstallPluginFromPath_RejectsNonExecutable(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix permissions checks")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "entire-noexec")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error for non-executable source")
	}
}

func TestBareNameFromBinaryName(t *testing.T) {
	t.Parallel()
	// Cases that hold on every platform.
	common := map[string]string{
		"entire-pgr": "pgr",
		"entire-":    "",
		"foo":        "",
		"":           "",
	}
	for in, want := range common {
		if got := bareNameFromBinaryName(in); got != want {
			t.Errorf("bareNameFromBinaryName(%q) = %q; want %q", in, got, want)
		}
	}
	// Platform-conditional: extensions are stripped only on Windows so a
	// managed entry actually resolves at runtime via exec.LookPath.
	if runtime.GOOS == windowsGOOS {
		for in, want := range map[string]string{
			"entire-pgr.exe": "pgr",
			"entire-foo.bat": "foo",
			"entire-foo.cmd": "foo",
		} {
			if got := bareNameFromBinaryName(in); got != want {
				t.Errorf("[windows] bareNameFromBinaryName(%q) = %q; want %q", in, got, want)
			}
		}
	} else {
		// On Unix, a .exe basename is *not* a valid bare name — installing
		// it would yield a managed entry that LookPath would never match.
		// We accept that bareNameFromBinaryName may return a non-empty
		// string here (the dispatcher uses exact-match LookPath); the
		// guarantee we test is that "entire-pgr.exe" doesn't collapse to
		// "pgr" on Unix.
		if got := bareNameFromBinaryName("entire-pgr.exe"); got == "pgr" {
			t.Errorf("[unix] bareNameFromBinaryName(entire-pgr.exe) collapsed to %q; should not strip .exe on Unix", got)
		}
	}
}

func TestValidatePluginName(t *testing.T) {
	t.Parallel()
	good := []string{"pgr", "foo-bar", "x", "v1"}
	for _, n := range good {
		if err := validatePluginName(n); err != nil {
			t.Errorf("validatePluginName(%q) = %v; want nil", n, err)
		}
	}
	bad := []string{"", ".", "..", "-foo", "agent-foo", "foo/bar", `foo\bar`}
	for _, n := range bad {
		if err := validatePluginName(n); err == nil {
			t.Errorf("validatePluginName(%q) = nil; want error", n)
		}
	}
}

func TestPluginDataDir_RejectsPathTraversal(t *testing.T) { //nolint:paralleltest // mutates env
	withPluginDir(t)
	for _, name := range []string{"", ".", "..", "agent-foo", "foo/bar"} {
		if _, err := PluginDataDir(name); err == nil {
			t.Errorf("PluginDataDir(%q) = nil error; want error", name)
		}
	}
}

func TestInstallPluginFromPath_RejectsAgentReservedName(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix-only test")
	}
	withPluginDir(t)
	src := filepath.Join(t.TempDir(), "entire-agent-foo")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err == nil {
		t.Errorf("expected error: agent-* basename is reserved")
	}
}

func TestInstallPluginFromPath_RejectsSelfInstall(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("Unix-only test")
	}
	root := withPluginDir(t)
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Drop the source directly into the managed dir, then attempt to
	// install it from that same path. Without the self-install guard,
	// --force would Remove() this file before symlinking to a missing
	// target, deleting the working install.
	src := filepath.Join(binDir, "entire-foo")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	_, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src, Force: true})
	if err == nil {
		t.Fatalf("expected self-install rejection; got nil")
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Errorf("self-install attempt deleted the source: %v", statErr)
	}
}

func TestInstallPluginFromPath_AtomicForceReplace(t *testing.T) { //nolint:paralleltest // mutates env
	if runtime.GOOS == windowsGOOS {
		t.Skip("symlink/atomic-rename behavior is Unix-focused here")
	}
	withPluginDir(t)
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "entire-foo")
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src}); err != nil {
		t.Fatalf("first install: %v", err)
	}
	binDir, err := PluginBinDir()
	if err != nil {
		t.Fatalf("PluginBinDir: %v", err)
	}
	dest := filepath.Join(binDir, "entire-foo")
	if _, err := os.Lstat(dest); err != nil {
		t.Fatalf("first install missing: %v", err)
	}

	// Force-install from the same source. The replace should succeed and
	// the previous symlink remains valid throughout.
	if _, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: src, Force: true}); err != nil {
		t.Errorf("force replace: %v", err)
	}
	if _, err := os.Lstat(dest); err != nil {
		t.Errorf("dest missing after force replace: %v", err)
	}
}
