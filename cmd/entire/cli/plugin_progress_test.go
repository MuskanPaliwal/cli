package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestPluginStepPlainOutputIsImmediate(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	stop := startPluginStep(withPluginProgress(t.Context(), &out), "Downloading plugin archive...")
	if got := out.String(); got != "Downloading plugin archive...\n" {
		t.Fatalf("status must be visible before work completes, got %q", got)
	}
	stop()
	stop()
	if strings.Count(out.String(), "Downloading") != 1 || strings.Contains(out.String(), "\x1b") {
		t.Fatalf("plain progress duplicated output or wrote terminal escapes: %q", out.String())
	}
}

func TestPluginInstallReportsStagesOnStderr(t *testing.T) { //nolint:paralleltest // isolates managed plugins and index cache
	withIsolatedPluginEnv(t)
	withIndexCache(t)
	repoURL, _ := newDemoPluginRepo(t, []string{remoteTestTagOld}, "0.1.0")
	indexURL, _ := newIndexRepo(t, fmt.Sprintf(`{"version":1,"plugins":[{"name":"demo","repo_url":%q}]}`, repoURL))
	cmd := newPluginInstallCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	err := runRemoteInstall(t.Context(), cmd, installSource{Kind: installFromIndex, Ref: "demo"}, remoteInstallFlags{index: indexURL})
	if err != nil {
		t.Fatal(err)
	}
	stages := []string{
		"Checking plugin index...",
		"Finding latest plugin release...",
		"Fetching plugin metadata for v0.1.0...",
		"Locating plugin release files...",
		"Downloading plugin archive...",
		"Verifying plugin checksum...",
		"Installing entire-demo v0.1.0...",
	}
	if got, want := errOut.String(), strings.Join(stages, "\n")+"\n"; got != want {
		t.Fatalf("install progress:\ngot %q\nwant %q", got, want)
	}
	if !strings.HasPrefix(out.String(), `Installed plugin "demo" v0.1.0 from `) || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("stdout should contain only the install result: %q", out.String())
	}
}
