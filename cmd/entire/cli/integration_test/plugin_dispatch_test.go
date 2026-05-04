//go:build integration

package integration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Integration tests for the early-dispatch path in cmd/entire/main.go.
// They build and exec the real binary so the wiring (pre-Cobra dispatch,
// exit-code propagation, stdio passthrough, signal handling) is exercised
// end-to-end — unit tests in cmd/entire/cli/plugin_test.go can't.

// writePluginScript writes a shell script that records argv and exits
// with exitCode. Skips the calling test on Windows.
func writePluginScript(t *testing.T, dir, binaryName, argFile string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("plugin shell-script harness only runs on Unix")
	}
	path := filepath.Join(dir, binaryName)
	body := fmt.Sprintf(
		"#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\n"+
			"echo \"plugin stdout\"\n"+
			"echo \"plugin stderr\" 1>&2\n"+
			"exit %d\n",
		argFile, exitCode,
	)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write plugin %s: %v", path, err)
	}
	return path
}

// pathWith returns os.Environ with dir prepended to PATH. Returning a
// fresh env slice (rather than t.Setenv) keeps tests parallel-safe.
func pathWith(dir string) []string {
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + dir + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
			return env
		}
	}
	return append(env, "PATH="+dir)
}

func TestPluginDispatch_HappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	argFile := filepath.Join(dir, "argv.txt")
	writePluginScript(t, dir, "entire-pgr", argFile, 0)

	cmd := exec.Command(getTestBinary(), "pgr", "hello", "--flag", "value")
	cmd.Env = pathWith(dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("entire pgr failed: %v\nstderr: %s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "plugin stdout" {
		t.Errorf("stdout = %q, want %q", got, "plugin stdout")
	}
	if got := strings.TrimSpace(stderr.String()); got != "plugin stderr" {
		t.Errorf("stderr = %q, want %q", got, "plugin stderr")
	}
	argsBytes, err := os.ReadFile(argFile)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	if got := strings.TrimSpace(string(argsBytes)); got != "hello\n--flag\nvalue" {
		t.Errorf("plugin argv = %q, want %q", got, "hello\n--flag\nvalue")
	}
}

func TestPluginDispatch_ExitCodePropagation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePluginScript(t, dir, "entire-failing", filepath.Join(dir, "argv.txt"), 42)

	cmd := exec.Command(getTestBinary(), "failing")
	cmd.Env = pathWith(dir)
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-zero exit")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if got := exitErr.ExitCode(); got != 42 {
		t.Errorf("exit code = %d, want 42", got)
	}
}

func TestPluginDispatch_BuiltinWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// If the shadowing plugin ran, the parent's exit code would be 99
	// (writePluginScript bakes that in via the requested code).
	writePluginScript(t, dir, "entire-version", filepath.Join(dir, "argv.txt"), 99)

	cmd := exec.Command(getTestBinary(), "version")
	cmd.Env = pathWith(dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("entire version failed: %v\nstderr: %s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "argv.txt")); err == nil {
		t.Errorf("entire-version plugin was invoked but built-in must take precedence\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Entire CLI") {
		t.Errorf("expected built-in version output, got: %s", stdout.String())
	}
}

func TestPluginDispatch_PluginNotFound(t *testing.T) {
	t.Parallel()
	// PATH deliberately points at an empty dir so no plugin can resolve.
	dir := t.TempDir()

	cmd := exec.Command(getTestBinary(), "definitely-not-a-real-plugin-or-builtin")
	cmd.Env = pathWith(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatal("expected failure for unknown command")
	}
	// Cobra's normal error path should fire — the dispatcher must not have
	// swallowed the invocation.
	if !strings.Contains(stderr.String(), "unknown command") &&
		!strings.Contains(stderr.String(), "Invalid usage") {
		t.Errorf("expected Cobra unknown-command error, got stderr: %s", stderr.String())
	}
}

func TestPluginDispatch_FlagAfterPluginNameNotEatenByCobra(t *testing.T) {
	t.Parallel()
	// Once we're routing to a plugin, flag-shaped args must reach the
	// child verbatim — Cobra's --help/--version handlers must not see them.
	dir := t.TempDir()
	argFile := filepath.Join(dir, "argv.txt")
	writePluginScript(t, dir, "entire-passthrough", argFile, 0)

	cmd := exec.Command(getTestBinary(), "passthrough", "--help", "--version", "subcmd")
	cmd.Env = pathWith(dir)
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		t.Fatalf("entire passthrough failed: %v", err)
	}

	argsBytes, err := os.ReadFile(argFile)
	if err != nil {
		t.Fatalf("read argv file: %v", err)
	}
	want := "--help\n--version\nsubcmd"
	if got := strings.TrimSpace(string(argsBytes)); got != want {
		t.Errorf("plugin argv = %q, want %q (Cobra ate flags)", got, want)
	}
}

func TestPluginDispatch_StdinPassthrough(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("plugin shell-script harness only runs on Unix")
	}
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.txt")
	body := fmt.Sprintf("#!/bin/sh\ncat > %q\nexit 0\n", outFile)
	if err := os.WriteFile(filepath.Join(dir, "entire-stdincat"), []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write plugin: %v", err)
	}

	cmd := exec.Command(getTestBinary(), "stdincat")
	cmd.Env = pathWith(dir)
	cmd.Stdin = strings.NewReader("hello from parent stdin\n")
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		t.Fatalf("entire stdincat failed: %v", err)
	}

	got, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read stdin file: %v", err)
	}
	if want := "hello from parent stdin\n"; string(got) != want {
		t.Errorf("plugin stdin = %q, want %q", string(got), want)
	}
}

func TestPluginDispatch_EnvVarsForwarded(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("plugin shell-script harness only runs on Unix")
	}
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env.txt")
	body := fmt.Sprintf(
		"#!/bin/sh\necho \"ENTIRE_CLI_VERSION=$ENTIRE_CLI_VERSION\" > %q\nexit 0\n",
		envFile,
	)
	if err := os.WriteFile(filepath.Join(dir, "entire-envcheck"), []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write plugin: %v", err)
	}

	cmd := exec.Command(getTestBinary(), "envcheck")
	cmd.Env = pathWith(dir)
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		t.Fatalf("entire envcheck failed: %v", err)
	}

	got, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read env file: %v", err)
	}
	out := strings.TrimSpace(string(got))
	if !strings.HasPrefix(out, "ENTIRE_CLI_VERSION=") {
		t.Fatalf("env line missing prefix: %q", out)
	}
	// Value depends on build-time linker flags; just check it's non-empty.
	if strings.TrimPrefix(out, "ENTIRE_CLI_VERSION=") == "" {
		t.Errorf("ENTIRE_CLI_VERSION was empty")
	}
}

func TestPluginDispatch_AgentProtocolBinarySkipped(t *testing.T) {
	t.Parallel()
	// `entire-agent-*` is reserved for the protocol — never dispatched as
	// a passthrough plugin even when present on PATH.
	dir := t.TempDir()
	writePluginScript(t, dir, "entire-agent-foo", filepath.Join(dir, "argv.txt"), 0)

	cmd := exec.Command(getTestBinary(), "agent-foo")
	cmd.Env = pathWith(dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err == nil {
		t.Fatal("expected failure — entire-agent-* must not be dispatched as a plugin")
	}
	if _, err := os.Stat(filepath.Join(dir, "argv.txt")); err == nil {
		t.Error("entire-agent-foo was invoked but must have been skipped")
	}
	// Should fall through to Cobra's unknown-command path, not be eaten silently.
	if !strings.Contains(stderr.String(), "unknown command") &&
		!strings.Contains(stderr.String(), "Invalid usage") {
		t.Errorf("expected Cobra unknown-command error, got stderr: %s", stderr.String())
	}
}
