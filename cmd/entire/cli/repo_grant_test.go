package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

// The project and repo the native lookups below resolve to.
const (
	repoGrantProjULID = "01HZX7QABCDEFGHJKMNPQRSTAA"
	repoGrantRepoULID = "01HZX7QABCDEFGHJKMNPQRSTAB"
)

// grantActiveCoreServer serves what `repo grant` asks the active context's
// core: the project and repo by-name lookups behind a /et/<project>/<repo>
// ref and that repo's grants, plus the placement lookup a mirror ref needs
// before its collaborators can be read on the cluster that holds it.
// placements is what that lookup returns, so a test can make a repo mirrored
// anywhere or nowhere. Every request path is recorded, so a test can assert
// that a command which should not reach the control plane made no call at all.
func grantActiveCoreServer(t *testing.T, paths *[]string, placements ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		var payload any
		switch {
		case strings.HasSuffix(r.URL.Path, "/mirrors/placements"):
			resolved := make([]coreapi.ResolvedPlacement, 0, len(placements))
			for i, host := range placements {
				resolved = append(resolved, coreapi.ResolvedPlacement{ClusterHost: host, MirrorId: fmt.Sprintf("01MIRROR%d", i)})
			}
			payload = &coreapi.ResolvePlacementsOutputBody{Placements: resolved}
		case strings.HasSuffix(r.URL.Path, "/grants"):
			payload = &coreapi.ListRepoGrantsOutputBody{Grants: []coreapi.RepoGrant{{
				GranteeId: "01ACCT", GranteeName: coreapi.NewOptString("github:alice"),
				GranteeType: granteeTypeAccount, Role: "writer", Source: "repo",
			}}}
		case strings.HasSuffix(r.URL.Path, "/repos"):
			payload = &coreapi.ListProjectReposOutputBody{Repo: coreapi.NewOptRepo(coreapi.Repo{ID: repoGrantRepoULID, Name: "web"})}
		case strings.HasSuffix(r.URL.Path, "/projects"):
			payload = &coreapi.ListProjectsOutputBody{Project: coreapi.NewOptProject(coreapi.Project{ID: repoGrantProjULID, Name: "acme", OwnerId: "01HZX7QABCDEFGHJKMNPQRSTAC", OwnerType: coreapi.ProjectOwnerTypeOrg})}
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		writeJSONResponse(t, w, http.StatusOK, payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRepoGrantList_AnswersBothForges pins the verb's grammar: one question,
// two sources. A native ref lists the repo's grants from the control plane; a
// mirror ref lists its upstream collaborators, read on a cluster the repo is
// mirrored on.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_AnswersBothForges(t *testing.T) {
	var clusterHosts []string
	mirror := newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/mirrors/collaborators") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		clusterHosts = append(clusterHosts, r.URL.Query().Get("clusterHost"))
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{
			Collaborators: []coreapi.MirrorCollaborator{{Handle: coreapi.NewOptString("github:alice"), Role: "reader", AccountId: "01ACCT"}},
		})
	})
	seamClusterCoreClient(t, mirror)
	var nativePaths []string
	srv := grantActiveCoreServer(t, &nativePaths, defaultClusterHost)

	t.Run("a native ref lists the repo's grants", func(t *testing.T) {
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/et/acme/web")
		require.NoError(t, err)
		require.Contains(t, stdout, "GRANTEE")
		require.Contains(t, stdout, "github:alice")
		require.Contains(t, stdout, "writer")
		require.Contains(t, nativePaths, "/api/v1/repos/"+repoGrantRepoULID+"/grants")
		require.Empty(t, clusterHosts, "a native repo is not a placement")
	})

	t.Run("a mirror ref lists the upstream collaborators", func(t *testing.T) {
		clusterHosts = nil
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.NoError(t, err)
		require.Contains(t, stdout, "GRANTEE")
		require.Contains(t, stdout, "github:alice")
		require.Equal(t, []string{defaultClusterHost}, clusterHosts,
			"the repo's placement, which the caller never names")
	})

	t.Run("no region flag to pick with", func(t *testing.T) {
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget", "--cluster", "eu.example")
		require.ErrorContains(t, err, "unknown flag: --cluster")
	})
}

// TestRepoGrantWrite_RefusesAMirrorRef pins that the write verbs' accepted
// grammar stays the native path alone: a mirror's access is the upstream
// GitHub repository's, so it is refused before any request, and the refusal
// names the read path that does answer for a mirror.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoGrantWrite_RefusesAMirrorRef(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"add", []string{"add", "/gh/acme/widget", "github:alice", "--role", "reader"}},
		// The ref decides before the flags do: a missing --role must not be
		// what the user is sent to fix on a repo this verb cannot write.
		{"add without a role", []string{"add", "/gh/acme/widget", "github:alice"}},
		{"remove", []string{"remove", "/gh/acme/widget", "github:alice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			srv := grantActiveCoreServer(t, &paths)
			_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, tc.args...)
			require.ErrorContains(t, err, "is a GitHub mirror")
			require.ErrorContains(t, err, "manage collaborators on GitHub")
			require.ErrorContains(t, err, "entire repo grant list /gh/acme/widget")
			require.ErrorContains(t, err, "shows who has access to the repo")
			require.Empty(t, paths, "a ref the verb cannot act on costs no round trip")
		})
	}
}

// TestRepoGrantList_PlacementLookupIsAHint pins that the placement lookup can
// never veto the read. It is pull-gated; the collaborator endpoint runs a live
// GitHub-admin check — so a caller who cannot pull the mirror, but can be asked
// about its upstream, is still answered, on the default cluster.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_PlacementLookupIsAHint(t *testing.T) {
	var clusterHosts []string
	seamClusterCoreClient(t, newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		clusterHosts = append(clusterHosts, r.URL.Query().Get("clusterHost"))
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{
			Collaborators: []coreapi.MirrorCollaborator{{Handle: coreapi.NewOptString("github:alice"), Role: "reader", AccountId: "01ACCT"}},
		})
	}))

	t.Run("a lookup that resolves nothing", func(t *testing.T) {
		clusterHosts = nil
		var paths []string
		srv := grantActiveCoreServer(t, &paths) // no placements
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.NoError(t, err)
		require.Contains(t, stdout, "github:alice")
		require.Equal(t, []string{defaultClusterHost}, clusterHosts)
	})

	t.Run("a placement naming no dialable host", func(t *testing.T) {
		clusterHosts = nil
		var paths []string
		srv := grantActiveCoreServer(t, &paths, "https://eu.example/mirrors")
		stdout, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
		require.NoError(t, err)
		require.Contains(t, stdout, "github:alice")
		require.Equal(t, []string{defaultClusterHost}, clusterHosts)
	})
}

// TestRepoGrantList_ReadsAPlacementTheRepoHas pins that the region asked is one
// the repo is actually mirrored on, not a fixed default: every placement
// materializes the same upstream collaborators, so the caller names none.
//
// Not parallel: swaps the package-level core-client seams.
func TestRepoGrantList_ReadsAPlacementTheRepoHas(t *testing.T) {
	var clusterHosts []string
	seamClusterCoreClient(t, newMirrorRequestClient(t, func(w http.ResponseWriter, r *http.Request) {
		clusterHosts = append(clusterHosts, r.URL.Query().Get("clusterHost"))
		writeJSONResponse(t, w, http.StatusOK, &coreapi.ListMirrorCollaboratorsOutputBody{})
	}))
	var paths []string
	srv := grantActiveCoreServer(t, &paths, "eu.example")
	_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, "list", "/gh/acme/widget")
	require.NoError(t, err)
	require.Equal(t, []string{"eu.example"}, clusterHosts)
}

// TestRepoGrant_RefErrorsMatchTheAcceptedGrammar pins that each verb's ref
// error names the refs that verb takes: `list` takes both forges, so a bare
// pair is offered both readings, while `add` names the native path alone.
//
// Not parallel: runCoreCmd swaps the package-level activeCoreClient seam.
func TestRepoGrant_RefErrorsMatchTheAcceptedGrammar(t *testing.T) {
	var paths []string
	srv := grantActiveCoreServer(t, &paths)
	run := func(args ...string) error {
		_, _, err := runCoreCmd(t, newRepoGrantCmd, srv.URL, args...)
		return err
	}

	err := run("list", "acme/widget")
	require.ErrorContains(t, err, "must name its forge")
	require.ErrorContains(t, err, "/et/acme/widget")
	require.ErrorContains(t, err, "/gh/acme/widget")

	err = run("list", "https://github.com/acme/widget.git")
	require.ErrorContains(t, err, "pass GitHub repositories as /gh/acme/widget")

	err = run("add", "acme/widget", "github:alice", "--role", "reader")
	require.ErrorContains(t, err, "must be a /et/<project>/<repo> path")

	require.Empty(t, paths, "a ref that names no repo costs no round trip")
}

func TestMirrorCollaboratorRow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   coreapi.MirrorCollaborator
		want []string
	}{
		{
			name: "resolved handle",
			in:   coreapi.MirrorCollaborator{AccountId: "01ACCT", Handle: coreapi.NewOptString("github:alice"), Role: "writer"},
			want: []string{"github:alice", "writer"},
		},
		{
			name: "no handle falls back to the account",
			in:   coreapi.MirrorCollaborator{AccountId: "01ACCT", Role: "reader"},
			want: []string{"01ACCT", "reader"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, mirrorCollaboratorRow(tt.in))
			require.Len(t, tt.want, len(mirrorCollaboratorColumns))
		})
	}
}
