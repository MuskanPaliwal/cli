package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPluginDependencyConfirmationUsesWriter(t *testing.T) { //nolint:paralleltest // isolates terminal opener and accessibility
	t.Setenv("ACCESSIBLE", "1")
	t.Setenv("ENTIRE_TEST_TTY", "1")
	original := openPluginPromptInput
	openPluginPromptInput = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("y\n")), nil }
	t.Cleanup(func() { openPluginPromptInput = original })
	var stderr bytes.Buffer
	ok, err := confirmPluginAction(t.Context(), &stderr, "Install them now?", false)
	if err != nil || !ok {
		t.Fatalf("answer=%v err=%v", ok, err)
	}
	if !strings.Contains(stderr.String(), "Install them now? [y/N]") {
		t.Fatalf("missing prompt on stderr: %q", stderr.String())
	}
}

func TestPluginAccessibleConfirmationCancellation(t *testing.T) { //nolint:paralleltest // isolates terminal opener and accessibility
	t.Setenv("ACCESSIBLE", "1")
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	original := openPluginPromptInput
	openPluginPromptInput = func() (io.ReadCloser, error) { return input, nil }
	t.Cleanup(func() { openPluginPromptInput = original })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{}, 1)
	result := make(chan error, 1)
	go func() {
		_, promptErr := runPluginConfirm(ctx, pluginPromptNotifyWriter{ready}, "Install?", true)
		result <- promptErr
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want cancellation", err)
		}
	case <-time.After(5 * time.Second):
		_ = writer.Close()
		<-result
		t.Fatal("accessible prompt did not stop on cancellation")
	}
}

type pluginPromptNotifyWriter struct{ ready chan<- struct{} }

func (w pluginPromptNotifyWriter) Write(p []byte) (int, error) {
	select {
	case w.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}
