// Command billetlint runs billet's own analyzers -- the rules golangci-lint
// cannot express.
//
// A SEPARATE MODULE (tools/lint/go.mod) so golang.org/x/tools/go/analysis does
// not become a dependency of the billet binary. billet ships as one static
// binary with four direct dependencies, and an analysis framework carried into
// it for a CI gate would be the tail wagging the dog.
//
// Because it is a nested module, `go test ./...` from the repository root does
// NOT reach the analyzers' own tests. Run them explicitly -- `make lint-custom`
// does both halves, and CI runs the same target. Without that, an analyzer could
// silently stop detecting anything and still report zero violations, which reads
// exactly like a clean tree.
package main

import (
	"golang.org/x/tools/go/analysis/multichecker"

	"github.com/junioryono/billet/tools/lint/analyzer/godiconstructor"
	"github.com/junioryono/billet/tools/lint/analyzer/godioptional"
	"github.com/junioryono/billet/tools/lint/analyzer/parallelshared"
	"github.com/junioryono/billet/tools/lint/analyzer/rawsql"
)

func main() {
	multichecker.Main(
		godiconstructor.Analyzer,
		godioptional.Analyzer,
		parallelshared.Analyzer,
		rawsql.Analyzer,
	)
}
