package host

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// SUDO IS RUN BY ITS ABSOLUTE PATH, never found on PATH: a wrapper in any
// directory the account can write (/usr/local/bin is one) would otherwise run
// in its place. Read from the source, because every other test replaces
// prereqSudo.
func TestJoinRunsSudoByItsAbsolutePath(t *testing.T) {
	if sudoBinary != "/usr/bin/sudo" {
		t.Errorf("sudo is %q, want /usr/bin/sudo", sudoBinary)
	}

	file, err := parser.ParseFile(token.NewFileSet(), "joinprereq.go", nil, 0)
	mustOK(t, err)

	byConstant := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != "CommandContext" || len(call.Args) < 2 {
			return true
		}

		switch name := call.Args[1].(type) {
		case *ast.BasicLit:
			if strings.Trim(name.Value, `"`) == "sudo" {
				t.Errorf("joinprereq.go runs sudo from PATH at %v", call.Pos())
			}
		case *ast.Ident:
			if name.Name == "sudoBinary" {
				byConstant++
			}
		}

		return true
	})
	if byConstant != 1 {
		t.Errorf("joinprereq.go runs sudoBinary %d times, want once (prereqSudo)", byConstant)
	}
}
