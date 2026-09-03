package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// syncBuffer is an io.Writer the test goroutine can poll while another
// goroutine writes to it. bytes.Buffer alone is not safe for that -- the race
// detector flags the concurrent grow/String, and a torn read would make the
// poll below flaky rather than failing honestly.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// TestClearSessionStateWithProgress_AnnouncesLockWait covers the UX half of
// gating doctor's clear: the wait is correct (deleting the state file out from
// under an in-flight write destroys it) but unbounded, and a condensation can
// hold the lock ~30s, so a silent wait reads as a hang. This drives a real
// held lock -- a writer parked inside MutateSessionState -- and asserts the
// notice is printed and the clear still completes once the lock frees.
func TestClearSessionStateWithProgress_AnnouncesLockWait(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	const sessionID = "doctor-lock-notice"
	if err := strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	writerHolding := make(chan struct{})
	writerMayFinish := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		if err := strategy.MutateSessionState(ctx, sessionID, func(*strategy.SessionState) error {
			close(writerHolding)
			<-writerMayFinish
			return nil
		}); err != nil {
			t.Errorf("MutateSessionState: %v", err)
		}
	}()
	<-writerHolding // the lock is genuinely held now

	var errBuf syncBuffer
	cleared := make(chan error, 1)
	go func() {
		cleared <- clearSessionStateWithProgress(ctx, sessionID, &errBuf, 20*time.Millisecond)
	}()

	// The notice must appear while the clear is still blocked, which is the
	// whole point -- it is useless if it only prints after the wait ends.
	deadline := time.After(2 * time.Second)
	for !strings.Contains(errBuf.String(), "release its state lock") {
		select {
		case <-deadline:
			t.Fatalf("no lock-wait notice while blocked; stderr was %q", errBuf.String())
		case err := <-cleared:
			t.Fatalf("clear returned (%v) before the writer released; stderr %q", err, errBuf.String())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !strings.Contains(errBuf.String(), sessionID) {
		t.Errorf("notice should name the session, got %q", errBuf.String())
	}

	close(writerMayFinish)
	<-writerDone
	if err := <-cleared; err != nil {
		t.Fatalf("clear failed after the lock freed: %v", err)
	}
}

// The uncontended case must stay silent: an unconditional notice would fire on
// every doctor run and train the user to ignore it.
func TestClearSessionStateWithProgress_SilentWhenUncontended(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	t.Chdir(dir)

	ctx := context.Background()
	const sessionID = "doctor-lock-quiet"
	if err := strategy.SaveSessionState(ctx, &strategy.SessionState{
		SessionID:  sessionID,
		BaseCommit: "abc123",
		StartedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("SaveSessionState: %v", err)
	}

	var errBuf syncBuffer
	if err := clearSessionStateWithProgress(ctx, sessionID, &errBuf, time.Second); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if errBuf.Len() != 0 {
		t.Errorf("expected no output when the lock is free, got %q", errBuf.String())
	}
}
