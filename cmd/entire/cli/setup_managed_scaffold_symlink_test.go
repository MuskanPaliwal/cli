package cli

import (
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/osroot"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A repository can ship a symlink at .claude, because the working tree arrives
// by clone. Before this was anchored, writeManagedScaffold did os.MkdirAll on
// two levels below it and os.WriteFile through it, landing the file wherever the
// link pointed and reporting Created.
func TestWriteManagedScaffold_RefusesASymlinkedAgentDirectory(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, target string }{
		{"pointing outside the worktree", ""},
		{"pointing elsewhere inside the worktree", "vendor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			worktree := t.TempDir()

			var dest string
			if tc.target == "" {
				dest = t.TempDir()
			} else {
				dest = filepath.Join(worktree, tc.target)
				require.NoError(t, os.MkdirAll(dest, 0o750))
			}
			link := filepath.Join(worktree, ".claude")
			if err := os.Symlink(dest, link); err != nil {
				t.Skipf("symlink not supported: %v", err)
			}

			rel := filepath.Join(".claude", "skills", "entire", "SKILL.md")
			st, rootErr := openScaffoldTarget(worktree, rel)
			require.NoError(t, rootErr)

			_, err := writeManagedScaffold(st, []byte("managed\n"), func([]byte) bool { return true })
			require.ErrorIs(t, err, osroot.ErrSymlinkedPath)

			entries, readErr := os.ReadDir(dest)
			require.NoError(t, readErr)
			require.Empty(t, entries, "nothing may be created through the link")
			info, lerr := os.Lstat(link)
			require.NoError(t, lerr)
			require.NotZero(t, info.Mode()&os.ModeSymlink, "the link itself must be left alone")
		})
	}
}

// The ordinary path still works, including creating the nested directories.
func TestWriteManagedScaffold_CreatesThroughARealDirectory(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	rel := filepath.Join(".claude", "skills", "entire", "SKILL.md")
	st, err := openScaffoldTarget(worktree, rel)
	require.NoError(t, err)

	res, err := writeManagedScaffold(st, []byte("managed\n"), func([]byte) bool { return true })
	require.NoError(t, err)
	require.Equal(t, managedScaffoldCreated, res.Status)

	got, err := os.ReadFile(filepath.Join(worktree, rel))
	require.NoError(t, err)
	require.Equal(t, "managed\n", string(got))

	res, err = writeManagedScaffold(st, []byte("managed\n"), func([]byte) bool { return true })
	require.NoError(t, err)
	require.Equal(t, managedScaffoldUnchanged, res.Status)

	res, err = writeManagedScaffold(st, []byte("changed\n"), func([]byte) bool { return false })
	require.NoError(t, err)
	require.Equal(t, managedScaffoldSkippedConflict, res.Status)
}

func TestWriteManagedScaffold_RefusesSymlinkedLeaf(t *testing.T) {
	t.Parallel()

	worktree := t.TempDir()
	rel := filepath.Join(".claude", "skills", "entire", "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(worktree, rel)), 0o750))
	victim := filepath.Join(worktree, "victim.md")
	require.NoError(t, os.WriteFile(victim, []byte("managed old\n"), 0o600))
	if err := os.Symlink(filepath.Join("..", "..", "..", "victim.md"), filepath.Join(worktree, rel)); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	st, rootErr := openScaffoldTarget(worktree, rel)
	require.NoError(t, rootErr)

	_, err := writeManagedScaffold(st, []byte("managed new\n"), func([]byte) bool { return true })
	require.ErrorIs(t, err, osroot.ErrSymlinkedPath)
	got, err := os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, "managed old\n", string(got))
}

// TestVouchableDirsMatchTheBuiltInAgents pins agent.vouchableDirs against the
// registry, in the one package where every built-in agent is registered.
//
// The list is pinned rather than derived because the registry is mutable at
// runtime and empty in most test binaries (see its doc comment), which costs
// the automatic coverage deriving would have given. This is that coverage put
// back: a new agent whose config directory is missing from the list fails here,
// and so does an entry left behind by an agent that was removed.
func TestVouchableDirsMatchTheBuiltInAgents(t *testing.T) {
	t.Parallel()

	want := map[string]struct{}{}
	for _, relPath := range agent.AllHookConfigRelPaths() {
		dir := path.Dir(filepath.ToSlash(relPath))
		for dir != "." && dir != "/" && dir != "" {
			want[dir] = struct{}{}
			dir = path.Dir(dir)
		}
	}
	require.NotEmpty(t, want, "no built-in agent declares a hook config path; this guard proves nothing")

	got := map[string]struct{}{}
	for _, d := range agent.VouchableSymlinkedDirs() {
		got[d] = struct{}{}
	}

	for d := range want {
		if _, ok := got[d]; !ok {
			assert.Failf(t, "missing vouchable directory",
				"%s is a built-in agent's config directory but is not in agent.vouchableDirs; "+
					"a user with it symlinked has no way to say so", d)
		}
	}
	for d := range got {
		if _, ok := want[d]; !ok {
			assert.Failf(t, "stale vouchable directory",
				"%s is in agent.vouchableDirs but no built-in agent's config lives under it; remove it", d)
		}
	}
}
