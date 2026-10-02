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

// lifecycleLimit bounds how long Start and Final wait for the sampler, since
// they sit on a job's launch and teardown. A sampler that has not answered by
// then is stuck on a read, and the caller goes on without it.
const lifecycleLimit = 2 * time.Second

// skewSlack is how far the jobs' CPU time may exceed the host's busy time over
// one interval before the interval is read as inconsistent. The job counters
// and /proc/stat are read one after another, so a little skew is ordinary; a
// share past it is could-not-tell, never clipped into a plausible number.
const skewSlack = 0.05

// staleAfter is how many intervals without a tick make energy could-not-tell:
// a sampler that stalled or stopped has missed intervals, and their energy is
// in no job's total.
const staleAfter = 3

// Monitor samples every job it is told about on one clock, and attributes the
// host's package energy among them.
//
// ONE GOROUTINE READS. Run is the only place any counter file is read and the
// only place a reading is applied, so a job's baseline, its samples and its
// final reading are ordered by construction, and a file that never answers
// blocks that one goroutine rather than one per caller. Start and Final ask Run
// and wait at most lifecycleLimit; past it they go on with what is already
// known, and the silence reads as staleness.
type Monitor struct {
	reader Reader
	opts   Options

	requests chan request
	// ticker is false in tests that drive every tick themselves; limit is
	// lifecycleLimit, shortened by those tests.
	ticker bool
	limit  time.Duration
	// afterTickRead runs in Run between a tick's reads and its apply, in tests
	// only, to place a Forget exactly where it would race.
	afterTickRead func()

	// mu guards what Forget and a Final that could not reach Run touch from
	// another goroutine. Run holds it only while applying, never while reading.
	mu           sync.Mutex
	jobs         map[string]*job
	lastTick     time.Time
	host         HostCPU
	hostOK       bool
	energy       Energy
	energyOK     bool
	energyReadAt time.Time
}

type requestKind int

const (
	requestStart requestKind = iota
	requestFinal
	requestTick
)

type request struct {
	kind  requestKind
	key   string
	job   *job
	reply chan reply
}

type reply struct {
	summary Summary
	ok      bool
}

type job struct {
	target  Target
	vcpus   int
	first   time.Time
	samples int64
	// pending is a job Start published whose baseline Run has not yet taken.
	// Ticks leave it alone, so a job still pending when it ends has only its
	// placeholder point, and summaryOf's two-point rule makes its energy
	// could-not-tell.
	pending bool
	// latest holds each group's most recent successful reading, and seen which
	// groups were ever read, so one failed read does not erase a measurement.
	latest     Sample
	seen       seen
	peakMemory int64
	lastCPU    int64
	lastCPUOK  bool
	points     []Point

	energyActive, energyIdle float64
	// processEnergyAt is when the process's energy was last read: a lifetime
	// total is the job's whole energy only if it is recent.
	processEnergyAt time.Time
	// energyBroken is set when any interval of the job's life could not be
	// attributed, which makes its energy could-not-tell rather than too low.
	energyBroken bool
}

type seen struct{ cpu, memory, oom, io, net, threads, pressure, processEnergy bool }

// NewMonitor builds a monitor reading under root ("/" on a real host). It
// samples nothing until Run.
func NewMonitor(root string, opts Options) *Monitor {
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Monitor{reader: Reader{Root: root}, opts: opts, jobs: map[string]*job{},
		requests: make(chan request), ticker: true, limit: lifecycleLimit}
}

// Run samples every job each interval, and serves Start and Final, until ctx
// ends.
func (m *Monitor) Run(ctx context.Context) {
	var tick <-chan time.Time
	if m.ticker {
		m.tick()
		t := time.NewTicker(m.opts.Interval)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			m.tick()
		case r := <-m.requests:
			m.serve(r)
		}
	}
}

func (m *Monitor) serve(r request) {
	switch r.kind {
	case requestStart:
		m.start(r.key, r.job)
		r.reply <- reply{}
	case requestFinal:
		s, ok := m.final(r.key)
		r.reply <- reply{summary: s, ok: ok}
	case requestTick:
		m.tick()
		r.reply <- reply{}
	}
}

// ask hands Run a request and waits for its answer, the whole exchange within
// limit, and reports whether the answer came in time. Run replies into a
// buffered channel, so an answer nobody waits for any more never blocks it.
func (m *Monitor) ask(r request, limit time.Duration) (reply, bool) {
	r.reply = make(chan reply, 1)
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	select {
	case m.requests <- r:
	case <-deadline.C:
		return reply{}, false
	}
	select {
	case a := <-r.reply:
		return a, true
	case <-deadline.C:
		return reply{}, false
	}
}

// Start begins measuring a job. Run takes its first sample, so the CPU it used
// before this moment (the boot) is never charged to one interval's energy, and
// the baseline is read after the host's last tick, so the job's CPU in its first
// interval is within the host's.
//
// THE JOB IS PUBLISHED FIRST, pending, and Run fills in the baseline only if
// that same job is still the one under the key: a Forget, or a successor, while
// Run was reading wins. A sampler that does not finish in time leaves the job
// pending, which is known and never attributed.
func (m *Monitor) Start(key string, target Target, vcpus int) {
	now := m.opts.Now()
	j := &job{target: target, vcpus: vcpus, first: now, pending: true}
	j.points = append(j.points, j.point(now))
	m.mu.Lock()
	m.jobs[key] = j
	m.mu.Unlock()

	m.ask(request{kind: requestStart, key: key, job: j}, m.limit)
}

func (m *Monitor) start(key string, j *job) {
	now := m.opts.Now()
	s := m.reader.Read(j.target)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.jobs[key] != j {
		return
	}
	j.first, j.pending, j.points = now, false, nil
	j.absorb(s, now)
	j.lastCPU, j.lastCPUOK = s.CPUUsage, s.CPUOK
	j.points = append(j.points, j.point(now))
}

// Forget stops measuring a job without reporting it.
func (m *Monitor) Forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.jobs, key)
}

// Tick asks Run for one tick and waits for it. Run ticks on its own clock;
// this is for tests that drive time themselves.
func (m *Monitor) Tick() {
	r := request{kind: requestTick, reply: make(chan reply, 1)}
	m.requests <- r
	<-r.reply
}

// tick samples every job once and attributes the energy since the last tick.
// Only Run calls it.
//
// SNAPSHOT UNDER THE LOCK, READ OUTSIDE IT, APPLY UNDER IT. What is applied is
// checked against the job it was read for, because Forget does not wait for
// Run.
func (m *Monitor) tick() {
	m.mu.Lock()
	snapshot := make(map[string]*job, len(m.jobs))
	for key, j := range m.jobs {
		if !j.pending {
			snapshot[key] = j
		}
	}
	m.mu.Unlock()

	now := m.opts.Now()
	host, hostErr := m.reader.ReadHostCPU()
	var energy Energy
	energyErr := errNoRAPL
	if m.opts.RAPL {
		energy, energyErr = m.reader.ReadEnergy()
	}
	samples := make(map[string]*Sample, len(snapshot))
	for key, j := range snapshot {
		s := m.reader.Read(j.target)
		samples[key] = &s
	}
	if m.afterTickRead != nil {
		m.afterTickRead()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	hostOK := hostErr == nil && m.hostOK && host.CPUs > 0 && host.CPUs == m.host.CPUs
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

	// THE RESERVATION COUNTS EVERY LIVE JOB, pending or unreadable included:
	// failing to take a job's baseline or read its CPU does not free the CPUs
	// its compute holds.
	reserved := 0
	for _, j := range m.jobs {
		reserved += j.vcpus
	}

	// EVERY JOB'S CPU DELTA IS CHECKED BEFORE ANY IS USED, alone and together:
	// a negative delta, or jobs that together ran longer than the host was busy,
	// makes the interval could-not-tell for all of them.
	deltas := make(map[string]int64, len(samples))
	var total int64
	consistent := energyOK && hostOK && busyDelta > 0 && dt > 0
	for key, s := range samples {
		j := m.jobs[key]
		if j != snapshot[key] {
			continue
		}
		if !s.CPUOK || !j.lastCPUOK {
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
	idleDenominator := float64(max(host.CPUs, reserved))

	for key, s := range samples {
		j := m.jobs[key]
		if j != snapshot[key] {
			continue
		}
		j.absorb(*s, now)
		if m.opts.RAPL {
			d, measured := deltas[key]
			// A GAP BREAKS A JOB THAT WENT UNSAMPLED FOR IT: the time since the later
			// of the last tick and the job's own baseline.
			missed := !m.lastTick.IsZero() &&
				now.Sub(maxTime(m.lastTick, j.first)) >= staleAfter*m.opts.Interval
			if !consistent || !measured || missed {
				j.energyBroken = true
			} else {
				j.energyActive += activePool * min(float64(d)/float64(busyDelta), 1)
				// IDLE ONLY FOR THE PART OF THE INTERVAL THE JOB EXISTED.
				overlap := now.Sub(maxTime(m.lastTick, j.first))
				if overlap > 0 {
					j.energyIdle += idlePool * (overlap.Seconds() / dt.Seconds()) *
						float64(j.vcpus) / idleDenominator
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

func (j *job) absorb(s Sample, now time.Time) {
	j.samples++
	if s.CPUOK {
		j.latest.CPUUsage, j.latest.CPUUser, j.latest.CPUSys = s.CPUUsage, s.CPUUser, s.CPUSys
		j.seen.cpu = true
	}
	if s.MemoryOK {
		j.latest.MemoryCurrent, j.latest.MemoryPeak = s.MemoryCurrent, s.MemoryPeak
		j.peakMemory = max(j.peakMemory, s.MemoryPeak, s.MemoryCurrent)
		j.seen.memory = true
	}
	if s.OOMOK {
		j.latest.OOMKills = s.OOMKills
		j.seen.oom = true
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
	if s.ProcessEnergyOK {
		j.latest.ProcessEnergy = s.ProcessEnergy
		j.seen.processEnergy, j.processEnergyAt = true, now
	}
}

func (j *job) point(now time.Time) Point {
	energy := int64(j.energyActive)
	if j.target.Process {
		energy = j.latest.ProcessEnergy
	}

	return Point{
		OffsetMillis: now.Sub(j.first).Milliseconds(), CPUUsage: j.latest.CPUUsage,
		MemoryCurrent: j.latest.MemoryCurrent, DiskRead: j.latest.DiskRead,
		DiskWrite: j.latest.DiskWrite, NetRx: j.latest.NetRx, NetTx: j.latest.NetTx,
		GuestCPU: j.latest.GuestCPU, VMMCPU: j.latest.VMMCPU,
		EnergyActive: energy,
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
	EnergySplit bool
	// EnergyProcess says EnergyActive is the kernel's own estimate for the job's
	// process (Target.Process), not a share of the package counter.
	EnergyProcess            bool
	EnergyActive, EnergyIdle int64 // µJ
	Points                   []Point
}

// Measured says which groups were read at least once, and whether energy was
// attributed for the job's whole life.
type Measured struct{ CPU, Memory, OOM, IO, Net, Threads, Pressure, Energy bool }

// Final takes one last sample of a job and summarises it. The job stays known,
// so a destroy that fails and is retried can ask again; Forget ends it.
//
// A SAMPLER THAT DOES NOT ANSWER IN TIME is stuck on a read: the summary is
// what was already known, and the silence makes its energy stale.
func (m *Monitor) Final(key string) (Summary, bool) {
	if answer, answered := m.ask(request{kind: requestFinal, key: key}, m.limit); answered {
		return answer.summary, answer.ok
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[key]
	if !ok {
		return Summary{}, false
	}

	return m.summaryOf(j, m.opts.Now()), true
}

func (m *Monitor) final(key string) (Summary, bool) {
	m.mu.Lock()
	j, ok := m.jobs[key]
	m.mu.Unlock()
	if !ok {
		return Summary{}, false
	}

	now := m.opts.Now()
	s := m.reader.Read(j.target)

	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.jobs[key]; !ok || current != j {
		return Summary{}, false
	}
	j.absorb(s, now)

	return m.summaryOf(j, now), true
}

// summaryOf summarises a job at now. Called with mu held.
func (m *Monitor) summaryOf(j *job, now time.Time) Summary {
	points := append(append([]Point(nil), j.points...), j.point(now))
	sum := Summary{
		Samples: j.samples, Interval: m.opts.Interval, Window: now.Sub(j.first),
		Latest: j.latest, MemoryPeak: j.peakMemory,
		Measured: Measured{CPU: j.seen.cpu, Memory: j.seen.memory, OOM: j.seen.oom, IO: j.seen.io,
			Net: j.seen.net, Threads: j.seen.threads, Pressure: j.seen.pressure},
		Points: points,
	}
	// A PROCESS'S ENERGY IS A LIFETIME TOTAL the kernel keeps, so no gap in
	// sampling loses any of it, but only a recent reading is the whole job's: one
	// from the final read, or from a tick the job did not outlive by
	// staleAfter intervals. An older one is could-not-tell, never a smaller total.
	if j.target.Process {
		fresh := now.Sub(j.processEnergyAt) < staleAfter*m.opts.Interval
		sum.Measured.Energy, sum.EnergyProcess = j.seen.processEnergy && fresh, true
		sum.EnergyActive = j.latest.ProcessEnergy

		return sum
	}
	stale := m.lastTick.IsZero() || now.Sub(m.lastTick) >= staleAfter*m.opts.Interval
	sum.Measured.Energy = m.opts.RAPL && !j.energyBroken && len(j.points) > 1 && !stale
	sum.EnergySplit = m.opts.IdleWatts > 0
	sum.EnergyActive, sum.EnergyIdle = int64(j.energyActive), int64(j.energyIdle)

	return sum
}
