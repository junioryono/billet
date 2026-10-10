package guestreport

import (
	"bytes"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// repeated is a JSON list of n copies of elem.
func repeated(elem string, n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat(elem+",", n), ",") + "]"
}

// A DECODER REFUSES A LIST PAST ITS BOUND BEFORE IT ALLOCATES THE LIST. Each input
// compresses to a few kilobytes and inflates within its bound; decoded into typed
// slices first, the batch's would cost 26 MB and the report's 440 MB before any
// bound was checked. Not parallel: it reads the process's allocation counter, which
// a parallel test would move.
func TestTheDecodersRefuseALongListBeforeAllocatingIt(t *testing.T) {
	const budget = 48 << 20

	for _, c := range []struct {
		name   string
		report bool
		json   string
		err    error
		where  string
	}{
		{"a batch of 300,000 samples", false, `{"samples":` + repeated("{}", 300_000) + `}`, ErrTooMany, "samples"},
		{"a report of 2,500,000 samples", true, `{"samples":` + repeated("{}", 2_500_000) + `}`, ErrTooMany, "samples"},
		// The location names the list the key fills, never the key the guest wrote.
		{"a key in another case", true, `{"SAMPLES":` + repeated("{}", 2_500_000) + `}`, ErrTooMany, "samples"},
		{"a million rows in lists within their bound", true,
			`{"processes":` + repeated(`{"rows":`+repeated("{}", MaxProcessRows)+`}`, MaxReportProcessSamples) + `}`,
			ErrTooMany, "processes"},
		{"too many rows in one sample", false,
			`{"processes":[{},{"rows":` + repeated("{}", 100_000) + `}]}`, ErrTooMany, "processes[1].rows"},
		{"too many failed names", true,
			`{"tests":[{"failed":` + repeated(`""`, 1_000_000) + `}]}`, ErrTooMany, "tests[0].failed"},
		{"lists nested past the schema", true, strings.Repeat("[", 1_000_000), ErrMalformed, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := deflated(t, []byte(c.json))

			var before, after runtime.MemStats

			runtime.GC()
			runtime.ReadMemStats(&before)

			var err error
			if c.report {
				_, err = Decode(data)
			} else {
				_, err = DecodeBatch(data)
			}

			runtime.ReadMemStats(&after)

			check(t, err, c.err, c.where)

			spent := after.TotalAlloc - before.TotalAlloc
			if spent > budget {
				t.Fatalf("refusing %d bytes of JSON allocated %d bytes", len(c.json), spent)
			}

			t.Logf("refusing %d bytes of JSON allocated %d bytes", len(c.json), spent)
		})
	}
}

// THE SHAPE CHECK ADMITS EVERY LIST AT ITS BOUND, and counts a list nested where the
// schema puts it, whatever else surrounds it.
func TestTheShapeCheckAdmitsListsAtTheirBounds(t *testing.T) {
	t.Parallel()

	b := batchAt(3)
	for len(b.Samples) < MaxBatchSamples {
		s := b.Samples[len(b.Samples)-1]
		s.AtMillis++
		b.Samples = append(b.Samples, s)
	}

	for len(b.Steps) < MaxBatchSteps {
		b.Steps = append(b.Steps, b.Steps[0])
	}

	raw := marshal(t, b)
	if err := checkShape(raw, batchShape); err != nil {
		t.Fatalf("a batch at its bounds: %v", err)
	}

	if _, err := DecodeBatch(deflated(t, raw)); err != nil {
		t.Fatalf("a batch at its bounds: %v", err)
	}

	// An unknown key's list is not one of the schema's, and the typed decode
	// refuses the key without filling anything.
	unknown := bytes.Replace(raw, []byte(`{"seq"`), []byte(`{"other_list":`+repeated("1", 100_000)+`,"seq"`), 1)
	if _, err := DecodeBatch(deflated(t, unknown)); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("an unknown list: %v", err)
	}
}
