package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/auth"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/internal/coreapi"
)

// dispatchWizardScopeTimeout bounds the control-plane repo index read behind
// the wizard's jurisdiction picker — the same budget code search gives the
// same call. Loaded in the background from wizard start, so it rarely blocks.
const dispatchWizardScopeTimeout = 10 * time.Second

// Seams for the wizard's cloud catalogue, swapped in tests.
var (
	listDispatchWizardPlacements = defaultListDispatchWizardPlacements
	resolveDispatchWizardHome    = defaultResolveDispatchWizardHome
)

// dispatchWizardScope is the wizard's view of where the caller's repos live,
// mirroring the web app's jurisdiction picker: a dispatch covers repos placed
// in exactly one jurisdiction, so the form asks for the jurisdiction first and
// offers only the repos placed there, making a mixed selection unrepresentable.
type dispatchWizardScope struct {
	// repos are the offerable slugs (repos with checkpoints), recent-first.
	repos []string
	// placements maps a lowercased slug to the sorted jurisdictions of its
	// READY placements. A nil map means the control plane could not be asked;
	// a missing key means the index did not name the repo. Either way the repo
	// is attributed to home, which is where the gateway routes when no
	// selector is sent.
	placements map[string][]string
	// home is the caller's home jurisdiction slug, "" when unknown.
	home string
}

// repoJurisdictions lists the jurisdictions a repo is offered in.
func (s *dispatchWizardScope) repoJurisdictions(slug string) []string {
	if js, ok := s.placements[strings.ToLower(strings.TrimSpace(slug))]; ok && len(js) > 0 {
		return js
	}
	return []string{s.home}
}

// eligibleJurisdictions are the jurisdictions with at least one offerable
// repo, sorted. The "" bucket (placement-less repos with home unknown) is not
// a jurisdiction a user could pick and is never offered.
func (s *dispatchWizardScope) eligibleJurisdictions() []string {
	seen := make(map[string]struct{})
	for _, slug := range s.repos {
		for _, j := range s.repoJurisdictions(slug) {
			if j != "" {
				seen[j] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for j := range seen {
		out = append(out, j)
	}
	sort.Strings(out)
	return out
}

// reposIn lists the offerable repos in a jurisdiction, preserving the
// catalogue order. An empty jurisdiction (no picker) offers every repo.
func (s *dispatchWizardScope) reposIn(jurisdiction string) []string {
	jurisdiction = strings.TrimSpace(jurisdiction)
	if jurisdiction == "" {
		return s.repos
	}
	out := make([]string, 0, len(s.repos))
	for _, slug := range s.repos {
		for _, j := range s.repoJurisdictions(slug) {
			if j == jurisdiction {
				out = append(out, slug)
				break
			}
		}
	}
	return out
}

// hasPicker reports whether there is a choice to make at all.
func (s *dispatchWizardScope) hasPicker() bool {
	return len(s.eligibleJurisdictions()) > 1
}

// defaultJurisdiction picks the pre-selected jurisdiction: home when the
// caller has repos there, else the jurisdiction holding the most repos (ties
// alphabetical). "" when nothing is eligible, which leaves routing to home.
func (s *dispatchWizardScope) defaultJurisdiction() string {
	eligible := s.eligibleJurisdictions()
	if len(eligible) == 0 {
		return ""
	}
	best := ""
	for _, j := range eligible {
		if j == s.home {
			return j
		}
		if best == "" || len(s.reposIn(j)) > len(s.reposIn(best)) {
			best = j
		}
	}
	return best
}

// loadDispatchWizardScope assembles the scope from three independent sources
// fetched concurrently: the authenticated repo listing (falling back to sibling
// repos on disk, as before), the control-plane placements, and the home
// jurisdiction. Every source is best-effort; a missing one degrades to the
// pre-picker behaviour rather than blocking the wizard.
func loadDispatchWizardScope(ctx context.Context, currentRepo string) *dispatchWizardScope {
	scope := &dispatchWizardScope{}
	var wg sync.WaitGroup
	wg.Go(func() {
		slugs, err := listDispatchWizardRepos(ctx)
		if err != nil || len(slugs) == 0 {
			slugs = discoverLocalRepoSlugs(ctx, currentRepo)
		}
		scope.repos = slugs
	})
	wg.Go(func() {
		placements, err := listDispatchWizardPlacements(ctx)
		if err != nil {
			logging.Warn(ctx, "dispatch wizard placements unavailable; offering repos unscoped", "error", err)
			return
		}
		scope.placements = placements
	})
	wg.Go(func() {
		scope.home = resolveDispatchWizardHome(ctx)
	})
	wg.Wait()
	return scope
}

// defaultListDispatchWizardPlacements reads the caller's repo index from the
// control plane and keeps, per repo, the jurisdictions of its READY placements
// — the only ones a cell can generate a dispatch from.
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
		slug := strings.ToLower(strings.TrimSpace(entry.FullName))
		if slug == "" {
			continue
		}
		out[slug] = readyPlacementJurisdictions(entry.Placements)
	}
	return out, nil
}

func defaultResolveDispatchWizardHome(ctx context.Context) string {
	home, err := auth.HomeJurisdiction(ctx, false)
	if err != nil {
		logging.Debug(ctx, "dispatch wizard: home jurisdiction unavailable", "error", err)
		return ""
	}
	return home
}
