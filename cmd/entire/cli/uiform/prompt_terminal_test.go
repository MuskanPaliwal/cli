//go:build !windows

package uiform

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/creack/pty"
)

// An empty Form.View is not enough: the renderer must move back over the
// question before erasing it. Exercise the actual terminal output, because
// accessible-mode tests bypass the renderer that left answered prompts behind.
func TestConfirmationClearsPromptAfterAnswer(t *testing.T) {
	t.Parallel()
	terminal, input, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = terminal.Close() })
	t.Cleanup(func() { _ = input.Close() })
	if err := pty.Setsize(terminal, &pty.Winsize{Rows: 24, Cols: 100}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const question = "Install the entire-graph plugin?"
	output := make(chan string, 1)
	go func() {
		var transcript strings.Builder
		answered := false
		buf := make([]byte, 4096)
		for {
			n, readErr := terminal.Read(buf)
			transcript.Write(buf[:n])
			if !answered && strings.Contains(transcript.String(), question) {
				answered = true
				if _, writeErr := io.WriteString(terminal, "\r"); writeErr != nil {
					cancel()
				}
			}
			if readErr != nil {
				output <- transcript.String()
				return
			}
		}
	}()
	answer := true
	form := New(huh.NewGroup(huh.NewConfirm().Title(question).Value(&answer))).
		WithProgramOptions(tea.WithEnvironment([]string{"TERM=xterm-256color"})).
		WithAccessible(false).WithInput(input).WithOutput(input)
	err = form.RunWithContext(ctx)
	_ = input.Close() // End the reader after the final render has been flushed.
	if err != nil {
		t.Fatal(err)
	}
	transcript := <-output
	// This fixed-width, five-row form ends with the cursor on its help row.
	// Clearing from that row alone leaves the question and choices visible.
	if !strings.Contains(transcript, "\x1b[4A\x1b[J") {
		t.Fatalf("completed prompt was not erased from its first row: %q", transcript)
	}
	if !answer {
		t.Fatal("Enter did not retain the default Yes answer")
	}
}
