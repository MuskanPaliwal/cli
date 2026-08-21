package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

func appendPromptToFile(ctx context.Context, sessionID, prompt string) error {
	if prompt == "" {
		return nil
	}

	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		return fmt.Errorf("resolve session metadata directory: %w", err)
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		return fmt.Errorf("create session metadata directory: %w", err)
	}

	promptPath := filepath.Join(sessionDirAbs, paths.PromptFileName)
	existing, err := os.ReadFile(promptPath) //nolint:gosec // session metadata path
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read prompt.txt: %w", err)
	}

	content := prompt
	if len(existing) > 0 {
		content = string(existing) + checkpoint.PromptSeparator + prompt
	}
	if err := os.WriteFile(promptPath, []byte(content), 0o600); err != nil { //nolint:gosec // path from internal metadata, not user input
		return fmt.Errorf("write prompt.txt: %w", err)
	}
	return nil
}

func handleLifecyclePromptUpdate(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "prompt-update",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	if event.SessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if event.Prompt == "" {
		return nil
	}

	if err := strategy.MutateSessionState(ctx, event.SessionID, func(state *strategy.SessionState) error {
		// OpenCode may deliver prompt text after turn-end has already backfilled it.
		if state.Phase == session.PhaseIdle && state.LastPrompt == event.Prompt {
			if appendPromptSkillEventToState(ag, event, state) {
				return nil
			}
			return strategy.ErrMutationSkip
		}
		// Condensation clears prompt.txt while holding the same lock. Keep the
		// repair file and state writes ordered with that operation.
		if err := appendPromptToFile(ctx, event.SessionID, event.Prompt); err != nil {
			return err
		}
		state.LastPrompt = session.TruncatePromptForStorage(event.Prompt)
		appendPromptSkillEventToState(ag, event, state)
		return nil
	}); err != nil {
		return fmt.Errorf("update prompt storage: %w", err)
	}
	return nil
}
