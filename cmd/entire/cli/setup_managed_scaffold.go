package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// managedScaffoldStatus is the outcome of writing an Entire-managed scaffold file
// (a search skill, an agent-help skill, ...). Statuses are shared so each
// feature's reporter can switch on them.
type managedScaffoldStatus string

const (
	managedScaffoldUnsupported     managedScaffoldStatus = "unsupported"
	managedScaffoldCreated         managedScaffoldStatus = "created"
	managedScaffoldUpdated         managedScaffoldStatus = "updated"
	managedScaffoldUnchanged       managedScaffoldStatus = "unchanged"
	managedScaffoldSkippedConflict managedScaffoldStatus = "skipped_conflict"
)

type managedScaffoldResult struct {
	Status  managedScaffoldStatus
	RelPath string
	// RemovedLegacyRelPath is the repo-relative path of a superseded
	// Entire-managed file deleted alongside this install ("" when none).
	RemovedLegacyRelPath string
	// LegacyCleanupWarning describes a failed best-effort deletion of a
	// superseded Entire-managed file ("" when cleanup succeeded or there was
	// nothing to clean). The install itself still succeeded.
	LegacyCleanupWarning string
}

// writeManagedScaffold writes content to relPath under root idempotently: it
// creates the file when absent, leaves it untouched (Unchanged) when
// identical, rewrites it (Updated) only when Entire already manages it, and
// refuses to clobber an unmanaged file (SkippedConflict). isManaged reports
// whether existing bytes carry this feature's management marker.
//
// All IO goes through the *os.Root so a symlinked path component cannot
// redirect the write outside the repository, and the write lands via rename
// so a symlink at the target (dangling ones read as "absent" — the shape
// settings.readConfined exists for) is replaced rather than written through.
// The relative path is fixed, but its resolution is not; that is why the
// root, not the caller-joined absolute path, is the API.
//
// Confinement alone is not the whole property. relPath names a file under an
// agent's own directory — .claude/skills/, .claude/agents/, .codex/agents/,
// .gemini/agents/ — and those arrive with a checkout, so a repository can ship
// a symlink at `.claude`. os.Root refuses a component that escapes the root
// and silently follows one pointing elsewhere inside it, which is why
// NoSymlinkedParent guards the read and MkdirAllNoSymlink the create: an
// unmanaged file at the far end of a planted link must not be mistaken for one
// of ours and rewritten.
func writeManagedScaffold(root *os.Root, relPath string, content []byte, isManaged func([]byte) bool) (managedScaffoldResult, error) {
	name := filepath.ToSlash(relPath)
	if err := osroot.NoSymlinkedParent(root, name); err != nil {
		return managedScaffoldResult{}, fmt.Errorf("resolve managed scaffold path %s: %w", relPath, err)
	}

	existingData, err := root.ReadFile(relPath)
	if err == nil {
		if !isManaged(existingData) {
			return managedScaffoldResult{Status: managedScaffoldSkippedConflict, RelPath: relPath}, nil
		}
		if bytes.Equal(existingData, content) {
			return managedScaffoldResult{Status: managedScaffoldUnchanged, RelPath: relPath}, nil
		}
		if err := writeScaffoldViaRename(root, relPath, content); err != nil {
			return managedScaffoldResult{}, fmt.Errorf("update managed scaffold: %w", err)
		}
		return managedScaffoldResult{Status: managedScaffoldUpdated, RelPath: relPath}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return managedScaffoldResult{}, fmt.Errorf("read managed scaffold: %w", err)
	}

	// Scaffolds are ordinary project files meant to be committed, so they get
	// standard shareable permissions, not config-file 0o600/0o750.
	if dir := path.Dir(name); dir != "." {
		if err := osroot.MkdirAllNoSymlink(root, dir, 0o755); err != nil {
			return managedScaffoldResult{}, fmt.Errorf("create managed scaffold directory: %w", err)
		}
	}
	if err := writeScaffoldViaRename(root, relPath, content); err != nil {
		return managedScaffoldResult{}, fmt.Errorf("write managed scaffold: %w", err)
	}
	return managedScaffoldResult{Status: managedScaffoldCreated, RelPath: relPath}, nil
}

// writeScaffoldViaRename writes to a sibling temp file and renames it over
// relPath. Rename replaces a symlink at the target instead of writing through
// it (jsonutil.WriteFileAtomic's property); a root-relative Root.WriteFile
// alone would follow the link.
//
// The temp path is predictable, so a checkout can plant a symlink there
// pointing at another in-repo file — Root confinement would not stop a write
// through it. Two guards close that: any pre-existing entry at the temp path
// is removed first (Remove unlinks a planted link itself, and clears a stale
// temp a crashed run left behind), and the create is O_EXCL, so whatever
// still exists at the path fails the write instead of receiving it.
func writeScaffoldViaRename(root *os.Root, relPath string, content []byte) error {
	tmpPath := relPath + ".tmp"
	if err := root.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear scaffold temp path: %w", err)
	}
	tmp, err := root.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create scaffold temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = root.Remove(tmpPath) //nolint:errcheck // best-effort temp cleanup after a failed write
		return fmt.Errorf("write scaffold temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = root.Remove(tmpPath) //nolint:errcheck // best-effort temp cleanup after a failed close
		return fmt.Errorf("close scaffold temp file: %w", err)
	}
	if err := root.Rename(tmpPath, relPath); err != nil {
		_ = root.Remove(tmpPath) //nolint:errcheck // best-effort temp cleanup after a failed rename
		return fmt.Errorf("rename scaffold into place: %w", err)
	}
	return nil
}

// openScaffoldRoot returns the shared worktree anchor for confined scaffold IO.
//
// It goes through worktreedir rather than opening its own root because the
// worktree already has exactly one anchor and repoRoot is what a resolver
// answered. The returned root is owned by the registry and shared with every
// other reader and writer of this tree, so callers must not close it.
func openScaffoldRoot(repoRoot string) (*os.Root, error) {
	root, err := worktreedir.OpenAt(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("open repository root for scaffolding: %w", err)
	}
	return root, nil
}

// setupOptionalSkillForNames installs an optional skill (search, agent-help, ...)
// for each named agent when enabled: it dedups names, resolves each agent, and
// runs install, joining any errors. Both the search and agent-help skills share
// this dedup/iterate/error-join plumbing; only the guard bool and the per-agent
// installer differ.
func setupOptionalSkillForNames(
	ctx context.Context,
	w io.Writer,
	names []string,
	enabled bool,
	install func(context.Context, io.Writer, agent.Agent, EnableOptions) error,
	opts EnableOptions,
) error {
	if !enabled {
		return nil
	}

	var errs []error
	seen := make(map[types.AgentName]struct{}, len(names))
	for _, name := range names {
		agentName := types.AgentName(name)
		if _, ok := seen[agentName]; ok {
			continue
		}
		seen[agentName] = struct{}{}

		ag, err := agent.Get(agentName)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to get agent %s: %w", name, err))
			continue
		}
		if err := install(ctx, w, ag, opts); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// printSkillNonInteractiveNoAgentsGuidance prints the actionable message shown
// when an optional skill is requested in non-interactive mode but no agent is
// available to install it for. label is the human name ("search skill"), flag is
// the install flag name ("search-skill").
func printSkillNonInteractiveNoAgentsGuidance(w io.Writer, label, flag string) {
	fmt.Fprintf(w, "Cannot install the %s in non-interactive mode because no agents are enabled.\n", label)
	fmt.Fprintln(w, "Install it for a specific agent with:")
	fmt.Fprintf(w, "  entire enable --agent <name> --%s\n", flag)
	fmt.Fprintln(w, "or:")
	fmt.Fprintf(w, "  entire agent add <name> --%s\n", flag)
}
