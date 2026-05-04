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
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

// Plugin dispatch — kubectl-style. When the user invokes `entire <name>` and
// <name> isn't a built-in subcommand, look for an `entire-<name>` binary on
// PATH and exec it with the remaining args. Stdio and exit codes pass
// through. Binaries matching the agent protocol prefix are ignored here —
// they're handled by the external agent registry.
const (
	pluginBinaryPrefix      = "entire-"
	agentPluginBinaryPrefix = "entire-agent-"
)

// MaybeDispatchPlugin returns (true, exitCode) when an external plugin
// handled the invocation. On launch failure (e.g. missing executable bit)
// returns (true, 1) after printing to stderr. On no-match returns (false, 0)
// so the caller can fall through to Cobra.
func MaybeDispatchPlugin(ctx context.Context, rootCmd *cobra.Command, args []string) (handled bool, exitCode int) {
	binPath, pluginArgs, ok := resolvePlugin(rootCmd, args)
	if !ok {
		return false, 0
	}
	pluginName := args[0]
	exitCode = runPlugin(ctx, binPath, pluginArgs)
	maybeTrackPluginInvocation(ctx, pluginName)
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
	binPath, err := exec.LookPath(pluginBinaryPrefix + name)
	if err != nil {
		return "", nil, false
	}
	if isAgentProtocolBinary(binPath) {
		return "", nil, false
	}
	return binPath, args[1:], true
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
func runPlugin(ctx context.Context, binPath string, args []string) int {
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "ENTIRE_CLI_VERSION="+versioninfo.Version)
	if repoRoot, err := paths.WorktreeRoot(ctx); err == nil {
		cmd.Env = append(cmd.Env, "ENTIRE_REPO_ROOT="+repoRoot)
	}

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
