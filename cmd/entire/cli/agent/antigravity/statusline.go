package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/internal/flock"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/internal/entireclient/userdirs"
)

// agy's title/statusline hook pipes a state JSON to the configured command on
// every agent state change. The context_window object is the ONLY surface
// where agy exposes token usage — it never appears in transcripts or
// lifecycle hook payloads. AppendStatusSnapshot persists those snapshots so
// the lifecycle can compute per-checkpoint deltas later.
//
// Totals are cumulative per conversation; current_usage is the latest API call.

// statusDirEnv overrides the snapshot cache directory (tests, ops).
const statusDirEnv = "ENTIRE_ANTIGRAVITY_STATUS_DIR"

// statusRetention is how long snapshot files for other conversations are kept.
const statusRetention = 14 * 24 * time.Hour

// statusCurrentUsage mirrors context_window.current_usage in the payload.
type statusCurrentUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// statusContextWindow mirrors context_window in the payload.
type statusContextWindow struct {
	TotalInputTokens  int                 `json:"total_input_tokens"`
	TotalOutputTokens int                 `json:"total_output_tokens"`
	ContextWindowSize int                 `json:"context_window_size,omitempty"`
	CurrentUsage      *statusCurrentUsage `json:"current_usage,omitempty"`
}

// statusSnapshot is one persisted line in <conversation_id>.jsonl.
type statusSnapshot struct {
	Timestamp      string              `json:"ts"`
	ConversationID string              `json:"conversation_id"`
	ContextWindow  statusContextWindow `json:"context_window"`
}

// statuslinePayload is the subset of agy's state JSON we consume.
type statuslinePayload struct {
	ConversationID string               `json:"conversation_id"`
	ContextWindow  *statusContextWindow `json:"context_window"`
}

// statusStore is where snapshot files live: a shared *os.Root anchor plus the
// directory name inside it (docs/development/filesystem-safety.md, "The Root
// Anchors"). Every read, append, lock and prune below is a NAME inside this
// root, so a symlink planted at any component is refused instead of followed.
type statusStore struct {
	root *os.Root
	// dir is the directory inside root holding the JSONL files. "" means the
	// root itself is the directory (the override case).
	dir string
}

// statusDefaultDir is the store's location inside the per-user cache root.
var statusDefaultDir = filepath.Join("antigravity", "status")

// openStatusStore resolves the store. It honours the ENTIRE_ANTIGRAVITY_STATUS_DIR
// env override (tests, ops), otherwise anchors on userdirs.CacheRoot. userdirs is
// the mandated resolver: it honours $XDG_CACHE_HOME on every platform
// (os.UserCacheDir ignores it on darwin, defeating harness isolation), falls back
// to a throwaway per-process dir under `go test`, and refuses a relative
// override before anything is created. The override is held to the same rule
// (RequireAbsoluteOverride) and opened through the shared registry like every
// other anchor, never as filepath.Dir of the file about to be written.
func openStatusStore() (statusStore, error) {
	if override := os.Getenv(statusDirEnv); override != "" {
		if err := userdirs.RequireAbsoluteOverride(statusDirEnv, override); err != nil {
			return statusStore{}, fmt.Errorf("antigravity status: %w", err)
		}
		if err := userdirs.EnsurePrivateDir(override); err != nil {
			return statusStore{}, fmt.Errorf("antigravity status: %w", err)
		}
		root, err := osroot.Shared(override)
		if err != nil {
			return statusStore{}, fmt.Errorf("antigravity status: open %s: %w", statusDirEnv, err)
		}
		return statusStore{root: root}, nil
	}
	root, err := userdirs.CacheRoot()
	if err != nil {
		return statusStore{}, fmt.Errorf("antigravity status: resolve cache dir: %w", err)
	}
	return statusStore{root: root, dir: statusDefaultDir}, nil
}

// dirName is the store directory as a name inside the root ("." for the root).
func (st statusStore) dirName() string {
	if st.dir == "" {
		return "."
	}
	return st.dir
}

// fileName returns the name inside the root of a conversation's JSONL file.
// filepath.Base guards against path traversal in the conversation ID.
func (st statusStore) fileName(conversationID string) string {
	return filepath.Join(st.dir, filepath.Base(conversationID)+".jsonl")
}

// statusFilePath returns the absolute path of a conversation's snapshot file.
// Diagnostics and tests only: production I/O goes through the root by name.
func statusFilePath(conversationID string) (string, error) {
	st, err := openStatusStore()
	if err != nil {
		return "", err
	}
	return filepath.Join(st.root.Name(), st.fileName(conversationID)), nil
}

// AppendStatusSnapshot parses an agy state-JSON payload and appends a snapshot
// to the per-conversation JSONL file. The hot path never returns an error for
// malformed input — only for genuine I/O failures.
//
// agy fires the title command on every agent state change and does not
// serialize the invocations, so two tees can run at once. The dedup read and
// the append therefore happen under one advisory lock per conversation
// (<id>.jsonl.lock, next to the file); without it a concurrent tee could append
// between the read and the write and the dedup would miss.
func AppendStatusSnapshot(payload []byte) error {
	var p statuslinePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	if p.ConversationID == "" || p.ContextWindow == nil {
		return nil // missing required fields — silently skip
	}

	// Dedup: compare compact JSON of the new context_window against the last
	// persisted line's context_window.
	newCWBytes, err := json.Marshal(p.ContextWindow)
	if err != nil {
		return nil
	}

	st, err := openStatusStore()
	if err != nil {
		return err
	}
	if st.dir != "" {
		if err := osroot.MkdirAllNoSymlink(st.root, st.dir, 0o750); err != nil {
			return fmt.Errorf("antigravity status: mkdir: %w", err)
		}
	}
	name := st.fileName(p.ConversationID)

	release, err := flock.AcquireIn(st.root, name+".lock")
	if err != nil {
		return fmt.Errorf("antigravity status: lock: %w", err)
	}
	defer release()

	f, err := osroot.OpenFileNoFollow(st.root, name, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("antigravity status: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("antigravity status: stat: %w", err)
	}
	isNew := info.Size() == 0
	if !isNew {
		lastSnap, readErr := readLastSnapshotFrom(f)
		if readErr == nil && lastSnap != nil {
			lastCWBytes, marshalErr := json.Marshal(lastSnap.ContextWindow)
			if marshalErr == nil && bytes.Equal(newCWBytes, lastCWBytes) {
				return nil // duplicate — skip
			}
		}
	}

	snap := statusSnapshot{
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
		ConversationID: p.ConversationID,
		ContextWindow:  *p.ContextWindow,
	}
	line, err := json.Marshal(snap)
	if err != nil {
		return nil
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("antigravity status: write: %w", err)
	}

	// Best-effort prune of stale files for other conversations when we first
	// create the active file (avoids per-append overhead).
	if isNew {
		pruneStaleStatusFiles(st, p.ConversationID)
	}

	return nil
}

// SnapshotTokenBaseline returns the latest persisted snapshot for the
// conversation, or nil if none exists yet. A nil baseline is exact only for a
// genuinely fresh conversation; a resumed conversation whose title-tee shim
// hasn't written a snapshot before the first TurnStart will over-count the
// prior cumulative total on that first tracked turn.
func (a *AntigravityAgent) SnapshotTokenBaseline(_ context.Context, sessionID string) (json.RawMessage, error) {
	st, err := openStatusStore()
	if err != nil {
		return nil, nil //nolint:nilerr // ditto: an unusable status dir means no baseline
	}
	snap, err := readLastStatusSnapshot(st, st.fileName(sessionID))
	if err != nil || snap == nil {
		return nil, nil //nolint:nilerr // ditto (missing file, no lines, malformed)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, nil //nolint:nilerr // ditto
	}
	return raw, nil
}

// CalculateTokenUsageSince computes the delta between the baseline snapshot
// and the latest persisted snapshot.
//
// Exact: InputTokens/OutputTokens (cumulative totals minus baseline totals).
// Best-effort: cache fields and APICallCount, derived from the snapshot lines
// appended after the baseline timestamp (the dedup writer appends ~one line
// per API response, but lines can be missed between agent state changes).
func (a *AntigravityAgent) CalculateTokenUsageSince(_ context.Context, sessionID string, baseline json.RawMessage) (*agent.TokenUsage, error) {
	snaps, err := readStatusSnapshots(sessionID)
	if err != nil || len(snaps) == 0 {
		return nil, nil //nolint:nilerr,nilnil // no data -> no token counts, never an error
	}

	var base statusSnapshot
	if len(baseline) > 0 {
		_ = json.Unmarshal(baseline, &base) //nolint:errcheck // unparseable baseline -> zero baseline
	}

	latest := snaps[len(snaps)-1]
	usage := &agent.TokenUsage{
		InputTokens:  max(0, latest.ContextWindow.TotalInputTokens-base.ContextWindow.TotalInputTokens),
		OutputTokens: max(0, latest.ContextWindow.TotalOutputTokens-base.ContextWindow.TotalOutputTokens),
	}

	// The strictly-after (.After, not >=) filter is load-bearing for
	// multi-turn correctness: turn N+1's baseline IS turn N's latest snapshot,
	// so excluding the equal-timestamp boundary line prevents re-counting it.
	// Changing this to >= would double-count the boundary line every turn.
	baseTS, baseTSErr := time.Parse(time.RFC3339Nano, base.Timestamp)
	for _, s := range snaps {
		// If baseTS is unparseable we count cache/apicalls over all lines; accepted because input/output remain exact via the totals delta.
		if base.Timestamp != "" && baseTSErr == nil {
			ts, parseErr := time.Parse(time.RFC3339Nano, s.Timestamp)
			if parseErr != nil || !ts.After(baseTS) {
				continue
			}
		}
		usage.APICallCount++
		if cu := s.ContextWindow.CurrentUsage; cu != nil {
			usage.CacheCreationTokens += cu.CacheCreationInputTokens
			usage.CacheReadTokens += cu.CacheReadInputTokens
		}
	}

	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheCreationTokens == 0 && usage.CacheReadTokens == 0 {
		return nil, nil //nolint:nilnil // nothing observed this turn
	}
	return usage, nil
}

// statusTailWindow bounds how many bytes readLastSnapshotFrom reads from the
// end of the file. Snapshot lines are well under 1 KB, so 64 KB always covers
// the final line with huge margin.
const statusTailWindow = 64 * 1024

// readLastStatusSnapshot opens name inside the store and returns its final
// snapshot; a missing file is nil, nil.
func readLastStatusSnapshot(st statusStore, name string) (*statusSnapshot, error) {
	f, err := osroot.OpenNoFollow(st.root, name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil //nolint:nilnil // a missing file is "no snapshots yet", not an error
		}
		return nil, fmt.Errorf("antigravity status: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	return readLastSnapshotFrom(f)
}

// readLastSnapshotFrom returns the snapshot on the final non-empty line of f,
// or nil if the file has no usable line. It reads a bounded tail window instead
// of streaming the whole file: it is shared by the per-fire dedup comparison
// in AppendStatusSnapshot (on the already-open, locked descriptor) and by
// every-TurnStart SnapshotTokenBaseline, and agy fires the title command on
// each agent state change — a front-to-back scan would cost O(file) per fire,
// O(n^2) over a conversation.
func readLastSnapshotFrom(f *os.File) (*statusSnapshot, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("antigravity status: stat: %w", err)
	}

	offset := info.Size() - statusTailWindow
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("antigravity status: read tail: %w", err)
	}

	// When the window starts mid-file, the first chunk may be a partial line —
	// discard through the first newline so only whole lines are considered.
	if offset > 0 {
		nl := bytes.IndexByte(buf, '\n')
		if nl < 0 {
			return nil, nil //nolint:nilnil // single line larger than the window — treat as no usable snapshot
		}
		buf = buf[nl+1:]
	}

	var lastLine []byte
	for _, line := range bytes.Split(buf, []byte("\n")) {
		if line = bytes.TrimSpace(line); len(line) > 0 {
			lastLine = line
		}
	}
	if len(lastLine) == 0 {
		return nil, nil //nolint:nilnil // no lines yet — caller handles nil gracefully
	}

	var snap statusSnapshot
	if err := json.Unmarshal(lastLine, &snap); err != nil {
		return nil, nil //nolint:nilerr,nilnil // malformed last line — treat as no prior snapshot
	}
	return &snap, nil
}

// readStatusSnapshots reads all valid snapshot lines from the JSONL file for
// the given conversationID. A missing file returns nil, nil (not an error).
func readStatusSnapshots(conversationID string) ([]statusSnapshot, error) {
	st, err := openStatusStore()
	if err != nil {
		return nil, err
	}
	f, err := osroot.OpenNoFollow(st.root, st.fileName(conversationID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("antigravity status: open for read: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var snaps []statusSnapshot
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var snap statusSnapshot
		if err := json.Unmarshal([]byte(line), &snap); err != nil {
			continue // skip malformed lines
		}
		snaps = append(snaps, snap)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("antigravity status: scan: %w", err)
	}
	return snaps, nil
}

// pruneStaleStatusFiles removes files in the store that do not belong to the
// active conversation and whose mtime is older than statusRetention — the
// JSONL files and their lock files alike. Best-effort: errors are silently
// ignored. Entries are lstat'ed and unlinked by name inside the root, so a
// symlink planted in the store costs the link, never its target.
func pruneStaleStatusFiles(st statusStore, activeConversationID string) {
	activePrefix := filepath.Base(activeConversationID) + ".jsonl"
	entries, err := osroot.ReadDirNoSymlinks(st.root, st.dirName())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-statusRetention)
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), activePrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = osroot.RemoveNoSymlinks(st.root, filepath.Join(st.dir, entry.Name())) //nolint:errcheck // best-effort prune
		}
	}
}
