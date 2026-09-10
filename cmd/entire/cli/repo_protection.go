package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// branchRule is the JSON/table view of one branch-protection rule: the
// pattern as the server stores it and its level.
type branchRule struct {
	Ref                 string `json:"ref"`
	ServerSideMergeOnly bool   `json:"serverSideMergeOnly"`
}

var protectionColumns = []string{"BRANCH", "LEVEL"}

const (
	protectionLevelProtected = "protected"
	protectionLevelMergeOnly = "server-side merge only"
	protectionEmpty          = "Nothing is protected yet."
	// headBranchPattern is the server's pattern for "whatever branch HEAD
	// points at"; it follows a default-branch rename.
	headBranchPattern = "HEAD"
)

func protectionRow(r branchRule) []string {
	level := protectionLevelProtected
	if r.ServerSideMergeOnly {
		level = protectionLevelMergeOnly
	}
	return []string{r.Ref, level}
}

func branchRulesFromWire(p *coreapi.BranchProtection) []branchRule {
	rules := make([]branchRule, 0, len(p.Rules))
	for _, r := range p.Rules {
		rules = append(rules, branchRule{Ref: r.Ref, ServerSideMergeOnly: r.ServerSideMergeOnly.Or(false)})
	}
	return rules
}

// expandBranchRef maps the CLI argument onto the server's pattern syntax:
// "HEAD" and anything under refs/ pass through, a short name is a branch
// under refs/heads/. Wildcards are left to the server to validate.
func expandBranchRef(s string) (string, error) {
	switch {
	case s == "":
		return "", errors.New("branch must not be empty")
	case s == headBranchPattern, strings.HasPrefix(s, "refs/"):
		return s, nil
	default:
		return "refs/heads/" + s, nil
	}
}

// newRepoProtectionCmd groups the verbs for a repository's branch protection.
// Each rule names a branch pattern and one of two levels. "protected" refuses
// force pushes and deletion; fast-forward pushes stay allowed. "server-side
// merge only" also refuses every direct push: the branch moves only through a
// merge Entire performs, such as a trail merge, and whether that merge runs is
// decided by the repository's gates.
func newRepoProtectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "protection",
		Short: "List, add, or remove branch-protection rules",
		Long: "List, add, or remove a repository's branch-protection rules.\n\n" +
			"Each rule names a branch pattern and a level. \"protected\" refuses force pushes " +
			"and deletion; fast-forward pushes stay allowed. \"server-side merge only\" also " +
			"refuses every direct push: the branch moves only through a merge Entire performs, " +
			"such as a trail merge. A pattern is \"HEAD\" (the default branch), a branch name, " +
			"or a branch pattern with * and ? wildcards such as release/*. Entire-native " +
			"repositories only; a GitHub mirror's protection is the upstream's.",
	}
	cmd.AddCommand(newRepoProtectionListCmd())
	cmd.AddCommand(newRepoProtectionAddCmd())
	cmd.AddCommand(newRepoProtectionRemoveCmd())
	return cmd
}

func newRepoProtectionListCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "Show a repository's branch-protection rules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				out, err := c.GetBranchProtection(ctx, coreapi.GetBranchProtectionParams{RepoId: repoID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	addJSONFlag(cmd)
	return cmd
}

func newRepoProtectionAddCmd() *cobra.Command {
	var project string
	var mergeOnly bool
	cmd := &cobra.Command{
		Use:   "add <repo> <branch>",
		Short: "Protect a branch, or change the level of an existing rule",
		Long: "Protect a branch, or change the level of an existing rule.\n\n" +
			"<branch> is \"HEAD\", a branch name such as main, or a pattern such as release/*. " +
			"Without --server-side-merge-only the branch is protected from force pushes and " +
			"deletion. With it, every direct push is refused and the branch moves only through " +
			"a merge Entire performs. Adding a branch that already has a rule replaces that " +
			"rule's level. Prints the resulting rules. Requires manage permission on the repo.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := expandBranchRef(args[1])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				body := &coreapi.UpdateBranchProtectionInputBody{
					AddRules: []coreapi.BranchRule{{Ref: ref, ServerSideMergeOnly: coreapi.NewOptBool(mergeOnly)}},
				}
				out, err := c.UpdateBranchProtection(ctx, body, coreapi.UpdateBranchProtectionParams{RepoId: repoID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	cmd.Flags().BoolVar(&mergeOnly, "server-side-merge-only", false, "Refuse every direct push; the branch moves only through a merge Entire performs")
	addJSONFlag(cmd)
	return cmd
}

func newRepoProtectionRemoveCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "remove <repo> <branch>",
		Short: "Remove a branch-protection rule",
		Long: "Remove a branch-protection rule.\n\n" +
			"<branch> names the rule as it was added: \"HEAD\", a branch name, or a pattern. " +
			"Removing a branch that has no rule changes nothing. Prints the resulting rules. " +
			"Requires manage permission on the repo.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := expandBranchRef(args[1])
			if err != nil {
				cmd.SilenceUsage = true
				return err
			}
			return runCoreList(cmd, protectionEmpty, protectionColumns, protectionRow, func(ctx context.Context, c *coreapi.Client) ([]branchRule, error) {
				repoID, err := resolveRepoRef(ctx, c, args[0], project)
				if err != nil {
					return nil, err
				}
				body := &coreapi.UpdateBranchProtectionInputBody{RemoveRefs: []string{ref}}
				out, err := c.UpdateBranchProtection(ctx, body, coreapi.UpdateBranchProtectionParams{RepoId: repoID})
				if err != nil {
					return nil, err
				}
				return branchRulesFromWire(out), nil
			})
		},
	}
	bindRepoProjectFlag(cmd, &project)
	addJSONFlag(cmd)
	return cmd
}
