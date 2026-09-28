package usage

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Options configure a Monitor.
type Options struct {
	Interval time.Duration
	// RAPL reads the package energy counter each tick.
	RAPL bool
	// IdleWatts is the host's measured idle package power; zero means no
	// baseline, and the whole package is shared by CPU time.
	IdleWatts float64
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// maxPackageWatts bounds how fast a package can advance its energy counter, to
// decide how long a gap between readings may be before a wrap could have gone
// unseen. No single socket billet runs on draws 1 kW.
const maxPackageWatts = 1000

// maxPoints bounds a job's in-memory series; past it the series is halved,
// keeping every total, so a job of any length costs bounded memory.
const maxPoints = 1 << 16

// Monitor samples every job it is told about on one clock, and attributes the
// host's package energy among them.
type Monitor struct {
	reader Reader
	opts   Options

	// ticking serialises Tick, so the reads it makes outside mu apply in order.
	ticking sync.Mutex

	// mu guards everything below. No file is read while it is held: a slow read
	// of one job's counters must not hold up Start, Final or Forget, which sit on
	// the launch and teardown paths.
	mu           sync.Mutex
	jobs         map[string]*job
	lastTick     time.Time
	host         HostCPU
	hostOK       bool
	energy       Energy
	energyOK     bool
	energyReadAt time.Time
}

type job struct {
	target  Target
	vcpus   int
	first   time.Time
	samples int64
	// latest holds each group's most recent successful reading, and seen which
	// groups were ever read, so one failed read does not erase a measurement.
	latest     Sample
	seen       seen
	peakMemory int64
	lastCPU    int64
	lastCPUOK  bool
	points     []Point

	energyActive, energyIdle float64
	// energyBroken is set when any interval of the job's life could not be
	// attributed, which makes its energy could-not-tell rather than too low.
	energyBroken bool
}

type seen struct{ cpu, memory, io, net, threads, pressure bool }

// NewMonitor builds a monitor reading under root ("/" on a real host).
func NewMonitor(root string, opts Options) *Monitor {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Monitor{reader: Reader{Root: root}, opts: opts, jobs: map[string]*job{}}
}

// Start begins measuring a job, taking its first sample now so the CPU it used
// before this moment (the boot) is never charged to one interval's energy.
func (m *Monitor) Start(key string, target Target, vcpus int) {
	now := m.opts.Now()
	s := m.reader.Read(target)

	m.mu.Lock()
	defer m.mu.Unlock()

	j := &job{target: target, vcpus: vcpus, first: now}
	j.absorb(s)
	j.lastCPU, j.lastCPUOK = s.CPUUsage, s.CPUOK
	j.points = append(j.points, j.point(now))
	m.jobs[key] = j
}

// Forget stops measuring a job without reporting it.
func (m *Monitor) Forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.jobs, key)
}

// Run samples every job each interval until ctx ends.
func (m *Monitor) Run(ctx context.Context) {
	m.Tick()
	t := time.NewTicker(m.opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick()
		}
	}
}

// skewSlack is how far the jobs' CPU time may exceed the host's busy time over
// one interval before the interval is read as inconsistent. The job counters
// and /proc/stat are read one after another, so a little skew is ordinary; a
// share past it is could-not-tell, never clipped into a plausible number.
const skewSlack = 0.05

// Tick samples every job once and attributes the energy since the last tick.
//
// SNAPSHOT UNDER THE LOCK, READ OUTSIDE IT, APPLY UNDER IT. A job started while
// the reads ran is sampled next tick; one forgotten meanwhile is skipped.
func (m *Monitor) Tick() {
	m.ticking.Lock()
	defer m.ticking.Unlock()

	m.mu.Lock()
	targets := make(map[string]Target, len(m.jobs))
	for key, j := range m.jobs {
		targets[key] = j.target
	}
	m.mu.Unlock()

	now := m.opts.Now()
	host, hostErr := m.reader.ReadHostCPU()
	var energy Energy
	energyErr := errNoRAPL
	if m.opts.RAPL {
		energy, energyErr = m.reader.ReadEnergy()
	}
	samples := make(map[string]*Sample, len(targets))
	for key, target := range targets {
		s := m.reader.Read(target)
		samples[key] = &s
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	hostOK := hostErr == nil && m.hostOK && host.CPUs > 0
	busyDelta := int64(0)
	if hostOK {
		busyDelta = host.Busy - m.host.Busy
	}
	pkgDelta, energyOK := m.energySince(now, energy, energyErr)
	dt := now.Sub(m.lastTick)

	// THE IDLE POOL IS NEVER MORE THAN WAS MEASURED: a baseline above an
	// interval's actual draw would otherwise hand out energy that never existed.
	var idlePool float64
	if m.opts.IdleWatts > 0 && !m.lastTick.IsZero() {
		idlePool = min(m.opts.IdleWatts*dt.Seconds()*1e6, float64(pkgDelta))
	}
	activePool := float64(pkgDelta) - idlePool

	// EVERY JOB'S CPU DELTA IS CHECKED BEFORE ANY IS USED, alone and together:
	// a negative delta, or jobs that together ran longer than the host was busy,
	// makes the interval could-not-tell for all of them.
	deltas := make(map[string]int64, len(samples))
	var total int64
	consistent := energyOK && hostOK && busyDelta > 0 && dt > 0
	for key, s := range samples {
		j := m.jobs[key]
		if j == nil || !s.CPUOK || !j.lastCPUOK {
			continue
		}
		d := s.CPUUsage - j.lastCPU
		if d < 0 {
			consistent = false
		}
		deltas[key] = d
		total += d
	}
	if float64(total) > float64(busyDelta)*(1+skewSlack) {
		consistent = false
	}

	for key, s := range samples {
		j := m.jobs[key]
		if j == nil {
			continue
		}
		j.absorb(*s)
		if m.opts.RAPL {
			d, measured := deltas[key]
			if !consistent || !measured {
				j.energyBroken = true
			} else {
				j.energyActive += activePool * min(float64(d)/float64(busyDelta), 1)
				// IDLE ONLY FOR THE PART OF THE INTERVAL THE JOB EXISTED.
				overlap := now.Sub(maxTime(m.lastTick, j.first))
				if overlap > 0 {
					j.energyIdle += idlePool * (overlap.Seconds() / dt.Seconds()) *
						float64(j.vcpus) / float64(host.CPUs)
				}
			}
		}
		j.lastCPU, j.lastCPUOK = s.CPUUsage, s.CPUOK
		j.points = append(j.points, j.point(now))
		if len(j.points) > maxPoints {
			j.points = downsample(j.points, 2)
		}
	}

	m.host, m.hostOK = host, hostErr == nil
	m.lastTick = now
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

// errNoRAPL stands in for a reading a monitor without RAPL never takes.
var errNoRAPL = errors.New("usage: rapl is not configured")

// energySince records a package reading and returns the energy since the last
// one. A gap long enough for the counter to have wrapped unseen is could not
// tell, never a small number. Called with mu held.
func (m *Monitor) energySince(now time.Time, e Energy, err error) (int64, bool) {
	if err != nil {
		m.energyOK = false
		return 0, false
	}
	prev, prevOK, prevAt := m.energy, m.energyOK, m.energyReadAt
	m.energy, m.energyOK, m.energyReadAt = e, true, now
	if !prevOK || prev.MaxRange != e.MaxRange {
		return 0, false
	}
	wrapSeconds := float64(e.MaxRange) / 1e6 / maxPackageWatts
	if now.Sub(prevAt).Seconds() >= wrapSeconds {
		return 0, false
	}

	return energyDelta(prev.Microjoules, e.Microjoules, e.MaxRange), true
}

func (j *job) absorb(s Sample) {
	j.samples++
	if s.CPUOK {
		j.latest.CPUUsage, j.latest.CPUUser, j.latest.CPUSys = s.CPUUsage, s.CPUUser, s.CPUSys
		j.seen.cpu = true
	}
	if s.MemoryOK {
		j.latest.MemoryCurrent, j.latest.MemoryPeak, j.latest.OOMKills = s.MemoryCurrent, s.MemoryPeak, s.OOMKills
		j.peakMemory = max(j.peakMemory, s.MemoryPeak, s.MemoryCurrent)
		j.seen.memory = true
	}
	if s.IOOK {
		j.latest.DiskRead, j.latest.DiskWrite = s.DiskRead, s.DiskWrite
		j.seen.io = true
	}
	if s.NetOK {
		j.latest.NetRx, j.latest.NetTx = s.NetRx, s.NetTx
		j.latest.NetRxPackets, j.latest.NetTxPackets = s.NetRxPackets, s.NetTxPackets
		j.seen.net = true
	}
	if s.ThreadsOK {
		j.latest.GuestCPU, j.latest.VMMCPU = s.GuestCPU, s.VMMCPU
		j.seen.threads = true
	}
	if s.PressureOK {
		j.latest.CPUSome, j.latest.CPUFull = s.CPUSome, s.CPUFull
		j.latest.MemorySome, j.latest.MemoryFull = s.MemorySome, s.MemoryFull
		j.latest.IOSome, j.latest.IOFull = s.IOSome, s.IOFull
		j.seen.pressure = true
	}
}

func (j *job) point(now time.Time) Point {
	return Point{
		OffsetMillis: now.Sub(j.first).Milliseconds(), CPUUsage: j.latest.CPUUsage,
		MemoryCurrent: j.latest.MemoryCurrent, DiskRead: j.latest.DiskRead,
		DiskWrite: j.latest.DiskWrite, NetRx: j.latest.NetRx, NetTx: j.latest.NetTx,
		GuestCPU: j.latest.GuestCPU, VMMCPU: j.latest.VMMCPU,
		EnergyActive: int64(j.energyActive),
	}
}

// Summary is what the host measured one job do over its life. A false
// Measured flag means that group was never read, and its fields are zero for
// that reason.
type Summary struct {
	Samples  int64
	Interval time.Duration
	Window   time.Duration
	Latest   Sample
	// MemoryPeak is the higher of the cgroup's own memory.peak and every
	// memory.current the sampler saw.
	MemoryPeak int64
	Measured   Measured
	// EnergySplit says an idle baseline was configured, so EnergyIdle is the
	// baseline's share and EnergyActive the energy above it.
	EnergySplit              bool
	EnergyActive, EnergyIdle int64 // µJ
	Points                   []Point
}

// Measured says which groups were read at least once, and whether energy was
// attributed for the job's whole life.
type Measured struct{ CPU, Memory, IO, Net, Threads, Pressure, Energy bool }

// staleAfter is how many intervals without a tick make a job's energy
// could-not-tell: a sampler that stopped (the node shutting down while jobs
// drain) has missed intervals, and those intervals' energy is not in the total.
const staleAfter = 3

// Final takes one last sample of a job and summarises it. The job stays known,
// so a destroy that fails and is retried can ask again; Forget ends it.
func (m *Monitor) Final(key string) (Summary, bool) {
	m.mu.Lock()
	j, ok := m.jobs[key]
	var target Target
	if ok {
		target = j.target
	}
	m.mu.Unlock()
	if !ok {
		return Summary{}, false
	}

	now := m.opts.Now()
	s := m.reader.Read(target)

	m.mu.Lock()
	defer m.mu.Unlock()

	if j, ok = m.jobs[key]; !ok {
		return Summary{}, false
	}
	j.absorb(s)
	points := append(append([]Point(nil), j.points...), j.point(now))
	stale := m.lastTick.IsZero() || now.Sub(m.lastTick) > staleAfter*m.opts.Interval

	return Summary{
		Samples: j.samples, Interval: m.opts.Interval, Window: now.Sub(j.first),
		Latest: j.latest, MemoryPeak: j.peakMemory,
		Measured: Measured{CPU: j.seen.cpu, Memory: j.seen.memory, IO: j.seen.io, Net: j.seen.net,
			Threads: j.seen.threads, Pressure: j.seen.pressure,
			Energy: m.opts.RAPL && !j.energyBroken && len(j.points) > 1 && !stale},
		EnergySplit:  m.opts.IdleWatts > 0,
		EnergyActive: int64(j.energyActive), EnergyIdle: int64(j.energyIdle),
		Points: points,
	}, true
}
