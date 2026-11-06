package fleetops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
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
	want := "boot output\n" + nodeapi.PoolConsoleLine("pa", realFP) + "\n" + nodeapi.PoolConsoleLine("pa", realFP) + "\n"
	if string(raw) != want {
		t.Fatalf("the console holds %q, want %q", raw, want)
	}
	if got := nodeapi.PoolConsoleFingerprints(string(raw), "pa"); len(got) != 1 || got[0] != realFP {
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
}

func (j *joinRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/ca":
		_ = json.NewEncoder(w).Encode(nodeapi.CAResponse{CAPEM: string(j.ca.CertPEM()), Fingerprint: j.ca.Fingerprint(),
			Deployment: "0123456789abcdef0123456789abcdef"})
	case "/v1/enroll":
		var req nodeapi.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		raw, _ := os.ReadFile(j.console) //nolint:errcheck // an unread console is an empty one, which the test reads
		j.mu.Lock()
		j.requests = append(j.requests, req)
		j.consoles = append(j.consoles, string(raw))
		n := len(j.requests)
		j.mu.Unlock()
		fp, err := wirecert.FingerprintOfCSR([]byte(req.CSRPEM))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		if n < 2 || req.IdentityDocument == "" {
			_ = json.NewEncoder(w).Encode(nodeapi.EnrollResponse{State: "pending", Fingerprint: fp})

			return
		}
		bundle, err := j.ca.SignNodeCSR(req.Node, []byte(req.CSRPEM))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		_ = json.NewEncoder(w).Encode(nodeapi.EnrollResponse{State: "approved", Fingerprint: fp,
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
	srv := httptest.NewUnstartedServer(rec)
	srv.TLS = conf
	srv.StartTLS()
	t.Cleanup(srv.Close)

	wasConsole, wasIdentity, wasEvery := consolePath, instanceIdentity, enrollPollEvery
	t.Cleanup(func() { consolePath, instanceIdentity, enrollPollEvery = wasConsole, wasIdentity, wasEvery })
	consolePath, enrollPollEvery = console, 0
	reads := 0
	instanceIdentity = func(context.Context) ([]byte, string, error) {
		reads++
		if reads == 1 {
			return nil, "", errors.New("the metadata service is not answering yet")
		}

		return []byte(`{"instanceId":"i-1"}`), "c2ln", nil
	}

	tlsDir := filepath.Join(dir, "tls")
	cfg := &config.Config{Node: &config.NodeConfig{Name: "pool-pa", TLS: &config.NodeTLS{
		CertPath: filepath.Join(tlsDir, "node.crt"), KeyPath: filepath.Join(tlsDir, "node.key"), CAPath: filepath.Join(tlsDir, "ca.crt"),
	}}}
	var out bytes.Buffer
	if err := EnrollNode(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, cfg, srv.URL, ca.Fingerprint(), "tok", "pa"); err != nil {
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
		if got := nodeapi.PoolConsoleFingerprints(c, "pa"); len(got) != 1 || got[0] != fp {
			t.Fatalf("ask %d: the console named %v, want the key %s", i+1, got, fp)
		}
	}
	if rec.requests[0].IdentityDocument != "" || rec.requests[1].IdentityDocument != `{"instanceId":"i-1"}` ||
		rec.requests[1].IdentitySignature != "c2ln" || reads != 2 {
		t.Fatalf("identities sent %+v after %d reads", rec.requests, reads)
	}
	if _, err := os.Stat(cfg.Node.TLS.CertPath); err != nil {
		t.Fatalf("the certificate was not written: %v", err)
	}
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
