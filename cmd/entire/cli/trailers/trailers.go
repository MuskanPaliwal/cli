// Package trailers provides parsing and formatting for Entire commit message trailers.
// Trailers are key-value metadata appended to git commit messages following the
// git trailer convention (key: value format after a blank line).
package trailers

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	checkpointID "github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// Trailer key constants used in commit messages.
const (
	// MetadataTrailerKey points to the metadata directory within a commit tree.
	MetadataTrailerKey = "Entire-Metadata"

	// StrategyTrailerKey indicates which strategy created the commit.
	StrategyTrailerKey = "Entire-Strategy"

	// SessionTrailerKey identifies which session created a commit.
	SessionTrailerKey = "Entire-Session"

	// CheckpointTrailerKey links commits to their checkpoint metadata on entire/checkpoints/v1.
	// Format: a checkpoint ID — either a legacy 12-hex ID (e.g. "a3b2c4d5e6f7")
	// or a 26-char ULID (see checkpoint/id.CheckpointPattern).
	// This trailer survives git amend and rebase operations.
	CheckpointTrailerKey = "Entire-Checkpoint"

	// AgentTrailerKey identifies the agent that created a checkpoint.
	// Format: human-readable agent name e.g. "Claude Code", "Cursor"
	AgentTrailerKey = "Entire-Agent"

	// OPFAppliedTrailerKey marks an entire/checkpoints/v1 commit whose blobs
	// have been redacted by the OpenAI Privacy Filter (the opt-in 9th,
	// network-backed layer, applied on top of the 8 regex layers).
	// Format: literal "true"; the trailer is omitted entirely when OPF was
	// not applied. The pre-push rewrite path treats commits lacking this
	// trailer as candidates to OPF-redact before they reach the remote.
	OPFAppliedTrailerKey = "Entire-OPF-Applied"

	// OPFAppliedTrailerValue is the only value that means "OPF ran." Any
	// other value (or trailer absence) is treated as "not applied" so a
	// future "false" / "skipped" value never accidentally enables OPF.
	OPFAppliedTrailerValue = "true"
)

// Pre-compiled regexes for trailer parsing.
var (
	metadataTrailerRegex   = regexp.MustCompile(MetadataTrailerKey + `:\s*(.+)`)
	sessionTrailerRegex    = regexp.MustCompile(SessionTrailerKey + `:\s*(.+)`)
	checkpointTrailerRegex = regexp.MustCompile(CheckpointTrailerKey + `:\s*(` + checkpointID.CheckpointPattern + `)(?:\s|$)`)
)

// ParseMetadata extracts metadata dir from commit message.
// Returns the metadata directory and true if found, empty string and false otherwise.
func ParseMetadata(commitMessage string) (string, bool) {
	matches := metadataTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseSession extracts the session ID from a commit message.
// Returns the session ID and true if found, empty string and false otherwise.
// Note: If multiple Entire-Session trailers exist, this returns only the first one.
func ParseSession(commitMessage string) (string, bool) {
	matches := sessionTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1]), true
	}
	return "", false
}

// ParseCheckpoint discovers the first checkpoint ID anywhere in a commit
// message. It supports squash history; callers must not treat the result as a
// trusted trailer when deciding whether to create, reuse, or mutate a checkpoint.
// Returns the CheckpointID and true if found, empty ID and false otherwise.
func ParseCheckpoint(commitMessage string) (checkpointID.CheckpointID, bool) {
	matches := checkpointTrailerRegex.FindStringSubmatch(commitMessage)
	if len(matches) > 1 {
		idStr := strings.TrimSpace(matches[1])
		// Validate it's a proper checkpoint ID
		if cpID, err := checkpointID.NewCheckpointID(idStr); err == nil {
			return cpID, true
		}
	}
	return checkpointID.EmptyCheckpointID, false
}

// ParseAllCheckpoints discovers all checkpoint IDs in a commit message.
// Returns a slice of CheckpointIDs (may be empty if none found).
// Duplicate IDs are deduplicated while preserving order.
// This is useful for squash merge commits that contain multiple Entire-Checkpoint trailers.
// The results are discovery candidates, not proof that the final trailer block
// authorized checkpoint mutation or reuse.
func ParseAllCheckpoints(commitMessage string) []checkpointID.CheckpointID {
	matches := checkpointTrailerRegex.FindAllStringSubmatch(commitMessage, -1)
	if len(matches) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	ids := make([]checkpointID.CheckpointID, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			idStr := strings.TrimSpace(match[1])
			if !seen[idStr] {
				if cpID, err := checkpointID.NewCheckpointID(idStr); err == nil {
					seen[idStr] = true
					ids = append(ids, cpID)
				}
			}
		}
	}
	return ids
}

// ParseCheckpointFromFinalTrailerBlock extracts the first checkpoint ID from
// the final trailer block. Commit body text is not considered.
func ParseCheckpointFromFinalTrailerBlock(commitMessage string) (checkpointID.CheckpointID, bool) {
	for _, line := range finalTrailerBlock(commitMessage) {
		if cpID, ok := checkpointFromTrailerLine(line); ok {
			return cpID, true
		}
	}
	return checkpointID.EmptyCheckpointID, false
}

// ParseAllCheckpointsFromFinalTrailerBlock extracts checkpoint IDs from the
// final trailer block, deduplicating them while preserving order.
func ParseAllCheckpointsFromFinalTrailerBlock(commitMessage string) []checkpointID.CheckpointID {
	lines := finalTrailerBlock(commitMessage)
	if len(lines) == 0 {
		return nil
	}

	seen := make(map[string]bool)
	ids := make([]checkpointID.CheckpointID, 0, len(lines))
	for _, line := range lines {
		cpID, ok := checkpointFromTrailerLine(line)
		if !ok || seen[cpID.String()] {
			continue
		}
		seen[cpID.String()] = true
		ids = append(ids, cpID)
	}
	return ids
}

func checkpointFromTrailerLine(line string) (checkpointID.CheckpointID, bool) {
	key, value, ok := strings.Cut(line, ":")
	if !ok || key != CheckpointTrailerKey {
		return checkpointID.EmptyCheckpointID, false
	}
	cpID, err := checkpointID.NewCheckpointID(strings.TrimSpace(value))
	return cpID, err == nil
}

// checkpointTrailerLineRegex matches one whole Entire-Checkpoint trailer line,
// however the separator is spaced ("Entire-Checkpoint:<id>", a tab).
var checkpointTrailerLineRegex = regexp.MustCompile(`^` + CheckpointTrailerKey + `:\s*(` + checkpointID.CheckpointPattern + `)\s*$`)

// scissorsMarker is the core of git's scissors line ("# ---- >8 ----"); with
// `commit -v` everything below it is the diff, which git discards.
const scissorsMarker = "------------------------ >8 ------------------------"

// DefaultCommentPrefix is git's comment prefix when core.commentChar is unset.
const DefaultCommentPrefix = "#"

// autoCommentCandidates are the prefixes git tries, in order, for
// core.commentChar=auto.
const autoCommentCandidates = "#;@!$%^&|:"

// isScissorsLine reports whether line is git's scissors line for commentPrefix.
// Git cuts only at that exact line, never at prose that mentions the marker.
func isScissorsLine(line, commentPrefix string) bool {
	return strings.TrimSuffix(line, "\r") == commentPrefix+" "+scissorsMarker
}

// StripEditorComments returns an editor-written commit message without the
// lines git's default cleanup drops before the commit is recorded: everything
// from the scissors line down, and every line starting with commentPrefix. An
// empty commentPrefix, for a -m or -F message whose lines git keeps as
// written, returns message unchanged.
func StripEditorComments(message, commentPrefix string) string {
	if commentPrefix == "" {
		return message
	}
	lines := strings.Split(CutAtScissors(message, commentPrefix), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.HasPrefix(line, commentPrefix) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// CutAtScissors returns message without git's scissors line for commentPrefix
// and everything below it, which git discards from a `commit -v` message. An
// empty commentPrefix returns message unchanged.
func CutAtScissors(message, commentPrefix string) string {
	if commentPrefix == "" {
		return message
	}
	lines := strings.Split(message, "\n")
	for i, line := range lines {
		if isScissorsLine(line, commentPrefix) {
			return strings.Join(lines[:i], "\n")
		}
	}
	return message
}

// AutoCommentPrefix returns the prefix git chose for core.commentChar=auto in a
// message it prepared for editing. Git picks it before adding its comment
// block, so it is read back from git's own lines: the scissors line if there is
// one, else the comment block that ends the message. A message without either
// (commit.status=false writes no block) gets DefaultCommentPrefix, unless its
// last line happens to start with a candidate.
func AutoCommentPrefix(message string) string {
	lines := strings.Split(message, "\n")
	for _, line := range lines {
		for _, candidate := range autoCommentCandidates {
			if isScissorsLine(line, string(candidate)) {
				return string(candidate)
			}
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if first := lines[i][:1]; strings.Contains(autoCommentCandidates, first) {
			return first
		}
		break
	}
	return DefaultCommentPrefix
}

// RemoveCheckpointTrailers removes every whole Entire-Checkpoint trailer line
// whose ID drop selects, anywhere between the subject paragraph and a scissors
// line, and returns the new message with the IDs removed (nil when the
// message is unchanged). It does not try to reproduce git's trailer-block
// rules (mixed blocks, a "(cherry picked from ...)" note, custom comment
// characters): a line that is exactly a checkpoint trailer is one in every
// form git accepts, and leaving one behind would let an amend keep a deleted
// checkpoint alive. Indentation is ignored, since a squash message indents
// the squashed commits' messages. Prose that merely mentions an ID is not a
// whole trailer line and stays. Blank lines a removal leaves doubled are
// collapsed, and trailing blank lines are trimmed unless a scissors line
// follows; nothing at or below the scissors line is touched.
func RemoveCheckpointTrailers(message string, drop func(checkpointID.CheckpointID) bool) (string, []checkpointID.CheckpointID) {
	lines := strings.Split(message, "\n")
	bodyStart := subjectParagraphEnd(lines)
	end := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(line, scissorsMarker) })
	hasScissors := end >= 0
	if !hasScissors {
		end = len(lines)
	}
	removeLine := make([]bool, end)
	var removed []checkpointID.CheckpointID
	for i := bodyStart; i < end; i++ {
		match := checkpointTrailerLineRegex.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if match == nil {
			continue
		}
		cpID, err := checkpointID.NewCheckpointID(match[1])
		if err != nil || !drop(cpID) {
			continue
		}
		removeLine[i] = true
		if !slices.Contains(removed, cpID) {
			removed = append(removed, cpID)
		}
	}
	if len(removed) == 0 {
		return message, nil
	}
	if hasScissors {
		kept := keepLines(lines[:end], removeLine, false, false)
		return strings.Join(append(kept, lines[end:]...), "\n"), removed
	}
	return strings.Join(keepLines(lines, removeLine, true, strings.HasSuffix(message, "\n")), "\n"), removed
}

// subjectParagraphEnd returns the index just past the subject paragraph: the
// first run of non-blank lines.
func subjectParagraphEnd(lines []string) int {
	i := 0
	for i < len(lines) && isBlankLine(lines[i]) {
		i++
	}
	for i < len(lines) && !isBlankLine(lines[i]) {
		i++
	}
	return i
}

// keepLines drops the removed lines and collapses a blank line that a
// removal would leave doubled. With trimTrailing it also trims trailing blank
// lines, keeping the message's final newline.
func keepLines(lines []string, removeLine []bool, trimTrailing, endsWithNewline bool) []string {
	kept := make([]string, 0, len(lines))
	afterRemoval := false
	for i, line := range lines {
		if removeLine[i] {
			afterRemoval = true
			continue
		}
		blank := isBlankLine(line)
		if blank && afterRemoval && len(kept) > 0 && isBlankLine(kept[len(kept)-1]) {
			continue
		}
		if !blank {
			afterRemoval = false
		}
		kept = append(kept, line)
	}
	if !trimTrailing {
		return kept
	}
	for len(kept) > 0 && isBlankLine(kept[len(kept)-1]) {
		kept = kept[:len(kept)-1]
	}
	if endsWithNewline {
		kept = append(kept, "")
	}
	return kept
}

func isBlankLine(line string) bool {
	return strings.TrimSpace(line) == ""
}

// FormatCheckpoint creates a commit message with a checkpoint trailer.
// This links user commits to their checkpoint metadata on entire/checkpoints/v1 branch.
func FormatCheckpoint(message string, cpID checkpointID.CheckpointID) string {
	return fmt.Sprintf("%s\n\n%s: %s\n", message, CheckpointTrailerKey, cpID.String())
}

// trailerLineRe matches git trailer format: "Key-Name:" at the start of the
// line, with no space before the colon. Like git, it accepts any separator
// after the colon ("Key:value", a tab), which also makes a line starting with a
// URL ("https://...") trailer-shaped.
var trailerLineRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*:`)

// IsTrailerLine reports whether a line matches git trailer format.
func IsTrailerLine(line string) bool {
	return trailerLineRe.MatchString(line)
}

// appendTrailerLine appends a single pre-formatted trailer line (e.g. "Key: value")
// to message in trailer-block-aware format. If the message's final paragraph is a
// trailer block by the same grammar finalTrailerBlock reads with, the line is
// joined directly to it; otherwise a blank line is inserted first to start a new
// trailer block. Sharing the grammar is the contract: a paragraph the writer joins
// must be one the strict readers accept, or the appended trailer disappears with it.
func appendTrailerLine(message, trailerLine string) string {
	trimmed := strings.TrimRight(message, "\n")

	// Collect the final paragraph. Joining also requires a blank line above
	// the paragraph so a trailer-shaped subject line (e.g. "fix: bug") never
	// has the trailer glued onto it.
	lines := strings.Split(trimmed, "\n")
	start := len(lines)
	separated := false
	for start > 0 {
		if strings.TrimSpace(lines[start-1]) == "" {
			separated = true
			break
		}
		start--
	}

	if separated && isTrailerBlock(lines[start:]) {
		return trimmed + "\n" + trailerLine + "\n"
	}
	return trimmed + "\n\n" + trailerLine + "\n"
}

// AppendCheckpointTrailer appends Entire-Checkpoint in trailer-aware format
// (see appendTrailerLine).
func AppendCheckpointTrailer(message, checkpointID string) string {
	trailer := fmt.Sprintf("%s: %s", CheckpointTrailerKey, checkpointID)
	return appendTrailerLine(message, trailer)
}

// HasOPFApplied reports whether the commit message carries an
// `Entire-OPF-Applied: true` trailer. Any other value (or absence) is
// treated as "OPF not applied" so the pre-push rewrite considers the
// commit a candidate for OPF redaction. Pinning the value to literal
// "true" — rather than just trailer presence — prevents a future
// "Entire-OPF-Applied: false" or "skipped" from accidentally meaning
// "yes, applied."
func HasOPFApplied(commitMessage string) bool {
	for _, line := range finalTrailerBlock(commitMessage) {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key != OPFAppliedTrailerKey {
			continue
		}
		if strings.TrimSpace(value) == OPFAppliedTrailerValue {
			return true
		}
	}
	return false
}

func finalTrailerBlock(message string) []string {
	trimmed := strings.TrimRight(message, "\n")
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	i := len(lines) - 1
	for i >= 0 && strings.TrimSpace(lines[i]) == "" {
		i--
	}
	end := i + 1
	for i >= 0 && strings.TrimSpace(lines[i]) != "" {
		i--
	}
	start := i + 1
	// Git reads the first paragraph as the subject, never as trailers.
	if start == 0 || !isTrailerBlock(lines[start:end]) {
		return nil
	}
	return lines[start:end]
}

// isTrailerBlock reports whether lines form a trailer block: at least one
// trailer line, with every line either a trailer or an indented continuation
// of a preceding trailer's value. Both finalTrailerBlock and appendTrailerLine
// classify with this so the writer never joins onto a paragraph the readers reject.
func isTrailerBlock(lines []string) bool {
	seenTrailer := false
	for _, line := range lines {
		switch {
		case IsTrailerLine(line):
			seenTrailer = true
		case seenTrailer && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")):
			// Git treats an indented line after a trailer as a continuation
			// of that trailer's value, never as a new trailer.
		default:
			return false
		}
	}
	return seenTrailer
}

// AppendOPFAppliedTrailer appends `Entire-OPF-Applied: true` in
// trailer-aware format. Idempotent: if the message already carries
// the trailer with value "true", the original message is returned
// unchanged so re-parenting an already-applied commit doesn't
// duplicate the trailer.
func AppendOPFAppliedTrailer(message string) string {
	if HasOPFApplied(message) {
		return message
	}
	trailer := fmt.Sprintf("%s: %s", OPFAppliedTrailerKey, OPFAppliedTrailerValue)
	return appendTrailerLine(message, trailer)
}
