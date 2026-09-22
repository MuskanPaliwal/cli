package antigravity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// Compile-time interface assertions.
var (
	_ agent.PromptExtractor      = (*AntigravityAgent)(nil)
	_ agent.TranscriptAnalyzer   = (*AntigravityAgent)(nil)
	_ agent.LateTranscriptWriter = (*AntigravityAgent)(nil)
)

// Antigravity 2.0 (agy) writes JSONL transcripts at
//   ~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/transcript_full.jsonl (the hook payload sends transcript_full; agy also writes a truncated transcript.jsonl alongside it)
// The on-disk schema is a sequence of "step" objects:
//   {
//     "step_index":  int,
//     "source":      "USER_EXPLICIT" | "SYSTEM" | "MODEL" | ...,
//     "type":        "USER_INPUT" | "CONVERSATION_HISTORY" | "PLANNER_RESPONSE" | ...,
//     "status":      "DONE" | ...,
//     "created_at":  RFC3339 timestamp,
//     "content":     string (optional — user request / model text),
//     "tool_calls":  [ { "name": string, "args": object } ] (optional)
//   }
// Prompt extraction and field-aware modified-file/position analysis
// (TranscriptAnalyzer) are implemented below. ReadTranscript/Chunk/Reassemble
// remain JSONL passthrough, and token counting is handled out-of-band
// elsewhere. See testdata/transcript_sample.jsonl for a captured fixture.

// agyStep is one line of agy's step-based JSONL transcript.
type agyStep struct {
	StepIndex int               `json:"step_index"`
	Source    string            `json:"source"`
	Type      string            `json:"type"`
	Content   string            `json:"content"`
	ToolCalls []agyStepToolCall `json:"tool_calls"`
	// TruncatedFields is set by agy when it trimmed parts of the step while
	// persisting it (observed on PLANNER_RESPONSE steps: ~0.8% of
	// replace_file_content calls in a daily-driver corpus lose TargetFile while
	// every other arg survives). Kept raw: only its presence matters here.
	TruncatedFields json.RawMessage `json:"truncated_fields"`
}

// truncated reports whether agy flagged the step as having truncated fields.
// Absent, null and empty-array all mean "intact".
func (s *agyStep) truncated() bool {
	t := bytes.TrimSpace(s.TruncatedFields)
	return len(t) > 0 && string(t) != "null" && string(t) != "[]" && string(t) != "{}"
}

type agyStepToolCall struct {
	Name string                     `json:"name"`
	Args map[string]json.RawMessage `json:"args"`
}

var userRequestRe = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)

// extractUserRequest returns the inner text of the first <USER_REQUEST> block,
// or the whole trimmed content if no wrapper is present.
func extractUserRequest(content string) string {
	if m := userRequestRe.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1])
	}
	// No wrapper: assume the content is itself the prompt. A hypothetical
	// metadata-only USER_INPUT step would surface verbatim — acceptable for v1.
	return strings.TrimSpace(content)
}

// forEachNonBlankLine iterates data's non-blank JSONL lines, counting them
// with the codex splitJSONL convention (blank lines skipped BEFORE counting),
// and calls fn for each line past fromOffset. Returns the total non-blank
// line count.
//
// This is the single owner of agy's transcript-offset metric: the position
// one method stores (GetTranscriptPosition → CheckpointTranscriptStart) is
// consumed as fromOffset by the others (ExtractPrompts,
// ExtractModifiedFilesFromOffset), so the counting MUST stay byte-identical
// across all of them — hence one iterator instead of three hand-synced loops.
func forEachNonBlankLine(data []byte, fromOffset int, fn func(raw []byte)) int {
	lineNum := 0
	for _, raw := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		lineNum++
		if fn != nil && lineNum > fromOffset {
			fn(raw)
		}
	}
	return lineNum
}

// ExtractPrompts implements agent.PromptExtractor. agy's PreInvocation hook
// carries no prompt, so the user prompt is recovered from the transcript's
// USER_INPUT steps. fromOffset is a count of non-blank lines already consumed.
func (a *AntigravityAgent) ExtractPrompts(sessionRef string, fromOffset int) ([]string, error) {
	// Every analyzer reads through ReadTranscript: one contained read when the
	// path is inside agy's brain directory, and the package's single ratcheted
	// unconfined read otherwise (see agent/transcript_read_guard_test.go).
	data, err := a.ReadTranscript(sessionRef)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("antigravity: read transcript for prompts: %w", err)
	}
	var prompts []string
	forEachNonBlankLine(data, fromOffset, func(raw []byte) {
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			return
		}
		if step.Type != "USER_INPUT" {
			return
		}
		if text := extractUserRequest(step.Content); text != "" {
			prompts = append(prompts, text)
		}
	})
	return prompts, nil
}

// GetTranscriptPosition implements agent.TranscriptAnalyzer. It returns the
// number of non-blank JSONL lines in the transcript, which the framework uses
// as a stable offset to bound subsequent extraction to a single checkpoint
// range. A missing file yields (0, nil) so a not-yet-flushed transcript (agy
// writes asynchronously) doesn't fail the hook.
func (a *AntigravityAgent) GetTranscriptPosition(path string) (int, error) {
	if path == "" {
		return 0, nil
	}
	data, err := a.ReadTranscript(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("antigravity: transcript position: %w", err)
	}
	return forEachNonBlankLine(data, 0, nil), nil
}

// CountTranscriptPosition implements agent.LateTranscriptWriter: agy writes
// its transcript only after the Stop hook, and its offset metric counts
// non-blank lines. Delegating to forEachNonBlankLine keeps this byte-identical
// with GetTranscriptPosition/ExtractPrompts (see the iterator's doc comment).
func (a *AntigravityAgent) CountTranscriptPosition(content []byte) int {
	return forEachNonBlankLine(content, 0, nil)
}

// ExtractModifiedFilesFromOffset implements agent.TranscriptAnalyzer. It scans
// agy step lines after startOffset for mutating tool calls and returns the
// target file paths they touch, deduplicated, alongside the new line position.
//
// Path convention: returned paths are ABSOLUTE and symlink-resolved — the same
// shape lifecycle.go's parsePreToolUse records into FilesTouched. The framework
// relativizes downstream via FilterAndNormalizePaths -> paths.ToRelativePath
// against the worktree root, so we must NOT pre-relativize here. We mirror
// parsePreToolUse exactly: decode the double-encoded TargetFile arg, then
// resolveAgySymlinks so the path matches what attribution diffs against (e.g.
// macOS /tmp -> /private/tmp). Both helpers live in lifecycle.go (same package)
// and are reused, not duplicated.
//
// The blank-skip -> lineNum++ -> (lineNum <= startOffset) ordering matches
// ExtractPrompts so positions stay consistent across analyzer methods.
func (a *AntigravityAgent) ExtractModifiedFilesFromOffset(path string, startOffset int) (files []string, currentPosition int, err error) {
	if path == "" {
		return nil, 0, nil
	}
	data, readErr := a.ReadTranscript(path)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("antigravity: extract modified files: %w", readErr)
	}
	seen := map[string]bool{}
	dropped := 0
	lineNum := forEachNonBlankLine(data, startOffset, func(raw []byte) {
		var step agyStep
		if json.Unmarshal(raw, &step) != nil {
			return
		}
		for _, tc := range step.ToolCalls {
			switch tc.Name {
			case "write_to_file", "replace_file_content", "multi_replace_file_content":
				target := resolveAgySymlinks(decodeAgyString(tc.Args["TargetFile"]))
				if target == "" {
					// A mutating call with no decodable TargetFile is a file we
					// know was modified but cannot name. When agy flagged the
					// step as truncated that is the cause (TargetFile was
					// trimmed away); say so instead of silently skipping, because
					// this analyzer feeds the fallbacks (late-flush, first-turn
					// mid-turn commit) where git status may not cover the file.
					if step.truncated() {
						dropped++
						logging.Warn(logging.WithComponent(context.Background(), "antigravity"),
							"transcript step truncated by agy; modified file cannot be named and is missing from the file list",
							slog.String("transcript", path),
							slog.Int("step_index", step.StepIndex),
							slog.String("tool", tc.Name))
					}
					continue
				}
				if !seen[target] {
					seen[target] = true
					files = append(files, target)
				}
			}
		}
	})
	if dropped > 0 {
		logging.Warn(logging.WithComponent(context.Background(), "antigravity"),
			"antigravity transcript-derived file list is incomplete",
			slog.String("transcript", path),
			slog.Int("dropped_truncated_calls", dropped))
	}
	return files, lineNum, nil
}

// ReadTranscript reads a transcript agy's hook payload named. When the path is
// inside agy's brain directory — where every real payload points — the read
// goes through the agent's SessionStore: a name inside a trusted root, no
// symlink followed at any component. A path outside it is the read-side gap
// docs/development/filesystem-safety.md describes (closing it needs RepoPath on
// HookInput), and it stays the one unconfined read the ratchet in
// agent/transcript_read_guard_test.go allows this file; it is never anchored on
// the path's own parent, which would contain nothing while looking like it did.
func (a *AntigravityAgent) ReadTranscript(sessionRef string) ([]byte, error) {
	if store, name, err := a.transcriptStore(sessionRef); err == nil {
		data, readErr := store.ReadFile(name)
		if readErr != nil {
			return nil, fmt.Errorf("antigravity: read transcript: %w", readErr)
		}
		return data, nil
	}
	data, err := os.ReadFile(sessionRef) //nolint:gosec // the ratcheted unconfined transcript read; see doc comment
	if err != nil {
		return nil, fmt.Errorf("antigravity: read transcript: %w", err)
	}
	return data, nil
}

func (a *AntigravityAgent) ChunkTranscript(_ context.Context, content []byte, maxSize int) ([][]byte, error) {
	chunks, err := agent.ChunkJSONL(content, maxSize)
	if err != nil {
		return nil, fmt.Errorf("antigravity: chunk transcript: %w", err)
	}
	return chunks, nil
}

func (a *AntigravityAgent) ReassembleTranscript(chunks [][]byte) ([]byte, error) {
	return agent.ReassembleJSONL(chunks), nil
}

// PrepareTranscript implements the optional TranscriptPreparer interface. The
// framework calls this in handleLifecycleTurnEnd BEFORE its fileExists check
// — so we use it to handle agy's asynchronous transcript write.
//
// Background: agy writes its transcript file at
//
//	~/.gemini/antigravity-cli/brain/<conv-id>/.system_generated/logs/transcript_full.jsonl
//
// AFTER the Stop hook fires (sometimes seconds later, depending on session
// shutdown timing). Our TurnEnd event maps to Stop, so we routinely race the
// transcript write. Without PrepareTranscript, the framework's fileExists
// check fails with "transcript file not found" and our hook returns exit 1,
// terminating agy's agent turn.
//
// We briefly wait for the real transcript first. If it is still missing,
// we materialise an empty placeholder. files_touched is already captured via
// the PreToolUse hook (independent of transcript content), so condensation can
// still produce a meaningful checkpoint from an empty transcript.
//
// The placeholder is a WRITE on a hook-supplied path, so it is created only
// inside agy's brain directory (the agent's SessionStore): parents with
// MkdirAllNoSymlink, the file with O_EXCL through the root, and a path outside
// that directory refused rather than anchored on its own parent
// (docs/development/filesystem-safety.md, "The Root Anchors"). Tests place
// transcripts under ENTIRE_TEST_ANTIGRAVITY_BRAIN_DIR for the same reason.
func (a *AntigravityAgent) PrepareTranscript(ctx context.Context, transcriptRef string) error {
	if transcriptRef == "" {
		return nil
	}
	store, name, err := a.transcriptStore(transcriptRef)
	if err != nil {
		return fmt.Errorf("antigravity: prepare transcript: %w", err)
	}

	deadline := time.Now().Add(1 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	// Poll until the transcript has content, the deadline passes, or ctx ends
	// (the select below wakes at once on an already-cancelled ctx). A cancelled
	// ctx stops the WAIT, not the fallback: the lifecycle only logs this
	// function's error and then requires the file to exist, so returning early
	// here would fail the Stop hook — the exact outcome the placeholder exists
	// to prevent. Both exits fall through to the exclusive create.
poll:
	for {
		info, err := store.Lstat(name)
		if err == nil {
			// Refuse a symlink rather than read through it: the path came from
			// agy's hook payload, and a link here would send the condensation
			// read wherever it points (docs/development/filesystem-safety.md).
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("antigravity: transcript %s: %w", transcriptRef, osroot.ErrSymlinkedPath)
			}
			if info.Size() > 0 {
				return nil
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("antigravity: stat transcript: %w", err)
		}

		if !time.Now().Before(deadline) {
			break
		}

		wait := 50 * time.Millisecond
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			break poll
		case <-timer.C:
		}
	}

	// Exclusive create: if agy wrote the real transcript between the last poll
	// and here, it wins and the placeholder is skipped — never replaced.
	if err := store.CreateExclusive(name, 0o600); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("antigravity: create empty transcript placeholder: %w", err)
	}
	return nil
}

// transcriptStore resolves transcriptRef to agy's brain-directory session store
// (GetSessionDir, which ignores the repo path: agy keeps one brain per user)
// and a name inside it. A path outside the store is reported through
// agent.ErrOutsideSessionStore; it is never re-anchored on the path's own
// parent, the derived base the filesystem-safety rules refuse because it
// contains nothing while looking like it does.
func (a *AntigravityAgent) transcriptStore(transcriptRef string) (*agent.SessionStore, string, error) {
	// agy's payload always carries an absolute path. SessionStore.Name would
	// accept a relative one as a name inside the store, which is not what a
	// relative path means to the callers that pass one (fixtures resolved
	// against the working directory), so it is classified as outside instead.
	if !filepath.IsAbs(transcriptRef) {
		return nil, "", fmt.Errorf("%w: %s is not absolute", agent.ErrOutsideSessionStore, transcriptRef)
	}
	store, err := agent.OpenSessionStore(a, "")
	if err != nil {
		return nil, "", err //nolint:wrapcheck // the store already names the agent and directory
	}
	name, err := store.Name(transcriptRef)
	if err != nil {
		return nil, "", err //nolint:wrapcheck // preserved for errors.Is(err, agent.ErrOutsideSessionStore)
	}
	return store, name, nil
}
