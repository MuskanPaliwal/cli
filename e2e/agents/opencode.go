package agents

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

type openCodeAgent struct {
	model   string
	timeout time.Duration
}

func init() {
	if env := os.Getenv("E2E_AGENT"); env != "" && env != "opencode" {
		return
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		return
	}
	model := os.Getenv("E2E_OPENCODE_MODEL")
	if model == "" {
		model = "anthropic/claude-haiku-4-5"
	}
	Register(&openCodeAgent{model: model, timeout: 2 * time.Minute})
}

func (a *openCodeAgent) Name() string               { return "opencode" }
func (a *openCodeAgent) Binary() string             { return "opencode" }
func (a *openCodeAgent) EntireAgent() string        { return "opencode" }
func (a *openCodeAgent) PromptPattern() string      { return `(Ask anything|▣)` }
func (a *openCodeAgent) TimeoutMultiplier() float64 { return 2.0 }

func (a *openCodeAgent) IsTransientError(out Output, _ error) bool {
	transientPatterns := []string{
		"overloaded",
		"rate limit",
		"529",
		"503",
		"ECONNRESET",
		"ETIMEDOUT",
		"Token refresh failed",
		"database is locked",
	}
	for _, p := range transientPatterns {
		if strings.Contains(out.Stderr, p) {
			return true
		}
	}
	return false
}

// openCodeWarmupBudget bounds a single warmup attempt. It sits far above the
// cost of the trivial model round-trip on purpose: the warmup's job is to pay
// opencode's first-run costs (per-directory dependency install, DB migration)
// once and serially, before ~40 tests start in parallel, so a budget that kills
// the attempt part-way leaves that work half-done for every test that follows.
// It was 30s until 2026-09-04, when opencode's startup latency stepped from
// ~10s to over 30s and the warmup began being killed on every CI run.
const openCodeWarmupBudget = 90 * time.Second

func (a *openCodeAgent) Bootstrap() error {
	// opencode has first-run DB migration + node_modules resolution that
	// races with parallel test execution (upstream issue #6935).
	// Run a trivial prompt to force full initialization before tests.
	//
	// Each attempt's duration is reported whether it succeeds or not: this is
	// the only serial, uncontended measurement of opencode startup we take, so
	// it is the cheapest place to see a step change in it from the CI log.
	for i := range 3 {
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), openCodeWarmupBudget)
		cmd := exec.CommandContext(ctx, a.Binary(), "run", "--model", a.model, "say hi")
		cmd.Env = os.Environ()
		out, err := cmd.CombinedOutput()
		cancel()
		elapsed := time.Since(start).Round(time.Millisecond)
		if err == nil {
			fmt.Fprintf(os.Stderr, "opencode warmup succeeded on attempt %d in %s\n", i+1, elapsed)
			return nil
		}
		if i < 2 {
			fmt.Fprintf(os.Stderr, "opencode warmup attempt %d failed after %s: %s\n%s\n", i+1, elapsed, err, out)
			time.Sleep(5 * time.Second)
		}
	}
	// Non-fatal: warmup failure shouldn't block tests entirely.
	fmt.Fprintln(os.Stderr, "opencode warmup failed after 3 attempts, proceeding anyway")
	return nil
}

func (a *openCodeAgent) RunPrompt(ctx context.Context, dir string, prompt string, opts ...Option) (Output, error) {
	cfg := &runConfig{}
	for _, o := range opts {
		o(cfg)
	}

	model := a.model
	if cfg.Model != "" {
		model = cfg.Model
	}

	args := []string{"run"}
	if model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, prompt)

	timeout := a.timeout
	if envTimeout := os.Getenv("E2E_TIMEOUT"); envTimeout != "" {
		if parsed, err := time.ParseDuration(envTimeout); err == nil {
			timeout = parsed
		}
	}
	// Per-prompt timeout is the most specific override.
	if cfg.PromptTimeout > 0 {
		timeout = cfg.PromptTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.Binary(), args...)
	cmd.Dir = dir
	cmd.Env = openCodePromptEnv(os.Environ(), dir)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := Output{
		Command: a.Binary() + " " + strings.Join(args, " "),
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			out.ExitCode = exitErr.ExitCode()
		} else {
			out.ExitCode = -1
		}
		return out, err
	}

	return out, nil
}

// openCodePromptEnv builds the child environment for a headless `opencode run`.
// cmd.Dir chdirs the child but does NOT update the inherited PWD env var, which
// still points at the `go test` package dir. opencode (Node) resolves its
// project/worktree root from process.env.PWD, so without forcing PWD to match
// cmd.Dir all file operations land in the wrong repo and the per-repo entire
// plugin never loads. The tmux/interactive path is unaffected because
// `tmux new-session -c dir` already sets PWD correctly.
func openCodePromptEnv(base []string, dir string) []string {
	return append(filterEnv(base, "ENTIRE_TEST_TTY", "PWD"), "PWD="+dir)
}

func (a *openCodeAgent) StartSession(ctx context.Context, dir string) (Session, error) {
	// opencode's TUI occasionally fails to render on CI (empty pane).
	// Retry once if the first attempt produces no output at all.
	var s *TmuxSession
	var lastErr error
	for attempt := range 2 {
		name := fmt.Sprintf("opencode-test-%d", time.Now().UnixNano())
		var err error
		s, err = NewTmuxSession(name, dir, []string{"ENTIRE_TEST_TTY"}, a.Binary(), "--model", a.model)
		if err != nil {
			return nil, err
		}

		// Wait for TUI to be ready (input area with placeholder text).
		// OpenCode's TUI has a large ASCII banner and multiple panels that
		// can take a while to render on CI, plus WaitFor needs 2s settle.
		if _, err := s.WaitFor(`Ask anything`, 60*time.Second); err != nil {
			content := s.Capture()
			_ = s.Close()
			if strings.TrimSpace(content) == "" && attempt == 0 {
				lastErr = err
				continue
			}
			return nil, fmt.Errorf("waiting for startup: %w", err)
		}
		s.stableAtSend = ""
		return s, nil
	}
	return nil, fmt.Errorf("opencode TUI failed to start after retry: %w", lastErr)
}
