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
			byHand(t, v, v.Samples, v.Processes, v.Steps, v.Tests, batchLimits)

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
			byHand(t, v, v.Samples, v.Processes, v.Steps, v.Tests, reportLimits)

			// THE ACCOUNT, by arithmetic: every number from first to last kept,
			// refused or missed, and the runs named in order, apart, inside, and no
			// more than are missing.
			var named uint64

			if v.FirstSeq < 1 || v.LastSeq < v.FirstSeq || v.Batches < 1 ||
				uint64(v.Batches)+uint64(v.Refused)+uint64(v.Missing) != v.LastSeq-v.FirstSeq+1 || len(v.Gaps) > MaxReportGaps {
				t.Fatalf("accepted the account %d..%d: %d kept, %d refused, %d missing, %d gaps",
					v.FirstSeq, v.LastSeq, v.Batches, v.Refused, v.Missing, len(v.Gaps))
			}

			for i, g := range v.Gaps {
				if g.FirstSeq <= v.FirstSeq || g.LastSeq >= v.LastSeq || g.LastSeq < g.FirstSeq ||
					i > 0 && g.FirstSeq <= v.Gaps[i-1].LastSeq+1 {
					t.Fatalf("accepted gap %d: %+v", i, g)
				}

				named += g.LastSeq - g.FirstSeq + 1
			}

			if named > uint64(v.Missing) || named < uint64(v.Missing) && len(v.Gaps) < MaxReportGaps {
				t.Fatalf("accepted %d missing with %d named", v.Missing, named)
			}

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

	if !slices.Contains(sentinels, e.Err) || !inVocabulary(e.Where) {
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

// limitsOf is what an accepted value of each kind may hold, from the exported
// constants that state the schema rather than the validator's own tables.
type limitsOf struct {
	samples, processes, rows, steps, suites, failed int
}

var (
	batchLimits  = limitsOf{MaxBatchSamples, MaxBatchProcessSamples, MaxBatchProcessSamples * MaxProcessRows, MaxBatchSteps, MaxBatchSuites, MaxBatchSuites * MaxFailedNames}
	reportLimits = limitsOf{MaxReportSamples, MaxReportProcessSamples, MaxReportRows, MaxReportSteps, MaxReportSuites, MaxReportFailedNames}
)

// byHand checks an accepted value without the validator: every number and every
// text by reflection, each list's bound, each time's order, each counter's.
func byHand(t *testing.T, v any, samples []Sample, processes []ProcessSample, steps []StepMark, tests []SuiteResult, l limitsOf) {
	t.Helper()

	everyField(t, reflect.ValueOf(v), "")

	rows, failed := 0, 0
	for _, p := range processes {
		rows += len(p.Rows)
	}

	for _, s := range tests {
		failed += len(s.Failed)
	}

	if len(samples) > l.samples || len(processes) > l.processes || rows > l.rows || len(steps) > l.steps ||
		len(tests) > l.suites || failed > l.failed {
		t.Fatalf("accepted %d samples, %d process samples, %d rows, %d steps, %d suites, %d failed names",
			len(samples), len(processes), rows, len(steps), len(tests), failed)
	}

	for i, s := range samples {
		if s.AtMillis <= 0 || s.CPUBusyTicks > s.CPUTotalTicks {
			t.Fatalf("accepted sample %d: %+v", i, s)
		}

		if i == 0 {
			continue
		}

		p := samples[i-1]
		if s.AtMillis <= p.AtMillis || s.CPUBusyTicks < p.CPUBusyTicks || s.CPUTotalTicks < p.CPUTotalTicks ||
			s.DiskReadBytes < p.DiskReadBytes || s.DiskWriteBytes < p.DiskWriteBytes ||
			s.NetRxBytes < p.NetRxBytes || s.NetTxBytes < p.NetTxBytes {
			t.Fatalf("accepted sample %d going back from %+v to %+v", i, p, s)
		}
	}

	for i, p := range processes {
		if p.AtMillis <= 0 || i > 0 && p.AtMillis <= processes[i-1].AtMillis {
			t.Fatalf("accepted process sample %d at %d", i, p.AtMillis)
		}

		if len(p.Rows) > MaxProcessRows || p.Other.Procs == 0 && p.Other != (ProcessUsage{}) {
			t.Fatalf("accepted process sample %d: %+v", i, p)
		}

		names := map[string]bool{}
		for _, row := range p.Rows {
			if len(row.Name) > MaxNameBytes || names[row.Name] || row.Procs < 1 {
				t.Fatalf("accepted the row %+v", row)
			}

			names[row.Name] = true
		}
	}

	for i, m := range steps {
		if len(m.Name) > MaxNameBytes || m.AtSeconds <= 0 || i > 0 && m.AtSeconds < steps[i-1].AtSeconds {
			t.Fatalf("accepted step %d: %+v", i, m)
		}
	}

	for _, s := range tests {
		if len(s.Name) > MaxNameBytes || len(s.Failed) > MaxFailedNames {
			t.Fatalf("accepted the suite %q with %d failed names", s.Name, len(s.Failed))
		}
	}
}

// everyField holds every number of an accepted value to [0, MaxValue] and every text
// to being set, UTF-8, free of control characters and within the longest bound.
func everyField(t *testing.T, v reflect.Value, path string) {
	t.Helper()

	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			everyField(t, v.Field(i), path+"."+v.Type().Field(i).Name)
		}
	case reflect.Slice:
		for i := range v.Len() {
			everyField(t, v.Index(i), path+"[]")
		}
	case reflect.Int64:
		if n := v.Int(); n < 0 || n > MaxValue {
			t.Fatalf("accepted %s = %d", path, n)
		}
	case reflect.Uint64:
		if n := v.Uint(); n > MaxValue {
			t.Fatalf("accepted %s = %d", path, n)
		}
	case reflect.String:
		s := v.String()
		if s == "" || len(s) > MaxFailedNameBytes || !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
			t.Fatalf("accepted %s = %q", path, s)
		}
	default:
		t.Fatalf("%s is a %s, which this walk does not check", path, v.Kind())
	}
}

// vocabulary is every name a refusal's location may use: the schema's own JSON
// names, read from its types.
var vocabulary = func() map[string]bool {
	names := map[string]bool{}

	var walk func(reflect.Type)

	walk = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Slice:
			walk(typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); name != "" {
					names[name] = true
				}

				walk(f.Type)
			}
		default:
		}
	}

	walk(reflect.TypeFor[Batch]())
	walk(reflect.TypeFor[Report]())

	return names
}()

// component is one step of a refusal's location: a name and perhaps an index.
var component = regexp.MustCompile(`^([a-z_]+)(\[\d+\])?$`)

// inVocabulary reports whether where is built only from the schema's names.
func inVocabulary(where string) bool {
	if where == "" {
		return true
	}

	for _, part := range strings.Split(where, ".") {
		m := component.FindStringSubmatch(part)
		if len(m) < 2 || !vocabulary[m[1]] {
			return false
		}
	}

	return true
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
