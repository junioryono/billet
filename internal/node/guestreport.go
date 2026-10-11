package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/guestreport"
)

// The guest-facing routes of billet's in-VM agent, served beside the caches on the
// same listener and the same per-guest bearer. EVERYTHING THEY TAKE IS THE GUEST'S
// OWN, UNVERIFIED VIEW: the job is root in its VM and can send anything the agent
// would, so the node keeps it for an operator to read and decides nothing from it.
const (
	guestPathPrefix  = "/v1/guest/"
	guestLimitsPath  = "/v1/guest/limits"
	guestReportsPath = "/v1/guest/reports"
	// guestBatchType is the one content type a batch is sent as, and guestFinal
	// the one query the agent's last batch adds.
	guestBatchType = "application/vnd.billet.guest-batch"
	guestFinal     = "final=1"
	// guestSchema is the batch schema this node decodes (guestreport, codec 1).
	guestSchema = 1
)

// The limits the node holds whatever a guest sends.
const (
	// guestSessionBudget is how many encoded bytes of accepted batches one job
	// may have kept, which is also what the node holds of them in memory.
	guestSessionBudget = 2 << 20
	// guestRefill and guestBurst are each session's token bucket: one batch every
	// five seconds, four at once.
	guestRefill = 5 * time.Second
	guestBurst  = 4
	// guestDecodeSlots bounds the batches the whole node decodes at once.
	guestDecodeSlots = 8
	// guestReportLimit bounds sending a job's report, which sits on the destroy
	// path as the usage report does, and separately the wait for a decode slot
	// to merge it in.
	guestReportLimit = 2 * time.Second
	// guestAssemblyBound is what a job's kept batches may decode to when they are
	// merged, as decodedSize counts it. A sampling agent's 2 MiB of batches
	// decodes to well under it.
	guestAssemblyBound = 16 << 20
)

// guestChannel is what one session's agent has told the node, kept in memory
// with the session and gone with it. Its own lock, never the session's: a
// session's lock is held for the whole of a Docker store's attach, and a report
// must not wait minutes behind it.
type guestChannel struct {
	mu sync.Mutex
	// batches are the encodings accepted, in the order they arrived, and kept
	// their bytes in all, at most guestSessionBudget.
	batches [][]byte
	kept    int64
	// hello says the agent asked for the limits; final that its last batch was
	// accepted; first and last when, on the node's clock, the first and last
	// accepted batch arrived.
	hello       bool
	final       bool
	first, last time.Time
	// accepted and refused count batches; dropped the bytes of the refused ones
	// the node read. limited and busy count the requests answered 429 by the
	// session's bucket and by the node's decode bound; the agent resends those,
	// so they are not refusals of a batch.
	accepted, refused, dropped int64
	limited, busy              int64
	// tokens and refilled are the bucket.
	tokens   float64
	refilled time.Time
	// taken says the report has been taken for the destroy; nothing more is
	// accepted, because nothing more would reach the ledger.
	taken bool
}

// admit takes a token from the bucket at now, or says how long until one is
// there. Called with mu held.
func (g *guestChannel) admit(now time.Time) (time.Duration, bool) {
	if g.refilled.IsZero() {
		g.tokens, g.refilled = guestBurst, now
	}

	if elapsed := now.Sub(g.refilled); elapsed > 0 {
		g.tokens = min(guestBurst, g.tokens+float64(elapsed)/float64(guestRefill))
		g.refilled = now
	}

	if g.tokens >= 1 {
		g.tokens--

		return 0, true
	}

	return time.Duration((1 - g.tokens) * float64(guestRefill)), false
}

// MonitorGuests turns on the guest routes and tells every guest launched after it
// to start its agent. Called before the listener serves; off, both routes answer
// 404, which the agent reads as a node that takes no reports.
func (s *CacheService) MonitorGuests() { s.guestMonitoring = true }

// GuestMonitoring reports whether this node takes its guests' reports.
func (s *CacheService) GuestMonitoring() bool { return s.guestMonitoring }

// serveGuest answers the agent's two routes. The lease is the bearer's session's
// and nothing in the request can name another.
func (s *CacheService) serveGuest(w http.ResponseWriter, r *http.Request) {
	if !s.guestMonitoring {
		http.NotFound(w, r)

		return
	}

	session, ok := s.authenticate(r)
	if !ok {
		http.Error(w, "unauthorised cache session", http.StatusUnauthorized)

		return
	}

	switch r.URL.Path {
	case guestLimitsPath:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		s.guestHello(w, session)
	case guestReportsPath:
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		s.acceptGuestBatch(w, r, session)
	default:
		http.NotFound(w, r)
	}
}

// guestLimits is what the agent is told it may send.
type guestLimits struct {
	Schemas    []int `json:"schemas"`
	IntervalMS int64 `json:"interval_ms"`
	MaxBody    int   `json:"max_body"`
}

// guestHello answers the limits and records that the agent asked.
func (s *CacheService) guestHello(w http.ResponseWriter, session *cacheSession) {
	g := &session.guest

	g.mu.Lock()
	ended := g.taken || session.closing.Load()
	if !ended {
		g.hello = true
	}
	g.mu.Unlock()

	if ended {
		http.Error(w, "the cache session has ended", http.StatusUnauthorized)

		return
	}

	writeCacheJSON(w, http.StatusOK, guestLimits{
		Schemas:    []int{guestSchema},
		IntervalMS: guestreport.BatchInterval.Milliseconds(),
		MaxBody:    guestreport.MaxBatchBytes,
	})
}

// acceptGuestBatch keeps one batch for the job, or refuses it.
//
// EVERY CHECK THAT COSTS NOTHING COMES BEFORE THE BODY IS READ: the session's
// bucket and its budget, so a flood costs the node a lock and a header. The body
// is read whole, bounded, before a decode slot is taken, so a guest that sends
// slowly holds nothing the node's other guests need. A refusal is counted and
// never logged, because what it would log is the guest's.
func (s *CacheService) acceptGuestBatch(w http.ResponseWriter, r *http.Request, session *cacheSession) {
	g := &session.guest
	final := r.URL.RawQuery == guestFinal

	g.mu.Lock()
	if g.taken || session.closing.Load() {
		g.mu.Unlock()
		http.Error(w, "the cache session has ended", http.StatusUnauthorized)

		return
	}

	if (r.URL.RawQuery != "" && !final) || r.Header.Get("Content-Type") != guestBatchType {
		g.refused++
		g.mu.Unlock()
		http.Error(w, "not a guest batch", http.StatusBadRequest)

		return
	}

	if wait, ok := g.admit(s.now()); !ok {
		g.limited++
		g.mu.Unlock()
		retryAfter(w, wait)
		http.Error(w, "too many guest batches", http.StatusTooManyRequests)

		return
	}

	// A BODY THAT SAYS IT WILL NOT FIT is refused unread; one that says nothing
	// is read and judged by what it was.
	if g.kept >= guestSessionBudget || (r.ContentLength > 0 && g.kept+r.ContentLength > guestSessionBudget) {
		g.refused++
		g.mu.Unlock()
		http.Error(w, "this job's guest report budget is spent", http.StatusRequestEntityTooLarge)

		return
	}
	g.mu.Unlock()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, guestreport.MaxBatchBytes))
	if err != nil {
		if tooLarge, over := errors.AsType[*http.MaxBytesError](err); over &&
			tooLarge.Limit == guestreport.MaxBatchBytes {
			s.refuseGuestBatch(g, len(body))
			http.Error(w, "the guest batch is over its bound", http.StatusRequestEntityTooLarge)

			return
		}

		// A BODY THAT NEVER ARRIVED WHOLE is refused like one that did not
		// decode: a client that cut its body short can still read the answer.
		s.refuseGuestBatch(g, len(body))
		http.Error(w, "the guest batch could not be read", http.StatusBadRequest)

		return
	}

	select {
	case s.guestDecode <- struct{}{}:
	default:
		g.mu.Lock()
		g.busy++
		g.mu.Unlock()
		retryAfter(w, time.Second)
		http.Error(w, "the node is decoding other guests' batches", http.StatusTooManyRequests)

		return
	}

	_, err = guestreport.DecodeBatch(body)
	<-s.guestDecode

	if err != nil {
		s.refuseGuestBatch(g, len(body))
		http.Error(w, "the guest batch was refused", http.StatusBadRequest)

		return
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// AGAIN, because the session may have closed or its report been taken while
	// the body was arriving, and a 204 would tell the agent the node kept a batch
	// nobody will record.
	if g.taken || session.closing.Load() {
		http.Error(w, "the cache session has ended", http.StatusUnauthorized)

		return
	}

	// CHECKED AGAIN WITH THE BATCH IN HAND, because two requests can pass the
	// check above together.
	if g.kept+int64(len(body)) > guestSessionBudget {
		g.refused++
		g.dropped += int64(len(body))
		http.Error(w, "this job's guest report budget is spent", http.StatusRequestEntityTooLarge)

		return
	}

	g.batches = append(g.batches, body)
	g.kept += int64(len(body))
	g.accepted++

	// THE WALL CLOCK, READ UNDER THE LOCK AND NEVER MOVED BACK: the ledger
	// refuses a first arrival after the last, so two batches committed in the
	// other order from their reads, or a clock stepped back, would cost the job
	// its whole row. Without its monotonic reading, so the two compare as the
	// ledger will.
	now := s.now().Round(0)
	if g.first.IsZero() {
		g.first = now
	}
	if now.After(g.last) {
		g.last = now
	}
	if final {
		g.final = true
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *CacheService) refuseGuestBatch(g *guestChannel, read int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.refused++
	g.dropped += int64(read)
}

// retryAfter says when to send again, in whole seconds, never less than one.
func retryAfter(w http.ResponseWriter, wait time.Duration) {
	seconds := max(int64((wait+time.Second-1)/time.Second), 1)
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// TakeGuestReport is what an instance's guest told the node, as the ledger keeps
// it: the accepted batches merged, halved until they fit and encoded, beside what
// the node counted of the channel. It is taken once; the guest is refused after
// it. False when the node takes no guest reports or holds no session for the
// instance.
//
// A ROW EVEN WHEN THE GUEST SAID NOTHING, with no hello and nothing accepted, so a
// reader can say the guest never contacted the node rather than show nothing.
//
// THE MERGE TAKES ONE OF THE NODE'S DECODE SLOTS, waited for until ctx ends, so
// the node decodes no more at once at a destroy than it does for its guests; a
// slot not had in time sends the counts without the batches, whose bytes are
// then counted as dropped.
func (s *CacheService) TakeGuestReport(ctx context.Context, instance string) (alloc.GuestReport, bool) {
	if !s.guestMonitoring {
		return alloc.GuestReport{}, false
	}

	session := s.sessionOf(instance)
	if session == nil {
		return alloc.GuestReport{}, false
	}

	g := &session.guest

	g.mu.Lock()
	if g.taken {
		g.mu.Unlock()

		return alloc.GuestReport{}, false
	}

	g.taken = true
	batches, kept := g.batches, g.kept
	g.batches = nil
	report := alloc.GuestReport{
		Codec:    alloc.GuestReportCodec,
		Accepted: g.accepted, Refused: g.refused, DroppedBytes: g.dropped,
		Hello: g.hello, FinalSeen: g.final,
		// A SESSION THIS PROCESS LOADED FROM DISK was created by one that is
		// gone, and what its guest sent that process went with it.
		NodeRestarted: session.recovered,
		FirstReceived: g.first, LastReceived: g.last,
	}
	limited, busy := g.limited, g.busy
	g.mu.Unlock()

	if len(batches) > 0 {
		if err := s.encodeGuestReport(ctx, &report, batches); err != nil {
			report.DroppedBytes += kept
			s.log.Warn("could not keep what a job's guest reported; its counts are kept without it",
				"instance", instance, "error", err)
		}
	}

	s.log.Info("what a job's guest reported, as this node counted it", "instance", instance,
		"hello", report.Hello, "accepted", report.Accepted, "refused", report.Refused,
		"rate_limited", limited, "node_busy", busy, "dropped_bytes", report.DroppedBytes,
		"final", report.FinalSeen, "node_restarted", report.NodeRestarted, "bytes", len(report.Data))

	return report, true
}

// encodeGuestReport merges batches into report's Data, with the agent's version
// where the ledger can keep it.
//
// WHAT THE BATCHES DECODE TO IS BOUNDED, not only what they take encoded: a batch
// of one name repeated compresses to almost nothing and decodes to a megabyte, so
// the budget's 2 MiB could otherwise hold hundreds of megabytes at the destroy.
// The batches are decoded in the order they arrived until guestAssemblyBound, and
// the bytes of the rest are counted as dropped.
func (s *CacheService) encodeGuestReport(ctx context.Context, report *alloc.GuestReport, batches [][]byte) error {
	select {
	case s.guestDecode <- struct{}{}:
		defer func() { <-s.guestDecode }()
	case <-ctx.Done():
		return fmt.Errorf("node: no decode slot came free for the guest's report: %w", ctx.Err())
	}

	decoded := make([]guestreport.Batch, 0, len(batches))
	held := 0

	var past int64

	for i, data := range batches {
		b, err := guestreport.DecodeBatch(data)
		if err != nil {
			return err
		}

		held += decodedSize(&b)
		if held > guestAssemblyBound {
			for _, rest := range batches[i:] {
				past += int64(len(rest))
			}

			break
		}

		decoded = append(decoded, b)
	}

	merged, err := guestreport.Merge(decoded)
	if err != nil {
		return err
	}

	_, data, err := guestreport.Downsample(merged, alloc.MaxGuestReportBytes)
	if err != nil {
		return err
	}

	report.Data, report.Schema = data, guestSchema
	report.DroppedBytes += past

	// THE LEDGER'S OWN RULE FOR A VERSION, asked of the version alone: one the
	// guest spelled in a way the ledger cannot keep is left out, never sent to
	// refuse the whole report. Data keeps it either way.
	if (alloc.GuestReport{AgentVersion: merged.AgentVersion, Codec: alloc.GuestReportCodec}).Validate() == nil {
		report.AgentVersion = merged.AgentVersion
	}

	return nil
}

// The bytes one decoded element holds at least: its fixed fields (eight bytes a
// number, sixteen a string's header, twenty-four a slice's), its text counted
// apart.
const (
	sampleBytes        = 10 * 8
	processSampleBytes = 8 + 24 + 5*8 + 8
	processRowBytes    = 16 + 5*8
	stepBytes          = 16 + 8
	suiteBytes         = 16 + 4*8 + 24
	failedNameBytes    = 16
)

// decodedSize is about what b holds in memory once decoded: its fixed fields and
// its text, without the allocator's own overhead.
func decodedSize(b *guestreport.Batch) int {
	n := len(b.AgentVersion) + len(b.Samples)*sampleBytes

	for _, p := range b.Processes {
		n += processSampleBytes + len(p.Rows)*processRowBytes
		for _, row := range p.Rows {
			n += len(row.Name)
		}
	}

	for _, m := range b.Steps {
		n += stepBytes + len(m.Name)
	}

	for _, t := range b.Tests {
		n += suiteBytes + len(t.Name)
		for _, name := range t.Failed {
			n += failedNameBytes + len(name)
		}
	}

	return n
}
