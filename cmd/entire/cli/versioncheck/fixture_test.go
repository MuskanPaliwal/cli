package versioncheck

import (
	"context"
	"io"
	"strings"
	"testing"
)

// autoUpdateFixture wires the test seams for maybeAutoUpdate.
type autoUpdateFixture struct {
	installCalls int
	installErr   error
	lastCommand  string
	chooseValue  AutoUpdateAction
	chooseErr    error
	lastCmdStr   string
}

func newAutoUpdateFixture(t *testing.T) *autoUpdateFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envKillSwitch, "")
	// Force interactive mode on by default; individual tests can opt out.
	t.Setenv("ENTIRE_TEST_TTY", "1")

	f := &autoUpdateFixture{chooseValue: autoUpdateActionUpdate}

	origRun := runInstaller
	runInstaller = func(_ context.Context, cmd string) error {
		f.installCalls++
		f.lastCommand = cmd
		return f.installErr
	}
	origChoose := chooseUpdate
	chooseUpdate = func(_ context.Context, _, _, cmdStr string) (AutoUpdateAction, error) {
		f.lastCmdStr = cmdStr
		return f.chooseValue, f.chooseErr
	}
	origIsTerminalOut := isTerminalOut
	isTerminalOut = func(_ io.Writer) bool { return true }

	t.Cleanup(func() {
		runInstaller = origRun
		chooseUpdate = origChoose
		isTerminalOut = origIsTerminalOut
	})
	return f
}

// setExecutable overrides the executable-path seam for the test's lifetime.
func setExecutable(t *testing.T, fn func() (string, error)) {
	t.Helper()
	orig := executablePath
	executablePath = fn
	t.Cleanup(func() { executablePath = orig })
}

// setExecutablePath points the install-manager detector at path.
func setExecutablePath(t *testing.T, path string) {
	t.Helper()
	setExecutable(t, func() (string, error) { return path, nil })
}

// assertPrintOnly checks that maybeAutoUpdate did not run the installer and
// instead printed the "To update, run" hint carrying wantCmd.
func assertPrintOnly(t *testing.T, f *autoUpdateFixture, action AutoUpdateAction, out, wantCmd string) {
	t.Helper()
	if f.installCalls != 0 {
		t.Errorf("installer called %d times, want 0", f.installCalls)
	}
	if action != autoUpdateActionSkip {
		t.Errorf("action = %q, want %q", action, autoUpdateActionSkip)
	}
	if !strings.Contains(out, "To update, run") {
		t.Errorf("missing manual-update hint: %q", out)
	}
	if !strings.Contains(out, "  "+wantCmd) {
		t.Errorf("manual hint missing installer command %q: %q", wantCmd, out)
	}
}

// isolateMiseInstallEnv clears the mise install roots so a relocated mise on
// the host cannot claim a test's exec path. MISE_INSTALLS_DIR is the only
// probe variable whose value becomes a root verbatim — MISE_DATA_DIR gets
// "installs" appended and Scoop's four get "apps", and that mandatory extra
// segment is what stops them prefixing an arbitrary path. So the rows most
// exposed are the ones asserting that NO probe matched, and the variable that
// hijacks them is this one.
//
// Lives here, untagged, because both platforms need it: the unix table test
// calls it directly and isolateWindowsInstallEnv wraps it.
func isolateMiseInstallEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MISE_INSTALLS_DIR", "")
	t.Setenv("MISE_DATA_DIR", "")
}
