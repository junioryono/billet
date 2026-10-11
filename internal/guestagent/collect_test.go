package guestagent

import (
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/junioryono/billet/internal/guestreport"
)

// The fixture's figures, worked by hand from testdata/proc.
var fixtureSample = guestreport.Sample{
	AtMillis: at(0).UnixMilli(),
	// user nice system idle iowait irq softirq steal: 52310 120 18877 2891032 1532 0 911 64
	CPUBusyTicks:      52310 + 120 + 18877 + 0 + 911 + 64,
	CPUTotalTicks:     52310 + 120 + 18877 + 2891032 + 1532 + 0 + 911 + 64,
	MemUsedBytes:      (32891480 - 30455120) * 1024,
	MemAvailableBytes: 30455120 * 1024,
	MemCachedBytes:    1912876 * 1024,
	DiskReadBytes:     2913584 * 512,
	DiskWriteBytes:    6623104 * 512,
	NetRxBytes:        219834567,
	NetTxBytes:        4123456,
}

func TestASampleIsTheGuestTotalsAsProcReportsThem(t *testing.T) {
	t.Parallel()

	log, _ := discardLog()
	c := newCollector(watch(t, os.DirFS("testdata/proc")), log)
	sampleAt(c, at(0), false)

	if len(c.samples) != 1 || c.samples[0] != fixtureSample {
		t.Fatalf("samples = %+v, want [%+v]", c.samples, fixtureSample)
	}
}

// A Sample cannot say one of its figures is unknown, so a reading missing any of
// them is not sent at all: a zero in its place would be a claim.
func TestAFigureTheAgentCannotReadSendsNoSample(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(t *testing.T, proc fstest.MapFS, w *watchedFS)
		kept   int
	}{
		{"nothing missing", func(*testing.T, fstest.MapFS, *watchedFS) {}, 1},
		{"no vda", func(_ *testing.T, proc fstest.MapFS, _ *watchedFS) {
			proc["diskstats"] = &fstest.MapFile{Data: []byte(" 254 16 vdb 312 0 9984 41 0 0 0 0 0 52 41\n")}
		}, 0},
		{"no eth0", func(_ *testing.T, proc fstest.MapFS, _ *watchedFS) {
			proc["net/dev"] = &fstest.MapFile{Data: []byte("Inter-|\n face |\n    lo: 1 2 0 0 0 0 0 0 3 4 0 0 0 0 0 0\n")}
		}, 0},
		{"no MemAvailable", func(_ *testing.T, proc fstest.MapFS, _ *watchedFS) {
			proc["meminfo"] = &fstest.MapFile{Data: []byte("MemTotal: 32891480 kB\nCached: 1912876 kB\n")}
		}, 0},
		{"a cpu line too short", func(_ *testing.T, proc fstest.MapFS, _ *watchedFS) {
			proc["stat"] = &fstest.MapFile{Data: []byte("cpu  1 2 3 4\n")}
		}, 0},
		{"a count that is not one", func(t *testing.T, proc fstest.MapFS, _ *watchedFS) {
			t.Helper()
			set(t, proc, "stat", "cpu ", 1, "-5")
		}, 0},
		{"a count past what a report carries", func(t *testing.T, proc fstest.MapFS, _ *watchedFS) {
			t.Helper()
			set(t, proc, "net/dev", "  eth0:", 1, "9007199254740993")
		}, 0},
		{"stat refused", func(_ *testing.T, _ fstest.MapFS, w *watchedFS) { w.fail["stat"] = eacces }, 0},
		{"net/dev gone", func(_ *testing.T, proc fstest.MapFS, _ *watchedFS) { delete(proc, "net/dev") }, 0},
		{"meminfo past its bound", func(_ *testing.T, proc fstest.MapFS, _ *watchedFS) {
			proc["meminfo"] = &fstest.MapFile{Data: []byte(strings.Repeat("Junk: 1 kB\n", totalsFileLimit/10))}
		}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proc := fixtureProc(t)
			w := watch(t, proc)
			tc.change(t, proc, w)

			log, logged := discardLog()
			c := newCollector(w, log)
			sampleAt(c, at(0), false)

			if len(c.samples) != tc.kept {
				t.Fatalf("kept %d samples (%+v), want %d", len(c.samples), c.samples, tc.kept)
			}

			if tc.kept == 0 && !strings.Contains(logged.String(), "no sample is sent") {
				t.Errorf("the log says %q, want it to say no sample is sent", logged.String())
			}
		})
	}
}

// The node refuses a batch whose time or counters go back, so a reading that
// does is not sent, and the next one that does not is.
func TestASampleThatGoesBackIsNotSent(t *testing.T) {
	t.Parallel()

	proc := fixtureProc(t)
	log, _ := discardLog()
	c := newCollector(proc, log)

	sampleAt(c, at(time.Second), false)
	sampleAt(c, at(time.Second), false) // the same instant
	sampleAt(c, at(0), false)           // a clock stepped back

	set(t, proc, "stat", "cpu ", 1, "52309") // user went back by a tick
	sampleAt(c, at(2*time.Second), false)

	set(t, proc, "diskstats", " 254       0 vda", 9, "6623103")
	set(t, proc, "stat", "cpu ", 1, "52310")
	sampleAt(c, at(3*time.Second), false)

	set(t, proc, "diskstats", "254 0 vda", 9, "6623105") // set wrote the line back unpadded
	sampleAt(c, at(4*time.Second), false)

	if len(c.samples) != 2 || c.samples[1].AtMillis != at(4*time.Second).UnixMilli() {
		t.Fatalf("kept %+v, want the first and the one at 4s", c.samples)
	}
}

// What the collector holds is one batch's worth; past it no reading is taken but
// the final one, whose place is kept for it, and once a batch takes them the next
// one is.
func TestPendingReadingsStopAtABatchsBound(t *testing.T) {
	t.Parallel()

	log, _ := discardLog()
	c := newCollector(fixtureProc(t), log)

	for i := range guestreport.MaxBatchSamples + 6 {
		sampleAt(c, at(time.Duration(i)*time.Second), false)
	}

	for i := range guestreport.MaxBatchProcessSamples + 6 {
		walkAt(c, at(time.Duration(i)*2*time.Second), false)
	}

	if len(c.samples) != guestreport.MaxBatchSamples-1 || len(c.processes) != guestreport.MaxBatchProcessSamples-1 {
		t.Fatalf("held %d samples and %d process samples, want %d and %d, the last place kept", len(c.samples),
			len(c.processes), guestreport.MaxBatchSamples-1, guestreport.MaxBatchProcessSamples-1)
	}

	end := at(10 * time.Minute)
	sampleAt(c, end, true)
	walkAt(c, end, true)
	sampleAt(c, end.Add(time.Second), true)

	if len(c.samples) != guestreport.MaxBatchSamples || len(c.processes) != guestreport.MaxBatchProcessSamples ||
		c.samples[len(c.samples)-1].AtMillis != end.UnixMilli() ||
		c.processes[len(c.processes)-1].AtMillis != end.UnixMilli() {
		t.Fatalf("the final reading was not kept in its place: %d samples, %d process samples", len(c.samples),
			len(c.processes))
	}

	samples, processes := c.take()
	if len(samples) != guestreport.MaxBatchSamples || len(processes) != guestreport.MaxBatchProcessSamples ||
		len(c.samples) != 0 || len(c.processes) != 0 {
		t.Fatalf("take handed over %d and %d and left %d and %d", len(samples), len(processes),
			len(c.samples), len(c.processes))
	}

	sampleAt(c, at(time.Hour), false)
	walkAt(c, at(time.Hour), false)

	if len(c.samples) != 1 || len(c.processes) != 1 {
		t.Fatalf("after a take, held %d samples and %d process samples, want one each", len(c.samples),
			len(c.processes))
	}
}

func TestAProcessIsNamedByTheTextBetweenItsFirstParenthesisAndItsLast(t *testing.T) {
	t.Parallel()

	tail := " S 1 2 2 0 -1 4194560 10 0 0 0 31 7 0 0 20 0 1 0 4100 1000 10 0"

	for _, tc := range []struct{ line, comm string }{
		{"812 (Runner.Worker)" + tail, "Runner.Worker"},
		{"900 (my) (weird comm)" + tail, "my) (weird comm"},
		{"901 (two words)" + tail, "two words"},
		{"902 ())" + tail, ")"},
		{"903 (()" + tail, "("},
		{"904 (a\nb)" + tail, "a\nb"},
		{"905 ()" + tail, ""},
	} {
		st, err := parsePIDStat([]byte(tc.line))
		if err != nil {
			t.Errorf("%q: %v", tc.line, err)

			continue
		}

		if st.comm != tc.comm || st.cpu != 38 || st.start != 4100 || st.state != 'S' {
			t.Errorf("%q parsed as %+v, want comm %q, cpu 38, start 4100", tc.line, st, tc.comm)
		}
	}

	for _, line := range []string{
		"812 Runner.Worker" + tail,
		"812 (Runner.Worker S 1 2",
		"812 (Runner.Worker) S 1 2 2 0 -1 4194560",
		"812 (x) S 1 2 2 0 -1 4194560 10 0 0 0 -31 7 0 0 20 0 1 0 4100",
	} {
		if _, err := parsePIDStat([]byte(line)); err == nil {
			t.Errorf("%q parsed, want could-not-tell", line)
		}
	}
}

// A process's argv and environment can hold a secret the job's masker never sees;
// the agent reads its stat, status and io, and opens neither. watch fails the test
// on an open of either, and the opens it saw prove the walk went through it.
func TestTheWalkNeverOpensACmdlineOrAnEnviron(t *testing.T) {
	t.Parallel()

	w := watch(t, os.DirFS("testdata/proc"))
	log, _ := discardLog()
	c := newCollector(w, log)

	walkAt(c, at(0), false)
	walkAt(c, at(2*time.Second), false)
	c.readAgentCPU()

	if len(c.processes) != 1 {
		t.Fatalf("kept %d process samples, want 1", len(c.processes))
	}

	for _, pid := range []string{"1", "2", "812", "900", "1033"} {
		for _, file := range []string{"stat", "status", "io"} {
			if !w.sawOpen(pid + "/" + file) {
				t.Errorf("the walk did not read %s/%s through the watched tree", pid, file)
			}
		}
	}
}

// Each name's row is what its processes did since the last kept walk; a process
// new since then (a reused pid included) did all it has done since; the busy CPU
// no live process accounts for is the residual.
func TestAProcessSampleIsWhatEachNameDidSinceTheLastWalk(t *testing.T) {
	t.Parallel()

	proc := fixtureProc(t)
	log, _ := discardLog()
	c := newCollector(proc, log)
	walkAt(c, at(0), false)

	if len(c.processes) != 0 {
		t.Fatalf("the first walk kept %d process samples, want none: it only sets the base", len(c.processes))
	}

	set(t, proc, "812/stat", "812 ", 13, "1930")        // utime +100
	set(t, proc, "812/io", "read_bytes:", 1, "1052672") // +4096
	set(t, proc, "900/stat", "900 ", 16, "73")          // stime +50 (the comm is three fields)
	set(t, proc, "1/stat", "1 ", 21, "16")              // pid 1 is another process now
	proc["1500/stat"] = &fstest.MapFile{Data: []byte("1500 (make) R 812 1500 1500 0 -1 4194560 1 0 0 0 30 0 0 0 20 0 1 0 5000 1 1 0\n")}
	proc["1500/status"] = &fstest.MapFile{Data: []byte("Name:\tmake\nVmRSS:\t    1024 kB\n")}
	proc["1500/io"] = &fstest.MapFile{Data: []byte("read_bytes: 0\nwrite_bytes: 512\n")}
	set(t, proc, "stat", "cpu ", 1, strconv.Itoa(52310+1000)) // busy +1000

	walkAt(c, at(2*time.Second), false)

	if len(c.processes) != 1 {
		t.Fatalf("kept %d process samples, want 1", len(c.processes))
	}

	got := c.processes[0]
	rows := map[string]guestreport.ProcessUsage{}

	for _, r := range got.Rows {
		rows[r.Name] = r.ProcessUsage
	}

	want := map[string]guestreport.ProcessUsage{
		"Runner.Worker":   {Procs: 1, CPUTicks: 100, RSSBytes: 98304 * 1024, ReadBytes: 4096},
		"my) (weird comm": {Procs: 1, CPUTicks: 50, RSSBytes: 2048 * 1024},
		"make":            {Procs: 1, CPUTicks: 30, RSSBytes: 1024 * 1024, WriteBytes: 512},
		"systemd":         {Procs: 1, CPUTicks: 380, RSSBytes: 13016 * 1024, ReadBytes: 40960, WriteBytes: 8192},
		"kthreadd":        {Procs: 1},
		"billet":          {Procs: 1, RSSBytes: 18432 * 1024},
	}

	for name, usage := range want {
		if rows[name] != usage {
			t.Errorf("row %q = %+v, want %+v", name, rows[name], usage)
		}
	}

	if len(rows) != len(want) {
		t.Errorf("rows %+v, want exactly %v", got.Rows, want)
	}

	if got.ResidualCPUTicks != 1000-100-50-30-380 || got.AtMillis != at(2*time.Second).UnixMilli() {
		t.Errorf("residual %d at %d, want %d at %d", got.ResidualCPUTicks, got.AtMillis, 1000-100-50-30-380,
			at(2*time.Second).UnixMilli())
	}
}

// A process gone between the listing and its reads is left out of the walk, and
// what it used goes to the residual; a file the agent is refused makes the whole
// walk could-not-tell, the base stays, and the next kept walk covers both intervals.
func TestAnExitedProcessIsResidualAndARefusedOneIsCouldNotTell(t *testing.T) {
	t.Parallel()

	proc := fixtureProc(t)
	w := watch(t, proc)
	log, logged := discardLog()
	c := newCollector(w, log)
	walkAt(c, at(0), false)

	set(t, proc, "812/stat", "812 ", 13, "1930") // +100
	set(t, proc, "stat", "cpu ", 1, "52710")     // busy +400
	w.fail["812/io"] = eacces
	walkAt(c, at(2*time.Second), false)

	if len(c.processes) != 0 || !strings.Contains(logged.String(), "no process sample is sent") {
		t.Fatalf("a refused io kept %+v and logged %q; want nothing kept, and said", c.processes, logged.String())
	}

	delete(w.fail, "812/io")
	set(t, proc, "812/stat", "812 ", 13, "1980") // +50 more
	w.fail["900/io"] = fs.ErrNotExist            // 900 exited mid-walk
	walkAt(c, at(4*time.Second), false)

	if len(c.processes) != 1 {
		t.Fatalf("kept %d process samples, want 1", len(c.processes))
	}

	got := c.processes[0]
	for _, r := range got.Rows {
		if r.Name == "my) (weird comm" {
			t.Errorf("an exited process has a row: %+v", r)
		}

		if r.Name == "Runner.Worker" && r.CPUTicks != 150 {
			t.Errorf("Runner.Worker used %d ticks, want 150 since the last kept walk", r.CPUTicks)
		}
	}

	if got.ResidualCPUTicks != 400-150 {
		t.Errorf("residual %d, want %d", got.ResidualCPUTicks, 400-150)
	}
}

// A kernel thread has no VmRSS line and no memory of its own; a user process
// without one is a status the agent could not read.
func TestOnlyAKernelThreadGoesWithoutVmRSS(t *testing.T) {
	t.Parallel()

	proc := fixtureProc(t)
	log, _ := discardLog()
	c := newCollector(proc, log)
	walkAt(c, at(0), false)
	walkAt(c, at(2*time.Second), false)

	if len(c.processes) != 1 {
		t.Fatalf("with kthreadd's status lacking VmRSS, kept %d process samples, want 1", len(c.processes))
	}

	proc["812/status"] = &fstest.MapFile{Data: []byte("Name:\tRunner.Worker\nThreads:\t9\n")}
	walkAt(c, at(4*time.Second), false)

	if len(c.processes) != 1 {
		t.Fatalf("with a user process lacking VmRSS, kept %d process samples, want still 1", len(c.processes))
	}
}

func TestMoreProcessesThanTheBoundAreCouldNotTell(t *testing.T) {
	t.Parallel()

	proc := fixtureProc(t)
	for pid := 2000; pid < 2000+maxProcesses; pid++ {
		proc[strconv.Itoa(pid)+"/stat"] = &fstest.MapFile{}
	}

	log, logged := discardLog()
	c := newCollector(proc, log)

	if _, err := c.pids(); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("listing %d processes answered %v, want could-not-tell", maxProcesses+5, err)
	}

	walkAt(c, at(0), false)

	if c.haveBase || !strings.Contains(logged.String(), "more than") {
		t.Errorf("a walk past the bound set a base (%v) or was not logged (%q)", c.haveBase, logged.String())
	}
}

func TestTheAgentsOwnCPUIsItsSelfStatAndNeverFalls(t *testing.T) {
	t.Parallel()

	proc := fixtureProc(t)
	w := watch(t, proc)
	log, _ := discardLog()
	c := newCollector(w, log)

	w.fail["self/stat"] = eacces
	c.readAgentCPU()

	if c.haveAgentCPU {
		t.Fatalf("a failed first read made the agent CPU known, as %d", c.agentCPU)
	}

	delete(w.fail, "self/stat")
	c.readAgentCPU()

	if c.agentCPU != 41+19 || !c.haveAgentCPU {
		t.Fatalf("agent CPU %d (known %v), want %d", c.agentCPU, c.haveAgentCPU, 41+19)
	}

	w.fail["self/stat"] = eacces
	c.readAgentCPU()

	if c.agentCPU != 60 {
		t.Fatalf("after a failed read the agent CPU is %d, want the last read, 60", c.agentCPU)
	}
}
