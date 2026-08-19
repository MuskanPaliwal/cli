package jsonutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteFileAtomicStream_ReplacesValidatedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	validated := false
	err := WriteFileAtomicStream(
		context.Background(),
		target,
		0o600,
		func(w io.Writer) error {
			_, err := io.WriteString(w, `{"fresh":true}`)
			return err
		},
		func(r io.Reader) error {
			data, err := io.ReadAll(r)
			if err == nil && string(data) != `{"fresh":true}` {
				err = fmt.Errorf("validate got %q", data)
			}
			validated = err == nil
			return err
		},
	)
	if err != nil {
		t.Fatalf("WriteFileAtomicStream: %v", err)
	}
	if !validated {
		t.Fatal("validator was not called")
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"fresh":true}` {
		t.Fatalf("destination = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("permission = %#o, want %#o", got, 0o600)
		}
	}
	assertNoAtomicTemps(t, dir)
}

func TestWriteFileAtomicStream_ProducerFailureCleansStaging(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := seedAtomicDestination(t, dir)
	wantErr := errors.New("producer failed")
	validated := false

	err := WriteFileAtomicStream(
		context.Background(), target, 0o600,
		func(w io.Writer) error {
			if _, err := io.WriteString(w, `{"partial":`); err != nil {
				return err
			}
			return wantErr
		},
		func(io.Reader) error {
			validated = true
			return nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want unchanged producer error", err)
	}
	if validated {
		t.Fatal("validator ran after producer failure")
	}
	assertAtomicDestinationUnchanged(t, target)
	assertNoAtomicTemps(t, dir)
}

func TestWriteFileAtomicStream_ValidatorFailureCleansStaging(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := seedAtomicDestination(t, dir)
	wantErr := errors.New("validator failed")

	err := WriteFileAtomicStream(
		context.Background(), target, 0o600,
		func(w io.Writer) error {
			_, err := io.WriteString(w, `{"fresh":true}`)
			return err
		},
		func(io.Reader) error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want unchanged validator error", err)
	}
	assertAtomicDestinationUnchanged(t, target)
	assertNoAtomicTemps(t, dir)
}

func TestWriteFileAtomicStream_CancellationBeforePublicationCleansStaging(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := seedAtomicDestination(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	validated := false

	err := WriteFileAtomicStream(
		ctx, target, 0o600,
		func(w io.Writer) error {
			_, err := io.WriteString(w, `{"fresh":true}`)
			cancel()
			return err
		},
		func(io.Reader) error {
			validated = true
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if validated {
		t.Fatal("validator ran after cancellation")
	}
	assertAtomicDestinationUnchanged(t, target)
	assertNoAtomicTemps(t, dir)
}

func TestWriteFileAtomicStream_RejectsIncompleteJSONBeforePublication(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
	}{
		{name: "empty"},
		{name: "truncated", data: `{"messages":[1,2`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			target := seedAtomicDestination(t, dir)

			err := WriteFileAtomicStream(
				context.Background(), target, 0o600,
				func(w io.Writer) error {
					_, err := io.WriteString(w, tt.data)
					return err
				},
				func(r io.Reader) error {
					data, err := io.ReadAll(r)
					if err != nil {
						return err
					}
					if !json.Valid(data) {
						return errors.New("invalid JSON")
					}
					return nil
				},
			)
			if err == nil {
				t.Fatal("expected validation error")
			}
			assertAtomicDestinationUnchanged(t, target)
			assertNoAtomicTemps(t, dir)
		})
	}
}

func TestWriteFileAtomicStream_SyncAndCloseFailuresCleanStaging(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		syncErr  error
		closeErr error
	}{
		{name: "sync", syncErr: errors.New("sync failed")},
		{name: "close", closeErr: errors.New("close failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			target := seedAtomicDestination(t, dir)
			validated := false
			ops := defaultAtomicWriteOps()
			ops.createTemp = func(dir, pattern string) (syncWriteCloser, error) {
				file, err := os.CreateTemp(dir, pattern)
				if err != nil {
					return nil, err
				}
				return &faultWriteFile{File: file, syncErr: tt.syncErr, closeErr: tt.closeErr}, nil
			}

			err := writeFileAtomic(
				context.Background(), target, 0o600,
				func(w io.Writer) error {
					_, err := io.WriteString(w, "fresh")
					return err
				},
				func(io.Reader) error {
					validated = true
					return nil
				},
				atomicWriteConfig{ops: ops, tempPattern: ".out.*.tmp"},
			)
			wantErr := tt.syncErr
			if wantErr == nil {
				wantErr = tt.closeErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want %v", err, wantErr)
			}
			if validated {
				t.Fatal("validator ran before staging sync and close succeeded")
			}
			assertAtomicDestinationUnchanged(t, target)
			assertNoAtomicTemps(t, dir)
		})
	}
}

func TestWriteFileAtomicStream_RenameFailureRetainsValidatedStaging(t *testing.T) {
	t.Parallel()
	const fresh = "fresh"
	dir := t.TempDir()
	target := seedAtomicDestination(t, dir)
	wantErr := errors.New("rename failed")
	ops := defaultAtomicWriteOps()
	ops.rename = func(string, string) error { return wantErr }

	err := writeFileAtomic(
		context.Background(), target, 0o600,
		func(w io.Writer) error {
			_, err := io.WriteString(w, fresh)
			return err
		},
		func(r io.Reader) error {
			data, err := io.ReadAll(r)
			if err == nil && string(data) != fresh {
				err = fmt.Errorf("validate got %q", data)
			}
			return err
		},
		atomicWriteConfig{
			ops:                          ops,
			tempPattern:                  ".out.*.tmp",
			retainStagedOnPublishFailure: true,
		},
	)
	var publishErr *PublishError
	if !errors.As(err, &publishErr) {
		t.Fatalf("error = %v, want *PublishError", err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error does not retain rename cause: %v", err)
	}
	assertAtomicDestinationUnchanged(t, target)
	staged, readErr := os.ReadFile(publishErr.StagedPath)
	if readErr != nil {
		t.Fatalf("read retained staging: %v", readErr)
	}
	if string(staged) != fresh {
		t.Fatalf("retained staging = %q, want fresh", staged)
	}
}

func TestWriteFileAtomicStream_DirectorySyncIsBestEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		openErr  error
		syncErr  error
		closeErr error
	}{
		{name: "open", openErr: errors.New("open directory failed")},
		{name: "sync", syncErr: errors.New("sync directory failed")},
		{name: "close", closeErr: errors.New("close directory failed")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			target := seedAtomicDestination(t, dir)
			directory := &faultSyncCloser{syncErr: tt.syncErr, closeErr: tt.closeErr}
			ops := defaultAtomicWriteOps()
			ops.openDir = func(path string) (syncCloser, error) {
				if path != dir {
					t.Fatalf("directory sync path = %q, want %q", path, dir)
				}
				if tt.openErr != nil {
					return nil, tt.openErr
				}
				return directory, nil
			}

			err := writeFileAtomic(
				context.Background(), target, 0o600,
				func(w io.Writer) error {
					_, err := io.WriteString(w, "fresh")
					return err
				},
				nil,
				atomicWriteConfig{ops: ops, tempPattern: ".out.*.tmp"},
			)
			if err != nil {
				t.Fatalf("writeFileAtomic returned directory sync error: %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "fresh" {
				t.Fatalf("destination = %q, want fresh", got)
			}
			if tt.openErr == nil && (!directory.syncCalled || !directory.closeCalled) {
				t.Fatalf("directory sync calls: sync=%v close=%v", directory.syncCalled, directory.closeCalled)
			}
		})
	}
}

type faultWriteFile struct {
	*os.File

	syncErr  error
	closeErr error
}

type faultSyncCloser struct {
	syncErr     error
	closeErr    error
	syncCalled  bool
	closeCalled bool
}

func (f *faultSyncCloser) Sync() error {
	f.syncCalled = true
	return f.syncErr
}

func (f *faultSyncCloser) Close() error {
	f.closeCalled = true
	return f.closeErr
}

func (f *faultWriteFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *faultWriteFile) Close() error {
	err := f.File.Close()
	if f.closeErr != nil {
		return f.closeErr
	}
	return err
}

func seedAtomicDestination(t *testing.T, dir string) string {
	t.Helper()
	target := filepath.Join(dir, "out.json")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertAtomicDestinationUnchanged(t *testing.T, target string) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("destination = %q, want old", got)
	}
}

func assertNoAtomicTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("staging file left behind: %s", entry.Name())
		}
	}
}

func TestWriteFileAtomic_CreatesNewFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	data := []byte(`{"hello":"world"}`)

	if err := WriteFileAtomic(target, data, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("content mismatch: got %q want %q", got, data)
	}
}

func TestWriteFileAtomic_ReplacesExistingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	if err := os.WriteFile(target, []byte("old contents"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	newData := []byte("new contents")
	if err := WriteFileAtomic(target, newData, 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(newData) {
		t.Errorf("content not replaced: got %q want %q", got, newData)
	}
}

// AppliesPermission verifies the Chmod-before-rename step actually lands the
// requested mode on the final file. os.CreateTemp defaults to 0o600 so
// without the Chmod a 0o644 caller would silently get a tighter mode.
func TestWriteFileAtomic_AppliesPermission(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	if err := WriteFileAtomic(target, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("perm: got %#o want %#o", got, 0o600)
	}
}

// LeavesNoTempOnSuccess guards against the removeTmp defer being skipped or
// the temp suffix changing in a way that breaks cleanup.
func TestWriteFileAtomic_LeavesNoTempOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")

	if err := WriteFileAtomic(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected exactly one entry in dir, got %d: %v", len(entries), names)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// CleansUpTempOnRenameFailure reaches the rename step and forces it to fail
// (renaming a regular file onto a non-empty directory is rejected on every
// POSIX filesystem, and on Windows). The removeTmp defer must clear the
// orphan so /tmp doesn't accumulate junk across many failed writes.
func TestWriteFileAtomic_CleansUpTempOnRenameFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "out.json")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "occupant"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	err := WriteFileAtomic(target, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error when target is a non-empty directory")
	}
	var publishErr *PublishError
	if errors.As(err, &publishErr) {
		t.Fatalf("WriteFileAtomic retained staging through PublishError: %s", publishErr.StagedPath)
	}

	info, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatalf("stat target: %v", statErr)
	}
	if !info.IsDir() {
		t.Error("target should still be a directory after failed rename")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file after failed rename: %s", e.Name())
		}
	}
}

func TestWriteFileAtomic_ParentMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "does-not-exist", "out.json")

	err := WriteFileAtomic(target, []byte("x"), 0o644)
	if err == nil {
		t.Fatal("expected error when parent dir is missing")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist; got: %v", err)
	}
}
