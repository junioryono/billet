package node

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/joblog"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/regularfile"
)

// The guest-facing route of a job's step logs, on the listener and bearer the guest
// report uses. WHAT IT TAKES IS WHAT THE JOB WROTE: kept for an operator to search,
// quoted wherever it is shown, and never read by a decision.
const (
	guestLogsPath = "/v1/guest/logs"
	guestLogType  = "application/vnd.billet.job-log"
	// jobLogRefill and jobLogBurst are each session's token bucket, apart from its
	// report's: a busy step writes far faster than an agent samples.
	jobLogRefill = 200 * time.Millisecond
	jobLogBurst  = 16
	// guestLogInterval is how often the agent is asked to send what it has of a step's
	// lines. A search has what a job wrote within about this.
	guestLogInterval = 10 * time.Second
)

// The store's own files and how it works through them.
const (
	jobLogDirectory = "joblogs"
	jobLogShipped   = "shipped"
	jobLogSuffix    = ".chunk"
	// jobLogPassEvery is how often a shipping pass runs with nothing to wake it, and
	// jobLogEvictEvery an eviction pass.
	jobLogPassEvery  = 10 * time.Second
	jobLogEvictEvery = 10 * time.Minute
	// jobLogShipTimeout bounds shipping one chunk.
	jobLogShipTimeout = 30 * time.Second
	// jobLogSeqSet bounds the chunk numbers one lease remembers.
	jobLogSeqSet = 1 << 20
)

// JobLogLimits are the bounds node.logs sets. A zero is config's default.
type JobLogLimits struct {
	Retention   time.Duration
	MaxBytes    int64
	MaxJobBytes int64
}

// JobLogSink is where a node's kept chunks go to be indexed: the control plane, through
// the node wire. The node keeps the text either way; this is what makes it searchable.
type JobLogSink interface {
	// LogIndexID names the index the control plane holds now, false when it holds none
	// or the pairing's wire carries none. A new one means nothing has been shipped to
	// it, which is how a promoted controller or a restored index comes to hold what
	// this node holds.
	LogIndexID() (string, bool)
	// ShipLogChunk sends one kept chunk. A refusal is permanent, so the chunk is not sent
	// again; any other error is could-not-tell, and it is.
	ShipLogChunk(ctx context.Context, leaseID string, firstLine int64, data []byte) (refused bool, err error)
}

// JobLogStore keeps the step logs of a node's jobs on its own disk, a chunk to a file as
// the guest sent it, and ships each to the control plane to index. Its text stays here:
// a search reads it back through the node wire.
type JobLogStore struct {
	dir    string
	limits JobLogLimits
	log    *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	leases map[string]*leaseLog
	total  int64
	wake   chan struct{}
}

// leaseLog is what the store holds of one lease. Only under the store's lock.
type leaseLog struct {
	nextLine int64
	bytes    int64
	newest   time.Time
	seqs     map[uint64]bool
}

// NewJobLogStore loads what an earlier process kept under stateDir. A chunk file that
// cannot be named or read back is left where it is and said once: it holds a line
// number other chunks' numbers count past.
func NewJobLogStore(stateDir string, limits JobLogLimits, log *slog.Logger) (*JobLogStore, error) {
	s := &JobLogStore{dir: filepath.Join(stateDir, jobLogDirectory), limits: limits, log: log, now: time.Now,
		leases: map[string]*leaseLog{}, wake: make(chan struct{}, 1)}
	if s.log == nil {
		s.log = slog.Default()
	}

	if err := (durablefile.Installer{}).MkdirAll(s.dir, 0o700); err != nil {
		return nil, fmt.Errorf("node: create the job log directory: %w", err)
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("node: read the job log directory: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() || !validLease(e.Name()) {
			continue
		}

		chunks, err := s.chunksOf(e.Name())
		if err != nil {
			return nil, err
		}

		l := &leaseLog{seqs: map[uint64]bool{}}
		for _, c := range chunks {
			l.bytes += c.bytes
			l.nextLine = max(l.nextLine, c.firstLine+c.lines)
			l.seqs[c.seq] = true
			if c.at.After(l.newest) {
				l.newest = c.at
			}
		}

		s.leases[e.Name()] = l
		s.total += l.bytes
	}

	return s, nil
}

// validLease says name can be a lease id: it comes from a filesystem or a command, and
// becomes a path.
func validLease(name string) bool {
	if len(name) != 32 {
		return false
	}

	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// keptChunk is one chunk file, by what its name says.
type keptChunk struct {
	name      string
	firstLine int64
	seq       uint64
	lines     int64
	bytes     int64
	at        time.Time
}

// chunkName is a chunk's file name, which is all that is read of it to order and find it.
func chunkName(firstLine int64, seq uint64, lines int64) string {
	return fmt.Sprintf("%012d-%012d-%06d%s", firstLine, seq, lines, jobLogSuffix)
}

// parseChunkName reverses chunkName, false for any other name.
func parseChunkName(name string) (firstLine int64, seq uint64, lines int64, ok bool) {
	stem, found := strings.CutSuffix(name, jobLogSuffix)
	if !found {
		return 0, 0, 0, false
	}

	parts := strings.Split(stem, "-")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}

	firstLine, err1 := strconv.ParseInt(parts[0], 10, 64)
	seq, err2 := strconv.ParseUint(parts[1], 10, 64)
	lines, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || firstLine < 0 || lines <= 0 {
		return 0, 0, 0, false
	}

	return firstLine, seq, lines, true
}

// chunksOf is a lease's chunk files in line order.
func (s *JobLogStore) chunksOf(leaseID string) ([]keptChunk, error) {
	dir := filepath.Join(s.dir, leaseID)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("node: read a lease's job log: %w", err)
	}

	out := make([]keptChunk, 0, len(entries))

	for _, e := range entries {
		firstLine, seq, lines, ok := parseChunkName(e.Name())
		if !ok {
			continue
		}

		info, err := e.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("node: read a lease's job log: %w", err)
		}

		out = append(out, keptChunk{name: e.Name(), firstLine: firstLine, seq: seq, lines: lines,
			bytes: info.Size(), at: info.ModTime()})
	}

	slices.SortFunc(out, func(a, b keptChunk) int { return cmp.Compare(a.firstLine, b.firstLine) })

	return out, nil
}

// ErrJobLogBudget says a job, or the node, has no more room for a job's log.
var ErrJobLogBudget = errors.New("node: this job's log budget is spent")

// Keep stores one chunk the guest sent, encoded as it arrived, numbering its lines from
// where the lease's last chunk ended. A number seen before is kept already, which is what
// a retried request sends: no error, and nothing stored twice.
func (s *JobLogStore) Keep(leaseID string, chunk *joblog.Chunk, data []byte) error {
	if !validLease(leaseID) {
		return errors.New("node: a job log needs a lease")
	}

	lines := int64(len(chunk.Lines))

	s.mu.Lock()
	defer s.mu.Unlock()

	l := s.leases[leaseID]
	if l == nil {
		l = &leaseLog{seqs: map[uint64]bool{}}
		s.leases[leaseID] = l
	}

	if l.seqs[chunk.Seq] {
		return nil
	}

	size := int64(len(data))
	if l.bytes+size > s.limits.MaxJobBytes || l.nextLine+lines > joblog.MaxLines || len(l.seqs) >= jobLogSeqSet {
		return ErrJobLogBudget
	}

	dir := filepath.Join(s.dir, leaseID)
	if err := (durablefile.Installer{}).MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("node: create a lease's job log: %w", err)
	}

	if _, err := (durablefile.Installer{}).InstallNew(dir, chunkName(l.nextLine, chunk.Seq, lines), 0o600,
		func(w io.Writer) error {
			_, err := w.Write(data)

			return err
		}); err != nil {
		return fmt.Errorf("node: keep a chunk of a job's log: %w", err)
	}

	l.seqs[chunk.Seq] = true
	l.nextLine += lines
	l.bytes += size
	l.newest = s.now()
	s.total += size

	select {
	case s.wake <- struct{}{}:
	default:
	}

	return nil
}

// ReadLines reads back the lines ranges name of a lease's log, at most maxBytes of
// text, in line order. Held is false when this node holds nothing of the lease, which is
// never no matching lines; Cut says the bound stopped the read.
func (s *JobLogStore) ReadLines(leaseID string, ranges []nodeapi.LogLineRange, maxBytes int) (nodeapi.LogReadResult,
	error,
) {
	var out nodeapi.LogReadResult

	if !validLease(leaseID) {
		return out, errors.New("node: a job log read needs a lease")
	}

	if _, err := os.Stat(filepath.Join(s.dir, leaseID)); errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}

	chunks, err := s.chunksOf(leaseID)
	if err != nil {
		return out, err
	}

	out.Held = true
	held := 0

	for _, c := range chunks {
		if !anyOverlap(ranges, c.firstLine, c.lines) {
			continue
		}

		chunk, err := s.readChunk(leaseID, c.name)
		if err != nil {
			out.Unreadable++

			continue
		}

		for i, line := range chunk.Lines {
			number := c.firstLine + int64(i)
			if !inRanges(ranges, number) {
				continue
			}

			if held+len(line.Text) > maxBytes || len(out.Lines) >= nodeapi.MaxLogReadLines {
				out.Cut = true

				return out, nil
			}

			held += len(line.Text)
			out.Lines = append(out.Lines, nodeapi.LogLine{Line: number, AtMillis: line.AtMillis, Text: line.Text})
		}
	}

	return out, nil
}

// readChunk reads one chunk file through a descriptor proved regular.
func (s *JobLogStore) readChunk(leaseID, name string) (joblog.Chunk, error) {
	f, _, err := regularfile.Open(filepath.Join(s.dir, leaseID, name), regularfile.Options{NoFollow: true})
	if err != nil {
		return joblog.Chunk{}, err
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, joblog.MaxChunkBytes+1))
	if err != nil {
		return joblog.Chunk{}, err
	}

	return joblog.Decode(data)
}

func anyOverlap(ranges []nodeapi.LogLineRange, first, lines int64) bool {
	for _, r := range ranges {
		if r.First < first+lines && first <= r.Last {
			return true
		}
	}

	return false
}

func inRanges(ranges []nodeapi.LogLineRange, line int64) bool {
	for _, r := range ranges {
		if r.First <= line && line <= r.Last {
			return true
		}
	}

	return false
}

// shippedMark is how far a lease's chunks have reached an index. A mark naming another
// index counts for nothing, which is how a new index comes to hold them.
type shippedMark struct {
	Index string `json:"index"`
	// Through is the line number the chunks shipped reach, the next one to ship beginning
	// there.
	Through int64 `json:"through"`
}

// readMark reads a lease's mark, the zero mark when there is none or it cannot be read:
// the cost of the second is shipping again, which the index takes as a repeat.
func (s *JobLogStore) readMark(leaseID string) shippedMark {
	var mark shippedMark

	data, err := regularfile.ReadFile(filepath.Join(s.dir, leaseID, jobLogShipped), 1<<10,
		regularfile.Options{NoFollow: true})
	if err != nil {
		return shippedMark{}
	}

	if err := json.Unmarshal(data, &mark); err != nil {
		return shippedMark{}
	}

	return mark
}

func (s *JobLogStore) writeMark(leaseID string, mark shippedMark) error {
	data, err := json.Marshal(mark)
	if err != nil {
		return err
	}

	_, err = (durablefile.Installer{}).Install(filepath.Join(s.dir, leaseID), jobLogShipped, 0o600,
		func(w io.Writer) error {
			_, err := w.Write(data)

			return err
		})

	return err
}

// Run ships what is kept and evicts what is past its bounds, until ctx ends. It decides
// nothing about any job: a pass that fails is said and the next tries again.
func (s *JobLogStore) Run(ctx context.Context, sink JobLogSink) {
	ship := time.NewTicker(jobLogPassEvery)
	defer ship.Stop()

	evict := time.NewTicker(jobLogEvictEvery)
	defer evict.Stop()

	for {
		s.shipPass(ctx, sink)

		select {
		case <-ctx.Done():
			return
		case <-ship.C:
		case <-s.wake:
		case <-evict.C:
			s.evictPass()
		}
	}
}

// leaseIDs is every lease the store holds, oldest first, so a new index is filled in the
// order the jobs ran.
func (s *JobLogStore) leaseIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]string, 0, len(s.leases))
	for id := range s.leases {
		ids = append(ids, id)
	}

	slices.SortFunc(ids, func(a, b string) int {
		return cmp.Or(s.leases[a].newest.Compare(s.leases[b].newest), cmp.Compare(a, b))
	})

	return ids
}

// shipPass sends what the index does not hold yet, stopping at the first thing that
// could not be told, since the next chunk would very likely meet the same.
func (s *JobLogStore) shipPass(ctx context.Context, sink JobLogSink) {
	index, ok := sink.LogIndexID()
	if !ok {
		return
	}

	for _, id := range s.leaseIDs() {
		if ctx.Err() != nil {
			return
		}

		if !s.shipLease(ctx, sink, index, id) {
			return
		}
	}
}

// shipLease sends a lease's chunks past its mark, and reports false when it should stop
// for now. The mark is written once for the lease, not for each chunk: a crash costs a
// repeat, which the index ignores.
func (s *JobLogStore) shipLease(ctx context.Context, sink JobLogSink, index, leaseID string) bool {
	mark := s.readMark(leaseID)
	if mark.Index != index {
		mark = shippedMark{Index: index}
	}

	chunks, err := s.chunksOf(leaseID)
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}

	if err != nil {
		s.log.Warn("could not list a job's kept log; trying again", "lease", leaseID, "error", err)

		return false
	}

	reached, ok := mark.Through, true

	for _, c := range chunks {
		if c.firstLine < reached {
			continue
		}

		data, err := regularfile.ReadFile(filepath.Join(s.dir, leaseID, c.name), joblog.MaxChunkBytes,
			regularfile.Options{NoFollow: true})
		if err != nil {
			s.log.Warn("could not read a chunk of a job's kept log; trying again", "lease", leaseID, "error", err)

			ok = false

			break
		}

		shipCtx, cancel := context.WithTimeout(ctx, jobLogShipTimeout)
		refused, err := sink.ShipLogChunk(shipCtx, leaseID, c.firstLine, data)
		cancel()

		if err != nil && !refused {
			s.log.Warn("could not send a job's log to the control plane to index; trying again",
				"lease", leaseID, "error", err)

			ok = false

			break
		}

		if refused {
			// WHAT THE PLANE REFUSES FOR GOOD IS NOT SENT AGAIN. The text stays here, and
			// is unfindable: said, since a search cannot say it.
			s.log.Warn("the control plane refused a chunk of a job's log, so it will not be searchable",
				"lease", leaseID, "line", c.firstLine, "error", err)
		}

		reached = c.firstLine + c.lines
	}

	if reached != mark.Through {
		mark.Through = reached
		if err := s.writeMark(leaseID, mark); err != nil {
			s.log.Warn("could not record how far a job's log reached the index", "lease", leaseID, "error", err)
		}
	}

	return ok
}

// evictPass deletes whole jobs' logs past the retention, then the oldest past the node's
// bound. The newest may still be running, which costs it the chunks already kept and
// nothing else.
func (s *JobLogStore) evictPass() {
	cutoff := s.now().Add(-s.limits.Retention)

	for _, id := range s.leaseIDs() {
		s.mu.Lock()
		l, known := s.leases[id]
		over := known && (l.newest.Before(cutoff) || s.total > s.limits.MaxBytes)
		s.mu.Unlock()

		if !over {
			continue
		}

		if err := os.RemoveAll(filepath.Join(s.dir, id)); err != nil {
			s.log.Warn("could not delete a job's kept log; trying again", "lease", id, "error", err)

			continue
		}

		s.mu.Lock()
		s.total -= l.bytes
		delete(s.leases, id)
		s.mu.Unlock()
	}
}

// guestLogOffer is what the agent is told it may send of a job's step logs.
type guestLogOffer struct {
	IntervalMS int64 `json:"interval_ms"`
	MaxBody    int   `json:"max_body"`
}

// KeepGuestLogs turns on the guest's log route, for sessions whose tier asks for it.
// Called before the listener serves; without it the route answers 404 and a hello
// offers no home for a log.
func (s *CacheService) KeepGuestLogs(store *JobLogStore) { s.jobLogs = store }

// JobLogs is the store this node keeps its jobs' logs in, nil where it keeps none.
func (s *CacheService) JobLogs() *JobLogStore { return s.jobLogs }

// keepsLogs says this session's job's log is kept: the node keeps logs, and the tier
// asked. An older control plane sends no such word, and nothing is kept.
func (s *CacheService) keepsLogs(session *cacheSession) bool {
	return s.jobLogs != nil && session.leaseID != "" && session.cache != nil && session.cache.KeepLogs
}

// acceptGuestLog keeps one chunk of a job's log. EVERY CHECK THAT COSTS NOTHING COMES
// BEFORE THE BODY, as for a batch, and the lease is the bearer's session's: nothing the
// guest sends can name another.
func (s *CacheService) acceptGuestLog(w http.ResponseWriter, r *http.Request, session *cacheSession) {
	if !s.keepsLogs(session) {
		http.NotFound(w, r)

		return
	}

	if r.Header.Get("Content-Type") != guestLogType {
		http.Error(w, "not a job log", http.StatusBadRequest)

		return
	}

	g := &session.guest

	g.mu.Lock()
	if g.taken || session.closing.Load() {
		g.mu.Unlock()
		http.Error(w, "the cache session has ended", http.StatusUnauthorized)

		return
	}

	wait, ok := g.admitLog(s.now())
	g.mu.Unlock()

	if !ok {
		retryAfter(w, wait)
		http.Error(w, "too many log chunks", http.StatusTooManyRequests)

		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, joblog.MaxChunkBytes))
	if err != nil {
		http.Error(w, "the log chunk could not be read", http.StatusBadRequest)

		return
	}

	select {
	case s.guestDecode <- struct{}{}:
	default:
		retryAfter(w, time.Second)
		http.Error(w, "the node is decoding other guests' batches", http.StatusTooManyRequests)

		return
	}

	chunk, err := joblog.Decode(data)
	<-s.guestDecode

	// NEVER LOGGED, for a refusal's usual reason: what it would log is the guest's.
	if err != nil {
		http.Error(w, "the log chunk was refused", http.StatusBadRequest)

		return
	}

	switch err := s.jobLogs.Keep(session.leaseID, &chunk, data); {
	case errors.Is(err, ErrJobLogBudget):
		http.Error(w, "this job's log budget is spent", http.StatusRequestEntityTooLarge)
	case err != nil:
		s.log.Warn("could not keep a chunk of a job's log", "instance", session.instance, "error", err)
		http.Error(w, "the log chunk was not kept", http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// admitLog is admit's bucket for a job's log chunks. Called with mu held.
func (g *guestChannel) admitLog(now time.Time) (time.Duration, bool) {
	if g.logRefilled.IsZero() {
		g.logTokens, g.logRefilled = jobLogBurst, now
	}

	if elapsed := now.Sub(g.logRefilled); elapsed > 0 {
		g.logTokens = min(jobLogBurst, g.logTokens+float64(elapsed)/float64(jobLogRefill))
		g.logRefilled = now
	}

	if g.logTokens >= 1 {
		g.logTokens--

		return 0, true
	}

	return time.Duration((1 - g.logTokens) * float64(jobLogRefill)), false
}
