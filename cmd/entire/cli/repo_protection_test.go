package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/internal/coreapi"
)

const testProtectionRepoULID = "01KS6KFJR2XS6PZ188MVYE07AN"

func TestExpandBranchRef(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"main":               "refs/heads/main",
		"release/*":          "refs/heads/release/*",
		"HEAD":               "HEAD",
		"refs/heads/main":    "refs/heads/main",
		"refs/heads/hotfix/": "refs/heads/hotfix/",
	}
	for in, want := range cases {
		got, err := expandBranchRef(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := expandBranchRef("")
	require.Error(t, err)
}

func TestProtectionRow(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"refs/heads/main", protectionLevelMergeOnly}, protectionRow(branchRule{Ref: "refs/heads/main", ServerSideMergeOnly: true}))
	assert.Equal(t, []string{"HEAD", protectionLevelProtected}, protectionRow(branchRule{Ref: "HEAD"}))
}

// fakeProtectionServer stands in for core's branch-protection resource. It
// applies PATCH bodies with the server's upsert-by-ref rule so a test sees the
// same resulting list the real core would return.
type fakeProtectionServer struct {
	mu       sync.Mutex
	provider string // the repo's provider as GET /repos/{id} reports it
	rules    []coreapi.BranchRule
	patches  []coreapi.UpdateBranchProtectionInputBody
	// repoGets counts GET /repos/{id}. Only the empty-list rendering needs
	// the provider, so every other path must leave this at zero — see
	// TestRepoProtection_LooksUpTheRepoOnlyForAnEmptyList.
	repoGets int
}

func (f *fakeProtectionServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/"+testProtectionRepoULID {
			f.mu.Lock()
			f.repoGets++
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if err := printJSON(w, &coreapi.Repo{
				ID: testProtectionRepoULID, Name: "web", OwningProjectId: testProjectULID,
				Provider: coreapi.NewOptString(f.provider),
			}); err != nil {
				t.Errorf("encode repo response: %v", err)
			}
			return
		}
		if r.URL.Path != "/api/v1/repos/"+testProtectionRepoULID+"/branch-protection" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
		case http.MethodPatch:
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read patch body: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body coreapi.UpdateBranchProtectionInputBody
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("decode patch body %s: %v", raw, err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			f.patches = append(f.patches, body)
			f.apply(body)
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := printJSON(w, &coreapi.BranchProtection{Rules: f.rules}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
}

func (f *fakeProtectionServer) apply(body coreapi.UpdateBranchProtectionInputBody) {
	var next []coreapi.BranchRule
	for _, r := range f.rules {
		removed := false
		for _, ref := range body.RemoveRefs {
			if ref == r.Ref {
				removed = true
			}
		}
		if removed {
			continue
		}
		for _, a := range body.AddRules {
			if a.Ref == r.Ref {
				r.ServerSideMergeOnly = a.ServerSideMergeOnly
			}
		}
		next = append(next, r)
	}
	for _, a := range body.AddRules {
		present := false
		for _, r := range next {
			if r.Ref == a.Ref {
				present = true
			}
		}
		if !present {
			next = append(next, a)
		}
	}
	f.rules = next
}

// newProtectionFixture installs the fake as the active core client.
// Not parallel: swaps the package-level activeCoreClient seam.
func newProtectionFixture(t *testing.T, rules ...coreapi.BranchRule) *fakeProtectionServer {
	t.Helper()
	fake := &fakeProtectionServer{provider: "entire", rules: rules}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	prev := activeCoreClient
	activeCoreClient = func(context.Context) (*coreapi.Client, error) {
		return coreapi.NewWithBearer(srv.URL, "tok")
	}
	t.Cleanup(func() { activeCoreClient = prev })
	return fake
}

// rulesView projects wire rules onto the comparable view; the decoded wire
// structs carry an empty AdditionalProps map that a literal does not.
func rulesView(rs []coreapi.BranchRule) []branchRule {
	return branchRulesFromWire(&coreapi.BranchProtection{Rules: rs})
}

func execRepoProtection(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	stdout, _, err = execRepoProtectionBothStreams(t, args...)
	return stdout, err
}

func execRepoProtectionBothStreams(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	parent := &cobra.Command{Use: "repo"}
	addControlPlaneFlags(parent)
	parent.AddCommand(newRepoProtectionCmd())
	var out, errOut bytes.Buffer
	parent.SetOut(&out)
	parent.SetErr(&errOut)
	parent.SetArgs(append([]string{"protection"}, args...))
	err = parent.ExecuteContext(t.Context())
	return out.String(), errOut.String(), err
}

func TestRepoProtection_ListEmpty(t *testing.T) {
	newProtectionFixture(t)
	out, err := execRepoProtection(t, "list", testProtectionRepoULID)
	require.NoError(t, err)
	assert.Equal(t, protectionEmpty+"\n", out)

	out, err = execRepoProtection(t, "list", testProtectionRepoULID, "--json")
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(out), "an empty list is a JSON array, not null")
}

// A GitHub mirror reads as empty from core, but "nothing is protected" would
// misstate it: its default branch is always protected and its rules are the
// upstream's. --json keeps the plain array on stdout so a script still parses
// it, and puts the caveat on stderr — a script concluding "no rules ⇒ nothing
// is protected" is wrong on a mirror, which is what the note exists to say.
func TestRepoProtection_ListOnMirrorExplains(t *testing.T) {
	fake := newProtectionFixture(t)
	fake.provider = repoProviderGitHub
	out, err := execRepoProtection(t, "list", testProtectionRepoULID)
	require.NoError(t, err)
	assert.Equal(t, protectionMirrorNote+"\n", out)

	out, errOut, err := execRepoProtectionBothStreams(t, "list", testProtectionRepoULID, "--json")
	require.NoError(t, err)
	assert.Equal(t, "[]", strings.TrimSpace(out), "stdout stays a bare array")
	assert.Equal(t, protectionMirrorNote+"\n", errOut, "the caveat reaches --json callers on stderr")
}

// The provider is needed only to render an empty list, so a list that has
// rules — and a --json render of one — costs a single round trip. A repo
// lookup here would also be a second way for `list` to fail after the
// branch-protection answer is already in hand.
func TestRepoProtection_LooksUpTheRepoOnlyForAnEmptyList(t *testing.T) {
	fake := newProtectionFixture(t, coreapi.BranchRule{Ref: "HEAD"})

	_, err := execRepoProtection(t, "list", testProtectionRepoULID)
	require.NoError(t, err)
	_, err = execRepoProtection(t, "list", testProtectionRepoULID, "--json")
	require.NoError(t, err)
	assert.Zero(t, fake.repoGets, "a non-empty list must not fetch the repo")

	_, err = execRepoProtection(t, "remove", testProtectionRepoULID, "HEAD")
	require.NoError(t, err)
	assert.Zero(t, fake.repoGets, "add/remove render their own result and never need the provider")

	_, err = execRepoProtection(t, "list", testProtectionRepoULID)
	require.NoError(t, err)
	assert.Equal(t, 1, fake.repoGets, "the empty rendering is the one branch that needs it")
}

func TestRepoProtection_ListShowsLevels(t *testing.T) {
	newProtectionFixture(t,
		coreapi.BranchRule{Ref: "HEAD", ServerSideMergeOnly: coreapi.NewOptBool(true)},
		coreapi.BranchRule{Ref: "refs/heads/release/*"},
	)
	out, err := execRepoProtection(t, "list", testProtectionRepoULID)
	require.NoError(t, err)
	assert.Contains(t, out, "BRANCH")
	assert.Contains(t, out, "HEAD")
	assert.Contains(t, out, protectionLevelMergeOnly)
	assert.Contains(t, out, "refs/heads/release/*")
	assert.Contains(t, out, protectionLevelProtected)

	out, err = execRepoProtection(t, "list", testProtectionRepoULID, "--json")
	require.NoError(t, err)
	var got []branchRule
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, []branchRule{{Ref: "HEAD", ServerSideMergeOnly: true}, {Ref: "refs/heads/release/*"}}, got)
}

// add expands a short branch name, sends the level as given, and re-adding a
// branch changes its level in place; remove sends only removeRefs. Each verb
// prints the resulting rules.
func TestRepoProtection_AddAndRemove(t *testing.T) {
	fake := newProtectionFixture(t, coreapi.BranchRule{Ref: "HEAD"})

	out, err := execRepoProtection(t, "add", testProtectionRepoULID, "release/*")
	require.NoError(t, err)
	require.Len(t, fake.patches, 1)
	assert.Equal(t, []branchRule{{Ref: "refs/heads/release/*"}}, rulesView(fake.patches[0].AddRules))
	assert.True(t, fake.patches[0].AddRules[0].ServerSideMergeOnly.IsSet(), "the level is always sent explicitly")
	assert.Empty(t, fake.patches[0].RemoveRefs)
	assert.Contains(t, out, "refs/heads/release/*")

	out, err = execRepoProtection(t, "add", testProtectionRepoULID, "HEAD", "--server-side-merge-only")
	require.NoError(t, err)
	require.Len(t, fake.patches, 2)
	assert.Equal(t, []branchRule{{Ref: "HEAD", ServerSideMergeOnly: true}}, rulesView(fake.patches[1].AddRules))
	assert.Contains(t, out, protectionLevelMergeOnly)
	assert.Equal(t, []branchRule{
		{Ref: "HEAD", ServerSideMergeOnly: true},
		{Ref: "refs/heads/release/*"},
	}, rulesView(fake.rules), "re-adding HEAD raised its level in place")

	out, err = execRepoProtection(t, "remove", testProtectionRepoULID, "HEAD")
	require.NoError(t, err)
	require.Len(t, fake.patches, 3)
	assert.Equal(t, []string{"HEAD"}, fake.patches[2].RemoveRefs)
	assert.Empty(t, fake.patches[2].AddRules)
	assert.NotContains(t, out, "HEAD")
	assert.Contains(t, out, "refs/heads/release/*")

	out, err = execRepoProtection(t, "remove", testProtectionRepoULID, "refs/heads/release/*")
	require.NoError(t, err)
	assert.Equal(t, protectionEmpty+"\n", out)
}

func TestRepoProtection_NameNeedsProject(t *testing.T) {
	fake := newProtectionFixture(t)
	_, err := execRepoProtection(t, "list", "web")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--project")
	assert.Empty(t, fake.patches)
}
