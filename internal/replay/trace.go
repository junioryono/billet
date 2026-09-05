package replay

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// ResultSucceeded is the one completion result billet recognises as success,
// spelled as the scale-set wire spells it. A trace that names no result gets it.
const ResultSucceeded = "succeeded"

// Duration is a time.Duration that reads and writes as Go spells one ("4m20s"),
// which is what the exporter script emits and what a person can read in a file.
type Duration time.Duration

// MarshalJSON renders the duration as Go spells it.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is a string such as \"4m20s\": %w", err)
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}

	*d = Duration(parsed)

	return nil
}

// Arrival is one job arriving at GitHub's queue: what a trace is made of.
//
// A ZERO IS FILLED IN OR REFUSED, NEVER REPLACED ON THE WIRE. The message the
// scripted service builds overlays these fields on a default, and an overlay
// cannot tell "zero" from "absent"; so Normalize writes the run id it will use
// and Validate refuses a job with no time, and what the file says is what is
// replayed.
type Arrival struct {
	// Seq numbers the arrivals from 1 in arrival order. It is the request id the
	// scripted GitHub offers the job under, so the ledger's request_id joins a
	// recorded row back to this line without any table of the harness's own.
	Seq int64 `json:"seq"`
	// At is when the job was queued.
	At time.Time `json:"at"`
	// Tier is the runs-on label, which is the scale set that will carry it.
	Tier string `json:"tier"`
	// Owner, Repository and WorkflowRef are what GitHub puts on the message.
	Owner       string `json:"owner"`
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflow"`
	RunID       int64  `json:"run_id"`
	// Duration is how long the job ran once a runner had it.
	Duration Duration `json:"duration"`
	// Result is GitHub's conclusion; empty means ResultSucceeded.
	Result string `json:"result"`
}

// Trace is a workload: arrivals in time order.
type Trace struct {
	Arrivals []Arrival
}

// Normalize sorts the arrivals by time, numbers them from 1 and fills the
// defaults a generator or an exporter may leave empty.
//
// SEQUENCE FOLLOWS TIME, ties broken by the order the arrivals were given in, so
// a trace read from a file and the same trace regenerated number their jobs the
// same way.
func (t *Trace) Normalize() {
	slices.SortStableFunc(t.Arrivals, func(a, b Arrival) int {
		return a.At.Compare(b.At)
	})

	for i := range t.Arrivals {
		a := &t.Arrivals[i]
		a.Seq = int64(i + 1)
		a.At = a.At.UTC()

		if a.Result == "" {
			a.Result = ResultSucceeded
		}

		if a.Owner == "" {
			a.Owner = DefaultOwner
		}

		// A job with no run of its own is its own run, said in the trace.
		if a.RunID == 0 {
			a.RunID = a.Seq
		}
	}
}

// Validate refuses a trace the replay could not carry faithfully.
func (t *Trace) Validate() error {
	if len(t.Arrivals) == 0 {
		return errors.New("replay: the trace has no arrivals")
	}

	var errs []error

	for i := range t.Arrivals {
		a := &t.Arrivals[i]
		where := fmt.Sprintf("arrival %d", i+1)

		if a.Seq != int64(i+1) {
			errs = append(errs, fmt.Errorf("%s: seq is %d; the trace is not normalized", where, a.Seq))
		}

		if a.At.IsZero() {
			errs = append(errs, fmt.Errorf("%s: has no arrival time", where))
		}

		if i > 0 && a.At.Before(t.Arrivals[i-1].At) {
			errs = append(errs, fmt.Errorf("%s: arrives before the one before it", where))
		}

		if a.RunID <= 0 {
			errs = append(errs, fmt.Errorf("%s: run id %d is not positive", where, a.RunID))
		}

		if strings.TrimSpace(a.Tier) == "" {
			errs = append(errs, fmt.Errorf("%s: names no tier", where))
		}

		if a.Duration <= 0 {
			errs = append(errs, fmt.Errorf("%s: duration %s is not positive", where, time.Duration(a.Duration)))
		}

		if a.Repository == "" || a.WorkflowRef == "" {
			errs = append(errs, fmt.Errorf("%s: needs a repository and a workflow", where))
		}
	}

	return errors.Join(errs...)
}

// Tiers reports the distinct labels the trace uses, sorted.
func (t *Trace) Tiers() []string {
	seen := map[string]bool{}

	var out []string

	for i := range t.Arrivals {
		tier := t.Arrivals[i].Tier
		if !seen[tier] {
			seen[tier] = true
			out = append(out, tier)
		}
	}

	slices.Sort(out)

	return out
}

// LongestDuration is the longest job on one tier, or zero for a tier the trace
// never uses.
func (t *Trace) LongestDuration(tier string) time.Duration {
	var longest time.Duration

	for i := range t.Arrivals {
		a := &t.Arrivals[i]
		if a.Tier == tier && time.Duration(a.Duration) > longest {
			longest = time.Duration(a.Duration)
		}
	}

	return longest
}

// ReadTrace reads a trace as JSON lines: one Arrival per line, blank lines
// ignored. The result is normalized and validated.
func ReadTrace(r io.Reader) (Trace, error) {
	var t Trace

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	line := 0

	for scanner.Scan() {
		line++

		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}

		var a Arrival
		if err := json.Unmarshal([]byte(text), &a); err != nil {
			return Trace{}, fmt.Errorf("replay: trace line %d: %w", line, err)
		}

		t.Arrivals = append(t.Arrivals, a)
	}

	if err := scanner.Err(); err != nil {
		return Trace{}, fmt.Errorf("replay: read trace: %w", err)
	}

	t.Normalize()

	if err := t.Validate(); err != nil {
		return Trace{}, err
	}

	return t, nil
}

// Write renders the trace as JSON lines, the shape ReadTrace reads.
func (t *Trace) Write(w io.Writer) error {
	enc := json.NewEncoder(w)

	for i := range t.Arrivals {
		if err := enc.Encode(&t.Arrivals[i]); err != nil {
			return fmt.Errorf("replay: write trace: %w", err)
		}
	}

	return nil
}
