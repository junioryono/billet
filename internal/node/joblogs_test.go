package node

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/joblog"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
)

const logLease = "0123456789abcdef0123456789abcdef"

func logLimits() JobLogLimits {
	return JobLogLimits{Retention: time.Hour, MaxBytes: 1 << 20, MaxJobBytes: 1 << 16}
}

func aLogStore(t *testing.T, limits JobLogLimits) *JobLogStore {
	t.Helper()

	s, err := NewJobLogStore(t.TempDir(), limits, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewJobLogStore: %v", err)
	}

	return s
}

// aChunkOf is an encoded chunk of the given lines.
func aChunkOf(t *testing.T, seq uint64, step int64, lines ...string) (*joblog.Chunk, []byte) {
	t.Helper()

	c := &joblog.Chunk{Seq: seq, Step: step, StepName: "Test"}
	for _, text := range lines {
		c.Lines = append(c.Lines, joblog.Line{AtMillis: 1_760_000_000_000, Text: text})
	}

	data, err := joblog.Encode(*c)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	return c, data
}

func keepChunk(t *testing.T, s *JobLogStore, lease string, seq uint64, lines ...string) {
	t.Helper()

	c, data := aChunkOf(t, seq, 0, lines...)
	if err := s.Keep(lease, c, data); err != nil {
		t.Fatalf("Keep: %v", err)
	}
}

// A CHUNK'S LINES ARE NUMBERED FROM WHERE THE LAST ENDED, a number seen before is a retry and
// stored once, and reading back gives each line its number.
func TestAKeptChunkReadsBackByLine(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())
	keepChunk(t, s, logLease, 1, "zero", "one")
	keepChunk(t, s, logLease, 1, "zero", "one")
	keepChunk(t, s, logLease, 2, "two")

	got, err := s.ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 1, Last: 2}}, 1<<10)
	if err != nil {
		t.Fatalf("ReadLines: %v", err)
	}
	if !got.Held || len(got.Lines) != 2 || got.Lines[0].Line != 1 || got.Lines[0].Text != "one" ||
		got.Lines[1].Line != 2 || got.Lines[1].Text != "two" {
		t.Errorf("read back %+v, want lines 1 and 2", got)
	}
}

// A LEASE THE NODE HOLDS NOTHING OF IS NOT HELD, which a search must not read as no such line;
// and a name that is no lease never becomes a path.
func TestALeaseNotHeldSaysSo(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())

	got, err := s.ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 0, Last: 1}}, 1<<10)
	if err != nil || got.Held {
		t.Errorf("a lease never kept reads %+v, %v; want not held", got, err)
	}

	for _, lease := range []string{"", "../../etc", strings.Repeat("x", 32)} {
		if _, err := s.ReadLines(t.Context(), lease, nil, 1); err == nil {
			t.Errorf("a read of %q was not refused", lease)
		}

		c, data := aChunkOf(t, 1, 0, "line")
		if err := s.Keep(lease, c, data); err == nil {
			t.Errorf("a chunk for %q was kept", lease)
		}
	}
}

// A READ STOPS AT ITS BOUNDS AND SAYS SO, so an answer always fits the wire and a line left
// out is never read as a line absent.
func TestAReadStopsAtItsBounds(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())
	keepChunk(t, s, logLease, 1, "aaaa", "bbbb", "cccc")

	got, err := s.ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 0, Last: 9}}, 6)
	if err != nil || !got.Cut || len(got.Lines) != 1 {
		t.Errorf("past the bytes: %+v, %v; want one line and cut", got, err)
	}

	lines := make([]string, nodeapi.MaxLogReadLines+1)
	for i := range lines {
		lines[i] = "x"
	}
	big := aLogStore(t, logLimits())
	keepChunk(t, big, logLease, 1, lines...)

	got, err = big.ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 0, Last: int64(len(lines))}}, 1<<20)
	if err != nil || !got.Cut || len(got.Lines) != nodeapi.MaxLogReadLines {
		t.Errorf("past the lines: %d lines, cut %t, %v", len(got.Lines), got.Cut, err)
	}
}

// A JOB PAST ITS BUDGET IS REFUSED, BY THE SENTINEL, and what it kept stands.
func TestAJobPastItsBudgetIsRefused(t *testing.T) {
	t.Parallel()

	_, first := aChunkOf(t, 1, 0, "line")
	limits := logLimits()
	limits.MaxJobBytes = int64(len(first)) + 1
	s := aLogStore(t, limits)

	keepChunk(t, s, logLease, 1, "line")

	c, data := aChunkOf(t, 2, 0, "another")
	if err := s.Keep(logLease, c, data); !errors.Is(err, ErrJobLogBudget) {
		t.Errorf("past the budget: %v", err)
	}

	got, err := s.ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 0, Last: 1}}, 1<<10)
	if err != nil || len(got.Lines) != 1 || got.Lines[0].Text != "line" {
		t.Errorf("what it kept reads as %+v, %v", got, err)
	}
}

// A RESTART KEEPS WHAT WAS KEPT: the files stay, line numbers go on from them, and a number
// seen before the restart is still a repeat.
func TestARestartKeepsTheNumbering(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := NewJobLogStore(dir, logLimits(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	keepChunk(t, s, logLease, 1, "zero", "one")

	again, err := NewJobLogStore(dir, logLimits(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	keepChunk(t, again, logLease, 1, "zero", "one")
	keepChunk(t, again, logLease, 2, "two")

	got, err := again.ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 2, Last: 2}}, 1<<10)
	if err != nil || len(got.Lines) != 1 || got.Lines[0].Text != "two" {
		t.Errorf("line 2 reads %+v, %v", got, err)
	}
	if again.total == 0 || again.leases[logLease].bytes == 0 {
		t.Error("the reload counted nothing of what it found")
	}
}

// fakeSink is the control plane's index.
type fakeSink struct {
	mu      sync.Mutex
	id      string
	refused bool
	err     error
	shipped []int64
}

func (f *fakeSink) LogIndexID() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.id, f.id != ""
}

func (f *fakeSink) ShipLogChunk(_ context.Context, _ string, first int64, _ []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return f.refused, f.err
	}

	f.shipped = append(f.shipped, first)

	return false, nil
}

func (f *fakeSink) got() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int64(nil), f.shipped...)
}

func sameLines(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// EACH CHUNK IS SHIPPED ONCE, AND AGAIN TO AN INDEX THAT HAS NOT HAD IT, which is how a
// promoted controller comes to hold what its nodes hold. A control plane with no index is
// shipped nothing.
func TestAChunkIsShippedOnceToEachIndex(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())
	keepChunk(t, s, logLease, 1, "zero", "one")
	keepChunk(t, s, logLease, 2, "two")

	sink := &fakeSink{}
	s.shipPass(t.Context(), sink)
	if len(sink.got()) != 0 {
		t.Fatalf("shipped %v to a control plane holding no index", sink.got())
	}

	sink.id = "index-a"
	s.shipPass(t.Context(), sink)
	s.shipPass(t.Context(), sink)
	if !sameLines(sink.got(), []int64{0, 2}) {
		t.Fatalf("shipped %v, want each chunk once", sink.got())
	}

	keepChunk(t, s, logLease, 3, "three")
	s.shipPass(t.Context(), sink)
	if !sameLines(sink.got(), []int64{0, 2, 3}) {
		t.Fatalf("shipped %v, want only the new chunk added", sink.got())
	}

	sink.id = "index-b"
	s.shipPass(t.Context(), sink)
	if !sameLines(sink.got(), []int64{0, 2, 3, 0, 2, 3}) {
		t.Errorf("shipped %v, want everything again to a new index", sink.got())
	}
}

// COULD-NOT-TELL IS TRIED AGAIN AND A REFUSAL IS NOT: the first must not lose a chunk, and the
// second must not wedge every chunk after it.
func TestAShipThatFailsIsTriedAgainAndARefusalIsNot(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())
	keepChunk(t, s, logLease, 1, "zero")

	sink := &fakeSink{id: "index", err: errors.New("the plane is away")}
	s.shipPass(t.Context(), sink)

	sink.mu.Lock()
	sink.err = nil
	sink.mu.Unlock()
	s.shipPass(t.Context(), sink)
	if !sameLines(sink.got(), []int64{0}) {
		t.Fatalf("after could-not-tell it shipped %v, want the chunk again", sink.got())
	}

	refusing := aLogStore(t, logLimits())
	keepChunk(t, refusing, logLease, 1, "zero")
	keepChunk(t, refusing, logLease, 2, "one")

	sink = &fakeSink{id: "index", refused: true, err: errors.New("refused")}
	refusing.shipPass(t.Context(), sink)

	sink.mu.Lock()
	sink.err, sink.refused = nil, false
	sink.mu.Unlock()
	refusing.shipPass(t.Context(), sink)
	if len(sink.got()) != 0 {
		t.Errorf("after refusals it shipped %v, want neither sent again", sink.got())
	}
}

// A JOB'S LOG GOES WHOLE, THE OLDEST FIRST, past its age or the node's bound, and nothing else
// does.
func TestEvictionTakesWholeJobsOldestFirst(t *testing.T) {
	t.Parallel()

	const other = "ffffffffffffffffffffffffffffffff"

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	s := aLogStore(t, logLimits())
	s.now = func() time.Time { return now }

	keepChunk(t, s, logLease, 1, "old")
	now = now.Add(30 * time.Minute)
	keepChunk(t, s, other, 1, "new")

	s.evictPass()
	if _, err := os.Stat(filepath.Join(s.dir, logLease)); err != nil {
		t.Fatalf("inside its age and bound the log went: %v", err)
	}

	now = now.Add(45 * time.Minute)
	s.evictPass()
	if got := chunksHeld(t, s, logLease); got != 0 {
		t.Errorf("past its age %d chunks stayed", got)
	}
	if got := chunksHeld(t, s, other); got != 1 {
		t.Errorf("inside its age %d chunks stayed", got)
	}

	// A BOUND LOWERED UNDER WHAT IS HELD, as a restart with a smaller one finds it.
	tight := aLogStore(t, logLimits())
	tight.now = s.now
	keepChunk(t, tight, logLease, 1, "a")
	now = now.Add(time.Minute)
	keepChunk(t, tight, other, 1, "b")
	tight.limits.MaxBytes = tight.total - 1
	tight.evictPass()
	if !tight.evicted(logLease) || tight.evicted(other) {
		t.Errorf("past the bound: the older evicted %t, the newer %t; want only the older, which is enough",
			tight.evicted(logLease), tight.evicted(other))
	}
}

// chunksHeld is how many chunk files a lease has on disk.
func chunksHeld(t *testing.T, s *JobLogStore, lease string) int {
	t.Helper()

	chunks, err := s.chunksOf(lease)
	if err != nil {
		t.Fatalf("chunksOf: %v", err)
	}

	return len(chunks)
}

// AN EVICTED LEASE TAKES NO MORE, across a restart too: its numbers are in the index, and a
// chunk numbered afresh would make a search read back a different line than it matched. A read
// says it is not held, never that it has no such line.
func TestAnEvictedLeaseIsNotNumberedAfresh(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	s, err := NewJobLogStore(dir, logLimits(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }

	keepChunk(t, s, logLease, 1, "matched")

	now = now.Add(2 * time.Hour)
	s.evictPass()

	c, data := aChunkOf(t, 2, 0, "new")
	if err := s.Keep(logLease, c, data); !errors.Is(err, ErrJobLogBudget) {
		t.Errorf("an evicted lease answered %v, want its budget spent", err)
	}

	got, err := s.ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 0, Last: 0}}, 1<<10)
	if err != nil || got.Held {
		t.Errorf("an evicted lease read as %+v, %v", got, err)
	}

	again, err := NewJobLogStore(dir, logLimits(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Keep(logLease, c, data); !errors.Is(err, ErrJobLogBudget) {
		t.Errorf("after a restart an evicted lease answered %v, want its budget spent", err)
	}

	// AND LONG ENOUGH LATER IT IS FORGOTTEN, so the directory does not grow for ever.
	s.evictPass()
	now = now.Add(jobLogEvictedKept + time.Hour)
	s.evictPass()
	if _, err := os.Stat(filepath.Join(dir, jobLogDirectory, logLease)); !os.IsNotExist(err) {
		t.Errorf("an evicted lease was never forgotten: %v", err)
	}
}

// THE NODE'S BOUND HOLDS AS A CHUNK ARRIVES, not at the next pass: the job written longest ago
// makes room, and one chunk that cannot fit is refused rather than kept past it.
func TestTheNodesBoundHoldsOnArrival(t *testing.T) {
	t.Parallel()

	const other = "ffffffffffffffffffffffffffffffff"

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	_, data := aChunkOf(t, 1, 0, "one")
	limits := logLimits()
	limits.MaxBytes = int64(len(data)) + 1

	s := aLogStore(t, limits)
	s.now = func() time.Time { return now }

	keepChunk(t, s, logLease, 1, "one")
	now = now.Add(time.Minute)

	// THE DISK IS THE BOUND, so a chunk waits for what was evicted to be deleted, and is asked for
	// again rather than refused.
	first, firstData := aChunkOf(t, 1, 0, "one")
	if err := s.Keep(other, first, firstData); !errors.Is(err, ErrJobLogRoom) {
		t.Fatalf("a chunk needing room answered %v", err)
	}
	if !s.evicted(logLease) || s.evicted(other) {
		t.Errorf("the room was not made from the oldest: %t, %t", s.evicted(logLease), s.evicted(other))
	}

	s.evictPass()

	if err := s.Keep(other, first, firstData); err != nil {
		t.Fatalf("the chunk asked for again, with the room made: %v", err)
	}
	if s.total+s.sweeping > limits.MaxBytes {
		t.Errorf("%d bytes held past a bound of %d", s.total+s.sweeping, limits.MaxBytes)
	}

	c, large := aChunkOf(t, 2, 0, "a longer line than the bound leaves room for")
	if err := s.Keep(other, c, large); !errors.Is(err, ErrJobLogBudget) {
		t.Errorf("a chunk past the node's bound answered %v", err)
	}
}

// A CHUNK ALREADY PUBLISHED IS ACCOUNTED FOR, whatever the install said: a retry of it must not
// trip over its own name for ever, and no later chunk may be numbered over it.
func TestAChunkPublishedByAFailedInstallCounts(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())
	dir := filepath.Join(s.dir, logLease)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	c, data := aChunkOf(t, 1, 0, "one", "two")
	if err := os.WriteFile(filepath.Join(dir, chunkName(0, 1, 2)), data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Keep(logLease, c, data); err != nil {
		t.Fatalf("a retry of a chunk already in place answered %v", err)
	}

	next, nextData := aChunkOf(t, 2, 0, "three")
	if err := s.Keep(logLease, next, nextData); err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, chunkName(2, 2, 1))); err != nil {
		t.Errorf("the next chunk was not numbered past it: %v", err)
	}

	// OTHER BYTES UNDER ITS NAME ARE NOT THIS CHUNK, which is not kept.
	lying, lyingData := aChunkOf(t, 3, 0, "four")
	if err := os.WriteFile(filepath.Join(dir, chunkName(3, 3, 1)), []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Keep(logLease, lying, lyingData); err == nil {
		t.Error("a chunk whose name holds other bytes was taken for kept")
	}
}

// A READ STOPS WHEN ITS COMMAND DOES, and says so rather than answering what it reached.
func TestAReadStopsWithItsContext(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())
	keepChunk(t, s, logLease, 1, "one")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.ReadLines(ctx, logLease, []nodeapi.LogLineRange{{First: 0, Last: 0}}, 1<<10); !errors.Is(err, context.Canceled) {
		t.Errorf("a read whose context ended answered %v", err)
	}
}

// logSession is a cache service keeping logs and a session of a tier with keep, or without.
func logSession(t *testing.T, keep bool) (*CacheService, string) {
	t.Helper()

	service, _, _, _ := guestService(t)
	service.KeepGuestLogs(aLogStore(t, logLimits()))

	credentials, err := service.PrepareScoped(provider.InstanceName(logLease), CacheSessionScope{
		Trust: provider.TrustTrusted, LeaseID: logLease, Epoch: 1, Cache: &config.CacheSpec{KeepLogs: keep},
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service, credentials.Token
}

func postLog(t *testing.T, service *CacheService, token string, data []byte) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, guestLogsPath, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", guestLogType)

	response := httptest.NewRecorder()
	service.ServeHTTP(response, req)

	return response
}

// A TIER THAT DID NOT ASK KEEPS NOTHING: no offer in the hello, and the route is not there. A
// tier that asked is offered a home, and what it sends is kept for the bearer's own lease.
func TestALogIsKeptOnlyForATierThatAsked(t *testing.T) {
	t.Parallel()

	_, data := aChunkOf(t, 1, 0, "line")

	service, token := logSession(t, false)
	if got := guestHello(t, service, token).Body.String(); strings.Contains(got, `"logs"`) {
		t.Errorf("a tier that did not ask is offered a home: %s", got)
	}
	if got := postLog(t, service, token, data).Code; got != http.StatusNotFound {
		t.Errorf("a chunk from a tier that did not ask answered %d, want 404", got)
	}

	service, token = logSession(t, true)
	if got := guestHello(t, service, token).Body.String(); !strings.Contains(got, `"logs":{"interval_ms":10000`) {
		t.Errorf("a tier that asked is offered no home: %s", got)
	}
	if got := postLog(t, service, token, data).Code; got != http.StatusNoContent {
		t.Fatalf("a chunk answered %d, want 204", got)
	}

	got, err := service.JobLogs().ReadLines(t.Context(), logLease, []nodeapi.LogLineRange{{First: 0, Last: 0}}, 1<<10)
	if err != nil || len(got.Lines) != 1 || got.Lines[0].Text != "line" {
		t.Errorf("the chunk reads back %+v, %v", got, err)
	}
}

// WHAT IS NOT A CHUNK IS REFUSED AND NEVER QUOTED, a bearer that is no session's is refused,
// and a node that keeps no logs has no such route.
func TestTheLogRouteRefusesWhatItShould(t *testing.T) {
	t.Parallel()

	const secret = "hunter2"

	service, token := logSession(t, true)
	response := postLog(t, service, token, []byte(secret))
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), secret) {
		t.Errorf("junk answered %d: %s", response.Code, response.Body.String())
	}

	_, data := aChunkOf(t, 1, 0, "line")
	if got := postLog(t, service, "not-a-bearer", data).Code; got != http.StatusUnauthorized {
		t.Errorf("a stranger's chunk answered %d, want 401", got)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, guestLogsPath, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/plain")

	response = httptest.NewRecorder()
	service.ServeHTTP(response, req)
	if response.Code != http.StatusBadRequest {
		t.Errorf("another content type answered %d, want 400", response.Code)
	}

	off, _, offToken, _ := guestService(t)
	if got := postLog(t, off, offToken, data).Code; got != http.StatusNotFound {
		t.Errorf("a node keeping no logs answered %d, want 404", got)
	}
}

// A FLOOD COSTS A HEADER: past the bucket a chunk is told to wait, unread, which a body that could
// not be decoded proves: reading it would have answered 400.
func TestAFloodOfChunksWaits(t *testing.T) {
	t.Parallel()

	service, token := logSession(t, true)

	limited := false
	for range jobLogBurst + 1 {
		if postLog(t, service, token, []byte("not a chunk")).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("a flood past the bucket was not told to wait")
	}
}

// A JOB WHOSE BUDGET IS SPENT COSTS NO BODY EITHER: the same undecodable body is told so, not 400.
func TestASpentJobIsToldBeforeItsBodyIsRead(t *testing.T) {
	t.Parallel()

	service, token := logSession(t, true)
	service.JobLogs().limits.MaxJobBytes = 1

	_, data := aChunkOf(t, 1, 0, "line")
	service.JobLogs().mu.Lock()
	service.JobLogs().leases[logLease] = &leaseLog{seqs: map[uint64]bool{}, bytes: int64(len(data)), newest: time.Now()}
	service.JobLogs().mu.Unlock()

	if got := postLog(t, service, token, []byte("not a chunk")).Code; got != http.StatusRequestEntityTooLarge {
		t.Errorf("a spent job's chunk answered %d", got)
	}
}

// A LOG'S SLOTS ARE ITS OWN: with every report slot taken a chunk goes on, and the reverse is what
// keeps a burst of log from turning away a job's final report.
func TestLogChunksDoNotTakeAReportsSlot(t *testing.T) {
	t.Parallel()

	service, token := logSession(t, true)

	for range cap(service.guestDecode) {
		service.guestDecode <- struct{}{}
	}

	_, data := aChunkOf(t, 1, 0, "line")
	if got := postLog(t, service, token, data).Code; got != http.StatusNoContent {
		t.Errorf("with every report slot taken a chunk answered %d", got)
	}

	for range cap(service.logDecode) {
		service.logDecode <- struct{}{}
	}

	_, again := aChunkOf(t, 2, 0, "line")
	if got := postLog(t, service, token, again).Code; got != http.StatusTooManyRequests {
		t.Errorf("with every log slot taken a chunk answered %d", got)
	}
}

// A FILE THAT CANNOT BE READ STOPS ITS OWN LEASE AND NO OTHER: oldest first, it would otherwise hold
// back every newer job's log for as long as it is kept.
func TestAnUnreadableLogStopsOnlyItsLease(t *testing.T) {
	t.Parallel()

	const other = "ffffffffffffffffffffffffffffffff"

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	s := aLogStore(t, logLimits())
	s.now = func() time.Time { return now }

	keepChunk(t, s, logLease, 1, "old")
	now = now.Add(time.Minute)
	keepChunk(t, s, other, 1, "new")

	chunks, err := s.chunksOf(logLease)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("chunksOf: %v, %v", chunks, err)
	}

	// A directory under a chunk's name is what no read of a regular file gets through.
	path := filepath.Join(s.dir, logLease, chunks[0].name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	sink := &fakeSink{id: "index-1"}
	s.shipPass(t.Context(), sink)

	if got := sink.got(); !sameLines(got, []int64{0}) {
		t.Errorf("shipped %v, want the newer job's one chunk", got)
	}
	if !s.shippedAll(other, "index-1") {
		t.Error("the newer job's log is not marked as shipped")
	}
}

// A PASS WITH NOTHING NEW READS NO DIRECTORY, so what a pass costs is what is new, not what is
// retained. A file under the directory's name is what would show a read, by the warning its failure
// logs; the second half proves that warning is there to be seen.
func TestAPassReadsOnlyWhatIsNew(t *testing.T) {
	t.Parallel()

	s := aLogStore(t, logLimits())

	var logged syncedBuffer

	s.log = slog.New(slog.NewTextHandler(&logged, nil))

	keepChunk(t, s, logLease, 1, "one")

	sink := &fakeSink{id: "index-1"}
	s.shipPass(t.Context(), sink)

	if !s.shippedAll(logLease, "index-1") {
		t.Fatal("a lease whose every chunk shipped does not say so")
	}
	if s.shippedAll(logLease, "index-2") {
		t.Error("shipped to one index reads as shipped to another")
	}

	dir := filepath.Join(s.dir, logLease)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	s.shipPass(t.Context(), sink)

	if got := logged.String(); strings.Contains(got, "could not read") {
		t.Errorf("a pass with nothing new read the directory: %s", got)
	}

	s.shipPass(t.Context(), &fakeSink{id: "index-2"})

	if got := logged.String(); !strings.Contains(got, "could not read") {
		t.Errorf("a pass with something to send did not read the directory, so this test proves nothing: %s", got)
	}
}

// syncedBuffer is a log the test reads.
type syncedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// WHAT COULD NOT BE DELETED STILL TAKES THE DISK, so the bound goes on counting it, across a restart
// too, and a restart does not start an evicted lease's time again.
func TestAnEvictionThatFailedStillCounts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	s, err := NewJobLogStore(dir, logLimits(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }

	keepChunk(t, s, logLease, 1, "one")
	held := s.total

	// MARKED AND NOT YET SWEPT: it takes what it took, and what ends it is the deletion.
	s.mu.Lock()
	s.markEvicted(s.leases[logLease])
	s.mu.Unlock()

	if s.total != 0 || s.sweeping != held {
		t.Errorf("marked: %d held and %d being deleted, want 0 and %d", s.total, s.sweeping, held)
	}

	// A MARK ON DISK WITH ITS CHUNKS STILL THERE is what a process stopped in the sweep leaves.
	mark := filepath.Join(dir, jobLogDirectory, logLease, jobLogEvicted)
	if err := os.WriteFile(mark, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	old := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(mark, old, old); err != nil {
		t.Fatal(err)
	}

	again, err := NewJobLogStore(dir, logLimits(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	if again.total != 0 || again.sweeping != held {
		t.Errorf("after a restart: %d held and %d being deleted, want 0 and %d", again.total, again.sweeping, held)
	}

	again.evictPass()

	if again.sweeping != 0 {
		t.Errorf("after the sweep %d is still being deleted", again.sweeping)
	}

	if chunks, err := again.chunksOf(logLease); err != nil || len(chunks) != 0 {
		t.Errorf("after the sweep: %v, %v", chunks, err)
	}

	info, err := os.Stat(mark)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("the sweep moved the mark's time to %s", info.ModTime())
	}
}

// THE ROUTE ASKS FOR A CHUNK AGAIN while room is being made, and says so as it does a wait, which
// the agent answers by sending the same chunk.
func TestTheRouteWaitsWhileRoomIsMade(t *testing.T) {
	t.Parallel()

	service, token := logSession(t, true)

	_, data := aChunkOf(t, 1, 0, "line")
	store := service.JobLogs()

	store.mu.Lock()
	store.limits.MaxBytes = int64(len(data))
	store.sweeping = 1
	store.mu.Unlock()

	if got := postLog(t, service, token, data).Code; got != http.StatusTooManyRequests {
		t.Errorf("a chunk needing room answered %d", got)
	}
}

// A LEASE THAT CANNOT BE READ DOES NOT STOP THE NODE: losing it loses no decision. It takes no more,
// since nobody knows what numbers it holds, and another goes on.
func TestAnUnreadableLeaseDoesNotStopTheStore(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, jobLogDirectory, logLease)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) })

	s, err := NewJobLogStore(dir, logLimits(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("one unreadable lease stopped the store: %v", err)
	}

	c, data := aChunkOf(t, 1, 0, "line")
	if err := s.Keep(logLease, c, data); !errors.Is(err, ErrJobLogBudget) {
		t.Errorf("an unreadable lease answered %v", err)
	}
	if err := s.Keep("ffffffffffffffffffffffffffffffff", c, data); err != nil {
		t.Errorf("another lease answered %v", err)
	}
}

// A COMMAND TO READ ASKS THE STORE, AND A NODE WITH NONE ANSWERS IT HOLDS NOTHING: the read is
// never an error a search would read as could-not-tell for good.
func TestAReadCommandAsksTheStore(t *testing.T) {
	t.Parallel()

	service, token := logSession(t, true)
	_, data := aChunkOf(t, 1, 0, "line")
	if got := postLog(t, service, token, data).Code; got != http.StatusNoContent {
		t.Fatalf("a chunk answered %d", got)
	}

	r := &Runner{cache: service}
	got, err := r.ReadJobLog(t.Context(), nodeapi.LogReadSpec{Lease: logLease,
		Ranges: []nodeapi.LogLineRange{{First: 0, Last: 0}}, MaxBytes: 1 << 10})
	if err != nil || !got.Held || len(got.Lines) != 1 {
		t.Errorf("the read answered %+v, %v", got, err)
	}

	got, err = (&Runner{}).ReadJobLog(t.Context(), nodeapi.LogReadSpec{Lease: logLease})
	if err != nil || got.Held {
		t.Errorf("a node keeping none answered %+v, %v", got, err)
	}
}
