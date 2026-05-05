package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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
	if p.Name != "pgr" {
		t.Errorf("Name = %q, want pgr", p.Name)
	}
	if !p.Symlink {
		t.Errorf("expected Symlink=true; got %+v", p)
	}

	plugins, err := ListInstalledPlugins()
	if err != nil {
		t.Fatalf("ListInstalledPlugins: %v", err)
	}
	if len(plugins) != 1 || plugins[0].Name != "pgr" {
		t.Errorf("ListInstalledPlugins = %+v; want one entry named pgr", plugins)
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
	if err := RemoveInstalledPlugin("pgr"); err != nil {
		t.Fatalf("RemoveInstalledPlugin: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source %s was disturbed by RemoveInstalledPlugin: %v", src, err)
	}
	if err := RemoveInstalledPlugin("pgr"); err == nil {
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
	cases := map[string]string{
		"entire-pgr":     "pgr",
		"entire-pgr.exe": "pgr",
		"entire-foo.bat": "foo",
		"entire-":        "",
		"foo":            "",
		"":               "",
	}
	for in, want := range cases {
		if got := bareNameFromBinaryName(in); got != want {
			t.Errorf("bareNameFromBinaryName(%q) = %q; want %q", in, got, want)
		}
	}
}
