package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// TestMainPreflightsTheWorkerItServes reads main.go rather than running it. A
// preflight that skips a step every job takes still passes on a working host,
// so no run of the binary can tell the two apart; the call is the only
// witness. main must preflight through the worker it then serves —
// Worker.Preflight builds the sandbox from the same configuration as every
// job — and must not hand sandbox.Preflight a spec of its own, which is how
// the artifact supervisor went unchecked.
func TestMainPreflightsTheWorkerItServes(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	type call struct {
		receiver string
		pos      token.Pos
	}
	calls := map[string][]call{} // method name -> every receiver it is called on
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok {
					calls[sel.Sel.Name] = append(calls[sel.Sel.Name], call{x.Name, c.Pos()})
				}
			}
		}
		return true
	})

	serve, preflight := calls["Serve"], calls["Preflight"]
	if len(serve) != 1 {
		t.Fatalf("main calls Serve %d times, want once: %v", len(serve), serve)
	}
	receivers := func(cs []call) []string {
		var r []string
		for _, c := range cs {
			r = append(r, c.receiver)
		}
		return r
	}
	if want := []string{serve[0].receiver}; !slices.Equal(receivers(preflight), want) {
		t.Fatalf("main calls Preflight on %v, want only on %v, the worker it serves",
			receivers(preflight), want)
	}
	if preflight[0].pos > serve[0].pos {
		t.Fatal("main preflights after it starts serving")
	}
}
