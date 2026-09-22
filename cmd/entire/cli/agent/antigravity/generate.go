package antigravity

import (
	"context"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// GenerateText submits a non-interactive prompt to the Antigravity CLI. The
// binary is `agy`; -p is the short alias for --print (single-prompt mode).
//
// The prompt travels in argv. Earlier releases accepted it on stdin behind a
// single-space -p placeholder (the Gemini CLI convention, verified on agy
// 1.0.16), but agy 1.2.7 ignores stdin in print mode: `-p " "` fails with
// "Error: empty prompt", and `-p -` is answered as the literal message "-"
// (both observed live, trail 444, 2026-09-22), which is how
// `entire dispatch --local --agent antigravity` came to hand agy an empty
// prompt. argv is the only documented route ("Usage: agy --print 'your
// prompt here'"). Summary prompts are a few tens of KB at most, well inside
// the Unix per-argument limit; Windows' 32K command-line limit is the one
// place a very long prompt could fail, and it fails loudly.
func (a *AntigravityAgent) GenerateText(ctx context.Context, prompt string, model string) (string, error) {
	args := []string{"-p", prompt}
	if model != "" {
		args = append(args, "--model", model)
	}
	result, capturedStderr, stdoutBytes, err := agent.RunIsolatedTextGeneratorCLI(ctx, a.CommandRunner, "agy", "antigravity", args, "")
	if err != nil {
		return "", &agent.TextGenerationError{
			Err:         fmt.Errorf("antigravity text generation failed: %w", err),
			Stderr:      capturedStderr,
			StdoutBytes: stdoutBytes,
		}
	}
	return result, nil
}
