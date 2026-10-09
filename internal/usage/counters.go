package usage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"time"
)

// Event is one hardware event a job's vCPU threads are counted for.
type Event int

// The events counted, in the order a group opens them.
//
// SIX, BECAUSE SIX FIT. The reference host (AMD EPYC 7763, Zen 3) has six core
// counters and no stalled-cycles-backend or ref-cycles; perf stat with these
// six events on one live VM's fc_vcpu thread read IPC 1.21 and 7.4 cache misses
// per 1,000 instructions (2026-09-25). That read named them as separate events;
// as one group they count only while nothing else holds a counter, which
// proveCounting checks at startup.
const (
	Cycles Event = iota
	Instructions
	CacheReferences
	CacheMisses
	BranchMisses
	FrontendStalls
	// NumEvents is how many events there are.
	NumEvents
)

// ErrThreadGone is what a CounterSource answers for a thread that no longer
// exists: one that exited between the listing and the open.
var ErrThreadGone = errors.New("usage: the thread is gone")

// CounterReading is one read of one thread's event group: each event's raw count
// and how long the group was enabled and how long it actually counted.
type CounterReading struct {
	Enabled, Running time.Duration
	Values           [NumEvents]uint64
}

// CounterGroup is the events open on one thread.
type CounterGroup interface {
	// Opened says which events the group counts. An event the CPU or the kernel
	// refused is not in it, and is could-not-tell for the job.
	Opened() [NumEvents]bool
	Read() (CounterReading, error)
	Close() error
}

// CounterSource opens the hardware counters of one thread. HardwareCounters is
// the real one; a test supplies its own.
type CounterSource interface {
	// Open counts tid in every mode, guest and kernel included. ErrThreadGone
	// means tid no longer exists; any other error means it could not be
	// counted.
	Open(tid int) (CounterGroup, error)
}

// Counters is what the hardware counters saw a job's vCPU threads do, from when
// the sampler first saw each thread. An event that is not Measured could not be
// counted on every vCPU thread for all of that time, and its value is zero for
// that reason rather than counted as zero.
type Counters struct {
	Values   [NumEvents]int64
	Measured [NumEvents]bool
}

// jobCounters is one job's open groups and what they have counted. Only Run
// touches it, so every descriptor is opened, read and closed on one goroutine.
type jobCounters struct {
	threads map[int]*threadCounters
	// failed holds the threads that could not be counted, which are not opened
	// again: the job's totals are already could-not-tell, and a retry every
	// sample would open a descriptor every sample.
	failed map[vcpuThread]bool
	// exited is the last scaled reading of every thread that has gone.
	exited [NumEvents]uint64
	// counted marks an event some thread's group opened, and broken one some
	// vCPU thread could not count for part of the job's life: its total is then
	// could-not-tell rather than too low.
	counted, broken [NumEvents]bool
	// finished is set once the groups are closed, after which nothing is opened
	// or read again.
	finished bool
}

type threadCounters struct {
	// start is the thread's start time: with its tid, which thread the group
	// counts.
	start  uint64
	group  CounterGroup
	opened [NumEvents]bool
	// last is the most recent scaled reading, and raw the reading it was scaled
	// from. A read that fails keeps them, as a failed read of any other counter
	// keeps the one before.
	last [NumEvents]uint64
	raw  CounterReading
	// read says some reading has been taken and scaled; until one has, the
	// thread's counts are could-not-tell, not zero.
	read bool
	// unscaled says the latest read could not be scaled (the group was enabled
	// and never ran), so the thread's counts are could-not-tell until one can.
	unscaled bool
}

// known says the thread's latest reading is a count: one was taken and it
// could be scaled.
func (th *threadCounters) known() bool { return th.read && !th.unscaled }

// scale extrapolates a count over the time its group was enabled from the time
// it ran. Equal times need no scaling and stay exact; a group that was enabled
// and never ran cannot be scaled at all.
func scale(value uint64, enabled, running time.Duration) (uint64, bool) {
	switch {
	case running < 0 || running > enabled:
		return 0, false
	case running == enabled:
		return value, true
	case running == 0:
		return 0, false
	}
	hi, lo := bits.Mul64(value, uint64(enabled))
	if hi >= uint64(running) {
		return 0, false
	}
	q, _ := bits.Div64(hi, lo, uint64(running))

	return q, true
}

// update brings a job's groups up to date with its vCPU thread listing and
// reads every one. listed says vcpus is a listing; without one, the groups
// already open are read and nothing is opened or retired. alive says whether a
// thread just opened is still one of the job's vCPU threads, or that it could
// not tell.
//
// A THREAD IS ITS TID AND ITS START TIME, so a tid freed by one vCPU thread and
// given to another between two listings retires the first thread's group and
// opens one for the second, rather than reading the departed thread's final
// count as the new one's.
func (c *jobCounters) update(src CounterSource, vcpus []vcpuThread, listed bool,
	alive func(vcpuThread) (bool, error),
) {
	if c.finished {
		return
	}
	if c.threads == nil {
		c.threads, c.failed = map[int]*threadCounters{}, map[vcpuThread]bool{}
	}
	if listed {
		for tid, th := range c.threads {
			if !slices.Contains(vcpus, vcpuThread{tid: tid, start: th.start}) {
				// A THREAD THAT EXITED KEEPS ITS LAST READING: the descriptor of an
				// exited task still answers its final count, and if it does not,
				// the reading before it stands.
				c.read(th)
				c.retire(tid, th)
			}
		}
		for _, v := range vcpus {
			if _, ok := c.threads[v.tid]; !ok && !c.failed[v] {
				c.open(src, v, alive)
			}
		}
	}
	for _, th := range c.threads {
		c.read(th)
	}
}

// open starts counting a thread the sampler has just seen.
//
// CHECKED AFTER THE OPEN, like readThreads: a tid the kernel gave to another
// process between the listing and the open is no longer one of this VMM's
// threads, and a group that counted it is closed unread. A check that cannot
// tell closes it too, and makes the job's counts could-not-tell, since the
// thread may be one of the job's and is no longer counted.
func (c *jobCounters) open(src CounterSource, v vcpuThread, alive func(vcpuThread) (bool, error)) {
	g, err := src.Open(v.tid)
	if errors.Is(err, ErrThreadGone) {
		return
	}
	if err != nil {
		c.fail(v)
		return
	}
	switch ok, err := alive(v); {
	case err != nil:
		_ = g.Close()
		c.fail(v)
		return
	case !ok:
		_ = g.Close()
		return
	}
	th := &threadCounters{start: v.start, group: g, opened: g.Opened()}
	for e := range NumEvents {
		if th.opened[e] {
			c.counted[e] = true
		} else {
			c.broken[e] = true
		}
	}
	c.threads[v.tid] = th
}

// fail records a vCPU thread that could not be counted: every event is then
// could-not-tell for the job, and the thread is not tried again.
func (c *jobCounters) fail(v vcpuThread) {
	c.failed[v] = true
	for e := range NumEvents {
		c.broken[e] = true
	}
}

func (c *jobCounters) read(th *threadCounters) {
	r, err := th.group.Read()
	if err != nil {
		return
	}
	// A COUNT NEVER RUNS BACKWARDS, and neither do its times: a reading that does
	// is not of this group's counters, and the one before stands. Checked on the
	// raw counts, because a scaled estimate may fall between two honest
	// readings as the group's share of the PMU changes.
	if r.Enabled < th.raw.Enabled || r.Running < th.raw.Running {
		return
	}
	var scaled [NumEvents]uint64
	for e := range NumEvents {
		if !th.opened[e] {
			continue
		}
		if r.Values[e] < th.raw.Values[e] {
			return
		}
		v, ok := scale(r.Values[e], r.Enabled, r.Running)
		if !ok {
			th.unscaled = true
			return
		}
		scaled[e] = v
	}
	th.last, th.raw, th.read, th.unscaled = scaled, r, true, false
}

// retire closes a thread's group and keeps what it counted. A thread whose
// latest reading is not a count leaves the job's counts unknown for good.
func (c *jobCounters) retire(tid int, th *threadCounters) {
	_ = th.group.Close()
	delete(c.threads, tid)
	for e := range NumEvents {
		c.exited[e] = addCapped(c.exited[e], th.last[e])
		if !th.known() {
			c.broken[e] = true
		}
	}
}

// finish closes every group a job holds, keeping what they counted.
func (c *jobCounters) finish() {
	for tid, th := range c.threads {
		c.retire(tid, th)
	}
	c.finished = true
}

// addCapped adds two counts, holding at the largest uint64 rather than
// wrapping, which totals then refuses as too large to report.
func addCapped(a, b uint64) uint64 {
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64
	}

	return sum
}

// totals sums every thread's latest reading. An event is measured only if some
// thread counted it, no vCPU thread failed to, every thread has a latest
// reading and it could be scaled, and the sum fits.
func (c *jobCounters) totals() *Counters {
	if c == nil {
		return nil
	}
	out := &Counters{}
	for e := range NumEvents {
		if !c.counted[e] || c.broken[e] {
			continue
		}
		sum, ok := c.exited[e], true
		for _, th := range c.threads {
			if !th.known() {
				ok = false
				break
			}
			sum = addCapped(sum, th.last[e])
		}
		if ok && sum <= math.MaxInt64 {
			out.Values[e], out.Measured[e] = int64(sum), true
		}
	}

	return out
}

// decodeGroupRead decodes a PERF_FORMAT_GROUP read with both times.
func decodeGroupRead(buf []byte, events []Event) (CounterReading, error) {
	if len(buf) != 8*(3+len(events)) {
		return CounterReading{}, fmt.Errorf("usage: a counter group read is %d bytes, want %d",
			len(buf), 8*(3+len(events)))
	}
	word := func(i int) uint64 { return binary.NativeEndian.Uint64(buf[8*i:]) }
	if word(0) != uint64(len(events)) {
		return CounterReading{}, fmt.Errorf("usage: a counter group read names %d events, want %d",
			word(0), len(events))
	}
	if word(1) > 1<<62 || word(2) > word(1) {
		return CounterReading{}, fmt.Errorf("usage: a counter group ran %d ns of %d enabled", word(2), word(1))
	}
	r := CounterReading{Enabled: time.Duration(word(1)), Running: time.Duration(word(2))}
	for i, e := range events {
		r.Values[e] = word(3 + i)
	}

	return r, nil
}

// proveCounting opens every event on tid and runs work on it, reading after
// each round, until the group has counted or has been enabled for enough
// without counting; it gives up after rounds, and refuses unless the group
// counted.
//
// ENOUGH IS THE GROUP'S OWN ENABLED TIME, which for a thread's event grows only
// while the thread is on a CPU, so a descheduled thread is not judged on time it
// did not run; and it spans several of the kernel's rotation intervals, because
// a group waiting behind other flexible groups counts only once the kernel has
// rotated each of them past it. A group that ran out of rounds first was never
// given that chance, which is could-not-tell rather than a held counter, and
// even a group that had it is refused without the claim that a counter is
// certainly held.
//
// A GROUP IS SCHEDULED WHOLE OR NOT AT ALL, so one counter held elsewhere (the
// NMI watchdog takes one of Zen 3's six) leaves a group of six enabled and
// never counting, on every vCPU thread of every job. Measured on the reference
// host on 2026-10-09 with nmi_watchdog at 1: perf stat with the six events as
// one group on a live fc_vcpu thread read <not counted> for all six, and the
// same group without stalled-cycles-frontend counted 100% of the time.
func proveCounting(src CounterSource, tid int, work func(), enough time.Duration, rounds int) error {
	g, err := src.Open(tid)
	if err != nil {
		return err
	}
	var r CounterReading
	var readErr error
	for range rounds {
		work()
		if r, readErr = g.Read(); readErr != nil || r.Running > 0 || r.Enabled >= enough {
			break
		}
	}
	if err := errors.Join(readErr, g.Close()); err != nil {
		return fmt.Errorf("usage: read the counters of the proving thread: %w", err)
	}
	if r.Running <= 0 && r.Enabled < enough {
		return fmt.Errorf("usage: the proving thread's counters were enabled for %s of the %s that "+
			"would show whether they count, so whether they can count is unknown", r.Enabled, enough)
	}
	if r.Running <= 0 {
		return errors.New("usage: the counter group was enabled and never counted: another user " +
			"of the CPU's counters holds one (most often the NMI watchdog; sysctl " +
			"kernel.nmi_watchdog=0 frees it), or other groups kept it off the counters for all " +
			"of that time")
	}

	return nil
}

// proveOnEveryCPU runs prove with the proving thread pinned to each of cpus in
// turn, and refuses at the first CPU it fails on or cannot be pinned to.
//
// EVERY CPU, because a counter can be held on some CPUs and not others (a
// pinned perf session, say), and a vCPU thread's group never counts on a CPU
// that has one too few: a proof on whichever CPU the node happened to run on
// says nothing about the rest.
func proveOnEveryCPU(cpus []int, pin func(cpu int) error, prove func() error) error {
	if len(cpus) == 0 {
		return errors.New("usage: no CPU to prove the counters on")
	}
	for _, cpu := range cpus {
		if err := pin(cpu); err != nil {
			return fmt.Errorf("usage: pin the proving thread to CPU %d: %w", cpu, err)
		}
		if err := prove(); err != nil {
			return fmt.Errorf("%w (on CPU %d)", err, cpu)
		}
	}

	return nil
}

// proofBudget is how long a group must be enabled without counting before the
// proof refuses it, given the kernel's rotation interval: ten intervals, so ten
// groups queued ahead of it have each been rotated past, and never less than
// 15 ms.
func proofBudget(rotation time.Duration) time.Duration {
	return max(10*rotation, 15*time.Millisecond)
}
