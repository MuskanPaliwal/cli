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

// These tests exercise the early-dispatch path in cmd/entire/main.go: the
// real `entire` binary is invoked with `entire <name>` arguments, and we
// verify a kubectl-style `entire-<name>` plugin on PATH is exec'd before
// Cobra handles the args. Coverage here complements the unit tests in
// cmd/entire/cli/plugin_test.go, which test resolvePlugin/runPlugin in
// isolation but cannot validate the main() wiring or exit-code propagation
// of the actual binary.

// writePluginScript creates a shell script plugin at dir/<binaryName> that
// records its argv to argFile (one line per arg) and exits with exitCode.
// On non-Unix platforms the test calling this is skipped.
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

// pathWith prepends dir to the current PATH and returns a slice suitable for
// passing as cmd.Env (as opposed to mutating the test process environment,
// which would break parallel tests).
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
	// A plugin shadowing a real built-in must NOT be invoked. If the plugin
	// runs, it exits 99 with sentinel output we can detect.
	body := "#!/bin/sh\necho 'plugin-shadowed-builtin'\nexit 99\n"
	if runtime.GOOS == "windows" {
		t.Skip("plugin shell-script harness only runs on Unix")
	}
	if err := os.WriteFile(filepath.Join(dir, "entire-version"), []byte(body), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write plugin: %v", err)
	}

	cmd := exec.Command(getTestBinary(), "version")
	cmd.Env = pathWith(dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("entire version failed: %v\nstderr: %s", err, stderr.String())
	}
	if strings.Contains(stdout.String(), "plugin-shadowed-builtin") {
		t.Errorf("built-in version was shadowed by entire-version plugin\nstdout: %s", stdout.String())
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
	// Cobra normally interprets --help itself. The dispatcher runs before
	// Cobra parses, so once we're routing to a plugin everything (including
	// flag-shaped args) must reach the child verbatim.
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
	// Just confirm the variable is set to *something* non-empty — value
	// depends on build-time linker flags and will be "dev" in tests.
	if strings.TrimPrefix(out, "ENTIRE_CLI_VERSION=") == "" {
		t.Errorf("ENTIRE_CLI_VERSION was empty")
	}
}

func TestPluginDispatch_AgentProtocolBinarySkipped(t *testing.T) {
	t.Parallel()
	// `entire agent-foo` must not be routed to entire-agent-foo (which is a
	// protocol binary, not a passthrough plugin). Cobra should see "agent-foo"
	// as an unknown command — the literal `agent` group exists, but
	// `agent-foo` is not a subcommand of agent and not a passthrough name.
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
}
