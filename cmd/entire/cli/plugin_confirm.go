package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/muesli/cancelreader"
)

// A separate terminal keeps confirmation from consuming the plugin's stdin.
// Tests replace the opener rather than redirecting the command's data stream.
var openPluginPromptInput = func() (io.ReadCloser, error) {
	in, out, err := tea.OpenTTY()
	if err != nil {
		return nil, fmt.Errorf("open confirmation terminal: %w", err)
	}
	if out != in {
		_ = out.Close()
	}
	return in, nil
}

func runPluginConfirm(ctx context.Context, out io.Writer, prompt string, defaultYes bool) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("confirmation cancelled: %w", err)
	}
	input, err := openPluginPromptInput()
	if err != nil {
		return false, err
	}
	closeInput := sync.OnceFunc(func() { _ = input.Close() })
	defer closeInput()
	answer := defaultYes
	form := NewAccessibleForm(huh.NewGroup(huh.NewConfirm().Title(prompt).Value(&answer))).WithOutput(out).WithInput(input)
	if IsAccessibleMode() {
		// Huh's accessible scanner ignores context and treats EOF as the default.
		// Make the read cancellable and retain EOF so it cannot authorize an install.
		reader, readErr := cancelreader.NewReader(input)
		if readErr != nil {
			return false, fmt.Errorf("confirmation input: %w", readErr)
		}
		defer reader.Close()
		cancelled := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			if !reader.Cancel() {
				// Some platforms cannot cancel reads on a separately opened
				// terminal. This descriptor belongs to the prompt, so closing
				// it is safe and also releases a blocked read.
				closeInput()
			}
			close(cancelled)
		})
		defer func() {
			if !stop() {
				<-cancelled
			}
		}()
		checked := &pluginConfirmReader{Reader: reader}
		err = form.WithInput(checked).RunWithContext(ctx)
		if ctx.Err() != nil {
			return false, fmt.Errorf("confirmation cancelled: %w", ctx.Err())
		}
		if checked.err != nil {
			if errors.Is(checked.err, io.EOF) {
				return false, nil
			}
			return false, fmt.Errorf("confirmation input: %w", checked.err)
		}
	} else {
		err = form.RunWithContext(ctx)
	}
	if ctx.Err() != nil {
		return false, fmt.Errorf("confirmation cancelled: %w", ctx.Err())
	}
	if err != nil {
		return false, fmt.Errorf("confirmation form: %w", err)
	}
	return answer, nil
}

type pluginConfirmReader struct {
	io.Reader

	err error
}

func (r *pluginConfirmReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n == 0 {
		r.err = err
	}
	return n, err //nolint:wrapcheck // preserve io.Reader EOF semantics for the scanner
}
