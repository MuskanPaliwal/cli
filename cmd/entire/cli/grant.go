package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/entireio/cli/internal/coreapi"
)

// The `grant` subtrees under `entire org`, `entire project` and `entire repo`:
// one builder, three target descriptions. Every leaf gives a user access to a
// target — `add <target> <grantee> --role`, `list <target>`, `remove <target>
// <grantee>` — and only the target kind changes: how it is addressed, which
// roles it has, and which generated client calls it maps to. The builder owns
// the command shape and the shared plumbing; a grantTarget owns the typed calls.
//
// Grantees are addressed by a provider-qualified handle (e.g. github:alice),
// which the CLI resolves to the provider account behind the scenes. `remove`
// also accepts an account ULID where the API has a typed-id route (project and
// repo). A user account is the only grantee kind the API grants to today.

// grantTarget describes one resource kind the shared `<noun> grant` subtree
// manages. Row is the wire type of one listing entry.
type grantTarget[Row any] struct {
	noun        string   // "org" | "project" | "repo": in Use, help and messages
	refUsage    string   // how a target is addressed, for Long: "name or ULID"; reads after "addressed by"
	exampleRef  string   // a target ref for the Example lines
	roles       []string // accepted --role values, in help order
	defaultRole string   // "" means --role is required; else the server default applied when --role is omitted
	columns     []string
	row         func(Row) []string

	// resolve turns the user's target ref into its ULID. project and repo wrap
	// theirs because those resolvers take an interface, not *coreapi.Client.
	resolve func(ctx context.Context, c *coreapi.Client, ref string) (string, error)
	// grant gives the provider account the role on the resolved target and
	// returns the effective role: the server's, when role was left to default.
	grant            func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (granted string, wire any, err error)
	list             func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]Row, coreapi.OptString, error)
	revokeByProvider func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error
	// revokeByID is the typed-id route for an account ULID grantee. nil when
	// the target has none (org), so a ULID grantee falls through to
	// resolveGranteeProvider and is refused with the handle form named.
	revokeByID func(ctx context.Context, c *coreapi.Client, id, granteeID string) error
	// candidates lists who could be granted this target for the interactive
	// picker: members of the owning org who hold no grant on it yet, plus the
	// org's total membership so an empty pool can say which of "no members" and
	// "everyone already has access" happened. nil where no pool is enumerable
	// (org), which is what leaves `org grant add` exactly as it was.
	candidates func(ctx context.Context, c *coreapi.Client, id string) (offer []grantCandidate, orgSize int, err error)
	// ownerNotOrg phrases an ownerNotOrgError for this target, given the name of
	// the account-owned project. The repo wording has to name the project
	// standing between the repo and the missing org, so one shared sentence
	// cannot serve both.
	ownerNotOrg func(pt grantPickerTarget, project string) string
}

func newOrgGrantCmd() *cobra.Command     { return newGrantSubtreeCmd(orgGrantTarget) }
func newProjectGrantCmd() *cobra.Command { return newGrantSubtreeCmd(projectGrantTarget) }
func newRepoGrantCmd() *cobra.Command    { return newGrantSubtreeCmd(repoGrantTarget) }

func newGrantSubtreeCmd[Row any](t grantTarget[Row]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   cmdGrant,
		Short: "Manage " + t.noun + " access",
	}
	cmd.AddCommand(newGrantAddCmd(t), newGrantListCmd(t), newGrantRemoveCmd(t))
	return cmd
}

func newGrantAddCmd[Row any](t grantTarget[Row]) *cobra.Command {
	var role string
	required := t.defaultRole == ""
	example := fmt.Sprintf("  entire %s grant add %s github:alice", t.noun, t.exampleRef)
	roleHelp := "Role: one of " + strings.Join(t.roles, ", ")
	if required {
		example += " --role " + t.roles[0]
		roleHelp += " (required; asked for if omitted on a terminal)"
	} else {
		roleHelp += " (default " + t.defaultRole + ")"
	}
	// A target with no candidate pool (org) keeps the two-argument shape, so
	// cobra reports a missing grantee exactly as it always has.
	use := fmt.Sprintf("add <%s> <grantee>", t.noun)
	long := fmt.Sprintf("Grant a user (addressed as provider:handle, e.g. github:alice) %s access. The %s is addressed by %s.", t.noun, t.noun, t.refUsage)
	args := cobra.ExactArgs(2)
	if t.candidates != nil {
		use = fmt.Sprintf("add <%s> [grantee]", t.noun)
		long += fmt.Sprintf(" Omit the grantee on a terminal to choose from the members of the owning org who do not have %s access yet, and set a role for each.", t.noun)
		args = cobra.RangeArgs(1, 2)
	}
	cmd := &cobra.Command{
		Use:     use,
		Short:   fmt.Sprintf("Grant a user %s access", t.noun),
		Long:    long,
		Example: example,
		Args:    args,
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			// A role the user typed is always checked, an explicit `--role=`
			// included: an empty value is not the same as leaving the flag out.
			// An omitted --role means the server default where the target has
			// one, and otherwise is resolved per grantee below.
			if cmd.Flags().Changed("role") {
				if err := validateRole(role, t.roles); err != nil {
					return err
				}
			}
			pt := grantPickerTarget{noun: t.noun, ref: args[0], roles: t.roles}
			grantee := ""
			if len(args) == 2 {
				grantee = args[1]
			}
			// Both refusals a non-interactive run can hit are decided from the
			// command line alone, so they are settled before any request: an
			// unanswerable prompt must not cost a lookup, and an omitted --role
			// must not reach the API — the property cobra's required-flag check
			// used to provide.
			if !interactive.CanPromptInteractively() {
				if grantee == "" {
					return pickerUnavailable(pt, "no grantee given")
				}
				if role == "" && required {
					return missingRoleErr(t.roles)
				}
			}
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return err
				}
				picked, err := resolveGrantSelections(ctx, cmd, c, t, pt, grantee, id, role, required)
				if err != nil {
					return err
				}
				return grantEach(ctx, cmd, c, t, pt, id, picked)
			})
		},
	}
	cmd.Flags().StringVar(&role, "role", "", roleHelp)
	addJSONFlag(cmd)
	return cmd
}

// resolveGrantSelections turns the command line into the grantee/role pairs to
// grant. A grantee argument is one pair, taking --role or the prompt; an omitted
// grantee opens the picker.
//
// --role is deliberately NOT cobra-required, even where the target has no server
// default: cobra enforces required flags before RunE, which would make a role
// impossible to prompt for. The property that used to guarantee — an omitted
// role never reaching validation, a lookup, or the API — is kept here instead,
// by resolving one before anything is granted.
func resolveGrantSelections[Row any](ctx context.Context, cmd *cobra.Command, c *coreapi.Client, t grantTarget[Row], pt grantPickerTarget, grantee, id, role string, roleRequired bool) ([]grantSelection, error) {
	if grantee != "" {
		// A grantee with no --role where the target has no server default still
		// needs one; prompting for it is the one-row version of the picker's
		// second screen. The non-interactive case was refused before any request.
		if role == "" && roleRequired {
			return grantPicker(cmd, pt, nil, []string{grantee}, "")
		}
		return []grantSelection{{handle: grantee, role: role}}, nil
	}
	offer, orgSize, err := t.candidates(ctx, c, id)
	if err != nil {
		var notOrg *ownerNotOrgError
		if errors.As(err, &notOrg) {
			return nil, pickerUnavailable(pt, t.ownerNotOrg(pt, notOrg.project))
		}
		return nil, err
	}
	if len(offer) == 0 {
		// Two conditions, not one: "every member already has access" is a claim
		// about members that must not be printed when there are none.
		if orgSize == 0 {
			return nil, pickerUnavailable(pt, pt.describe()+" has no org members to choose from")
		}
		return nil, pickerUnavailable(pt, fmt.Sprintf("every member of the org owning %s already has access to it", pt.describe()))
	}
	return grantPicker(cmd, pt, offer, nil, role)
}

// grantEach grants every pair in turn. On a failure it stops and returns,
// having reported the grants that already landed: those are real, and the CLI
// cannot undo them, so the user needs to know which ones to skip on a retry.
func grantEach[Row any](ctx context.Context, cmd *cobra.Command, c *coreapi.Client, t grantTarget[Row], pt grantPickerTarget, id string, picked []grantSelection) error {
	wires := make([]any, 0, len(picked))
	for _, p := range picked {
		provider, providerUserID, err := resolveGranteeProvider(ctx, c, p.handle)
		if err != nil {
			return errors.Join(err, emitGrantJSON(cmd, wires, len(picked)))
		}
		granted, wire, err := t.grant(ctx, c, id, provider, providerUserID, p.role)
		if err != nil {
			return errors.Join(err, emitGrantJSON(cmd, wires, len(picked)))
		}
		wires = append(wires, wire)
		if !jsonRequested(cmd) {
			fmt.Fprintf(cmd.OutOrStdout(), "✓ Granted %s %s access to %s\n", p.handle, granted, pt.describe())
		}
	}
	return emitGrantJSON(cmd, wires, len(picked))
}

// emitGrantJSON writes the wire objects for --json; text mode has already
// printed a line per grant as it went. The SHAPE follows how many grants were
// asked for and the CONTENT follows how many landed, so one grantee named on
// the command line stays the single object callers already parse, and a picked
// set stays an array even when a failure cut it short.
func emitGrantJSON(cmd *cobra.Command, wires []any, requested int) error {
	if !jsonRequested(cmd) || len(wires) == 0 {
		return nil
	}
	if requested == 1 {
		return printJSON(cmd.OutOrStdout(), wires[0])
	}
	return printJSON(cmd.OutOrStdout(), wires)
}

// missingRoleErr replaces cobra's required-flag message for --role, which no
// longer marks it required (see resolveGrantSelections).
func missingRoleErr(roles []string) error {
	return fmt.Errorf("--role is required: one of %s", strings.Join(roles, ", "))
}

func newGrantListCmd[Row any](t grantTarget[Row]) *cobra.Command {
	cmd := &cobra.Command{
		Use:   fmt.Sprintf("list <%s>", t.noun),
		Short: fmt.Sprintf("List who has %s access", t.noun),
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCoreList(cmd, "No grants found.", t.columns, t.row, func(ctx context.Context, c *coreapi.Client) ([]Row, error) {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return nil, err
				}
				return pagedList(ctx, func(ctx context.Context, pageToken coreapi.OptString) ([]Row, coreapi.OptString, error) {
					return t.list(ctx, c, id, pageToken)
				})
			})
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newGrantRemoveCmd[Row any](t grantTarget[Row]) *cobra.Command {
	grantee := "a provider-qualified handle (e.g. github:alice)"
	if t.revokeByID != nil {
		grantee += " or an account ULID"
	}
	return &cobra.Command{
		Use:     fmt.Sprintf("remove <%s> <grantee>", t.noun),
		Short:   fmt.Sprintf("Revoke a user's %s access", t.noun),
		Long:    fmt.Sprintf("Revoke a grantee's %s access. The %s is addressed by %s; the grantee is %s.", t.noun, t.noun, t.refUsage, grantee),
		Example: fmt.Sprintf("  entire %s grant remove %s github:alice", t.noun, t.exampleRef),
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
				id, err := t.resolve(ctx, c, args[0])
				if err != nil {
					return err
				}
				target := t.noun + " " + args[0]
				if t.revokeByID != nil && looksLikeULID(args[1]) {
					return revokeGrant(cmd, "account "+args[1]+" from "+target, func() error {
						return t.revokeByID(ctx, c, id, args[1])
					})
				}
				provider, providerUserID, err := resolveGranteeProvider(ctx, c, args[1])
				if err != nil {
					return err
				}
				return revokeGrant(cmd, args[1]+" from "+target, func() error {
					return t.revokeByProvider(ctx, c, id, provider, providerUserID)
				})
			})
		},
	}
}

// validateRole rejects a --role outside the target's set at the CLI boundary
// so the user gets a clear message instead of a server 422. The generated
// bodies use a distinct enum type per target that shares these values, so the
// targets cast the validated string to whichever type they need.
func validateRole(role string, allowed []string) error {
	if slices.Contains(allowed, role) {
		return nil
	}
	return fmt.Errorf("invalid --role %q: must be one of %s", role, strings.Join(allowed, ", "))
}

// revokeGrant runs a grant-removal API call idempotently. A 404 means the
// grantee already has no such grant — the desired end state — so it's reported
// as a no-op rather than surfaced as a raw error, matching runControlPlaneDelete.
// subject describes the grant, e.g. "github:alice from repo /et/acme/web".
func revokeGrant(cmd *cobra.Command, subject string, revoke func() error) error {
	if err := revoke(); err != nil {
		if isCoreNotFound(err) {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: no such grant; nothing to revoke\n", subject)
			return nil
		}
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "✓ Revoked %s\n", subject)
	return nil
}

// orgMemberColumns / grantColumns are the human table views of the
// membership/grant listings. Both lead with GRANTEE, the friendly name the
// server resolved (a provider:handle, or an org name), falling back to the
// ULID. Org membership is flat, so it carries the membership STATUS instead
// of provenance; project and repo grants include owner and inherited rows,
// so they add SOURCE and TYPE. No table prints an internal id: the grantee
// ULID is in the --json output for anyone who needs it.
var (
	orgMemberColumns = []string{"GRANTEE", colHeaderRole, colHeaderStatus}
	grantColumns     = []string{"GRANTEE", colHeaderRole, "SOURCE", "TYPE"}
)

func orgMemberRow(m coreapi.Membership) []string {
	return []string{granteeName(m.Handle, m.AccountId), m.Role, m.Status}
}

func projectGrantRow(g coreapi.ProjectGrant) []string {
	return []string{granteeName(g.GranteeName, g.GranteeId), g.Role, g.Source, g.GranteeType}
}

// repoGrantRow mirrors projectGrantRow; RepoGrant and ProjectGrant share the
// grantee/role/source shape, so both reuse grantColumns.
func repoGrantRow(g coreapi.RepoGrant) []string {
	return []string{granteeName(g.GranteeName, g.GranteeId), g.Role, g.Source, g.GranteeType}
}

// granteeName returns the friendly name when the server resolved one, falling
// back to the ULID for grantees it couldn't label (e.g. teams).
func granteeName(name coreapi.OptString, granteeID string) string {
	if n := name.Or(""); n != "" {
		return n
	}
	return granteeID
}

// granteeTypeAccount is the only grantee kind the revoke-by-id calls take; the
// provider-qualified variant has its own endpoint.
const granteeTypeAccount = "account"

// accessRoles are the project and repo grant roles; the two targets share the
// set because the server's SpiceDB relations are the same for both.
var accessRoles = []string{"reader", "writer", "admin"}

// orgGrantTarget is org membership: roles owner/admin/member with member as
// the server default, a target addressed by name or ULID, and no typed-id
// revoke route — members are removed by their provider identity.
var orgGrantTarget = grantTarget[coreapi.Membership]{
	noun:        cmdOrg,
	refUsage:    "name or ULID",
	exampleRef:  "acme",
	roles:       []string{"owner", "admin", "member"},
	defaultRole: "member",
	columns:     orgMemberColumns,
	row:         orgMemberRow,
	resolve:     resolveOrgRef,
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		body := &coreapi.AddOrgMemberInputBody{Provider: provider, ProviderUserId: providerUserID}
		if role != "" {
			body.Role = coreapi.NewOptAddOrgMemberInputBodyRole(coreapi.AddOrgMemberInputBodyRole(role))
		}
		m, err := c.AddOrgMember(ctx, body, coreapi.AddOrgMemberParams{OrgId: id})
		if err != nil {
			return "", nil, err
		}
		return m.Role, m, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.Membership, coreapi.OptString, error) {
		out, err := c.ListOrgMembers(ctx, coreapi.ListOrgMembersParams{OrgId: id, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RemoveOrgMember(ctx, coreapi.RemoveOrgMemberParams{OrgId: id, Provider: provider, ProviderUserId: providerUserID})
	},
}

// projectGrantTarget is project access: roles reader/writer/admin, required,
// on a project addressed by name or ULID.
var projectGrantTarget = grantTarget[coreapi.ProjectGrant]{
	noun:       "project",
	refUsage:   "name or ULID",
	exampleRef: "widgets",
	roles:      accessRoles,
	columns:    grantColumns,
	row:        projectGrantRow,
	resolve: func(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
		return resolveProjectRef(ctx, c, ref)
	},
	candidates: projectGrantCandidates,
	ownerNotOrg: func(pt grantPickerTarget, _ string) string {
		// The project the user named IS the account-owned one, so its name is
		// already in pt.ref and the error's copy would just repeat it.
		return pt.describe() + " is owned by an account, so it has no member list to choose from"
	},
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		out, err := c.GrantProjectAccess(ctx, &coreapi.GrantProjectAccessInputBody{
			Provider:       provider,
			ProviderUserId: providerUserID,
			Role:           coreapi.GrantProjectAccessInputBodyRole(role),
		}, coreapi.GrantProjectAccessParams{ProjectId: id})
		if err != nil {
			return "", nil, err
		}
		return role, out, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.ProjectGrant, coreapi.OptString, error) {
		out, err := c.ListProjectMembers(ctx, coreapi.ListProjectMembersParams{ProjectId: id, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Members, out.NextPageToken, nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RevokeProjectAccessByProvider(ctx, coreapi.RevokeProjectAccessByProviderParams{ProjectId: id, Provider: provider, ProviderUserId: providerUserID})
	},
	revokeByID: func(ctx context.Context, c *coreapi.Client, id, granteeID string) error {
		return c.RevokeProjectAccess(ctx, coreapi.RevokeProjectAccessParams{ProjectId: id, GranteeType: granteeTypeAccount, GranteeId: granteeID})
	},
}

// repoGrantTarget is repo access: roles reader/writer/admin, required, on a
// repo addressed by its /et/<project>/<repo> path and nothing else.
var repoGrantTarget = grantTarget[coreapi.RepoGrant]{
	noun:       cmdRepo,
	refUsage:   "its /" + nativeCloneForge + "/<project>/<repo> path",
	exampleRef: "/" + nativeCloneForge + "/acme/web",
	roles:      accessRoles,
	columns:    grantColumns,
	row:        repoGrantRow,
	resolve: func(ctx context.Context, c *coreapi.Client, ref string) (string, error) {
		return resolveRepoPath(ctx, c, ref)
	},
	candidates: repoGrantCandidates,
	ownerNotOrg: func(pt grantPickerTarget, project string) string {
		// A repo's pool comes from its project's org, so the refusal names the
		// project in between rather than leaving the user to find it.
		return fmt.Sprintf("%s is in project %s, which is owned by an account, so it has no member list to choose from", pt.describe(), project)
	},
	grant: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID, role string) (string, any, error) {
		out, err := c.GrantRepoAccess(ctx, &coreapi.GrantRepoAccessInputBody{
			Provider:       provider,
			ProviderUserId: providerUserID,
			Role:           coreapi.GrantRepoAccessInputBodyRole(role),
		}, coreapi.GrantRepoAccessParams{RepoId: id})
		if err != nil {
			return "", nil, err
		}
		return role, out, nil
	},
	list: func(ctx context.Context, c *coreapi.Client, id string, pageToken coreapi.OptString) ([]coreapi.RepoGrant, coreapi.OptString, error) {
		out, err := c.ListRepoGrants(ctx, coreapi.ListRepoGrantsParams{RepoId: id, PageToken: pageToken})
		if err != nil {
			return nil, coreapi.OptString{}, err
		}
		return out.Grants, out.NextPageToken, nil
	},
	revokeByProvider: func(ctx context.Context, c *coreapi.Client, id, provider, providerUserID string) error {
		return c.RevokeRepoAccessByProvider(ctx, coreapi.RevokeRepoAccessByProviderParams{RepoId: id, Provider: provider, ProviderUserId: providerUserID})
	},
	revokeByID: func(ctx context.Context, c *coreapi.Client, id, granteeID string) error {
		return c.RevokeRepoAccess(ctx, coreapi.RevokeRepoAccessParams{RepoId: id, GranteeType: granteeTypeAccount, GranteeId: granteeID})
	},
}
