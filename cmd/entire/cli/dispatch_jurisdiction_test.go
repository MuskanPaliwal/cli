package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	dispatchpkg "github.com/entireio/cli/cmd/entire/cli/dispatch"
	"github.com/entireio/cli/internal/coreapi"
)

// These tests swap package-level seams (newCellCoreClient /
// lookupDispatchRepoJurisdictions) and therefore must not run in parallel.

func stubDispatchRepoJurisdictions(t *testing.T, fn func(context.Context, []string) map[string][]string) {
	t.Helper()
	prev := lookupDispatchRepoJurisdictions
	lookupDispatchRepoJurisdictions = fn
	t.Cleanup(func() { lookupDispatchRepoJurisdictions = prev })
}

func TestDescribeDispatchRepoNotFound_AppendsPlacementHint(t *testing.T) {
	stubDispatchRepoJurisdictions(t, func(_ context.Context, repos []string) map[string][]string {
		if len(repos) != 2 {
			t.Errorf("expected both missing repos to be looked up, got %v", repos)
		}
		return map[string][]string{
			"entirehq/ferrata": {"us"},
			"entirehq/plans":   {},
		}
	})

	err := describeDispatchRepoNotFound(context.Background(), &dispatchpkg.RepoNotFoundError{
		Jurisdiction: "au",
		Repos:        []string{"entirehq/ferrata", "entirehq/plans"},
		Message:      "repository not found: entirehq/ferrata, entirehq/plans",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "In AU: repository not found: entirehq/ferrata, entirehq/plans.") {
		t.Fatalf("expected the dispatch error to lead, got %q", msg)
	}
	if !strings.Contains(msg, "\n  entirehq/ferrata is placed in: us") {
		t.Fatalf("expected placement hint, got %q", msg)
	}
	if !strings.Contains(msg, "\n  entirehq/plans has no ready placement in any jurisdiction") {
		t.Fatalf("expected no-placement hint, got %q", msg)
	}
}

func TestDescribeDispatchRepoNotFound_LeavesErrorAloneWithoutHint(t *testing.T) {
	stubDispatchRepoJurisdictions(t, func(context.Context, []string) map[string][]string { return nil })

	original := &dispatchpkg.RepoNotFoundError{Jurisdiction: "us", Repos: []string{"a/b"}, Message: "repository not found: a/b"}
	err := describeDispatchRepoNotFound(context.Background(), original)
	var notFound *dispatchpkg.RepoNotFoundError
	if !errors.As(err, &notFound) || notFound != original {
		t.Fatalf("expected the original typed error when no hint is available, got %v", err)
	}

	other := errors.New("boom")
	if got := describeDispatchRepoNotFound(context.Background(), other); !errors.Is(got, other) {
		t.Fatalf("non-404 errors must pass through untouched, got %v", got)
	}
}

func TestDefaultLookupDispatchRepoJurisdictions_UsesControlPlanePlacements(t *testing.T) {
	fake := &fakeCellCore{repos: &coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{{
		FullName: "entirehq/ferrata",
		Placements: []coreapi.RepoPlacement{
			{ID: "p1", Jurisdiction: "US", Status: coreapi.RepoPlacementStatusReady},
			{ID: "p2", Jurisdiction: "us", Status: coreapi.RepoPlacementStatusProcessing},
			{ID: "p3", Jurisdiction: "eu", Status: coreapi.RepoPlacementStatusFailed},
			{ID: "p4", Jurisdiction: "au", Status: coreapi.RepoPlacementStatusReady},
		},
	}}}}
	withFakeCellCore(t, fake)

	got := defaultLookupDispatchRepoJurisdictions(context.Background(), []string{"entirehq/ferrata"})
	jurisdictions, ok := got["entirehq/ferrata"]
	if !ok {
		t.Fatalf("expected a placement entry, got %v", got)
	}
	if strings.Join(jurisdictions, ",") != "au,us" {
		t.Fatalf("expected deduped, sorted, non-failed jurisdictions, got %v", jurisdictions)
	}
	if filter, ok := fake.lastListReposParams.Filter.Get(); !ok || filter != "entirehq/ferrata" {
		t.Fatalf("expected an exact filtered index lookup, got %+v", fake.lastListReposParams)
	}
}

func TestDefaultLookupDispatchRepoJurisdictions_UnknownRepoIsAbsent(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{repos: &coreapi.ListReposOutputBody{Repos: []coreapi.RepoIndexEntry{{
		FullName:   "someone/else",
		Placements: []coreapi.RepoPlacement{{ID: "p1", Jurisdiction: "us", Status: coreapi.RepoPlacementStatusReady}},
	}}}})

	got := defaultLookupDispatchRepoJurisdictions(context.Background(), []string{"entirehq/ferrata"})
	if _, ok := got["entirehq/ferrata"]; ok {
		t.Fatalf("a repo the index does not name must not get a hint, got %v", got)
	}
}

func TestDefaultLookupDispatchRepoJurisdictions_ControlPlaneErrorIsSilent(t *testing.T) {
	withFakeCellCore(t, &fakeCellCore{reposErr: errors.New("core down")})

	if got := defaultLookupDispatchRepoJurisdictions(context.Background(), []string{"entirehq/ferrata"}); len(got) != 0 {
		t.Fatalf("expected no hint on control-plane error, got %v", got)
	}

	prev := newCellCoreClient
	newCellCoreClient = func() (cellCoreClient, error) { return nil, errors.New("no client") }
	t.Cleanup(func() { newCellCoreClient = prev })
	if got := defaultLookupDispatchRepoJurisdictions(context.Background(), []string{"entirehq/ferrata"}); got != nil {
		t.Fatalf("expected nil when the control plane client cannot be built, got %v", got)
	}
}
