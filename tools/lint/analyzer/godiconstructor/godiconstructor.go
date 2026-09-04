// Package godiconstructor reports an exported `New*` constructor, in a file
// that registers services with godi, whose first result is a pointer to an
// unexported struct.
//
// WHAT THE RULE IS. A constructor the container registers is a seam: what it
// returns is what every dependent names in its own parameters. Returning
// `*thing` from `NewThing` lets a dependent name the implementation, so the fake
// a test would register in its place must be that same struct, which defeats the
// reason for registering through the container at all. A constructor returns the
// interface it satisfies, or an EXPORTED concrete type where the concrete type is
// the contract (`*state.DB`, `*alloc.Allocator`).
//
// HOW IT DECIDES. Only files that import godi are looked at, because that is
// where registrations are written; a `New*` in a domain package returning its own
// unexported struct is that package's business. Only exported `New*` functions
// count, because an unexported constructor is registered only from inside its
// own package, where the type is visible anyway.
package godiconstructor

import (
	"go/ast"
	"go/types"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/tools/go/analysis"

	"github.com/junioryono/billet/tools/lint/suppress"
)

const analyzerName = "godiconstructor"

const godiModule = "github.com/junioryono/godi/v5"

// Analyzer is the godiconstructor analyzer.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "an exported godi constructor returns the interface, not a *unexportedStruct",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		name := pass.Fset.Position(file.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") || !importsGodi(file) {
			continue
		}

		var ignores suppress.Lines

		if suppress.Present(file) {
			src, err := pass.ReadFile(name)
			if err != nil {
				return nil, err
			}

			ignores = suppress.Index(pass, file, src)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !isExportedConstructor(fn.Name.Name) {
				continue
			}

			if fn.Type.Results == nil || len(fn.Type.Results.List) == 0 {
				continue
			}

			hidden := unexportedPointee(pass, fn.Type.Results.List[0].Type)
			if hidden == "" {
				continue
			}

			if ignores.Skip(pass, fn.Name.Pos(), analyzerName) {
				continue
			}

			pass.Reportf(fn.Name.Pos(),
				"%s returns *%s, an unexported struct, from a file that registers with "+
					"godi. Return the interface it implements (or an exported concrete "+
					"type that IS the contract), so a dependent names the seam and a test "+
					"can register something else behind it",
				fn.Name.Name, hidden)
		}

		ignores.ReportUnused(pass, analyzerName)
	}

	return nil, nil
}

// importsGodi reports whether the file names the godi module in its imports.
func importsGodi(file *ast.File) bool {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err == nil && strings.HasPrefix(path, godiModule) {
			return true
		}
	}

	return false
}

// isExportedConstructor reports whether a name is New followed by an exported
// suffix: NewThing, not New (which names nothing) and not newThing.
func isExportedConstructor(name string) bool {
	if !strings.HasPrefix(name, "New") || len(name) < 4 {
		return false
	}

	return unicode.IsUpper([]rune(name[3:])[0])
}

// unexportedPointee is the name of the unexported struct a result type points
// at, or empty when the result is anything else.
func unexportedPointee(pass *analysis.Pass, expr ast.Expr) string {
	ptr, ok := pass.TypesInfo.TypeOf(expr).(*types.Pointer)
	if !ok {
		return ""
	}

	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return ""
	}

	obj := named.Obj()
	if obj == nil || obj.Exported() {
		return ""
	}

	if _, isStruct := named.Underlying().(*types.Struct); !isStruct {
		return ""
	}

	return obj.Name()
}
