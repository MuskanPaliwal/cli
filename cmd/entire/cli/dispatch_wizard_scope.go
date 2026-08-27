package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// dispatchWizardScopeTimeout bounds the control-plane repo index read behind
// the wizard's jurisdiction picker (the budget code search gives the same call).
const dispatchWizardScopeTimeout = 10 * time.Second

// Seams for the wizard's cloud catalogue, swapped in tests.
var (
	listDispatchWizardPlacements = defaultListDispatchWizardPlacements
	resolveDispatchWizardHome    = defaultResolveDispatchWizardHome
)

// dispatchWizardScope is the wizard's view of where the caller's repos live,
// mirroring the web app's dispatch form: a dispatch covers repos placed in
// exactly one jurisdiction, so the form asks for the jurisdiction first and
// offers only the repos placed there, making a mixed selection unrepresentable.
//
// Everything is precomputed once by newDispatchWizardScope; the form reads it
// on every render. A repo the control plane does not place (or, with no
// placement data at all, every repo) is attributed to home, which is where the
// gateway routes when no selector is sent. Only READY placements count — a
// cell cannot generate from a copy still syncing — which deliberately differs
// from routedRepoPlacement's single elected primary (a search-indexing rule).
type dispatchWizardScope struct {
	// repos are the offerable slugs (repos with checkpoints), recent-first.
	repos []string
	// byJurisdiction lists the offerable repos per jurisdiction, in repos order.
	byJurisdiction map[string][]string
	// jurisdictions are the keys of byJurisdiction, sorted; "" (placement-less
	// repos with home unknown) is not a jurisdiction a user could pick.
	jurisdictions []string
	// defaultJurisdiction is home when the caller has repos there, else the
	// jurisdiction holding the most repos (ties alphabetical), else "".
	defaultJurisdiction string
	home                string
}

// newDispatchWizardScope indexes the catalogue. placements maps a lowercased
// slug to the sorted jurisdictions of its READY placements (nil when the
// control plane could not be asked); home is the caller's home slug or "".
func newDispatchWizardScope(repos []string, placements map[string][]string, home string) *dispatchWizardScope {
	scope := &dispatchWizardScope{repos: repos, byJurisdiction: make(map[string][]string), home: home}
	for _, slug := range repos {
		jurisdictions, ok := placements[strings.ToLower(strings.TrimSpace(slug))]
		if !ok || len(jurisdictions) == 0 {
			jurisdictions = []string{home}
		}
		for _, j := range jurisdictions {
			if j != "" {
				scope.byJurisdiction[j] = append(scope.byJurisdiction[j], slug)
			}
		}
	}
	scope.jurisdictions = slices.Sorted(maps.Keys(scope.byJurisdiction))
	for _, j := range scope.jurisdictions {
		if j == home {
			scope.defaultJurisdiction = j
			break
		}
		if len(scope.byJurisdiction[j]) > len(scope.byJurisdiction[scope.defaultJurisdiction]) {
			scope.defaultJurisdiction = j
		}
	}
	return scope
}

// reposIn lists the offerable repos in a jurisdiction; "" (nothing picked)
// offers every repo.
func (s *dispatchWizardScope) reposIn(jurisdiction string) []string {
	if jurisdiction == "" {
		return s.repos
	}
	return s.byJurisdiction[jurisdiction]
}

// options renders the jurisdiction select, default first — huh seeds the bound
// value from the first option, so the ordering IS the default. With nothing
// eligible the single "Home" option keeps the selector unsent.
func (s *dispatchWizardScope) options() []huh.Option[string] {
	if len(s.jurisdictions) == 0 {
		return []huh.Option[string]{huh.NewOption("Home", "")}
	}
	options := make([]huh.Option[string], 0, len(s.jurisdictions))
	for _, j := range s.jurisdictions {
		label := strings.ToUpper(j)
		if j == s.home {
			label += " (home)"
		}
		option := huh.NewOption(label, j)
		if j == s.defaultJurisdiction {
			options = slices.Insert(options, 0, option)
		} else {
			options = append(options, option)
		}
	}
	return options
}

// loadDispatchWizardScope fetches the three independent sources concurrently:
// the authenticated repo listing (falling back to sibling repos on disk, as
// before), the control-plane placements, and the home jurisdiction. Each is
// best-effort; a missing one degrades to the pre-picker behaviour.
func loadDispatchWizardScope(ctx context.Context, currentRepo string) *dispatchWizardScope {
	var (
		repos      []string
		placements map[string][]string
		home       string
		wg         sync.WaitGroup
	)
	wg.Go(func() {
		slugs, err := listDispatchWizardRepos(ctx)
		if err != nil || len(slugs) == 0 {
			slugs = discoverLocalRepoSlugs(ctx, currentRepo)
		}
		repos = slugs
	})
	wg.Go(func() {
		var err error
		if placements, err = listDispatchWizardPlacements(ctx); err != nil {
			logging.Warn(ctx, "dispatch wizard placements unavailable; offering repos unscoped", "error", err)
		}
	})
	wg.Go(func() { home = resolveDispatchWizardHome(ctx) })
	wg.Wait()
	return newDispatchWizardScope(repos, placements, home)
}

// defaultListDispatchWizardPlacements reads the caller's repo index from the
// control plane, keeping per repo the jurisdictions of its READY placements.
func defaultListDispatchWizardPlacements(ctx context.Context) (map[string][]string, error) {
	ctx, cancel := context.WithTimeout(ctx, dispatchWizardScopeTimeout)
	defer cancel()

	client, err := newCellCoreClient()
	if err != nil {
		return nil, fmt.Errorf("control plane unavailable: %w", err)
	}
	index, err := client.ListRepos(ctx, coreapi.ListReposParams{})
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller logs and degrades; no extra context to add
	}
	if index.Truncated {
		// Repos beyond the truncation point are attributed to home; the
		// --jurisdiction flag still reaches them.
		logging.Warn(ctx, "repo index truncated; dispatch wizard may attribute some repos to home")
	}
	out := make(map[string][]string, len(index.Repos))
	for _, entry := range index.Repos {
		if slug := strings.ToLower(strings.TrimSpace(entry.FullName)); slug != "" {
			out[slug] = readyPlacementJurisdictions(entry.Placements)
		}
	}
	return out, nil
}

func defaultResolveDispatchWizardHome(ctx context.Context) string {
	home, err := auth.HomeJurisdictionFromActiveLogin(ctx, false)
	if err != nil {
		logging.Debug(ctx, "dispatch wizard: home jurisdiction unavailable", "error", err)
		return ""
	}
	return home
}
