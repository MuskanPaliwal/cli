package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/copilotcli"
	"github.com/entireio/cli/cmd/entire/cli/agent/cursor"
	"github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
	"github.com/entireio/cli/cmd/entire/cli/agent/geminicli"
	"github.com/entireio/cli/cmd/entire/cli/agent/opencode"
	"github.com/entireio/cli/cmd/entire/cli/agent/pi"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestScaffoldSearchSkill_CreatesManagedFiles(t *testing.T) {
	testCases := []struct {
		name    string
		agent   agent.Agent
		relPath string
	}{
		{"claude", claudecode.NewClaudeCodeAgent(), filepath.Join(".claude", "skills", "entire-search", "SKILL.md")},
		{"codex", codex.NewCodexAgent(), filepath.Join(".agents", "skills", "entire-search", "SKILL.md")},
		{"gemini", geminicli.NewGeminiCLIAgent(), filepath.Join(".gemini", "skills", "entire-search", "SKILL.md")},
		{"opencode", opencode.NewOpenCodeAgent(), filepath.Join(".opencode", "skills", "entire-search", "SKILL.md")},
		{"copilot", copilotcli.NewCopilotCLIAgent(), filepath.Join(".github", "skills", "entire-search", "SKILL.md")},
		{"cursor", cursor.NewCursorAgent(), filepath.Join(".cursor", "skills", "entire-search", "SKILL.md")},
		{"factory", factoryaidroid.NewFactoryAIDroidAgent(), filepath.Join(".factory", "skills", "entire-search", "SKILL.md")},
		{"pi", pi.NewPiAgent(), filepath.Join(".pi", "skills", "entire-search", "SKILL.md")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := setupTestDir(t)

			result, err := scaffoldSearchSkill(context.Background(), tc.agent)
			if err != nil {
				t.Fatalf("scaffoldSearchSkill() error = %v", err)
			}
			if result.Status != managedScaffoldCreated {
				t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldCreated)
			}
			if result.RelPath != tc.relPath {
				t.Fatalf("scaffoldSearchSkill() relPath = %q, want %q", result.RelPath, tc.relPath)
			}

			data, err := os.ReadFile(filepath.Join(tmpDir, tc.relPath))
			if err != nil {
				t.Fatalf("failed to read scaffolded file: %v", err)
			}
			content := string(data)
			if !strings.Contains(content, entireManagedSearchSkillMarker) {
				t.Fatal("scaffolded file should contain Entire-managed marker")
			}
			if !strings.Contains(content, "name: entire-search\n") {
				t.Fatal("scaffolded skill should declare name: entire-search")
			}
			assertStrictJSONSearchInstructions(t, content)
		})
	}
}

func TestScaffoldSearchSkill_IdempotentManagedFile(t *testing.T) {
	setupTestDir(t)

	ag := claudecode.NewClaudeCodeAgent()
	if _, err := scaffoldSearchSkill(context.Background(), ag); err != nil {
		t.Fatalf("first scaffoldSearchSkill() error = %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("second scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldUnchanged {
		t.Fatalf("second scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldUnchanged)
	}
}

func TestScaffoldSearchSkill_UpdatesManagedFile(t *testing.T) {
	tmpDir := setupTestDir(t)

	ag := claudecode.NewClaudeCodeAgent()
	relPath, _, ok := searchSkillTemplate(ag.Name())
	if !ok {
		t.Fatal("searchSkillTemplate() unexpectedly unsupported for claude")
	}

	targetPath := filepath.Join(tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	oldContent := "<!-- " + legacyEntireManagedSearchSubagentMarker + " -->\noutdated\n"
	if err := os.WriteFile(targetPath, []byte(oldContent), 0o644); err != nil {
		t.Fatalf("failed to write old managed content: %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldUpdated {
		t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldUpdated)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read updated content: %v", err)
	}
	if !strings.Contains(string(data), "name: entire-search\n") {
		t.Fatal("updated managed file should contain the current template")
	}
	assertStrictJSONSearchInstructions(t, string(data))
}

func TestScaffoldSearchSkill_PreservesUserOwnedFile(t *testing.T) {
	tmpDir := setupTestDir(t)

	ag := claudecode.NewClaudeCodeAgent()
	relPath, _, ok := searchSkillTemplate(ag.Name())
	if !ok {
		t.Fatal("searchSkillTemplate() unexpectedly unsupported for claude")
	}

	targetPath := filepath.Join(tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		t.Fatalf("failed to create target dir: %v", err)
	}
	userContent := "user-owned search skill\n"
	if err := os.WriteFile(targetPath, []byte(userContent), 0o644); err != nil {
		t.Fatalf("failed to write user-owned file: %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldSkippedConflict {
		t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldSkippedConflict)
	}

	data, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatalf("failed to read preserved file: %v", err)
	}
	if string(data) != userContent {
		t.Fatal("user-owned file should not be overwritten")
	}
}

func TestScaffoldSearchSkill_RemovesManagedLegacySubagent(t *testing.T) {
	testCases := []struct {
		name          string
		agent         agent.Agent
		legacyRelPath string
	}{
		{"claude", claudecode.NewClaudeCodeAgent(), filepath.Join(".claude", "agents", "entire-search.md")},
		{"codex", codex.NewCodexAgent(), filepath.Join(".codex", "agents", "entire-search.toml")},
		{"gemini", geminicli.NewGeminiCLIAgent(), filepath.Join(".gemini", "agents", "entire-search.md")},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := setupTestDir(t)

			legacyPath := filepath.Join(tmpDir, tc.legacyRelPath)
			if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
				t.Fatalf("failed to create legacy dir: %v", err)
			}
			legacyContent := "<!-- " + legacyEntireManagedSearchSubagentMarker + " -->\nold subagent\n"
			if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
				t.Fatalf("failed to write legacy subagent: %v", err)
			}

			result, err := scaffoldSearchSkill(context.Background(), tc.agent)
			if err != nil {
				t.Fatalf("scaffoldSearchSkill() error = %v", err)
			}
			if result.Status != managedScaffoldCreated {
				t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldCreated)
			}
			if result.RemovedLegacyRelPath != tc.legacyRelPath {
				t.Fatalf("RemovedLegacyRelPath = %q, want %q", result.RemovedLegacyRelPath, tc.legacyRelPath)
			}
			if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
				t.Fatalf("managed legacy subagent should be removed, stat err = %v", err)
			}
		})
	}
}

func TestScaffoldSearchSkill_RemovesManagedLegacySubagentOnSkillConflict(t *testing.T) {
	tmpDir := setupTestDir(t)

	ag := claudecode.NewClaudeCodeAgent()
	relPath, _, ok := searchSkillTemplate(ag.Name())
	if !ok {
		t.Fatal("searchSkillTemplate() unexpectedly unsupported for claude")
	}
	skillPath := filepath.Join(tmpDir, relPath)
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatalf("failed to create skill dir: %v", err)
	}
	userContent := "user-owned search skill\n"
	if err := os.WriteFile(skillPath, []byte(userContent), 0o644); err != nil {
		t.Fatalf("failed to write user-owned skill: %v", err)
	}

	legacyRelPath := filepath.Join(".claude", "agents", "entire-search.md")
	legacyPath := filepath.Join(tmpDir, legacyRelPath)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("failed to create legacy dir: %v", err)
	}
	legacyContent := "<!-- " + legacyEntireManagedSearchSubagentMarker + " -->\nold subagent\n"
	if err := os.WriteFile(legacyPath, []byte(legacyContent), 0o644); err != nil {
		t.Fatalf("failed to write legacy subagent: %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldSkippedConflict {
		t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldSkippedConflict)
	}
	if result.RemovedLegacyRelPath != legacyRelPath {
		t.Fatalf("RemovedLegacyRelPath = %q, want %q", result.RemovedLegacyRelPath, legacyRelPath)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("managed legacy subagent should be removed despite skill conflict, stat err = %v", err)
	}
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("failed to read preserved skill file: %v", err)
	}
	if string(data) != userContent {
		t.Fatal("user-owned skill file should not be overwritten")
	}
}

func TestScaffoldSearchSkill_LegacyCleanupFailureDoesNotFailInstall(t *testing.T) {
	tmpDir := setupTestDir(t)

	// A directory at the legacy path makes the cleanup's os.ReadFile fail
	// (EISDIR) while the skill write itself is unaffected.
	legacyRelPath := filepath.Join(".claude", "agents", "entire-search.md")
	if err := os.MkdirAll(filepath.Join(tmpDir, legacyRelPath), 0o755); err != nil {
		t.Fatalf("failed to create legacy dir: %v", err)
	}

	ag := claudecode.NewClaudeCodeAgent()
	result, err := scaffoldSearchSkill(context.Background(), ag)
	if err != nil {
		t.Fatalf("scaffoldSearchSkill() error = %v, want nil: cleanup is best-effort", err)
	}
	if result.Status != managedScaffoldCreated {
		t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldCreated)
	}
	if result.RemovedLegacyRelPath != "" {
		t.Fatalf("RemovedLegacyRelPath = %q, want empty when cleanup failed", result.RemovedLegacyRelPath)
	}
	if result.LegacyCleanupWarning == "" {
		t.Fatal("LegacyCleanupWarning should report the failed cleanup")
	}
	if !strings.Contains(result.LegacyCleanupWarning, legacyRelPath) {
		t.Fatalf("warning %q should name the legacy path %q", result.LegacyCleanupWarning, legacyRelPath)
	}

	skillPath := filepath.Join(tmpDir, ".claude", "skills", "entire-search", "SKILL.md")
	if _, err := os.Stat(skillPath); err != nil {
		t.Fatalf("skill should be installed despite cleanup failure: %v", err)
	}

	var out bytes.Buffer
	reportSearchSkillScaffold(&out, ag, result)
	if !strings.Contains(out.String(), "Warning:") {
		t.Fatalf("report should surface the cleanup warning, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "Installed Claude Code search skill") {
		t.Fatalf("report should still announce the successful install, got: %s", out.String())
	}
}

func TestScaffoldSearchSkill_PreservesUserOwnedLegacySubagent(t *testing.T) {
	tmpDir := setupTestDir(t)

	legacyRelPath := filepath.Join(".claude", "agents", "entire-search.md")
	legacyPath := filepath.Join(tmpDir, legacyRelPath)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatalf("failed to create legacy dir: %v", err)
	}
	userContent := "user-owned search agent\n"
	if err := os.WriteFile(legacyPath, []byte(userContent), 0o644); err != nil {
		t.Fatalf("failed to write user-owned subagent: %v", err)
	}

	result, err := scaffoldSearchSkill(context.Background(), claudecode.NewClaudeCodeAgent())
	if err != nil {
		t.Fatalf("scaffoldSearchSkill() error = %v", err)
	}
	if result.Status != managedScaffoldCreated {
		t.Fatalf("scaffoldSearchSkill() status = %q, want %q", result.Status, managedScaffoldCreated)
	}
	if result.RemovedLegacyRelPath != "" {
		t.Fatalf("RemovedLegacyRelPath = %q, want empty for user-owned file", result.RemovedLegacyRelPath)
	}

	data, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("failed to read preserved file: %v", err)
	}
	if string(data) != userContent {
		t.Fatal("user-owned legacy subagent should not be removed")
	}
}

func TestSetupAgentHooksNonInteractive_SearchSkillOptInOnly(t *testing.T) {
	tmpDir := setupTestDir(t)
	testutil.InitRepo(t, tmpDir)
	ag := claudecode.NewClaudeCodeAgent()

	var out bytes.Buffer
	if err := setupAgentHooksNonInteractive(context.Background(), &out, ag, EnableOptions{}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive(default) error = %v", err)
	}
	searchPath := filepath.Join(tmpDir, ".claude", "skills", "entire-search", "SKILL.md")
	if _, err := os.Stat(searchPath); !os.IsNotExist(err) {
		t.Fatalf("default setup should not install search skill, stat err = %v", err)
	}

	out.Reset()
	if err := setupAgentHooksNonInteractive(context.Background(), &out, ag, EnableOptions{SearchSkill: true}); err != nil {
		t.Fatalf("setupAgentHooksNonInteractive(search skill) error = %v", err)
	}
	if _, err := os.Stat(searchPath); err != nil {
		t.Fatalf("opt-in setup should install search skill: %v", err)
	}
	if !strings.Contains(out.String(), "Installed Claude Code search skill") {
		t.Fatalf("output should mention installed search skill, got: %s", out.String())
	}
}

func TestManageAgentsNonInteractive_SearchSkillWithoutAgentsShowsInstallGuidance(t *testing.T) {
	setupTestRepo(t)
	writeSettings(t, testSettingsEnabled)

	var out bytes.Buffer
	err := runManageAgents(context.Background(), &out, EnableOptions{SearchSkill: true}, nil)
	if err == nil {
		t.Fatal("expected error when --search-skill cannot choose an agent non-interactively")
	}
	var silentErr *SilentError
	if !errors.As(err, &silentErr) {
		t.Fatalf("error = %T %v, want SilentError", err, err)
	}

	output := out.String()
	for _, want := range []string{
		"Cannot install the search skill in non-interactive mode because no agents are enabled.",
		"entire enable --agent <name> --search-skill",
		"entire agent add <name> --search-skill",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q, got: %s", want, output)
		}
	}
}

func assertStrictJSONSearchInstructions(t *testing.T, content string) {
	t.Helper()

	if !strings.Contains(content, "entire search --json") {
		t.Fatal("scaffolded file should instruct use of `entire search --json`")
	}
	if !strings.Contains(content, "Never run `entire search` without `--json`; it opens an interactive TUI.") {
		t.Fatal("scaffolded file should explicitly forbid plain `entire search`")
	}
	if strings.Contains(content, "Your only history-search mechanism is the `entire search` command.") {
		t.Fatal("scaffolded file should not present plain `entire search` as the required command")
	}
	if !strings.Contains(content, "entire search --json --compact") {
		t.Fatal("scaffolded file should recommend `--json --compact` for scanning results")
	}
	if !strings.Contains(content, "entire checkpoint explain <id>") {
		t.Fatal("scaffolded file should point drill-down at `entire checkpoint explain <id>`")
	}
	if !strings.Contains(content, "entire checkpoint explain --session <id>") {
		t.Fatal("scaffolded file should bridge session hits via `explain --session`")
	}
	if !strings.Contains(content, "session hit on the current branch") {
		t.Fatal("scaffolded file should scope the session bridge to the current branch")
	}
	if !strings.Contains(content, "session hits are projections of the same checkpoints") {
		t.Fatal("scaffolded file should frame session hits as projections of checkpoints")
	}
	if !strings.Contains(content, "add `--full` to pull the checkpoint's entire session transcript") {
		t.Fatal("scaffolded file should escalate to `explain --full` for the session transcript")
	}
	if !strings.Contains(content, "summarize from the compact fields alone") {
		t.Fatal("scaffolded file should tell agents repo/pr and cross-repo hits aren't explainable")
	}
}

// TestSearchSkillTemplates_NameMatchesTelemetryProbe pins the scaffolded skill
// name to strategy.EntireSearchSubagentName, the value the commit-condensed
// telemetry probe matches legacy subagent dispatches against and the skill
// directory every agent's scaffold path is built from. Without this pin,
// renaming the skill in the template compiles and passes every template test
// while silently splitting the scaffolded artifact from the probe's identity.
func TestSearchSkillTemplates_NameMatchesTelemetryProbe(t *testing.T) {
	t.Parallel()

	nameDecl := "name: " + strategy.EntireSearchSubagentName + "\n"
	if !strings.Contains(searchSkillTemplateContent, nameDecl) {
		t.Errorf("template does not declare the skill name the telemetry probe matches: want %q", nameDecl)
	}

	for _, agentName := range []types.AgentName{
		agent.AgentNameClaudeCode,
		agent.AgentNameCodex,
		agent.AgentNameCopilotCLI,
		agent.AgentNameCursor,
		agent.AgentNameFactoryAIDroid,
		agent.AgentNameGemini,
		agent.AgentNameOpenCode,
		agent.AgentNamePi,
	} {
		relPath, _, ok := searchSkillTemplate(agentName)
		if !ok {
			t.Fatalf("searchSkillTemplate(%s) unexpectedly unsupported", agentName)
		}
		if base := filepath.Base(relPath); base != "SKILL.md" {
			t.Errorf("scaffold path for %s ends in %q, want SKILL.md", agentName, base)
		}
		if got := filepath.Base(filepath.Dir(relPath)); got != strategy.EntireSearchSubagentName {
			t.Errorf("scaffold skill dir for %s is %q, probe matches %q", agentName, got, strategy.EntireSearchSubagentName)
		}
	}
}
