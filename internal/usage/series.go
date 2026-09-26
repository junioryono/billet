package usage

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	if len(points) == 0 {
		return nil, 0, errors.New("usage: no points to encode")
	}
	for stride := 1; ; stride *= 2 {
		kept := downsample(points, stride)
		data, err := encode(kept)
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

func downsample(points []Point, stride int) []Point {
	if stride == 1 {
		return points
	}
	var out []Point
	for i := 0; i < len(points); i += stride {
		out = append(out, points[i])
	}
	if last := points[len(points)-1]; out[len(out)-1] != last {
		out = append(out, last)
	}

	return out
}

func encode(points []Point) ([]byte, error) {
	var raw []byte
	raw = binary.AppendUvarint(raw, uint64(len(SeriesColumns)))
	raw = binary.AppendUvarint(raw, uint64(len(points)))
	for col := range SeriesColumns {
		var prev int64
		for _, p := range points {
			v := p.columns()[col]
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

// maxDecodedSeries bounds what DecodeSeries will inflate, so a ledger row
// cannot make a reader allocate without limit.
const maxDecodedSeries = 64 << 20

// DecodeSeries reverses EncodeSeries.
func DecodeSeries(data []byte) ([]Point, error) {
	raw, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(data)), maxDecodedSeries+1))
	if err != nil {
		return nil, fmt.Errorf("usage: inflate the series: %w", err)
	}
	if len(raw) > maxDecodedSeries {
		return nil, errors.New("usage: the series inflates past its bound")
	}
	r := bytes.NewReader(raw)
	columns, err := binary.ReadUvarint(r)
	if err != nil || columns != uint64(len(SeriesColumns)) {
		return nil, fmt.Errorf("usage: the series has %d columns, want %d", columns, len(SeriesColumns))
	}
	n, err := binary.ReadUvarint(r)
	if err != nil || n > uint64(len(raw)) {
		return nil, errors.New("usage: the series has no valid point count")
	}
	values := make([][]int64, n)
	for i := range values {
		values[i] = make([]int64, len(SeriesColumns))
	}
	for col := range SeriesColumns {
		var prev int64
		for i := range values {
			d, err := binary.ReadVarint(r)
			if err != nil {
				return nil, fmt.Errorf("usage: the series ends inside column %s", SeriesColumns[col])
			}
			prev += d
			values[i][col] = prev
		}
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("usage: %d bytes follow the series", r.Len())
	}
	points := make([]Point, n)
	for i, v := range values {
		points[i] = pointFrom(v)
	}

	return points, nil
}
