//nolint:govet // Test file with struct field assignments for completeness
package agent

import (
	"testing"
	"time"
)

func TestAgentSessionStructure(t *testing.T) {
	t.Parallel()

	session := AgentSession{
		SessionID:     "test-session-123",
		AgentName:     "claude-code",
		RepoPath:      "/path/to/repo",
		SessionRef:    "/path/to/session/file",
		StartTime:     time.Now(),
		Entries:       []SessionEntry{},
		ModifiedFiles: []string{"file1.go"},
		NewFiles:      []string{"file2.go"},
		DeletedFiles:  []string{"file3.go"},
	}

	if session.SessionID != "test-session-123" {
		t.Errorf("expected SessionID %q, got %q", "test-session-123", session.SessionID)
	}
	if session.AgentName != "claude-code" {
		t.Errorf("expected AgentName %q, got %q", "claude-code", session.AgentName)
	}
}

func TestSessionEntryStructure(t *testing.T) {
	t.Parallel()

	entry := SessionEntry{
		UUID:          "entry-uuid-123",
		Type:          EntryTool,
		Timestamp:     time.Now(),
		Content:       "Tool output",
		ToolName:      "Write",
		ToolInput:     map[string]interface{}{"file_path": "test.go"},
		ToolOutput:    "file written",
		FilesAffected: []string{"test.go"},
	}

	if entry.UUID != "entry-uuid-123" {
		t.Errorf("expected UUID %q, got %q", "entry-uuid-123", entry.UUID)
	}
	if entry.Type != EntryTool {
		t.Errorf("expected Type %q, got %q", EntryTool, entry.Type)
	}
}
