package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestMaybeRunPlugin_MissingGraphNonInteractive(t *testing.T) { //nolint:paralleltest // isolates PATH and terminal detection
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ENTIRE_TEST_TTY", "0")
	// The managed dir is consulted before the prompt (see installMissingPlugin),
	// and it is NOT covered by the testdirs fallback — pluginParentDir reads
	// $ENTIRE_PLUGIN_DIR/$XDG_DATA_HOME and the home dir itself. Without this
	// the test reads the developer's real plugins and passes only on a machine
	// that happens not to have graph installed.
	withPluginDir(t)
	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code := MaybeRunPlugin(t.Context(), root, []string{"graph", "search", "hello"})
	if !handled || code != 1 {
		t.Fatalf("handled=%v code=%d, want true, 1", handled, code)
	}
	if !strings.Contains(stderr.String(), "entire plugin install graph") {
		t.Fatalf("missing installation hint: %q", stderr.String())
	}
}

func TestMaybeRunPlugin_InstallGraphAndRun(t *testing.T) { //nolint:paralleltest // isolates environment and installer seam
	for _, tc := range []struct {
		name          string
		answer        string
		installErr    error
		cancelInstall bool
		pluginCode    int
		wantCode      int
		wantInstall   bool
		wantRun       bool
	}{
		{name: "enter accepts default yes", answer: "\n", wantInstall: true, wantRun: true},
		{name: "explicit yes preserves exit code", answer: "y\n", pluginCode: 42, wantCode: 42, wantInstall: true, wantRun: true},
		{name: "cancelled install stays quiet", answer: "y\n", cancelInstall: true, installErr: context.Canceled, wantCode: ExitPluginSignalled, wantInstall: true},
		{name: "cancelled dependency confirmation does not run", answer: "y\n", cancelInstall: true, wantCode: ExitPluginSignalled, wantInstall: true},
		{name: "EOF declines", answer: "", wantCode: 1},
		{name: "no cancels", answer: "n\n", wantCode: 1},
		{name: "failed install does not run", answer: "\n", installErr: errors.New("download failed"), wantCode: 1, wantInstall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			t.Setenv("ENTIRE_PLUGIN_DIR", filepath.Join(dir, "managed"))
			t.Setenv("ENTIRE_TEST_TTY", "1")
			t.Setenv("ACCESSIBLE", "1")
			t.Setenv("ENTIRE_TELEMETRY_OPTOUT", "1")
			interceptVersionCheck(t)
			argFile := filepath.Join(dir, "args.txt")
			sourceDir := t.TempDir()
			source := writePluginBinary(t, sourceDir, "entire-graph", argFile, tc.pluginCode)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			installCalls := 0
			original := onDemandPluginInstall
			onDemandPluginInstall = func(_ context.Context, cmd *cobra.Command, src installSource, flags remoteInstallFlags) error {
				installCalls++
				if tc.cancelInstall {
					cancel()
				}
				if src.Kind != installFromIndex || src.Ref != "graph" || flags != (remoteInstallFlags{}) {
					t.Fatalf("unexpected install request: %+v %+v", src, flags)
				}
				if tc.installErr != nil {
					return tc.installErr
				}
				_, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: source})
				fmt.Fprintln(cmd.OutOrStdout(), "Installed graph")
				return err
			}
			t.Cleanup(func() { onDemandPluginInstall = original })
			root := newTestRoot()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			originalInput := openPluginPromptTerminal
			openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
				return pluginPromptTerminal{in: io.NopCloser(strings.NewReader(tc.answer))}, nil
			}
			t.Cleanup(func() { openPluginPromptTerminal = originalInput })
			data := strings.NewReader("plugin data\n")
			root.SetIn(data)
			args := []string{"graph", "search", "two words", "--json", "--", "$(untouched)", ""}
			handled, code := MaybeRunPlugin(ctx, root, args)
			if !handled || code != tc.wantCode {
				t.Fatalf("handled=%v code=%d, want true, %d; stderr=%s", handled, code, tc.wantCode, &stderr)
			}
			if (installCalls == 1) != tc.wantInstall {
				t.Errorf("install calls=%d, want install=%v", installCalls, tc.wantInstall)
			}
			if !strings.Contains(stderr.String(), "Install the entire-graph plugin?") || !strings.Contains(stderr.String(), "[Y/n]") {
				t.Errorf("missing Yes-default prompt: %q", stderr.String())
			}
			if data.Len() != len("plugin data\n") {
				t.Error("confirmation consumed plugin stdin")
			}
			if stdout.Len() != 0 {
				t.Errorf("installation polluted stdout: %q", stdout.String())
			}
			got, err := os.ReadFile(argFile)
			if tc.wantRun {
				if err != nil || string(got) != strings.Join(args[1:], "\n")+"\n" {
					t.Errorf("forwarded args=%q err=%v", got, err)
				}
			} else if !os.IsNotExist(err) {
				t.Errorf("plugin unexpectedly ran: args=%q err=%v", got, err)
			}
			if tc.cancelInstall && strings.Contains(stderr.String(), "context canceled") {
				t.Errorf("raw cancellation: %s", &stderr)
			}
			if tc.installErr != nil && !tc.cancelInstall && !strings.Contains(stderr.String(), tc.installErr.Error()) {
				t.Errorf("missing install failure: %q", stderr.String())
			}
		})
	}
}

func TestMaybeRunPlugin_GraphInstalledSkipsPrompt(t *testing.T) { //nolint:paralleltest // isolates PATH and version check
	dir := t.TempDir()
	argFile := filepath.Join(dir, "args.txt")
	writePluginBinary(t, dir, "entire-graph", argFile, 0)
	t.Setenv("PATH", dir)
	interceptVersionCheck(t)
	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code := MaybeRunPlugin(t.Context(), root, []string{"graph", "--help"})
	if !handled || code != 0 || stderr.Len() != 0 {
		t.Fatalf("handled=%v code=%d stderr=%q", handled, code, stderr.String())
	}
}

func TestResolvePlugin_OnDemandEligibility(t *testing.T) { //nolint:paralleltest // isolates PATH
	t.Setenv("PATH", t.TempDir())
	for _, args := range [][]string{nil, {"--help"}, {"other-plugin"}, {"Graph"}, {"agent-graph"}, {"session", "graph"}} {
		if _, _, ok := resolvePlugin(newTestRoot(), args); ok {
			t.Errorf("unexpected plugin resolution for %q", args)
		}
	}
	root := newTestRoot()
	root.AddCommand(&cobra.Command{Use: "graph"})
	if _, _, ok := resolvePlugin(root, []string{"graph", "search"}); ok {
		t.Fatal("built-in graph must take precedence over on-demand installation")
	}
}

// A managed entry PATH cannot reach is the case installMissingPlugin's return
// contract already promised: run it. Offering to install over it dead-ended,
// because an existing install needs --force and the on-demand path passes
// none — so the user answered Yes, waited for the index and metadata fetches,
// and got "already installed; use --force to replace".
func TestMaybeRunPlugin_GraphInManagedDirIsRunNotReinstalled(t *testing.T) { //nolint:paralleltest // isolates PATH and managed plugins
	withIsolatedPluginEnv(t)
	interceptVersionCheck(t)
	binDir, err := EnsurePluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	argFile := filepath.Join(t.TempDir(), "args.txt")
	writePluginBinary(t, binDir, "entire-graph", argFile, 0)
	// Deliberately NOT on PATH: this is the managed bin dir that could not be
	// prepended at startup.
	if _, lookErr := exec.LookPath("entire-graph"); lookErr == nil {
		t.Fatal("precondition: entire-graph must not resolve through PATH")
	}

	t.Setenv("ENTIRE_TEST_TTY", "1")
	t.Setenv("ACCESSIBLE", "1")
	originalTerminal := openPluginPromptTerminal
	openPluginPromptTerminal = func() (pluginPromptTerminal, error) {
		t.Error("an already-installed plugin must not prompt for installation")
		return pluginPromptTerminal{in: io.NopCloser(strings.NewReader("n\n"))}, nil
	}
	t.Cleanup(func() { openPluginPromptTerminal = originalTerminal })
	originalInstall := onDemandPluginInstall
	onDemandPluginInstall = func(context.Context, *cobra.Command, installSource, remoteInstallFlags) error {
		t.Error("an already-installed plugin must not be reinstalled")
		return nil
	}
	t.Cleanup(func() { onDemandPluginInstall = originalInstall })

	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	handled, code := MaybeRunPlugin(t.Context(), root, []string{"graph", "search", "hello"})
	if !handled || code != 0 {
		t.Fatalf("handled=%v code=%d, want true, 0; stderr=%s", handled, code, &stderr)
	}
	got, err := os.ReadFile(argFile)
	if err != nil || string(got) != "search\nhello\n" {
		t.Fatalf("managed entry did not run with the forwarded args: %q %v", got, err)
	}
	// The announcement names the command, not just its arguments — a bare
	// `entire graph` used to print "Running plugin with command:" and stop.
	if !strings.Contains(stderr.String(), "Running entire graph search hello\n") {
		t.Errorf("forwarded command was not announced: %q", stderr.String())
	}
}

// A bare `entire graph` has no arguments to forward, which is the case the
// announcement got wrong: joining only the arguments left a trailing colon
// and nothing after it.
func TestMaybeRunPlugin_BareGraphAnnouncesTheCommand(t *testing.T) { //nolint:paralleltest // isolates PATH and managed plugins
	withIsolatedPluginEnv(t)
	interceptVersionCheck(t)
	binDir, err := EnsurePluginBinDir()
	if err != nil {
		t.Fatal(err)
	}
	writePluginBinary(t, binDir, "entire-graph", filepath.Join(t.TempDir(), "args.txt"), 0)

	root := newTestRoot()
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	if handled, code := MaybeRunPlugin(t.Context(), root, []string{"graph"}); !handled || code != 0 {
		t.Fatalf("handled=%v code=%d; stderr=%s", handled, code, &stderr)
	}
	if !strings.Contains(stderr.String(), "Running entire graph\n") {
		t.Errorf("bare command was not announced: %q", stderr.String())
	}
}
