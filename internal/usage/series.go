package usage

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
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
	// AfterGap says the sampler did not see the interval that ends at this
	// point, so what it holds is known only as that interval's total. It is not
	// one of SeriesColumns: a SeriesCodec series never marks one.
	AfterGap bool
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

// EncodeSeries encodes points into at most limit bytes, keeping every point
// if it fits and otherwise every second, fourth, ... point plus the last one,
// so the totals the last point carries are never lost. It reports the stride
// it kept.
func EncodeSeries(points []Point, limit int) ([]byte, int, error) {
	return encodeWithin(points, limit, func(kept []Point) ([]byte, error) {
		return encode(nil, kept, false)
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
// A point dropped with its AfterGap mark passes the mark to the point kept
// after it, whose interval now holds the one the sampler did not see.
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
	gap := false
	for i, p := range points {
		gap = gap || p.AfterGap
		if i%stride != 0 && i != len(points)-1 {
			continue
		}
		p.AfterGap = gap
		gap = false
		out = append(out, p)
	}

	return out
}

// encode writes prefix, then the points' columns (with the gap mark when
// clocked), and compresses the whole.
func encode(prefix []byte, points []Point, clocked bool) ([]byte, error) {
	columns := len(SeriesColumns)
	if clocked {
		columns = len(clockedColumns)
	}
	raw := prefix
	raw = binary.AppendUvarint(raw, uint64(columns))
	raw = binary.AppendUvarint(raw, uint64(len(points)))
	for col := range columns {
		var prev int64
		for _, p := range points {
			v := p.column(col)
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

// column is one of clockedColumns' values for the point; the last, the gap
// mark, is 1 or 0.
func (p Point) column(col int) int64 {
	if col < len(SeriesColumns) {
		return p.columns()[col]
	}
	if p.AfterGap {
		return 1
	}

	return 0
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

	return decode(bytes.NewReader(raw), false)
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

// decode reads the columns encode wrote, to the end of r.
func decode(r *bytes.Reader, clocked bool) ([]Point, error) {
	want := len(SeriesColumns)
	if clocked {
		want = len(clockedColumns)
	}
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
	names := SeriesColumns
	if clocked {
		names = clockedColumns
	}
	for col := range want {
		var prev int64
		for i := range values {
			d, err := binary.ReadVarint(r)
			if err != nil {
				return nil, fmt.Errorf("usage: the series ends inside column %s", names[col])
			}
			prev += d
			// EVERY COLUMN IS A COUNT, A LEVEL, AN OFFSET OR A MARK, none of them
			// negative, which also catches overflow: prev is never negative before
			// this add, so a positive d that overflows wraps negative, and a
			// negative d cannot overflow.
			if prev < 0 {
				return nil, fmt.Errorf("usage: column %s goes negative", names[col])
			}
			if col == len(SeriesColumns) && prev > 1 {
				return nil, fmt.Errorf("usage: column %s is not a mark", names[col])
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
		if clocked {
			points[i].AfterGap = v[len(SeriesColumns)] == 1
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
// followed by one more column, each point's AfterGap mark, the whole compressed
// with DEFLATE. It is what lets a reader place a window of wall time on the
// series, and what tells it which intervals the sampler did not see.
const SeriesCodecClocked = 2

// clockedColumns is SeriesColumns and the gap mark.
var clockedColumns = append(slices.Clone(SeriesColumns), "after_gap")

// EncodeSeriesAt encodes points as SeriesCodecClocked, first being the wall
// time of the first point, into at most limit bytes, downsampling as
// EncodeSeries does and carrying every dropped point's gap mark onto the point
// kept after it.
func EncodeSeriesAt(points []Point, first time.Time, limit int) ([]byte, int, error) {
	if first.UnixMilli() <= 0 {
		return nil, 0, errors.New("usage: a clocked series needs the wall time of its first sample")
	}

	return encodeWithin(points, limit, func(kept []Point) ([]byte, error) {
		return encode(binary.AppendUvarint(nil, uint64(first.UnixMilli())), kept, true)
	})
}

// DecodeSeriesAt reverses EncodeSeriesAt.
func DecodeSeriesAt(data []byte) (Timeline, error) {
	raw, err := inflate(data)
	if err != nil {
		return Timeline{}, err
	}
	r := bytes.NewReader(raw)
	ms, err := binary.ReadUvarint(r)
	if err != nil || ms == 0 || ms > math.MaxInt64 {
		return Timeline{}, errors.New("usage: the series has no valid start time")
	}
	points, err := decode(r, true)
	if err != nil {
		return Timeline{}, err
	}

	return Timeline{First: time.UnixMilli(int64(ms)).UTC(), Points: points}, nil
}

// WithoutClock re-encodes a SeriesCodecClocked series as SeriesCodec, for a
// peer that reads only that one. The start time and the gap marks are dropped,
// which is what a SeriesCodec series has never had.
func WithoutClock(data []byte, limit int) ([]byte, error) {
	tl, err := DecodeSeriesAt(data)
	if err != nil {
		return nil, err
	}
	out, _, err := EncodeSeries(tl.Points, limit)

	return out, err
}
