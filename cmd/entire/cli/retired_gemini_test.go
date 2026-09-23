package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// retiredGeminiSettingsFixture is a .gemini/settings.json as Gemini CLI support
// left it, plus the things a user may have added around Entire's entries: a
// user hook in the same matcher, a matcher and a hook type Entire never owned,
// unknown per-entry fields, and unknown top-level keys.
func retiredGeminiSettingsFixture() string {
	wrapped := agent.WrapProductionJSONWarningHookCommand("entire hooks gemini session-end", agent.WarningFormatSingleLine)
	return `{
  "theme": "dark",
  "hooksConfig": {"enabled": true},
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {"name": "entire-session-start", "type": "command", "command": "entire hooks gemini session-start"},
          {"name": "my-hook", "type": "command", "command": "./my-start.sh", "timeout": 5000}
        ]
      },
      {
        "matcher": "resume",
        "hooks": [{"type": "command", "command": "./on-resume.sh"}]
      }
    ],
    "SessionEnd": [
      {"hooks": [{"type": "command", "command": ` + jsonQuote(wrapped) + `}]}
    ],
    "BeforeTool": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "entire hooks gemini before-tool"}]}
    ],
    "Notification": [
      {"hooks": [{"type": "command", "command": "notify-send gemini"}]}
    ]
  }
}`
}

func jsonQuote(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func TestStripRetiredGeminiHooks_RemovesOnlyEntireEntries(t *testing.T) {
	t.Parallel()

	out, changed, err := stripRetiredGeminiHooks([]byte(retiredGeminiSettingsFixture()))
	require.NoError(t, err)
	require.True(t, changed)

	// SessionEnd and BeforeTool held only Entire entries (one of them in the
	// production wrapper), so both hook types are dropped. The user's hook in
	// the shared matcher, the other matcher, the other hook type, unknown
	// entry fields, and unknown top-level keys survive.
	want := `{
  "theme": "dark",
  "hooksConfig": {"enabled": true},
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {"name": "my-hook", "type": "command", "command": "./my-start.sh", "timeout": 5000}
        ]
      },
      {
        "matcher": "resume",
        "hooks": [{"type": "command", "command": "./on-resume.sh"}]
      }
    ],
    "Notification": [
      {"hooks": [{"type": "command", "command": "notify-send gemini"}]}
    ]
  }
}`
	require.JSONEq(t, want, string(out))
}

func TestStripRetiredGeminiHooks_RemovesHooksKeyWhenNothingRemains(t *testing.T) {
	t.Parallel()

	input := `{
  "theme": "dark",
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "entire hooks gemini session-start"}]}],
    "AfterAgent": [{"hooks": [{"type": "command", "command": "entire hooks gemini after-agent"}]}]
  }
}`
	out, changed, err := stripRetiredGeminiHooks([]byte(input))
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"theme": "dark"}`, string(out))
}

// Old Entire versions wrote "enabled": true directly under "hooks", which
// Gemini CLI 0.33+ rejects because every hooks property must be an array.
// Rewriting the file with it left in place would hand the user a settings file
// Gemini refuses to load, so it goes along with Entire's entries — and on its
// own, since it is Entire's artifact too.
func TestStripRetiredGeminiHooks_DropsLegacyNonArrayHookFields(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ input, want string }{
		"alongside entire and user hooks": {
			input: `{"hooks": {"enabled": true, "SessionStart": [{"hooks": [
				{"type": "command", "command": "entire hooks gemini session-start"},
				{"type": "command", "command": "./my-start.sh"}]}]}}`,
			want: `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "./my-start.sh"}]}]}}`,
		},
		"alone": {
			input: `{"theme": "dark", "hooks": {"enabled": true}}`,
			want:  `{"theme": "dark"}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, changed, err := stripRetiredGeminiHooks([]byte(tc.input))
			require.NoError(t, err)
			require.True(t, changed)
			require.JSONEq(t, tc.want, string(out))
		})
	}
}

func TestStripRetiredGeminiHooks_NoEntireEntriesIsUnchanged(t *testing.T) {
	t.Parallel()

	for name, input := range map[string]string{
		"no hooks key":   `{"theme": "dark"}`,
		"only user hook": `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "./my-start.sh"}]}]}}`,
		// A launcher that runs the CLI for something other than a hook is not ours.
		"entire non-hook command": `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "entire status"}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, changed, err := stripRetiredGeminiHooks([]byte(input))
			require.NoError(t, err)
			require.False(t, changed)
			require.Nil(t, out, "nothing to rewrite, so no output")
		})
	}
}

func TestStripRetiredGeminiHooks_MalformedJSON(t *testing.T) {
	t.Parallel()

	for name, input := range map[string]string{
		"truncated document": `{"hooks": {"SessionStart": [`,
		"hooks not object":   `{"hooks": ["entire hooks gemini session-start"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, changed, err := stripRetiredGeminiHooks([]byte(input))
			require.Error(t, err)
			require.False(t, changed)
		})
	}
}

func TestRemoveRetiredGeminiHooks_MissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	changed, err := removeRetiredGeminiHooks(dir)
	require.NoError(t, err)
	require.False(t, changed)
	require.NoFileExists(t, filepath.Join(dir, ".gemini", "settings.json"))
}

func TestRemoveRetiredGeminiHooks_RewritesFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	settingsPath := filepath.Join(dir, ".gemini", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	require.NoError(t, os.WriteFile(settingsPath, []byte(retiredGeminiSettingsFixture()), 0o600))

	changed, err := removeRetiredGeminiHooks(dir)
	require.NoError(t, err)
	require.True(t, changed)

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	content := string(data)
	require.NotContains(t, content, "entire hooks gemini")
	require.Contains(t, content, "./my-start.sh")
	require.Contains(t, content, `"theme"`)

	// A second pass finds nothing left to remove and leaves the file alone.
	changed, err = removeRetiredGeminiHooks(dir)
	require.NoError(t, err)
	require.False(t, changed)
	again, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.Equal(t, content, string(again))
}

func TestRemoveRetiredGeminiHooks_MalformedFileIsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	settingsPath := filepath.Join(dir, ".gemini", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	require.NoError(t, os.WriteFile(settingsPath, []byte(`{"hooks":`), 0o600))

	changed, err := removeRetiredGeminiHooks(dir)
	require.Error(t, err)
	require.False(t, changed)
	require.Contains(t, err.Error(), "settings.json")

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.Equal(t, `{"hooks":`, string(data), "a file that fails to parse must not be rewritten")
}

// Hooks left behind in .gemini/settings.json keep invoking
// `entire hooks gemini <verb>` after support was removed; each must exit
// cleanly and silently instead of failing the user's Gemini event.
func TestHooksCmd_RetiredGeminiAgentIsSilentNoOp(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir. The temp repo has no external_agents
	// setting, so external discovery stays off and cannot claim the name.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	cmd := newHooksCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader(`{"session_id":"retired-gemini","hook_event_name":"SessionStart"}`))
	cmd.SetArgs([]string{string(retiredGeminiAgentName), "session-start"})

	require.NoError(t, cmd.Execute())
	require.Empty(t, stdout.String())
	require.Empty(t, stderr.String())
}

func TestHooksCmd_UnknownAgentStillErrors(t *testing.T) {
	// Cannot use t.Parallel(): t.Chdir.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	cmd := newHooksCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetIn(strings.NewReader(`{}`))
	cmd.SetArgs([]string{"no-such-agent", "stop"})

	err := cmd.Execute()
	require.Error(t, err)
	require.Contains(t, err.Error(), `unknown agent "no-such-agent"`)
}

// writeRetiredGeminiSettings writes content to dir/.gemini/settings.json.
func writeRetiredGeminiSettings(t *testing.T, dir, content string) string {
	t.Helper()
	settingsPath := filepath.Join(dir, ".gemini", "settings.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(settingsPath), 0o755))
	require.NoError(t, os.WriteFile(settingsPath, []byte(content), 0o600))
	return settingsPath
}

func TestCheckRetiredGeminiHooks(t *testing.T) {
	// Cannot use t.Parallel(): setupTestRepo uses t.Chdir.
	run := func(t *testing.T) string {
		t.Helper()
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		var out bytes.Buffer
		cmd.SetOut(&out)
		checkRetiredGeminiHooks(cmd)
		return out.String()
	}

	t.Run("removes entries", func(t *testing.T) {
		dir := setupTestRepo(t)
		settingsPath := writeRetiredGeminiSettings(t, dir, retiredGeminiSettingsFixture())

		out := run(t)
		require.Contains(t, out, "Gemini CLI hooks: RETIRED")
		require.Contains(t, out, "✓ Fixed: Entire's entries removed")

		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		require.NotContains(t, string(data), "entire hooks gemini")
		require.Contains(t, string(data), "./my-start.sh")
	})

	t.Run("nothing to remove is silent", func(t *testing.T) {
		setupTestRepo(t)
		require.Empty(t, run(t))
	})

	t.Run("malformed file reports failure", func(t *testing.T) {
		dir := setupTestRepo(t)
		writeRetiredGeminiSettings(t, dir, `{"hooks":`)

		out := run(t)
		require.Contains(t, out, "Gemini CLI hooks: CHECK FAILED")
		require.NotContains(t, out, "RETIRED")
	})
}

func TestUninstallRetiredGeminiHooks(t *testing.T) {
	t.Parallel()

	t.Run("removes entries", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		settingsPath := writeRetiredGeminiSettings(t, dir, retiredGeminiSettingsFixture())

		var out, errOut bytes.Buffer
		require.True(t, uninstallRetiredGeminiHooks(newUninstallPrinter(&out, &errOut), dir))
		require.Contains(t, out.String(), "Removed retired Gemini CLI hooks")
		require.Empty(t, errOut.String())

		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		require.NotContains(t, string(data), "entire hooks gemini")
	})

	t.Run("nothing to remove prints nothing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		testutil.InitRepo(t, dir)

		var out, errOut bytes.Buffer
		require.True(t, uninstallRetiredGeminiHooks(newUninstallPrinter(&out, &errOut), dir))
		require.Empty(t, out.String())
		require.Empty(t, errOut.String())
	})

	t.Run("malformed file fails the step", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		testutil.InitRepo(t, dir)
		writeRetiredGeminiSettings(t, dir, `{"hooks":`)

		var out, errOut bytes.Buffer
		require.False(t, uninstallRetiredGeminiHooks(newUninstallPrinter(&out, &errOut), dir))
		require.Contains(t, out.String(), "Failed to remove retired Gemini CLI hooks")
		require.Contains(t, errOut.String(), "entire hooks gemini")
	})
}

func TestRunRemoveAgent_RetiredGemini(t *testing.T) {
	// Cannot use t.Parallel(): setupTestRepo uses t.Chdir.
	t.Run("removes entries", func(t *testing.T) {
		dir := setupTestRepo(t)
		settingsPath := writeRetiredGeminiSettings(t, dir, retiredGeminiSettingsFixture())

		var out bytes.Buffer
		require.NoError(t, runRemoveAgent(context.Background(), &out, string(retiredGeminiAgentName)))
		require.Contains(t, out.String(), "Removed Gemini CLI hooks. Gemini CLI is no longer supported.")

		data, err := os.ReadFile(settingsPath)
		require.NoError(t, err)
		require.NotContains(t, string(data), "entire hooks gemini")
	})

	t.Run("not installed", func(t *testing.T) {
		setupTestRepo(t)

		var out bytes.Buffer
		require.NoError(t, runRemoveAgent(context.Background(), &out, string(retiredGeminiAgentName)))
		require.Contains(t, out.String(), "Gemini CLI hooks are not installed.")
	})
}

func TestPrintWrongAgentError_RetiredGemini(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	printWrongAgentError(&buf, string(retiredGeminiAgentName))
	output := buf.String()

	require.Contains(t, output, "Gemini CLI is no longer supported. Available agents:")
	require.NotContains(t, output, "Unknown agent")
	require.NotContains(t, output, "  gemini\n", "the retired agent must not be listed as available")
}

// Retired Gemini hooks can be the only thing Entire left in a repository, e.g.
// after an earlier uninstall removed .entire/ before this cleanup existed. The
// "nothing installed" preflight must see them, or the uninstall reports success
// and leaves them in place.
func TestRunUninstall_RetiredGeminiHooksAloneAreRemoved(t *testing.T) {
	// Cannot use t.Parallel: setupTestRepo changes cwd.
	dir := setupTestRepo(t)
	settingsPath := writeRetiredGeminiSettings(t, dir, retiredGeminiSettingsFixture())

	var stdout, stderr bytes.Buffer
	require.NoError(t, runUninstall(context.Background(), &stdout, &stderr, true), "stderr:\n%s", stderr.String())

	out := stdout.String()
	require.NotContains(t, out, "not installed in this repository")
	require.Contains(t, out, "Removed retired Gemini CLI hooks")

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "entire hooks gemini")
	require.Contains(t, string(data), "./my-start.sh", "the user's own hooks must survive")
}

// The confirmation summary lists what the removal will touch, so it must name
// the retired hooks too.
func TestRunUninstall_SummaryNamesRetiredGeminiHooks(t *testing.T) {
	// Cannot use t.Parallel: setupTestRepo changes cwd.
	dir := setupTestRepo(t)
	settingsPath := writeRetiredGeminiSettings(t, dir, retiredGeminiSettingsFixture())

	// force=false prints the summary, then stops at the no-terminal guard.
	var stdout, stderr bytes.Buffer
	require.Error(t, runUninstall(context.Background(), &stdout, &stderr, false))
	require.Contains(t, stderr.String(), "Re-run with --force")

	out := stdout.String()
	require.Contains(t, out, "retired hooks")
	require.Contains(t, out, "Gemini CLI")

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "entire hooks gemini", "a declined confirmation must leave the file alone")
}

// A .gemini/settings.json that cannot be parsed is not evidence of absence: the
// uninstall must not claim Entire is not installed, and must fail with the
// reason rather than rewriting a file it could not read.
func TestRunUninstall_UncheckableRetiredGeminiHooksFailTheCommand(t *testing.T) {
	// Cannot use t.Parallel: setupTestRepo changes cwd.
	dir := setupTestRepo(t)
	settingsPath := writeRetiredGeminiSettings(t, dir, `{"hooks":`)

	var stdout, stderr bytes.Buffer
	require.Error(t, runUninstall(context.Background(), &stdout, &stderr, true))
	require.NotContains(t, stdout.String(), "not installed in this repository")
	require.Contains(t, stdout.String(), "Failed to remove retired Gemini CLI hooks")
	require.Contains(t, stderr.String(), "settings.json")

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	require.Equal(t, `{"hooks":`, string(data))
}
