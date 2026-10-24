package app

import (
	"context"
	"errors"
	"go/ast"
	"go/types"
	"math"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/wallmeter"
)

// THE SAMPLER READS WHAT THE METER INTEGRATED, a total that never wraps in the
// meter's epoch, and the meter's refusal as its own: a meter with no recent
// reading is could-not-tell, never zero.
func TestTheSamplerReadsTheMetersEnergy(t *testing.T) {
	t.Parallel()

	watts := []float64{10, 20}
	meter := wallmeter.New(func(context.Context) (float64, error) {
		w := watts[0]
		watts = watts[1:]

		return w, nil
	}, time.Hour, nil)
	read := meterEnergy(meter)

	if _, err := read(); !errors.Is(err, wallmeter.ErrNoReading) {
		t.Fatalf("before any reading the sampler read %v, want the meter's ErrNoReading", err)
	}

	meter.Poll(t.Context())
	e, err := read()
	if err != nil || e.MaxRange != math.MaxInt64 || e.Epoch != 1 {
		t.Fatalf("after one reading the sampler read %+v, %v; want epoch 1 and a range that never wraps",
			e, err)
	}
}

// NODE MONITORING HANDS THE SAMPLER THE METER IT STARTED, with the meter's idle
// draw as the baseline: only the source shows the configured block reaches the
// sampler, since the node's tests build their own.
func TestNodeMonitoringGivesTheSamplerItsMeter(t *testing.T) {
	t.Parallel()

	var started, wired bool
	ast.Inspect(nodeFunc(t, "nodemonitoring.go", "nodeMonitorOptions"), func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.GoStmt:
			if sel, ok := x.Call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, "meter") && sel.Sel.Name == "Run" {
				started = true
			}
		case *ast.AssignStmt:
			if len(x.Lhs) == 2 && len(x.Rhs) == 2 && types.ExprString(x.Lhs[0]) == "sampling.Meter" &&
				types.ExprString(x.Rhs[0]) == "meterEnergy(meter)" &&
				types.ExprString(x.Lhs[1]) == "sampling.IdleWatts" &&
				types.ExprString(x.Rhs[1]) == "m.Meter.IdleWatts" {
				wired = true
			}
		}

		return true
	})
	if !started || !wired {
		t.Errorf("nodeMonitorOptions starts the meter %v and gives the sampler its energy and idle draw %v; "+
			"want both", started, wired)
	}
}
