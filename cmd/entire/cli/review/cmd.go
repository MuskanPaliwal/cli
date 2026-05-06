// Package review — see env.go for package-level rationale.
//
// cmd.go provides NewCommand(), the cobra entry point for `entire review`.
// It routes through the new AgentReviewer / Sink / Run architecture for
// launchable agents (claude-code, codex, gemini-cli) and falls back to
// RunMarkerFallback for non-launchable agents (cursor, opencode,
// factoryai-droid, copilot-cli).
package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/charmbracelet/huh"
	git "github.com/go-git/go-git/v6"
	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	reviewtypes "github.com/entireio/cli/cmd/entire/cli/review/types"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// Deps collects the runtime-injectable hooks NewCommand needs from the
// parent cli package. Tests stub fields to drive branches that would
// otherwise require a real TTY or enabled repo. Production wiring is
// provided by buildReviewDeps in cmd/entire/cli/review_bridge.go and
// passed to NewCommand from root.go.
type Deps struct {
	// GetAgentsWithHooksInstalled returns the registry names of all agents
	// whose lifecycle hooks are installed in the current repo.
	GetAgentsWithHooksInstalled func(ctx context.Context) []types.AgentName

	// NewSilentError wraps an error so the cobra root does not double-print it.
	NewSilentError func(err error) error

	// PromptForAgentFn overrides the interactive agent picker. Nil means
	// PromptForAgent is used (the real huh form). Tests inject a stub.
	PromptForAgentFn func(ctx context.Context, eligible []AgentChoice) (string, error)

	// HeadHasReviewCheckpoint checks whether HEAD's checkpoint metadata
	// includes a review session. Returns (true, infoString) if HasReview is set.
	// Injected to avoid an import cycle: review → checkpoint → codex → review.
	HeadHasReviewCheckpoint func(ctx context.Context) (bool, string)

	// ReviewerFor maps an agent registry name to its AgentReviewer
	// implementation. Returns nil for non-launchable agents (cursor, opencode,
	// factoryai-droid, copilot-cli). Injected to break the import cycle:
	// per-agent reviewer packages import review (for ComposeReviewPrompt /
	// AppendReviewEnv), so review/cmd.go cannot import them back.
	ReviewerFor func(agentName string) reviewtypes.AgentReviewer

	// AttachCmd, when non-nil, is registered as the `review attach`
	// subcommand. Callers in the cli package pass newReviewAttachCmd() here;
	// tests pass nil to skip the subcommand.
	AttachCmd *cobra.Command
}

// runReviewDeps carries the subset of Deps that runReview itself reads
// directly (vs. NewCommand's wiring). Kept unexported so tests construct a
// Deps value at the package boundary; runReview unpacks the relevant fields.
type runReviewDeps struct {
	promptForAgentFn func(ctx context.Context, eligible []AgentChoice) (string, error)
}

// NewCommand returns the `entire review` cobra command wired with the
// provided deps. Callers in the cli package pass a fully-populated Deps;
// tests pass a Deps with stub fields.
func NewCommand(deps Deps) *cobra.Command {
	var edit bool
	var agentOverride string

	cmd := &cobra.Command{
		Use: "review",
		// Hidden from `entire help` while the feature is still maturing —
		// users who know about it can still run `entire review` / `entire
		// review --help` and the command works normally.
		Hidden: true,
		Short:  "Run configured review skills against the current branch",
		Long: `Run the review skills configured in .entire/settings.json against
the current branch. On first run, an interactive picker writes the config.

The review session is recorded as part of the next checkpoint, so the
review metadata is permanently attached to the commit it covers.

Flags:
  --edit         re-open the review config picker
  --agent NAME   select a specific configured agent when more than one is
                 configured (default: alphabetically first)

Subcommands:
  attach <id>    tag an existing session as a review (equivalent to
                 'entire attach --review <id>')`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			// Discover external agents so review configs that target them
			// resolve correctly — without this, GetAgentsWithHooksInstalled
			// and agent.Get can't see them.
			external.DiscoverAndRegister(ctx)

			if edit {
				_, err := RunReviewConfigPicker(ctx, cmd.OutOrStdout(), deps.GetAgentsWithHooksInstalled)
				return err
			}
			innerDeps := runReviewDeps{promptForAgentFn: deps.PromptForAgentFn}
			return runReview(ctx, cmd, agentOverride, deps, innerDeps)
		},
	}
	cmd.Flags().BoolVar(&edit, "edit", false, "re-open the review config picker")
	cmd.Flags().StringVar(&agentOverride, "agent", "", "select a specific configured agent (default: alphabetically first)")
	if deps.AttachCmd != nil {
		cmd.AddCommand(deps.AttachCmd)
	}
	return cmd
}

// runReview executes the main review flow.
func runReview(ctx context.Context, cmd *cobra.Command, agentOverride string, deps Deps, innerDeps runReviewDeps) error {
	out := cmd.OutOrStdout()
	silentErr := deps.NewSilentError

	// 1. Pre-flight: must be in a git repo.
	if _, err := paths.WorktreeRoot(ctx); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire enable` first.")
		return silentErr(errors.New("not a git repository"))
	}

	// 2. Load config. A load error means the settings file exists but is
	// malformed (Load returns a default-filled object when the file is
	// missing). Surface the error instead of silently opening the picker,
	// which would cause SaveReviewConfig to write over the user's other
	// settings with an empty EntireSettings{}.
	s, err := settings.Load(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(), "Failed to load settings: %v\n", err)
		fmt.Fprintln(cmd.ErrOrStderr(), "Fix `.entire/settings.json` and re-run `entire review`.")
		return silentErr(err)
	}
	if s == nil || len(s.Review) == 0 {
		if !ConfirmFirstRunSetup(ctx, out) {
			return nil
		}
		picked, pickErr := RunReviewConfigPicker(ctx, out, deps.GetAgentsWithHooksInstalled)
		if pickErr != nil {
			return pickErr
		}
		if s == nil {
			s = &settings.EntireSettings{}
		}
		s.Review = picked
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Setup complete — running review now.")
	}

	// 3. Pick agent. When --agent override is empty, base the selection on
	// the eligible set (configured AND installed) so the run always picks a
	// usable agent:
	//   - 0 eligible: fall through; SelectReviewAgent below errors with the
	//     full configured map (clearer "no installed agent" diagnostic than
	//     a silent fail).
	//   - 1 eligible: use it directly. This matters when the alphabetically-
	//     first configured agent isn't installed but exactly one other is —
	//     without this, SelectReviewAgent would default to the alphabetical
	//     first and the verify-hooks check below would error needlessly.
	//   - 2+ eligible: prompt.
	installed := deps.GetAgentsWithHooksInstalled(ctx)
	if agentOverride == "" {
		eligible := ComputeEligibleConfigured(s, installed)
		switch {
		case len(eligible) == 1:
			agentOverride = eligible[0].Name
		case len(eligible) > 1:
			fn := innerDeps.promptForAgentFn
			if fn == nil {
				fn = PromptForAgent
			}
			picked, pickErr := fn(ctx, eligible)
			if pickErr != nil {
				cmd.SilenceUsage = true
				fmt.Fprintln(cmd.ErrOrStderr(), pickErr.Error())
				return silentErr(pickErr)
			}
			if picked == "" {
				// Defensive: empty picker return must not fall through to
				// alphabetical-first default.
				cmd.SilenceUsage = true
				emptyErr := errors.New("agent picker returned empty agent name")
				fmt.Fprintln(cmd.ErrOrStderr(), emptyErr.Error())
				return silentErr(emptyErr)
			}
			agentOverride = picked
		}
	}

	agentName, cfg, err := SelectReviewAgent(s.Review, agentOverride)
	if err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}

	// 3.5. Verify hooks are installed for the selected agent.
	installedNames := make([]types.AgentName, len(installed))
	copy(installedNames, installed)
	found := false
	for _, n := range installedNames {
		if string(n) == agentName {
			found = true
			break
		}
	}
	if !found {
		cmd.SilenceUsage = true
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Hooks are not installed for %q. Run `entire configure --agent %s` first, "+
				"or remove %q from review settings.\n",
			agentName, agentName, agentName)
		return silentErr(fmt.Errorf("hooks not installed for %s", agentName))
	}

	// 3.6. Verify configured skills are actually installed on disk.
	ag, agErr := agent.Get(types.AgentName(agentName))
	if agErr != nil {
		return fmt.Errorf("resolve agent %s: %w", agentName, agErr)
	}
	if err := VerifyConfiguredSkillsInstalled(ctx, ag, cfg); err != nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), err.Error())
		return silentErr(err)
	}

	// 4. Re-run guard: check if HEAD's checkpoint already has a review.
	if reviewed, meta := deps.HeadHasReviewCheckpoint(ctx); reviewed {
		var proceed bool
		form := newAccessibleForm(huh.NewGroup(
			huh.NewConfirm().
				Title(fmt.Sprintf("Already reviewed: %s. Proceed anyway?", meta)).
				Value(&proceed),
		))
		if err := form.RunWithContext(ctx); err != nil {
			fmt.Fprintln(out, "prompt cancelled")
			return err //nolint:wrapcheck // propagate huh cancellation
		}
		if !proceed {
			fmt.Fprintln(out, "Review cancelled.")
			return nil
		}
	}

	// 5. Resolve HEAD SHA and worktree root.
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return fmt.Errorf("resolve worktree root: %w", err)
	}

	// 6. Resolve HEAD SHA and detect scope. Scope work happens BEFORE the
	// launchability branch so non-launchable agents (cursor, opencode,
	// factoryai-droid) also see the scope banner and get a scope-aware
	// prompt persisted to the marker — same context the launchable path
	// passes to Run(). Best-effort: scope detection failure prints no
	// banner and leaves ScopeBaseRef empty.
	headSHA, shaErr := currentHeadSHA(ctx, worktreeRoot)
	if shaErr != nil {
		return fmt.Errorf("resolve HEAD: %w", shaErr)
	}

	// Compute scope via the canonical scope.go path: closest non-self
	// ancestor branch by tip timestamp, fallback chain, full ScopeStats
	// (commits + files changed + uncommitted). Best-effort: scope detection
	// failure prints no banner and leaves ScopeBaseRef empty so the run
	// proceeds with the agent picking its own scope (degraded mode).
	var scopeBaseRef string
	if repo, openErr := git.PlainOpen(worktreeRoot); openErr == nil {
		if stats, statsErr := ComputeScopeStats(ctx, repo); statsErr == nil {
			scopeBaseRef = stats.BaseRef
			fmt.Fprintln(out, formatScopeBanner(stats))
		} else {
			logging.Debug(ctx, "review scope detection failed", slog.String("error", statsErr.Error()))
		}
	} else {
		logging.Debug(ctx, "review repo open failed", slog.String("error", openErr.Error()))
	}

	runCfg := reviewtypes.RunConfig{
		PromptOverride: cfg.Prompt,
		Skills:         cfg.Skills,
		ScopeBaseRef:   scopeBaseRef,
		StartingSHA:    headSHA,
	}

	// 7. Branch on launchability.
	reviewer := deps.ReviewerFor(agentName)
	if reviewer == nil {
		// Non-launchable: write marker (with scope-aware prompt) and print guidance.
		return RunMarkerFallback(ctx, agentName, runCfg, worktreeRoot, out)
	}

	_, waitErr := Run(ctx, reviewer, runCfg, []reviewtypes.Sink{DumpSink{W: out}})
	if waitErr != nil && ctx.Err() == nil {
		// Non-cancellation error: surface to caller.
		return fmt.Errorf("review run: %w", waitErr)
	}
	return nil
}

// currentHeadSHA returns the current HEAD commit hash as a 40-char hex string.
func currentHeadSHA(ctx context.Context, repoRoot string) (string, error) {
	out, err := runGit(ctx, repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}
