package node

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/guestreport"
	"github.com/junioryono/billet/internal/provider"
)

// guestClock is a node clock a test moves by hand.
type guestClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *guestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.at
}

func (c *guestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.at = c.at.Add(d)
}

// guestService is a cache service that takes guest reports, on a clock the test
// moves, with one session for billet-one; its log is kept for a test to read.
func guestService(t *testing.T) (*CacheService, *guestClock, string, *bytes.Buffer) {
	t.Helper()

	var logged bytes.Buffer

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	service.MonitorGuests()

	clock := &guestClock{at: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	service.now = clock.now

	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	return service, clock, credentials.Token, &logged
}

// guestBatch is the seq-th batch an agent would send: one sample, ten seconds after
// the last, and a step named step when it is not empty.
func guestBatch(t *testing.T, seq uint64, step string) []byte {
	t.Helper()

	at := int64(1_760_086_800_000) + int64(seq)*10_000
	b := guestreport.Batch{
		Seq: seq, AgentVersion: "v0.15.0", TicksPerSecond: 100, AgentCPUTicks: int64(seq),
		Samples: []guestreport.Sample{{
			AtMillis: at, CPUBusyTicks: int64(seq) * 10, CPUTotalTicks: int64(seq) * 100,
			MemUsedBytes: 1 << 30, MemAvailableBytes: 3 << 30,
		}},
	}
	if step != "" {
		b.Steps = []guestreport.StepMark{{Name: step, AtSeconds: at / 1000}}
	}

	data, err := guestreport.EncodeBatch(b)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}

	return data
}

// bulkyBatch is a batch of failed test names nothing compresses, between 40 KiB
// and the bound encoded, so a few dozen spend a session's budget.
func bulkyBatch(t *testing.T, seq uint64, rng *rand.Rand) []byte {
	t.Helper()

	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := guestreport.Batch{Seq: seq, AgentVersion: "v0.15.0", TicksPerSecond: 100}
	for s := range 8 {
		suite := guestreport.SuiteResult{Name: fmt.Sprintf("suite-%d", s), Tests: 50, Failures: 50}
		for range 50 {
			name := make([]byte, guestreport.MaxFailedNameBytes)
			for i := range name {
				name[i] = letters[rng.IntN(len(letters))]
			}
			suite.Failed = append(suite.Failed, string(name))
		}
		b.Tests = append(b.Tests, suite)
	}

	data, err := guestreport.EncodeBatch(b)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}
	if len(data) < 40<<10 || len(data) > guestreport.MaxBatchBytes {
		t.Fatalf("a bulky batch encodes to %d bytes, outside 40 KiB to %d", len(data), guestreport.MaxBatchBytes)
	}

	return data
}

// countingBody is a request body that records whether anything read it.
type countingBody struct {
	r    io.Reader
	read bool
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.read = true

	return c.r.Read(p)
}

func (c *countingBody) Close() error { return nil }

// guestPost sends body as a batch with token, with the query given.
func guestPost(t *testing.T, service *CacheService, token, query string, body io.Reader,
	length int64,
) *httptest.ResponseRecorder {
	t.Helper()

	target := guestReportsPath
	if query != "" {
		target += "?" + query
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, body)
	req.ContentLength = length
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", guestBatchType)

	response := httptest.NewRecorder()
	service.ServeHTTP(response, req)

	return response
}

func postBatch(t *testing.T, service *CacheService, token, query string, data []byte) *httptest.ResponseRecorder {
	t.Helper()

	return guestPost(t, service, token, query, bytes.NewReader(data), int64(len(data)))
}

// guestHello asks for the limits with token, or with no bearer when it is empty.
func guestHello(t *testing.T, service *CacheService, token string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, guestLimitsPath, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	response := httptest.NewRecorder()
	service.ServeHTTP(response, req)

	return response
}

func takeGuest(t *testing.T, service *CacheService, instance string) alloc.GuestReport {
	t.Helper()

	report, ok := service.TakeGuestReport(t.Context(), instance)
	if !ok {
		t.Fatalf("no guest report was taken for %s", instance)
	}
	if err := report.Validate(); err != nil {
		t.Fatalf("the report taken for %s is one the ledger refuses: %v", instance, err)
	}

	return report
}

// A NODE THAT TAKES NO GUEST REPORTS ANSWERS 404 ON BOTH ROUTES, whoever asks,
// which the agent reads as a node that takes none and exits; and it has no report
// to take.
func TestTheGuestRoutesAreNotFoundWithMonitoringOff(t *testing.T) {
	t.Parallel()

	service, _, token := testCacheService(t, provider.TrustTrusted)

	for name, response := range map[string]*httptest.ResponseRecorder{
		"limits":               guestHello(t, service, token),
		"limits with no token": guestHello(t, service, ""),
		"a batch":              postBatch(t, service, token, "", guestBatch(t, 1, "")),
		"a final batch":        postBatch(t, service, token, guestFinal, guestBatch(t, 1, "")),
	} {
		if response.Code != http.StatusNotFound {
			t.Errorf("%s answered %d with guest monitoring off, want 404", name, response.Code)
		}
	}

	if _, ok := service.TakeGuestReport(t.Context(), "billet-one"); ok {
		t.Error("a node that takes no guest reports took one")
	}
}

// THE LIMITS ARE THE CONTRACT'S, BYTE FOR BYTE, and asking for them is the hello.
// Only the session's bearer is answered.
func TestTheLimitsAreTheContractsAndAskingIsTheHello(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	if got := guestHello(t, service, "").Code; got != http.StatusUnauthorized {
		t.Errorf("limits with no bearer answered %d, want 401", got)
	}
	if got := guestHello(t, service, strings.Repeat("0", 64)).Code; got != http.StatusUnauthorized {
		t.Errorf("limits with an unknown bearer answered %d, want 401", got)
	}

	response := guestHello(t, service, token)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("limits answered %d (%s), want 200 application/json", response.Code,
			response.Header().Get("Content-Type"))
	}
	if got, want := strings.TrimSpace(response.Body.String()),
		`{"schemas":[1],"interval_ms":10000,"max_body":65536}`; got != want {
		t.Errorf("the limits are %s, want %s", got, want)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, guestLimitsPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	wrong := httptest.NewRecorder()
	service.ServeHTTP(wrong, req)
	if wrong.Code != http.StatusMethodNotAllowed {
		t.Errorf("a POST to the limits answered %d, want 405", wrong.Code)
	}

	if report := takeGuest(t, service, "billet-one"); !report.Hello || report.Accepted != 0 {
		t.Errorf("after asking for the limits the report says hello %t, accepted %d; want true, 0",
			report.Hello, report.Accepted)
	}
}

// A BATCH IS KEPT AND COMES BACK MERGED WHEN THE REPORT IS TAKEN, with when it
// arrived on the node's clock and whether the last one did; once taken, the guest
// is answered 401 and there is nothing to take again.
func TestAGuestBatchIsKeptAndTakenOnce(t *testing.T) {
	t.Parallel()

	service, clock, token, _ := guestService(t)
	first := clock.now()

	if got := postBatch(t, service, token, "", guestBatch(t, 1, "Build")).Code; got != http.StatusNoContent {
		t.Fatalf("a batch answered %d, want 204", got)
	}

	clock.advance(10 * time.Second)

	if got := postBatch(t, service, token, guestFinal, guestBatch(t, 2, "Test")).Code; got != http.StatusNoContent {
		t.Fatalf("the final batch answered %d, want 204", got)
	}

	report := takeGuest(t, service, "billet-one")
	if report.Accepted != 2 || report.Refused != 0 || !report.FinalSeen || report.Hello ||
		report.NodeRestarted {
		t.Errorf("the report counts accepted %d, refused %d, final %t, hello %t, restarted %t; "+
			"want 2, 0, true, false, false", report.Accepted, report.Refused, report.FinalSeen,
			report.Hello, report.NodeRestarted)
	}
	if !report.FirstReceived.Equal(first) || !report.LastReceived.Equal(first.Add(10*time.Second)) {
		t.Errorf("the batches arrived at %s and %s by the report, want %s and %s",
			report.FirstReceived, report.LastReceived, first, first.Add(10*time.Second))
	}
	if report.Codec != guestreport.Codec || report.Schema != 1 || report.AgentVersion != "v0.15.0" {
		t.Errorf("the report is codec %d, schema %d, agent %q; want %d, 1, v0.15.0",
			report.Codec, report.Schema, report.AgentVersion, guestreport.Codec)
	}

	merged, err := guestreport.Decode(report.Data)
	if err != nil {
		t.Fatalf("the report's data does not decode: %v", err)
	}
	if merged.Batches != 2 || len(merged.Steps) != 2 || merged.Steps[1].Name != "Test" {
		t.Errorf("the merged report holds %d batches and steps %v, want 2 and Build, Test",
			merged.Batches, merged.Steps)
	}

	if got := postBatch(t, service, token, "", guestBatch(t, 3, "")).Code; got != http.StatusUnauthorized {
		t.Errorf("a batch after the report was taken answered %d, want 401", got)
	}
	if got := guestHello(t, service, token).Code; got != http.StatusUnauthorized {
		t.Errorf("limits after the report was taken answered %d, want 401", got)
	}
	if _, ok := service.TakeGuestReport(t.Context(), "billet-one"); ok {
		t.Error("a report was taken twice")
	}
}

// THE GUEST'S ROUTES DO NOT WAIT BEHIND THE VOLUME API, whose one admission a
// Docker store's attach holds for minutes.
func TestTheGuestRoutesAreAnsweredWhileTheVolumeAPIIsBusy(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	session := service.sessionOf("billet-one")
	session.admit <- struct{}{}
	t.Cleanup(func() { <-session.admit })

	if got := guestHello(t, service, token).Code; got != http.StatusOK {
		t.Errorf("limits answered %d while a volume call was admitted, want 200", got)
	}
	if got := postBatch(t, service, token, "", guestBatch(t, 1, "")).Code; got != http.StatusNoContent {
		t.Errorf("a batch answered %d while a volume call was admitted, want 204", got)
	}
}

// A SESSION WHOSE COMPUTE HAS STOPPED TAKES NOTHING MORE, report taken or not: the
// guest is answered 401, which the agent reads as the job being over.
func TestAClosedSessionTakesNoMoreBatches(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	if err := service.Close(t.Context(), "billet-one"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data := guestBatch(t, 1, "")
	body := &countingBody{r: bytes.NewReader(data)}

	if got := guestPost(t, service, token, "", body, int64(len(data))).Code; got != http.StatusUnauthorized ||
		body.read {
		t.Errorf("a batch to a closed session answered %d, read %t; want 401 unread", got, body.read)
	}
	if got := guestHello(t, service, token).Code; got != http.StatusUnauthorized {
		t.Errorf("limits from a closed session answered %d, want 401", got)
	}
}

// takingBody is a batch whose body is still arriving when the destroy takes the
// report: its first read takes it.
type takingBody struct {
	r    io.Reader
	take func()
	once sync.Once
}

func (b *takingBody) Read(p []byte) (int, error) {
	b.once.Do(b.take)

	return b.r.Read(p)
}

// A BATCH STILL ARRIVING WHEN THE REPORT IS TAKEN IS NOT ACCEPTED: the report has
// gone, so a 204 would tell the agent the node kept what nobody will record.
func TestABatchThatArrivesAsTheReportIsTakenIsNotAccepted(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	var taken alloc.GuestReport

	data := guestBatch(t, 1, "")
	body := &takingBody{r: bytes.NewReader(data), take: func() {
		taken, _ = service.TakeGuestReport(t.Context(), "billet-one")
	}}

	if got := guestPost(t, service, token, "", body, int64(len(data))).Code; got != http.StatusUnauthorized {
		t.Errorf("a batch whose body arrived as the report was taken answered %d, want 401", got)
	}
	if taken.Codec != alloc.GuestReportCodec || taken.Accepted != 0 {
		t.Errorf("the report taken mid-batch is %+v, want one with nothing accepted", taken)
	}
}

// AND ONE STILL ARRIVING WHEN THE COMPUTE STOPS: the session closed under it, so
// it is answered 401, not kept for a report that may never be taken.
func TestABatchThatArrivesAsTheSessionClosesIsNotAccepted(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	data := guestBatch(t, 1, "")
	body := &takingBody{r: bytes.NewReader(data), take: func() {
		if err := service.Close(context.WithoutCancel(t.Context()), "billet-one"); err != nil {
			t.Errorf("Close: %v", err)
		}
	}}

	if got := guestPost(t, service, token, "", body, int64(len(data))).Code; got != http.StatusUnauthorized {
		t.Errorf("a batch whose body arrived as the session closed answered %d, want 401", got)
	}
	if report := takeGuest(t, service, "billet-one"); report.Accepted != 0 {
		t.Errorf("the report counts %d accepted after the session closed under it, want 0", report.Accepted)
	}
}

// cutBody is a body that ends before its stated length.
type cutBody struct{ sent bool }

func (c *cutBody) Read(p []byte) (int, error) {
	if c.sent {
		return 0, io.ErrUnexpectedEOF
	}

	c.sent = true

	return copy(p, "GUEST"), nil
}

// A BODY CUT SHORT IS REFUSED AND COUNTED like one that did not decode: a client
// that stopped sending can still read the answer.
func TestABodyCutShortIsRefusedAndCounted(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	if got := guestPost(t, service, token, "", &cutBody{}, 100).Code; got != http.StatusBadRequest {
		t.Errorf("a body cut short answered %d, want 400", got)
	}

	if report := takeGuest(t, service, "billet-one"); report.Refused != 1 || report.DroppedBytes != 5 {
		t.Errorf("the report counts %d refused and %d bytes dropped, want 1 and 5",
			report.Refused, report.DroppedBytes)
	}
}

// THE NODE'S CLOCK STEPPED BACK BETWEEN TWO BATCHES DOES NOT PUT THE LAST ARRIVAL
// BEFORE THE FIRST, which the ledger would refuse, costing the job its row.
func TestAClockSteppedBackKeepsTheArrivalsInOrder(t *testing.T) {
	t.Parallel()

	service, clock, token, _ := guestService(t)
	first := clock.now()

	if got := postBatch(t, service, token, "", guestBatch(t, 1, "")).Code; got != http.StatusNoContent {
		t.Fatalf("a batch answered %d, want 204", got)
	}

	clock.advance(-time.Minute)

	if got := postBatch(t, service, token, "", guestBatch(t, 2, "")).Code; got != http.StatusNoContent {
		t.Fatalf("a batch after the clock stepped back answered %d, want 204", got)
	}

	report := takeGuest(t, service, "billet-one")
	if !report.FirstReceived.Equal(first) || !report.LastReceived.Equal(first) || report.Accepted != 2 {
		t.Errorf("the arrivals are %s to %s with %d accepted, want both %s and 2",
			report.FirstReceived, report.LastReceived, report.Accepted, first)
	}
}

// repeatedBatch is a batch of one failed test name repeated: almost nothing
// encoded, about 170 KiB decoded.
func repeatedBatch(t *testing.T, seq uint64) []byte {
	t.Helper()

	name := strings.Repeat("x", guestreport.MaxFailedNameBytes)
	b := guestreport.Batch{Seq: seq, AgentVersion: "v0.15.0", TicksPerSecond: 100}

	for s := range guestreport.MaxBatchSuites {
		suite := guestreport.SuiteResult{Name: fmt.Sprintf("suite-%d", s), Tests: 50, Failures: 50}
		for range guestreport.MaxFailedNames {
			suite.Failed = append(suite.Failed, name)
		}
		b.Tests = append(b.Tests, suite)
	}

	data, err := guestreport.EncodeBatch(b)
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}

	return data
}

// WHAT THE KEPT BATCHES DECODE TO AT THE DESTROY IS BOUNDED, not only what they
// take encoded. Batches that decode past 16 MiB are left out of the merge and
// their bytes counted as dropped; the earlier ones are kept.
func TestTheMergeAtTheDestroyHoldsABoundedAmount(t *testing.T) {
	t.Parallel()

	// THE BOUND'S NUMBER, not the constant, so the constant cannot move alone.
	const bound = 16 << 20

	service, clock, token, _ := guestService(t)

	var (
		sent  [][]byte
		total int
	)

	for seq := uint64(1); total <= bound+(4<<20); seq++ {
		data := repeatedBatch(t, seq)
		decoded, err := guestreport.DecodeBatch(data)
		if err != nil {
			t.Fatalf("DecodeBatch: %v", err)
		}

		clock.advance(guestRefill)

		if got := postBatch(t, service, token, "", data).Code; got != http.StatusNoContent {
			t.Fatalf("batch %d answered %d, want 204", seq, got)
		}

		sent = append(sent, data)
		total += decodedSize(&decoded)
	}

	report := takeGuest(t, service, "billet-one")

	merged, err := guestreport.Decode(report.Data)
	if err != nil {
		t.Fatalf("the report's data does not decode: %v", err)
	}

	kept := int(merged.LastSeq)
	if kept == 0 || kept >= len(sent) || report.Accepted != int64(len(sent)) {
		t.Fatalf("the merge kept batches 1 to %d of %d accepted (%d), want some and not all",
			kept, len(sent), report.Accepted)
	}

	held, dropped := 0, int64(0)
	for i, data := range sent {
		decoded, err := guestreport.DecodeBatch(data)
		if err != nil {
			t.Fatalf("DecodeBatch: %v", err)
		}

		if i < kept {
			held += decodedSize(&decoded)
		} else {
			dropped += int64(len(data))
		}
	}

	if held > bound || report.DroppedBytes != dropped {
		t.Errorf("the merge held %d bytes decoded and dropped %d, want at most %d and %d",
			held, report.DroppedBytes, bound, dropped)
	}
}

// THE MERGE AT THE DESTROY TAKES ONE OF THE NODE'S DECODE SLOTS. With all eight
// held past the wait the report goes without its batches, which are counted as
// dropped; with one free it carries them.
func TestTheMergeAtTheDestroyWaitsForADecodeSlot(t *testing.T) {
	t.Parallel()

	for _, free := range []bool{false, true} {
		service, _, token, _ := guestService(t)
		data := guestBatch(t, 1, "")

		if got := postBatch(t, service, token, "", data).Code; got != http.StatusNoContent {
			t.Fatalf("a batch answered %d, want 204", got)
		}

		held := 8
		if free {
			held = 7
		}

		for range held {
			service.guestDecode <- struct{}{}
		}

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		report, ok := service.TakeGuestReport(ctx, "billet-one")
		cancel()

		switch {
		case !ok || report.Accepted != 1:
			t.Errorf("free %t: took %t with %d accepted, want a report of 1", free, ok, report.Accepted)
		case free && (len(report.Data) == 0 || report.DroppedBytes != 0):
			t.Errorf("with a slot free the report carries %d bytes and drops %d, want the batch",
				len(report.Data), report.DroppedBytes)
		case !free && (report.Data != nil || report.DroppedBytes != int64(len(data))):
			t.Errorf("with no slot free the report carries %d bytes and drops %d, want none and %d",
				len(report.Data), report.DroppedBytes, len(data))
		}

		if got := len(service.guestDecode); got != held {
			t.Errorf("free %t: %d slots are held after the merge, want the %d the test holds", free, got, held)
		}
	}
}

// AN AGENT VERSION THE LEDGER CANNOT KEEP IS LEFT OUT OF THE ROW, never sent to
// have the whole report refused; the data keeps it.
func TestAnAgentVersionTheLedgerCannotKeepIsLeftOut(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	data, err := guestreport.EncodeBatch(guestreport.Batch{Seq: 1, AgentVersion: "v0.15.0 beta", TicksPerSecond: 100})
	if err != nil {
		t.Fatalf("EncodeBatch: %v", err)
	}

	if got := postBatch(t, service, token, "", data).Code; got != http.StatusNoContent {
		t.Fatalf("a batch answered %d, want 204", got)
	}

	report := takeGuest(t, service, "billet-one")
	if report.AgentVersion != "" || report.Accepted != 1 || len(report.Data) == 0 {
		t.Errorf("the report carries agent %q, accepted %d, %d bytes; want no agent, 1 and the data",
			report.AgentVersion, report.Accepted, len(report.Data))
	}
}

// A GUEST THAT NEVER SAID A WORD STILL HAS A ROW: no hello, nothing accepted, no
// data, no arrival, so a reader says it never contacted the node rather than show
// nothing.
func TestAGuestThatNeverReportedStillHasARow(t *testing.T) {
	t.Parallel()

	service, _, _, _ := guestService(t)

	report := takeGuest(t, service, "billet-one")
	if report.Hello || report.Accepted != 0 || report.Data != nil || !report.FirstReceived.IsZero() ||
		report.Codec != alloc.GuestReportCodec || report.Schema != 0 || report.AgentVersion != "" {
		t.Errorf("a silent guest's report is %+v, want no hello, nothing accepted, no data, codec %d",
			report, alloc.GuestReportCodec)
	}

	if _, ok := service.TakeGuestReport(t.Context(), "billet-nobody"); ok {
		t.Error("a report was taken for an instance with no session")
	}
}

// A SESSION THIS PROCESS LOADED FROM DISK SAYS THE NODE RESTARTED: what its guest
// sent the process before went with it.
func TestARecoveredSessionSaysTheNodeRestarted(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()

	first, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	first.MonitorGuests()

	credentials, err := first.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if got := postBatch(t, first, credentials.Token, "", guestBatch(t, 1, "")).Code; got != http.StatusNoContent {
		t.Fatalf("a batch answered %d, want 204", got)
	}

	second, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", stateDir,
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService after a restart: %v", err)
	}

	second.MonitorGuests()

	if got := postBatch(t, second, credentials.Token, "", guestBatch(t, 2, "")).Code; got != http.StatusNoContent {
		t.Fatalf("a batch after the restart answered %d, want 204", got)
	}

	report := takeGuest(t, second, "billet-one")
	if !report.NodeRestarted || report.Accepted != 1 {
		t.Errorf("the restarted node's report says restarted %t with %d accepted, want true and 1",
			report.NodeRestarted, report.Accepted)
	}
}

// EACH SESSION HAS A BUCKET: four batches at once, then one every five seconds.
// Past it the answer is 429 with when to come back, the body is not read, and the
// batch is not counted as refused, because the agent sends it again.
func TestTheSessionBucketAnswers429WithRetryAfter(t *testing.T) {
	t.Parallel()

	service, clock, token, _ := guestService(t)

	for seq := uint64(1); seq <= 4; seq++ {
		if got := postBatch(t, service, token, "", guestBatch(t, seq, "")).Code; got != http.StatusNoContent {
			t.Fatalf("batch %d of the burst answered %d, want 204", seq, got)
		}
	}

	data := guestBatch(t, 5, "")
	body := &countingBody{r: bytes.NewReader(data)}

	limited := guestPost(t, service, token, "", body, int64(len(data)))
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "5" {
		t.Fatalf("the fifth batch at once answered %d with Retry-After %q, want 429 and 5",
			limited.Code, limited.Header().Get("Retry-After"))
	}
	if body.read {
		t.Error("a rate-limited batch's body was read")
	}

	clock.advance(2 * time.Second)

	if got := postBatch(t, service, token, "", data); got.Code != http.StatusTooManyRequests ||
		got.Header().Get("Retry-After") != "3" {
		t.Fatalf("two seconds later the batch answered %d with Retry-After %q, want 429 and 3",
			got.Code, got.Header().Get("Retry-After"))
	}

	clock.advance(3 * time.Second)

	if got := postBatch(t, service, token, "", data).Code; got != http.StatusNoContent {
		t.Fatalf("five seconds after the burst the batch answered %d, want 204", got)
	}
	if got := postBatch(t, service, token, "", guestBatch(t, 6, "")).Code; got != http.StatusTooManyRequests {
		t.Errorf("a second batch in the same five seconds answered %d, want 429", got)
	}

	if report := takeGuest(t, service, "billet-one"); report.Accepted != 5 || report.Refused != 0 {
		t.Errorf("the report counts %d accepted and %d refused, want 5 and 0", report.Accepted, report.Refused)
	}
}

// A BODY OVER max_body IS 413, read no further than the bound, counted as refused
// with the bytes read; one exactly at the bound is read whole and judged.
func TestABatchOverItsBoundIs413(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	if got := postBatch(t, service, token, "", make([]byte, guestreport.MaxBatchBytes+1)).Code; got !=
		http.StatusRequestEntityTooLarge {
		t.Errorf("a body one byte over the bound answered %d, want 413", got)
	}
	if got := postBatch(t, service, token, "", make([]byte, guestreport.MaxBatchBytes)).Code; got !=
		http.StatusBadRequest {
		t.Errorf("a body of zeros at the bound answered %d, want 400", got)
	}

	report := takeGuest(t, service, "billet-one")
	if report.Refused != 2 || report.Accepted != 0 || report.DroppedBytes != 2*guestreport.MaxBatchBytes {
		t.Errorf("the report counts %d refused, %d accepted, %d bytes dropped; want 2, 0, %d",
			report.Refused, report.Accepted, report.DroppedBytes, 2*guestreport.MaxBatchBytes)
	}
}

// A JOB'S GUEST KEEPS AT MOST 2 MiB OF ACCEPTED BATCHES. The batch that would pass
// it is 413; one whose length says so is refused unread, and one that does not say
// is read and refused with its bytes counted.
func TestTheSessionBudgetAnswers413(t *testing.T) {
	t.Parallel()

	// THE CONTRACT'S NUMBER, not the constant, so the constant cannot move alone.
	const budget = 2 << 20

	service, clock, token, _ := guestService(t)
	rng := rand.New(rand.NewChaCha8([32]byte{1}))

	var (
		kept     int64
		seq      uint64
		refusing []byte
	)

	for {
		seq++
		data := bulkyBatch(t, seq, rng)
		clock.advance(guestRefill)

		response := postBatch(t, service, token, "", data)
		if response.Code == http.StatusRequestEntityTooLarge {
			refusing = data

			break
		}
		if response.Code != http.StatusNoContent {
			t.Fatalf("batch %d answered %d, want 204 or 413", seq, response.Code)
		}

		kept += int64(len(data))
		if kept > budget {
			t.Fatalf("the session kept %d bytes, past its %d byte budget", kept, budget)
		}
	}

	if kept+int64(len(refusing)) <= budget {
		t.Fatalf("a batch was refused with %d of %d bytes kept and %d more to come",
			kept, budget, len(refusing))
	}

	body := &countingBody{r: bytes.NewReader(refusing)}
	clock.advance(guestRefill)

	if got := guestPost(t, service, token, "", body, int64(len(refusing))).Code; got !=
		http.StatusRequestEntityTooLarge || body.read {
		t.Errorf("a batch whose length passes the budget answered %d, read %t; want 413 unread", got, body.read)
	}

	clock.advance(guestRefill)

	if got := guestPost(t, service, token, "", bytes.NewReader(refusing), -1).Code; got !=
		http.StatusRequestEntityTooLarge {
		t.Errorf("a batch of no stated length past the budget answered %d, want 413", got)
	}

	// THE BYTES OF THE ONE READ: the two that said their length were refused
	// unread.
	report := takeGuest(t, service, "billet-one")
	if report.Accepted != int64(seq-1) || report.Refused != 3 || report.DroppedBytes != int64(len(refusing)) {
		t.Errorf("the report counts %d accepted, %d refused, %d bytes dropped; want %d, 3, %d",
			report.Accepted, report.Refused, report.DroppedBytes, seq-1, len(refusing))
	}
}

// THE WHOLE NODE DECODES AT MOST EIGHT BATCHES AT ONCE. With eight under way a
// ninth is 429, not a refusal, and with seven it is decoded.
func TestTheNodeDecodesAtMostEightBatchesAtOnce(t *testing.T) {
	t.Parallel()

	service, _, token, _ := guestService(t)

	for range 8 {
		service.guestDecode <- struct{}{}
	}

	busy := postBatch(t, service, token, "", guestBatch(t, 1, ""))
	if busy.Code != http.StatusTooManyRequests || busy.Header().Get("Retry-After") != "1" {
		t.Fatalf("a batch with eight decoding answered %d with Retry-After %q, want 429 and 1",
			busy.Code, busy.Header().Get("Retry-After"))
	}

	<-service.guestDecode

	if got := postBatch(t, service, token, "", guestBatch(t, 1, "")).Code; got != http.StatusNoContent {
		t.Fatalf("a batch with seven decoding answered %d, want 204", got)
	}
	if got := len(service.guestDecode); got != 7 {
		t.Errorf("%d decode slots are held after the batch, want the seven the test holds", got)
	}

	if report := takeGuest(t, service, "billet-one"); report.Accepted != 1 || report.Refused != 0 {
		t.Errorf("the report counts %d accepted and %d refused, want 1 and 0", report.Accepted, report.Refused)
	}
}

// WHAT IS NOT A BATCH IS REFUSED AND COUNTED, and nothing the guest sent reaches
// the node's log, whether it was refused or kept.
func TestWhatIsNotABatchIsRefusedAndNeverLogged(t *testing.T) {
	t.Parallel()

	service, clock, token, logged := guestService(t)

	const secret = "GUEST-WROTE-THIS"

	if got := postBatch(t, service, token, "", []byte(secret)).Code; got != http.StatusBadRequest {
		t.Errorf("bytes that are not a batch answered %d, want 400", got)
	}

	clock.advance(guestRefill)

	if got := postBatch(t, service, token, "final=1&more=1", guestBatch(t, 1, "")).Code; got !=
		http.StatusBadRequest {
		t.Errorf("a batch with more in its query answered %d, want 400", got)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, guestReportsPath,
		bytes.NewReader(guestBatch(t, 1, "")))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	wrongType := httptest.NewRecorder()
	service.ServeHTTP(wrongType, req)
	if wrongType.Code != http.StatusBadRequest {
		t.Errorf("a batch sent as application/json answered %d, want 400", wrongType.Code)
	}

	clock.advance(guestRefill)

	if got := postBatch(t, service, token, "", guestBatch(t, 1, secret)).Code; got != http.StatusNoContent {
		t.Fatalf("a batch answered %d, want 204", got)
	}

	report := takeGuest(t, service, "billet-one")
	if report.Refused != 3 || report.Accepted != 1 || report.DroppedBytes != int64(len(secret)) {
		t.Errorf("the report counts %d refused, %d accepted, %d bytes dropped; want 3, 1, %d",
			report.Refused, report.Accepted, report.DroppedBytes, len(secret))
	}
	if strings.Contains(logged.String(), secret) {
		t.Errorf("the node's log quotes what the guest sent: %s", logged.String())
	}
	if !strings.Contains(logged.String(), "accepted=1") {
		t.Errorf("the node's log does not count what it took: %s", logged.String())
	}
}

// THE LEASE IS THE BEARER'S SESSION'S. A guest that names another job anywhere in
// its request, in a header, the query or the batch itself, reports only into its
// own job's record, and the other job's record is untouched.
func TestABatchIsKeptOnlyForTheBearersOwnSession(t *testing.T) {
	t.Parallel()

	service, clock, token, _ := guestService(t)

	other, err := service.Prepare("billet-two", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	data := guestBatch(t, 1, "billet-two")
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, guestReportsPath, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", guestBatchType)
	req.Header.Set("X-Billet-Lease", "two")
	req.Header.Set("X-Billet-Instance", "billet-two")
	req.Header.Set("X-Billet-Token", other.Token)

	forged := httptest.NewRecorder()
	service.ServeHTTP(forged, req)
	if forged.Code != http.StatusNoContent {
		t.Fatalf("a batch naming another job in its headers and its steps answered %d, want 204", forged.Code)
	}

	clock.advance(guestRefill)

	if got := postBatch(t, service, token, "lease=two", guestBatch(t, 2, "")).Code; got != http.StatusBadRequest {
		t.Errorf("a batch naming another lease in its query answered %d, want 400", got)
	}

	if report := takeGuest(t, service, "billet-two"); report.Accepted != 0 || report.Refused != 0 || report.Hello {
		t.Errorf("the other job's report took %d batches and refused %d (hello %t); want none of either",
			report.Accepted, report.Refused, report.Hello)
	}
	if report := takeGuest(t, service, "billet-one"); report.Accepted != 1 || report.Refused != 1 {
		t.Errorf("the bearer's own report took %d and refused %d, want 1 and 1", report.Accepted, report.Refused)
	}
}

// guestRunner is a runner over a real allocator whose cache service takes guest
// reports when monitor is set.
func guestRunner(t *testing.T, store LeaseStore, a *alloc.Allocator, host string, monitor bool,
) (*Runner, *fakeProvider) {
	t.Helper()

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	if monitor {
		service.MonitorGuests()
	}

	if store == nil {
		store = a
	}

	p := &fakeProvider{kind: config.ProviderDocker}

	return New(store, host, &fakeJIT{setID: 7}, p, slog.New(slog.DiscardHandler), WithCacheService(service)), p
}

// launchGuest launches one job and returns its lease and what its guest was given.
func launchGuest(t *testing.T, runner *Runner, p *fakeProvider, a *alloc.Allocator) (*alloc.Lease, provider.Spec) {
	t.Helper()

	lease := assignedLease(t, a)
	if err := runner.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	return lease, p.launched[len(p.launched)-1]
}

// THE RUNNER TELLS THE GUEST TO START ITS AGENT AND RECORDS WHAT IT SAID WHEN IT
// DESTROYS THE COMPUTE, against the lease the runner holds for that guest: a guest
// that names another job in its batch reports into its own, and the other job's
// row says its guest said nothing.
func TestTheRunnerRecordsWhatTheGuestSaidAtDestroy(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	runner, p := guestRunner(t, nil, a, host, true)
	own, spec := launchGuest(t, runner, p, a)
	other, otherSpec := launchGuest(t, runner, p, a)

	if !spec.GuestMonitor || !otherSpec.GuestMonitor {
		t.Fatal("a guest of a node that takes guest reports was not told to start its agent")
	}

	data := guestBatch(t, 1, other.ID)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, guestReportsPath+"?"+guestFinal,
		bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+spec.CacheToken)
	req.Header.Set("Content-Type", guestBatchType)
	req.Header.Set("X-Billet-Lease", other.ID)

	response := httptest.NewRecorder()
	runner.cache.ServeHTTP(response, req)
	if response.Code != http.StatusNoContent {
		t.Fatalf("the guest's batch answered %d, want 204", response.Code)
	}

	for _, lease := range []*alloc.Lease{own, other} {
		if err := runner.Destroy(t.Context(), lease.RequestID); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
	}

	recorded, err := a.LeaseGuestReport(t.Context(), own.ID)
	if err != nil {
		t.Fatalf("LeaseGuestReport: %v", err)
	}
	if recorded.Node != host || recorded.Accepted != 1 || !recorded.FinalSeen || recorded.AgentVersion != "v0.15.0" {
		t.Errorf("the ledger keeps node %q, accepted %d, final %t, agent %q; want %q, 1, true, v0.15.0",
			recorded.Node, recorded.Accepted, recorded.FinalSeen, recorded.AgentVersion, host)
	}
	if merged, err := guestreport.Decode(recorded.Data); err != nil || merged.Batches != 1 {
		t.Errorf("the ledger's data decodes to %d batches (%v), want 1", merged.Batches, err)
	}

	silent, err := a.LeaseGuestReport(t.Context(), other.ID)
	if err != nil {
		t.Fatalf("LeaseGuestReport of the other job: %v", err)
	}
	if silent.Hello || silent.Accepted != 0 || len(silent.Data) != 0 {
		t.Errorf("the other job's row says hello %t, accepted %d, %d bytes; want its guest silent",
			silent.Hello, silent.Accepted, len(silent.Data))
	}
}

// A NODE THAT TAKES NO GUEST REPORTS TELLS NO GUEST TO START ITS AGENT AND RECORDS
// NO ROW, which is how a reader tells it from a guest that never spoke.
func TestANodeThatTakesNoGuestReportsRecordsNone(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	runner, p := guestRunner(t, nil, a, host, false)
	lease, spec := launchGuest(t, runner, p, a)

	if spec.GuestMonitor {
		t.Error("a guest of a node that takes no guest reports was told to start its agent")
	}

	if err := runner.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if _, err := a.LeaseGuestReport(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("a node that takes no guest reports left a row (%v), want none", err)
	}
}

// stalledGuestStore is a ledger that never answers a guest report until it is
// given up on.
type stalledGuestStore struct {
	*alloc.Allocator

	deadline chan time.Duration
}

func (s stalledGuestStore) RecordGuestReport(ctx context.Context, _ string, _ int64, _ alloc.GuestReport) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Hour)
	}
	s.deadline <- time.Until(deadline)
	<-ctx.Done()

	return ctx.Err()
}

// A CONTROL PLANE THAT CANNOT TAKE THE REPORT COSTS THE REPORT, NEVER THE TEARDOWN:
// the send is given up on within its bound and the destroy succeeds.
func TestAGuestReportThatCannotBeSentNeverHoldsTheDestroy(t *testing.T) {
	t.Parallel()

	a, host := newAllocatorWithHost(t)
	store := stalledGuestStore{Allocator: a, deadline: make(chan time.Duration, 1)}
	runner, p := guestRunner(t, store, a, host, true)
	lease, _ := launchGuest(t, runner, p, a)

	// THE CONTRACT'S BOUND, not the constant, so the constant cannot move alone.
	const bound = 2 * time.Second

	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)

		done <- runner.Destroy(context.WithoutCancel(t.Context()), lease.RequestID)
	}()
	t.Cleanup(func() {
		join := time.NewTimer(time.Minute)
		defer join.Stop()

		select {
		case <-finished:
		case <-join.C:
		}
	})

	// THE LEDGER WAS ASKED, or the destroy below would pass by never sending.
	asked := time.NewTimer(30 * time.Second)
	defer asked.Stop()

	select {
	case got := <-store.deadline:
		if got > bound {
			t.Errorf("the report was given %s to be taken, want at most %s", got, bound)
		}
	case <-asked.C:
		t.Fatal("the destroy never offered the ledger the guest's report")
	}

	held := time.NewTimer(bound + 10*time.Second)
	defer held.Stop()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a report the ledger never took failed the destroy: %v", err)
		}
	case <-held.C:
		t.Fatal("a report the ledger never took held the destroy")
	}
}

// A NODE WHOSE DECODE SLOTS ARE ALL HELD COSTS A DESTROY AT MOST THE WAIT FOR ONE:
// the row is recorded with the counts, and the batches it could not merge are
// counted as dropped.
func TestADestroyWaitsABoundedTimeForADecodeSlot(t *testing.T) {
	t.Parallel()

	const bound = 2 * time.Second

	a, host := newAllocatorWithHost(t)
	runner, p := guestRunner(t, nil, a, host, true)
	lease, spec := launchGuest(t, runner, p, a)

	data := guestBatch(t, 1, "")
	if got := postBatch(t, runner.cache, spec.CacheToken, "", data).Code; got != http.StatusNoContent {
		t.Fatalf("the guest's batch answered %d, want 204", got)
	}

	for range 8 {
		runner.cache.guestDecode <- struct{}{}
	}

	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)

		done <- runner.Destroy(context.WithoutCancel(t.Context()), lease.RequestID)
	}()
	t.Cleanup(func() {
		join := time.NewTimer(time.Minute)
		defer join.Stop()

		select {
		case <-finished:
		case <-join.C:
		}
	})

	held := time.NewTimer(bound + 10*time.Second)
	defer held.Stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Destroy: %v", err)
		}
	case <-held.C:
		t.Fatal("a destroy waited past its bound for a decode slot")
	}

	recorded, err := a.LeaseGuestReport(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("LeaseGuestReport: %v", err)
	}
	if recorded.Accepted != 1 || len(recorded.Data) != 0 || recorded.DroppedBytes != int64(len(data)) {
		t.Errorf("the ledger keeps accepted %d, %d bytes, %d dropped; want 1, none and %d",
			recorded.Accepted, len(recorded.Data), recorded.DroppedBytes, len(data))
	}
}
