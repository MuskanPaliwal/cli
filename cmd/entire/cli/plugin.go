package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
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
// handled the invocation, and (false, 0) otherwise (in which case the caller
// should fall through to normal Cobra execution).
func MaybeDispatchPlugin(ctx context.Context, rootCmd *cobra.Command, args []string) (handled bool, exitCode int) {
	binPath, pluginArgs, ok := resolvePlugin(rootCmd, args)
	if !ok {
		return false, 0
	}
	return true, runPlugin(ctx, binPath, pluginArgs, os.Stdin, os.Stdout, os.Stderr)
}

// resolvePlugin decides whether args should be routed to an external plugin.
// It is split out from MaybeDispatchPlugin so tests can exercise resolution
// without spawning a subprocess.
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

// isPluginCandidate filters out args that obviously aren't plugin invocations:
// flags, empty strings, and names that would map onto agent protocol binaries
// or contain path separators.
func isPluginCandidate(name string) bool {
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "-") {
		return false
	}
	if strings.HasPrefix(name, "agent-") {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	return true
}

// isAgentProtocolBinary returns true when the binary name is reserved for the
// external agent protocol. We check both the literal name and the
// extension-stripped form so a user with `entire-agent-foo.exe` on Windows
// still gets filtered out.
func isAgentProtocolBinary(binPath string) bool {
	base := filepath.Base(binPath)
	if strings.HasPrefix(base, agentPluginBinaryPrefix) {
		return true
	}
	if strings.HasPrefix(stripPluginExeExt(base), agentPluginBinaryPrefix) {
		return true
	}
	return false
}

// stripPluginExeExt mirrors agent/external/discovery.stripExeExt for plugin
// name normalization on Windows.
func stripPluginExeExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".bat", ".cmd":
		return strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name
}

// runPlugin executes the resolved plugin binary, propagating its exit code.
// On context cancellation the child is sent SIGINT (with a short grace
// window before the runtime falls back to SIGKILL), so plugins get a chance
// to clean up. Terminal signals (Ctrl+C) reach the child directly through
// the foreground process group as well.
func runPlugin(ctx context.Context, binPath string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.CommandContext(ctx, binPath, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), "ENTIRE_CLI_VERSION="+versioninfo.Version)
	if repoRoot, err := paths.WorktreeRoot(ctx); err == nil {
		cmd.Env = append(cmd.Env, "ENTIRE_REPO_ROOT="+repoRoot)
	}

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "entire: failed to run plugin %s: %v\n", filepath.Base(binPath), err)
		return 1
	}
	return 0
}
