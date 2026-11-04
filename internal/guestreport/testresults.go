package guestreport

import (
	"cmp"
	"errors"
	"slices"
)

// The status of one test case, as the result file says.
const (
	CasePassed  = "passed"
	CaseFailed  = "failed"
	CaseErrored = "errored"
	CaseSkipped = "skipped"
)

// SuiteCases is one test suite's cases, which a batch of SchemaCases carries in the
// agent's last batch beside the suite counts of SchemaBase.
//
// Complete says Cases holds every case the suite counted, so a name the list does
// not hold did not run in it. Without it a name's absence says nothing.
type SuiteCases struct {
	Name     string `json:"name"`
	Tests    int64  `json:"tests"`
	Failures int64  `json:"failures"`
	Errors   int64  `json:"errors"`
	Skipped  int64  `json:"skipped"`
	Complete bool   `json:"complete"`
	// Cases are in the result file's order, or what is left of it.
	Cases []CaseResult `json:"cases,omitempty"`
}

// CaseResult is one test case: its class and its name, what it came to, how long
// the result file says it took (nil where it does not say, never zero), and the
// message of its failure or error, cut to MaxCaseMessageBytes.
//
// THE MESSAGE IS WHAT THE JOB WROTE TO A FILE, PAST GITHUB'S MASKING. The runner
// masks what reaches the job's log and not what a test writes to disk, so a message
// can hold a secret a test printed: it is an operator's to read, and goes nowhere
// else.
type CaseResult struct {
	Name           string `json:"name"`
	Status         string `json:"status"`
	DurationMillis *int64 `json:"duration_ms,omitempty"`
	Message        string `json:"message,omitempty"`
}

// Failing reports whether the case failed or errored.
func (c *CaseResult) Failing() bool { return c.Status == CaseFailed || c.Status == CaseErrored }

// TestResults is a job's suites and cases as the ledger keeps them, past the guest
// report's own retention. Dropped counts what the bounds left out.
type TestResults struct {
	Suites  []SuiteCases `json:"suites,omitempty"`
	Dropped DroppedCases `json:"dropped"`
}

// DroppedCases counts the suites and cases a job's results left out past the
// bounds, a dropped suite's cases with it.
type DroppedCases struct {
	Suites int64 `json:"suites"`
	Cases  int64 `json:"cases"`
}

// CountCases is how many cases suites hold in all.
func CountCases(suites []SuiteCases) int {
	n := 0
	for i := range suites {
		n += len(suites[i].Cases)
	}

	return n
}

// TestResultsOf is the suites a job's batches carried, in the order they were
// received and within the bounds of one TestResults, and what the agent or the bounds
// left out; false when they carried nothing. A number received twice counts once, as
// Merge counts it, the first received kept.
func TestResultsOf(batches []Batch) (TestResults, bool) {
	var out TestResults

	seen := make(map[uint64]bool, len(batches))
	held := 0

	for i := range batches {
		b := &batches[i]
		if seen[b.Seq] {
			continue
		}

		seen[b.Seq] = true
		out.Dropped.Suites = min(out.Dropped.Suites+b.CasesDropped.Suites, MaxValue)
		out.Dropped.Cases = min(out.Dropped.Cases+b.CasesDropped.Cases, MaxValue)

		for j := range b.Cases {
			s := b.Cases[j]

			if len(out.Suites) == MaxBatchCaseSuites {
				out.Dropped.Suites++
				out.Dropped.Cases += int64(len(s.Cases))

				continue
			}

			if room := MaxBatchCases - held; len(s.Cases) > room {
				out.Dropped.Cases += int64(len(s.Cases) - room)
				s.Cases, s.Complete = s.Cases[:room], false
			}

			held += len(s.Cases)

			s.Cases = slices.Clone(s.Cases)
			if len(s.Cases) == 0 {
				s.Cases = nil
			}

			out.Suites = append(out.Suites, s)
		}
	}

	return out, len(out.Suites) > 0 || out.Dropped != (DroppedCases{})
}

// EncodeTestResults is r in Codec within limit bytes, giving up cases with ShedCases
// until it fits, and answers what it encoded: what it gave up is counted in Dropped.
// The caller's r is not changed.
func EncodeTestResults(r TestResults, limit int) (TestResults, []byte, error) {
	r.Suites = cloneSuites(r.Suites)

	for {
		if err := validateTestResults(&r); err != nil {
			return TestResults{}, nil, err
		}

		data, err := encode(&r, min(limit, MaxTestResultsBytes), MaxTestResultsInflatedBytes)
		if err == nil {
			return r, data, nil
		}

		suites, cases := len(r.Suites), CountCases(r.Suites)
		if !errors.Is(err, ErrTooLarge) || !ShedCases(&r.Suites) {
			return TestResults{}, nil, err
		}

		r.Dropped.Suites += int64(suites - len(r.Suites))
		r.Dropped.Cases += int64(cases - CountCases(r.Suites))
	}
}

// DecodeTestResults reverses EncodeTestResults, refusing anything it would not have
// written.
func DecodeTestResults(data []byte) (TestResults, error) {
	var r TestResults

	raw, err := decode(data, &r, MaxTestResultsBytes, MaxTestResultsInflatedBytes, testResultsShape)
	if err != nil {
		return TestResults{}, err
	}

	if err := validateTestResults(&r); err != nil {
		return TestResults{}, err
	}

	if err := canonical(raw, &r); err != nil {
		return TestResults{}, err
	}

	return r, nil
}

// cloneSuites is suites with case lists of their own, so giving cases up leaves the
// caller's as they were.
func cloneSuites(suites []SuiteCases) []SuiteCases {
	out := slices.Clone(suites)
	for i := range out {
		out[i].Cases = slices.Clone(out[i].Cases)
	}

	return out
}

// ShedCases gives up the least worth keeping of suites, and reports false when
// nothing is left to give up. A failing case says more than a passing one and a slow
// one more than a fast one, so each call gives up the first of these it finds:
//
//   - the fastest half of the cases that did not fail, a case with no duration first;
//   - every failure's message;
//   - the later half of each suite's failing cases;
//   - the later half of the suites, which by then hold no case.
//
// A suite that loses a case is no longer Complete. Halving bounds the calls by about
// twice the log of what was held, so a caller encoding after each call spends little
// more than twice its first encoding.
func ShedCases(suites *[]SuiteCases) bool {
	if shedPassing(*suites) || shedMessages(*suites) {
		return true
	}

	return shedRest(suites)
}

// ShedCaseCount is ShedCases for a bound on how many cases are held, which clearing a
// message does nothing for: every call that can gives up cases.
func ShedCaseCount(suites *[]SuiteCases) bool {
	if shedPassing(*suites) {
		return true
	}

	return shedRest(suites)
}

// shedRest gives up the later half of each suite's cases or, when they hold none, the
// later half of the suites.
func shedRest(suites *[]SuiteCases) bool {
	switch {
	case shedFailing(*suites):
	case len(*suites) > 0:
		*suites = slices.Clip((*suites)[:len(*suites)/2])
		if len(*suites) == 0 {
			*suites = nil
		}
	default:
		return false
	}

	return true
}

// shedPassing drops the fastest half of the cases that did not fail, rounded up so
// that one is dropped, and reports whether any was.
func shedPassing(suites []SuiteCases) bool {
	type place struct {
		suite, index int
		millis       int64
	}

	var passing []place

	for i := range suites {
		for j := range suites[i].Cases {
			c := &suites[i].Cases[j]
			if c.Failing() {
				continue
			}

			millis := int64(-1)
			if c.DurationMillis != nil {
				millis = *c.DurationMillis
			}

			passing = append(passing, place{i, j, millis})
		}
	}

	if len(passing) == 0 {
		return false
	}

	slices.SortStableFunc(passing, func(a, b place) int { return cmp.Compare(a.millis, b.millis) })

	drop := make(map[[2]int]bool, (len(passing)+1)/2)
	for _, p := range passing[:(len(passing)+1)/2] {
		drop[[2]int{p.suite, p.index}] = true
	}

	for i := range suites {
		s := &suites[i]
		kept := make([]CaseResult, 0, len(s.Cases))

		for j := range s.Cases {
			if !drop[[2]int{i, j}] {
				kept = append(kept, s.Cases[j])
			}
		}

		if len(kept) != len(s.Cases) {
			s.Complete = false
		}

		s.Cases = kept
		if len(s.Cases) == 0 {
			s.Cases = nil
		}
	}

	return true
}

// shedMessages clears every failure's message and reports whether any had one.
func shedMessages(suites []SuiteCases) bool {
	shed := false

	for i := range suites {
		for j := range suites[i].Cases {
			if suites[i].Cases[j].Message != "" {
				suites[i].Cases[j].Message = ""
				shed = true
			}
		}
	}

	return shed
}

// shedFailing halves each suite's cases, every one of which is failing by the time
// it is called, and reports whether any suite held one.
func shedFailing(suites []SuiteCases) bool {
	shed := false

	for i := range suites {
		s := &suites[i]
		if n := len(s.Cases); n > 0 {
			s.Cases = slices.Clip(s.Cases[:n/2])
			if n/2 == 0 {
				s.Cases = nil
			}

			s.Complete = false
			shed = true
		}
	}

	return shed
}

// validateTestResults holds a job's results to the bounds of one batch's.
func validateTestResults(r *TestResults) error {
	if err := checkValue(r.Dropped.Suites, "dropped.suites"); err != nil {
		return err
	}

	if err := checkValue(r.Dropped.Cases, "dropped.cases"); err != nil {
		return err
	}

	return validateCases(r.Suites, "suites")
}

// validateCases holds suites to their bounds and each case to its text, a status
// from the closed set, and a message only on a case that failed. A suite's counts
// are the guest's and are not checked against its list, except that a Complete list
// names as many cases as the suite counted, so the word means what a reader takes it
// to.
func validateCases(suites []SuiteCases, where string) error {
	if len(suites) > MaxBatchCaseSuites {
		return refuse(ErrTooMany, where)
	}

	total := 0

	for i := range suites {
		s := &suites[i]
		suiteAt := at(where, i)

		if err := checkText(s.Name, MaxNameBytes, suiteAt+".name"); err != nil {
			return err
		}

		for _, c := range []struct {
			v    int64
			name string
		}{
			{s.Tests, "tests"}, {s.Failures, "failures"}, {s.Errors, "errors"}, {s.Skipped, "skipped"},
		} {
			if err := checkValue(c.v, suiteAt+"."+c.name); err != nil {
				return err
			}
		}

		total += len(s.Cases)
		if total > MaxBatchCases {
			return refuse(ErrTooMany, suiteAt+".cases")
		}

		if s.Complete && int64(len(s.Cases)) != s.Tests {
			return refuse(ErrInconsistent, suiteAt+".complete")
		}

		for j := range s.Cases {
			if err := validateCase(&s.Cases[j], at(suiteAt+".cases", j)); err != nil {
				return err
			}
		}
	}

	return nil
}

func validateCase(c *CaseResult, where string) error {
	if err := checkText(c.Name, MaxFailedNameBytes, where+".name"); err != nil {
		return err
	}

	switch c.Status {
	case CasePassed, CaseFailed, CaseErrored, CaseSkipped:
	default:
		// NOT QUOTED, like every refusal here: the guest wrote it.
		return refuse(ErrOutOfRange, where+".status")
	}

	if c.DurationMillis != nil {
		if err := checkValue(*c.DurationMillis, where+".duration_ms"); err != nil {
			return err
		}
	}

	if c.Message == "" {
		return nil
	}

	if !c.Failing() {
		return refuse(ErrInconsistent, where+".message")
	}

	return checkText(c.Message, MaxCaseMessageBytes, where+".message")
}
