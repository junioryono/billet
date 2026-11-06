package e2e

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/joinconsole"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/pool"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
	"github.com/junioryono/billet/internal/wirecert"
)

// consoleSource is a source whose hosts' consoles are a buffer a test writes, read the
// way the AWS source reads them, through joinconsole.Fingerprints, and judged as
// it judges them: no key is could-not-tell, this key alone vouches, other keys alone
// contradict, and this key among others is could-not-tell. Only Witness is ever asked
// of it here; the AWS source's own tests cover its document and address.
type consoleSource struct {
	hostsource.Source
	mu      sync.Mutex
	console map[string]string // host -> console
	asked   []string
}

func (s *consoleSource) write(host, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.console[host] += "\r\n" + line + "\r\n"
}

func (s *consoleSource) Witness(_ context.Context, c hostsource.Claim) hostsource.Testimony {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, c.HostID)
	keys := joinconsole.Fingerprints(s.console[c.HostID], c.Order)
	switch {
	case len(keys) == 0:
		return hostsource.Testimony{Reason: "nothing on the console yet"}
	case len(keys) == 1 && keys[0] == c.Fingerprint:
		return hostsource.Testimony{Verdict: hostsource.VerdictVouches, Reason: "the console names it"}
	case slices.Contains(keys, c.Fingerprint):
		return hostsource.Testimony{Reason: "the console names several keys"}
	}

	return hostsource.Testimony{Verdict: hostsource.VerdictContradicts, Reason: "the console names " + strings.Join(keys, ",")}
}

func (s *consoleSource) asks() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.asked)
}

// poolJoinWire is the enrollment listener as the control plane assembles it for a pool:
// the real allocator and ledger, and the real judge over a console source, both on one
// clock the test moves.
type poolJoinWire struct {
	ca    *wirecert.CA
	base  string
	alloc *alloc.Allocator
	src   *consoleSource
	mu    sync.Mutex
	now   time.Time
}

// advance moves the clock the judge and the ledger read.
func (w *poolJoinWire) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
}

func (w *poolJoinWire) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.now
}

type joinsOf struct {
	*alloc.Allocator
	*pool.Joins
}

func newPoolJoinWire(t *testing.T) *poolJoinWire {
	t.Helper()
	dir := t.TempDir()
	ledgertest.Seed(t, dir)
	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w := &poolJoinWire{now: time.Now().UTC(), src: &consoleSource{console: map[string]string{}}}
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{{
		Label: "fc", Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux, VCPU: 2, Memory: 4 * config.GiB,
		Image: "ubuntu@1",
	}}, alloc.WithClock(w.clock))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	identity := t.TempDir()
	ca, err := wirecert.LoadOrCreateCA(identity, wireDeployment)
	if err != nil {
		t.Fatal(err)
	}
	w.ca, w.alloc = ca, a
	joins := joinsOf{Allocator: a, Joins: pool.NewJoins(a, map[string]hostsource.Source{"metal": w.src}, w.clock)}
	log := slog.New(slog.DiscardHandler)

	// THE WIRE AS THE CONTROL PLANE ASSEMBLES IT, so a pool's joins that never reach the
	// enrollment listener fail here.
	wire, err := app.BuildNodeWire(app.NodeWireRequest{
		StateDir: identity, Deployment: wireDeployment, Hosts: []string{"127.0.0.1"}, Log: log,
		Plane: nodeplane.New(log, wireDeployment, time.Minute), Enrollments: a,
		PoolJoins: joins, SeesHostAddress: true,
	})
	if err != nil {
		t.Fatalf("BuildNodeWire: %v", err)
	}
	conf, err := wirecert.BootstrapTLS(wire.Serving)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(wire.Bootstrap)
	srv.TLS = conf
	srv.StartTLS()
	t.Cleanup(srv.Close)
	w.base = srv.URL

	return w
}

// order records and places an order, minting its token as the pool does.
func (w *poolJoinWire) order(t *testing.T, id string) string {
	t.Helper()
	ctx := t.Context()
	if err := w.alloc.RecordPoolOrder(ctx, alloc.PoolOrder{ID: id, Source: "metal", Scope: "metal:here", Shape: "m",
		Arch: config.ArchX64}); err != nil {
		t.Fatal(err)
	}
	token, err := w.alloc.NewPoolJoinToken(ctx, id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.alloc.PoolOrderPlaced(ctx, id, "host-"+id); err != nil {
		t.Fatal(err)
	}

	return token
}

func (w *poolJoinWire) ask(t *testing.T, name, token string, csrPEM []byte) ([]byte, error) {
	t.Helper()
	caPEM, _, err := nodeclient.FetchCA(t.Context(), w.base, w.ca.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := nodeclient.EnrollWithIdentity(t.Context(), w.base, name, token, caPEM, csrPEM,
		nodeclient.Identity{Document: []byte(`{"instanceId":"host"}`), Signature: "c2ln"})

	return cert, err
}

// A POOL HOST JOINS WITHOUT A PERSON ONCE ITS SOURCE VOUCHES FOR IT, through the
// assembly the control plane uses: its first ask, before its console shows its key, is
// recorded pending under its order; once the console names it and the judge asks
// again, it is handed a certificate for its order's name, and the order is linked to
// its node with that key and the certificate's serial. A retry is answered the same
// certificate.
func TestAPoolHostJoinsOnceItsSourceVouches(t *testing.T) {
	t.Parallel()
	w := newPoolJoinWire(t)
	token := w.order(t, "pa")
	name := alloc.PoolNodeName("pa")
	csr, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := wirecert.FingerprintOfCSR(csr)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.ask(t, name, token, csr); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("before the console names the key: %v, want not approved", err)
	}
	if e, ok, err := w.alloc.LookupEnrollment(t.Context(), name); err != nil || !ok || e.State != alloc.EnrollPending ||
		e.PoolOrder != "pa" {
		t.Fatalf("the first ask left %+v, %v, %v", e, ok, err)
	}

	w.src.write("host-pa", joinconsole.Line("pa", fp))
	w.advance(pool.DefaultJudgeEvery)
	cert, err := w.ask(t, name, token, csr)
	if err != nil {
		t.Fatalf("once the console names the key: %v", err)
	}
	leaf, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: cert})
	if err != nil || leaf.Subject.CommonName != name {
		t.Fatalf("the certificate: %v, %v", leaf, err)
	}
	o, err := w.alloc.ReadPoolOrder(t.Context(), "pa")
	if err != nil || o.NodeName != name || o.NodeFingerprint != fp || o.NodeCertSerial != wirecert.Serial(leaf) {
		t.Fatalf("the order after the join: %+v, %v", o, err)
	}
	again, err := w.ask(t, name, token, csr)
	if err != nil || !bytes.Equal(again, cert) {
		t.Fatalf("a retry: %v, the same certificate %v", err, bytes.Equal(again, cert))
	}
}

// A HOST WHOSE CONSOLE NAMES ANOTHER KEY IS TOLD TO WAIT AND ITS ORDER IS HELD: the
// request is recorded for a person without spending the token, and the host the console
// names is told to wait too, its source unasked, while the name is taken. A person who
// denies the other key and releases the hold lets that host's next ask be judged again,
// and it joins.
func TestAPoolHostTheConsoleContradictsWaitsForAPerson(t *testing.T) {
	t.Parallel()
	w := newPoolJoinWire(t)
	ctx := t.Context()
	token := w.order(t, "pb")
	name := alloc.PoolNodeName("pb")
	imposter, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	imposterFP, err := wirecert.FingerprintOfCSR(imposter)
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	hostFP, err := wirecert.FingerprintOfCSR(host)
	if err != nil {
		t.Fatal(err)
	}
	w.src.write("host-pb", joinconsole.Line("pb", hostFP))
	if _, err := w.ask(t, name, token, imposter); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("a contradicted ask: %v, want told to wait", err)
	}
	e, ok, err := w.alloc.LookupEnrollment(ctx, name)
	if err != nil || !ok || e.Fingerprint != imposterFP || e.State != alloc.EnrollPending || e.PoolOrder != "pb" {
		t.Fatalf("the contradicted ask was recorded as %+v (%v, %v), want pending for a person", e, ok, err)
	}
	o, err := w.alloc.ReadPoolOrder(ctx, "pb")
	if err != nil || o.JoinHold == "" || o.NodeName != "" {
		t.Fatalf("the order after a contradiction: %+v, %v", o, err)
	}
	tokens, err := w.alloc.JoinTokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].Uses != 1 {
		t.Fatalf("the token after a contradiction: %+v, %v", tokens, err)
	}

	w.advance(pool.DefaultJudgeEvery)
	asked := w.src.asks()
	if _, err := w.ask(t, name, token, host); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("the console's own key while the order is held: %v, want told to wait", err)
	}
	if w.src.asks() != asked {
		t.Fatal("a held order's source was asked")
	}

	if err := w.alloc.DecideEnrollment(ctx, name, imposterFP, alloc.EnrollDenied, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.alloc.ReleasePoolJoin(ctx, "pb"); err != nil {
		t.Fatal(err)
	}
	w.advance(pool.DefaultJudgeEvery)
	cert, err := w.ask(t, name, token, host)
	if err != nil || len(cert) == 0 {
		t.Fatalf("the console's own key once the other was denied and the hold released: %v", err)
	}
}

// A HOST WHOSE ORDER IS HELD UNTIL ITS TOKEN LAPSES STILL JOINS ON A PERSON'S WORD: its
// request was recorded while held, a person approves it, and the host's next ask, with
// its lapsed token, collects the certificate.
func TestAHeldHostWhoseTokenLapsedJoinsOnAPersonsWord(t *testing.T) {
	t.Parallel()
	w := newPoolJoinWire(t)
	ctx := t.Context()
	token := w.order(t, "pc")
	name := alloc.PoolNodeName("pc")
	host, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	hostFP, err := wirecert.FingerprintOfCSR(host)
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	otherFP, err := wirecert.FingerprintOfCSR(other)
	if err != nil {
		t.Fatal(err)
	}
	// THE CONSOLE NAMES A KEY THE HOST NO LONGER HOLDS, so its own ask is contradicted.
	w.src.write("host-pc", joinconsole.Line("pc", otherFP))
	if _, err := w.ask(t, name, token, host); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("a contradicted ask: %v", err)
	}
	w.advance(3 * time.Hour)
	if _, err := w.ask(t, name, token, host); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("an ask once the token lapsed: %v, want told to wait", err)
	}
	// AS `billet nodes approve` DOES: sign the recorded request, record its serial, decide.
	bundle, err := w.ca.SignNodeCSR(name, host)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.alloc.DecideEnrollment(ctx, name, hostFP, alloc.EnrollApproved, string(bundle.CertPEM),
		wirecert.Serial(leaf)); err != nil {
		t.Fatalf("a person's approval of the held request: %v", err)
	}
	cert, err := w.ask(t, name, token, host)
	if err != nil || !bytes.Equal(cert, bundle.CertPEM) {
		t.Fatalf("the host's ask with its lapsed token after the approval: %v", err)
	}
	if o, err := w.alloc.ReadPoolOrder(ctx, "pc"); err != nil || o.NodeName != name || o.JoinHold != "" {
		t.Fatalf("the order after the approval: %+v, %v", o, err)
	}
}

// A HOST WHOSE TOKEN LAPSES BEFORE ITS ORDER IS PLACED IS NEVER TURNED AWAY: it is
// told to ask again while the order is being ordered, and again once it is placed with
// the token lapsed, recording nothing; the pool then holds the order nothing can join
// from, the host's next ask is recorded for a person, and their approval admits it.
func TestAHostWhoseTokenLapsedBeforePlacementWaitsForAPerson(t *testing.T) {
	t.Parallel()
	w := newPoolJoinWire(t)
	ctx := t.Context()
	if err := w.alloc.RecordPoolOrder(ctx, alloc.PoolOrder{ID: "pd", Source: "metal", Scope: "metal:here", Shape: "m",
		Arch: config.ArchX64}); err != nil {
		t.Fatal(err)
	}
	token, err := w.alloc.NewPoolJoinToken(ctx, "pd", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	name := alloc.PoolNodeName("pd")
	host, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	hostFP, err := wirecert.FingerprintOfCSR(host)
	if err != nil {
		t.Fatal(err)
	}
	waits := func(when string) {
		t.Helper()
		if _, err := w.ask(t, name, token, host); !errors.Is(err, nodeclient.ErrEnrollUnavailable) &&
			!errors.Is(err, nodeclient.ErrNotApproved) {
			t.Fatalf("%s: %v, want told to wait", when, err)
		}
	}
	waits("while the order is being ordered")
	w.advance(2 * time.Hour)
	if err := w.alloc.PoolOrderPlaced(ctx, "pd", "host-pd"); err != nil {
		t.Fatal(err)
	}
	waits("placed, with its token lapsed")
	if _, ok, err := w.alloc.LookupEnrollment(ctx, name); err != nil || ok {
		t.Fatalf("a lapsed token recorded a request (%v, %v)", ok, err)
	}
	if held, err := w.alloc.HoldLapsedPoolJoins(ctx); err != nil || held != 1 {
		t.Fatalf("the pool held %d, %v", held, err)
	}
	waits("once the order is held")
	e, ok, err := w.alloc.LookupEnrollment(ctx, name)
	if err != nil || !ok || e.Fingerprint != hostFP || e.State != alloc.EnrollPending || e.PoolOrder != "pd" {
		t.Fatalf("the held order's request: %+v, %v, %v", e, ok, err)
	}
	bundle, err := w.ca.SignNodeCSR(name, host)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.alloc.DecideEnrollment(ctx, name, hostFP, alloc.EnrollApproved, string(bundle.CertPEM),
		wirecert.Serial(leaf)); err != nil {
		t.Fatal(err)
	}
	if cert, err := w.ask(t, name, token, host); err != nil || !bytes.Equal(cert, bundle.CertPEM) {
		t.Fatalf("after the approval: %v", err)
	}
}
