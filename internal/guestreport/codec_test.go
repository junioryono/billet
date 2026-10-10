package guestreport

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestABatchRoundTrips(t *testing.T) {
	t.Parallel()

	for _, b := range []Batch{batchAt(1), batchAt(3), {Seq: 7, AgentVersion: "v1", TicksPerSecond: 100}} {
		data, err := EncodeBatch(b)
		if err != nil {
			t.Fatalf("encode batch %d: %v", b.Seq, err)
		}

		if len(data) > MaxBatchBytes {
			t.Fatalf("batch %d encodes to %d bytes, over %d", b.Seq, len(data), MaxBatchBytes)
		}

		got, err := DecodeBatch(data)
		if err != nil {
			t.Fatalf("decode batch %d: %v", b.Seq, err)
		}

		if !reflect.DeepEqual(got, b) {
			t.Fatalf("batch %d came back as\n%+v\nwant\n%+v", b.Seq, got, b)
		}
	}
}

func TestAReportRoundTrips(t *testing.T) {
	t.Parallel()

	r := mustMerge(t, append(batchesThrough(4), batchAt(9)))

	data, err := Encode(r)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got, r) {
		t.Fatalf("report came back as\n%+v\nwant\n%+v", got, r)
	}

	// THE TWO ENCODINGS ARE NOT INTERCHANGEABLE: a report is not a batch.
	if _, err := DecodeBatch(data); !errors.Is(err, ErrUnknownField) {
		t.Fatalf("a report decoded as a batch: %v", err)
	}
}

// marker is text a refusal must never repeat: the guest wrote it.
const marker = "GUESTWROTETHIS"

// refusal is a batch the encoder would refuse to write, built as JSON so the decoder
// sees it, and what each must answer. A JSON edit is the first from replaced by to,
// then json; the bytes decoded are the JSON compressed, then data.
type refusal struct {
	name     string
	edit     func(*Batch)
	from, to string
	json     func(raw []byte) []byte
	data     func(raw, compressed []byte) []byte
	err      error
	where    string
}

func batchRefusals() []refusal {
	long := strings.Repeat("x", MaxNameBytes) + marker
	unique := func(b *Batch) *ProcessSample { return &b.Processes[1] }
	named := func(b *Batch) { b.Steps[0].Name = "step " + marker }

	return []refusal{
		{name: "unknown field", from: `{"seq"`, to: `{"` + marker + `":1,"seq"`, err: ErrUnknownField},
		{name: "unknown nested field", from: `"procs"`, to: `"` + marker + `":1,"procs"`, err: ErrUnknownField},
		{name: "invalid UTF-8", edit: named, from: marker, to: "\xff\xfe" + marker, err: ErrInvalidUTF8},
		{name: "an escaped lone surrogate", edit: named, from: marker, to: `\ud800` + marker, err: ErrNotCanonical},
		{name: "a key in another case", from: `"seq"`, to: `"SEQ"`, err: ErrNotCanonical},
		{name: "a repeated key", from: `{"seq":3,`, to: `{"seq":2,"seq":3,`, err: ErrNotCanonical},
		{name: "a missing field", from: `"agent_cpu_ticks":9,`, err: ErrNotCanonical},
		{name: "an empty list spelled out", edit: func(b *Batch) { b.Tests = nil }, from: `]}`, to: `],"tests":[]}`,
			err: ErrNotCanonical},
		{name: "whitespace", json: func(raw []byte) []byte { return append(raw, '\n') }, err: ErrNotCanonical},
		{name: "a second value", json: func(raw []byte) []byte { return append(raw, "{}"...) }, err: ErrMalformed},
		{name: "a fraction", from: `"agent_cpu_ticks":9`, to: `"agent_cpu_ticks":9.5`, err: ErrMalformed},
		{name: "not JSON", json: func([]byte) []byte { return []byte(marker) }, err: ErrMalformed},
		{name: "not DEFLATE", data: func(raw, _ []byte) []byte { return raw }, err: ErrMalformed},
		{name: "bytes after the stream", data: func(_, d []byte) []byte { return append(d, 0) }, err: ErrMalformed},
		{name: "a truncated stream", data: func(_, d []byte) []byte { return d[:len(d)-1] }, err: ErrMalformed},
		{name: "over the encoded bound", data: func(_, _ []byte) []byte { return make([]byte, MaxBatchBytes+1) },
			err: ErrTooLarge},
		{name: "over the inflated bound", json: func(raw []byte) []byte {
			return append(raw, bytes.Repeat([]byte(" "), MaxBatchInflatedBytes)...)
		}, err: ErrTooLarge},

		{name: "seq zero", edit: func(b *Batch) { b.Seq = 0 }, err: ErrEmpty, where: "seq"},
		{name: "seq past 2^53", edit: func(b *Batch) { b.Seq = MaxValue + 1 }, err: ErrOutOfRange, where: "seq"},
		{name: "no agent version", edit: func(b *Batch) { b.AgentVersion = "" }, err: ErrEmpty, where: "agent_version"},
		{name: "a long agent version", edit: func(b *Batch) { b.AgentVersion = long }, err: ErrNameTooLong, where: "agent_version"},
		{name: "no tick rate", edit: func(b *Batch) { b.TicksPerSecond = 0 }, err: ErrOutOfRange, where: "ticks_per_second"},
		{name: "a tick rate past its bound", edit: func(b *Batch) { b.TicksPerSecond = MaxTicksPerSecond + 1 },
			err: ErrOutOfRange, where: "ticks_per_second"},
		{name: "negative agent CPU", edit: func(b *Batch) { b.AgentCPUTicks = -1 }, err: ErrOutOfRange, where: "agent_cpu_ticks"},

		{name: "too many samples", edit: func(b *Batch) {
			for len(b.Samples) <= MaxBatchSamples {
				s := b.Samples[len(b.Samples)-1]
				s.AtMillis++
				b.Samples = append(b.Samples, s)
			}
		}, err: ErrTooMany, where: "samples"},
		{name: "a sample at time zero", edit: func(b *Batch) { b.Samples[0].AtMillis = 0 },
			err: ErrEmpty, where: "samples[0].at_ms"},
		{name: "a sample at its predecessor's time", edit: func(b *Batch) { b.Samples[4].AtMillis = b.Samples[3].AtMillis },
			err: ErrNotMonotonic, where: "samples[4].at_ms"},
		{name: "a sample before its predecessor", edit: func(b *Batch) { b.Samples[4].AtMillis = b.Samples[3].AtMillis - 1 },
			err: ErrNotMonotonic, where: "samples[4].at_ms"},
		{name: "a counter that falls", edit: func(b *Batch) { b.Samples[4].NetTxBytes = b.Samples[3].NetTxBytes - 1 },
			err: ErrNotMonotonic, where: "samples[4].net_tx_bytes"},
		{name: "a level that falls", edit: func(b *Batch) { b.Samples[4].MemCachedBytes = 0 }},
		{name: "busy over total", edit: func(b *Batch) { b.Samples[2].CPUBusyTicks = b.Samples[2].CPUTotalTicks + 1 },
			err: ErrInconsistent, where: "samples[2].cpu_busy_ticks"},
		{name: "a negative level", edit: func(b *Batch) { b.Samples[2].MemUsedBytes = -1 },
			err: ErrOutOfRange, where: "samples[2].mem_used_bytes"},
		{name: "a counter past 2^53", edit: func(b *Batch) { b.Samples[9].DiskReadBytes = MaxValue + 1 },
			err: ErrOutOfRange, where: "samples[9].disk_read_bytes"},

		{name: "too many process samples", edit: func(b *Batch) {
			for len(b.Processes) <= MaxBatchProcessSamples {
				p := b.Processes[len(b.Processes)-1]
				p.AtMillis++
				b.Processes = append(b.Processes, p)
			}
		}, err: ErrTooMany, where: "processes"},
		{name: "process samples out of order", edit: func(b *Batch) { b.Processes[1].AtMillis = b.Processes[0].AtMillis },
			err: ErrNotMonotonic, where: "processes[1].at_ms"},
		{name: "too many rows", edit: func(b *Batch) {
			p := unique(b)
			for i := len(p.Rows); i <= MaxProcessRows; i++ {
				p.Rows = append(p.Rows, ProcessRow{Name: "p" + string(rune('A'+i%26)) + string(rune('A'+i/26)),
					ProcessUsage: ProcessUsage{Procs: 1}})
			}
		}, err: ErrTooMany, where: "processes[1].rows"},
		{name: "a long process name", edit: func(b *Batch) { unique(b).Rows[0].Name = long },
			err: ErrNameTooLong, where: "processes[1].rows[0].name"},
		{name: "an empty process name", edit: func(b *Batch) { unique(b).Rows[0].Name = "" },
			err: ErrEmpty, where: "processes[1].rows[0].name"},
		{name: "a control character", edit: func(b *Batch) { unique(b).Rows[0].Name = marker + "\x1b[2J" },
			err: ErrControlCharacter, where: "processes[1].rows[0].name"},
		{name: "a C1 control character", edit: func(b *Batch) { unique(b).Rows[0].Name = marker + "\u009b2J" },
			err: ErrControlCharacter, where: "processes[1].rows[0].name"},
		{name: "a name twice", edit: func(b *Batch) { unique(b).Rows[1].Name = unique(b).Rows[0].Name },
			err: ErrDuplicateName, where: "processes[1].rows[1].name"},
		{name: "a row of no processes", edit: func(b *Batch) { unique(b).Rows[0].Procs = 0 },
			err: ErrEmpty, where: "processes[1].rows[0].procs"},
		{name: "a negative row", edit: func(b *Batch) { unique(b).Rows[0].WriteBytes = -1 },
			err: ErrOutOfRange, where: "processes[1].rows[0].write_bytes"},
		{name: "an other row of no processes with usage", edit: func(b *Batch) { unique(b).Other = ProcessUsage{CPUTicks: 1} },
			err: ErrInconsistent, where: "processes[1].other"},
		{name: "a negative residual", edit: func(b *Batch) { unique(b).ResidualCPUTicks = -1 },
			err: ErrOutOfRange, where: "processes[1].residual_cpu_ticks"},

		{name: "too many steps", edit: func(b *Batch) {
			for len(b.Steps) <= MaxBatchSteps {
				b.Steps = append(b.Steps, b.Steps[0])
			}
		}, err: ErrTooMany, where: "steps"},
		{name: "a long step name", edit: func(b *Batch) { b.Steps[0].Name = long },
			err: ErrNameTooLong, where: "steps[0].name"},
		{name: "a step at time zero", edit: func(b *Batch) { b.Steps[0].AtSeconds = 0 },
			err: ErrEmpty, where: "steps[0].at_s"},
		{name: "two steps in one second", edit: func(b *Batch) { b.Steps = append(b.Steps, b.Steps[0]) }},
		{name: "a step before its predecessor", edit: func(b *Batch) {
			b.Steps = append(b.Steps, StepMark{Name: "earlier", AtSeconds: b.Steps[0].AtSeconds - 1})
		}, err: ErrNotMonotonic, where: "steps[1].at_s"},

		{name: "too many suites", edit: func(b *Batch) {
			for len(b.Tests) <= MaxBatchSuites {
				b.Tests = append(b.Tests, SuiteResult{Name: "s", Tests: 1})
			}
		}, err: ErrTooMany, where: "tests"},
		{name: "a long suite name", edit: func(b *Batch) { b.Tests[0].Name = long },
			err: ErrNameTooLong, where: "tests[0].name"},
		{name: "a negative count", edit: func(b *Batch) { b.Tests[0].Skipped = -1 },
			err: ErrOutOfRange, where: "tests[0].skipped"},
		{name: "too many failed names", edit: func(b *Batch) {
			for len(b.Tests[0].Failed) <= MaxFailedNames {
				b.Tests[0].Failed = append(b.Tests[0].Failed, "TestX")
			}
		}, err: ErrTooMany, where: "tests[0].failed"},
		{name: "a failed name at its bound", edit: func(b *Batch) {
			b.Tests[0].Failed[0] = strings.Repeat("y", MaxFailedNameBytes)
		}},
		{name: "a long failed name", edit: func(b *Batch) {
			b.Tests[0].Failed[0] = strings.Repeat("y", MaxFailedNameBytes) + marker
		}, err: ErrNameTooLong, where: "tests[0].failed[0]"},
		{name: "an empty failed name", edit: func(b *Batch) { b.Tests[0].Failed[0] = "" },
			err: ErrEmpty, where: "tests[0].failed[0]"},
	}
}

// EACH REFUSAL IS TYPED, NAMES WHERE, AND QUOTES NOTHING THE GUEST WROTE. Every case
// is decoded from bytes, so the decoder is what refuses; a case the encoder could
// write is also refused by the encoder, with the same answer, so neither side can
// write what the other will not read. A case with no error is the boundary that
// must still be admitted.
func TestEachRefusalIsTyped(t *testing.T) {
	t.Parallel()

	for _, c := range batchRefusals() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			b := batchAt(3)
			if c.edit != nil {
				c.edit(&b)
			}

			raw := marshal(t, b)
			if c.from != "" {
				raw = replaced(t, raw, []byte(c.from), []byte(c.to))
			}

			if c.json != nil {
				raw = c.json(raw)
			}

			data := deflated(t, raw)
			if c.data != nil {
				data = c.data(raw, data)
			}

			_, err := DecodeBatch(data)
			check(t, err, c.err, c.where)

			if c.from == "" && c.json == nil && c.data == nil {
				_, err := EncodeBatch(b)
				check(t, err, c.err, c.where)
			}
		})
	}
}

func check(t *testing.T, err, want error, where string) {
	t.Helper()

	if want == nil {
		if err != nil {
			t.Fatalf("refused: %v", err)
		}

		return
	}

	if !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}

	e, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("%v is not an *Error", err)
	}

	if e.Where != where {
		t.Fatalf("refused at %q, want %q", e.Where, where)
	}

	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "GUEST") {
		t.Fatalf("the refusal quotes what the guest wrote: %v", err)
	}
}

// reportRefusals is the account a Report gives of its batches, broken one way each.
func TestAReportsAccountIsChecked(t *testing.T) {
	t.Parallel()

	// Batches 1, 2, 5, 6 and 9: two gaps, 3-4 and 7-8.
	base := mustMerge(t, []Batch{batchAt(1), batchAt(2), batchAt(5), batchAt(6), batchAt(9)})

	for _, c := range []struct {
		name  string
		edit  func(*Report)
		err   error
		where string
	}{
		{"as merged", func(*Report) {}, nil, ""},
		{"first seq zero", func(r *Report) { r.FirstSeq = 0 }, ErrEmpty, "first_seq"},
		{"last before first", func(r *Report) { r.LastSeq = 0 }, ErrInconsistent, "last_seq"},
		{"last past 2^53", func(r *Report) { r.LastSeq = MaxValue + 1 }, ErrOutOfRange, "last_seq"},
		{"no batch kept", func(r *Report) { r.Batches = 0; r.Refused = 5 }, ErrEmpty, "batches"},
		{"negative refused", func(r *Report) { r.Refused = -1; r.Batches = 6 }, ErrOutOfRange, "refused"},
		{"negative conflicts", func(r *Report) { r.Conflicts = -1 }, ErrOutOfRange, "conflicts"},
		{"a miscount", func(r *Report) { r.Missing = 3 }, ErrInconsistent, "missing"},
		{"a gap before the first", func(r *Report) { r.Gaps[0].FirstSeq = 1 }, ErrInconsistent, "gaps[0]"},
		{"a gap at the last", func(r *Report) {
			r.Gaps[1].LastSeq = 9
			r.LastSeq = 9
		}, ErrInconsistent, "gaps[1]"},
		{"a gap ending before it starts", func(r *Report) { r.Gaps[0].LastSeq = 2 }, ErrInconsistent, "gaps[0]"},
		{"two gaps that touch", func(r *Report) { r.Gaps[1].FirstSeq = 5 }, ErrInconsistent, "gaps[1]"},
		{"gaps naming more than is missing", func(r *Report) { r.Gaps[1].LastSeq = 8; r.Missing = 3; r.Batches = 6 },
			ErrInconsistent, "gaps"},
		{"gaps naming less than is missing", func(r *Report) { r.Gaps = r.Gaps[:1] }, ErrInconsistent, "gaps"},
		// A FULL LIST LEAVES THE UNNAMED NUMBERS AFTER ITS LAST RUN: every even
		// number from 2 to 128 named, so 129 is received, and one more missing
		// number needs room between 129 and LastSeq.
		{"an unnamed missing number with no room for it", func(r *Report) { fullGaps(r, 129) }, ErrInconsistent, "gaps"},
		{"an unnamed missing number with room for it", func(r *Report) { fullGaps(r, 131) }, nil, ""},
		{"two unnamed missing numbers with room for one", func(r *Report) {
			fullGaps(r, 131)
			r.Missing++
			r.Batches--
		}, ErrInconsistent, "gaps"},
		{"too many gaps", func(r *Report) {
			r.Gaps = make([]Gap, MaxReportGaps+1)
		}, ErrTooMany, "gaps"},
		{"negative saturated", func(r *Report) { r.Saturated = -1 }, ErrOutOfRange, "saturated"},
		{"stride zero", func(r *Report) { r.Stride = 0 }, ErrOutOfRange, "stride"},
		{"stride past its bound", func(r *Report) { r.Stride = MaxStride + 1 }, ErrOutOfRange, "stride"},
		{"negative drops", func(r *Report) { r.Dropped.FailedNames = -1 }, ErrOutOfRange, "dropped.failed_names"},
		// The two suites merged hold one name each; the sixth suite added, tests[7],
		// takes the count past MaxReportFailedNames.
		{"too many failed names", func(r *Report) {
			full := slices.Repeat([]string{"TestX"}, MaxFailedNames)
			for range 6 {
				r.Tests = append(r.Tests, SuiteResult{Name: "s", Failed: full})
			}
		}, ErrTooMany, "tests[7].failed"},
		{"failed names at their bound", func(r *Report) {
			full := slices.Repeat([]string{"TestX"}, MaxFailedNames)
			for range 5 {
				r.Tests = append(r.Tests, SuiteResult{Name: "s", Failed: full})
			}

			r.Tests = append(r.Tests, SuiteResult{Name: "s", Failed: full[:MaxReportFailedNames-2-5*MaxFailedNames]})
		}, nil, ""},
		{"too many steps", func(r *Report) {
			for len(r.Steps) <= MaxReportSteps {
				r.Steps = append(r.Steps, r.Steps[len(r.Steps)-1])
			}
		}, ErrTooMany, "steps"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			r := base
			r.Gaps = append([]Gap(nil), base.Gaps...)
			r.Tests = append([]SuiteResult(nil), base.Tests...)
			r.Steps = append([]StepMark(nil), base.Steps...)
			c.edit(&r)

			_, err := Decode(deflated(t, marshal(t, r)))
			check(t, err, c.err, c.where)

			_, err = Encode(r)
			check(t, err, c.err, c.where)
		})
	}
}

// fullGaps gives r the account of numbers 1 to last with every even number from 2 to
// 2*MaxReportGaps missing and named, and one more missing number the list leaves
// unnamed.
func fullGaps(r *Report, last uint64) {
	r.FirstSeq, r.LastSeq, r.Refused, r.Gaps = 1, last, 0, nil
	for i := range uint64(MaxReportGaps) {
		r.Gaps = append(r.Gaps, Gap{2 * (i + 1), 2 * (i + 1)})
	}

	r.Missing = MaxReportGaps + 1
	r.Batches = int64(last) - r.Missing
}

// A REPORT PAST MaxReportRows IS REFUSED BY THE ENCODER, every sample within its own
// bound; its JSON is past the inflated bound too, so the decoder never sees one.
func TestEncodeRefusesTooManyRowsInAll(t *testing.T) {
	t.Parallel()

	r := Report{AgentVersion: "v1", TicksPerSecond: 100, FirstSeq: 1, LastSeq: 1, Batches: 1, Stride: 1}

	var rows []ProcessRow
	for i := range MaxProcessRows {
		rows = append(rows, ProcessRow{Name: fmt.Sprintf("p%d", i), ProcessUsage: ProcessUsage{Procs: 1}})
	}

	for i := range MaxReportRows/MaxProcessRows + 1 {
		r.Processes = append(r.Processes, ProcessSample{AtMillis: start + int64(i), Rows: rows})
	}

	_, err := Encode(r)
	check(t, err, ErrTooMany, "processes")

	// DOWNSAMPLE HALVES IT UNDER THE BOUND rather than hand back the refusal.
	got, _, err := Downsample(r, MaxReportBytes)
	if err != nil || got.Stride < 2 {
		t.Fatalf("downsampled to stride %d: %v", got.Stride, err)
	}

	r.Processes = r.Processes[:MaxReportRows/MaxProcessRows]
	if err := validateReport(&r); err != nil {
		t.Fatalf("a report at the bound: %v", err)
	}
}

// TEXT THAT IS NOT UTF-8 IS REFUSED BY THE ENCODER, which would otherwise write it
// as U+FFFD and hand its decoder other text than it was given.
func TestTheEncodersRefuseTextThatIsNotUTF8(t *testing.T) {
	t.Parallel()

	b := batchAt(3)
	b.AgentVersion = "v\xff"

	_, err := EncodeBatch(b)
	check(t, err, ErrInvalidUTF8, "agent_version")

	r := mustMerge(t, batchesThrough(3))
	r.Tests[0].Failed[0] = "Test\xc3"

	_, err = Encode(r)
	check(t, err, ErrInvalidUTF8, "tests[0].failed[0]")
}

// THE ENCODERS REFUSE WHAT THEIR DECODERS WOULD: a batch at its bounds that does not
// compress (the agent splits it), a report of counters that do not compress (the
// node halves it), and JSON past the inflated bound however well it compresses.
func TestTheEncodersRefuseWhatWouldNotDecode(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(7, 8))
	text := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(byte('!' + rng.IntN(94)))
		}

		return b.String()
	}

	b := batchAt(1)
	b.Tests = nil

	for range MaxBatchSuites {
		s := SuiteResult{Name: "suite", Tests: MaxFailedNames}
		for range MaxFailedNames {
			s.Failed = append(s.Failed, text(MaxFailedNameBytes))
		}

		b.Tests = append(b.Tests, s)
	}

	if _, err := EncodeBatch(b); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a batch of %d random failed names encoded: %v", MaxBatchSuites*MaxFailedNames, err)
	}

	r := Report{AgentVersion: "v1", TicksPerSecond: 100, FirstSeq: 1, LastSeq: 1, Batches: 1, Stride: 1}

	var s Sample
	for i := range 8000 {
		s.AtMillis = start + int64(i)*1000
		s.CPUTotalTicks += 1<<20 + rng.Int64N(1<<40)
		s.CPUBusyTicks = s.CPUTotalTicks - rng.Int64N(1<<20)
		s.DiskReadBytes += rng.Int64N(1 << 40)
		s.DiskWriteBytes += rng.Int64N(1 << 40)
		s.NetRxBytes += rng.Int64N(1 << 40)
		s.NetTxBytes += rng.Int64N(1 << 40)
		s.MemUsedBytes, s.MemAvailableBytes, s.MemCachedBytes = rng.Int64N(1<<40), rng.Int64N(1<<40), rng.Int64N(1<<40)
		r.Samples = append(r.Samples, s)
	}

	if _, err := Encode(r); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a report of %d random samples encoded: %v", len(r.Samples), err)
	}

	if got, data, err := Downsample(r, MaxReportBytes); err != nil || len(data) > MaxReportBytes || got.Stride < 2 {
		t.Fatalf("halved to stride %d in %d bytes: %v", got.Stride, len(data), err)
	}

	small := batchAt(1)
	if _, err := encode(&small, MaxBatchBytes, len(marshal(t, small))-1); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("JSON a byte past the inflated bound encoded: %v", err)
	}

	if _, err := encode(&small, MaxBatchBytes, len(marshal(t, small))); err != nil {
		t.Fatalf("JSON at the inflated bound refused: %v", err)
	}
}
