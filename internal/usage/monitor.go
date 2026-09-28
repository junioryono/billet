package usage

import (
	"context"
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
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.opts.Now()
	j := &job{target: target, vcpus: vcpus, first: now}
	s := m.reader.Read(target)
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

// Tick samples every job once and attributes the energy since the last tick.
func (m *Monitor) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.opts.Now()
	host, hostErr := m.reader.ReadHostCPU()
	busyDelta := int64(0)
	hostOK := hostErr == nil && m.hostOK
	if hostOK {
		busyDelta = host.Busy - m.host.Busy
	}

	pkgDelta, energyOK := m.readEnergy(now)
	dt := now.Sub(m.lastTick)
	var idle float64
	if m.opts.IdleWatts > 0 && !m.lastTick.IsZero() {
		idle = m.opts.IdleWatts * dt.Seconds() * 1e6
	}
	active := float64(pkgDelta)
	if m.opts.IdleWatts > 0 {
		active = max(active-idle, 0)
	}

	for _, j := range m.jobs {
		s := m.reader.Read(j.target)
		j.absorb(s)
		if m.opts.RAPL {
			switch {
			case !energyOK || !hostOK || busyDelta <= 0 || !s.CPUOK || !j.lastCPUOK || host.CPUs <= 0:
				j.energyBroken = true
			default:
				share := min(float64(s.CPUUsage-j.lastCPU)/float64(busyDelta), 1)
				j.energyActive += active * max(share, 0)
				j.energyIdle += idle * float64(j.vcpus) / float64(host.CPUs)
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

// readEnergy reads the package counter and returns the energy since the last
// reading. A gap long enough for the counter to have wrapped unseen is could
// not tell, never a small number.
func (m *Monitor) readEnergy(now time.Time) (int64, bool) {
	if !m.opts.RAPL {
		return 0, false
	}
	e, err := m.reader.ReadEnergy()
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

// Final takes one last sample of a job and summarises it. The job stays known,
// so a destroy that fails and is retried can ask again; Forget ends it.
func (m *Monitor) Final(key string) (Summary, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	j, ok := m.jobs[key]
	if !ok {
		return Summary{}, false
	}
	now := m.opts.Now()
	j.absorb(m.reader.Read(j.target))
	points := append(append([]Point(nil), j.points...), j.point(now))

	return Summary{
		Samples: j.samples, Interval: m.opts.Interval, Window: now.Sub(j.first),
		Latest: j.latest, MemoryPeak: j.peakMemory,
		Measured: Measured{CPU: j.seen.cpu, Memory: j.seen.memory, IO: j.seen.io, Net: j.seen.net,
			Threads: j.seen.threads, Pressure: j.seen.pressure,
			Energy: m.opts.RAPL && !j.energyBroken && len(j.points) > 1},
		EnergySplit:  m.opts.IdleWatts > 0,
		EnergyActive: int64(j.energyActive), EnergyIdle: int64(j.energyIdle),
		Points: points,
	}, true
}
