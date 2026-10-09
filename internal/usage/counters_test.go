package usage

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// refVCPUs are the reference VM's vCPU thread ids.
var refVCPUs = []int{315371, 315372, 315373, 315374, 315375, 315376, 315377, 315378}

// fakeCounters is a CounterSource over threads a test controls: what each
// thread's group reads, which events refuse to open, and which threads cannot
// be opened at all. It records the goroutine of every call, so a test can tell
// whether descriptors stayed on the sampler's goroutine.
type fakeCounters struct {
	mu      sync.Mutex
	reading map[int]CounterReading
	readErr map[int]error
	refuse  map[Event]bool
	// refuseOn refuses an event on one thread only.
	refuseOn map[int]Event
	openErr  map[int]error
	// onOpen runs inside Open, before it returns, to change the world at the
	// moment a race would.
	onOpen func(tid int)
	groups []*fakeGroup
	calls  map[uint64]bool
	// opens counts the opens asked of each thread.
	opens map[int]int
}

type fakeGroup struct {
	src    *fakeCounters
	tid    int
	opened [NumEvents]bool
	closed bool
}

func newFakeCounters() *fakeCounters {
	return &fakeCounters{reading: map[int]CounterReading{}, readErr: map[int]error{},
		refuse: map[Event]bool{}, refuseOn: map[int]Event{}, openErr: map[int]error{},
		calls: map[uint64]bool{}, opens: map[int]int{}}
}

// goroutineID is the calling goroutine's number, from its stack header.
func goroutineID() uint64 {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	field := strings.Fields(strings.TrimPrefix(string(buf), "goroutine "))[0]
	id, err := strconv.ParseUint(field, 10, 64)
	if err != nil {
		panic("goroutineID: " + string(buf))
	}

	return id
}

func (f *fakeCounters) Open(tid int) (CounterGroup, error) {
	f.mu.Lock()
	f.calls[goroutineID()] = true
	f.opens[tid]++
	onOpen := f.onOpen
	err := f.openErr[tid]
	g := &fakeGroup{src: f, tid: tid}
	for e := range NumEvents {
		g.opened[e] = !f.refuse[e]
	}
	if e, ok := f.refuseOn[tid]; ok {
		g.opened[e] = false
	}
	f.mu.Unlock()

	if onOpen != nil {
		onOpen(tid)
	}
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.groups = append(f.groups, g)
	f.mu.Unlock()

	return g, nil
}

func (g *fakeGroup) Opened() [NumEvents]bool { return g.opened }

func (g *fakeGroup) Read() (CounterReading, error) {
	g.src.mu.Lock()
	defer g.src.mu.Unlock()
	g.src.calls[goroutineID()] = true
	if g.closed {
		return CounterReading{}, errors.New("read of a closed group")
	}
	if err := g.src.readErr[g.tid]; err != nil {
		return CounterReading{}, err
	}

	return g.src.reading[g.tid], nil
}

func (g *fakeGroup) Close() error {
	g.src.mu.Lock()
	defer g.src.mu.Unlock()
	g.src.calls[goroutineID()] = true
	g.closed = true

	return nil
}

// set makes every listed thread read the same counts.
func (f *fakeCounters) set(tids []int, r CounterReading) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, tid := range tids {
		f.reading[tid] = r
	}
}

// open reports the groups still open, by thread.
func (f *fakeCounters) open() map[int]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]int{}
	for _, g := range f.groups {
		if !g.closed {
			out[g.tid]++
		}
	}

	return out
}

// reading is a group read of every event at value, scaled by nothing unless
// running differs from enabled.
func reading(enabled, running time.Duration, values ...uint64) CounterReading {
	r := CounterReading{Enabled: enabled, Running: running}
	copy(r.Values[:], values)

	return r
}

// countedVM is the reference VM sampled by a monitor counting it with src.
func countedVM(t *testing.T, src CounterSource) (tree, Target, *Monitor) {
	t.Helper()

	tr, target := referenceVM(t)
	m := runMonitor(t, tr.root, Options{Interval: time.Second, Counters: src})

	return tr, target, m
}

func finalCounters(t *testing.T, m *Monitor) *Counters {
	t.Helper()

	s, ok := m.Final("vm")
	if !ok {
		t.Fatal("Final found no job")
	}
	if s.Counters == nil {
		t.Fatal("a counted job's summary carries no counters")
	}

	return s.Counters
}

// A MULTIPLEXED GROUP IS SCALED BY ITS ENABLED AND RUNNING TIMES, and the job's
// total is the sum over its vCPU threads of each thread's latest cumulative
// reading: a later reading replaces an earlier one rather than adding to it.
func TestCountersAreScaledAndSummedOverTheVCPUThreads(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	_, target, m := countedVM(t, src)
	m.Start("vm", target, 8)

	src.set(refVCPUs, reading(10*time.Second, 5*time.Second, 1000, 1210, 300, 9, 4, 120))
	m.Tick()
	src.set(refVCPUs, reading(20*time.Second, 20*time.Second, 3000, 3630, 900, 27, 12, 360))
	m.Tick()

	c := finalCounters(t, m)
	want := [NumEvents]int64{8 * 3000, 8 * 3630, 8 * 900, 8 * 27, 8 * 12, 8 * 360}
	if c.Values != want || c.Measured != [NumEvents]bool{true, true, true, true, true, true} {
		t.Errorf("counters = %+v, want %v all measured: the latest cumulative reading of each thread", c, want)
	}

	// THE FIRST READING WAS SCALED: half the time running is twice the count.
	m2src := newFakeCounters()
	_, target2, m2 := countedVM(t, m2src)
	m2.Start("vm", target2, 8)
	m2src.set(refVCPUs, reading(10*time.Second, 5*time.Second, 1000, 1210, 300, 9, 4, 120))
	m2.Tick()
	if got := finalCounters(t, m2).Values[Cycles]; got != 8*2000 {
		t.Errorf("cycles = %d, want %d: 1000 counted in half the enabled time is 2000", got, 8*2000)
	}
}

// AN EVENT THE CPU WILL NOT OPEN IS UNMEASURED, and only that event: the group
// opens without it and the rest are counted.
func TestAnEventThatWillNotOpenIsUnmeasured(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	src.refuse[FrontendStalls] = true
	_, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 10, 12, 3, 1, 1, 999))
	m.Tick()

	c := finalCounters(t, m)
	if c.Measured[FrontendStalls] || c.Values[FrontendStalls] != 0 {
		t.Errorf("frontend stalls = %d measured %v, want unmeasured and zero", c.Values[FrontendStalls],
			c.Measured[FrontendStalls])
	}
	if !c.Measured[Instructions] || c.Values[Instructions] != 8*12 {
		t.Errorf("instructions = %d measured %v, want %d", c.Values[Instructions], c.Measured[Instructions], 8*12)
	}
}

// AN EVENT ONE vCPU THREAD COULD NOT COUNT IS UNMEASURED FOR THE JOB, though
// the other seven counted it: their sum would be a smaller total presented as
// the whole job's.
func TestAnEventOneThreadCouldNotCountIsUnmeasured(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	src.refuseOn[315373] = CacheMisses
	_, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 10, 12, 3, 1, 1, 2))
	m.Tick()

	c := finalCounters(t, m)
	if c.Measured[CacheMisses] {
		t.Errorf("cache misses = %d measured, want unmeasured: one thread never counted them",
			c.Values[CacheMisses])
	}
	if !c.Measured[Cycles] || c.Values[Cycles] != 8*10 {
		t.Errorf("cycles = %d measured %v, want %d", c.Values[Cycles], c.Measured[Cycles], 8*10)
	}
}

// A THREAD THAT CANNOT BE OPENED MAKES EVERY EVENT COULD-NOT-TELL: the other
// threads' sum would be a smaller total presented as the whole job's.
func TestAThreadThatCannotBeCountedLeavesNoTotal(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	src.openErr[315374] = errors.New("permission denied")
	_, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 10, 12, 3, 1, 1, 2))
	m.Tick()
	m.Tick()

	if c := finalCounters(t, m); c.Measured != [NumEvents]bool{} {
		t.Errorf("counters = %+v, want every event unmeasured", c)
	}
	// AND IT IS NOT TRIED AGAIN EVERY SAMPLE, which would open a descriptor
	// every sample on a host where opening one can leak it.
	src.mu.Lock()
	defer src.mu.Unlock()
	if n := src.opens[315374]; n != 1 {
		t.Errorf("a thread that could not be opened was asked %d times, want once", n)
	}
}

// A THREAD WITH NO READING IS NOT A THREAD THAT COUNTED ZERO: a group whose
// every read failed leaves the job's counts could-not-tell, while it is open
// and after it is closed.
func TestAThreadThatWasNeverReadIsNotCountedAsZero(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	src.readErr[315372] = errors.New("read failed")
	_, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 10, 12, 3, 1, 1, 2))
	m.Tick()

	// WHILE THE GROUP IS OPEN, as a tick publishes the job's totals and a Final
	// that cannot reach Run answers them.
	m.mu.Lock()
	live := m.jobs["vm"].counters
	m.mu.Unlock()
	if live == nil || live.Measured != [NumEvents]bool{} {
		t.Errorf("the live counters are %+v, want every event unmeasured", live)
	}
	if c := finalCounters(t, m); c.Measured != [NumEvents]bool{} {
		t.Errorf("counters = %+v, want every event unmeasured", c)
	}
}

// A SCALED ESTIMATE MAY FALL BETWEEN TWO HONEST READINGS as the group's share
// of the PMU changes, and the latest is the one reported; a raw count that
// runs backwards is not this group's, and the reading before it stands.
func TestTheLatestEstimateIsReportedAndABackwardsCountIsNot(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	_, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(10, 1, 1000, 1000, 1000, 1000, 1000, 1000))
	m.Tick()
	src.set(refVCPUs, reading(20, 11, 1100, 1100, 1100, 1100, 1100, 1100))
	m.Tick()
	src.set(refVCPUs, reading(30, 21, 900, 1200, 1200, 1200, 1200, 1200))
	m.Tick()

	c := finalCounters(t, m)
	if got := c.Values[Instructions]; got != 8*2000 {
		t.Errorf("instructions = %d, want %d: 1100 counted in 11 of 20 ns is 2000, and a "+
			"reading with a count that ran backwards is not taken", got, 8*2000)
	}
	if !c.Measured[Instructions] {
		t.Error("instructions were not measured")
	}
}

// A THREAD WHOSE IDENTITY CANNOT BE CHECKED AFTER THE OPEN IS CLOSED, and the
// job's counts are could-not-tell: it may be one of the job's vCPU threads,
// and nothing counts it now.
func TestAThreadWhoseIdentityCannotBeCheckedLeavesNoTotal(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target, m := countedVM(t, src)
	var writeErr error
	src.onOpen = func(tid int) {
		if tid == 315376 {
			writeErr = os.WriteFile(filepath.Join(tr.root, "proc", "315359", "task", "315376", "stat"),
				[]byte("unparseable\n"), 0o600)
		}
	}
	src.set(refVCPUs, reading(time.Second, time.Second, 70, 70, 70, 70, 70, 70))
	m.Start("vm", target, 8)
	m.Tick() // the barrier, as above
	if writeErr != nil {
		t.Fatalf("the thread's stat could not be spoiled: %v", writeErr)
	}

	if n := src.open()[315376]; n != 0 {
		t.Errorf("a group whose thread could not be checked is still open (%d)", n)
	}
	if c := finalCounters(t, m); c.Measured != [NumEvents]bool{} {
		t.Errorf("counters = %+v, want every event unmeasured", c)
	}
}

// A GROUP THAT WAS ENABLED AND NEVER RAN CANNOT BE SCALED, and its zeros are
// not a count.
func TestAGroupThatNeverRanIsNotMeasured(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	_, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(5*time.Second, 0))
	m.Tick()

	if c := finalCounters(t, m); c.Measured != [NumEvents]bool{} {
		t.Errorf("counters = %+v, want every event unmeasured", c)
	}
}

// A THREAD THAT EXITS KEEPS ITS LAST READING, and its group is closed.
func TestAThreadThatExitsKeepsItsLastReading(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 100, 100, 100, 100, 100, 100))
	m.Tick()

	tr.remove("/proc/315359/task/315378/stat")
	src.mu.Lock()
	src.readErr[315378] = errors.New("no such process")
	src.mu.Unlock()
	src.set(refVCPUs[:7], reading(2*time.Second, 2*time.Second, 200, 200, 200, 200, 200, 200))
	m.Tick()

	if n := src.open()[315378]; n != 0 {
		t.Errorf("the exited thread still holds %d open groups", n)
	}
	if got := finalCounters(t, m).Values[Cycles]; got != 7*200+100 {
		t.Errorf("cycles = %d, want %d: seven live threads at 200 and the exited one's last 100",
			got, 7*200+100)
	}
}

// A vCPU THREAD THAT APPEARS LATER IS COUNTED FROM THE NEXT SAMPLE.
func TestAThreadThatAppearsLaterIsCounted(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target, m := countedVM(t, src)
	tr.remove("/proc/315359/task/315378/stat")
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 50, 50, 50, 50, 50, 50))
	m.Tick()
	if n := src.open()[315378]; n != 0 {
		t.Fatalf("a thread that is not listed was opened %d times", n)
	}

	tr.write("/proc/315359/task/315378/stat", refThreads["315378"]+"\n")
	m.Tick()
	if n := src.open()[315378]; n != 1 {
		t.Fatalf("the new thread has %d open groups, want 1", n)
	}
	if got := finalCounters(t, m).Values[Cycles]; got != 8*50 {
		t.Errorf("cycles = %d, want %d with the late thread counted", got, 8*50)
	}
}

// A TID THAT STOPPED BEING THE VMM'S BETWEEN THE LISTING AND THE OPEN is
// closed unread: the group counted whatever holds the number now.
func TestATidThatIsNoLongerTheVMMsIsNotCounted(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target, m := countedVM(t, src)
	// ON THE SAMPLER'S GOROUTINE, so it records its failure rather than failing
	// the test from there.
	var removeErr error
	src.onOpen = func(tid int) {
		if tid == 315378 {
			removeErr = os.Remove(filepath.Join(tr.root, "proc", "315359", "task", "315378", "stat"))
		}
	}
	src.set(refVCPUs, reading(time.Second, time.Second, 70, 70, 70, 70, 70, 70))
	m.Start("vm", target, 8)
	// TICK IS THE BARRIER: it waits for Run without a limit, so whatever Run did
	// for Start, the callback included, has happened, even if Start gave up.
	m.Tick()
	if removeErr != nil {
		t.Fatalf("the tid could not be taken from the VMM: %v", removeErr)
	}

	if n := src.open()[315378]; n != 0 {
		t.Errorf("a group on a tid that left the VMM is still open (%d)", n)
	}
	if got := finalCounters(t, m).Values[Cycles]; got != 7*70 {
		t.Errorf("cycles = %d, want %d: the departed tid counted nothing of the job's", got, 7*70)
	}
}

// EVERY DESCRIPTOR IS OPENED, READ AND CLOSED ON THE SAMPLER'S GOROUTINE, and
// every one is closed by Final, by Forget, and when the sampler stops.
func TestCounterGroupsLiveAndDieOnTheSamplersGoroutine(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 1, 1, 1, 1, 1, 1))
	m.Tick()
	first := finalCounters(t, m)
	if open := src.open(); len(open) != 0 {
		t.Errorf("Final left groups open: %v", open)
	}
	// AND A FINAL ASKED AGAIN ANSWERS THE SAME TOTALS without opening anything.
	src.set(refVCPUs, reading(time.Hour, time.Hour, 9, 9, 9, 9, 9, 9))
	if again := finalCounters(t, m); *again != *first || len(src.open()) != 0 {
		t.Errorf("a second Final read %+v with %v open, want %+v and nothing open", again, src.open(), first)
	}

	// FORGET CANNOT CLOSE ANYTHING ITSELF, being on another goroutine; Run's next
	// sweep does.
	other := target
	other.CgroupDir = "/sys/fs/cgroup/other"
	tr.write(other.CgroupDir+"/cpu.stat", "usage_usec 5\nuser_usec 5\nsystem_usec 0\n")
	m.Start("other", other, 8)
	m.Tick() // the barrier: Start may give up before Run has opened anything
	if len(src.open()) == 0 {
		t.Fatal("the second job opened no groups, so this proves nothing about Forget")
	}
	m.Forget("other")
	m.Tick()
	if open := src.open(); len(open) != 0 {
		t.Errorf("a forgotten job's groups are still open after a tick: %v", open)
	}

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.calls) != 1 || src.calls[goroutineID()] {
		t.Errorf("descriptors were touched on %d goroutines (the test's included: %v), want the sampler's alone",
			len(src.calls), src.calls[goroutineID()])
	}
}

// THE SAMPLER CLOSES WHAT IT HOLDS WHEN IT STOPS.
func TestAStoppedSamplerClosesItsGroups(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target := referenceVM(t)
	m := NewMonitor(tr.root, Options{Interval: time.Second, Counters: src})
	m.ticker, m.limit = false, testLimit
	runCtx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(runCtx)
	}()
	m.Start("vm", target, 8)
	m.Tick() // the barrier: Start may give up before Run has opened anything
	if len(src.open()) != 8 {
		t.Fatalf("Start opened %d threads' groups, want 8", len(src.open()))
	}
	stop()
	<-done
	if open := src.open(); len(open) != 0 {
		t.Errorf("a stopped sampler left groups open: %v", open)
	}
}

// NOTHING IS COUNTED WITHOUT A SOURCE, OR FOR A JOB WITH NO VMM.
func TestOnlyAVMMIsCountedAndOnlyWithASource(t *testing.T) {
	t.Parallel()

	tr, target := referenceVM(t)
	m := runMonitor(t, tr.root, Options{Interval: time.Second})
	m.Start("vm", target, 8)
	if s, _ := m.Final("vm"); s.Counters != nil {
		t.Errorf("a monitor with no counter source counted %+v", s.Counters)
	}

	src := newFakeCounters()
	_, container, counted := countedVM(t, src)
	container.PID, container.PIDStart, container.VCPUThreadPrefix = 0, 0, ""
	counted.Start("vm", container, 2)
	if s, _ := counted.Final("vm"); s.Counters != nil || len(src.groups) != 0 {
		t.Errorf("a job with no VMM was counted: %+v, %d groups", s.Counters, len(src.groups))
	}
}

func TestScalingIsExactWhereItCanBe(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name             string
		value            uint64
		enabled, running time.Duration
		want             uint64
		ok               bool
	}{
		{"never multiplexed", 1_234_567, 10, 10, 1_234_567, true},
		{"not yet enabled", 0, 0, 0, 0, true},
		{"half the time", 1000, 10, 5, 2000, true},
		{"a product past 64 bits", math.MaxUint64 / 4, 3 * time.Second, 2 * time.Second, 6_917_529_027_641_081_854, true},
		{"a result past 64 bits", math.MaxUint64 / 2, 10, 1, 0, false},
		{"enabled and never ran", 0, 10, 0, 0, false},
		{"ran longer than enabled", 5, 10, 11, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := scale(tc.value, tc.enabled, tc.running)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("scale(%d, %d, %d) = %d, %v; want %d, %v", tc.value, tc.enabled, tc.running,
					got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A GROUP READ IS DECODED ONLY WHEN IT IS WHOLE: a short read would otherwise
// read as every count being zero.
func TestAGroupReadIsDecodedOnlyWhole(t *testing.T) {
	t.Parallel()

	events := []Event{Cycles, Instructions, BranchMisses}
	words := func(w ...uint64) []byte {
		b := make([]byte, 8*len(w))
		for i, v := range w {
			binary.NativeEndian.PutUint64(b[8*i:], v)
		}
		return b
	}

	r, err := decodeGroupRead(words(3, 1000, 500, 11, 22, 33), events)
	if err != nil || r.Enabled != 1000 || r.Running != 500 || r.Values[Cycles] != 11 ||
		r.Values[Instructions] != 22 || r.Values[BranchMisses] != 33 || r.Values[CacheMisses] != 0 {
		t.Errorf("decoded %+v, %v", r, err)
	}
	for name, tc := range map[string]struct {
		buf  []byte
		want string
	}{
		"empty":                   {nil, "0 bytes"},
		"short":                   {words(3, 1000, 500, 11, 22), "40 bytes"},
		"long":                    {words(3, 1000, 500, 11, 22, 33, 44), "56 bytes"},
		"another group":           {words(2, 1000, 500, 11, 22, 33), "names 2 events"},
		"ran longer than enabled": {words(3, 500, 1000, 11, 22, 33), "ran 1000 ns of 500"},
	} {
		if _, err := decodeGroupRead(tc.buf, events); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: decoded with %v, want a refusal saying %q", name, err, tc.want)
		}
	}
}

// A TID GIVEN TO ANOTHER vCPU THREAD BETWEEN TWO SAMPLES IS ANOTHER THREAD:
// the departed thread's group is retired with its last reading, and the new
// thread gets a group of its own, rather than the old descriptor's final count
// standing in for it.
func TestATidGivenToAnotherThreadIsCountedAsAnotherThread(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target, m := countedVM(t, src)
	m.Start("vm", target, 8)
	src.set(refVCPUs, reading(time.Second, time.Second, 100, 100, 100, 100, 100, 100))
	m.Tick()

	reborn := strings.Replace(refThreads["315378"], " 298947566 ", " 298999999 ", 1)
	if reborn == refThreads["315378"] {
		t.Fatal("the fixture's start time moved, so this restarts nothing")
	}
	tr.write("/proc/315359/task/315378/stat", reborn+"\n")
	src.set([]int{315378}, reading(time.Second, time.Second, 40, 40, 40, 40, 40, 40))
	m.Tick()

	src.mu.Lock()
	opens := src.opens[315378]
	src.mu.Unlock()
	if opens != 2 || src.open()[315378] != 1 {
		t.Errorf("the reused tid was opened %d times with %d groups open, want twice and one",
			opens, src.open()[315378])
	}
	if got := finalCounters(t, m).Values[Cycles]; got != 7*100+100+40 {
		t.Errorf("cycles = %d, want %d: seven threads at 100, the departed one's last 100, and "+
			"its successor's 40", got, 7*100+100+40)
	}
}

// A TID THAT NAMES ANOTHER THREAD BY THE TIME IT IS OPENED is closed unread:
// the group counts the newcomer, which the next listing names and opens as
// itself, and the departed thread is not counted twice.
func TestATidReusedBetweenTheListingAndTheOpenIsNotCountedTwice(t *testing.T) {
	t.Parallel()

	src := newFakeCounters()
	tr, target, m := countedVM(t, src)
	reborn := strings.Replace(refThreads["315378"], " 298947566 ", " 298999999 ", 1)
	var writeErr error
	src.onOpen = func(tid int) {
		if tid == 315378 && writeErr == nil && reborn != "" {
			writeErr = os.WriteFile(filepath.Join(tr.root, "proc", "315359", "task", "315378", "stat"),
				[]byte(reborn+"\n"), 0o600)
			reborn = ""
		}
	}
	src.set(refVCPUs, reading(time.Second, time.Second, 70, 70, 70, 70, 70, 70))
	m.Start("vm", target, 8)
	m.Tick() // the barrier, and the sample that lists the newcomer
	if writeErr != nil || reborn != "" {
		t.Fatalf("the tid was not given to another thread during the open (%v)", writeErr)
	}

	if got := finalCounters(t, m).Values[Cycles]; got != 8*70 {
		t.Errorf("cycles = %d, want %d: the first open counted the newcomer and was closed unread",
			got, 8*70)
	}
}
