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
		"gitrepo/repository.go:resolveDotGitPath":    "Codex still uses the exported legacy resolver until the remaining-consumer split",
		"gitrepo/repository.go:resolveCommonGitPath": "Codex still uses the exported legacy resolver until the remaining-consumer split",
		"paths/worktree.go:GetWorktreeID":            "worktree-ID consumers migrate with session and the remaining consumers",
		"status.go:resolveWorktreeBranch":            "status migrates with the remaining consumers",
	}
	policyDotGitInspections := map[string]string{
		"agent/codex/hook_root.go:hasDotGitEntry":                    "Codex policy checks whether a candidate checkout owns a .git entry",
		"agent/codex/hook_root.go:linkedWorktreeRegistrationMatches": "Codex backlink policy validates the registered worktree path",
		"dispatch_wizard.go:discoverLocalRepoRoots":                  "dispatch discovery filters sibling repository candidates",
		"gitrepo/status.go:insideNestedCheckout":                     "status walking stops at nested checkout boundaries",
		"plugin_index.go:SyncPluginIndex":                            "plugin index sync checks whether Entire's cache directory contains its clone",
	}
	allowedCommonDirQueries := map[string]string{
		"checkpoint/git_common_dir.go:resolveGitCommonDir":          "checkpoint migrates in the checkpoint split",
		"gitdir/gitdir.go:CommonDir":                                "session removes the current-worktree resolver in the session split",
		"gitdir/gitdir.go:CommonDirForWorktree":                     "session removes the explicit-worktree resolver in the session split",
		"session_adopt.go:stateStoreForWorktree":                    "adoption validates an arbitrary source repository in the session split",
		"settings/settings.go:clonePreferencesPathForWorktreeRoot":  "settings migrates with the remaining consumers",
		"strategy/common.go:GetGitCommonDir":                        "strategy migrates in the strategy-and-hooks split",
		"strategy/manual_commit_session.go:gitCommonDirForWorktree": "session routing migrates in the strategy-and-hooks split",
		"strategy/metadata_reconcile.go:loadShallowHashes":          "shallow metadata access migrates in the strategy-and-hooks split",
		"trail_checkout_worktree.go:gitCommonDirForTrailWorktree":   "trail storage paths migrate with the remaining consumers",
		"trail_checkout_worktree.go:validateTrailWorktreeReuse":     "trail reuse validation migrates with the remaining consumers",
	}
	allowedLegacyResolverCalls := map[string]string{
		"agent/codex/hook_root.go:resolveHookDiscovery": "Codex discovery migrates with the remaining consumers",
		"agent/codex/hook_root.go:rootOwnsGitDir":       "Codex ownership policy migrates with the remaining consumers",
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve guard source path")
	}
	cliRoot := filepath.Dir(filepath.Dir(thisFile))
	fset := token.NewFileSet()
	canonicalTokens := 0
	legacyOwnersSeen := map[string]bool{}
	policyInspectionsSeen := map[string]bool{}
	commonDirQueriesSeen := map[string]bool{}
	legacyResolverCallsSeen := map[string]bool{}

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
		dotGitIdentifiers := metadataStringIdentifiers(file, ".git")
		dotGitIdentifiers["gitDir"] = true
		gitrepoImports, dotImportedGitrepo := gitrepoImportNames(file)
		filesystemImports := filesystemImportNames(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := rel + ":" + fn.Name.Name
			canonicalOwner := rel == "gitrepo/metadata.go"
			_, legacyOwner := legacyTraversalOwners[key]
			_, policyInspection := policyDotGitInspections[key]

			hasDotGitReference := containsMetadataString(fn.Body, ".git") || containsAnyIdentifier(fn.Body, dotGitIdentifiers)
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
						if _, allowed := allowedCommonDirQueries[key]; allowed {
							commonDirQueriesSeen[key] = true
						} else {
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
					if legacyOwner {
						legacyOwnersSeen[key] = true
					} else {
						t.Errorf("%s independently parses Git metadata token %q; use gitrepo.ResolveWorktreeMetadata", fset.Position(n.Pos()), value)
					}
				case *ast.CallExpr:
					if isLegacyMetadataResolverCall(n, gitrepoImports, dotImportedGitrepo) {
						if _, allowed := allowedLegacyResolverCalls[key]; allowed {
							legacyResolverCallsSeen[key] = true
						} else {
							t.Errorf("%s calls a legacy Git metadata resolver; use gitrepo.ResolveWorktreeMetadata or document the migration exception", fset.Position(n.Pos()))
						}
					}
					callHasDotGitReference := containsMetadataString(n, ".git") || containsAnyIdentifier(n, dotGitIdentifiers)
					packageInspection := hasDotGitReference && isPackageFilesystemInspection(n, filesystemImports)
					if (!callHasDotGitReference && !packageInspection) || !isFilesystemMetadataInspection(n) || canonicalOwner {
						return true
					}
					if legacyOwner {
						legacyOwnersSeen[key] = true
						return true
					}
					if policyInspection {
						policyInspectionsSeen[key] = true
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
	assertGuardLedgerSeen(t, "legacy traversal owner", legacyTraversalOwners, legacyOwnersSeen)
	assertGuardLedgerSeen(t, ".git policy inspection", policyDotGitInspections, policyInspectionsSeen)
	assertGuardLedgerSeen(t, "--git-common-dir query", allowedCommonDirQueries, commonDirQueriesSeen)
	assertGuardLedgerSeen(t, "legacy resolver call", allowedLegacyResolverCalls, legacyResolverCallsSeen)
}

func TestGitMetadataGuardRecognizesBypassForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		source     string
		wantLegacy bool
	}{
		{
			name:   "rooted filesystem",
			source: `package probe; import "os"; func inspect(root *os.Root) { _, _ = root.Lstat(".git") }`,
		},
		{
			name:   "package constant",
			source: `package probe; import "os"; const dotGitName = ".git"; func inspect() { _, _ = os.Stat(dotGitName) }`,
		},
		{
			name:   "readlink",
			source: `package probe; import "os"; func inspect() { _, _ = os.Readlink(".git") }`,
		},
		{
			name:       "aliased legacy resolver import",
			source:     `package probe; import repo "github.com/entireio/cli/cmd/entire/cli/gitrepo"; func inspect() { _, _ = repo.ResolveDotGitPath(".") }`,
			wantLegacy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "probe.go", tt.source, 0)
			if err != nil {
				t.Fatalf("parse probe: %v", err)
			}
			identifiers := metadataStringIdentifiers(file, ".git")
			identifiers["gitDir"] = true
			imports, dotImported := gitrepoImportNames(file)
			found := false
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				hasDotGitReference := containsMetadataString(fn.Body, ".git") || containsAnyIdentifier(fn.Body, identifiers)
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if tt.wantLegacy {
						found = found || isLegacyMetadataResolverCall(call, imports, dotImported)
					} else {
						found = found || hasDotGitReference && isFilesystemMetadataInspection(call)
					}
					return true
				})
			}
			if !found {
				t.Fatal("guard did not recognize probe")
			}
		})
	}
}

func isFilesystemMetadataInspection(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch selector.Sel.Name {
	case "Lstat", "Open", "OpenFile", "ReadDir", "ReadFile", "Readlink", "Stat":
		return true
	default:
		return false
	}
}

func metadataStringIdentifiers(file *ast.File, want string) map[string]bool {
	identifiers := map[string]bool{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, value := range valueSpec.Values {
				literal, ok := value.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err == nil && unquoted == want && i < len(valueSpec.Names) {
					identifiers[valueSpec.Names[i].Name] = true
				}
			}
		}
	}
	return identifiers
}

func gitrepoImportNames(file *ast.File) (map[string]bool, bool) {
	names := map[string]bool{}
	dotImported := false
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "github.com/entireio/cli/cmd/entire/cli/gitrepo" {
			continue
		}
		switch {
		case spec.Name == nil:
			names["gitrepo"] = true
		case spec.Name.Name == ".":
			dotImported = true
		case spec.Name.Name != "_":
			names[spec.Name.Name] = true
		}
	}
	return names, dotImported
}

func filesystemImportNames(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || (path != "os" && path != "io/fs") || (spec.Name != nil && spec.Name.Name == "_") {
			continue
		}
		switch {
		case spec.Name == nil:
			names[filepath.Base(path)] = true
		case spec.Name.Name != ".":
			names[spec.Name.Name] = true
		}
	}
	return names
}

func isPackageFilesystemInspection(call *ast.CallExpr, importNames map[string]bool) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && importNames[pkg.Name]
}

func isLegacyMetadataResolverCall(call *ast.CallExpr, importNames map[string]bool, dotImported bool) bool {
	legacyName := func(name string) bool {
		return name == "ResolveDotGitPath" || name == "ResolveCommonGitPath"
	}
	if ident, ok := call.Fun.(*ast.Ident); ok {
		return dotImported && legacyName(ident.Name)
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !legacyName(selector.Sel.Name) {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && importNames[pkg.Name]
}

func assertGuardLedgerSeen(t *testing.T, label string, ledger map[string]string, seen map[string]bool) {
	t.Helper()
	for key, reason := range ledger {
		if !seen[key] {
			t.Errorf("documented %s %s (%s) no longer exists; remove or update the exception", label, key, reason)
		}
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

func containsAnyIdentifier(node ast.Node, want map[string]bool) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if ok && want[ident.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}
