package app

import (
	"context"
	"errors"
	"go/ast"
	"go/types"
	"log/slog"
	"testing"
	"time"
)

// THE SAMPLER'S READ OF THE PLUG ANSWERS THE PLUG'S POWER, and its refusal as
// its own, each within half an interval of the sampler's goroutine and never
// longer than meterReadLimit, however long the interval.
func TestTheSamplersReadOfThePlugIsBoundedAndTrue(t *testing.T) {
	t.Parallel()

	for interval, most := range map[time.Duration]time.Duration{
		500 * time.Millisecond: 250 * time.Millisecond,
		30 * time.Second:       meterReadLimit,
	} {
		var left time.Duration
		read := meterReader(t.Context(), func(ctx context.Context) (float64, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("the plug was read with no deadline")
			}
			left = time.Until(deadline)

			return 17.5, nil
		}, interval, slog.New(slog.DiscardHandler))
		if w, err := read(); err != nil || w != 17.5 {
			t.Fatalf("read %v, %v; want the plug's 17.5 W", w, err)
		}
		if left <= most/2 || left > most {
			t.Errorf("at a %s interval the read had %s left, want at most %s", interval, left, most)
		}
	}

	answer, answerErr := 0.0, errors.New("unplugged")
	read := meterReader(t.Context(), func(context.Context) (float64, error) { return answer, answerErr },
		2*time.Second, slog.New(slog.DiscardHandler))
	if _, err := read(); !errors.Is(err, answerErr) {
		t.Fatalf("a failed read answered %v, want the plug's error", err)
	}
}

// NODE MONITORING HANDS THE SAMPLER ITS PLUG AND THE PLUG'S IDLE DRAW: only the
// source shows the configured block reaches the sampler, since the node's
// tests build their own.
func TestNodeMonitoringGivesTheSamplerItsMeter(t *testing.T) {
	t.Parallel()

	wired := false
	ast.Inspect(nodeFunc(t, "nodemonitoring.go", "nodeMonitorOptions"), func(n ast.Node) bool {
		if x, ok := n.(*ast.AssignStmt); ok && len(x.Lhs) == 2 && len(x.Rhs) == 2 &&
			types.ExprString(x.Lhs[0]) == "sampling.Meter" &&
			types.ExprString(x.Rhs[0]) == "meterReader(ctx, plug.Watts, interval, slog.Default())" &&
			types.ExprString(x.Lhs[1]) == "sampling.IdleWatts" &&
			types.ExprString(x.Rhs[1]) == "m.Meter.IdleWatts" {
			wired = true
		}

		return true
	})
	if !wired {
		t.Error("nodeMonitorOptions does not give the sampler its plug's power and idle draw")
	}
}
