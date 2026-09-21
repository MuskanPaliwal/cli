package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/gitremote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/internal/coreapi"
)

// defaultMirrorRemote is the remote `remote add` reads the repo's identity from
// when the name being written does not exist yet — which, for an add, is the
// usual case. It is git's own default for fetch and push, so it is the remote
// that names the repo the user is standing in.
const defaultMirrorRemote = "origin"

// defaultMirrorUpstreamRemote is where a replaced URL is preserved, so
// repointing origin is never a lossy operation — the forge stays reachable under
// the name git's own fork workflow uses for it.
const defaultMirrorUpstreamRemote = "upstream"

// gitRemoteNameRe is the remote-name charset `remote add` accepts. Git itself is
// laxer, but these names are written into `.git/config` section headers and
// passed as argv to `git remote`, so the value is pinned to a conservative
// shape: it must start alphanumeric (so it can never be read as a flag) and
// carries no path or glob metacharacters.
var gitRemoteNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validateGitRemoteName rejects names git would refuse (or that would land
// somewhere unintended in .git/config) before they reach `git remote`.
func validateGitRemoteName(name string) error {
	if name == "" {
		return errors.New("remote name cannot be empty")
	}
	if !gitRemoteNameRe.MatchString(name) {
		return fmt.Errorf("%q is not a valid remote name (letters, digits, and . _ - / after a leading alphanumeric)", name)
	}
	// ".." would escape the intended config path; a ".lock" suffix collides with
	// git's own lockfile naming.
	if strings.Contains(name, "..") || strings.HasSuffix(name, ".lock") {
		return fmt.Errorf("%q is not a valid remote name", name)
	}
	return nil
}

// redactGitArgs returns args with anything that could carry credentials replaced
// by its redacted form, so the argv echoed in an error message is safe to print.
// A replaced remote URL can embed a token (https://user:token@host/...), and
// these errors reach stderr through main.go and from there into logs and pasted
// transcripts — the same reason reportMirrorRemotePlan redacts what it prints.
//
// Non-URL args (bare words like "remote", local paths) pass through untouched;
// see gitremote.RedactURLOrPath for why RedactURL cannot be applied blanket-fashion.
func redactGitArgs(args []string) []string {
	safe := make([]string, len(args))
	for i, a := range args {
		safe[i] = gitremote.RedactURLOrPath(a)
	}
	return safe
}

// gitRunner runs a git subcommand in dir. A package var so tests exercise the
// planning and prompt logic without mutating a real repository's config.
var gitRunner = func(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(redactGitArgs(args), " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// listGitRemotes returns the names of every configured remote in dir.
func listGitRemotes(ctx context.Context, dir string) (map[string]bool, error) {
	out, err := gitRunner(ctx, dir, "remote")
	if err != nil {
		return nil, fmt.Errorf("list git remotes: %w", err)
	}
	remotes := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			remotes[name] = true
		}
	}
	return remotes, nil
}

// mirrorRemotePlan is the resolved set of git-config writes `remote add` will
// perform. It is computed in full before anything is written so the command can
// echo exactly what it is about to do (and so the planning is unit-testable
// without touching a repo).
type mirrorRemotePlan struct {
	// remote is the remote that ends up pointing at mirrorURL.
	remote string
	// mirrorURL is the entire:// clone URL being adopted.
	mirrorURL string
	// add is true when remote does not exist yet (`git remote add` rather than
	// `git remote set-url`).
	add bool
	// replacedURL is the URL remote currently holds, when it is being
	// repointed. Empty when add is true.
	replacedURL string
	// preserveAs, when non-empty, is a new remote that will be created holding
	// replacedURL so the previous URL stays reachable.
	preserveAs string
	// preserveSkipped names the remote replacedURL would have been kept under,
	// when preservation was asked for but could not be done (the name is already
	// taken). Mutually exclusive with preserveAs, and empty when preservation was
	// never requested (`--upstream ''`). Set so the report can say out loud that
	// the previous URL did not make it into git config — the difference matters:
	// this is the one path where a successful-looking run drops the old URL.
	preserveSkipped string
	// noop is true when remote already points at mirrorURL.
	noop bool
}

// planMirrorRemote resolves what to write for a `remote add` invocation.
// remotes is the set of already-configured remote names and currentURL the
// URL of the target remote ("" when it does not exist).
//
// An occupied name is refused unless override is set, matching what `git remote
// add` itself does with one: an add that silently repoints an existing remote is
// not an add. A remote that already carries this exact URL is not a collision —
// it is the requested end state — so it reports as a no-op and a re-run stays
// safe.
//
// upstream is the requested preserve-under name; it is honored only when the
// target remote is actually being repointed and the name is free. An occupied
// name is never clobbered — silently rewriting an existing `upstream` would be
// the one genuinely destructive thing this command could do — but it is recorded
// in preserveSkipped rather than dropped quietly, because a fork checkout
// (`origin` + `upstream` both already configured) hits that path by default and
// would otherwise see a clean ✓ while the replaced URL left git config for good.
func planMirrorRemote(remote, mirrorURL, currentURL, upstream string, override bool, remotes map[string]bool) (mirrorRemotePlan, error) {
	plan := mirrorRemotePlan{remote: remote, mirrorURL: mirrorURL}
	if !remotes[remote] {
		plan.add = true
		return plan, nil
	}
	if strings.EqualFold(strings.TrimSpace(currentURL), mirrorURL) {
		plan.noop = true
		return plan, nil
	}
	if !override {
		return mirrorRemotePlan{}, fmt.Errorf("remote %q already exists and points at %s; pass --override to repoint it, or name a remote that does not exist yet",
			remote, gitremote.RedactURL(currentURL))
	}
	plan.replacedURL = currentURL
	if upstream != "" {
		// `remote` is known to exist in this branch, so an upstream naming it is
		// "occupied" too and lands in the skipped case — no separate check needed.
		if remotes[upstream] {
			plan.preserveSkipped = upstream
		} else {
			plan.preserveAs = upstream
		}
	}
	return plan, nil
}

// applyMirrorRemotePlan performs the plan's git-config writes. The preserve step
// runs first so a failure there aborts before the original URL is overwritten.
func applyMirrorRemotePlan(ctx context.Context, dir string, plan mirrorRemotePlan) error {
	if plan.noop {
		return nil
	}
	// Deferred, not tail-positioned: the preserve step below is itself a remote
	// mutation, so a failure in the second write would otherwise leave the first
	// one uninvalidated and the invocation's memoized remote reads stale.
	defer strategy.InvalidateGitRemoteCache(ctx)
	if plan.preserveAs != "" {
		if _, err := gitRunner(ctx, dir, "remote", "add", plan.preserveAs, plan.replacedURL); err != nil {
			return fmt.Errorf("preserve current %s URL as %q: %w", plan.remote, plan.preserveAs, err)
		}
	}
	verb := "set-url"
	if plan.add {
		verb = "add"
	}
	if _, err := gitRunner(ctx, dir, "remote", verb, plan.remote, plan.mirrorURL); err != nil {
		return fmt.Errorf("point remote %q at the mirror: %w", plan.remote, err)
	}
	return nil
}

// reportMirrorRemotePlan echoes what was written, in recovery-friendly terms:
// every replaced URL is printed even when it was also preserved under another
// remote, so the previous value is always visible in the transcript.
//
// When preservation was requested but skipped, that gets an explicit stderr
// warning rather than just the absence of the "Kept the previous URL" line — the
// old URL is then only in this output, and an omitted line is far too quiet a
// signal for "your previous remote URL is no longer in git config" (a reader, or
// an agent scanning for ✓, would miss it).
func reportMirrorRemotePlan(out, errW io.Writer, plan mirrorRemotePlan) {
	if plan.noop {
		fmt.Fprintf(out, "Remote %q already points at the mirror:\n  %s\n", plan.remote, plan.mirrorURL)
		return
	}
	if plan.add {
		fmt.Fprintf(out, "✓ Added remote %q\n  %s\n", plan.remote, plan.mirrorURL)
	} else {
		fmt.Fprintf(out, "✓ Repointed remote %q at the mirror\n  %s\n", plan.remote, plan.mirrorURL)
		fmt.Fprintf(out, "  was: %s\n", gitremote.RedactURL(plan.replacedURL))
		if plan.preserveAs != "" {
			fmt.Fprintf(out, "✓ Kept the previous URL as remote %q\n", plan.preserveAs)
		}
	}
	fmt.Fprintf(out, "\nFetch through it:\n  git fetch %s\n", plan.remote)

	if plan.preserveSkipped != "" {
		// The URL is redacted here for the same reason it is on the "was:" line:
		// a replaced URL can carry credentials, and this warning is as likely to
		// end up in a log or a pasted transcript as anything else we print. Say so,
		// so a reader who needs the credentialed original knows to reconstruct it.
		fmt.Fprintf(errW, "\nWARNING: the previous URL of %q was NOT saved to git config — remote %q already exists.\n", plan.remote, plan.preserveSkipped)
		fmt.Fprintf(errW, "         It now only appears in the output above. To keep it under another name:\n")
		fmt.Fprintf(errW, "           git remote add <name> %s\n", gitremote.RedactURL(plan.replacedURL))
		fmt.Fprintf(errW, "         (credentials, if the URL had any, are redacted and must be re-supplied.)\n")
	}
}

// resolveMirrorUseUpstream determines the repository `remote add` should look
// for placements of. An explicit [repo] wins. Otherwise the coordinates are
// read from a configured remote — which already names the repo the user is
// standing in.
//
// Both forges resolve: a GitHub remote names a mirrorable upstream, and an
// entire:// remote names the repo it was cloned from, native or not. Which one
// came back decides how placements are looked up.
//
// Note the two distinct roles a remote name plays here: `remote` is the *write
// target* (the name being added or repointed), while repo identity can come
// from any remote that names the upstream. So the target remote is consulted
// first (re-running `add entire --override` on an already-mirrored remote must
// resolve), then `origin` — otherwise `add entire` on a fresh clone would fail
// purely because the remote it is about to create does not exist yet.
//
// entire:// remotes resolve as readily as forge remotes (their forge lives in
// the URL path), so switching clusters never needs the repo retyped.
func resolveMirrorUseUpstream(ctx context.Context, dir, remote, arg string) (mirrorRepoRef, error) {
	if arg != "" {
		return parseMirrorRepoRef(arg)
	}

	candidates := []string{remote}
	if remote != defaultMirrorRemote {
		candidates = append(candidates, defaultMirrorRemote)
	}
	// Track why each candidate was rejected so the error can say which remotes
	// were tried and what was wrong with them, rather than a bare "not found".
	var tried []string
	for _, name := range candidates {
		rawURL, gerr := gitremote.GetRemoteURLInDir(ctx, dir, name)
		if gerr != nil {
			tried = append(tried, name+" (not configured)")
			continue
		}
		info, perr := gitremote.ParseURL(rawURL)
		if perr != nil {
			tried = append(tried, name+" (unparseable URL)")
			continue
		}
		switch info.Forge {
		case mirrorCloneForge:
			// GitHub owners and repos are stored lowercase server-side.
			return mirrorRepoRef{forge: mirrorCloneForge, owner: strings.ToLower(info.Owner), repo: strings.ToLower(info.Repo)}, nil
		case nativeCloneForge:
			// Native names keep the spelling the remote carries; both lookups
			// behind them fold case server-side.
			return mirrorRepoRef{forge: nativeCloneForge, owner: info.Owner, repo: info.Repo}, nil
		default:
			tried = append(tried, name+" (not an Entire or GitHub repo)")
		}
	}
	return mirrorRepoRef{}, fmt.Errorf("cannot tell which repo to act on from the git remotes (tried %s); pass a repository reference explicitly (for example, /gh/owner/repo or /et/project/repo)", strings.Join(tried, ", "))
}

// newRepoRemoteCmd is the `entire repo remote` subtree: the git remote of an
// Entire repository. `add` points a remote in the current clone at one.
func newRepoRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Work with an Entire repository's git remote",
	}
	cmd.AddCommand(newRepoRemoteAddCmd())
	return requireSubcommand(cmd)
}
func newRepoRemoteAddCmd() *cobra.Command {
	var upstream, cluster string
	var override bool
	cmd := &cobra.Command{
		Use:   "add <remote-name> [repo]",
		Short: "Point a git remote in this clone at an Entire cluster",
		Long: "Adds a git remote so fetch and push go through Entire instead of " +
			"the forge.\n\n" +
			"The repo is resolved from the current clone's remotes; pass [repo] to " +
			"name a different one. With more than one cluster to choose from, a " +
			"terminal gets a picker and a script gets the repo's own cluster — " +
			"--cluster selects one either way, the same as `entire repo clone`.\n\n" +
			"<remote-name> must not already exist: as with `git remote add`, an " +
			"occupied name is refused rather than repointed. --override repoints " +
			"it instead, preserving the replaced URL under --upstream so the " +
			"forge stays reachable.\n\n" +
			"It only ever edits local git config — the cluster must already serve " +
			"the repo (`entire repo mirror add`); nothing server-side is " +
			"changed.\n\n" + mirrorRepoRefHelp,
		Example: "  entire repo remote add entire\n" +
			"  entire repo remote add entire --cluster aws-us-east-2.entire.io\n" +
			"  entire repo remote add entire /gh/octocat/hello-world\n" +
			"  entire repo remote add origin --override\n" +
			"  entire repo remote add origin --override --upstream ''",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			remote := strings.TrimSpace(args[0])
			if err := validateGitRemoteName(remote); err != nil {
				return fmt.Errorf("invalid remote name: %w", err)
			}
			// An empty --upstream is the documented opt-out of preserving the
			// replaced URL, so only a non-empty value is validated.
			if upstream != "" {
				if err := validateGitRemoteName(upstream); err != nil {
					return fmt.Errorf("invalid --upstream: %w", err)
				}
			}
			// Arguments are validated before the repo is resolved so a
			// malformed invocation fails identically inside and outside a clone.
			var repoArg string
			if len(args) > 1 {
				repoArg = strings.TrimSpace(args[1])
			}
			clusterHost := strings.TrimSpace(cluster)
			if clusterHost != "" {
				if err := validateClusterHost(clusterHost); err != nil {
					return fmt.Errorf("invalid --cluster: %w", err)
				}
			}

			ctx := cmd.Context()
			repoRoot, err := paths.WorktreeRoot(ctx)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Not a git repository. Run `entire repo remote add` from inside the clone whose remote you want to write.")
				return NewSilentError(errors.New("not a git repository"))
			}

			repoRef, err := resolveMirrorUseUpstream(ctx, repoRoot, remote, repoArg)
			if err != nil {
				return err
			}
			name := repoRef.owner + "/" + repoRef.repo
			qualified := "/" + repoRef.forge + "/" + name

			// For GitHub, the pull-gated placement lookup is the same authority
			// the clone's STS exchange enforces, so anything the user could clone
			// resolves here — public mirrors included. For a native repo the
			// placements are the repo's own primary plus its ready mirrors,
			// which the catalog turns into the cluster hosts a placement is
			// addressed by.
			var (
				placements []coreapi.ResolvedPlacement
				nativeRepo *coreapi.Repo
			)
			if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				if repoRef.forge == nativeCloneForge {
					repo, cat, lerr := loadNativeRepo(ctx, c, repoRef)
					if lerr != nil {
						return lerr
					}
					mirrors, lerr := listNativeMirrors(ctx, c, repo.ID)
					if lerr != nil {
						return lerr
					}
					nativeRepo = repo
					placements = nativeUsePlacements(repo, mirrors, cat)
					return nil
				}
				ps, lerr := resolvePullablePlacements(ctx, c, repoRef.owner, repoRef.repo)
				if lerr != nil {
					return lerr
				}
				placements = ps
				return nil
			}); err != nil {
				return err
			}
			if len(placements) == 0 {
				return fmt.Errorf("%s has no cluster you can fetch from; create a mirror first:\n  entire repo mirror add %s", qualified, qualified)
			}

			// Only a native repo has a primary — the cluster it lives on, which
			// a script that named no cluster wants. A GitHub repo's placements
			// are peer mirrors, so that case still asks for --cluster.
			primaryHost := ""
			if repoRef.forge == nativeCloneForge {
				primaryHost = strings.TrimSpace(nativeRepo.ClusterHost.Or(""))
			}
			chosen, err := selectPlacement(cmd, placements, clusterHost, primaryHost, placementPicker{
				selector: clusterSelectorFlag,
				title:    qualified + " is on more than one cluster — pick the one to use",
				action:   "Remote update",
			})
			if err != nil {
				return err
			}
			mirrorURL := forgeCloneURL(mirrorCloneForge, chosen.ClusterHost, repoRef.owner, repoRef.repo)
			// A native URL is the server's own path, not a reconstruction from
			// the ref, so `repo view`'s remote and this one cannot disagree.
			if repoRef.forge == nativeCloneForge {
				mirrorURL = nativeRepoURLAt(nativeRepo, chosen.ClusterHost)
			}
			// A native URL needs the repo's server-provided path, which a repo
			// that is still provisioning does not have yet. `git remote set-url`
			// accepts an empty URL and exits 0, so without this the command
			// would report "✓ Repointed remote" while leaving the remote
			// pointing nowhere — the one outcome worse than refusing.
			if mirrorURL == "" {
				return fmt.Errorf("%s has no clone URL on %s yet (still provisioning?); remote %q is unchanged", qualified, chosen.ClusterHost, remote)
			}

			remotes, err := listGitRemotes(ctx, repoRoot)
			if err != nil {
				return err
			}
			// GetRemoteURLInDir errors when the remote is absent; that is the
			// "add" case, which carries no current URL.
			currentURL := ""
			if remotes[remote] {
				if currentURL, err = gitremote.GetRemoteURLInDir(ctx, repoRoot, remote); err != nil {
					return fmt.Errorf("read current URL of remote %q: %w", remote, err)
				}
			}

			plan, err := planMirrorRemote(remote, mirrorURL, currentURL, upstream, override, remotes)
			if err != nil {
				return err
			}
			if err := applyMirrorRemotePlan(ctx, repoRoot, plan); err != nil {
				return err
			}
			reportMirrorRemotePlan(cmd.OutOrStdout(), cmd.ErrOrStderr(), plan)
			return nil
		},
	}
	cmd.Flags().StringVar(&upstream, "upstream", defaultMirrorUpstreamRemote, "Remote to preserve a replaced URL under; empty to discard it")
	cmd.Flags().StringVar(&cluster, "cluster", "", "Cluster host to use when the repo is on several")
	cmd.Flags().BoolVar(&override, "override", false, "Repoint <remote-name> when it already exists instead of refusing")
	return cmd
}
