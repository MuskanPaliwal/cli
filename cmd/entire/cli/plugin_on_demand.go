package cli

import (
	"context"
	"fmt"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

// onDemandPluginInstall shares the normal install workflow, including index
// overrides, name/checksum validation and dependency confirmation. Tests replace
// it to exercise the prompt and dispatch without downloading real releases.
var onDemandPluginInstall = runRemoteInstall

func installMissingPlugin(ctx context.Context, rootCmd *cobra.Command, name string) (string, error) {
	if !interactive.CanPromptInteractively() {
		return "", fmt.Errorf("the entire-%s plugin is not installed; run 'entire plugin install %s' and retry", name, name)
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("install plugin: %w", err)
	}
	confirmed := true
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewConfirm().Title(fmt.Sprintf("Install the entire-%s plugin?", name)).Value(&confirmed),
	)).WithInput(rootCmd.InOrStdin()).WithOutput(rootCmd.ErrOrStderr())
	if err := form.RunWithContext(ctx); err != nil {
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
