package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/usage"
)

// recordingLedger is the ledger as a node reaches it, keeping, in order, every
// renewal and every usage and guest report sent, whether the ledger kept it
// or not: a report the ledger refused is still one the node should not have
// sent.
type recordingLedger struct {
	*alloc.Allocator

	mu     sync.Mutex
	events []string
	// failRead, when positive, is how many lease reads succeed before one fails,
	// as a ledger blip would; that read disarms it.
	failRead atomic.Int32
	// failRenewal, when positive, is how many renewals land before one is lost
	// on the way, as a dropped connection would; that renewal disarms it.
	failRenewal atomic.Int32
	// onRead, when set, runs after every lease read that succeeded.
	onRead func(leaseID string)
	// afterRenewal, when set, runs after every renewal the ledger answered,
	// with its answer, before the answer is returned.
	afterRenewal func(leaseID string, err error)
}

func (l *recordingLedger) Lease(ctx context.Context, leaseID string) (*alloc.Lease, error) {
	if l.failRead.Load() > 0 && l.failRead.Add(-1) == 0 {
		return nil, errors.New("the ledger could not be read")
	}
	lease, err := l.Allocator.Lease(ctx, leaseID)
	if err == nil && l.onRead != nil {
		l.onRead(leaseID)
	}

	return lease, err
}

func (l *recordingLedger) Release(ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase) error {
	l.note("release " + leaseID)

	return l.Allocator.Release(ctx, leaseID, epoch, outcome)
}

func (l *recordingLedger) note(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *recordingLedger) Heartbeat(ctx context.Context, leaseID string, epoch int64) error {
	l.note("heartbeat " + leaseID)
	if l.failRenewal.Load() > 0 && l.failRenewal.Add(-1) == 0 {
		return errors.New("the renewal was lost on the way")
	}
	err := l.Allocator.Heartbeat(ctx, leaseID, epoch)
	if l.afterRenewal != nil {
		l.afterRenewal(leaseID, err)
	}

	return err
}

func (l *recordingLedger) RecordLeaseUsage(ctx context.Context, leaseID string, epoch int64,
	u alloc.JobUsage, series *alloc.UsageSeries,
) error {
	l.note(fmt.Sprintf("usage %s@%d", leaseID, epoch))

	return l.Allocator.RecordLeaseUsage(ctx, leaseID, epoch, u, series)
}

func (l *recordingLedger) RecordGuestReport(ctx context.Context, leaseID string, epoch int64,
	report alloc.GuestReport,
) error {
	l.note("guest " + leaseID)

	return l.Allocator.RecordGuestReport(ctx, leaseID, epoch, report)
}

// sent is every event so far, in order.
func (l *recordingLedger) sent() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return slices.Clone(l.events)
}

// usageReports is how many usage reports were sent.
func (l *recordingLedger) usageReports() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if strings.HasPrefix(e, "usage ") {
			n++
		}
	}

	return n
}

// handoverHost is one node's host as two processes of it see it: one ledger,
// one backend whose instances outlive either process, and one state directory.
type handoverHost struct {
	a     *alloc.Allocator
	rec   *recordingLedger
	host  string
	p     *measuredProvider
	root  string
	dir   string
	lease *alloc.Lease
	name  string
	id    string
}

func newHandoverHost(t *testing.T) *handoverHost {
	t.Helper()

	root := t.TempDir()
	h := &handoverHost{root: root, dir: t.TempDir(),
		p: &measuredProvider{fakeProvider: &fakeProvider{kind: config.ProviderDocker}, root: root}}
	h.a, h.host = newAllocatorWithHost(t)
	h.rec = &recordingLedger{Allocator: h.a}
	h.lease = assignedLease(t, h.a)
	h.name = provider.InstanceName(h.lease.ID)
	h.id = "instance-" + h.name

	return h
}

// process is one node process on the host, with a sampler of its own,
// reaching the ledger through the recorder.
func (h *handoverHost) process(t *testing.T) (*Runner, *usage.Monitor) {
	t.Helper()

	m := runningMonitor(t, h.root)

	return New(h.rec, h.host, &fakeJIT{setID: 7}, h.p, nil, WithMonitor(m), WithUsageCheckpoints(h.dir)), m
}

// launch starts the job on r, its cgroup having used a millisecond.
func (h *handoverHost) launch(t *testing.T, r *Runner) {
	t.Helper()

	h.p.writeCPU(t, h.id, "1000")
	if err := r.Launch(t.Context(), h.lease, dockerSpec(), Job{RequestID: h.lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
}

// timeline is the series the ledger kept for the lease.
func (h *handoverHost) timeline(t *testing.T) usage.Timeline {
	t.Helper()

	return h.timelineOf(t, h.lease.ID)
}

// timelineOf is the series the ledger kept for a lease on the host.
func (h *handoverHost) timelineOf(t *testing.T, leaseID string) usage.Timeline {
	t.Helper()

	series, err := h.a.LeaseUsageSeries(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("no series was kept: %v", err)
	}
	tl, err := usage.TimelineOf(series.Codec, series.Data)
	if err != nil {
		t.Fatalf("the kept series does not read back: %v", err)
	}

	return tl
}

// within is what ch answers, failing the test if it does not within ten
// seconds.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not finish", what)
		panic("unreachable")
	}
}

// causes are the marks of a timeline's points.
func causes(tl usage.Timeline) []usage.GapCause {
	var out []usage.GapCause
	for _, p := range tl.Points {
		out = append(out, p.Gap)
	}

	return out
}

// A JOB THAT RUNS ACROSS A HANDOVER REPORTS ITS USAGE (#384). The process that
// launched it keeps what it measured as it hands over; the next process adopts
// the job, continues its series from that, and when the job completes, the
// path that ends an adopted job reports it, fenced on the lease, with what
// both processes sampled and the time between them marked handover. The
// checkpoint is used once.
func TestAJobRunAcrossAHandoverReportsUsageFromBothProcesses(t *testing.T) {
	h := newHandoverHost(t)

	first, firstMonitor := h.process(t)
	h.launch(t, first)
	h.p.writeCPU(t, h.id, "2000000")
	firstMonitor.Tick()
	first.HandOver(t.Context())
	checkpoint := filepath.Join(h.dir, h.name+checkpointSuffix)
	if _, err := os.Stat(checkpoint); err != nil {
		t.Fatalf("the handover kept no checkpoint: %v", err)
	}

	next, nextMonitor := h.process(t)
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := os.Stat(checkpoint); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the checkpoint outlived the adoption that read it: %v", err)
	}
	h.p.writeCPU(t, h.id, "7000000")
	nextMonitor.Tick()

	// THE JOB COMPLETES, and its completion's destroy reaches the process that
	// adopted it, which holds it in custody.
	if err := next.Destroy(t.Context(), h.lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, err := h.a.LeaseUsage(t.Context(), h.lease.ID)
	if err != nil {
		t.Fatalf("a job run across a handover reported no usage: %v", err)
	}
	if got.CPUUserMicros != 7_000_000 {
		t.Errorf("cpu = %d µs, want the 7000000 read before the destroy", got.CPUUserMicros)
	}
	if got.Counters != nil {
		t.Errorf("a handed-over job reported hardware counters %+v, which counted only part of its life", got.Counters)
	}
	tl := h.timeline(t)
	marks := causes(tl)
	handovers := 0
	for _, c := range marks {
		if c == usage.GapHandover {
			handovers++
		}
	}
	if len(tl.Points) < 5 || handovers != 1 || marks[1] != usage.GapNone {
		t.Errorf("marks = %v, want the first process's samples unmarked and one handover", marks)
	}
	if tl.Points[1].CPUUsage != 2_000_000 {
		t.Errorf("the first process's tick is gone from the series: %+v", tl.Points[1])
	}
	if _, ok := nextMonitor.Final(h.name); ok {
		t.Error("the sampler still measures a job whose compute is gone")
	}
}

// A PROCESS THAT ADOPTS ITS OWN JOB KEEPS MEASURING IT (#384). Recovery runs at
// every registration and moves this process's own running jobs into custody;
// their series goes on unbroken, and the custody that ends them reports it.
func TestAJobItsOwnProcessReadoptsKeepsAnUnbrokenSeries(t *testing.T) {
	h := newHandoverHost(t)

	r, m := h.process(t)
	h.launch(t, r)
	h.p.writeCPU(t, h.id, "2000000")
	m.Tick()
	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if held, _ := r.heldForRequest(h.lease.RequestID); held == "" {
		t.Fatal("recovery did not adopt the process's own job, so this proves nothing")
	}
	h.p.writeCPU(t, h.id, "3000000")
	m.Tick()
	if err := r.Destroy(t.Context(), h.lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, err := h.a.LeaseUsage(t.Context(), h.lease.ID)
	if err != nil || got.CPUUserMicros != 3_000_000 {
		t.Fatalf("usage = %+v, %v; want the 3000000 read before the destroy", got, err)
	}
	tl := h.timeline(t)
	for i, c := range causes(tl) {
		if c != usage.GapNone {
			t.Errorf("point %d is marked %v in a series one process sampled throughout", i, c)
		}
	}
	if len(tl.Points) != 4 || tl.Points[1].CPUUsage != 2_000_000 || tl.Points[2].CPUUsage != 3_000_000 {
		t.Errorf("the series lost samples across the re-adoption: %+v", tl.Points)
	}
}

// A JOB ADOPTED WITH NOTHING HANDED OVER IS MEASURED FROM ITS ADOPTION: a
// process that crashed left no checkpoint, and what the job's cgroup counts
// for its whole life is still its usage.
func TestAJobAdoptedWithoutACheckpointIsMeasuredFromItsAdoption(t *testing.T) {
	h := newHandoverHost(t)

	first, _ := h.process(t)
	h.launch(t, first)

	next, nextMonitor := h.process(t)
	h.p.writeCPU(t, h.id, "4000000")
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.p.writeCPU(t, h.id, "6000000")
	nextMonitor.Tick()
	if err := next.Destroy(t.Context(), h.lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	got, err := h.a.LeaseUsage(t.Context(), h.lease.ID)
	if err != nil || got.CPUUserMicros != 6_000_000 {
		t.Fatalf("usage = %+v, %v; want the cgroup's 6000000", got, err)
	}
	tl := h.timeline(t)
	if tl.Points[0].CPUUsage != 4_000_000 {
		t.Errorf("the series begins at %d µs, want the adoption's 4000000", tl.Points[0].CPUUsage)
	}
}

// AN ADOPTED JOB WHOSE LEASE IS GONE REPORTS NOTHING: the report is fenced on
// a lease the ledger no longer holds, and the job stops being measured.
func TestAnAdoptedJobWhoseLeaseIsGoneReportsNothing(t *testing.T) {
	h := newHandoverHost(t)

	first, _ := h.process(t)
	h.launch(t, first)
	next, nextMonitor := h.process(t)
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	lease, err := h.a.Lease(t.Context(), h.lease.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := next.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if n := h.rec.usageReports(); n != 0 {
		t.Errorf("%d usage reports were sent for a lease that was gone, want none", n)
	}
	if _, ok := nextMonitor.Final(h.name); ok {
		t.Error("the sampler still measures a job whose lease is gone")
	}
}

// RECOVERY RENEWS EVERY SURVIVOR BEFORE IT MEASURES ANY: finding a job's
// counters asks the backend and the sampler, and done inline it held the next
// survivor's first renewal behind it.
func TestRecoveryRenewsEverySurvivorBeforeItMeasuresAny(t *testing.T) {
	h := newHandoverHost(t)
	second := assignedLease(t, h.a)

	first, _ := h.process(t)
	h.launch(t, first)
	if err := first.Launch(t.Context(), second, dockerSpec(),
		Job{RequestID: second.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	h.p.onTarget = func(id string) { h.rec.note("target " + id) }

	next, _ := h.process(t)
	h.rec.mu.Lock()
	h.rec.events = nil
	h.rec.mu.Unlock()
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	h.rec.mu.Lock()
	events := slices.Clone(h.rec.events)
	h.rec.mu.Unlock()
	firstTarget, targets := len(events), 0
	for i, e := range events {
		if strings.HasPrefix(e, "target ") {
			targets++
			firstTarget = min(firstTarget, i)
		}
	}
	for _, leaseID := range []string{h.lease.ID, second.ID} {
		if i := slices.Index(events, "heartbeat "+leaseID); i < 0 || i > firstTarget {
			t.Errorf("lease %s was renewed at %d, want before the first lookup at %d: %v", leaseID, i,
				firstTarget, events)
		}
	}
	if targets != 2 {
		t.Errorf("recovery looked for %d jobs' counters, want both survivors': %v", targets, events)
	}
}

// A RECOVERY THAT FAILS PART WAY STILL MEASURES WHAT IT ADOPTED, once it has
// stopped adopting: a survivor it never reached is neither adopted nor renewed,
// and is left with its checkpoint to the retry; and a job it did adopt that
// ends before that retry reports what the handover kept, where measuring it
// only at the retry left nothing to report and its checkpoint pruned.
func TestARecoveryThatFailsPartWayMeasuresWhatItAdopted(t *testing.T) {
	h := newHandoverHost(t)
	second := assignedLease(t, h.a)

	first, firstMonitor := h.process(t)
	h.launch(t, first)
	if err := first.Launch(t.Context(), second, dockerSpec(),
		Job{RequestID: second.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	h.p.writeCPU(t, h.id, "2000000")
	h.p.writeCPU(t, "instance-"+provider.InstanceName(second.ID), "2000000")
	firstMonitor.Tick()
	first.HandOver(t.Context())

	// THE SECOND SURVIVOR RECOVERY READS, in the backend's order, so the first
	// is adopted and renewed before the recovery fails.
	adopted, missed := h.lease, second
	if provider.InstanceName(second.ID) < h.name {
		adopted, missed = second, h.lease
	}
	adoptedName, missedName := provider.InstanceName(adopted.ID), provider.InstanceName(missed.ID)
	next, nextMonitor := h.process(t)
	h.rec.failRead.Store(2)
	if err := next.Recover(t.Context()); err == nil {
		t.Fatal("a recovery that could not read a survivor's lease succeeded")
	}
	if !nextMonitor.Measuring(adoptedName) || nextMonitor.Measuring(missedName) {
		t.Fatalf("measuring %s=%v and %s=%v, want only the survivor the failed recovery adopted",
			adoptedName, nextMonitor.Measuring(adoptedName), missedName, nextMonitor.Measuring(missedName))
	}
	if _, err := os.Stat(filepath.Join(h.dir, missedName+checkpointSuffix)); err != nil {
		t.Errorf("the checkpoint of the survivor not yet adopted is gone: %v", err)
	}

	// THE ADOPTED JOB ENDS before any retry, and the next tend finds it gone.
	delete(h.p.live, adoptedName)
	if err := next.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}
	if _, err := h.a.LeaseUsage(t.Context(), adopted.ID); err != nil {
		t.Fatalf("a job adopted by a recovery that failed, and ended before the retry, reported no usage: %v", err)
	}
	tl := h.timelineOf(t, adopted.ID)
	if marks := causes(tl); len(tl.Points) < 2 || !slices.Contains(marks, usage.GapHandover) ||
		tl.Points[1].CPUUsage != 2_000_000 {
		t.Errorf("marks = %v, points = %+v; want the series continued from the handover", marks, tl.Points)
	}
}

// AN ADOPTION WHOSE OWN RENEWAL IS LOST IS STILL MEASURED: the entry holds the
// lease regardless and renewal is retried, and the recovery that fails on it
// measures the job after the renewal it tried, so the job reports what the
// handover kept when it ends before the retry.
func TestAnAdoptionWhoseRenewalFailedIsStillMeasured(t *testing.T) {
	h := newHandoverHost(t)

	first, firstMonitor := h.process(t)
	h.launch(t, first)
	h.p.writeCPU(t, h.id, "2000000")
	firstMonitor.Tick()
	first.HandOver(t.Context())

	next, nextMonitor := h.process(t)
	h.p.onTarget = func(id string) { h.rec.note("target " + id) }
	h.rec.failRenewal.Store(1)
	if err := next.Recover(t.Context()); err == nil || !strings.Contains(err.Error(), "lost on the way") {
		t.Fatalf("Recover = %v, want the lost renewal", err)
	}
	events := h.rec.sent()
	renewed, looked := slices.Index(events, "heartbeat "+h.lease.ID), slices.Index(events, "target "+h.id)
	if renewed < 0 || looked < renewed {
		t.Errorf("events = %v, want the counters looked for after the renewal was tried", events)
	}
	if !nextMonitor.Measuring(h.name) {
		t.Fatal("a job whose adoption's renewal was lost is not measured")
	}

	delete(h.p.live, h.name)
	if err := next.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}
	if marks := causes(h.timeline(t)); !slices.Contains(marks, usage.GapHandover) {
		t.Errorf("marks = %v, want the series continued from the handover", marks)
	}
}

// AN ADOPTION WAITS FOR A TEND SETTLING THE ENTRY IT REPLACES: the tend holds
// the entry from its last sample to its release, and a re-registration that
// took the report from it in the middle left the tend releasing the lease
// unreported and the new entry with a lease that was gone. The tend is held
// in the destroy, after its last sample, and the adoption is let run up to
// the entry before the tend goes on; the job is reported once, before its
// release.
func TestAnAdoptionWaitsForATendSettlingTheEntryItReplaces(t *testing.T) {
	h := newHandoverHost(t)

	first, _ := h.process(t)
	h.launch(t, first)
	next, _ := h.process(t)
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// EVERY WAY OUT OF THIS TEST RELEASES THE TEND AND JOINS BOTH WORKERS, which
	// run detached from the test's context.
	inDestroy, resume := make(chan struct{}), make(chan struct{})
	var resumed sync.Once
	release := func() { resumed.Do(func() { close(resume) }) }
	var workers sync.WaitGroup
	t.Cleanup(func() {
		release()
		joined := make(chan struct{})
		go func() {
			workers.Wait()
			close(joined)
		}()
		select {
		case <-joined:
		case <-time.After(10 * time.Second):
			t.Error("a worker outlived the test")
		}
	})
	var once sync.Once
	h.p.onDestroy = func(string) {
		once.Do(func() {
			close(inDestroy)
			<-resume
		})
	}
	destroyed := make(chan error, 1)
	workers.Go(func() { destroyed <- next.Destroy(context.WithoutCancel(t.Context()), h.lease.RequestID) })
	select {
	case <-inDestroy:
	case err := <-destroyed:
		t.Fatalf("the destroy ended (%v) without reaching the backend", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy never reached the backend")
	}

	read := make(chan struct{})
	var readOnce sync.Once
	h.rec.onRead = func(leaseID string) {
		if leaseID == h.lease.ID {
			readOnce.Do(func() { close(read) })
		}
	}
	readopted := make(chan error, 1)
	workers.Go(func() { readopted <- next.Recover(context.WithoutCancel(t.Context())) })
	select {
	case <-read:
	case err := <-readopted:
		t.Fatalf("the re-registration's recovery ended (%v) without reading the lease", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the re-registration's recovery never read the lease")
	}
	// THE ADOPTION IS NEXT for the recovery that read the lease. Nothing it does
	// short of the swap itself can be observed, so it is given a moment: the
	// time is a window for the defect to show in, and the assertions below
	// hold however long it is.
	time.Sleep(50 * time.Millisecond)
	release()

	destroyErr, readoptErr := within(t, destroyed, "the destroy"), within(t, readopted, "the re-adoption")
	if destroyErr != nil {
		t.Fatalf("Destroy: %v", destroyErr)
	}
	// THE ADOPTION RAN ONCE THE TEND HAD RELEASED THE LEASE, so its renewal
	// finds the lease gone.
	if !errors.Is(readoptErr, alloc.ErrLeaseNotFound) {
		t.Errorf("the re-adoption's recovery = %v, want its renewal to find the lease gone", readoptErr)
	}
	if err := next.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	events := h.rec.sent()
	reported := slices.Index(events, fmt.Sprintf("usage %s@%d", h.lease.ID, h.lease.Epoch))
	released := slices.Index(events, "release "+h.lease.ID)
	if reported < 0 || released < 0 || reported > released || h.rec.usageReports() != 1 {
		t.Errorf("events = %v, want one usage report and it before the release", events)
	}
}

// A JOB WHOSE ENTRY OWES NO REPORT IS NOT LEFT MEASURED: one that settled, or
// whose lease went, while its counters were being found starts a series that
// nothing would ever end.
func TestAResumeForAnEntryThatOwesNothingIsForgotten(t *testing.T) {
	h := newHandoverHost(t)

	first, _ := h.process(t)
	h.launch(t, first)
	next, nextMonitor := h.process(t)

	next.resumeMonitoring(t.Context(), h.lease, &provider.Instance{ID: h.id, Name: h.name, Running: true})
	if nextMonitor.Measuring(h.name) {
		t.Error("a job with no custody entry owing its report was left measured")
	}
}

// AN ENTRY THAT SETTLES WHILE ITS COUNTERS ARE BEING FOUND leaves its job
// unmeasured: the lease goes and a tend settles the entry inside the lookup,
// after the adoption said the job was owed a report and before the series
// begins.
func TestAnEntrySettledDuringItsLookupLeavesNothingMeasured(t *testing.T) {
	h := newHandoverHost(t)

	first, _ := h.process(t)
	h.launch(t, first)
	next, nextMonitor := h.process(t)
	settled := false
	h.p.onTarget = func(string) {
		if settled {
			return
		}
		settled = true
		lease, err := h.a.Lease(t.Context(), h.lease.ID)
		if err != nil {
			t.Errorf("read the lease: %v", err)
			return
		}
		if err := h.a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			t.Errorf("Release: %v", err)
		}
		if err := next.Tend(t.Context()); err != nil {
			t.Errorf("Tend: %v", err)
		}
	}
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if !settled {
		t.Fatal("recovery looked for no counters, so this proves nothing")
	}
	if next.owesReport(h.lease.ID, h.name) {
		t.Fatal("the tend inside the lookup did not settle the entry, so this proves nothing")
	}
	if nextMonitor.Measuring(h.name) {
		t.Error("a job whose entry settled while its counters were found was left measured")
	}
}

// A RECOVERY THE STOP CANCELLED AFTER ITS RENEWALS STILL MEASURES WHAT IT
// ADOPTED: it returns as recovered, so the drain that follows does not recover
// again, and skipping the measuring left a drained job settling with nothing
// measured. A handoff after it still leaves the next process the series.
func TestARecoveryTheStopCancelledStillMeasuresWhatItAdopted(t *testing.T) {
	h := newHandoverHost(t)

	first, firstMonitor := h.process(t)
	h.launch(t, first)
	firstMonitor.Tick()
	first.HandOver(t.Context())
	targets := h.p.targets.Load()

	next, nextMonitor := h.process(t)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	h.rec.afterRenewal = func(string, error) { stop() }
	// THE STOP COMES AFTER THE ONE SURVIVOR'S RENEWAL, so the adoption itself
	// is done.
	if err := next.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the stop never came, so this proves nothing")
	}
	if got := h.p.targets.Load(); got == targets || !nextMonitor.Measuring(h.name) {
		t.Fatalf("a recovery the stop cancelled looked for counters %d times and measuring = %v, want it "+
			"measured", got-targets, nextMonitor.Measuring(h.name))
	}

	// AND A HANDOFF STILL LEAVES THE NEXT PROCESS WHAT BOTH SAMPLED.
	checkpoint := filepath.Join(h.dir, h.name+checkpointSuffix)
	if _, err := os.Stat(checkpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the checkpoint outlived the adoption that read it: %v", err)
	}
	next.HandOver(t.Context())
	data, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatalf("the handoff after a cancelled recovery kept no checkpoint: %v", err)
	}
	cp, err := usage.ReadCheckpoint(data)
	if err != nil {
		t.Fatalf("the handoff's checkpoint does not read back: %v", err)
	}
	tl, err := usage.DecodeSeriesGaps(cp.Series)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(causes(tl), usage.GapHandover) && cp.Unseen != usage.GapHandover {
		t.Errorf("marks %v, unseen %v; want the first handover carried into the next", causes(tl), cp.Unseen)
	}
}

// A FORCE RELEASE A TEND SETTLED FIRST IS DONE WITH: the adoption's renewal
// finds the lease force-released, and before the adoption acts on it, a tend
// has settled the entry and released the lease. The adoption leaves the entry
// to that tend, releasing the lease once, and the recovery goes on to the next
// survivor.
func TestAForceReleaseATendSettledFirstIsNotSettledAgain(t *testing.T) {
	h := newHandoverHost(t)
	h.p.asyncTeardown = true
	second := assignedLease(t, h.a)

	first, _ := h.process(t)
	h.launch(t, first)
	if err := first.Launch(t.Context(), second, dockerSpec(),
		Job{RequestID: second.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// THE FORCED ONE IS THE FIRST SURVIVOR RECOVERY READS, in the backend's
	// order, so the other is reached only if the recovery goes on.
	forced, other := h.lease, second
	if provider.InstanceName(second.ID) < h.name {
		forced, other = second, h.lease
	}
	// AN OPERATOR FORCES THE RELEASE OF A TEARDOWN THE BACKEND NEVER CONFIRMED.
	if err := first.Destroy(t.Context(), forced.RequestID); !errors.Is(err, dispatch.ErrCustody) {
		t.Fatalf("Destroy = %v, want the unconfirmed teardown held in custody", err)
	}
	if err := first.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}
	if _, err := h.a.ForceRelease(t.Context(), forced.ID); err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}
	next, nextMonitor := h.process(t)
	var tended atomic.Bool
	h.rec.afterRenewal = func(_ string, err error) {
		if errors.Is(err, alloc.ErrForceRelease) && tended.CompareAndSwap(false, true) {
			if err := next.Tend(t.Context()); err != nil {
				t.Errorf("Tend: %v", err)
			}
		}
	}
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover = %v, want a force release a tend settled to be done with", err)
	}

	if !tended.Load() {
		t.Fatal("no renewal found the force release, so this proves nothing")
	}
	releases := 0
	for _, e := range h.rec.sent() {
		if e == "release "+forced.ID {
			releases++
		}
	}
	if releases != 1 {
		t.Errorf("the forced lease was released %d times, want once: %v", releases, h.rec.sent())
	}
	if held := next.heldLeases(); len(held) != 1 || !held[other.ID] {
		t.Errorf("held %v after the recovery, want only the other survivor %s", held, other.ID)
	}
	forcedName, otherName := provider.InstanceName(forced.ID), provider.InstanceName(other.ID)
	if nextMonitor.Measuring(forcedName) || !nextMonitor.Measuring(otherName) {
		t.Error("want the forced job unmeasured and the other survivor measured")
	}
}

// notingJIT notes each runner removal among the ledger's events, so a test can
// order it against the renewals.
type notingJIT struct {
	*fakeJIT
	rec *recordingLedger
}

func (j *notingJIT) EnsureRunnerRemoved(_ context.Context, leaseID string) error {
	j.rec.note("remove " + leaseID)

	return nil
}

// RECOVERY RENEWS EVERY SURVIVOR BEFORE IT SETTLES A FORCE RELEASE: settling
// removes the runner at GitHub, for up to half a minute, and done inline it
// held the next survivor's first renewal behind it.
func TestRecoveryRenewsEverySurvivorBeforeItSettlesAForceRelease(t *testing.T) {
	h := newHandoverHost(t)
	h.p.asyncTeardown = true
	second := assignedLease(t, h.a)

	first, _ := h.process(t)
	h.launch(t, first)
	if err := first.Launch(t.Context(), second, dockerSpec(),
		Job{RequestID: second.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// THE FORCED ONE IS THE FIRST SURVIVOR RECOVERY READS.
	forced, other := h.lease, second
	if provider.InstanceName(second.ID) < h.name {
		forced, other = second, h.lease
	}
	if err := first.Destroy(t.Context(), forced.RequestID); !errors.Is(err, dispatch.ErrCustody) {
		t.Fatalf("Destroy = %v, want the unconfirmed teardown held in custody", err)
	}
	if err := first.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}
	if _, err := h.a.ForceRelease(t.Context(), forced.ID); err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}

	next := New(h.rec, h.host, &notingJIT{fakeJIT: &fakeJIT{setID: 7}, rec: h.rec}, h.p, nil,
		WithMonitor(runningMonitor(t, h.root)), WithUsageCheckpoints(h.dir))
	h.p.onTarget = func(id string) { h.rec.note("target " + id) }
	h.rec.mu.Lock()
	h.rec.events = nil
	h.rec.mu.Unlock()
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	events := h.rec.sent()
	removed, renewed := slices.Index(events, "remove "+forced.ID), slices.Index(events, "heartbeat "+other.ID)
	if removed < 0 || renewed < 0 || removed < renewed || !slices.Contains(events, "release "+forced.ID) {
		t.Errorf("events = %v, want the other survivor renewed before the forced one's runner is removed "+
			"and its lease released", events)
	}
	// AND MEASURED BEFORE IT, since the removal can wait on GitHub.
	if looked := slices.Index(events, "target instance-"+provider.InstanceName(other.ID)); looked < 0 ||
		looked > removed {
		t.Errorf("events = %v, want the other survivor's counters looked for before the removal", events)
	}
}

// A JOB A PROVIDER WARNS IT WILL RECLAIM REPORTS WHAT IT DID: the warning moves
// the running job into custody to be torn down, and that custody reports it
// once, before its lease is released, and stops measuring it.
func TestAJobAProviderReclaimsReportsItsUsage(t *testing.T) {
	h := newHandoverHost(t)

	r, m := h.process(t)
	h.launch(t, r)
	h.p.writeCPU(t, h.id, "3000000")
	m.Tick()
	// READ ONLY BY THE LAST SAMPLE, which the teardown takes before the compute
	// goes.
	h.p.writeCPU(t, h.id, "4000000")
	notice := &provider.InterruptionNotice{InstanceID: h.id, Action: "terminate"}
	if err := r.handleInterruption(t.Context(), notice); err != nil {
		t.Fatalf("handleInterruption: %v", err)
	}

	events := h.rec.sent()
	reported := slices.Index(events, fmt.Sprintf("usage %s@%d", h.lease.ID, h.lease.Epoch))
	released := slices.Index(events, "release "+h.lease.ID)
	if reported < 0 || released < 0 || reported > released || h.rec.usageReports() != 1 {
		t.Errorf("events = %v, want one usage report and it before the release", events)
	}
	got, err := h.a.LeaseUsage(t.Context(), h.lease.ID)
	if err != nil || got.CPUUserMicros != 4_000_000 || slices.Contains(got.Unmeasured, alloc.UsageCPU) {
		t.Errorf("usage = %+v, %v; want the 4000000 the last sample read, measured", got, err)
	}
	if m.Measuring(h.name) {
		t.Error("the sampler still measures a job whose compute is gone")
	}
}

// A JOB IS REPORTED ONCE, NOT ONCE PER ADOPTION, AND ITS OWN PROCESS DOES NOT
// LOOK FOR ITS COUNTERS AGAIN. An adopted job whose teardown the backend has not
// confirmed has reported and stays in custody; a re-registration adopts it
// again, and neither that nor the next tend reports it or measures it again.
func TestAJobIsReportedOnceAcrossReadoptions(t *testing.T) {
	h := newHandoverHost(t)
	h.p.asyncTeardown = true

	first, _ := h.process(t)
	h.launch(t, first)
	next, _ := h.process(t)
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	targets := h.p.targets.Load()
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover again: %v", err)
	}
	if got := h.p.targets.Load(); got != targets {
		t.Errorf("re-adopting a job this process measures looked for its counters again (%d lookups, was %d)",
			got, targets)
	}

	if err := next.Destroy(t.Context(), h.lease.RequestID); !errors.Is(err, dispatch.ErrCustody) {
		t.Fatalf("Destroy = %v, want the unconfirmed teardown held in custody", err)
	}
	if n := h.rec.usageReports(); n != 1 {
		t.Fatalf("%d usage reports after the teardown was asked, want 1", n)
	}

	targets = h.p.targets.Load()
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover after the report: %v", err)
	}
	if err := next.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}
	if n := h.rec.usageReports(); n != 1 {
		t.Errorf("%d usage reports after a re-adoption and another tend, want still 1", n)
	}
	if got := h.p.targets.Load(); got != targets {
		t.Errorf("a re-adoption resumed measuring a job already reported (%d lookups, was %d)", got, targets)
	}
}

// A LOOKUP THAT FAILED IS TRIED AGAIN, AND THE CHECKPOINT WAITS FOR IT: the
// entry still owes its report, so the next recovery looks for the counters
// again and continues the series from what the handover kept.
func TestAFailedLookupLeavesTheCheckpointForTheNextRecovery(t *testing.T) {
	h := newHandoverHost(t)

	first, firstMonitor := h.process(t)
	h.launch(t, first)
	firstMonitor.Tick()
	first.HandOver(t.Context())

	next, nextMonitor := h.process(t)
	h.p.targetErr = errors.New("the jail cannot be read yet")
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dir, h.name+checkpointSuffix)); err != nil {
		t.Fatalf("a failed lookup consumed the checkpoint: %v", err)
	}
	if nextMonitor.Measuring(h.name) {
		t.Fatal("a job whose counters were not found is measured")
	}

	h.p.targetErr = nil
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover again: %v", err)
	}
	if !nextMonitor.Measuring(h.name) {
		t.Fatal("the next recovery did not measure the job")
	}
	if err := next.Destroy(t.Context(), h.lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if marks := causes(h.timeline(t)); !slices.Contains(marks, usage.GapHandover) {
		t.Errorf("marks = %v, want the series continued from the checkpoint", marks)
	}
}

// A SUPERSEDED PROCESS REPORTS NOTHING: another process owns the name now, and
// the adopted job it measured is let go unreported.
func TestASupersededProcessReportsNoAdoptedJob(t *testing.T) {
	h := newHandoverHost(t)

	first, _ := h.process(t)
	h.launch(t, first)
	next, nextMonitor := h.process(t)
	if err := next.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	next.Superseded()
	if _, ok := nextMonitor.Final(h.name); ok {
		t.Error("a superseded process still measures the job it adopted")
	}
	if err := next.Destroy(t.Context(), h.lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if n := h.rec.usageReports(); n != 0 {
		t.Errorf("a superseded process sent %d usage reports, want none", n)
	}
}

// RECOVERY REMOVES THE CHECKPOINTS OF COMPUTE THE BACKEND NO LONGER LISTS, and
// leaves every other file alone.
func TestRecoveryRemovesTheCheckpointsOfComputeThatIsGone(t *testing.T) {
	h := newHandoverHost(t)
	r, _ := h.process(t)

	gone := filepath.Join(h.dir, provider.InstanceName("a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5")+checkpointSuffix)
	other := filepath.Join(h.dir, "notes.txt")
	for _, path := range []string{gone, other} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the checkpoint of compute that is gone was kept: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("recovery removed a file that is no checkpoint: %v", err)
	}
}
