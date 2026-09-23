package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// Gemini CLI support was removed, but repositories that enabled it still carry
// Entire hooks in .gemini/settings.json, and nothing re-runs setup when the CLI
// is upgraded. Two things keep those repositories working until the entries are
// gone: `entire hooks gemini <verb>` exits cleanly instead of failing every
// Gemini event (see newHooksCmd), and `entire doctor` and
// `entire disable --uninstall` remove the entries.

// retiredGeminiAgentName is the hook namespace Gemini CLI support used
// (`entire hooks gemini <verb>`).
const retiredGeminiAgentName types.AgentName = "gemini"

// retiredGeminiHookConfigRelPath is where Gemini CLI support installed hooks.
const retiredGeminiHookConfigRelPath = ".gemini/settings.json"

// removeRetiredGeminiHooks removes Entire-managed hook entries from the
// worktree's .gemini/settings.json and reports whether it changed the file.
// Other hooks, matchers, and settings are preserved field for field; a hook
// type left with no matchers is dropped, as Gemini CLI support's own uninstall
// did. A missing file is not an error.
func removeRetiredGeminiHooks(worktreeRoot string) (bool, error) {
	cfg, output, changed, err := planRetiredGeminiHookRemoval(worktreeRoot)
	if err != nil || !changed {
		return false, err
	}
	if err := cfg.Write(output, 0o600); err != nil {
		return false, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}
	return true, nil
}

// retiredGeminiHooksInstalled reports whether the worktree's
// .gemini/settings.json still holds Entire-managed hook entries, without
// writing anything. An error means the file could not be read or parsed, which
// is not the same answer as "none": callers deciding whether there is anything
// to clean up must not treat it as absence.
func retiredGeminiHooksInstalled(worktreeRoot string) (bool, error) {
	_, _, changed, err := planRetiredGeminiHookRemoval(worktreeRoot)
	return changed, err
}

// planRetiredGeminiHookRemoval reads .gemini/settings.json and returns the
// file handle and its content with Entire's entries stripped. changed is false
// when the file is missing or holds no Entire entries.
func planRetiredGeminiHookRemoval(worktreeRoot string) (*agent.HookConfigFile, []byte, bool, error) {
	cfg, err := agent.OpenHookConfig(worktreeRoot, retiredGeminiHookConfigRelPath)
	if err != nil {
		return nil, nil, false, err //nolint:wrapcheck // agent.HookConfigFile already names the file in its error
	}
	data, err := cfg.Read()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("read %s: %w", cfg.Path(), err)
	}
	output, changed, err := stripRetiredGeminiHooks(data)
	if err != nil {
		return nil, nil, false, fmt.Errorf("%s: %w", cfg.Path(), err)
	}
	return cfg, output, changed, nil
}

// stripRetiredGeminiHooks returns settings with every Entire-managed hook
// command removed. Values it does not recognize (a non-array hook type, a
// matcher without a hooks list) are left as they are.
func stripRetiredGeminiHooks(data []byte) ([]byte, bool, error) {
	var rawSettings map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawSettings); err != nil {
		return nil, false, fmt.Errorf("parse settings: %w", err)
	}
	hooksRaw, ok := rawSettings["hooks"]
	if !ok {
		return nil, false, nil
	}
	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
		return nil, false, fmt.Errorf("parse hooks: %w", err)
	}

	changed := false
	for hookType, value := range rawHooks {
		var matchers []map[string]json.RawMessage
		if json.Unmarshal(value, &matchers) != nil {
			continue
		}
		kept, matchersChanged, err := stripManagedGeminiEntries(matchers)
		if err != nil {
			return nil, false, err
		}
		if !matchersChanged {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(rawHooks, hookType)
			continue
		}
		encoded, err := jsonutil.MarshalWithNoHTMLEscape(kept)
		if err != nil {
			return nil, false, fmt.Errorf("marshal %s hooks: %w", hookType, err)
		}
		rawHooks[hookType] = encoded
	}
	if !changed {
		return nil, false, nil
	}

	if len(rawHooks) == 0 {
		delete(rawSettings, "hooks")
	} else {
		encoded, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return nil, false, fmt.Errorf("marshal hooks: %w", err)
		}
		rawSettings["hooks"] = encoded
	}
	output, err := jsonutil.MarshalIndentWithNewline(rawSettings, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("marshal settings: %w", err)
	}
	return output, true, nil
}

// stripManagedGeminiEntries drops Entire-managed entries from each matcher's
// hooks list, and drops a matcher once its list is empty.
func stripManagedGeminiEntries(matchers []map[string]json.RawMessage) ([]map[string]json.RawMessage, bool, error) {
	kept := make([]map[string]json.RawMessage, 0, len(matchers))
	changed := false
	for _, matcher := range matchers {
		var entries []map[string]json.RawMessage
		if json.Unmarshal(matcher["hooks"], &entries) != nil {
			kept = append(kept, matcher)
			continue
		}
		remaining := make([]map[string]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			var command string
			if json.Unmarshal(entry["command"], &command) == nil && agent.IsManagedHookCommand(command) {
				changed = true
				continue
			}
			remaining = append(remaining, entry)
		}
		if len(remaining) == 0 && len(entries) > 0 {
			continue
		}
		if len(remaining) != len(entries) {
			encoded, err := jsonutil.MarshalWithNoHTMLEscape(remaining)
			if err != nil {
				return nil, false, fmt.Errorf("marshal hook entries: %w", err)
			}
			matcher["hooks"] = encoded
		}
		kept = append(kept, matcher)
	}
	return kept, changed, nil
}
