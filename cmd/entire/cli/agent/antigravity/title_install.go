package antigravity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/osroot"
)

// agy reads its window-title command from the GLOBAL config
// ~/.gemini/antigravity-cli/settings.json — a single slot:
//
//	{"title": {"type": "command", "command": "<cmd>"}}
//
// We occupy that slot with the title-tee shim (the title script receives the
// same state JSON as the statusline script — agy's only token-usage surface).
// A pre-existing user command is preserved INSIDE the shim invocation via
// --wrap '<original>', making the config self-describing: uninstall restores
// the original without any backup file. Because the slot is global, per-repo
// `entire disable` does NOT uninstall it (other repos may rely on it); only
// agent removal does.

// configDirEnv overrides the agy config directory (tests).
const configDirEnv = "ENTIRE_ANTIGRAVITY_CONFIG_DIR"

const titleTeeMarker = "hooks antigravity title-tee"

type titleConfig struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// agySettingsFileName is agy's global settings file inside its config dir.
const agySettingsFileName = "settings.json"

// agyConfigDir returns the agy config directory, honouring the override env var.
func agyConfigDir() (string, error) {
	if dir := os.Getenv(configDirEnv); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", configDirEnv, dir)
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to resolve home dir: %w", err)
	}
	return filepath.Join(home, ".gemini", "antigravity-cli"), nil
}

// openAgyConfigRoot opens an *os.Root over agy's config directory so
// settings.json is read and written as a NAME inside it, never through a
// symlink (docs/development/filesystem-safety.md). The directory is agy's own,
// resolved from HOME (or the operator override), so it is the trusted base;
// with create it is made first — the root is the directory itself, so it
// cannot be created through it. The caller closes the root: this is not one of
// the process-wide anchors, and the override changes between tests.
func openAgyConfigRoot(create bool) (*os.Root, error) {
	dir, err := agyConfigDir()
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("failed to create agy config dir: %w", err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err //nolint:wrapcheck // preserved for os.IsNotExist at call sites
	}
	return root, nil
}

// titleTeeCommand returns the full shell command string for the title-tee shim.
// If original is non-empty, the original command is wrapped via --wrap.
//
// The command always names the `entire` binary (resolved via $PATH): the title
// slot lives in agy's GLOBAL settings.json and is invoked from whatever
// directory agy runs in, so it must never depend on a repository path.
func titleTeeCommand(original string) string {
	base := "entire hooks antigravity title-tee"
	if original == "" {
		return base
	}
	return base + " --wrap " + shellSingleQuote(original)
}

// shellSingleQuote wraps s in POSIX single quotes. Embedded single quotes are
// rewritten with the standard close-escape-reopen technique (see the
// strings.ReplaceAll below) so the result is safe inside a single-quoted shell
// argument.
//
// Trust boundary: s is only ever the title command the user already configured
// in agy's own global settings.json — a command agy runs verbatim, as the
// user, on every state change. Quoting it here changes nothing about what it
// may do; it only guarantees that agy hands it to `entire ... --wrap` as ONE
// argument, so that title-tee later re-executes exactly the command that was
// there (via `sh -c`, the same shell agy would have used) and uninstall can
// restore it byte for byte. No content from a repository, a hook payload, or
// any other party reaches this function, and `entire` never composes a shell
// command from anything but that user-authored string.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// InstallTitleTee installs the title-tee shim into agy's global settings.json.
// If a user's own title command is already present, it is preserved via --wrap.
// The call is idempotent: if our marker is already in the command, it returns nil.
func InstallTitleTee() error {
	rawFile, err := readAgySettings()
	if err != nil {
		return err
	}

	// Parse existing title entry (if any). Unparseable → treat as absent.
	var existing titleConfig
	if raw, ok := rawFile["title"]; ok {
		_ = json.Unmarshal(raw, &existing) //nolint:errcheck // treat unparseable as absent
	}

	// Idempotency: already contains our marker.
	if strings.Contains(existing.Command, titleTeeMarker) {
		return nil
	}

	// Build new title config, wrapping any pre-existing command.
	cfg := titleConfig{
		Type:    hookTypeCommand,
		Command: titleTeeCommand(existing.Command),
	}

	cfgBytes, err := jsonutil.MarshalWithNoHTMLEscape(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal title config: %w", err)
	}
	rawFile["title"] = cfgBytes

	return writeAgySettings(rawFile)
}

// TitleTeeInstalled reports whether agy's global settings.json declares a
// title command containing the title-tee marker. It is used by `entire doctor`
// to warn when Antigravity hooks are installed in a repo but the global title
// slot — agy's only token-usage surface — has not been claimed, which would
// leave token counts missing from checkpoints. A missing or unparseable
// settings file reports false.
func TitleTeeInstalled() bool {
	rawFile, err := readAgySettings()
	if err != nil {
		return false
	}

	raw, ok := rawFile["title"]
	if !ok {
		return false
	}

	var existing titleConfig
	if err := json.Unmarshal(raw, &existing); err != nil {
		return false
	}
	return strings.Contains(existing.Command, titleTeeMarker)
}

// UninstallTitleTee removes or restores the title entry in agy's global settings.json:
//   - bare tee (no --wrap)      → delete "title" key
//   - tee with --wrap 'X'       → restore X
//   - any other (foreign) cmd   → leave untouched
//   - missing settings file     → no-op
func UninstallTitleTee() error {
	// Missing file → nothing to uninstall.
	exists, err := agySettingsExist()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}

	rawFile, err := readAgySettings()
	if err != nil {
		return err
	}

	raw, ok := rawFile["title"]
	if !ok {
		return nil // no title key — nothing to do
	}

	var existing titleConfig
	if err := json.Unmarshal(raw, &existing); err != nil {
		return nil //nolint:nilerr // unparseable title entry — leave it alone rather than destroying user data
	}

	// Not our command → leave untouched.
	if !strings.Contains(existing.Command, titleTeeMarker) {
		return nil
	}

	wrapped, hasWrap := extractWrappedCommand(existing.Command)
	if hasWrap {
		// Restore the original command.
		restored := titleConfig{
			Type:    hookTypeCommand,
			Command: wrapped,
		}
		restoredBytes, err := jsonutil.MarshalWithNoHTMLEscape(restored)
		if err != nil {
			return fmt.Errorf("failed to marshal restored title config: %w", err)
		}
		rawFile["title"] = restoredBytes
	} else {
		// Only delete the key when the command has the shape of a bare tee we
		// would have written ourselves. Anything else containing the marker is
		// a user-authored wrapper (e.g. "my-wrapper.sh 'entire hooks
		// antigravity title-tee'") or a corrupted command — leaving it alone
		// is always safer than deleting the user's config.
		if !isBareTitleTeeCommand(existing.Command) {
			return nil
		}
		delete(rawFile, "title")
	}

	return writeAgySettings(rawFile)
}

// isBareTitleTeeCommand reports whether command is one of the bare (no --wrap)
// tee commands any Entire install could have written: the production form, or
// the legacy local-dev form `go run '<repo>/cmd/entire/main.go' hooks
// antigravity title-tee` that older versions wrote (local-dev mode was removed
// in a9a676e79). The legacy form is matched by SHAPE, from ANY repo/worktree:
// uninstall may run from a different worktree (or outside a repo) than install
// did, and an exact-path comparison would silently orphan the global entry,
// leaving agy to spawn a failing `go run` on every state change after the
// original worktree is deleted.
func isBareTitleTeeCommand(command string) bool {
	if command == titleTeeCommand("") {
		return true
	}
	return strings.HasPrefix(command, "go run ") &&
		strings.HasSuffix(command, " hooks antigravity title-tee")
}

// extractWrappedCommand parses the --wrap '<original>' portion of a title-tee
// command string. It returns the original command and true if found and valid,
// or ("", false) otherwise.
func extractWrappedCommand(command string) (string, bool) {
	const wrapFlag = " --wrap "
	idx := strings.Index(command, wrapFlag)
	if idx < 0 {
		return "", false
	}
	rest := strings.TrimSpace(command[idx+len(wrapFlag):])
	if len(rest) < 2 || rest[0] != '\'' || rest[len(rest)-1] != '\'' {
		return "", false
	}
	// Strip outer single quotes and reverse the '\'' escaping.
	inner := rest[1 : len(rest)-1]
	return strings.ReplaceAll(inner, `'\''`, "'"), true
}

// agySettingsExist reports whether settings.json is present in agy's config
// dir. Lstat, not Stat: a dangling symlink still occupies the slot.
func agySettingsExist() (bool, error) {
	root, err := openAgyConfigRoot(false)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to open agy config dir: %w", err)
	}
	defer root.Close()
	if _, err := osroot.LstatNoSymlinks(root, agySettingsFileName); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat agy settings: %w", err)
	}
	return true, nil
}

// readAgySettings reads and parses settings.json into a raw map.
// A missing directory or file returns an empty map (not an error); a
// symlinked settings.json is refused rather than read through.
func readAgySettings() (map[string]json.RawMessage, error) {
	rawFile := make(map[string]json.RawMessage)
	root, err := openAgyConfigRoot(false)
	if err != nil {
		if os.IsNotExist(err) {
			return rawFile, nil
		}
		return nil, fmt.Errorf("failed to open agy config dir: %w", err)
	}
	defer root.Close()
	data, err := osroot.ReadFileNoFollow(root, agySettingsFileName)
	if os.IsNotExist(err) {
		return rawFile, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read agy settings: %w", err)
	}
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return nil, fmt.Errorf("failed to parse agy settings: %w", err)
	}
	return rawFile, nil
}

// writeAgySettings marshals rawFile and writes settings.json atomically inside
// agy's config dir, creating the dir as needed. settings.json is
// machine-global (its title slot is shared by every repo on the machine), so a
// crash mid-write must not truncate it, and a symlink at the file is refused
// rather than replaced.
func writeAgySettings(rawFile map[string]json.RawMessage) error {
	output, err := jsonutil.MarshalIndentWithNewline(rawFile, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal agy settings: %w", err)
	}
	root, err := openAgyConfigRoot(true)
	if err != nil {
		return fmt.Errorf("failed to open agy config dir: %w", err)
	}
	defer root.Close()
	if info, err := osroot.LstatNoSymlinks(root, agySettingsFileName); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("failed to write agy settings: %w", osroot.ErrSymlinkedPath)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to inspect agy settings: %w", err)
	}
	if err := jsonutil.WriteFileAtomicIn(root, agySettingsFileName, output, 0o600); err != nil {
		return fmt.Errorf("failed to write agy settings: %w", err)
	}
	return nil
}
