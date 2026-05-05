package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Managed plugin storage. The kubectl-style dispatcher in plugin.go resolves
// `entire-<name>` binaries from $PATH, period. To let `entire plugin install`
// be additive rather than a parallel mechanism, this file provides:
//
//  1. PluginBinDir() — a per-user managed dir that main.go prepends to PATH
//     before the dispatcher runs. Anything dropped here (or symlinked here)
//     becomes invocable as `entire <name>` without the user fiddling with PATH.
//
//  2. PluginDataDir(name) — a per-plugin durable storage dir, passed to plugins
//     as ENTIRE_PLUGIN_DATA_DIR. Independent of where the binary itself lives
//     so plugins installed via PATH and via the managed dir get the same
//     contract.
//
// Honors ENTIRE_PLUGIN_DIR as a parent-dir override; falls back to
// XDG_DATA_HOME, then a platform default.

const (
	pluginEnvPluginDir       = "ENTIRE_PLUGIN_DIR"
	pluginManagedBinSubdir   = "bin"
	pluginManagedDataSubdir  = "data"
	pluginEnvPluginData      = "ENTIRE_PLUGIN_DATA_DIR"
	pluginManagedDirEntireXD = "entire/plugins"
)

// pluginParentDir returns the per-user directory that holds the managed
// plugin storage. Honors ENTIRE_PLUGIN_DIR; otherwise XDG_DATA_HOME, then a
// platform default.
func pluginParentDir() (string, error) {
	if v := os.Getenv(pluginEnvPluginDir); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, pluginManagedDirEntireXD), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	if runtime.GOOS == windowsGOOS {
		if appData := os.Getenv("LOCALAPPDATA"); appData != "" {
			return filepath.Join(appData, pluginManagedDirEntireXD), nil
		}
		return filepath.Join(home, "AppData", "Local", pluginManagedDirEntireXD), nil
	}
	return filepath.Join(home, ".local", "share", pluginManagedDirEntireXD), nil
}

// PluginBinDir returns the managed install directory. Binaries (or symlinks)
// placed here are auto-discovered by the kubectl-style dispatcher because
// main.go prepends this dir to PATH before MaybeRunPlugin runs.
func PluginBinDir() (string, error) {
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, pluginManagedBinSubdir), nil
}

// PluginDataDir returns the per-plugin data directory for the given bare name
// (e.g. "pgr" for `entire-pgr`). The returned path is not created — that's
// the plugin's responsibility on first use.
func PluginDataDir(name string) (string, error) {
	parent, err := pluginParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, pluginManagedDataSubdir, name), nil
}

// EnsurePluginBinDir creates the managed install dir if it doesn't exist.
func EnsurePluginBinDir() (string, error) {
	dir, err := PluginBinDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create plugin bin dir: %w", err)
	}
	return dir, nil
}

// PrependPluginBinDirToPATH prepends the managed bin dir to the process's
// PATH so the kubectl dispatcher discovers managed-installed plugins. Idempotent
// against an already-prepended dir. Called from main.go before MaybeRunPlugin.
//
// Errors are non-fatal — managed installs simply won't be discoverable on this
// invocation, which mirrors the kubectl-style "drop a binary on PATH yourself"
// fallback.
func PrependPluginBinDirToPATH() {
	dir, err := PluginBinDir()
	if err != nil || dir == "" {
		return
	}
	cur := os.Getenv("PATH")
	sep := string(os.PathListSeparator)
	if cur == "" {
		_ = os.Setenv("PATH", dir)
		return
	}
	// Idempotent: if the first entry already matches, leave it alone.
	if i := strings.Index(cur, sep); i >= 0 && cur[:i] == dir {
		return
	}
	if cur == dir {
		return
	}
	_ = os.Setenv("PATH", dir+sep+cur)
}

// InstalledPlugin describes a single entry in the managed bin dir.
type InstalledPlugin struct {
	// Name is the bare plugin name (without the `entire-` prefix and any
	// platform-specific extension).
	Name string
	// Path is the absolute path inside the managed bin dir.
	Path string
	// Symlink is true when Path is a symlink to a source location elsewhere
	// (the typical local-dev install). LinkTarget is populated in that case.
	Symlink    bool
	LinkTarget string
}

// ListInstalledPlugins enumerates entries in the managed bin dir whose name
// starts with `entire-`. Sorted by bare name. A missing dir returns no error
// and an empty slice.
func ListInstalledPlugins() ([]*InstalledPlugin, error) {
	dir, err := PluginBinDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read plugin bin dir: %w", err)
	}

	var out []*InstalledPlugin
	for _, e := range entries {
		full := e.Name()
		if !strings.HasPrefix(full, pluginBinaryPrefix) {
			continue
		}
		bare := bareNameFromBinaryName(full)
		if bare == "" {
			continue
		}
		path := filepath.Join(dir, full)
		info, err := os.Lstat(path)
		if err != nil {
			continue
		}
		ip := &InstalledPlugin{Name: bare, Path: path}
		if info.Mode()&os.ModeSymlink != 0 {
			ip.Symlink = true
			if target, err := os.Readlink(path); err == nil {
				ip.LinkTarget = target
			}
		}
		out = append(out, ip)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FindInstalledPlugin returns the entry for the given bare name, or nil if
// it isn't installed in the managed dir.
func FindInstalledPlugin(name string) (*InstalledPlugin, error) {
	all, err := ListInstalledPlugins()
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, nil //nolint:nilnil // not-installed signal
}

// InstallPluginOptions configures InstallPluginFromPath.
type InstallPluginOptions struct {
	// SourcePath is the absolute (or working-dir-relative) path to the plugin
	// executable. Its basename — minus any platform extension — must match
	// `entire-<name>` so the dispatcher can resolve it.
	SourcePath string
	// Force replaces an already-installed plugin with the same name.
	Force bool
}

// InstallPluginFromPath symlinks SourcePath into the managed bin dir. The
// caller is responsible for built-in conflict checks (resolvePlugin already
// gates dispatch on rootCmd.Find — installing a name that shadows a built-in
// is allowed but the built-in still wins at runtime).
func InstallPluginFromPath(opts InstallPluginOptions) (*InstalledPlugin, error) {
	src, err := filepath.Abs(opts.SourcePath)
	if err != nil {
		return nil, fmt.Errorf("resolve source path: %w", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("stat source: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("source must be a file, got directory: %s", src)
	}
	base := filepath.Base(src)
	bare := bareNameFromBinaryName(base)
	if bare == "" {
		return nil, fmt.Errorf("source basename %q must start with %q", base, pluginBinaryPrefix)
	}
	if runtime.GOOS != windowsGOOS && info.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("source %s is not executable (chmod +x)", src)
	}

	binDir, err := EnsurePluginBinDir()
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(binDir, base)

	if _, err := os.Lstat(dest); err == nil {
		if !opts.Force {
			return nil, fmt.Errorf("plugin %q already installed; use --force to replace", bare)
		}
		if err := os.Remove(dest); err != nil {
			return nil, fmt.Errorf("remove existing entry: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat install destination: %w", err)
	}

	if err := os.Symlink(src, dest); err != nil {
		return nil, fmt.Errorf("symlink plugin: %w", err)
	}
	return FindInstalledPlugin(bare)
}

// RemoveInstalledPlugin removes the managed-dir entry for name. Symlinks are
// unlinked without touching the source file.
func RemoveInstalledPlugin(name string) error {
	p, err := FindInstalledPlugin(name)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("plugin %q is not installed in the managed directory", name)
	}
	if err := os.Remove(p.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove plugin entry: %w", err)
	}
	return nil
}

// bareNameFromBinaryName turns "entire-pgr" or "entire-pgr.exe" into "pgr".
// Returns "" if the input doesn't match the expected shape.
func bareNameFromBinaryName(base string) string {
	if !strings.HasPrefix(base, pluginBinaryPrefix) {
		return ""
	}
	cleaned := stripPluginExeExt(base)
	bare := strings.TrimPrefix(cleaned, pluginBinaryPrefix)
	if bare == "" {
		return ""
	}
	return bare
}

// stripPluginExeExt drops Windows executable extensions (.exe, .bat, .cmd).
// On Unix this is a no-op for typical binaries with no extension. We don't
// reuse external.StripExeExt to keep this layer independent of the agent
// package and avoid an import cycle if the agent package ever grows one.
func stripPluginExeExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".bat", ".cmd":
		return strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name
}
