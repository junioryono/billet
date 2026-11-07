package fleetops

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/joinconsole"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/wirecert"
)

// realFP is a fingerprint of the shape wirecert writes.
const realFP = "SHA256:abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ"

// A POOL HOST SAYS ITS LINE ON THE CONSOLE, in the one spelling the plane reads, and
// appends it rather than replacing what the console holds; a console that is not there
// is a failure, never a file billet creates.
func TestAPoolHostSaysItsFingerprintOnTheConsole(t *testing.T) { //nolint:paralleltest // consolePath is shared
	dir := t.TempDir()
	console := filepath.Join(dir, "console")
	if err := os.WriteFile(console, []byte("boot output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	was := consolePath
	t.Cleanup(func() { consolePath = was })
	consolePath = console

	for range 2 {
		if err := announceOnConsole("pa", realFP); err != nil {
			t.Fatalf("announce: %v", err)
		}
	}
	raw, err := os.ReadFile(console)
	if err != nil {
		t.Fatal(err)
	}
	// EACH ON A LINE OF ITS OWN, whatever the console was part way through.
	want := "boot output\n\n" + joinconsole.Line("pa", realFP) + "\n\n" + joinconsole.Line("pa", realFP) + "\n"
	if string(raw) != want {
		t.Fatalf("the console holds %q, want %q", raw, want)
	}
	if got := joinconsole.Fingerprints(string(raw), "pa"); len(got) != 1 || got[0] != realFP {
		t.Fatalf("the plane would read %v", got)
	}

	consolePath = filepath.Join(dir, "absent")
	if err := announceOnConsole("pa", realFP); err == nil {
		t.Fatal("a console that is not there was written")
	}
	if _, err := os.Stat(consolePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("announcing created %s (%v)", consolePath, err)
	}
}

// A HOST JOINING FROM A POOL ORDER IS ITS ORDER'S NODE, and a config naming another is
// refused before anything is asked.
func TestAPoolHostsNameIsItsOrders(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Node: &config.NodeConfig{Name: "epyc-1", TLS: &config.NodeTLS{}}}
	err := EnrollNode(t.Context(), cli.Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}, cfg,
		"https://127.0.0.1:1", "SHA256:ca", "tok", "pa")
	if err == nil || !strings.Contains(err.Error(), "pool-pa") {
		t.Fatalf("a pool host under another name answered %v", err)
	}
}

// joinRecorder is a bootstrap listener that answers pending until a host has asked
// twice with what it should send, then approves; it records, at each ask, the request
// and what the console held.
type joinRecorder struct {
	mu       sync.Mutex
	ca       *wirecert.CA
	console  string
	requests []nodeapi.EnrollRequest
	consoles []string
	// caFaults and approvalFaults are what the next authority and the next approval are
	// answered instead, one each (a fault's name, below); enrollStatus, when set, answers
	// every enrollment. caAsks counts the authorities asked for, and cancel is called at
	// the cancelAt'th ask of either route.
	caFaults, approvalFaults []string
	enrollStatus             int
	caAsks                   int
	cancel                   context.CancelFunc
	cancelAt                 int
	// servedBy, when set, signs the listener's certificate in place of ca, which the
	// recorder still answers as the deployment's; deployment, when set, is the
	// deployment the authority's answer names.
	servedBy   *wirecert.CA
	deployment string
	// appendServer appends servedBy's certificate to the authority the recorder answers,
	// as a responder on the unverified connection may.
	appendServer bool
}

// The faults a joinRecorder answers with: the connection ended before any answer, an
// answer cut off mid-body, as a connection lost after the plane decided would, or a
// status code.
const (
	faultAbort = "abort"
	faultBody  = "body"
	// faultReason answers 404 with a reason phrase of its own, as anything that answers
	// the unverified connection may.
	faultReason = "reason"
)

// secretReason is the reason phrase faultReason writes.
const secretReason = "the secret reason"

// fault answers w with the named fault and reports whether it did.
func fault(w http.ResponseWriter, name string) bool {
	switch name {
	case "":
		return false
	case faultAbort:
		panic(http.ErrAbortHandler)
	case faultReason:
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		_, _ = buf.WriteString("HTTP/1.1 404 " + secretReason + "\r\nContent-Length: 0\r\nConnection: close\r\n\r\n") //nolint:errcheck // the host sees what arrived
		_ = buf.Flush()
		_ = conn.Close()

		return true
	case faultBody:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"state":"appr`)) //nolint:errcheck // the answer is being cut off anyway
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	status, err := strconv.Atoi(name)
	if err != nil {
		panic("no fault " + name)
	}
	w.WriteHeader(status)

	return true
}

// next takes the first of faults, under the recorder's lock, and calls cancel when this
// is the cancelAt'th ask.
func (j *joinRecorder) next(faults *[]string, asks int) string {
	if j.cancel != nil && asks == j.cancelAt {
		j.cancel()
	}
	if len(*faults) == 0 {
		return ""
	}
	f := (*faults)[0]
	*faults = (*faults)[1:]

	return f
}

// answer writes a JSON body; a write that fails reaches the host as a failed ask, which
// the test sees.
func answer(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (j *joinRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/ca":
		j.mu.Lock()
		j.caAsks++
		f := j.next(&j.caFaults, j.caAsks)
		j.mu.Unlock()
		if fault(w, f) {
			return
		}
		j.mu.Lock()
		deployment := j.deployment
		j.mu.Unlock()
		if deployment == "" {
			deployment = "0123456789abcdef0123456789abcdef"
		}
		authority := string(j.ca.CertPEM())
		if j.appendServer {
			authority += string(j.servedBy.CertPEM())
		}
		answer(w, nodeapi.CAResponse{CAPEM: authority, Fingerprint: j.ca.Fingerprint(),
			Deployment: deployment})
	case "/v1/enroll":
		var req nodeapi.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		raw, _ := os.ReadFile(j.console) //nolint:errcheck // an unread console is an empty one, which the test reads
		// THE CONSOLE IS EMPTIED AFTER EACH ASK, as the newest 64 KiB a source reads
		// moves on, so the next ask must find a line written for it.
		if err := os.WriteFile(j.console, nil, 0o600); err != nil {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}
		j.mu.Lock()
		j.requests = append(j.requests, req)
		j.consoles = append(j.consoles, string(raw))
		n := len(j.requests)
		var none []string
		j.next(&none, n)
		status := j.enrollStatus
		j.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)

			return
		}
		fp, err := wirecert.FingerprintOfCSR([]byte(req.CSRPEM))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		if n < 2 || req.IdentityDocument == "" {
			answer(w, nodeapi.EnrollResponse{State: "pending", Fingerprint: fp})

			return
		}
		bundle, err := j.ca.SignNodeCSR(req.Node, []byte(req.CSRPEM))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		j.mu.Lock()
		f := j.next(&j.approvalFaults, 0)
		j.mu.Unlock()
		if fault(w, f) {
			return
		}
		answer(w, nodeapi.EnrollResponse{State: "approved", Fingerprint: fp,
			CertPEM: string(bundle.CertPEM), CAPEM: string(j.ca.CertPEM())})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// A POOL HOST'S ENROLLMENT SAYS ITS KEY ON THE CONSOLE BEFORE EVERY ASK, AND SENDS WHAT
// ITS PLATFORM SIGNED ONCE IT CAN READ IT: an identity the metadata service could not
// give on the first ask is read again before the next, and the console already names
// the key the first ask carries.
func TestAPoolHostsEnrollmentSaysItsKeyAndSendsItsIdentity(t *testing.T) { //nolint:paralleltest // shared vars
	rec, srv, cfg, ca, reads := servePoolJoin(t, nil)
	var out bytes.Buffer
	if err := EnrollNode(bounded(t), cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "pa"); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out.String())
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 2 {
		t.Fatalf("asked %d times, want twice", len(rec.requests))
	}
	fp, err := wirecert.FingerprintOfCSR([]byte(rec.requests[0].CSRPEM))
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range rec.consoles {
		if got := joinconsole.Fingerprints(c, "pa"); len(got) != 1 || got[0] != fp {
			t.Fatalf("ask %d: the console named %v, want the key %s", i+1, got, fp)
		}
	}
	if rec.requests[0].IdentityDocument != "" || rec.requests[1].IdentityDocument != `{"instanceId":"i-1"}` ||
		rec.requests[1].IdentitySignature != "c2ln" || *reads != 2 {
		t.Fatalf("identities sent %+v after %d reads", rec.requests, *reads)
	}
	if _, err := os.Stat(cfg.Node.TLS.CertPath); err != nil {
		t.Fatalf("the certificate was not written: %v", err)
	}
}

// AN ANSWER THAT DID NOT ARRIVE IS NO VERDICT: a pool host whose boot runs its join once
// asks again for an authority it could not read (no answer, an answer cut off, a 503),
// and again with the same key for an approval it lost (no answer, an answer cut off, a
// server error in its place), and is handed the certificate.
func TestAPoolHostAsksAgainForAnAnswerItLost(t *testing.T) { //nolint:paralleltest // shared vars
	rec, srv, cfg, ca, _ := servePoolJoin(t, func(j *joinRecorder) {
		j.caFaults = []string{faultAbort, faultBody, "503"}
		j.approvalFaults = []string{faultAbort, faultBody, "500", "502", "504"}
	})
	var out bytes.Buffer
	if err := EnrollNode(bounded(t), cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "pa"); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out.String())
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.caFaults) != 0 || len(rec.approvalFaults) != 0 || rec.caAsks != 4 || len(rec.requests) != 7 {
		t.Fatalf("faults left %v %v, the authority asked for %d times, enrollment %d", rec.caFaults, rec.approvalFaults,
			rec.caAsks, len(rec.requests))
	}
	for i, r := range rec.requests {
		if r.CSRPEM != rec.requests[0].CSRPEM {
			t.Fatalf("ask %d carried another key", i+1)
		}
	}
	if _, err := os.Stat(cfg.Node.TLS.CertPath); err != nil {
		t.Fatalf("the certificate was not written: %v", err)
	}
}

// AN ANSWER IS NOT ASKED AGAIN: a refusal of the enrollment, an authority the plane
// does not have, and an authority that is not the one named each end the join after one
// ask, and none reads as a lost answer.
func TestAnAnswerEndsAPoolHostsJoin(t *testing.T) { //nolint:paralleltest // shared vars
	for name, c := range map[string]struct {
		set                func(*joinRecorder)
		otherCA            bool
		caAsks, enrollAsks int
		// says is what the answer must name, if anything.
		says string
	}{
		"a refusal":         {set: func(j *joinRecorder) { j.enrollStatus = http.StatusConflict }, caAsks: 1, enrollAsks: 1},
		"a bad token":       {set: func(j *joinRecorder) { j.enrollStatus = http.StatusUnauthorized }, caAsks: 1, enrollAsks: 1},
		"no authority":      {set: func(j *joinRecorder) { j.caFaults = []string{"404"} }, caAsks: 1},
		"another authority": {otherCA: true, caAsks: 1},
		"a listener the authority did not sign": {set: func(j *joinRecorder) {
			other, err := wirecert.LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"), "fedcba9876543210fedcba9876543210")
			if err != nil {
				t.Fatal(err)
			}
			j.servedBy = other
		}, caAsks: 1, says: "billet ca retire"},
		"a root appended to the authority": {set: func(j *joinRecorder) {
			other, err := wirecert.LoadOrCreateCA(filepath.Join(t.TempDir(), "ca"), "fedcba9876543210fedcba9876543210")
			if err != nil {
				t.Fatal(err)
			}
			j.servedBy, j.appendServer = other, true
		}, caAsks: 1, says: "billet ca retire"},
	} {
		rec, srv, cfg, ca, _ := servePoolJoin(t, c.set)
		fp := ca.Fingerprint()
		if c.otherCA {
			fp = "SHA256:" + strings.Repeat("A", 43)
		}
		var out bytes.Buffer
		err := EnrollNode(bounded(t), cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, fp, "tok", "pa")
		rec.mu.Lock()
		caAsks, enrollAsks := rec.caAsks, len(rec.requests)
		rec.mu.Unlock()
		if err == nil || errors.Is(err, nodeclient.ErrUnreachable) || caAsks != c.caAsks || enrollAsks != c.enrollAsks ||
			!strings.Contains(err.Error(), c.says) {
			t.Fatalf("%s: answered %v after %d authorities and %d enrollments, want an answer after %d and %d",
				name, err, caAsks, enrollAsks, c.caAsks, c.enrollAsks)
		}
	}
}

// A PERSON'S JOIN IS NOT ASKED AGAIN FOR AN AUTHORITY that did not arrive: they see the
// failure, where a pool host nobody is watching asks again.
func TestAPersonsJoinSaysTheAuthorityDidNotArrive(t *testing.T) { //nolint:paralleltest // shared vars
	rec, srv, cfg, ca, _ := servePoolJoin(t, func(j *joinRecorder) { j.caFaults = []string{faultAbort} })
	var out bytes.Buffer
	err := EnrollNode(bounded(t), cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !errors.Is(err, nodeclient.ErrUnreachable) || rec.caAsks != 1 || len(rec.requests) != 0 {
		t.Fatalf("answered %v after %d authorities and %d enrollments", err, rec.caAsks, len(rec.requests))
	}
}

// A CANCELLED JOIN STOPS ASKING AGAIN, while it waits for an authority and while it
// waits for its enrollment's answer.
func TestACancelledJoinStopsAskingAgain(t *testing.T) { //nolint:paralleltest // shared vars
	for name, set := range map[string]func(*joinRecorder){
		"the authority":  func(j *joinRecorder) { j.caFaults = slices.Repeat([]string{faultAbort}, 1000) },
		"the enrollment": func(j *joinRecorder) { j.enrollStatus = http.StatusServiceUnavailable },
	} {
		ctx, cancel := context.WithCancel(bounded(t))
		rec, srv, cfg, ca, _ := servePoolJoin(t, func(j *joinRecorder) {
			set(j)
			j.cancel, j.cancelAt = cancel, 2
		})
		var out bytes.Buffer
		err := EnrollNode(ctx, cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "pa")
		cancel()
		rec.mu.Lock()
		asks := rec.caAsks + len(rec.requests)
		rec.mu.Unlock()
		if !errors.Is(err, context.Canceled) || asks > 4 {
			t.Fatalf("%s: answered %v after %d asks", name, err, asks)
		}
	}
}

// A TLS ANSWER IS NOT A LOST ONE: a listener that wants a client certificate, which a
// join pointed at the node wire rather than the bootstrap listener meets, ends a pool
// host's join rather than asking again for good.
func TestAPoolHostsJoinEndsOnATLSAnswer(t *testing.T) { //nolint:paralleltest // shared vars
	rec, _, cfg, ca, _ := servePoolJoin(t, nil)
	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatal(err)
	}
	conf.ClientAuth = tls.RequireAnyClientCert
	srv := httptest.NewUnstartedServer(rec)
	srv.TLS = conf
	srv.StartTLS()
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(t.Context(), joinBound)
	defer cancel()
	var out bytes.Buffer
	err = EnrollNode(ctx, cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "pa")
	if err == nil || errors.Is(err, nodeclient.ErrUnreachable) || ctx.Err() != nil {
		t.Fatalf("a listener that wants a client certificate answered %v\n%s", err, out.String())
	}
}

// joinBound is how long a join in these tests may take: a retry that never stops ends
// the test there, asking every 10 ms meanwhile rather than as fast as it can.
const joinBound = 20 * time.Second

// bounded is the test's context, ended at joinBound.
func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), joinBound)
	t.Cleanup(cancel)

	return ctx
}

// servePoolJoin serves a joinRecorder set as set says, and points the pool host's
// console and metadata at test doubles; the identity is readable from the second read
// on.
func servePoolJoin(t *testing.T, set func(*joinRecorder)) (*joinRecorder, *httptest.Server, *config.Config, *wirecert.CA, *int) {
	t.Helper()
	dir := t.TempDir()
	ca, err := wirecert.LoadOrCreateCA(filepath.Join(dir, "ca"), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(dir, "console")
	if err := os.WriteFile(console, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &joinRecorder{ca: ca, console: console}
	if set != nil {
		set(rec)
	}
	if rec.servedBy != nil {
		if serving, err = rec.servedBy.IssueServer([]string{"127.0.0.1"}); err != nil {
			t.Fatal(err)
		}
		if conf, err = wirecert.BootstrapTLS(serving); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewUnstartedServer(rec)
	srv.TLS = conf
	srv.StartTLS()
	t.Cleanup(srv.Close)

	wasConsole, wasIdentity, wasEvery := consolePath, instanceIdentity, enrollPollEvery
	t.Cleanup(func() { consolePath, instanceIdentity, enrollPollEvery = wasConsole, wasIdentity, wasEvery })
	consolePath, enrollPollEvery = console, 10*time.Millisecond
	reads := new(int)
	instanceIdentity = func(context.Context) ([]byte, string, error) {
		*reads++
		if *reads == 1 {
			return nil, "", errors.New("the metadata service is not answering yet")
		}

		return []byte(`{"instanceId":"i-1"}`), "c2ln", nil
	}

	tlsDir := filepath.Join(dir, "tls")
	cfg := &config.Config{Node: &config.NodeConfig{Name: "pool-pa", TLS: &config.NodeTLS{
		CertPath: filepath.Join(tlsDir, "node.crt"), KeyPath: filepath.Join(tlsDir, "node.key"), CAPath: filepath.Join(tlsDir, "ca.crt"),
	}}}

	return rec, srv, cfg, ca, reads
}

// A PERSON'S APPROVAL OF A POOL HOST'S REQUEST LINKS ITS ORDER TO ITS NODE with the
// serial of the certificate the approval issued, so a release can revoke it first.
func TestApprovingAPoolRequestLinksItsOrder(t *testing.T) {
	stateDir := t.TempDir()
	cfgPath := writeCAConfig(t, stateDir)
	ctx := t.Context()
	a, closeDB, err := ControlPlaneAllocator(ctx, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{ID: "pa", Source: "phoenixnap", Scope: "pnap:x", Shape: "s1", Arch: config.ArchX64}); err != nil {
		t.Fatal(err)
	}
	tok, err := a.NewPoolJoinToken(ctx, "pa", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.PoolOrderPlaced(ctx, "pa", "srv-1"); err != nil {
		t.Fatal(err)
	}
	csr, _, err := wirecert.NewNodeCSR(alloc.PoolNodeName("pa"))
	if err != nil {
		t.Fatal(err)
	}
	fp, err := wirecert.FingerprintOfCSR(csr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.RequestEnrollmentWithToken(ctx, alloc.PoolNodeName("pa"), fp, string(csr), tok); err != nil {
		t.Fatal(err)
	}
	closeDB()

	env, out := poolEnv()
	if err := Nodes(ctx, env, []string{"approve", "--config", cfgPath, "--fingerprint", fp, alloc.PoolNodeName("pa")}); err != nil {
		t.Fatalf("approve: %v\n%s", err, out)
	}

	a, closeDB, err = ControlPlaneAllocator(ctx, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	o, err := a.ReadPoolOrder(ctx, "pa")
	if err != nil {
		t.Fatal(err)
	}
	certs, err := a.LiveCertsFor(ctx, alloc.PoolNodeName("pa"))
	if err != nil || len(certs) != 1 {
		t.Fatalf("issued %+v, %v", certs, err)
	}
	if o.NodeName != alloc.PoolNodeName("pa") || o.NodeFingerprint != fp || o.NodeCertSerial != certs[0].Serial {
		t.Fatalf("the order after a person's approval: %+v, want linked with serial %s", o, certs[0].Serial)
	}
}

// NOTHING THE UNVERIFIED CONNECTION WROTE IS PRINTED: the fingerprint proves the
// authority and nothing beside it, so a deployment that is no deployment's id ends the
// join unprinted, and a status's reason phrase is never quoted.
func TestAJoinQuotesNothingTheUnverifiedAnswerWrote(t *testing.T) { //nolint:paralleltest // shared vars
	forged := "x\n" + joinconsole.Line("pa", realFP)
	rec, srv, cfg, ca, _ := servePoolJoin(t, func(j *joinRecorder) { j.deployment = forged })
	var out bytes.Buffer
	err := EnrollNode(bounded(t), cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "pa")
	if err == nil || !strings.Contains(err.Error(), "names no deployment") || strings.Contains(out.String()+err.Error(), "billet-pool-join") {
		t.Fatalf("a forged deployment answered %v, printing %q", err, out.String())
	}
	rec.mu.Lock()
	asked := len(rec.requests)
	rec.mu.Unlock()
	if asked != 0 {
		t.Fatalf("a forged deployment was asked to join %d times", asked)
	}

	_, srv, cfg, ca, _ = servePoolJoin(t, func(j *joinRecorder) { j.caFaults = []string{faultReason} })
	out.Reset()
	err = EnrollNode(bounded(t), cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "pa")
	if err == nil || !strings.Contains(err.Error(), "404") || strings.Contains(out.String()+err.Error(), secretReason) {
		t.Fatalf("a reason phrase answered %v, printing %q", err, out.String())
	}
}
