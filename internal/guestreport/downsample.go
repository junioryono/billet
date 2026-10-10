package guestreport

import (
	"cmp"
	"errors"
	"slices"
)

// Downsample is r at the finest resolution whose encoding fits maxBytes (at most
// MaxReportBytes), and that encoding. Each step halves the samples as the node's
// usage series is halved, keeping every second, fourth, ... sample and the last;
// every sample but the memory levels is a counter, so no total changes. The process
// samples are halved by merging each run of them into one, which keeps every CPU
// and IO figure's sum. The steps, the suites, the account of the batches and the
// agent's own CPU are kept whole.
func Downsample(r Report, maxBytes int) (Report, []byte, error) {
	if maxBytes <= 0 || maxBytes > MaxReportBytes {
		return Report{}, nil, refuse(ErrOutOfRange, "max_bytes")
	}

	for stride := 1; ; stride *= 2 {
		kept := halve(r, stride)

		if len(kept.Samples) <= MaxReportSamples && len(kept.Processes) <= MaxReportProcessSamples {
			data, err := Encode(kept)
			if err == nil && len(data) <= maxBytes {
				return kept, data, nil
			}

			if err != nil && !errors.Is(err, ErrTooLarge) {
				return Report{}, nil, err
			}
		}

		if len(kept.Samples) <= 2 && len(kept.Processes) <= 1 {
			return Report{}, nil, refuse(ErrCannotFit, "")
		}
	}
}

// halve is r with one sample kept for every stride of them.
func halve(r Report, stride int) Report {
	if stride == 1 {
		return r
	}

	var samples []Sample

	for i, s := range r.Samples {
		if i%stride == 0 || i == len(r.Samples)-1 {
			samples = append(samples, s)
		}
	}

	var processes []ProcessSample
	for group := range slices.Chunk(r.Processes, stride) {
		processes = append(processes, mergeRun(group))
	}

	r.Samples, r.Processes = samples, processes
	r.Stride = min(r.Stride*int64(stride), MaxStride+1)

	return r
}

// mergeRun is a run of process samples as one ending where the run ends: the CPU
// and IO each name added, summed, and its processes and RSS, the largest of them.
// The names past KeepProcessesByName are summed into Other.
func mergeRun(run []ProcessSample) ProcessSample {
	out := ProcessSample{AtMillis: run[len(run)-1].AtMillis}

	var rows []ProcessRow

	index := map[string]int{}

	for _, p := range run {
		for _, row := range p.Rows {
			i, ok := index[row.Name]
			if !ok {
				index[row.Name] = len(rows)
				rows = append(rows, row)

				continue
			}

			rows[i].ProcessUsage = overTime(rows[i].ProcessUsage, row.ProcessUsage)
		}

		out.Other = overTime(out.Other, p.Other)
		out.ResidualCPUTicks = add(out.ResidualCPUTicks, p.ResidualCPUTicks)
	}

	out.Rows, out.Other = cut(rows, out.Other)

	return out
}

// Fold is one instant's processes as a ProcessSample carries them: the processes of
// a name summed into one row, the busiest KeepProcessesByName names kept (by CPU,
// then by name), and every other one summed into the returned Other row. Each row
// names one process, or several of one name; its Procs is how many.
func Fold(rows []ProcessRow) ([]ProcessRow, ProcessUsage) {
	var named []ProcessRow

	index := map[string]int{}

	for _, row := range rows {
		i, ok := index[row.Name]
		if !ok {
			index[row.Name] = len(named)
			named = append(named, row)

			continue
		}

		named[i].ProcessUsage = together(named[i].ProcessUsage, row.ProcessUsage)
	}

	return cut(named, ProcessUsage{})
}

// cut keeps the busiest KeepProcessesByName rows, sorted, and sums the rest into
// other. rows name each process once.
func cut(rows []ProcessRow, other ProcessUsage) ([]ProcessRow, ProcessUsage) {
	slices.SortFunc(rows, func(a, b ProcessRow) int {
		return cmp.Or(cmp.Compare(b.CPUTicks, a.CPUTicks), cmp.Compare(a.Name, b.Name))
	})

	if len(rows) <= KeepProcessesByName {
		return rows, other
	}

	for _, row := range rows[KeepProcessesByName:] {
		other = together(other, row.ProcessUsage)
	}

	return slices.Clip(rows[:KeepProcessesByName]), other
}

// together is two sets of processes at one instant: everything summed.
func together(a, b ProcessUsage) ProcessUsage {
	return ProcessUsage{
		Procs:      add(a.Procs, b.Procs),
		CPUTicks:   add(a.CPUTicks, b.CPUTicks),
		RSSBytes:   add(a.RSSBytes, b.RSSBytes),
		ReadBytes:  add(a.ReadBytes, b.ReadBytes),
		WriteBytes: add(a.WriteBytes, b.WriteBytes),
	}
}

// overTime is one set of processes over two intervals: what each added, summed, and
// the levels, the larger.
func overTime(a, b ProcessUsage) ProcessUsage {
	return ProcessUsage{
		Procs:      max(a.Procs, b.Procs),
		CPUTicks:   add(a.CPUTicks, b.CPUTicks),
		RSSBytes:   max(a.RSSBytes, b.RSSBytes),
		ReadBytes:  add(a.ReadBytes, b.ReadBytes),
		WriteBytes: add(a.WriteBytes, b.WriteBytes),
	}
}

// add is a+b, held at MaxValue: two values each within it cannot overflow, and a sum
// past it is one the guest's own figures made absurd.
func add(a, b int64) int64 {
	return min(a+b, MaxValue)
}
