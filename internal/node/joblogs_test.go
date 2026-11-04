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

	got, err := s.ReadLines(logLease, []nodeapi.LogLineRange{{First: 1, Last: 2}}, 1<<10)
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

	got, err := s.ReadLines(logLease, []nodeapi.LogLineRange{{First: 0, Last: 1}}, 1<<10)
	if err != nil || got.Held {
		t.Errorf("a lease never kept reads %+v, %v; want not held", got, err)
	}

	for _, lease := range []string{"", "../../etc", strings.Repeat("x", 32)} {
		if _, err := s.ReadLines(lease, nil, 1); err == nil {
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

	got, err := s.ReadLines(logLease, []nodeapi.LogLineRange{{First: 0, Last: 9}}, 6)
	if err != nil || !got.Cut || len(got.Lines) != 1 {
		t.Errorf("past the bytes: %+v, %v; want one line and cut", got, err)
	}

	lines := make([]string, nodeapi.MaxLogReadLines+1)
	for i := range lines {
		lines[i] = "x"
	}
	big := aLogStore(t, logLimits())
	keepChunk(t, big, logLease, 1, lines...)

	got, err = big.ReadLines(logLease, []nodeapi.LogLineRange{{First: 0, Last: int64(len(lines))}}, 1<<20)
	if err != nil || !got.Cut || len(got.Lines) != nodeapi.MaxLogReadLines {
		t.Errorf("past the lines: %d lines, cut %t, %v", len(got.Lines), got.Cut, err)
	}
}

// A JOB PAST ITS BUDGET IS REFUSED, BY THE SENTINEL, and what it kept stands.
func TestAJobPastItsBudgetIsRefused(t *testing.T) {
	t.Parallel()

	limits := logLimits()
	limits.MaxJobBytes = 1
	s := aLogStore(t, limits)

	c, data := aChunkOf(t, 1, 0, "line")
	if err := s.Keep(logLease, c, data); !errors.Is(err, ErrJobLogBudget) {
		t.Errorf("past the budget: %v", err)
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

	got, err := again.ReadLines(logLease, []nodeapi.LogLineRange{{First: 2, Last: 2}}, 1<<10)
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
	if _, err := os.Stat(filepath.Join(s.dir, logLease)); !os.IsNotExist(err) {
		t.Errorf("past its age the log stayed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, other)); err != nil {
		t.Errorf("a log inside its age went: %v", err)
	}

	limits := logLimits()
	limits.MaxBytes = 1
	tight := aLogStore(t, limits)
	tight.now = s.now
	keepChunk(t, tight, logLease, 1, "a")
	keepChunk(t, tight, other, 1, "b")
	tight.evictPass()
	if tight.total != 0 {
		t.Errorf("past the bound %d bytes stayed", tight.total)
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

	got, err := service.JobLogs().ReadLines(logLease, []nodeapi.LogLineRange{{First: 0, Last: 0}}, 1<<10)
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

// A FLOOD COSTS A HEADER: past the bucket a chunk is told to wait, unread.
func TestAFloodOfChunksWaits(t *testing.T) {
	t.Parallel()

	service, token := logSession(t, true)

	limited := false
	for i := range jobLogBurst + 1 {
		_, data := aChunkOf(t, uint64(i+1), 0, "line")
		if postLog(t, service, token, data).Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("a flood past the bucket was not told to wait")
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
