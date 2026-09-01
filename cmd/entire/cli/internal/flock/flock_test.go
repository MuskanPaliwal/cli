package flock

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAcquireContext_CancelWithoutDeadline(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cancel.lock")
	releaseHolder, err := Acquire(path)
	require.NoError(t, err)
	holderReleased := false
	release := func() {
		if !holderReleased {
			releaseHolder()
			holderReleased = true
		}
	}
	t.Cleanup(release)

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		acquiredRelease, acquireErr := AcquireContext(ctx, path)
		if acquiredRelease != nil {
			acquiredRelease()
		}
		result <- acquireErr
	}()
	<-started
	cancel()

	select {
	case acquireErr := <-result:
		require.ErrorIs(t, acquireErr, context.Canceled)
	case <-time.After(500 * time.Millisecond):
		release()
		<-result
		t.Fatal("AcquireContext ignored cancellation because the context had no deadline")
	}
}
