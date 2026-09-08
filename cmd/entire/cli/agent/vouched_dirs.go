package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/osroot"
	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// Vouched agent directories: the one place a symlink Entire would otherwise
// refuse is followed on purpose.
//
// The refusal these opt out of is real and stays the default. A working tree
// arrives by clone, so a symlink at `.claude` can be something the REPOSITORY
// chose, and `entire enable` would then create directories and write JSON
// through it to wherever it points. Nothing about the link says which of those
// two it is.
//
// What tells them apart is not the link. It is who said the link is fine, and
// where they said it: settings.AllowSymlinkedAgentDirs is honored only from an
// untracked, index-and-HEAD-verified .entire/settings.local.json, the same gate
// that guards the OPF command and external_agents. A repository cannot put a
// path in that list, so following one is always this developer's own decision
// about their own machine.
//
// It exists because the setup is ordinary rather than exotic: dotfile managers
// (chezmoi, stow, yadm) symlink `.claude` into a repository, and the people
// running coding agents are the people who use those. Before this, the only
// answer was to stop managing the directory that way.
//
// Deliberately NOT extended to `.entire`, to the settings files, or to the git
// hooks directory:
//
//   - `.entire` holds the redaction settings that decide what may be committed
//     and pushed. "Follow this link to decide what your transcripts leak" is
//     not a knob worth having.
//   - A settings file is where the grant itself would live, so a symlinked
//     settings.local.json vouching for symlinked settings files would authorize
//     itself. There is no non-circular place to put that permission.
//   - The git hooks directory already has this escape hatch, and git owns it:
//     `git config core.hooksPath <target>` says the same thing without the
//     link. A second spelling would be ours to keep in step for no gain.
var (
	vouchedMu   sync.RWMutex
	vouchedDirs []string
)

// SetVouchedSymlinkedDirs installs the set of worktree-relative agent
// directories whose symlinks may be followed, replacing any previous set, and
// returns the entries it refused.
//
// Refusal is by NAME, against the directories actually derivable from the
// agents' own hook-config paths (see VouchableSymlinkedDirs). That is what keeps
// the list from becoming a general "follow symlinks" switch: `.entire`,
// `.git/hooks`, and anything else that is not an agent's config directory are
// not spellable here at all, whoever is doing the spelling.
//
// The zero value vouches for nothing, so a caller that never configures a
// policy gets the strict behaviour. That direction is deliberate: forgetting
// this call costs a refusal the user can act on, where the opposite default
// would silently follow links nobody approved.
func SetVouchedSymlinkedDirs(dirs []string) (rejected []string) {
	vouchable := VouchableSymlinkedDirs()
	accepted := make([]string, 0, len(dirs))
	for _, d := range dirs {
		clean := path.Clean(filepath.ToSlash(strings.TrimSpace(d)))
		clean = strings.TrimSuffix(clean, "/")
		if clean == "" || !slices.Contains(vouchable, clean) {
			rejected = append(rejected, d)
			continue
		}
		if !slices.Contains(accepted, clean) {
			accepted = append(accepted, clean)
		}
	}

	vouchedMu.Lock()
	vouchedDirs = accepted
	vouchedMu.Unlock()
	return rejected
}

// VouchedSymlinkedDirs returns the accepted set, for doctor and status to
// report what is being followed rather than leaving it invisible.
func VouchedSymlinkedDirs() []string {
	vouchedMu.RLock()
	defer vouchedMu.RUnlock()
	return slices.Clone(vouchedDirs)
}

// vouchableDirs is every worktree-relative directory a user may vouch for: each
// directory component of every BUILT-IN agent's hook-config path.
//
// Pinned rather than derived from AllHookConfigRelPaths, which was the first
// version and was wrong twice over. The registry is MUTABLE at runtime -- an
// external plugin registers into it -- so deriving would let a plugin widen the
// set of paths a user can be persuaded to vouch for, which is the opposite of
// what a boundary is for. And it is EMPTY in any binary that has not imported
// the agent packages, so the derived set silently refused every legitimate
// entry depending on the import graph of the caller.
//
// Drift is caught rather than trusted: TestVouchableDirsMatchTheBuiltInAgents
// runs in a binary where every built-in agent IS registered and fails in both
// directions, so a new agent that forgets this list cannot ship.
var vouchableDirs = []string{
	".claude",
	".codex",
	".cursor",
	".factory",
	".gemini",
	".github",
	".github/hooks",
	".opencode",
	".opencode/plugins",
	".pi",
	".pi/extensions",
	// .pi/extensions/entire is deliberately absent, and neverVouchable refuses
	// it a second time. See that function.
}

// VouchableSymlinkedDirs is the set a user may name, with the trees Entire owns
// removed whatever the list above says.
//
// neverVouchable is applied here rather than only trusted to be absent, because
// this is the last line between a settings file and a followed symlink: if a
// future agent ever declared a config path under .entire or .git, or the list
// above were edited carelessly, the structural rule still refuses it. Two cheap
// checks that must both pass beats one that must never be got wrong.
func VouchableSymlinkedDirs() []string {
	out := make([]string, 0, len(vouchableDirs))
	for _, d := range vouchableDirs {
		if neverVouchable(d) {
			continue
		}
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// neverVouchable names the paths whose symlink refusals are not negotiable,
// whoever is asking and however the path is spelled.
//
// Two rules. `.entire` holds the redaction settings that decide what may be
// committed, and `.git` holds the hooks directory, whose escape hatch is
// core.hooksPath rather than this one. Matched as path prefixes so a deeper
// path cannot slip under.
//
// And a directory named `entire` is refused wherever it appears, because that
// is the one directory Entire both CREATES and DELETES: HookConfigFile.RemoveDir
// removes `.pi/extensions/entire` wholesale on uninstall, since pi discovers
// extensions by directory and removing only the file leaves one it still loads.
// A path Entire owns the lifecycle of cannot also be a link the user manages --
// the two claims are incompatible, and the hatch is for the agent's own
// directory, not for Entire's scratch space inside it. Vouching for it also
// anchored the root ON that directory, so RemoveDir had nothing above it to
// delete from and refused, leaving the extension in place.
func neverVouchable(dir string) bool {
	for _, owned := range []string{".entire", ".git"} {
		if dir == owned || strings.HasPrefix(dir, owned+"/") {
			return true
		}
	}
	return path.Base(dir) == entireOwnedDirName
}

// isVouched reports whether a worktree-relative directory has been vouched for.
func isVouched(dir string) bool {
	vouchedMu.RLock()
	defer vouchedMu.RUnlock()
	return slices.Contains(vouchedDirs, dir)
}

// AnchorWorktreePath resolves the directory to anchor a root on for a
// worktree-relative path, following a vouched symlinked agent directory and
// returning the remaining name inside that directory.
//
// Walks the path's directory components outside in. A component that is not a
// symlink stays a NAME inside the current base, which is what keeps the strict
// open below able to refuse it; a component that IS a symlink is followed only
// when its accumulated worktree-relative path was vouched for, and re-anchors
// the walk on the resolved target. An unvouched link is left in the name on
// purpose, so the caller's own MkdirAllNoSymlink / LstatNoSymlinks refuses it
// and says which component, exactly as before.
//
// followed names the links that were resolved, so callers can report them. It
// is normally empty.
//
// Exported because the skill scaffolds (writeManagedScaffold) write under the
// same agent directories and must reach the same place. A vouched `.claude`
// that hook installation follows but scaffolding does not would leave `entire
// enable` half-applied, with files in two directories.
func AnchorWorktreePath(worktreeRoot, relPath string) (baseDir, name string, followed []string, err error) {
	segments := strings.Split(filepath.ToSlash(relPath), "/")
	baseDir = worktreeRoot
	onDisk := worktreeRoot
	vouchName := ""
	start := 0

	for i := range len(segments) - 1 {
		onDisk = filepath.Join(onDisk, segments[i])
		vouchName = path.Join(vouchName, segments[i])

		info, lerr := os.Lstat(onDisk)
		if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if !isVouched(vouchName) {
			continue
		}
		resolved, rerr := filepath.EvalSymlinks(onDisk)
		if rerr != nil {
			// A vouched but unresolvable link is an error rather than a silent
			// fallback to the strict path: the user said to follow this one, and
			// "we could not" is a different answer from "we would not".
			return "", "", nil, &VouchedDirError{Dir: vouchName, Err: rerr}
		}
		followed = append(followed, vouchName)
		baseDir = resolved
		onDisk = resolved
		start = i + 1
	}
	return baseDir, strings.Join(segments[start:], "/"), followed, nil
}

// OpenAnchoredRoot resolves relPath through any vouched symlinked agent
// directory and returns the root to work in plus the name inside it.
//
// The single place the anchor for a worktree-relative agent path is chosen, so
// the hook configs and the skill scaffolds cannot drift onto different bases.
// The two bases it can return are both trusted: the worktree root, which a
// resolver produced, and a vouched directory's resolved target, which the user
// named in a file only they can write.
func OpenAnchoredRoot(worktreeRoot, relPath string) (*os.Root, string, error) {
	baseDir, name, _, err := AnchorWorktreePath(worktreeRoot, relPath)
	if err != nil {
		return nil, "", err
	}
	if baseDir == worktreeRoot {
		root, openErr := worktreedir.OpenAt(worktreeRoot)
		if openErr != nil {
			return nil, "", fmt.Errorf("open worktree root: %w", openErr)
		}
		return root, name, nil
	}
	root, openErr := osroot.Shared(baseDir)
	if openErr != nil {
		return nil, "", fmt.Errorf("open vouched agent directory %s: %w", baseDir, openErr)
	}
	return root, name, nil
}

// VouchedDirError reports a vouched agent directory that could not be resolved.
type VouchedDirError struct {
	Dir string
	Err error
}

func (e *VouchedDirError) Error() string {
	return "resolve vouched agent directory " + e.Dir + ": " + e.Err.Error()
}

func (e *VouchedDirError) Unwrap() error { return e.Err }
