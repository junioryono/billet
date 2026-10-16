package usage

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// Point is one sample of a job's cumulative counters, at an offset from the
// first sample. Every column is cumulative, so a rate is a difference of two
// points and a downsampled series loses resolution but never a total.
type Point struct {
	OffsetMillis  int64
	CPUUsage      int64 // µs
	MemoryCurrent int64 // bytes, the one column that is a level, not a total
	DiskRead      int64 // bytes
	DiskWrite     int64
	NetRx         int64 // bytes, guest view
	NetTx         int64
	GuestCPU      int64 // µs
	VMMCPU        int64
	EnergyActive  int64 // µJ attributed so far
	// Gap is why the sampler did not see the interval that ends at this point,
	// so what it holds is known only as that interval's total; GapNone when it
	// saw it. It is not one of SeriesColumns: a SeriesCodec series never marks
	// one, and a SeriesCodecClocked one marks one without its cause.
	Gap GapCause
}

// GapCause is why the sampler did not see an interval of a job's series. The
// set is closed: a series is refused when it names a cause this build does not
// know, and the node's metrics label gaps with these names.
type GapCause uint8

// The causes. The numbers are what SeriesCodecGaps stores, so they never
// change and a new cause takes the next number.
const (
	// GapNone is an interval the sampler saw.
	GapNone GapCause = iota
	// GapUnspecified is an interval marked by a series that records no cause
	// (SeriesCodecClocked).
	GapUnspecified
	// GapMissedTick is two intervals or more between two samples the sampler
	// kept with nothing else to say why: a tick it did not take.
	GapMissedTick
	// GapSlowRead is a read of the job that took a whole interval, so which of
	// its counters were read when is not known.
	GapSlowRead
	// GapFailedRead is a group the job had read before that a read could not:
	// that sample is not kept, since it would hold the group's last value at a
	// time it was not read.
	GapFailedRead
	// GapLateFirstRead is every interval before a group was first read, each of
	// which held that group at zero.
	GapLateFirstRead
	// GapCounterFell is an interval a cumulative counter fell across, and the
	// one after it, which holds the catch-up.
	GapCounterFell
	// GapFinalEnergy is the last interval of a job whose energy is shared from
	// the package: the final read takes no package reading.
	GapFinalEnergy
	// GapSamplerStuck is the last interval of a job whose final read the
	// sampler did not answer in time.
	GapSamplerStuck
	// GapHandover is the time between two node processes: the one that handed
	// the job over and the one that adopted it.
	GapHandover
	// GapSeveral is an interval made of intervals marked for different causes,
	// which halving a long series merges.
	GapSeveral

	gapCauses // one past the last cause
)

var gapCauseNames = [gapCauses]string{
	GapNone: "none", GapUnspecified: "unspecified", GapMissedTick: "missed_tick",
	GapSlowRead: "slow_read", GapFailedRead: "failed_read", GapLateFirstRead: "late_first_read",
	GapCounterFell: "counter_fell", GapFinalEnergy: "final_energy", GapSamplerStuck: "sampler_stuck",
	GapHandover: "handover", GapSeveral: "several",
}

// String is the cause's name, as `billet jobs show` and the metrics write it.
func (c GapCause) String() string {
	if c >= gapCauses {
		return fmt.Sprintf("cause-%d", uint8(c))
	}

	return gapCauseNames[c]
}

// GapCauses is every cause an interval can be marked with, GapNone aside.
func GapCauses() []GapCause {
	out := make([]GapCause, 0, gapCauses-1)
	for c := GapUnspecified; c < gapCauses; c++ {
		out = append(out, c)
	}

	return out
}

// joinGaps is the cause of an interval made of two, either of which may be
// GapNone: the one when only one is marked or both say the same, GapSeveral
// when they differ.
func joinGaps(a, b GapCause) GapCause {
	switch {
	case a == GapNone || a == b:
		return b
	case b == GapNone:
		return a
	default:
		return GapSeveral
	}
}

// SeriesColumns names Point's columns in the order the codec writes them.
var SeriesColumns = []string{"offset_ms", "cpu_usage_us", "memory_current_bytes",
	"disk_read_bytes", "disk_write_bytes", "net_rx_bytes", "net_tx_bytes",
	"guest_cpu_us", "vmm_cpu_us", "energy_active_uj"}

func (p Point) columns() []int64 {
	return []int64{p.OffsetMillis, p.CPUUsage, p.MemoryCurrent, p.DiskRead, p.DiskWrite,
		p.NetRx, p.NetTx, p.GuestCPU, p.VMMCPU, p.EnergyActive}
}

func pointFrom(c []int64) Point {
	return Point{OffsetMillis: c[0], CPUUsage: c[1], MemoryCurrent: c[2], DiskRead: c[3],
		DiskWrite: c[4], NetRx: c[5], NetTx: c[6], GuestCPU: c[7], VMMCPU: c[8], EnergyActive: c[9]}
}

// SeriesCodec is the encoding EncodeSeries writes: a column count and a point
// count, then each column's values as zig-zag varint deltas from the previous
// point, the whole compressed with DEFLATE.
const SeriesCodec = 1

// layout is what a codec writes after SeriesColumns: nothing, each point's gap
// mark, or each point's gap cause.
type layout int

const (
	unmarked layout = iota
	marked
	caused
)

// columns is how many columns the layout writes.
func (l layout) columns() int {
	if l == unmarked {
		return len(SeriesColumns)
	}

	return len(SeriesColumns) + 1
}

// name is the name of column col, for an error about it.
func (l layout) name(col int) string {
	switch {
	case col < len(SeriesColumns):
		return SeriesColumns[col]
	case l == caused:
		return "gap_cause"
	default:
		return "after_gap"
	}
}

// EncodeSeries encodes points into at most limit bytes, keeping every point
// if it fits and otherwise every second, fourth, ... point plus the last one,
// so the totals the last point carries are never lost. It reports the stride
// it kept.
func EncodeSeries(points []Point, limit int) ([]byte, int, error) {
	return encodeWithin(points, limit, func(kept []Point) ([]byte, error) {
		return encode(nil, kept, unmarked)
	})
}

// encodeWithin halves the points kept until enc fits them into limit bytes.
func encodeWithin(points []Point, limit int, enc func([]Point) ([]byte, error)) ([]byte, int, error) {
	if len(points) == 0 {
		return nil, 0, errors.New("usage: no points to encode")
	}
	for stride := 1; ; stride *= 2 {
		kept := downsample(points, stride)
		data, err := enc(kept)
		if err != nil {
			return nil, 0, err
		}
		if len(data) <= limit {
			return data, stride, nil
		}
		if len(kept) <= 2 {
			return nil, 0, fmt.Errorf("usage: two points encode to %d bytes, over the %d limit",
				len(data), limit)
		}
	}
}

// downsample keeps every stride-th point of the settled series and its last.
// A point dropped with a gap passes it to the point kept after it, whose
// interval now holds the one the sampler did not see; gaps of different causes
// merged into one interval are GapSeveral.
//
// SETTLED FIRST, so a counter fall, a fall between two samples at one offset
// included, is already a mark on the point it fell into and on the one after
// (settle), and dropping the points around it cannot hide it.
func downsample(points []Point, stride int) []Point {
	if stride == 1 {
		return points
	}
	points = settle(points)
	var out []Point
	gap := GapNone
	for i, p := range points {
		gap = joinGaps(gap, p.Gap)
		if i%stride != 0 && i != len(points)-1 {
			continue
		}
		p.Gap = gap
		gap = GapNone
		out = append(out, p)
	}

	return out
}

// encode writes prefix, then the points' columns in the layout, and
// compresses the whole.
func encode(prefix []byte, points []Point, l layout) ([]byte, error) {
	columns := l.columns()
	raw := prefix
	raw = binary.AppendUvarint(raw, uint64(columns))
	raw = binary.AppendUvarint(raw, uint64(len(points)))
	for col := range columns {
		var prev int64
		for _, p := range points {
			v := p.column(col, l)
			raw = binary.AppendVarint(raw, v-prev)
			prev = v
		}
	}

	var out bytes.Buffer
	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(raw); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

// column is the point's value of column col in the layout: one of
// SeriesColumns, then its mark (1 or 0) or its cause.
func (p Point) column(col int, l layout) int64 {
	switch {
	case col < len(SeriesColumns):
		return p.columns()[col]
	case l == caused:
		return int64(p.Gap)
	case p.Gap != GapNone:
		return 1
	default:
		return 0
	}
}

// maxDecodedSeries bounds what DecodeSeries will inflate, and maxDecodedPoints
// how many points it will allocate for, so a ledger row cannot make a reader
// allocate without limit. A monitor never keeps more than maxPoints plus the
// final one.
const (
	maxDecodedSeries = 64 << 20
	maxDecodedPoints = 2 * maxPoints
	// maxOffsetMillis is the largest offset a time.Duration holds, some 292
	// years.
	maxOffsetMillis = math.MaxInt64 / int64(time.Millisecond)
)

// DecodeSeries reverses EncodeSeries.
func DecodeSeries(data []byte) ([]Point, error) {
	raw, err := inflate(data)
	if err != nil {
		return nil, err
	}

	return decode(bytes.NewReader(raw), unmarked)
}

// inflate decompresses a series within maxDecodedSeries.
func inflate(data []byte) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(data)), maxDecodedSeries+1))
	if err != nil {
		return nil, fmt.Errorf("usage: inflate the series: %w", err)
	}
	if len(raw) > maxDecodedSeries {
		return nil, errors.New("usage: the series inflates past its bound")
	}

	return raw, nil
}

// decode reads the columns encode wrote in the layout, to the end of r. A
// mark read back is GapUnspecified, since the layout that writes one records
// no cause.
func decode(r *bytes.Reader, l layout) ([]Point, error) {
	want := l.columns()
	columns, err := binary.ReadUvarint(r)
	if err != nil || columns != uint64(want) {
		return nil, fmt.Errorf("usage: the series has %d columns, want %d", columns, want)
	}
	n, err := binary.ReadUvarint(r)
	// EVERY VALUE IS AT LEAST ONE BYTE, so a count the remaining bytes cannot
	// hold is refused before anything is allocated for it.
	if err != nil || n > maxDecodedPoints || n*uint64(want) > uint64(r.Len()) {
		return nil, errors.New("usage: the series has no valid point count")
	}
	values := make([][]int64, n)
	for i := range values {
		values[i] = make([]int64, want)
	}
	highest := int64(1)
	if l == caused {
		highest = int64(gapCauses - 1)
	}
	for col := range want {
		var prev int64
		for i := range values {
			d, err := binary.ReadVarint(r)
			if err != nil {
				return nil, fmt.Errorf("usage: the series ends inside column %s", l.name(col))
			}
			prev += d
			// EVERY COLUMN IS A COUNT, A LEVEL, AN OFFSET, A MARK OR A CAUSE, none
			// of them negative, which also catches overflow: prev is never negative
			// before this add, so a positive d that overflows wraps negative, and a
			// negative d cannot overflow.
			if prev < 0 {
				return nil, fmt.Errorf("usage: column %s goes negative", l.name(col))
			}
			if col == len(SeriesColumns) && prev > highest {
				return nil, fmt.Errorf("usage: column %s holds %d, which is not a %s this build knows",
					l.name(col), prev, map[bool]string{true: "cause", false: "mark"}[l == caused])
			}
			values[i][col] = prev
		}
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("usage: %d bytes follow the series", r.Len())
	}
	points := make([]Point, n)
	for i, v := range values {
		points[i] = pointFrom(v)
		switch {
		case l == caused:
			points[i].Gap = GapCause(v[len(SeriesColumns)])
		case l == marked && v[len(SeriesColumns)] == 1:
			points[i].Gap = GapUnspecified
		}
		if i > 0 && points[i].OffsetMillis < points[i-1].OffsetMillis {
			return nil, errors.New("usage: the series goes back in time")
		}
		// AN OFFSET A DURATION CANNOT HOLD would wrap when a window converts it,
		// into a plausible time it never was.
		if points[i].OffsetMillis > maxOffsetMillis {
			return nil, errors.New("usage: the series runs past any offset a duration holds")
		}
	}

	return points, nil
}

// SeriesCodecClocked is the encoding EncodeSeriesAt writes: SeriesCodec's,
// preceded by the Unix milliseconds of the first sample on the host's clock and
// followed by one more column, each point's gap mark (1 when its interval was
// not seen, 0 when it was), the whole compressed with DEFLATE. It is what lets
// a reader place a window of wall time on the series, and what tells it which
// intervals the sampler did not see, but not why.
const SeriesCodecClocked = 2

// SeriesCodecGaps is the encoding EncodeSeriesGaps writes: SeriesCodecClocked's,
// with each point's GapCause in place of its mark, so a reader can say why an
// interval was not seen as well as which.
const SeriesCodecGaps = 3

// EncodeSeriesAt encodes points as SeriesCodecClocked, first being the wall
// time of the first point, into at most limit bytes, downsampling as
// EncodeSeries does and carrying every dropped point's gap onto the point kept
// after it.
func EncodeSeriesAt(points []Point, first time.Time, limit int) ([]byte, int, error) {
	return encodeClocked(points, first, limit, marked)
}

// EncodeSeriesGaps encodes points as SeriesCodecGaps, as EncodeSeriesAt does.
func EncodeSeriesGaps(points []Point, first time.Time, limit int) ([]byte, int, error) {
	return encodeClocked(points, first, limit, caused)
}

func encodeClocked(points []Point, first time.Time, limit int, l layout) ([]byte, int, error) {
	if first.UnixMilli() <= 0 {
		return nil, 0, errors.New("usage: a clocked series needs the wall time of its first sample")
	}

	return encodeWithin(points, limit, func(kept []Point) ([]byte, error) {
		return encode(binary.AppendUvarint(nil, uint64(first.UnixMilli())), kept, l)
	})
}

// DecodeSeriesAt reverses EncodeSeriesAt. Every marked interval reads back as
// GapUnspecified.
func DecodeSeriesAt(data []byte) (Timeline, error) {
	return decodeClocked(data, marked)
}

// DecodeSeriesGaps reverses EncodeSeriesGaps.
func DecodeSeriesGaps(data []byte) (Timeline, error) {
	return decodeClocked(data, caused)
}

func decodeClocked(data []byte, l layout) (Timeline, error) {
	raw, err := inflate(data)
	if err != nil {
		return Timeline{}, err
	}
	r := bytes.NewReader(raw)
	ms, err := binary.ReadUvarint(r)
	if err != nil || ms == 0 || ms > math.MaxInt64 {
		return Timeline{}, errors.New("usage: the series has no valid start time")
	}
	points, err := decode(r, l)
	if err != nil {
		return Timeline{}, err
	}

	return Timeline{First: time.UnixMilli(int64(ms)).UTC(), Points: points}, nil
}

// WithoutCauses re-encodes a SeriesCodecGaps series as SeriesCodecClocked, for a
// peer that reads only that one. Each gap keeps its mark and loses its cause,
// which is what a SeriesCodecClocked series has never had.
func WithoutCauses(data []byte, limit int) ([]byte, error) {
	tl, err := DecodeSeriesGaps(data)
	if err != nil {
		return nil, err
	}
	out, _, err := EncodeSeriesAt(tl.Points, tl.First, limit)

	return out, err
}

// WithoutClock re-encodes a SeriesCodecClocked or SeriesCodecGaps series (codec
// says which) as SeriesCodec, for a peer that reads only that one. The start
// time and the gaps are dropped, which is what a SeriesCodec series has never
// had.
func WithoutClock(codec int, data []byte, limit int) ([]byte, error) {
	var tl Timeline
	var err error
	switch codec {
	case SeriesCodecClocked:
		tl, err = DecodeSeriesAt(data)
	case SeriesCodecGaps:
		tl, err = DecodeSeriesGaps(data)
	default:
		return nil, fmt.Errorf("usage: series codec %d has no clock to drop", codec)
	}
	if err != nil {
		return nil, err
	}
	out, _, err := EncodeSeries(tl.Points, limit)

	return out, err
}
