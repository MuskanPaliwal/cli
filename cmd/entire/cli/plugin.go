package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/versioncheck"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

// External-command resolution, kubectl-style. When the user invokes
// `entire <name>` and <name> isn't a built-in subcommand, look for an
// `entire-<name>` binary on PATH and exec it with the remaining args.
// Stdio and exit codes pass through. Binaries matching the agent
// protocol prefix are reserved for the external agent registry and
// skipped here.
const (
	pluginBinaryPrefix      = "entire-"
	agentPluginBinaryPrefix = "entire-agent-"
)

// MaybeRunPlugin returns (true, exitCode) when an external command was
// resolved and run. On launch failure (e.g. missing executable bit)
// returns (true, 1) after printing to stderr. On no-match returns
// (false, 0) so the caller can fall through to Cobra.
//
// Telemetry and the version-check notice mirror Cobra's PersistentPostRun
// behavior for built-ins: both fire only on a successful (exit-0) run.
func MaybeRunPlugin(ctx context.Context, rootCmd *cobra.Command, args []string) (handled bool, exitCode int) {
	binPath, pluginArgs, ok := resolvePlugin(rootCmd, args)
	if !ok {
		return false, 0
	}
	pluginName := args[0]
	exitCode = runPlugin(ctx, pluginName, binPath, pluginArgs)
	if exitCode == 0 {
		maybeTrackPluginInvocation(ctx, pluginName)
		versioncheck.CheckAndNotify(ctx, os.Stdout, versioninfo.Version)
	}
	return true, exitCode
}

// maybeTrackPluginInvocation fires telemetry only for plugins on the
// official allowlist. Third-party plugin names are never sent — see
// IsOfficialPlugin for the rationale.
func maybeTrackPluginInvocation(ctx context.Context, pluginName string) {
	if !IsOfficialPlugin(pluginName) {
		return
	}
	s, err := LoadEntireSettings(ctx)
	if err != nil {
		return
	}
	if s.Telemetry == nil || !*s.Telemetry {
		return
	}
	telemetry.TrackPluginDetached(pluginName, s.Enabled, versioninfo.Version)
}

func resolvePlugin(rootCmd *cobra.Command, args []string) (binPath string, pluginArgs []string, ok bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	name := args[0]
	if !isPluginCandidate(name) {
		return "", nil, false
	}
	// Built-in commands always win.
	if cmd, _, err := rootCmd.Find(args); err == nil && cmd != rootCmd {
		return "", nil, false
	}
	binName := pluginBinaryPrefix + name
	binPath, err := exec.LookPath(binName)
	if err != nil {
		// LookPath conflates "not on PATH" with "found but not executable".
		// Distinguish: if a file with this name exists on PATH but isn't
		// executable, surface that as a launch error rather than falling
		// through to Cobra's generic unknown-command path.
		if p, found := findInaccessiblePlugin(binName); found {
			return p, args[1:], true
		}
		return "", nil, false
	}
	if isAgentProtocolBinary(binPath) {
		return "", nil, false
	}
	return binPath, args[1:], true
}

// findInaccessiblePlugin scans PATH for a non-directory file with the
// given name. Only meaningful after exec.LookPath has already failed —
// indicates the file exists but lacks the executable bit (or the
// equivalent platform-specific accessibility).
func findInaccessiblePlugin(filename string) (string, bool) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, filename)
		info, err := os.Stat(candidate) //nolint:gosec // PATH entries are user-trusted; scanning them is the point.
		if err != nil || info.IsDir() {
			continue
		}
		return candidate, true
	}
	return "", false
}

func isPluginCandidate(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "-") {
		return false
	}
	// `agent-*` is reserved for the external agent protocol.
	if strings.HasPrefix(name, "agent-") {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	// "." and ".." would collapse out of any joined path (e.g. for
	// ENTIRE_PLUGIN_DATA_DIR), breaking the per-plugin isolation contract.
	if name == "." || name == ".." {
		return false
	}
	return true
}

// isAgentProtocolBinary returns true when the binary name is reserved for
// the external agent protocol. Strip Windows extensions first so
// `entire-agent-foo.exe` is also recognized.
func isAgentProtocolBinary(binPath string) bool {
	base := external.StripExeExt(filepath.Base(binPath))
	return strings.HasPrefix(base, agentPluginBinaryPrefix)
}

// runPlugin executes the resolved plugin binary, propagating its exit code.
// On context cancellation the child gets SIGINT (with a 5s grace before the
// runtime falls back to SIGKILL) so plugins can clean up. Terminal signals
// reach the child directly via the shared process group.
func runPlugin(ctx context.Context, pluginName, binPath string, args []string) int {
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Use upsertEnv (not append) so the CLI-computed values can't be
	// shadowed by pre-existing ENTIRE_* entries inherited from the
	// user's shell. getenv() typically returns the first match.
	env := upsertEnv(os.Environ(), "ENTIRE_CLI_VERSION", versioninfo.Version)
	if repoRoot, err := paths.WorktreeRoot(ctx); err == nil {
		env = upsertEnv(env, "ENTIRE_REPO_ROOT", repoRoot)
	}
	// Per-plugin durable storage. Passed regardless of where the binary lives
	// so plugins installed via raw PATH and via `entire plugin install` get
	// the same contract. The dir is not pre-created — that's the plugin's
	// responsibility on first use.
	//
	// PluginDataDir can only fail in degenerate environments (e.g. no
	// resolvable home dir). The plugin name itself has already passed
	// isPluginCandidate validation in resolvePlugin, so the validator
	// branch can't fire here. We warn-and-proceed rather than refuse to
	// launch: a HOME-less environment is the user's problem to surface,
	// not a reason to break plugins that don't read the var.
	if dataDir, err := PluginDataDir(pluginName); err == nil {
		env = upsertEnv(env, pluginEnvPluginData, dataDir)
	} else {
		fmt.Fprintf(os.Stderr, "warning: ENTIRE_PLUGIN_DATA_DIR not set for plugin %q: %v\n", pluginName, err)
	}
	cmd.Env = env

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		// Prefix with the plugin name so users can tell parent vs child
		// errors apart in mixed stderr.
		fmt.Fprintf(os.Stderr, "Failed to run plugin %s: %v\n", filepath.Base(binPath), err)
		return 1
	}
	return 0
}
