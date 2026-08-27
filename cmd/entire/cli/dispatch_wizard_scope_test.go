package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/entireio/cli/internal/coreapi"
)

func TestDispatchWizardScope_JurisdictionsAndRepos(t *testing.T) {
	t.Parallel()

	scope := &dispatchWizardScope{
		repos: []string{"acme/us-only", "acme/both", "acme/eu-only", "acme/unplaced"},
		placements: map[string][]string{
			"acme/us-only": {"us"},
			"acme/both":    {"eu", "us"},
			"acme/eu-only": {"eu"},
		},
		home: "au",
	}
	// Unplaced repos fall into the home bucket, so AU is eligible too.
	if got := strings.Join(scope.eligibleJurisdictions(), ","); got != "au,eu,us" {
		t.Fatalf("unexpected eligible jurisdictions: %q", got)
	}
	if !scope.hasPicker() {
		t.Fatal("expected a picker with several jurisdictions")
	}
	if got := strings.Join(scope.reposIn("us"), ","); got != "acme/us-only,acme/both" {
		t.Fatalf("expected catalogue order preserved, got %q", got)
	}
	if got := strings.Join(scope.reposIn("au"), ","); got != "acme/unplaced" {
		t.Fatalf("expected placement-less repos under home, got %q", got)
	}
	if got := strings.Join(scope.reposIn(""), ","); got != strings.Join(scope.repos, ",") {
		t.Fatalf("no jurisdiction must offer everything, got %q", got)
	}
	if got := scope.defaultJurisdiction(); got != "au" {
		t.Fatalf("home is eligible and must win, got %q", got)
	}
}

func TestDispatchWizardScope_DefaultsWithoutHomeRepos(t *testing.T) {
	t.Parallel()

	scope := &dispatchWizardScope{
		repos:      []string{"a/one", "a/two", "a/three"},
		placements: map[string][]string{"a/one": {"us"}, "a/two": {"eu"}, "a/three": {"eu"}},
		home:       "au",
	}
	if got := scope.defaultJurisdiction(); got != "eu" {
		t.Fatalf("expected the busiest jurisdiction when home has no repos, got %q", got)
	}

	// Home unknown and no placement data: nothing to pick, route to home.
	unscoped := &dispatchWizardScope{repos: []string{"a/one"}}
	if unscoped.hasPicker() || unscoped.defaultJurisdiction() != "" {
		t.Fatalf("expected no picker and no default, got %v / %q", unscoped.hasPicker(), unscoped.defaultJurisdiction())
	}
	if got := strings.Join(unscoped.reposIn(""), ","); got != "a/one" {
		t.Fatalf("unscoped catalogue must still offer repos, got %q", got)
	}

	// A single jurisdiction is pre-selected without a picker.
	single := &dispatchWizardScope{repos: []string{"a/one"}, placements: map[string][]string{"a/one": {"us"}}}
	if single.hasPicker() || single.defaultJurisdiction() != "us" {
		t.Fatalf("expected the sole jurisdiction as default, got %v / %q", single.hasPicker(), single.defaultJurisdiction())
	}
}

func TestDispatchWizardState_CloudReposRestrictedToJurisdiction(t *testing.T) {
	t.Parallel()

	state := newDispatchWizardState()
	state.modeChoice = dispatchWizardModeServer
	state.scope = &dispatchWizardScope{
		repos:      []string{"a/us", "a/eu"},
		placements: map[string][]string{"a/us": {"us"}, "a/eu": {"eu"}},
	}
	state.selectedRepos = []string{"a/us", "a/eu"}
	state.jurisdiction = "eu"

	opts, err := state.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(opts.RepoPaths, ","); got != "a/eu" {
		t.Fatalf("a repo from another jurisdiction must be dropped, got %q", got)
	}
	if opts.Jurisdiction != "eu" {
		t.Fatalf("expected jurisdiction to propagate, got %q", opts.Jurisdiction)
	}
}

func TestDefaultListDispatchWizardPlacements_ReadyOnlyKeyedBySlug(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{repos: &coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{
		{FullName: "Acme/Widget", Placements: []coreapi.RepoPlacement{
			{ID: "p1", Jurisdiction: "US", Status: coreapi.RepoPlacementStatusReady},
			{ID: "p2", Jurisdiction: "eu", Status: coreapi.RepoPlacementStatusProcessing},
		}},
		{FullName: "", Placements: []coreapi.RepoPlacement{{ID: "p3", Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady}}},
	}}})

	got, err := defaultListDispatchWizardPlacements(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || strings.Join(got["acme/widget"], ",") != "us" {
		t.Fatalf("expected ready placements keyed by lowercased slug, got %v", got)
	}
}

func TestLoadDispatchWizardScope_DegradesPerSource(t *testing.T) {
	stubDispatchWizardScopeSources(t, []string{"a/one"}, nil, errors.New("core down"), "au")

	scope := loadDispatchWizardScope(context.Background(), t.TempDir())
	if strings.Join(scope.repos, ",") != "a/one" || scope.placements != nil || scope.home != "au" {
		t.Fatalf("expected repos and home with placements degraded, got %+v", scope)
	}
	if scope.hasPicker() {
		t.Fatal("no placement data must mean no picker")
	}
}

// stubDispatchWizardScopeSources swaps the wizard's three catalogue seams.
// Not parallel-safe: the seams are package globals.
func stubDispatchWizardScopeSources(t *testing.T, repos []string, placements map[string][]string, placementsErr error, home string) {
	t.Helper()
	oldRepos, oldPlacements, oldHome := listDispatchWizardRepos, listDispatchWizardPlacements, resolveDispatchWizardHome
	listDispatchWizardRepos = func(context.Context) ([]string, error) { return repos, nil }
	listDispatchWizardPlacements = func(context.Context) (map[string][]string, error) { return placements, placementsErr }
	resolveDispatchWizardHome = func(context.Context) string { return home }
	t.Cleanup(func() {
		listDispatchWizardRepos, listDispatchWizardPlacements, resolveDispatchWizardHome = oldRepos, oldPlacements, oldHome
	})
}
