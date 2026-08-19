package jsonutil

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	renameAttempts = 5
	renameBackoff  = 40 * time.Millisecond
)

// PublishError reports that a complete, validated staging file could not be
// installed at its destination. Ownership of StagedPath transfers to the
// caller, which must recover or remove it.
type PublishError struct {
	StagedPath  string
	destination string
	err         error
}

func (e *PublishError) Error() string {
	return fmt.Sprintf("rename temp to %s: %v", e.destination, e.err)
}

func (e *PublishError) Unwrap() error { return e.err }

// WriteFileAtomicStream atomically replaces filePath with bytes written by
// produce after validate accepts the completed staging file. The helper owns
// the staging file's full lifecycle: create, produce, sync, close, validate,
// chmod, rename, and best-effort parent-directory fsync.
//
// The writer and reader belong to the helper and are valid only for the
// duration of their callbacks. Each callback is invoked at most once.
//
// Producer and validator errors are returned unchanged. Failures before the
// rename leave the destination untouched and make a best-effort attempt to
// remove the staging file. A rename failure instead returns *PublishError and
// transfers ownership of its already-validated StagedPath to the caller.
func WriteFileAtomicStream(
	ctx context.Context,
	filePath string,
	perm fs.FileMode,
	produce func(io.Writer) error,
	validate func(io.Reader) error,
) error {
	if produce == nil {
		return fmt.Errorf("produce callback for %s is nil", filePath)
	}
	if validate == nil {
		return fmt.Errorf("validate callback for %s is nil", filePath)
	}

	return writeFileAtomic(ctx, filePath, perm, produce, validate, atomicWriteConfig{
		ops:                          defaultAtomicWriteOps(),
		tempPattern:                  "." + filepath.Base(filePath) + ".*.tmp",
		retainStagedOnPublishFailure: true,
	})
}

// WriteFileAtomic writes data to filePath atomically by writing to a temp file
// in the same directory, fsyncing it, renaming into place, and fsyncing the
// parent directory. A crash or signal mid-write leaves the original file
// intact rather than a truncated partial — important for config files like
// .entire/settings.json that callers expect to remain parseable across
// interrupted writes.
//
// The fsync between Write and Close guarantees the temp file's bytes are on
// disk before the rename takes effect; without it, some filesystems (notably
// ext4 with non-default mount options) can surface the rename as completed
// while the file is still empty after a hard crash.
//
// The parent-directory fsync after rename guarantees the rename's directory
// entry is durable. Without it, the file contents are on disk but the
// directory may still point to the pre-rename state after a crash, so the
// "leaves the original intact" promise would silently break. Windows does
// not support directory fsync; we make this step best-effort so the call
// does not fail on platforms where the operation is a no-op.
//
// perm is applied to the temp file via Chmod before rename so the final file
// lands with the requested permission regardless of the temp file's default.
func WriteFileAtomic(filePath string, data []byte, perm fs.FileMode) error {
	produce := func(w io.Writer) error {
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("write temp for %s: %w", filePath, err)
		}
		return nil
	}

	return writeFileAtomic(context.Background(), filePath, perm, produce, nil, atomicWriteConfig{
		ops:         defaultAtomicWriteOps(),
		tempPattern: filepath.Base(filePath) + ".*.tmp",
	})
}

type syncWriteCloser interface {
	io.Writer
	Sync() error
	Close() error
	Name() string
}

type syncCloser interface {
	Sync() error
	Close() error
}

type atomicWriteOps struct {
	createTemp         func(dir, pattern string) (syncWriteCloser, error)
	open               func(path string) (io.ReadCloser, error)
	chmod              func(path string, mode fs.FileMode) error
	rename             func(oldPath, newPath string) error
	remove             func(path string) error
	openDir            func(path string) (syncCloser, error)
	isRenameContention func(error) bool
	wait               func(context.Context, time.Duration) error
}

type atomicWriteConfig struct {
	ops                          atomicWriteOps
	tempPattern                  string
	retainStagedOnPublishFailure bool
}

func defaultAtomicWriteOps() atomicWriteOps {
	return atomicWriteOps{
		createTemp: func(dir, pattern string) (syncWriteCloser, error) {
			return os.CreateTemp(dir, pattern)
		},
		open: func(path string) (io.ReadCloser, error) {
			return os.Open(path) //nolint:gosec // the helper opens its own sibling staging path
		},
		chmod:              os.Chmod,
		rename:             os.Rename,
		remove:             os.Remove,
		openDir:            openSyncDir,
		isRenameContention: isRenameContention,
		wait:               waitForRenameRetry,
	}
}

func openSyncDir(path string) (syncCloser, error) {
	dir, err := os.Open(path) //nolint:gosec // path is filepath.Dir of the caller-supplied destination
	if err != nil {
		return nil, fmt.Errorf("open directory for sync: %w", err)
	}
	return dir, nil
}

func waitForRenameRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("wait to retry rename: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func writeFileAtomic(
	ctx context.Context,
	filePath string,
	perm fs.FileMode,
	produce func(io.Writer) error,
	validate func(io.Reader) error,
	config atomicWriteConfig,
) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("atomic write for %s canceled: %w", filePath, err)
	}

	dir := filepath.Dir(filePath)
	tmp, err := config.ops.createTemp(dir, config.tempPattern)
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", filePath, err)
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = config.ops.remove(tmpName) //nolint:errcheck // cleanup cannot replace the operation's primary error
		}
	}()

	if err := produce(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("atomic write for %s canceled after produce: %w", filePath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp for %s: %w", filePath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", filePath, err)
	}

	if validate != nil {
		reader, err := config.ops.open(tmpName)
		if err != nil {
			return fmt.Errorf("open temp for validation of %s: %w", filePath, err)
		}
		validateErr := validate(reader)
		closeErr := reader.Close()
		if validateErr != nil {
			return validateErr
		}
		if closeErr != nil {
			return fmt.Errorf("close temp after validation of %s: %w", filePath, closeErr)
		}
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("atomic write for %s canceled after validation: %w", filePath, err)
	}
	if err := config.ops.chmod(tmpName, perm); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", filePath, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("atomic write for %s canceled before publish: %w", filePath, err)
	}

	if err := renameWithRetry(ctx, tmpName, filePath, config.ops); err != nil {
		if config.retainStagedOnPublishFailure {
			removeTmp = false
			return &PublishError{StagedPath: tmpName, destination: filePath, err: err}
		}
		return fmt.Errorf("rename temp to %s: %w", filePath, err)
	}
	removeTmp = false
	syncDirBestEffort(dir, config.ops)
	return nil
}

func renameWithRetry(ctx context.Context, staged, destination string, ops atomicWriteOps) error {
	var err error
	for attempt := range renameAttempts {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("rename staging file canceled: %w", ctxErr)
		}
		if err = ops.rename(staged, destination); err == nil {
			return nil
		}
		if !ops.isRenameContention(err) {
			return err
		}
		if attempt == renameAttempts-1 {
			break
		}
		if waitErr := ops.wait(ctx, renameBackoff); waitErr != nil {
			return waitErr
		}
	}
	return fmt.Errorf("%w (another process may be reading %s)", err, filepath.Base(destination))
}

func syncDirBestEffort(dir string, ops atomicWriteOps) {
	d, err := ops.openDir(dir)
	if err != nil {
		return
	}
	_ = d.Sync() //nolint:errcheck // failure does not roll back an already-successful rename
	_ = d.Close()
}
