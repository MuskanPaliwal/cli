package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// lookupDispatchRepoJurisdictions is the seam for the best-effort placement
// hint below; swapped in tests.
var lookupDispatchRepoJurisdictions = defaultLookupDispatchRepoJurisdictions

// describeDispatchRepoNotFound enriches the cell's cross-jurisdiction 404.
//
// A cloud dispatch is generated and stored by ONE jurisdiction's cell, and that
// cell only knows repos placed in its region — so "repository not found" from
// the US cell about a repo that lives only in EU is a routing problem, not a
// missing repo. The dispatch package already phrases the error around the
// jurisdiction that was targeted; this layer adds the one fact the user needs
// to act on it — which jurisdictions the repo IS placed in — from the control
// plane's repo index, which is the same source the repo-scoped cell resolvers
// use. It is best-effort: any control-plane failure leaves the message as is.
func describeDispatchRepoNotFound(ctx context.Context, err error) error {
	var notFound *dispatchpkg.RepoNotFoundError
	if !errors.As(err, &notFound) || len(notFound.Repos) == 0 {
		return err
	}
	placements := lookupDispatchRepoJurisdictions(ctx, notFound.Repos)
	if len(placements) == 0 {
		return err
	}

	var b strings.Builder
	b.WriteString(err.Error())
	for _, repo := range notFound.Repos {
		jurisdictions, ok := placements[repo]
		if !ok {
			continue
		}
		if len(jurisdictions) == 0 {
			fmt.Fprintf(&b, "\n  %s has no ready placement in any jurisdiction", repo)
			continue
		}
		fmt.Fprintf(&b, "\n  %s is placed in: %s", repo, strings.Join(jurisdictions, ", "))
	}
	return errors.New(b.String())
}

// defaultLookupDispatchRepoJurisdictions returns, per repo, the sorted
// jurisdiction slugs of its non-failed placements. A repo the control plane
// does not know (not onboarded, not visible) is absent from the map; a repo
// known but with no usable placement maps to an empty slice. Bounded by
// cellResolveTimeout and never fails — the caller has an error to show already.
func defaultLookupDispatchRepoJurisdictions(ctx context.Context, repos []string) map[string][]string {
	ctx, cancel := context.WithTimeout(ctx, cellResolveTimeout)
	defer cancel()

	client, err := newCellCoreClient()
	if err != nil {
		logging.Debug(ctx, "dispatch: control plane unavailable for placement hint", "error", err)
		return nil
	}

	out := make(map[string][]string, len(repos))
	for _, repo := range repos {
		fullName := strings.TrimSpace(repo)
		if fullName == "" {
			continue
		}
		resp, err := client.ListRepos(ctx, coreapi.ListReposParams{Filter: coreapi.NewOptString(fullName)})
		if err != nil {
			logging.Debug(ctx, "dispatch: placement hint lookup failed", "error", err)
			continue
		}
		for _, entry := range resp.Repos {
			if !strings.EqualFold(strings.TrimSpace(entry.FullName), fullName) {
				continue
			}
			out[repo] = placementJurisdictions(entry.Placements)
			break
		}
	}
	return out
}

// placementJurisdictions lists the distinct jurisdictions a repo has a
// non-failed placement in, sorted. Failed/suspended copies are skipped: a
// cell cannot answer for them, so offering them would just reproduce the 404.
func placementJurisdictions(placements []coreapi.RepoPlacement) []string {
	seen := make(map[string]struct{}, len(placements))
	for _, p := range placements {
		if p.Status == coreapi.RepoPlacementStatusFailed || p.Status == coreapi.RepoPlacementStatusSuspended {
			continue
		}
		j := strings.ToLower(strings.TrimSpace(p.Jurisdiction))
		if j == "" {
			continue
		}
		seen[j] = struct{}{}
	}
	jurisdictions := make([]string, 0, len(seen))
	for j := range seen {
		jurisdictions = append(jurisdictions, j)
	}
	sort.Strings(jurisdictions)
	return jurisdictions
}
