package cli

import (
	"context"
	"fmt"

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

// newMirrorGrantListing is the mirror reading of a `repo grant list` ref. It
// claims every ref that does not declare the native forge, which is what keeps
// the ref errors honest: this verb takes both forges, so a ref naming neither
// must be offered both readings rather than the native path alone.
//
// The endpoint takes a cluster because a core fronting one serves it — a mirror
// is a per-placement copy — but what it answers with is the upstream GitHub
// repository's collaborators, one answer wherever it is asked. So the caller
// never names a region: the default cluster reads it.
func newMirrorGrantListing() *grantListBranch {
	return &grantListBranch{
		long:    repoGrantListLong,
		example: repoGrantListExample,
		list: func(cmd *cobra.Command, ref string) (bool, error) {
			if declaresForge(ref, nativeCloneForge) {
				return false, nil
			}
			// Past the native branch, a failure is about the ref, never the
			// command's shape.
			cmd.SilenceUsage = true
			if !declaresForge(ref, mirrorCloneForge) {
				return true, forgeQualifiedRefError(ref)
			}
			_, owner, repo, err := parseMirrorCloneRef(ref)
			if err != nil {
				return true, fmt.Errorf("invalid <repo> %q: %w", ref, err)
			}
			return true, runCoreListForCluster(cmd, defaultClusterHost, "No collaborators on this mirror; its access follows the upstream GitHub repository.", mirrorCollaboratorColumns, mirrorCollaboratorRow, func(ctx context.Context, c *coreapi.Client) ([]coreapi.MirrorCollaborator, error) {
				out, err := c.ListMirrorCollaborators(ctx, coreapi.ListMirrorCollaboratorsParams{
					Provider:    coreapi.ListMirrorCollaboratorsProviderGithub,
					Owner:       owner,
					Repo:        repo,
					ClusterHost: defaultClusterHost,
				})
				if err != nil {
					return nil, err
				}
				return out.Collaborators, nil
			})
		},
	}
}

// mirrorGrantsAreUpstreamErr is what `repo grant add` and `repo grant remove`
// answer a mirror ref: the grammar they accept is the native path, and a mirror
// has no grant to write. It names the read path that does answer for a mirror,
// so the refusal ends somewhere rather than at "unsupported".
func mirrorGrantsAreUpstreamErr(ref string) error {
	return fmt.Errorf("repo %q is a GitHub mirror: its access comes from the upstream GitHub repository, so it cannot be granted or revoked here — manage collaborators on GitHub; `entire repo grant list %s` shows who can pull the mirror", ref, ref)
}
