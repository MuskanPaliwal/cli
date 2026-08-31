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

func TestGitMetadataTraversalHasSingleOwner(t *testing.T) {
	t.Parallel()
	allowedDotGitInspections := map[string]string{
		"agent/codex/hook_root.go:hasDotGitEntry": "Codex policy checks whether a candidate checkout owns a .git entry",
		"plugin_index.go:SyncPluginIndex":         "the plugin cache checks whether its private clone has been materialized",
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve guard source path")
	}
	cliRoot := filepath.Dir(filepath.Dir(thisFile))
	fset := token.NewFileSet()
	ownerTokens := 0
	allowedSeen := map[string]bool{}

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
					if strings.HasPrefix(rel, "gitrepo/") {
						if strings.Contains(value, "gitdir:") || value == "commondir" {
							ownerTokens++
						}
						return true
					}
					if strings.Contains(value, "gitdir:") || value == "commondir" {
						t.Errorf("%s independently parses Git metadata token %q; use gitrepo.ResolveWorktreeMetadata", fset.Position(n.Pos()), value)
					}
				case *ast.CallExpr:
					if strings.HasPrefix(rel, "gitrepo/") || !isOSMetadataInspection(n) || !containsStringLiteral(n, ".git") {
						return true
					}
					key := rel + ":" + fn.Name.Name
					if _, allowed := allowedDotGitInspections[key]; allowed {
						allowedSeen[key] = true
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
	if ownerTokens == 0 {
		t.Fatal("guard found no canonical gitdir/commondir parser tokens; update the guard with the metadata owner")
	}
	for key, reason := range allowedDotGitInspections {
		if !allowedSeen[key] {
			t.Errorf("documented .git inspection exception %s (%s) no longer exists; remove or update the exception", key, reason)
		}
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

func containsStringLiteral(node ast.Node, want string) bool {
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
