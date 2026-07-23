package router

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestProductionNewTaskDoesNotHideOptions protects Lite mode from a subtle
// parity regression: asynq.Task keeps NewTask options private, so an alternate
// TaskEnqueuer cannot recover them. Production options must therefore always
// be passed to Enqueue, where both Redis and Lite executors can observe them.
func TestProductionNewTaskDoesNotHideOptions(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", "internal"))
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		aliases := map[string]struct{}{}
		dotImported := false
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil || importPath != "github.com/hibiken/asynq" {
				continue
			}
			if spec.Name != nil {
				if spec.Name.Name == "." {
					dotImported = true
				} else if spec.Name.Name != "_" {
					aliases[spec.Name.Name] = struct{}{}
				}
			} else {
				aliases["asynq"] = struct{}{}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) <= 2 {
				return true
			}
			isNewTask := false
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				if ok {
					_, imported := aliases[pkg.Name]
					isNewTask = imported && fun.Sel.Name == "NewTask"
				}
			case *ast.Ident:
				isNewTask = dotImported && fun.Name == "NewTask"
			}
			if isNewTask {
				pos := fset.Position(call.Lparen)
				violations = append(violations, fmt.Sprintf("%s:%d", filepath.ToSlash(path), pos.Line))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan production Go sources: %v", err)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("asynq.NewTask options are invisible to Lite mode; move them to Enqueue:\n%s",
			strings.Join(violations, "\n"))
	}
}
