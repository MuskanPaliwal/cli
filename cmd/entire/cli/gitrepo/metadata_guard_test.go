package gitrepo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestGitMetadataTraversalHasCanonicalOwner(t *testing.T) {
	t.Parallel()

	legacyTraversalOwners := map[string]string{
		"gitrepo/repository.go:resolveDotGitPath":    "Codex still uses the exported legacy resolver until GMT-005",
		"gitrepo/repository.go:resolveCommonGitPath": "Codex still uses the exported legacy resolver until GMT-005",
		"paths/worktree.go:GetWorktreeID":            "worktree-ID consumers migrate across GMT-003 through GMT-005",
		"status.go:resolveWorktreeBranch":            "status migrates in GMT-005",
	}
	policyDotGitInspections := map[string]string{
		"agent/codex/hook_root.go:hasDotGitEntry":                    "Codex policy checks whether a candidate checkout owns a .git entry",
		"agent/codex/hook_root.go:linkedWorktreeRegistrationMatches": "Codex backlink policy validates the registered worktree path",
		"dispatch_wizard.go:discoverLocalRepoRoots":                  "dispatch discovery filters sibling repository candidates",
		"gitrepo/status.go:insideNestedCheckout":                     "status walking stops at nested checkout boundaries",
	}
	allowedCommonDirQueries := map[string]string{
		"checkpoint/git_common_dir.go:resolveGitCommonDir":          "checkpoint migrates in GMT-002",
		"gitdir/gitdir.go:CommonDir":                                "session removes the current-worktree resolver in GMT-003",
		"gitdir/gitdir.go:CommonDirForWorktree":                     "session removes the explicit-worktree resolver in GMT-003",
		"session_adopt.go:stateStoreForWorktree":                    "adoption validates an arbitrary source repository in GMT-003",
		"settings/settings.go:clonePreferencesPathForWorktreeRoot":  "settings migrates in GMT-005",
		"strategy/common.go:GetGitCommonDir":                        "strategy migrates in GMT-004",
		"strategy/manual_commit_session.go:gitCommonDirForWorktree": "session routing compares explicit repositories in GMT-004",
		"strategy/metadata_reconcile.go:loadShallowHashes":          "shallow metadata access migrates in GMT-004",
		"trail_checkout_worktree.go:gitCommonDirForTrailWorktree":   "trail storage paths migrate in GMT-005",
		"trail_checkout_worktree.go:validateTrailWorktreeReuse":     "trail reuse validates repository identity in GMT-005",
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve guard source path")
	}
	cliRoot := filepath.Dir(filepath.Dir(thisFile))
	fset := token.NewFileSet()
	canonicalTokens := 0

	err := filepath.WalkDir(cliRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(cliRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") ||
			strings.HasPrefix(rel, "integration_test/") || strings.HasPrefix(rel, "benchutil/") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := rel + ":" + fn.Name.Name
			canonicalOwner := rel == "gitrepo/metadata.go"
			_, legacyOwner := legacyTraversalOwners[key]
			_, policyInspection := policyDotGitInspections[key]

			hasDotGitReference := containsMetadataString(fn.Body, ".git") || containsIdentifier(fn.Body, "gitDir")
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						return true
					}
					value, unquoteErr := strconv.Unquote(n.Value)
					if unquoteErr != nil {
						return true
					}
					if value == "--git-common-dir" {
						if _, allowed := allowedCommonDirQueries[key]; !allowed {
							t.Errorf("%s runs an unaudited --git-common-dir query; use gitrepo.ResolveWorktreeMetadata or document a semantic-query exception", fset.Position(n.Pos()))
						}
					}
					if value != "gitdir: " && value != "gitdir:" && value != "commondir" {
						return true
					}
					if canonicalOwner {
						canonicalTokens++
						return true
					}
					if !legacyOwner {
						t.Errorf("%s independently parses Git metadata token %q; use gitrepo.ResolveWorktreeMetadata", fset.Position(n.Pos()), value)
					}
				case *ast.CallExpr:
					if !hasDotGitReference || !isOSMetadataInspection(n) || canonicalOwner || legacyOwner || policyInspection {
						return true
					}
					t.Errorf("%s independently inspects a .git entry; use gitrepo.ResolveWorktreeMetadata or document a narrow policy exception", fset.Position(n.Pos()))
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan Go sources: %v", err)
	}
	if canonicalTokens < 2 {
		t.Fatal("guard found no canonical gitdir/commondir parser tokens")
	}
}

func isOSMetadataInspection(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "os" {
		return false
	}
	switch selector.Sel.Name {
	case "Lstat", "Open", "OpenFile", "ReadDir", "ReadFile", "Stat":
		return true
	default:
		return false
	}
}

func containsMetadataString(node ast.Node, want string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		literal, ok := n.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && value == want {
			found = true
			return false
		}
		return true
	})
	return found
}

func containsIdentifier(node ast.Node, want string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if ok && ident.Name == want {
			found = true
			return false
		}
		return true
	})
	return found
}
