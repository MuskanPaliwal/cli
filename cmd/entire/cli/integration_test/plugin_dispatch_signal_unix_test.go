//go:build integration && !windows

package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPluginDispatch_SigintReachesPlugin asserts that when the parent CLI
// receives SIGINT, the running plugin gets a chance to handle it (rather
// than being SIGKILL'd). The contract: runPlugin uses CommandContext with a
// custom Cancel that sends SIGINT, plus terminal SIGINT reaches the child
// directly via the shared process group. Either path getting the signal to
// the child is acceptable; both being missing is the regression we guard.
func TestPluginDispatch_SigintReachesPlugin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	signalFile := filepath.Join(dir, "got-sigint.txt")

	// The plugin installs a SIGINT trap, writes a "ready" marker after the
	// trap is in place, then loops. The test waits for "ready" before
	// signalling so the trap is guaranteed to be installed (avoids racing
	// SIGINT against shell startup). On SIGINT the plugin writes the
	// "trapped" marker and exits 130. Without a working signal path the
	// parent's WaitDelay (5s) would expire and the child would be SIGKILL'd
	// with no marker.
	readyFile := filepath.Join(dir, "ready.txt")
	body := fmt.Sprintf(
		"#!/bin/sh\ntrap 'echo trapped > %q; exit 130' INT\n"+
			"echo ready > %q\n"+
			"i=0\nwhile [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done\nexit 0\n",
		signalFile, readyFile,
	)
	if err := os.WriteFile(filepath.Join(dir, "entire-trapint"), []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write plugin: %v", err)
	}

	cmd := exec.Command(getTestBinary(), "trapint")
	cmd.Env = pathWith(dir)
	var pStderr bytes.Buffer
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &pStderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for the plugin to install its trap before signalling.
	if !waitForFile(readyFile, 3*time.Second) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("plugin never reached ready state\nparent stderr:\n%s", pStderr.String())
	}

	// Send SIGINT only to the parent (entire) PID. The child must learn
	// about it through the parent's context-cancel handler invoking
	// exec.Cmd.Cancel, which sends SIGINT to the child.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal parent: %v", err)
	}

	if !waitForFile(signalFile, 5*time.Second) {
		_ = cmd.Wait()
		t.Fatalf("plugin never observed SIGINT — marker missing\nparent stderr:\n%s", pStderr.String())
	}
	_ = cmd.Wait()

	contents, err := os.ReadFile(signalFile)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got := strings.TrimSpace(string(contents)); got != "trapped" {
		t.Errorf("marker = %q, want %q", got, "trapped")
	}
}

// waitForFile polls until path exists or the deadline elapses. Returns true
// if the file appeared, false on timeout.
func waitForFile(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
