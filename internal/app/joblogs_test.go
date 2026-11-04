package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/joblog"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/peercred"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// jobOn is a ledger and a lease whose job ran on nodeName.
func jobOn(t *testing.T, nodeName string) (*alloc.Allocator, string) {
	t.Helper()

	db, err := state.Open(t.Context(), ledgertest.Dir(t))
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tier := config.Tier{Label: "t", Provider: config.ProviderDocker, GuestOS: config.GuestLinux, VCPU: 2,
		Memory: 8 * config.GiB, Image: "ubuntu:24.04"}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 16, MaxMemory: 64 * config.GiB}, []config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{Name: nodeName, Provider: config.ProviderDocker,
		VCPU: 16, Memory: 64 * config.GiB}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	lease, err := a.Reserve(t.Context(), "t")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 7, 1); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, nodeName); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	return a, lease.ID
}

func encodedChunk(t *testing.T, lines ...string) (*joblog.Chunk, []byte) {
	t.Helper()

	c := &joblog.Chunk{Seq: 1}
	for _, text := range lines {
		c.Lines = append(c.Lines, joblog.Line{Text: text})
	}

	data, err := joblog.Encode(*c)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	return c, data
}

// A CHUNK REACHES THE INDEX ONLY FROM THE NODE THE LEDGER SAYS RAN THE JOB. A lease is only a
// name, and a node writing into another's job would have a search read a line from a node
// that never held it. What cannot be indexed is refused for good, by the sentinel a node
// reads as not to send again.
func TestALogChunkIsIndexedOnlyFromItsJobsNode(t *testing.T) {
	t.Parallel()

	a, lease := jobOn(t, "n1")

	dir := filepath.Join(t.TempDir(), "logindex")
	index, err := state.OpenLogIndex(dir)
	if err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	defer index.Close()

	sink := newLogIndexSink(index, a)
	_, data := encodedChunk(t, "error: found me")

	for name, tc := range map[string]struct {
		node, lease string
		data        []byte
		first       int64
	}{
		"another node":   {"n2", lease, data, 0},
		"no such job":    {"n1", "ffffffffffffffffffffffffffffffff", data, 0},
		"not a chunk":    {"n1", lease, []byte("hunter2"), 0},
		"past the rowid": {"n1", lease, data, joblog.MaxLines},
	} {
		err := sink.IndexLogChunk(t.Context(), tc.node, tc.lease, tc.first, tc.data)
		if !errors.Is(err, nodeplane.ErrRefused) {
			t.Errorf("%s: %v, want refused", name, err)
		}
	}

	if err := sink.IndexLogChunk(t.Context(), "n1", lease, 0, data); err != nil {
		t.Fatalf("the job's own node: %v", err)
	}

	match, err := state.BuildLogMatch([]string{"found"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var hits []state.LogHit
	if _, err := state.SearchLogIndex(t.Context(), dir, state.LogSearch{Match: match, Since: time.Now().Add(-time.Hour),
		MaxScan: 10, Visit: func(h state.LogHit) bool {
			hits = append(hits, h)

			return true
		}}); err != nil {
		t.Fatalf("SearchLogIndex: %v", err)
	}
	if len(hits) != 1 || hits[0].Lease != lease {
		t.Errorf("found %+v, want the one line of the job", hits)
	}
}

// nodeLogSink ships a node's store into a real sink, as the wire would.
type nodeLogSink struct {
	id   string
	node string
	sink *logIndexSink
}

func (s *nodeLogSink) LogIndexID() (string, bool) { return s.id, true }

func (s *nodeLogSink) ShipLogChunk(ctx context.Context, lease string, first int64, data []byte) (bool, error) {
	err := s.sink.IndexLogChunk(ctx, s.node, lease, first, data)

	return errors.Is(err, nodeplane.ErrRefused), err
}

// A LINE A GUEST SENT IS FOUND BY THE INDEX AND READ BACK FROM THE NODE, with what the index said
// of its number: the pieces meet at the one thing a search depends on.
func TestALineReachesASearchAndReadsBack(t *testing.T) {
	t.Parallel()

	a, lease := jobOn(t, "n1")

	dir := filepath.Join(t.TempDir(), "logindex")
	index, err := state.OpenLogIndex(dir)
	if err != nil {
		t.Fatalf("OpenLogIndex: %v", err)
	}
	defer index.Close()

	store, err := node.NewJobLogStore(t.TempDir(), node.JobLogLimits{Retention: time.Hour, MaxBytes: 1 << 20,
		MaxJobBytes: 1 << 16}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewJobLogStore: %v", err)
	}

	c, data := encodedChunk(t, "go test ./...", "error: the second line")
	if err := store.Keep(lease, c, data); err != nil {
		t.Fatalf("Keep: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)

		store.Run(ctx, &nodeLogSink{id: index.ID(), node: "n1", sink: newLogIndexSink(index, a)})
	}()
	defer func() {
		cancel()
		<-done
	}()

	match, err := state.BuildLogMatch([]string{"second line"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var hit *state.LogHit

	deadline := time.Now().Add(30 * time.Second)
	for hit == nil {
		if time.Now().After(deadline) {
			t.Fatal("the line never reached the index")
		}

		_, err := state.SearchLogIndex(ctx, dir, state.LogSearch{Match: match, Since: time.Now().Add(-time.Hour),
			MaxScan: 10, Visit: func(h state.LogHit) bool {
				hit = &h

				return false
			}})
		if err != nil {
			t.Fatalf("SearchLogIndex: %v", err)
		}

		if hit == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}

	read, err := store.ReadLines(t.Context(), lease, []nodeapi.LogLineRange{{First: hit.Line, Last: hit.Line}}, 1<<10)
	if err != nil {
		t.Fatalf("ReadLines: %v", err)
	}
	if len(read.Lines) != 1 || read.Lines[0].Text != "error: the second line" || hit.Level != joblog.LevelError {
		t.Errorf("line %d, %s, reads back %+v", hit.Line, hit.Level, read)
	}
}

// socketConfig is a config whose identity directory is short enough for a Unix socket's path.
func socketConfig(t *testing.T) *config.Config {
	t.Helper()

	dir, err := os.MkdirTemp("", "b") //nolint:usetesting // a Unix socket's path has a bound that t.TempDir's long name passes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	return &config.Config{Server: &config.ServerConfig{IdentityDir: dir}}
}

// NOTHING ANSWERING IS NOT NO SUCH LINE, and says so by its sentinel.
func TestNoControllerIsSaid(t *testing.T) {
	t.Parallel()

	_, err := ReadJobLogLines(t.Context(), socketConfig(t), JobLogRead{Lease: "l"})
	if !errors.Is(err, ErrNoController) {
		t.Errorf("nothing listening: %v", err)
	}
}

// THE SOCKET ANSWERS ITS OWN USER, by what the kernel says of the peer; its mode is the
// service's and its group's; and something else at the name is not replaced.
func TestTheOperatorSocketIsTheOperators(t *testing.T) {
	t.Parallel()

	cfg := socketConfig(t)

	ln, err := listenOperator(t.Context(), cfg)
	if err != nil {
		t.Fatalf("listenOperator: %v", err)
	}
	defer ln.Close()

	info, err := os.Lstat(operatorSocketPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Errorf("the socket's mode is %o", got)
	}

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			t.Errorf("Accept: %v", err)

			return
		}
		accepted <- conn
	}()

	var d net.Dialer

	client, err := d.DialContext(t.Context(), "unix", operatorSocketPath(cfg))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	conn := <-accepted
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		t.Fatalf("accepted a %T", conn)
	}

	uid, err := peercred.UID(unixConn)
	if err != nil || uid != os.Geteuid() {
		t.Errorf("the peer is %d, %v; want this process's own user", uid, err)
	}

	// A SOCKET LEFT BEHIND IS REPLACED, and anything else at the name stands.
	if _, err := listenOperator(t.Context(), cfg); err != nil {
		t.Errorf("a socket left behind: %v", err)
	}

	other := socketConfig(t)
	if err := os.WriteFile(operatorSocketPath(other), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenOperator(t.Context(), other); err == nil {
		t.Error("a file at the socket's name was replaced")
	}
}

// A READ THE SOCKET CANNOT SERVE SAYS WHY IN ITS STATUS, so a reader does not take it for lines
// that do not exist: a job the ledger holds none of is 404, and a ledger that could not answer is
// 503, which is could-not-tell and never absent.
func TestAReadOfAJobTheLedgerHoldsNoneOfIsRefused(t *testing.T) {
	t.Parallel()

	a, _ := jobOn(t, "n1")
	cfg := socketConfig(t)
	plane := nodeplane.New(slog.New(slog.DiscardHandler), "deployment", time.Minute)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		serveOperator(ctx, cfg, &operatorServer{jobs: a, plane: plane, log: slog.New(slog.DiscardHandler)})
	}()
	defer func() {
		cancel()
		<-done
	}()

	var err error

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, err = ReadJobLogLines(t.Context(), cfg, JobLogRead{Lease: "ffffffffffffffffffffffffffffffff"})
		if !errors.Is(err, ErrNoController) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}
	if err == nil || errors.Is(err, ErrNoController) || !strings.Contains(err.Error(), "404") {
		t.Errorf("a job the ledger holds none of: %v", err)
	}
}

// ANOTHER USER'S CONNECTION IS CLOSED BEFORE IT IS SERVED, and the listener goes on to the next.
func TestAnotherUsersConnectionIsRefused(t *testing.T) {
	t.Parallel()

	ln, err := listenOperator(t.Context(), socketConfig(t))
	if err != nil {
		t.Fatalf("listenOperator: %v", err)
	}
	defer ln.Close()

	ln.log = slog.New(slog.DiscardHandler)

	var allow atomic.Bool

	ln.allowed = func(int) bool { return allow.Load() }

	accepted := make(chan net.Conn, 1)

	go func() {
		if conn, err := ln.Accept(); err == nil {
			accepted <- conn
		}
	}()

	var dialer net.Dialer

	refused, err := dialer.DialContext(t.Context(), "unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer refused.Close()

	if err := refused.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := refused.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("a refused connection read %v, want it closed", err)
	}

	allow.Store(true)

	kept, err := dialer.DialContext(t.Context(), "unix", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()

	select {
	case conn := <-accepted:
		_ = conn.Close()
	case <-time.After(30 * time.Second):
		t.Error("the listener did not go on to the next connection")
	}
}

// unanswering is a ledger that cannot be read.
type unanswering struct{}

func (unanswering) Job(context.Context, string) (alloc.JobRecord, error) {
	return alloc.JobRecord{}, errors.New("the ledger did not answer")
}

func (unanswering) Lease(context.Context, string) (*alloc.Lease, error) {
	return nil, errors.New("the ledger did not answer")
}

func (unanswering) EndLeaseHold(context.Context, string, string) (bool, error) {
	return false, errors.New("the ledger did not answer")
}

// A LEDGER THAT COULD NOT ANSWER IS 503, could-not-tell, and never a job that does not exist.
func TestAReadTheLedgerCouldNotAnswerIsNotAbsent(t *testing.T) {
	t.Parallel()

	cfg := socketConfig(t)
	plane := nodeplane.New(slog.New(slog.DiscardHandler), "deployment", time.Minute)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		serveOperator(ctx, cfg, &operatorServer{jobs: unanswering{}, plane: plane, log: slog.New(slog.DiscardHandler)})
	}()
	defer func() {
		cancel()
		<-done
	}()

	var err error

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, err = ReadJobLogLines(t.Context(), cfg, JobLogRead{Lease: "ffffffffffffffffffffffffffffffff"})
		if !errors.Is(err, ErrNoController) {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}
	if err == nil || errors.Is(err, ErrNoController) || !strings.Contains(err.Error(), "503") {
		t.Errorf("a ledger that could not answer: %v", err)
	}
}
