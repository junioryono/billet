package host

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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

	src, err := os.ReadFile("joinprereq.go")
	mustOK(t, err)

	file, err := parser.ParseFile(token.NewFileSet(), "joinprereq.go", src, 0)
	mustOK(t, err)

	// AND A COMMAND GIVEN INPUT READS IT, with its output discarded: the sshd
	// policy reaches root on tee's standard input, never through a file. Read
	// as code, so a commented-out assignment does not count.
	handsInput := false
	ast.Inspect(file, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 2 || len(as.Rhs) != 2 {
			return true
		}

		var b strings.Builder
		for _, e := range append(append([]ast.Expr{}, as.Lhs...), as.Rhs...) {
			b.Write(src[e.Pos()-file.FileStart : e.End()-file.FileStart])
			b.WriteString(";")
		}
		handsInput = handsInput || b.String() == "cmd.Stdin;cmd.Stdout;input;io.Discard;"

		return true
	})
	if !handsInput {
		t.Error("prereqSudo no longer hands a command its input on standard input")
	}

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
