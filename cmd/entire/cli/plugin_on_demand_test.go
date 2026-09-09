package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestMaybeRunPlugin_MissingGraphNonInteractive(t *testing.T) { //nolint:paralleltest // isolates PATH and terminal detection
	t.Setenv("PATH", t.TempDir())
	t.Setenv("ENTIRE_TEST_TTY", "0")
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
		{name: "cancelled install stays quiet", answer: "y\n", cancelInstall: true, installErr: context.Canceled, wantCode: 1, wantInstall: true},
		{name: "cancelled dependency confirmation does not run", answer: "y\n", cancelInstall: true, wantCode: 1, wantInstall: true},
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
			originalInput := openPluginPromptInput
			openPluginPromptInput = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(tc.answer)), nil }
			t.Cleanup(func() { openPluginPromptInput = originalInput })
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
