package replay

import (
	"io"

	"github.com/junioryono/billet/internal/jobtrace"
)

// The trace format is internal/jobtrace's, the one `billet jobs export`
// writes, named here as the replay has always named it.

// ResultSucceeded is the one completion result billet recognises as success.
const ResultSucceeded = jobtrace.ResultSucceeded

type (
	// Duration is a time.Duration that reads and writes as Go spells one.
	Duration = jobtrace.Duration
	// Arrival is one job arriving at GitHub's queue.
	Arrival = jobtrace.Arrival
	// Trace is a workload: arrivals in time order.
	Trace = jobtrace.Trace
)

// ReadTrace reads a trace as JSON lines, normalized and validated.
func ReadTrace(r io.Reader) (Trace, error) { return jobtrace.ReadTrace(r) }
