package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// All bd output must use the run-scoped clean writers. This catches a new
// direct fmt.Print or os.Stdout write that would bypass the boundary.
func TestReview2OutputBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	stdRefs := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && (sel.Sel.Name == "Stdout" || sel.Sel.Name == "Stderr") {
					if name != "main.go" {
						t.Errorf("%s: direct os.%s bypasses clean output", name, sel.Sel.Name)
					}
					stdRefs[sel.Sel.Name]++
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name == "fmt" && (sel.Sel.Name == "Print" || sel.Sel.Name == "Println" || sel.Sel.Name == "Printf") {
				t.Errorf("%s: fmt.%s bypasses clean output", name, sel.Sel.Name)
			}
			return true
		})
	}
	for _, name := range []string{"Stdout", "Stderr"} {
		if stdRefs[name] != 1 {
			t.Errorf("os.%s referenced %d times; want only the writer declaration", name, stdRefs[name])
		}
	}
}
