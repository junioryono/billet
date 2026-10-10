package guestreport

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// What a fuzz input's first byte chooses: the decoder, and whether the input is the
// encoding itself or the JSON inside it, which the harness compresses so the search
// mutates the schema rather than only the DEFLATE stream around it.
const (
	fuzzBatch  uint8 = 0
	fuzzReport uint8 = 1
	fuzzJSON   uint8 = 2
)

// sentinels is every reason a refusal may give.
var sentinels = []error{ErrMalformed, ErrTooLarge, ErrUnknownField, ErrInvalidUTF8,
	ErrNotCanonical, ErrNameTooLong, ErrControlCharacter, ErrEmpty, ErrTooMany,
	ErrNotMonotonic, ErrOutOfRange, ErrDuplicateName, ErrInconsistent}

// where is the shape of a refusal's location: the schema's own field names and
// indices, which nothing the guest wrote can extend.
var where = regexp.MustCompile(`^([a-z_]+(\[\d+\])?(\.[a-z_]+(\[\d+\])?)*)?$`)

// FuzzDecode holds both decoders to what a decoder of guest bytes must be. A
// refusal is an *Error with one of this package's reasons and a location built only
// from field names, so its text can quote nothing the guest wrote. An acceptance is
// checked against what does not share the decoder's code: a plain inflate and an
// ordinary json.Unmarshal of the same bytes must agree with it, the bytes must be
// exactly what json.Marshal writes for the value, every bound and order must hold
// when walked by hand, the value must encode again to the same value, and the same
// JSON with one unknown key must be refused as one. An accepted batch must also
// merge into a report Downsample can encode.
func FuzzDecode(f *testing.F) {
	seeds, err := filepath.Glob(filepath.Join("testdata", "fuzz", "FuzzDecode", "*"))
	if err != nil || len(seeds) < 6 {
		f.Fatalf("the committed seeds are missing (%d, %v)", len(seeds), err)
	}

	// A SEED SET THAT NEVER REACHES ACCEPTANCE proves nothing past the first
	// refusal, so the generated seeds must decode.
	r := mustMergeF(f, append(batchesThrough(4), batchAt(7)))

	batch, err := EncodeBatch(batchAt(3))
	if err != nil {
		f.Fatal(err)
	}

	report, err := Encode(r)
	if err != nil {
		f.Fatal(err)
	}

	for _, seed := range []struct {
		kind      uint8
		data, raw []byte
	}{
		{fuzzBatch, batch, marshal(f, batchAt(3))},
		{fuzzReport, report, marshal(f, r)},
	} {
		if !accepts(f, seed.kind, seed.data) || !accepts(f, seed.kind|fuzzJSON, seed.raw) {
			f.Fatalf("the seed for decoder %d does not decode", seed.kind)
		}

		f.Add(seed.kind, seed.data)
		f.Add(seed.kind|fuzzJSON, seed.raw)
	}

	f.Fuzz(func(t *testing.T, kind uint8, data []byte) {
		kind %= 4
		if kind&fuzzJSON != 0 {
			kind &^= fuzzJSON
			data = deflated(t, data)
		}

		var (
			value any
			err   error
		)

		switch kind {
		case fuzzBatch:
			value, err = DecodeBatch(data)
		default:
			value, err = Decode(data)
		}

		if err != nil {
			refusedAsDesigned(t, err)

			return
		}

		raw := plainInflate(t, data)
		if !utf8.Valid(raw) {
			t.Fatalf("accepted JSON that is not UTF-8: %q", raw)
		}

		ordinary := reflect.New(reflect.TypeOf(value))
		if err := json.Unmarshal(raw, ordinary.Interface()); err != nil {
			t.Fatalf("accepted %q, which json.Unmarshal refuses: %v", raw, err)
		}

		if !reflect.DeepEqual(ordinary.Elem().Interface(), value) {
			t.Fatalf("decoded %q as %+v, and json.Unmarshal as %+v", raw, value, ordinary.Elem().Interface())
		}

		if canon, err := json.Marshal(value); err != nil || !bytes.Equal(canon, raw) {
			t.Fatalf("accepted %q, which json.Marshal writes as %q (%v)", raw, canon, err)
		}

		switch v := value.(type) {
		case Batch:
			byHand(t, v.Samples, v.Processes, v.Steps, v.Tests, batchBounds)

			again, err := EncodeBatch(v)
			if err != nil {
				t.Fatalf("accepted a batch EncodeBatch refuses: %v", err)
			}

			if back, err := DecodeBatch(again); err != nil || !reflect.DeepEqual(back, v) {
				t.Fatalf("the batch did not survive encoding again (%v)", err)
			}

			r, err := Merge([]Batch{v})
			if err != nil {
				t.Fatalf("a decoded batch did not merge: %v", err)
			}

			if _, _, err := Downsample(r, MaxReportBytes); err != nil {
				t.Fatalf("a decoded batch merged into a report that cannot be encoded: %v", err)
			}
		case Report:
			byHand(t, v.Samples, v.Processes, v.Steps, v.Tests, reportBounds)

			again, err := Encode(v)
			if err != nil {
				t.Fatalf("accepted a report Encode refuses: %v", err)
			}

			if back, err := Decode(again); err != nil || !reflect.DeepEqual(back, v) {
				t.Fatalf("the report did not survive encoding again (%v)", err)
			}
		}

		// AN UNKNOWN KEY FIRST, which the decoder meets before anything else it
		// could refuse, unless the key itself takes the JSON past its bound.
		unknown := append([]byte(`{"fuzz_unknown":0,`), raw[1:]...)
		if err := decodeAs(kind, deflated(t, unknown)); !errors.Is(err, ErrUnknownField) && !errors.Is(err, ErrTooLarge) {
			t.Fatalf("decoded %q with an unknown key as %v", unknown, err)
		}
	})
}

func mustMergeF(f *testing.F, batches []Batch) Report {
	f.Helper()

	r, err := Merge(batches)
	if err != nil {
		f.Fatal(err)
	}

	return r
}

func decodeAs(kind uint8, data []byte) error {
	var err error

	if kind == fuzzBatch {
		_, err = DecodeBatch(data)
	} else {
		_, err = Decode(data)
	}

	return err
}

// accepts reports whether the decoder kind names admits data, compressing it first
// when kind says it is the JSON.
func accepts(tb testing.TB, kind uint8, data []byte) bool {
	tb.Helper()

	if kind&fuzzJSON != 0 {
		data = deflated(tb, data)
	}

	return decodeAs(kind&^fuzzJSON, data) == nil
}

func refusedAsDesigned(t *testing.T, err error) {
	t.Helper()

	e, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("refused with %T, not an *Error: %v", err, err)
	}

	if !slices.Contains(sentinels, e.Err) || !where.MatchString(e.Where) {
		t.Fatalf("refused with a reason or location outside the closed set: %q", err.Error())
	}
}

func plainInflate(t *testing.T, data []byte) []byte {
	t.Helper()

	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(data)))
	if err != nil {
		t.Fatalf("accepted bytes a plain inflate refuses: %v", err)
	}

	return raw
}

// byHand walks an accepted value's sections without the validator: each bound,
// each order, each text rule.
func byHand(t *testing.T, samples []Sample, processes []ProcessSample, steps []StepMark, tests []SuiteResult, b bounds) {
	t.Helper()

	text := func(s string, limit int) {
		t.Helper()

		if s == "" || len(s) > limit || strings.IndexFunc(s, unicode.IsControl) >= 0 {
			t.Fatalf("accepted the text %q", s)
		}
	}

	if len(samples) > b.samples || len(processes) > b.processes || len(steps) > b.steps || len(tests) > b.suites {
		t.Fatalf("accepted %d samples, %d process samples, %d steps, %d suites",
			len(samples), len(processes), len(steps), len(tests))
	}

	for i := 1; i < len(samples); i++ {
		if samples[i].AtMillis <= samples[i-1].AtMillis || samples[i].CPUTotalTicks < samples[i-1].CPUTotalTicks ||
			samples[i].NetRxBytes < samples[i-1].NetRxBytes {
			t.Fatalf("accepted sample %d going back", i)
		}
	}

	for i, p := range processes {
		if i > 0 && p.AtMillis <= processes[i-1].AtMillis {
			t.Fatalf("accepted process sample %d going back", i)
		}

		if len(p.Rows) > MaxProcessRows {
			t.Fatalf("accepted %d rows", len(p.Rows))
		}

		names := map[string]bool{}
		for _, row := range p.Rows {
			text(row.Name, MaxNameBytes)

			if names[row.Name] {
				t.Fatalf("accepted %q twice in one sample", row.Name)
			}

			names[row.Name] = true
		}
	}

	for i, m := range steps {
		text(m.Name, MaxNameBytes)

		if m.AtSeconds <= 0 || i > 0 && m.AtSeconds < steps[i-1].AtSeconds {
			t.Fatalf("accepted step %d at %d", i, m.AtSeconds)
		}
	}

	failed := 0

	for _, s := range tests {
		text(s.Name, MaxNameBytes)

		if len(s.Failed) > MaxFailedNames {
			t.Fatalf("accepted %d failed names in a suite", len(s.Failed))
		}

		failed += len(s.Failed)

		for _, name := range s.Failed {
			text(name, MaxFailedNameBytes)
		}
	}

	if failed > b.failedNames {
		t.Fatalf("accepted %d failed names", failed)
	}
}

// THE COMMITTED SEEDS ARE WHAT THEY SAY: a batch and a report the decoders accept,
// and each handed to the other decoder, which must refuse it as a stranger's
// fields. BILLET_WRITE_FUZZ_SEEDS=1 writes them from the encoders first, in the
// format `go test` reads a corpus entry in.
func TestTheFuzzSeedsAreCommitted(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("testdata", "fuzz", "FuzzDecode")

	batch, err := EncodeBatch(batchAt(3))
	if err != nil {
		t.Fatal(err)
	}

	merged := mustMerge(t, append(batchesThrough(4), batchAt(7)))

	report, err := Encode(merged)
	if err != nil {
		t.Fatal(err)
	}

	seeds := []struct {
		name string
		kind uint8
		data []byte
		want error
	}{
		{"batch", fuzzBatch, batch, nil},
		{"report", fuzzReport, report, nil},
		{"report-as-batch", fuzzBatch, report, ErrUnknownField},
		{"batch-as-report", fuzzReport, batch, ErrUnknownField},
		{"batch-json", fuzzBatch | fuzzJSON, marshal(t, batchAt(3)), nil},
		{"report-json", fuzzReport | fuzzJSON, marshal(t, merged), nil},
	}

	if os.Getenv("BILLET_WRITE_FUZZ_SEEDS") == "1" {
		for _, s := range seeds {
			body := fmt.Sprintf("go test fuzz v1\nbyte(%q)\n[]byte(%+q)\n", rune(s.kind), s.data)
			if err := os.WriteFile(filepath.Join(dir, s.name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, s := range seeds {
		body, err := os.ReadFile(filepath.Join(dir, s.name))
		if err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}

		lines := strings.Split(string(body), "\n")
		if len(lines) != 4 || lines[0] != "go test fuzz v1" || lines[1] != fmt.Sprintf("byte(%q)", rune(s.kind)) ||
			!strings.HasPrefix(lines[2], "[]byte(") || !strings.HasSuffix(lines[2], ")") || lines[3] != "" {
			t.Fatalf("seed %s is not a corpus entry for decoder %d", s.name, s.kind)
		}

		data, err := strconv.Unquote(strings.TrimSuffix(strings.TrimPrefix(lines[2], "[]byte("), ")"))
		if err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}

		in := []byte(data)
		if s.kind&fuzzJSON != 0 {
			in = deflated(t, in)
		}

		if err := decodeAs(s.kind&^fuzzJSON, in); !errors.Is(err, s.want) || (s.want == nil) != (err == nil) {
			t.Fatalf("seed %s decodes with %v, want %v", s.name, err, s.want)
		}
	}
}
