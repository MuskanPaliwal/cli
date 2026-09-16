package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// The picker's pool is the owning org's membership minus whoever already holds
// the target. These tests drive it through cobra against an httptest control
// plane, with the form seam swapped: `go test` is non-interactive, so the real
// forms are unreachable and the refusing paths are what run by default.

const (
	pickerOrgULID  = "01HZX7QABCDEFGHJKMNPQRSTW0"
	pickerProjULID = "01HZX7QABCDEFGHJKMNPQRSTW1"
	pickerRepoULID = "01HZX7QABCDEFGHJKMNPQRSTW2"
)

// pickerFixture is one control plane's answers: the org behind the target, its
// members, and who already holds the target.
type pickerFixture struct {
	ownerType coreapi.ProjectOwnerType
	members   []coreapi.Membership
	held      []string // grantee ULIDs holding the target directly
	// viaProject holds the grantees a repo carries through its project. Listing
	// returns them alongside the direct rows, and the pool must subtract both.
	viaProject []string
}

func member(handle, accountID string) coreapi.Membership {
	return coreapi.Membership{
		AccountId: accountID,
		Handle:    coreapi.NewOptString(handle),
		Provider:  coreapi.NewOptString(providerGitHub),
		Role:      "member",
		Status:    "active",
	}
}

func (f pickerFixture) projectGrants() []coreapi.ProjectGrant {
	rows := make([]coreapi.ProjectGrant, 0, len(f.held))
	for _, id := range f.held {
		rows = append(rows, coreapi.ProjectGrant{GranteeId: id, GranteeType: granteeTypeAccount, Role: "writer", Source: "direct"})
	}
	return rows
}

func (f pickerFixture) repoGrants() []coreapi.RepoGrant {
	rows := make([]coreapi.RepoGrant, 0, len(f.held)+len(f.viaProject))
	for _, id := range f.held {
		rows = append(rows, coreapi.RepoGrant{GranteeId: id, GranteeType: granteeTypeAccount, Role: "writer", Source: "direct"})
	}
	for _, id := range f.viaProject {
		rows = append(rows, coreapi.RepoGrant{GranteeId: id, GranteeType: granteeTypeAccount, Role: "writer", Source: "project:widgets"})
	}
	return rows
}

// pickerServer serves every lookup the picker makes, and records grant POSTs as
// "<path> <providerUserId> <role>". grantStatus, when set, chooses the status
// code for the nth grant so a mid-walk failure can be staged.
//
// Failures are reported with t.Errorf rather than require, which must not be
// called from a handler goroutine.
func pickerServer(t *testing.T, f pickerFixture, grants *[]string, grantStatus func(i int) int) *httptest.Server {
	t.Helper()
	ownerType := f.ownerType
	if ownerType == "" {
		ownerType = coreapi.ProjectOwnerTypeOrg
	}
	write := func(w http.ResponseWriter, code int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		if err := printJSON(w, payload); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				ProviderUserID string `json:"providerUserId"`
				Role           string `json:"role"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode grant body: %v", err)
				return
			}
			i := len(*grants)
			*grants = append(*grants, r.URL.Path+" "+body.ProviderUserID+" "+body.Role)
			// Anything from 400 up is the staged failure; every other answer
			// is the ordinary 201 the grant routes return.
			if grantStatus != nil {
				if code := grantStatus(i); code >= http.StatusBadRequest {
					w.WriteHeader(code)
					return
				}
			}
			write(w, http.StatusCreated, map[string]string{"status": "ok"})
			return
		}
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/identity/handles/"):
			// The provider user id is derived from the handle so a test can
			// tell the grants apart by who they were for.
			seg := strings.Split(strings.Trim(path, "/"), "/")
			handle := seg[len(seg)-1]
			write(w, http.StatusOK, &coreapi.ResolvedIdentity{
				AccountId: "acct-" + handle, Provider: providerGitHub,
				Handle: handle, ProviderUserId: "uid-" + handle,
			})
		case strings.HasSuffix(path, "/projects"):
			// The by-name project lookup behind a /et/<project>/<repo> ref.
			write(w, http.StatusOK, &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{
				ID: pickerProjULID, Name: "widgets", OwnerId: pickerOrgULID, OwnerType: ownerType,
			})})
		case strings.HasSuffix(path, "/repos"):
			write(w, http.StatusOK, &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{
				ID: pickerRepoULID, Name: "web", OwningProjectId: pickerProjULID,
			})})
		case strings.HasSuffix(path, "/repos/"+pickerRepoULID):
			write(w, http.StatusOK, &coreapi.Repo{ID: pickerRepoULID, Name: "web", OwningProjectId: pickerProjULID})
		case strings.HasSuffix(path, "/projects/"+pickerProjULID):
			write(w, http.StatusOK, &coreapi.Project{ID: pickerProjULID, Name: "widgets", OwnerId: pickerOrgULID, OwnerType: ownerType})
		case strings.HasSuffix(path, "/members") && strings.Contains(path, "/orgs/"):
			write(w, http.StatusOK, &coreapi.ListOrgMembersOutputBody{Members: f.members})
		case strings.HasSuffix(path, "/members"):
			write(w, http.StatusOK, &coreapi.ListProjectMembersOutputBody{Members: f.projectGrants()})
		case strings.HasSuffix(path, "/grants"):
			write(w, http.StatusOK, &coreapi.ListRepoGrantsOutputBody{Grants: f.repoGrants()})
		default:
			t.Errorf("unexpected GET %s", path)
		}
	}))
}

// capturePicker puts the command on the interactive path and swaps the form
// seam, recording what was offered and answering with the given selections.
//
// ENTIRE_TEST_TTY=1 is what gets past the gate: `go test` is non-interactive, so
// without it every one of these commands refuses before reaching a picker. The
// forms themselves never run, because the seam replaces them.
func capturePicker(t *testing.T, answer func(offered []grantCandidate, known []string, fixedRole string) ([]grantSelection, error)) *[]grantCandidate {
	t.Helper()
	t.Setenv("ENTIRE_TEST_TTY", "1")
	var offered []grantCandidate
	prev := grantPicker
	grantPicker = func(_ *cobra.Command, _ grantPickerTarget, candidates []grantCandidate, known []string, fixedRole string) ([]grantSelection, error) {
		offered = candidates
		return answer(candidates, known, fixedRole)
	}
	t.Cleanup(func() { grantPicker = prev })
	return &offered
}

func handles(cs []grantCandidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.handle
	}
	return out
}

// TestGrantPicker_PoolSubtractsExistingAccess is the core of the picker: the
// offered set is the owning org's members minus whoever already holds the
// target. For a repo that subtraction must also remove the holders who arrived
// through the project, since project access reaches the project's repos and
// offering it again would grant a no-op.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_PoolSubtractsExistingAccess(t *testing.T) {
	members := []coreapi.Membership{
		member("github:alice", "acct-a"),
		member("github:bob", "acct-b"),
		member("github:carol", "acct-c"),
	}
	for name, tc := range map[string]struct {
		newCmd  func() *cobra.Command
		ref     string
		fixture pickerFixture
		want    []string
	}{
		"project/direct holder excluded": {
			newProjectGrantCmd, pickerProjULID,
			pickerFixture{members: members, held: []string{"acct-b"}},
			[]string{"github:alice", "github:carol"},
		},
		"repo/direct holder excluded": {
			newRepoGrantCmd, wiringRepoPath,
			pickerFixture{members: members, held: []string{"acct-a"}},
			[]string{"github:bob", "github:carol"},
		},
		"repo/project-inherited holder excluded": {
			newRepoGrantCmd, wiringRepoPath,
			pickerFixture{members: members, viaProject: []string{"acct-c"}},
			[]string{"github:alice", "github:bob"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			var grants []string
			srv := pickerServer(t, tc.fixture, &grants, nil)
			t.Cleanup(srv.Close)
			offered := capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
				return []grantSelection{{handle: cs[0].handle, role: "reader"}}, nil
			})

			_, _, err := runPickerCmd(t, tc.newCmd, srv.URL, tc.ref)
			require.NoError(t, err)
			require.Equal(t, tc.want, handles(*offered))
		})
	}
}

// TestGrantPicker_UngrantableMembersAreDropped: a member with no handle or one
// who has not joined cannot be resolved to the (provider, providerUserId) pair
// the grant routes need, so offering them would produce a selection that fails
// at the grant.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_UngrantableMembersAreDropped(t *testing.T) {
	noHandle := member("", "acct-x")
	noHandle.Handle = coreapi.OptString{}
	invited := member("github:pending", "acct-y")
	invited.Status = "invited"

	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), noHandle, invited,
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	offered := capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		return []grantSelection{{handle: cs[0].handle, role: "reader"}}, nil
	})

	_, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.Equal(t, []string{"github:alice"}, handles(*offered))
}

// TestGrantPicker_PerGranteeRoles pins that each selection carries its own role
// rather than one role being applied to the set, and that the grants are issued
// in the order chosen.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_PerGranteeRoles(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	capturePicker(t, func(_ []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		return []grantSelection{
			{handle: "github:alice", role: "reader"},
			{handle: "github:bob", role: "admin"},
		}, nil
	})

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.Equal(t, []string{
		"/api/v1/projects/" + pickerProjULID + "/grants uid-alice reader",
		"/api/v1/projects/" + pickerProjULID + "/grants uid-bob admin",
	}, grants)
	require.Contains(t, out, "✓ Granted github:alice reader access to project "+pickerProjULID)
	require.Contains(t, out, "✓ Granted github:bob admin access to project "+pickerProjULID)
}

// TestGrantPicker_FixedRoleIsNotPrompted: --role decides the roles, so the
// picker is handed it rather than asked for one, and every grant carries it.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_FixedRoleIsNotPrompted(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, nil)
	t.Cleanup(srv.Close)
	var gotFixed string
	capturePicker(t, func(cs []grantCandidate, _ []string, fixedRole string) ([]grantSelection, error) {
		gotFixed = fixedRole
		out := make([]grantSelection, len(cs))
		for i, c := range cs {
			out[i] = grantSelection{handle: c.handle, role: fixedRole}
		}
		return out, nil
	})

	_, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--role", "admin")
	require.NoError(t, err)
	require.Equal(t, "admin", gotFixed)
	require.Equal(t, []string{
		"/api/v1/projects/" + pickerProjULID + "/grants uid-alice admin",
		"/api/v1/projects/" + pickerProjULID + "/grants uid-bob admin",
	}, grants)
}

// TestGrantPicker_PartialFailureStopsAndReports: a failure part-way through
// leaves the earlier grants in place and says so, rather than rolling back a
// server-side change the CLI cannot undo or swallowing the error.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_PartialFailureStopsAndReports(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"), member("github:carol", "acct-c"),
	}}, &grants, func(i int) int {
		if i == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	t.Cleanup(srv.Close)
	capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		out := make([]grantSelection, len(cs))
		for i, c := range cs {
			out[i] = grantSelection{handle: c.handle, role: "reader"}
		}
		return out, nil
	})

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.Error(t, err)
	require.Contains(t, out, "github:alice")
	require.NotContains(t, out, "github:carol", "the walk must stop at the failure, not carry on")
	require.Len(t, grants, 2, "the third grant is never attempted")
}

// TestGrantPicker_PartialFailureIsReportedInJSONToo: --json replaces the
// confirmation lines, so without this the grants that landed before a failure
// would be invisible to exactly the callers parsing the output. The shape stays
// the array a picked set always produces, carrying only what succeeded.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_PartialFailureIsReportedInJSONToo(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
		member("github:alice", "acct-a"), member("github:bob", "acct-b"),
	}}, &grants, func(i int) int {
		if i == 1 {
			return http.StatusInternalServerError
		}
		return http.StatusCreated
	})
	t.Cleanup(srv.Close)
	capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		out := make([]grantSelection, len(cs))
		for i, c := range cs {
			out[i] = grantSelection{handle: c.handle, role: "reader"}
		}
		return out, nil
	})

	out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--json")
	require.Error(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &arr))
	require.Len(t, arr, 1, "the one grant that landed is reported, the failed one is not")
}

// TestGrantPicker_NoCandidatesCases pins one message per way the picker cannot
// run. They are separate branches on purpose: "every member already has access"
// is a claim about members that must not be printed when there are none, and an
// account-owned project is the absence of a pool rather than an empty one.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_NoCandidatesCases(t *testing.T) {
	for name, tc := range map[string]struct {
		newCmd  func() *cobra.Command
		ref     string
		fixture pickerFixture
		want    string
	}{
		"org has no members": {
			newProjectGrantCmd, pickerProjULID,
			pickerFixture{},
			"has no org members to choose from",
		},
		"every member already has access": {
			newProjectGrantCmd, pickerProjULID,
			pickerFixture{members: []coreapi.Membership{member("github:alice", "acct-a")}, held: []string{"acct-a"}},
			"every member of the org owning project " + pickerProjULID + " already has access to it",
		},
		"project owned by an account": {
			newProjectGrantCmd, pickerProjULID,
			pickerFixture{ownerType: coreapi.ProjectOwnerTypeAccount},
			"project " + pickerProjULID + " is owned by an account, so it has no member list to choose from",
		},
		"repo whose project is owned by an account": {
			newRepoGrantCmd, wiringRepoPath,
			pickerFixture{ownerType: coreapi.ProjectOwnerTypeAccount},
			"repo " + wiringRepoPath + " is in project widgets, which is owned by an account",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var grants []string
			srv := pickerServer(t, tc.fixture, &grants, nil)
			t.Cleanup(srv.Close)
			capturePicker(t, func([]grantCandidate, []string, string) ([]grantSelection, error) {
				t.Error("picker must not open when there is nobody to offer")
				return nil, nil
			})

			_, _, err := runPickerCmd(t, tc.newCmd, srv.URL, tc.ref)
			require.ErrorContains(t, err, tc.want)
			// Every refusal still names the form that always works, because a
			// grantee never has to be an org member.
			require.ErrorContains(t, err, "pass a grantee as provider:handle")
			require.Empty(t, grants)
		})
	}
}

// TestGrantAdd_NoGranteeIsRefusedBeforeAnyRequest: without a terminal the
// candidate list has no use, so the refusal is decided from the command line
// alone and costs no lookup.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestGrantAdd_NoGranteeIsRefusedBeforeAnyRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	for name, tc := range map[string]struct {
		newCmd func() *cobra.Command
		ref    string
	}{
		"project": {newProjectGrantCmd, wiringProjULID},
		"repo":    {newRepoGrantCmd, wiringRepoPath},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCoreCmd(t, tc.newCmd, srv.URL, "add", tc.ref)
			require.ErrorContains(t, err, "no grantee given; pass a grantee as provider:handle")
		})
	}
}

// TestOrgGrantAdd_HasNoPicker: org membership has no enumerable pool of
// candidates — everyone not already a member is, by definition, absent from the
// only list there is — so `org grant add` still requires both arguments.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestOrgGrantAdd_HasNoPicker(t *testing.T) {
	require.Nil(t, orgGrantTarget.candidates)

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runCoreCmd(t, newOrgGrantCmd, srv.URL, "add", wiringOrgULID)
	require.ErrorContains(t, err, "accepts 2 arg(s), received 1")
}

// runPickerCmd runs `add <ref> [flags]` — the grantee-omitted form — against srv.
func runPickerCmd(t *testing.T, newCmd func() *cobra.Command, srvURL, ref string, extra ...string) (stdout, stderr string, err error) {
	t.Helper()
	return runCoreCmd(t, newCmd, srvURL, append([]string{"add", ref}, extra...)...)
}

// TestGrantPicker_SoleCandidateIsStillOffered: a picker elsewhere in the CLI
// auto-picks when only one choice exists (selectPlacement returns the lone
// cluster without prompting), and that is right for choosing where to read
// from. This one writes access, so the single eligible person is still shown
// and still has to be chosen.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantPicker_SoleCandidateIsStillOffered(t *testing.T) {
	var grants []string
	srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{member("github:alice", "acct-a")}}, &grants, nil)
	t.Cleanup(srv.Close)
	opened := false
	capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
		opened = true
		require.Len(t, cs, 1)
		return []grantSelection{{handle: cs[0].handle, role: "reader"}}, nil
	})

	_, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID)
	require.NoError(t, err)
	require.True(t, opened, "the lone candidate must be chosen, not assumed")
}

// TestGrantAdd_JSONShapeFollowsTheRequest: one grantee named on the command
// line stays the single object callers already parse; a picked set is an array,
// one entry per grant. The shape tracks how many grants were asked for.
//
// Not parallel: swaps the activeCoreClient and grantPicker seams.
func TestGrantAdd_JSONShapeFollowsTheRequest(t *testing.T) {
	t.Run("argument form is an object", func(t *testing.T) {
		var grants []string
		srv := pickerServer(t, pickerFixture{}, &grants, nil)
		t.Cleanup(srv.Close)

		out, _, err := runCoreCmd(t, newProjectGrantCmd, srv.URL,
			"add", pickerProjULID, "github:alice", "--role", "reader", "--json")
		require.NoError(t, err)
		var obj map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &obj))
		require.NotEmpty(t, grants)
	})

	t.Run("picked set is an array", func(t *testing.T) {
		var grants []string
		srv := pickerServer(t, pickerFixture{members: []coreapi.Membership{
			member("github:alice", "acct-a"), member("github:bob", "acct-b"),
		}}, &grants, nil)
		t.Cleanup(srv.Close)
		capturePicker(t, func(cs []grantCandidate, _ []string, _ string) ([]grantSelection, error) {
			out := make([]grantSelection, len(cs))
			for i, c := range cs {
				out[i] = grantSelection{handle: c.handle, role: "reader"}
			}
			return out, nil
		})

		out, _, err := runPickerCmd(t, newProjectGrantCmd, srv.URL, pickerProjULID, "--json")
		require.NoError(t, err)
		var arr []map[string]any
		require.NoError(t, json.Unmarshal([]byte(out), &arr))
		require.Len(t, arr, 2)
		// The confirmation lines are the human rendering; --json replaces them.
		require.NotContains(t, out, "✓")
	})
}
