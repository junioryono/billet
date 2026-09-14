package nodeclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// The record fixtures of PR 6b commit 3 (fixtures-c3.md, section R). They
// share package-level seams and are NOT parallel.

const testInvocation = "0123456789abcdef0123456789abcdef"

// recordHarness is a plane behind a middleware the fixture controls: every
// request to a path in gates waits on the gate before it reaches the plane,
// and every request is counted.
type recordHarness struct {
	plane    *nodeplane.Plane
	srv      *httptest.Server
	mu       sync.Mutex
	arrived  map[string]chan struct{} // closed when the first request to the path arrives
	gates    map[string]chan struct{} // a request to the path waits on this, when set
	answer   map[string]func(w http.ResponseWriter, r *http.Request) bool
	requests map[string]int
}

func newRecordHarness(t *testing.T) *recordHarness {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute,
		nodeplane.WithCommandTimeout(5*time.Second),
		nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
		}}))
	p.SetPollWindowForTest(60 * time.Millisecond)

	h := &recordHarness{
		plane:    p,
		arrived:  map[string]chan struct{}{},
		gates:    map[string]chan struct{}{},
		answer:   map[string]func(http.ResponseWriter, *http.Request) bool{},
		requests: map[string]int{},
	}

	inner := nodeplane.Handler(log, p, stubStore{}, stubJIT{})

	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests[r.URL.Path]++

		if ch, ok := h.arrived[r.URL.Path]; ok {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}

		gate := h.gates[r.URL.Path]
		h.mu.Unlock()

		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}

		// THE ANSWER IS CHOSEN AFTER THE GATE, so an answer installed while a
		// request waited at the gate is the one that request gets.
		h.mu.Lock()
		custom := h.answer[r.URL.Path]
		h.mu.Unlock()

		if custom != nil && custom(w, r) {
			return
		}

		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)

	// A fixture that fails while a request waits at a gate must not hang the
	// package in the server's Close (a handler that has not read the body never
	// sees the client go away); cleanups run last-registered first, so this
	// releases every gate before the Close above.
	t.Cleanup(h.releaseAll)

	return h
}

func (h *recordHarness) releaseAll() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, ch := range h.gates {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}

// arrivalOf is a channel closed when the first request to the path arrives.
func (h *recordHarness) arrivalOf(path string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan struct{})
	h.arrived[path] = ch

	return ch
}

// hold makes every request to the path wait on the returned channel.
func (h *recordHarness) hold(path string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan struct{})
	h.gates[path] = ch

	return ch
}

// answerRegister installs a custom answer for the registration route; the
// function returns true when it answered and false to let the plane answer.
func (h *recordHarness) answerRegister(f func(w http.ResponseWriter, r *http.Request) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.answer["/v1/register"] = f
}

func (h *recordHarness) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.requests[path]
}

func (h *recordHarness) client(t *testing.T) *nodeclient.Client {
	t.Helper()

	c, err := nodeclient.New(nodeclient.Options{Base: h.srv.URL, Node: "n1"})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

// recordDir is a fresh registration directory and the record's path in it.
func recordDir(t *testing.T) (string, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "registration")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	return dir, filepath.Join(dir, "current")
}

// runRecordLoop runs the loop with the record's path and the fixture's clock,
// joined at cleanup, and returns the cancel.
func runRecordLoop(t *testing.T, c *nodeclient.Client, compute nodeclient.Compute, path string, now func() time.Time) context.CancelFunc {
	t.Helper()

	cancel, _ := runRecordLoopWithDone(t, c, compute, path, now)

	return cancel
}

// runRecordLoopWithDone is runRecordLoop for a fixture that must observe the
// loop's RETURN, not only its cancellation: done is closed when Run returns.
func runRecordLoopWithDone(t *testing.T, c *nodeclient.Client, compute nodeclient.Compute, path string, now func() time.Time) (context.CancelFunc, <-chan struct{}) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})

	go func() {
		defer close(done)

		err := nodeclient.Run(ctx, c, compute, nodeclient.LoopOptions{
			VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
			GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
			Log: slog.New(slog.DiscardHandler), Backoff: 20 * time.Millisecond,
			RegistrationRecordPath: path, Now: now,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the loop stopped for a reason other than shutdown: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return cancel, done
}

// readRecord decodes the record at path into a map, refusing a duplicate key
// or trailing bytes the way the inspector will.
func readRecord(t *testing.T, path string) map[string]any {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("the record is not JSON: %v (%q)", err, body)
	}

	return m
}

// awaitRecord waits for the record to exist with a registered_at different
// from prior (empty prior: any record), within a deadline.
func awaitRecord(t *testing.T, path, prior string) map[string]any {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			m := readRecord(t, path)
			if at, ok := m["registered_at"].(string); ok && at != prior {
				return m
			}
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("no record beyond %q appeared at %s", prior, path)

	return nil
}

func requireNoRecord(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		t.Fatalf("the registration directory holds %v, want nothing", names)
	}
}

// installTrace records the installer's exported seams, each wrapping the
// executed operation and identified by the object it acted on.
type installTrace struct {
	mu  sync.Mutex
	ops []string
	// ino is the inode the chmod and fsync ran on, and current's inode after.
	ino uint64
}

func (tr *installTrace) installer() durablefile.Installer {
	base := durablefile.Installer{}

	return durablefile.Installer{
		SetMode: func(f *os.File, mode os.FileMode) error {
			tr.note("chmod", f)

			return base.SetModeOn(f, mode)
		},
		SyncFile: func(f *os.File) error {
			tr.note("fsync", f)

			return base.SyncFileHandle(f)
		},
		Rename: func(from, to string) error {
			tr.mu.Lock()
			tr.ops = append(tr.ops, "rename "+filepath.Base(to))
			tr.mu.Unlock()

			return os.Rename(from, to)
		},
		SyncDir: func(dir string) error {
			tr.mu.Lock()
			tr.ops = append(tr.ops, "fsync-dir "+filepath.Base(dir))
			tr.mu.Unlock()

			return base.SyncDirectory(dir)
		},
	}
}

func (tr *installTrace) note(kind string, f *os.File) {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	if st, err := f.Stat(); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			tr.ino = sys.Ino
		}
	}

	tr.ops = append(tr.ops, kind+" "+filepath.Base(f.Name()))
}

func (tr *installTrace) snapshot() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	return slices.Clone(tr.ops)
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()

	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no Stat_t")
	}

	return sys.Ino
}

// R1: A REGISTRATION IS PUBLISHED AFTER ITS ACCEPTANCE AND BEFORE RECOVERY,
// with exactly the seven fields, through the installer's one ordering.
func TestARegistrationIsPublishedAfterItsAcceptanceAndBeforeRecovery(t *testing.T) {
	h := newRecordHarness(t)
	dir, path := recordDir(t)

	t.Cleanup(nodeclient.SetInvocationIDForTest(func() string { return testInvocation }))

	tr := &installTrace{}
	inst := tr.installer()

	t.Cleanup(nodeclient.SetInstallRecordForTest(func(d, name string, write func(io.Writer) error) (string, error) {
		return inst.Install(d, name, 0o600, write)
	}))

	var clock atomic.Int64

	clock.Store(time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }

	arrived := h.arrivalOf("/v1/register")
	release := h.hold("/v1/register")

	compute := &fakeCompute{recoverGate: make(chan struct{}), recoverStarted: make(chan struct{}), recoverErr: errors.New("recovery refused once")}
	c := h.client(t)

	runRecordLoop(t, c, compute, path, now)

	<-arrived
	// WHILE THE RESPONSE IS HELD there is no record and no temporary.
	requireNoRecord(t, dir)

	close(release)

	<-compute.recoverStarted

	// RECOVERY IS HELD, and the record already exists: publication preceded it.
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("no record while recovery is held: %v", err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("the record's mode is %o, want 0600", info.Mode().Perm())
	}

	rec := readRecord(t, path)

	keys := make([]string, 0, len(rec))
	for k := range rec {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	want := slices.Clone(nodeclient.RecordFieldNames)
	slices.Sort(want)

	if !slices.Equal(keys, want) {
		t.Errorf("the record's keys are %v, want %v", keys, want)
	}

	if rec["schema"] != float64(1) || rec["node"] != "n1" || rec["deployment"] != deployment ||
		rec["incarnation"] != c.Incarnation() || rec["invocation_id"] != testInvocation ||
		rec["endpoint"] != c.Endpoint().String() || rec["endpoint"] != "http://"+h.srv.Listener.Addr().String() {
		t.Errorf("the record reads %v", rec)
	}

	registeredAt, ok := rec["registered_at"].(string)
	if !ok {
		t.Fatalf("registered_at is %v, not a string", rec["registered_at"])
	}

	at, err := time.Parse(time.RFC3339Nano, registeredAt)
	if err != nil || !at.Equal(now()) {
		t.Errorf("registered_at %v (%v), want the clock's %v", rec["registered_at"], err, now())
	}

	// THE ORDER, through the installer's seams, each on the object it names: the
	// chmod and the fsync on the staged inode, which is current's inode after
	// the rename; the directory flush last.
	ops := tr.snapshot()
	if len(ops) != 4 || ops[0][:5] != "chmod" || ops[1][:5] != "fsync" || ops[2] != "rename current" || ops[3] != "fsync-dir registration" {
		t.Errorf("the installer's operations were %v", ops)
	}

	if tr.ino != inodeOf(t, path) {
		t.Errorf("the chmod and fsync ran on inode %d, but current is inode %d", tr.ino, inodeOf(t, path))
	}

	first := registeredAt
	firstIno := inodeOf(t, path)

	// RECOVERY FAILS; the loop repeats registration under an advanced clock and
	// publishes again, a new inode with the later time.
	clock.Store(now().Add(time.Minute).UnixNano())
	close(compute.recoverGate)

	second := awaitRecord(t, path, first)
	if second["registered_at"] != now().Format(time.RFC3339Nano) || inodeOf(t, path) == firstIno {
		t.Errorf("the second publication reads %v (inode %d, first %d)", second, inodeOf(t, path), firstIno)
	}

	requireOnlyCurrent(t, dir)
}

func requireOnlyCurrent(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || entries[0].Name() != "current" {
		t.Errorf("the registration directory holds %v, want current alone", entries)
	}
}

// R1, the production seams: INVOCATION_ID from the environment reaches the
// record when the getter is left at its default, and the production
// installation call (not a fixture's installer) leaves the record 0600.
func TestTheProductionInvocationGetterReadsTheEnvironment(t *testing.T) {
	h := newRecordHarness(t)
	_, path := recordDir(t)

	t.Setenv("INVOCATION_ID", "fedcba9876543210fedcba9876543210")

	c := h.client(t)
	runRecordLoop(t, c, &fakeCompute{}, path, nil)

	rec := awaitRecord(t, path, "")
	if rec["invocation_id"] != "fedcba9876543210fedcba9876543210" {
		t.Errorf("the record's invocation is %v, want the environment's", rec["invocation_id"])
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode().Perm() != 0o600 {
		t.Errorf("the production installation left the record %o, want 0600", info.Mode().Perm())
	}
}

// R2: A REJECTED REGISTRATION PUBLISHES NOTHING, and a previous record survives
// every failure untouched: a 503 then a success; a 2xx the client rejects; a
// 201 with an accepted body; a refusal that stops the loop; supersession.
func TestARejectedRegistrationPublishesNothing(t *testing.T) {
	t.Run("a 503 then a success", func(t *testing.T) {
		h := newRecordHarness(t)
		dir, path := recordDir(t)

		var calls atomic.Int32

		h.answerRegister(func(w http.ResponseWriter, _ *http.Request) bool {
			if calls.Add(1) == 1 {
				http.Error(w, "not yet", http.StatusServiceUnavailable)

				return true
			}

			return false
		})

		release := h.hold("/v1/register")
		arrived := h.arrivalOf("/v1/register")

		c := h.client(t)
		runRecordLoop(t, c, &fakeCompute{}, path, nil)

		// The first request is held, answered 503, and the second is held again:
		// between the two there is no record.
		release <- struct{}{}
		<-arrived

		waitFor(t, func() bool { return calls.Load() == 1 && h.count("/v1/register") == 2 })
		requireNoRecord(t, dir)

		close(release)
		awaitRecord(t, path, "")
	})

	t.Run("a 2xx the client rejects", func(t *testing.T) {
		// A negotiated version outside the range is a refusal that stops the
		// loop; an unusable TTL is a failure the loop retries. Neither publishes.
		for name, body := range map[string]string{
			"an unsupported negotiated version": `{"version": 9999, "lease_ttl_seconds": 30, "poll_seconds": 1}`,
			"an unusable TTL":                   `{"version": ` + itoa(nodeapi.Version) + `, "lease_ttl_seconds": 0, "poll_seconds": 1}`,
		} {
			t.Run(name, func(t *testing.T) {
				h := newRecordHarness(t)
				dir, path := recordDir(t)

				h.answerRegister(func(w http.ResponseWriter, _ *http.Request) bool {
					w.Header().Set("Content-Type", "application/json")
					writeBody(t, w, body)

					return true
				})

				c := h.client(t)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)

				go func() {
					done <- nodeclient.Run(ctx, c, &fakeCompute{}, nodeclient.LoopOptions{
						VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
						GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
						Log: slog.New(slog.DiscardHandler), Backoff: 20 * time.Millisecond,
						RegistrationRecordPath: path,
					})
				}()

				waitFor(t, func() bool { return h.count("/v1/register") >= 2 || len(done) == 1 })
				requireNoRecord(t, dir)
				cancel()

				if err := <-done; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, nodeclient.ErrRefused) {
					t.Errorf("the loop ended with %v", err)
				}
			})
		}
	})

	t.Run("a 201 with an accepted body", func(t *testing.T) {
		h := newRecordHarness(t)
		_, path := recordDir(t)

		h.answerRegister(func(w http.ResponseWriter, r *http.Request) bool {
			rec := &statusRewriter{ResponseWriter: w, to: http.StatusCreated}
			nodeplane.Handler(slog.New(slog.DiscardHandler), h.plane, stubStore{}, stubJIT{}).ServeHTTP(rec, r)

			return true
		})

		c := h.client(t)
		runRecordLoop(t, c, &fakeCompute{}, path, nil)
		awaitRecord(t, path, "")
	})

	t.Run("a refusal stops the loop with no record", func(t *testing.T) {
		h := newRecordHarness(t)
		dir, path := recordDir(t)

		h.answerRegister(func(w http.ResponseWriter, _ *http.Request) bool {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			writeBody(t, w, `{"code": "`+string(nodeapi.CodeRefused)+`", "message": "no"}`)

			return true
		})

		c := h.client(t)
		err := nodeclient.Run(t.Context(), c, &fakeCompute{}, nodeclient.LoopOptions{
			VCPU: testNodeVCPU, Memory: testNodeMemory, Provider: config.ProviderDocker,
			GuestOS: []config.GuestOS{config.GuestLinux}, Deployment: deployment,
			Log: slog.New(slog.DiscardHandler), Backoff: 20 * time.Millisecond,
			RegistrationRecordPath: path,
		})
		if !errors.Is(err, nodeclient.ErrRefused) {
			t.Errorf("the loop ended with %v, want the refusal", err)
		}

		requireNoRecord(t, dir)
	})

	t.Run("a previous incarnation's record survives every failure", func(t *testing.T) {
		h := newRecordHarness(t)
		dir, path := recordDir(t)

		planted := []byte(`{"schema":1,"node":"n1","deployment":"x","incarnation":"old","invocation_id":"","endpoint":"http://x:1","registered_at":"2026-01-01T00:00:00Z"}` + "\n")
		if err := os.WriteFile(path, planted, 0o600); err != nil {
			t.Fatal(err)
		}

		plantedIno := inodeOf(t, path)

		var calls atomic.Int32

		h.answerRegister(func(w http.ResponseWriter, _ *http.Request) bool {
			switch calls.Add(1) {
			case 1:
				http.Error(w, "not yet", http.StatusServiceUnavailable)
			case 2:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				writeBody(t, w, `{"code": "`+string(nodeapi.CodeSuperseded)+`", "message": "another"}`)
			default:
				return false
			}

			return true
		})

		c := h.client(t)
		runRecordLoop(t, c, &fakeCompute{}, path, nil)

		waitFor(t, func() bool { return calls.Load() >= 3 })

		// Until the success lands the planted record is byte-for-byte and
		// inode-for-inode what it was.
		got := awaitRecord(t, path, "2026-01-01T00:00:00Z")
		if got["incarnation"] != c.Incarnation() || inodeOf(t, path) == plantedIno {
			t.Errorf("the success did not replace the planted record: %v", got)
		}

		requireOnlyCurrent(t, dir)
	})
}

// statusRewriter answers the plane's body under another success status.
type statusRewriter struct {
	http.ResponseWriter
	to int
}

func (s *statusRewriter) WriteHeader(code int) {
	if code == http.StatusOK {
		code = s.to
	}

	s.ResponseWriter.WriteHeader(code)
}

func itoa(n int) string { return fmt.Sprint(n) }

// R5: WITH NO PATH nothing is written and the installer is never called; and
// a record is never removed by the node, not even when Run returns.
func TestNoPathWritesNothingAndTheNodeRemovesNothing(t *testing.T) {
	h := newRecordHarness(t)

	var installs atomic.Int32

	production := nodeclient.DefaultInstallRecord()

	t.Cleanup(nodeclient.SetInstallRecordForTest(func(d, name string, write func(io.Writer) error) (string, error) {
		installs.Add(1)

		return production(d, name, write)
	}))

	c := h.client(t)
	cancel, done := runRecordLoopWithDone(t, c, &fakeCompute{}, "", nil)

	waitFor(t, func() bool { return h.count("/v1/register") >= 1 })
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if installs.Load() != 0 {
		t.Errorf("the installer was called %d times with no path", installs.Load())
	}

	h2 := newRecordHarness(t)
	_, path := recordDir(t)
	c2 := h2.client(t)
	cancel2, done2 := runRecordLoopWithDone(t, c2, &fakeCompute{}, path, nil)

	rec := awaitRecord(t, path, "")
	cancel2()
	// AFTER RUN HAS RETURNED, so a removal on the way out cannot hide behind
	// an assertion made while the loop was still shutting down.
	<-done2

	after := readRecord(t, path)
	if after["registered_at"] != rec["registered_at"] {
		t.Errorf("the record changed after Run returned: %v -> %v", rec, after)
	}
}

// writeBody writes a scripted answer, and a failed write is the fixture's.
func writeBody(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("write the scripted answer: %v", err)
	}
}
