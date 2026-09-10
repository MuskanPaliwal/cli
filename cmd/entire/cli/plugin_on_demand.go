package cli

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

// onDemandPluginInstall shares the normal install workflow, including index
// overrides, name/checksum validation and dependency confirmation. Tests replace
// it to exercise the prompt and dispatch without downloading real releases.
var onDemandPluginInstall = runRemoteInstall

func installMissingPlugin(ctx context.Context, rootCmd *cobra.Command, name string) (string, error) {
	// The plugin may already be in the managed directory and merely
	// unreachable through PATH: a local-dev symlink whose target moved, or a
	// managed bin dir that could not be prepended at startup. Offering to
	// install over it is a dead end — installRepoAtTag refuses an existing
	// install without --force, so the user answers Yes, waits for three
	// network round-trips and gets "already installed; use --force to
	// replace". Execute the managed entry instead, which is what this
	// function's own return contract promises below.
	//
	// A listing error falls through to the install rather than failing here:
	// the install path reads the same directory and reports the problem in
	// terms of what it was trying to do.
	if installed, err := FindInstalledPlugin(name); err == nil && installed != nil {
		return installed.Path, nil
	}
	if !interactive.CanPromptInteractively() {
		return "", fmt.Errorf("the entire-%s plugin is not installed; run 'entire plugin install %s' and retry", name, name)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}
	confirmed, err := runPluginConfirm(ctx, rootCmd.ErrOrStderr(), fmt.Sprintf("Install the entire-%s plugin?", name), true)
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "", handleFormCancellation(rootCmd.ErrOrStderr(), "Install", err)
	}
	if !confirmed {
		fmt.Fprintln(rootCmd.ErrOrStderr(), "Install cancelled.")
		return "", nil
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}

	// Keep install progress off stdout: the original command may emit JSON or
	// be piped to another tool. Do not parse any of the plugin's arguments.
	cmd := newPluginInstallCmd()
	cmd.SetOut(rootCmd.ErrOrStderr())
	cmd.SetErr(rootCmd.ErrOrStderr())
	if err := onDemandPluginInstall(ctx, cmd, installSource{Kind: installFromIndex, Ref: name}, remoteInstallFlags{}); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}
	installed, err := FindInstalledPlugin(name)
	if err != nil {
		return "", err
	}
	if installed == nil {
		return "", fmt.Errorf("the entire-%s plugin was not installed; run 'entire plugin install %s' and retry", name, name)
	}
	// Execute the managed entry directly, even if the managed directory could
	// not be prepended to PATH at startup.
	return installed.Path, nil
}
