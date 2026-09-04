// Package godioptional reports an `optional:"true"` field on a godi parameter
// object that does not say what its zero value refuses.
//
// WHAT THE RULE IS. godi fills an `optional:"true"` field of a `godi.In` struct
// with the ZERO VALUE when nothing is registered for it, and a constructor then
// proceeds as though it had been handed a collaborator. Every zero value in
// billet's domain refuses (`TeardownRequested`, `TrustUnknown`,
// `AdmissionUnknown`, an unrecorded epoch), which is what makes an optional field
// acceptable where the zero value is one of those and dangerous everywhere else:
// a nil store that a caller reads as "keep going" is a control plane that skips a
// step nobody registered.
//
// HOW IT DECIDES. Any struct that embeds godi.In is a parameter object. Every
// field of one carrying `optional:"true"` is reported unless the field carries
// billet's suppression directive with its reason:
//
//	//billet:ignore godioptional // the zero value refuses: a nil store keeps the authority as files
//
// The reason is mandatory and an unused directive is reported, exactly as every
// other billetlint analyzer works, so "0 violations" means "0 unexplained
// optional fields". The rule is about the SENTENCE: nothing here can check that
// the zero value really refuses, only that somebody wrote down why.
//
// # What it does not look at
//
//   - A struct that does not embed godi.In. The tag means nothing to godi there.
//   - _test.go files, where a parameter object is a fixture.
package godioptional

import (
	"go/ast"
	"go/types"
	"reflect"
	"strconv"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/junioryono/billet/tools/lint/suppress"
)

const analyzerName = "godioptional"

// godiModule is the import path prefix every godi type lives under. godi.In is
// an alias of a type in godi's internal reflection package, so the prefix is
// what both spellings share.
const godiModule = "github.com/junioryono/godi/v5"

// Analyzer is the godioptional analyzer.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "report an optional godi.In field that does not say what its zero value refuses",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		name := pass.Fset.Position(file.Pos()).Filename
		if strings.HasSuffix(name, "_test.go") {
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

		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok || !embedsGodiIn(pass, st) {
				return true
			}

			for _, field := range st.Fields.List {
				if field.Tag == nil || !isOptional(field.Tag.Value) {
					continue
				}

				if ignores.Skip(pass, field.Pos(), analyzerName) {
					continue
				}

				pass.Reportf(field.Pos(),
					"%s is optional, so godi hands the constructor its ZERO VALUE when "+
						"nothing is registered. Say what that zero value refuses: "+
						"`//billet:ignore %s // the zero value refuses: <what>` on the field, "+
						"or make the dependency required",
					fieldName(field), analyzerName)
			}

			return true
		})

		ignores.ReportUnused(pass, analyzerName)
	}

	return nil, nil
}

// embedsGodiIn reports whether a struct embeds godi.In, which is what makes
// godi read its tags at all.
func embedsGodiIn(pass *analysis.Pass, st *ast.StructType) bool {
	for _, field := range st.Fields.List {
		if len(field.Names) != 0 {
			continue
		}

		named, ok := pass.TypesInfo.TypeOf(field.Type).(*types.Named)
		if !ok {
			continue
		}

		obj := named.Obj()
		if obj == nil || obj.Pkg() == nil {
			continue
		}

		if obj.Name() == "In" && strings.HasPrefix(obj.Pkg().Path(), godiModule) {
			return true
		}
	}

	return false
}

// isOptional reads the tag the way godi does: the `optional` key with the
// value `true`, whatever else the tag carries.
func isOptional(raw string) bool {
	tag, err := strconv.Unquote(raw)
	if err != nil {
		return false
	}

	return reflect.StructTag(tag).Get("optional") == "true"
}

// fieldName is what the diagnostic calls the field.
func fieldName(field *ast.Field) string {
	if len(field.Names) == 0 {
		return "an embedded field"
	}

	return field.Names[0].Name
}
