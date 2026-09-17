package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/entireio/cli/internal/coreapi"
)

// "Who can reach this repository?" is one question, and `repo grant` is where
// it is answered — for both kinds of repository, because a user asking it does
// not necessarily know which backs the repo. The two answers come from
// different places, which is what this file holds:
//
//   - An Entire repository's access IS its grants: `list` reads them and `add`
//     and `remove` write them.
//   - A GitHub mirror's access is the upstream repository's. Entire only
//     materializes it per placement, so `list` reads it and nothing writes it
//     — mirrorGrantsAreUpstreamErr is what `add` and `remove` say instead.
//
// The mirror half is deliberately absent from `add`/`remove`'s grammar rather
// than accepted and always refused: a command that takes a ref it can never act
// on reads as a bug the first time and a lie the second.

// mirrorCollaboratorColumns is the mirror half of `repo grant list`. It is
// grantColumns' leading pair, the two things the mirror endpoint answers for:
// who, and with which role. The columns behind them are the native listing's
// provenance (SOURCE/TYPE), which the mirror endpoint does not report and which
// would be invented if this table filled them in. Like every grant table it
// prints no internal id — the account ULID is in the --json output.
var mirrorCollaboratorColumns = []string{colHeaderGrantee, colHeaderRole}

func mirrorCollaboratorRow(c coreapi.MirrorCollaborator) []string {
	return []string{granteeName(c.Handle, c.AccountId), c.Role}
}

// repoGrantListLong explains the one question and its two answers, including
// the two ways a caller can be refused: the native listing is answered by
// Entire from your Entire access, the mirror listing by GitHub from your GitHub
// identity.
const repoGrantListLong = "List who can reach a repository.\n\n" +
	"An Entire repository (/" + nativeCloneForge + "/<project>/<repo>) lists its grants: each grantee, " +
	"their reader/writer/admin role, and whether the grant is held on the repo itself or " +
	"inherited. Entire's control plane answers it from your access to the repo.\n\n" +
	"A GitHub mirror (/" + mirrorCloneForge + "/<owner>/<repo>) lists who can pull the mirror. Mirror " +
	"access follows the upstream GitHub repository and is " +
	"read-only here, so `add` and `remove` do not take a mirror ref. The check is live and " +
	"against your own GitHub identity — you must be a current admin of the upstream " +
	"repository, or the owner of a personal one — so run it as yourself rather than with a " +
	"service-account token.\n\n" +
	"--json prints what the API returned, which differs between the two: grants for an " +
	"Entire repository, collaborators for a mirror."

const repoGrantListExample = "  entire repo grant list /" + nativeCloneForge + "/acme/web\n" +
	"  entire repo grant list /" + mirrorCloneForge + "/acme/widget"

// mirrorGrantListing is the mirror reading of a `repo grant list` ref. It
// claims every ref that does not declare the native forge, which is what keeps
// the ref errors honest: this verb takes both forges, so a ref naming neither
// must be offered both readings rather than the native path alone.
var mirrorGrantListing = &grantListBranch{
	long:    repoGrantListLong,
	example: repoGrantListExample,
	claims:  func(ref string) bool { return !declaresForge(ref, nativeCloneForge) },
	list: func(cmd *cobra.Command, ref string) error {
		// Every failure from here is about the ref, never the command's shape.
		cmd.SilenceUsage = true
		if !declaresForge(ref, mirrorCloneForge) {
			return forgeQualifiedRefError(ref)
		}
		_, owner, repo, err := parseMirrorCloneRef(ref)
		if err != nil {
			return fmt.Errorf("invalid <repo> %q: %w", ref, err)
		}
		return listMirrorCollaborators(cmd, owner, repo)
	},
}

// listMirrorCollaborators prints who can pull owner/repo's mirror.
//
// The collaborator endpoint is served by the core fronting ONE cluster, so the
// placement to ask is resolved first, on the active context's core. The caller
// is never asked which region they mean: every placement materializes the same
// upstream GitHub collaborators, so any one answers — but it has to be a
// placement the repo actually has, which is the whole reason this lookup is
// here rather than a hard-coded default.
//
// Resolving as the active login is also what scopes this to one federation:
// a cluster can be fronted by cores the active login has no account with, and
// those mirrors are reached by acting as the login that does (`--context`),
// not by naming their cluster.
func listMirrorCollaborators(cmd *cobra.Command, owner, repo string) error {
	var clusterHost string
	if err := runCore(cmd, func(ctx context.Context, c *coreapi.Client) error {
		// The pull-gated placement lookup, the same authority `repo clone` and
		// `remote use` resolve through, so a public mirror resolves too.
		placements, err := resolvePullablePlacements(ctx, c, owner, repo)
		if err != nil {
			return err
		}
		if len(placements) == 0 {
			// The lookup runs as the active login, so "no placements" also
			// covers a mirror in a federation that login does not front —
			// which is an identity to switch, not a region to name.
			return fmt.Errorf("no mirror of %s/%s you can read as this login: it is not mirrored, you have no access to its mirrors, or it lives in a federation this login does not front (`entire auth contexts` lists your logins; --context acts as one)", owner, repo)
		}
		// Told apart from "not mirrored" deliberately: the repo IS mirrored, so
		// blaming access would send the user to fix the wrong thing — and the
		// command named below would then list the placement and contradict it.
		clusterHost = mirrorReadCluster(placements)
		if clusterHost == "" {
			return fmt.Errorf("%s/%s is mirrored, but no placement names a cluster host this command can dial; `entire repo mirror get /%s/%s/%s` lists them", owner, repo, mirrorCloneForge, owner, repo)
		}
		return nil
	}); err != nil {
		return err
	}
	return runCoreListForCluster(cmd, clusterHost, "No collaborators on this mirror; its access follows the upstream GitHub repository.", mirrorCollaboratorColumns, mirrorCollaboratorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.MirrorCollaborator, error) {
		out, err := c.ListMirrorCollaborators(ctx, coreapi.ListMirrorCollaboratorsParams{
			Provider:    coreapi.ListMirrorCollaboratorsProviderGithub,
			Owner:       owner,
			Repo:        repo,
			ClusterHost: clusterHost,
		})
		if err != nil {
			return nil, err
		}
		return out.Collaborators, nil
	})
}

// mirrorReadCluster picks which placement answers for the mirror. Any of them
// gives the same upstream collaborators, so this is only about being
// deterministic: the default cluster when the repo is mirrored there, else the
// first host in sorted order. A host that is not a bare host[:port] is skipped
// rather than dialed — it names the core this command authenticates to, so the
// same guard repoRemoteURL applies to a server-provided host applies here.
// Returns "" when no placement is usable, which the caller reports.
func mirrorReadCluster(placements []coreapi.ResolvedPlacement) string {
	hosts := make([]string, 0, len(placements))
	for _, p := range placements {
		host := strings.TrimSpace(p.ClusterHost)
		if validateClusterHost(host) != nil {
			continue
		}
		if strings.EqualFold(host, defaultClusterHost) {
			return defaultClusterHost
		}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return ""
	}
	sort.Strings(hosts)
	return hosts[0]
}

// mirrorGrantsAreUpstream is what `repo grant add` and `repo grant remove`
// answer a mirror ref: the grammar they accept is the native path, and a mirror
// has no grant to write. It names the read path that does answer for a mirror,
// so the refusal ends somewhere rather than at "unsupported". Every other ref
// passes, to be judged by the resolver that parses it.
func mirrorGrantsAreUpstream(ref string) error {
	if !declaresForge(ref, mirrorCloneForge) {
		return nil
	}
	return fmt.Errorf("repo %q is a GitHub mirror: its access comes from the upstream GitHub repository, so it cannot be granted or revoked here — manage collaborators on GitHub; `entire repo grant list %s` shows who has access to the repo", ref, ref)
}
