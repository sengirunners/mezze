package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/sengirunners/mezze/internal/agentpipe"
	"github.com/sengirunners/mezze/internal/initgc"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/mezze/internal/session/taskcloud"
	"github.com/sengirunners/mezze/internal/session/tasklaunch"
	"github.com/sengirunners/potluck/kit/swap"
)

// Each hidden mode starts from an init and exits without reaching main, so
// initgc must leave the GC on for it. A new mode dispatched from an init
// belongs in this list.
func TestHiddenModesKeepGC(t *testing.T) {
	for _, arg := range []string{holder.Arg, holder.PriorityArg, tasklaunch.Arg, taskcloud.Arg, "__zm-task-anchor",
		agentpipe.Arg, agentpipe.FakeArg, swap.LaunchArg} {
		if initgc.Command([]string{"zm", arg}) {
			t.Errorf("initgc.Command takes hidden mode %q for a plain command: it would run with no GC", arg)
		}
	}
}

// internal/initgc turns the GC off for package init on a plain command. main
// and TestMain must turn it back on before anything else: otherwise that zm
// process (the daemon included) runs with no collector.
func TestRestoreGCFirst(t *testing.T) {
	for file, fn := range map[string]string{"main.go": "main", "startup_test.go": "TestMain"} {
		f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var body *ast.BlockStmt
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == fn {
				body = fd.Body
			}
		}
		if body == nil || len(body.List) == 0 {
			t.Fatalf("%s: no func %s", file, fn)
		}
		if !isRestore(body.List[0]) {
			t.Errorf("%s: %s's first statement is not initgc.Restore()", file, fn)
		}
	}
}

func isRestore(s ast.Stmt) bool {
	es, ok := s.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Restore" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "initgc"
}
