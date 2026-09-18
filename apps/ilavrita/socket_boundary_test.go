package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestASocketReadsTheWiringOnce. A socket outlives the request that accepted it,
// so a function it runs afterwards must not reach back into `serving`: what that
// points at is replaced when a process reconfigures and unset when a test
// finishes, and a goroutine still reading it then is reading a variable somebody
// else is writing. That is a data race the race detector only catches when the
// timing happens to line up, which is why it is asserted here instead.
//
// Everything a socket needs is taken once, in the handler, and carried from
// there as a socketDuty.
func TestASocketReadsTheWiringOnce(t *testing.T) {
	// The one function that may: it runs inside the request, before the socket
	// is accepted, the same as every other route's first line.
	const reader = "subscribeOverWebSocket"

	file, err := parser.ParseFile(token.NewFileSet(), "websocket.go", nil, 0)
	if err != nil {
		t.Fatalf("read the socket source: %v", err)
	}

	for _, declared := range file.Decls {
		function, isFunction := declared.(*ast.FuncDecl)
		if !isFunction || function.Name.Name == reader {
			continue
		}

		ast.Inspect(function, func(node ast.Node) bool {
			named, isName := node.(*ast.Ident)
			if isName && named.Name == "serving" {
				t.Errorf("%s reads the wiring; take it in %s and carry it",
					function.Name.Name, reader)
			}

			return true
		})
	}

	// And the guard is worth something only while that function does read it,
	// so a rename that emptied this rule would be caught too.
	found := false

	ast.Inspect(file, func(node ast.Node) bool {
		named, isName := node.(*ast.Ident)
		if isName && named.Name == "serving" {
			found = true
		}

		return true
	})

	if !found {
		t.Errorf("nothing in websocket.go reads the wiring, so %s no longer exists", reader)
	}
}
