package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

func TestParseHookEvent_SessionStart(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Plugin now only sends session_id, not transcript_path
	input := `{"session_id": "sess-abc123"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionStart {
		t.Errorf("expected SessionStart, got %v", event.Type)
	}
	if event.SessionID != "sess-abc123" {
		t.Errorf("expected session_id 'sess-abc123', got %q", event.SessionID)
	}
	// SessionRef is now empty for session-start (no transcript path from plugin)
	if event.SessionRef != "" {
		t.Errorf("expected empty session ref, got %q", event.SessionRef)
	}
}

func TestParseHookEvent_TurnStart(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Plugin now only sends session_id and prompt, not transcript_path
	input := `{"session_id": "sess-1", "prompt": "Fix the bug in login.ts"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnStart {
		t.Errorf("expected TurnStart, got %v", event.Type)
	}
	if event.Prompt != "Fix the bug in login.ts" {
		t.Errorf("expected prompt 'Fix the bug in login.ts', got %q", event.Prompt)
	}
	if event.SessionID != "sess-1" {
		t.Errorf("expected session_id 'sess-1', got %q", event.SessionID)
	}
	// SessionRef is computed from session_id, should end with .json
	if !strings.HasSuffix(event.SessionRef, "sess-1.json") {
		t.Errorf("expected session ref to end with 'sess-1.json', got %q", event.SessionRef)
	}
}

func TestParseHookEvent_TurnStart_IncludesModel(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-model", "prompt": "hello", "model": "claude-sonnet-4-20250514"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "claude-sonnet-4-20250514" {
		t.Errorf("expected model 'claude-sonnet-4-20250514', got %q", event.Model)
	}
}

func TestParseHookEvent_TurnStart_EmptyModel(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Model field absent — should parse as empty string
	input := `{"session_id": "sess-no-model", "prompt": "hello"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.Model != "" {
		t.Errorf("expected empty model, got %q", event.Model)
	}
}

func TestParseHookEvent_TurnEnd(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-2"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameTurnEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.TurnEnd {
		t.Errorf("expected TurnEnd, got %v", event.Type)
	}
	if event.SessionID != "sess-2" {
		t.Errorf("expected session_id 'sess-2', got %q", event.SessionID)
	}
	// SessionRef is computed from session_id, should end with .json (same pattern as TurnStart)
	if !strings.HasSuffix(event.SessionRef, "sess-2.json") {
		t.Errorf("expected session ref to end with 'sess-2.json', got %q", event.SessionRef)
	}
}

func TestParseHookEvent_Compaction(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "sess-3"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameCompaction, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.Compaction {
		t.Errorf("expected Compaction, got %v", event.Type)
	}
	if event.SessionID != "sess-3" {
		t.Errorf("expected session_id 'sess-3', got %q", event.SessionID)
	}
}

func TestParseHookEvent_SessionEnd(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	// Plugin now only sends session_id
	input := `{"session_id": "sess-4"}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameSessionEnd, strings.NewReader(input))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	require.NotNil(t, event, "expected event, got nil")
	if event.Type != agent.SessionEnd {
		t.Errorf("expected SessionEnd, got %v", event.Type)
	}
}

func TestParseHookEvent_UnknownHook(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook", strings.NewReader(`{}`))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event != nil {
		t.Errorf("expected nil event for unknown hook, got %+v", event)
	}
}

func TestParseHookEvent_EmptyInput(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))

	if err == nil {
		t.Fatal("expected error for empty input")
	}
	if !strings.Contains(err.Error(), "empty hook input") {
		t.Errorf("expected 'empty hook input' error, got: %v", err)
	}
}

func TestParseHookEvent_MalformedJSON(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader("not json"))

	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestFormatResumeCommand(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	cmd := ag.FormatResumeCommand("sess-abc123")

	expected := "opencode -s sess-abc123"
	if cmd != expected {
		t.Errorf("expected %q, got %q", expected, cmd)
	}
}

func TestFormatResumeCommand_Empty(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	cmd := ag.FormatResumeCommand("")

	if cmd != "opencode" {
		t.Errorf("expected %q, got %q", "opencode", cmd)
	}
}

func TestHookNames(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	names := ag.HookNames()

	expected := []string{
		HookNameSessionStart,
		HookNameSessionEnd,
		HookNameTurnStart,
		HookNameTurnEnd,
		HookNameCompaction,
	}

	if len(names) != len(expected) {
		t.Fatalf("expected %d hook names, got %d", len(expected), len(names))
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	for _, e := range expected {
		if !nameSet[e] {
			t.Errorf("missing expected hook name: %s", e)
		}
	}
}

func TestPrepareTranscript_AlwaysRefreshesTranscript(t *testing.T) {
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "sess-123.json")

	// Create an existing file with stale data
	if err := os.WriteFile(transcriptPath, []byte(`{"info":{},"messages":[]}`), 0o600); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	wantErr := errors.New("refresh attempted")
	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(context.Context, string, string) error { return wantErr }
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })

	err := (&OpenCodeAgent{}).PrepareTranscript(context.Background(), transcriptPath)
	if !errors.Is(err, wantErr) {
		t.Fatalf("PrepareTranscript error = %v, want %v", err, wantErr)
	}
}

func TestPrepareTranscript_ErrorOnInvalidPath(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}

	// Path without .json extension
	err := ag.PrepareTranscript(context.Background(), "/tmp/not-a-json-file")
	if err == nil {
		t.Fatal("expected error for path without .json extension")
	}
	if !strings.Contains(err.Error(), "invalid OpenCode transcript path") {
		t.Errorf("expected 'invalid OpenCode transcript path' error, got: %v", err)
	}
}

func TestPrepareTranscript_ErrorOnBrokenSymlink(t *testing.T) {
	t.Parallel()

	// Broken symlinks cause os.Stat to return a non-IsNotExist error.
	// PrepareTranscript should surface this as a stat error.
	tmpDir := t.TempDir()
	transcriptPath := filepath.Join(tmpDir, "broken-link.json")

	if err := os.Symlink("/nonexistent/target", transcriptPath); err != nil {
		t.Skipf("cannot create symlink (permission denied?): %v", err)
	}

	ag := &OpenCodeAgent{}
	err := ag.PrepareTranscript(context.Background(), transcriptPath)

	if err == nil {
		t.Fatal("expected error for broken symlink, got nil")
	}
}

func TestPrepareTranscript_ErrorOnEmptySessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}

	// Path with empty session ID (.json with no basename)
	err := ag.PrepareTranscript(context.Background(), "/tmp/.json")
	if err == nil {
		t.Fatal("expected error for empty session ID")
	}
	if !strings.Contains(err.Error(), "empty session ID") {
		t.Errorf("expected 'empty session ID' error, got: %v", err)
	}
}

func TestParseHookEvent_TurnStart_InvalidSessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "../escape", "prompt": "hello"}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameTurnStart, strings.NewReader(input))

	if err == nil {
		t.Fatal("expected error for path-traversal session ID")
	}
	if !strings.Contains(err.Error(), "contains path separators") {
		t.Errorf("expected 'contains path separators' error, got: %v", err)
	}
}

func TestParseHookEvent_TurnEnd_InvalidSessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	input := `{"session_id": "../escape"}`

	_, err := ag.ParseHookEvent(context.Background(), HookNameTurnEnd, strings.NewReader(input))

	if err == nil {
		t.Fatal("expected error for path-traversal session ID")
	}
	if !strings.Contains(err.Error(), "contains path separators") {
		t.Errorf("expected 'contains path separators' error, got: %v", err)
	}
}

func TestFetchAndCacheExport_WritesAndValidatesExportFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(_ context.Context, sessionID, outputPath string) error {
		if sessionID != "ses_abc123" {
			return fmt.Errorf("unexpected session id: %s", sessionID)
		}
		return os.WriteFile(outputPath, []byte(`{"info":{"id":"ses_abc123"},"messages":[]}`), 0o600)
	}
	t.Cleanup(func() {
		runOpenCodeExportToFileFn = original
	})

	ag := &OpenCodeAgent{}
	transcriptPath, err := ag.fetchAndCacheExport(context.Background(), "ses_abc123")
	require.NoError(t, err)

	content, err := os.ReadFile(transcriptPath)
	require.NoError(t, err)
	require.True(t, json.Valid(content), "expected cached transcript to be valid JSON")
	require.Contains(t, string(content), "\"ses_abc123\"")
}

func TestFetchTranscript_ValidatesSessionID(t *testing.T) {
	t.Parallel()

	ag := &OpenCodeAgent{}
	if _, err := ag.FetchTranscript(context.Background(), "bad/session-id"); err == nil {
		t.Fatal("expected error for session ID with path separator, got nil")
	}
}

func TestFetchTranscript_AttemptsExport(t *testing.T) {
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	wantErr := errors.New("export attempted")
	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(context.Context, string, string) error { return wantErr }
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })

	ag := &OpenCodeAgent{}
	_, err := ag.FetchTranscript(context.Background(), "test-fetch-transcript-no-such-session")
	if !errors.Is(err, wantErr) {
		t.Fatalf("FetchTranscript error = %v, want %v", err, wantErr)
	}
}

func TestFetchTranscript_ReportsInvalidJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	original := runOpenCodeExportToFileFn
	runOpenCodeExportToFileFn = func(_ context.Context, _, outputPath string) error {
		return os.WriteFile(outputPath, []byte("not json"), 0o600)
	}
	t.Cleanup(func() { runOpenCodeExportToFileFn = original })

	const sessionID = "test-invalid-json"
	_, err := (&OpenCodeAgent{}).FetchTranscript(context.Background(), sessionID)
	want := `OpenCode returned invalid transcript data for session "test-invalid-json". Try updating OpenCode and running the command again.`
	if err == nil || err.Error() != want {
		t.Fatalf("FetchTranscript error = %v, want %q", err, want)
	}
}
