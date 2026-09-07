//go:build unix

package cli

import (
	"path/filepath"
	"syscall"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/worktreedir"
)

// TestScanForSymlinkedComponent_NonTraversableComponent pins the allowlist. An
// earlier revision tested only for a regular file, so a FIFO, socket or device
// node where a directory belongs came back clean and doctor printed nothing —
// while os.Root and every hook install fail on it.
//
// Unix-only by build constraint rather than by a runtime t.Skip, because
// syscall.Mkfifo does not exist on Windows at all: a runtime guard still has to
// compile, and this file did not.
func TestScanForSymlinkedComponent_NonTraversableComponent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, claudeDirName), 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	root, err := worktreedir.OpenAt(dir)
	if err != nil {
		t.Fatal(err)
	}

	name, outcome := scanForSymlinkedComponent(root, claudeDirName+"/settings.json")
	if outcome != componentScanWrongType {
		t.Errorf("outcome = %v, want componentScanWrongType for a FIFO", outcome)
	}
	if name != claudeDirName {
		t.Errorf("name = %q, want %s", name, claudeDirName)
	}
}
