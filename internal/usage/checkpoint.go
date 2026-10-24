package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// checkpointSchema is the layout Checkpoint writes. A checkpoint of any other
// is refused rather than read as this one.
const checkpointSchema = 1

// maxCheckpointSeries bounds a checkpoint's encoded series; a longer one is
// halved to fit, as a reported series is. A job of maxPoints samples encodes
// to a few hundred KiB.
const maxCheckpointSeries = 4 << 20

// MaxCheckpointBytes bounds a checkpoint file a node reads back: the series at
// its bound in base64, and room for the rest.
const MaxCheckpointBytes = maxCheckpointSeries*4/3 + 64<<10

// Checkpoint is what a sampler knew of one job when its node process handed
// the job over (#384): the series so far and what the rest of the summary is
// made of, for the next process to continue measuring the same compute from.
//
// HARDWARE COUNTERS ARE NOT CARRIED: they count only while a process holds
// them, so the time between the two processes went uncounted, and a resumed
// job reports none. Nor is package energy continued, for the same reason: the
// gap's energy was attributed to nobody.
type Checkpoint struct {
	Schema         int    `json:"schema"`
	IntervalMillis int64  `json:"interval_ms"`
	Target         Target `json:"target"`
	Samples        int64  `json:"samples"`
	Latest         Sample `json:"latest"`
	Seen           struct {
		CPU, Memory, OOM, IO, Net, Threads, Pressure, ProcessEnergy bool
	} `json:"seen"`
	PeakMemory      int64     `json:"peak_memory"`
	EnergyActive    float64   `json:"energy_active"`
	EnergyIdle      float64   `json:"energy_idle"`
	ProcessEnergyAt time.Time `json:"process_energy_at"`
	// EnergyFromProcess says the series' energy is the kernel's own estimate for
	// the job's process rather than a share of what the host read; nil, from a
	// billet before the meter, means what every such billet did: the process's
	// estimate for a process target and a share otherwise.
	EnergyFromProcess *bool `json:"energy_from_process,omitempty"`
	// Unseen is why the sampler had not seen the time since its last point.
	Unseen GapCause `json:"unseen"`
	// Series is the job's points so far, SeriesCodecGaps, which also holds the
	// wall time of the first.
	Series []byte `json:"series"`

	timeline Timeline
}

// Checkpoint is what the sampler knows of a job, for the next node process to
// continue its series from, or false for a job it does not measure or has
// taken no baseline of. It reads nothing and asks the sampler nothing, so it
// answers at once on a process that is stopping. A job carried from an earlier
// checkpoint and still waiting for its baseline is checkpointed as it was
// carried, its handover already marked, so a stop soon after a handover does
// not lose the first process's series.
func (m *Monitor) Checkpoint(key string) (Checkpoint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[key]
	if !ok || j.pending && !j.carried || len(j.points) == 0 {
		return Checkpoint{}, false
	}
	series, _, err := EncodeSeriesGaps(j.points, j.first, maxCheckpointSeries)
	if err != nil {
		return Checkpoint{}, false
	}

	byProcess := j.byProcess
	c := Checkpoint{
		Schema: checkpointSchema, IntervalMillis: m.opts.Interval.Milliseconds(), Target: j.target,
		Samples: j.samples, Latest: j.latest, PeakMemory: j.peakMemory,
		EnergyActive: j.energyActive, EnergyIdle: j.energyIdle, ProcessEnergyAt: j.processEnergyAt,
		EnergyFromProcess: &byProcess, Unseen: j.unseen, Series: series,
	}
	c.Seen.CPU, c.Seen.Memory, c.Seen.OOM, c.Seen.IO = j.seen.cpu, j.seen.memory, j.seen.oom, j.seen.io
	c.Seen.Net, c.Seen.Threads, c.Seen.Pressure = j.seen.net, j.seen.threads, j.seen.pressure
	c.Seen.ProcessEnergy = j.seen.processEnergy

	return c, true
}

// ReadCheckpoint reads a checkpoint Checkpoint wrote, refusing one of another
// schema, one with a field this build does not know, and one whose series
// does not decode.
func ReadCheckpoint(data []byte) (Checkpoint, error) {
	if len(data) > MaxCheckpointBytes {
		return Checkpoint{}, fmt.Errorf("usage: a checkpoint of %d bytes is past its %d bound", len(data),
			MaxCheckpointBytes)
	}
	var c Checkpoint
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Checkpoint{}, fmt.Errorf("usage: read the checkpoint: %w", err)
	}
	if dec.More() {
		return Checkpoint{}, errors.New("usage: the checkpoint has more after it")
	}
	if c.Schema != checkpointSchema {
		return Checkpoint{}, fmt.Errorf("usage: checkpoint schema %d is not %d", c.Schema, checkpointSchema)
	}
	if c.IntervalMillis <= 0 || c.Samples < 1 || c.Unseen >= gapCauses {
		return Checkpoint{}, errors.New("usage: the checkpoint's interval, samples or cause is not one a " +
			"sampler writes")
	}
	tl, err := DecodeSeriesGaps(c.Series)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("usage: the checkpoint's series: %w", err)
	}
	if len(tl.Points) == 0 {
		return Checkpoint{}, errors.New("usage: the checkpoint's series holds no samples")
	}
	c.timeline = tl

	return c, nil
}

// continues says why a checkpoint cannot carry a job measured at target every
// interval, or nil when it can: the same compute, measured the same way. A
// target is the job's cgroup, its VMM's pid and that pid's start time, so a
// checkpoint for another instance under the same name is refused.
func (c Checkpoint) continues(target Target, interval time.Duration, byProcess bool) error {
	fromProcess := target.Process
	if c.EnergyFromProcess != nil {
		fromProcess = *c.EnergyFromProcess
	}

	switch {
	case len(c.timeline.Points) == 0:
		return errors.New("the checkpoint was not read back")
	case c.Target != target:
		return errors.New("it names other compute than the job now runs on")
	case c.IntervalMillis != interval.Milliseconds():
		return fmt.Errorf("it was sampled every %dms and this node samples every %s", c.IntervalMillis, interval)
	// ONE SOURCE A SERIES: a series whose earlier points hold the process's own
	// estimate and whose later ones a share of the wall, or the other way, reads
	// as one energy and is two.
	case fromProcess != byProcess:
		return errors.New("its energy came from another source than this node reads, the process's " +
			"own estimate on one side and a share of what the host read on the other")
	}

	return nil
}

// Measuring reports whether the sampler measures a job under key.
func (m *Monitor) Measuring(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, ok := m.jobs[key]

	return ok
}

// Resume begins measuring a job this process adopted from another, or from
// itself before a restart, and is a no-op for a job it already measures.
//
// A RESUMED JOB COUNTS NO HARDWARE COUNTERS AND ATTRIBUTES NO PACKAGE ENERGY:
// whatever happened before this process took it was counted by nobody here,
// and a counter total or an energy share from part of a job's life would read
// as the whole of it. Everything the job's cgroup counts for its whole life
// (CPU, memory, io, network, pressure) is measured as ever.
//
// With a checkpoint from the process that handed the job over, the series
// continues from it: its samples are kept, and the time between the two
// processes is marked GapHandover. A checkpoint that does not continue this
// job is not used, and the error says why; the job is measured from now.
func (m *Monitor) Resume(key string, target Target, vcpus int, from *Checkpoint) error {
	now := m.opts.Now()
	j := &job{target: target, vcpus: vcpus, first: now, pending: true, resumed: true, energyBroken: true,
		byProcess: target.Process && m.opts.Meter == nil}
	var why error
	if from != nil {
		why = from.continues(target, m.opts.Interval, j.byProcess)
	}
	if from != nil && why == nil {
		j.first, j.points, j.samples = from.timeline.First, from.timeline.Points, from.Samples
		j.latest, j.peakMemory = from.Latest, from.PeakMemory
		j.energyActive, j.energyIdle, j.processEnergyAt = from.EnergyActive, from.EnergyIdle, from.ProcessEnergyAt
		// MARKED FROM THE MOMENT IT IS PUBLISHED: a sampler that does not take
		// the baseline in time leaves the job pending, and a later final read
		// must still find the time between the processes unseen.
		j.unseen = joinGaps(from.Unseen, GapHandover)
		j.seen = seen{cpu: from.Seen.CPU, memory: from.Seen.Memory, oom: from.Seen.OOM, io: from.Seen.IO,
			net: from.Seen.Net, threads: from.Seen.Threads, pressure: from.Seen.Pressure,
			processEnergy: from.Seen.ProcessEnergy}
		j.carried = true
	} else {
		j.points = []Point{j.point(now)}
	}

	m.mu.Lock()
	if _, measured := m.jobs[key]; measured {
		m.mu.Unlock()

		return nil
	}
	m.jobs[key] = j
	m.mu.Unlock()

	m.begin(key, j)

	if why != nil {
		return fmt.Errorf("usage: not continuing the series from the checkpoint, because %w", why)
	}

	return nil
}
