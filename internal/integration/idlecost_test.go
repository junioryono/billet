package integration_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
)

// WHAT AN IDLE CONTROL PLANE COSTS, MEASURED AGAINST A FAKE GITHUB (#556).
//
// docs/reference/records/idle-control-plane.md is what this produces. Fifty real
// `billet server` processes cannot share a real App: each holds six message
// sessions, and twenty at once is what the test App has been measured to carry.
// So scripts/measure-idle-control-planes.sh points the host's names for GitHub at
// this process, which speaks just enough of the protocol to hold a deployment
// idle, and it runs as a test binary because it is never billet.
//
// IT IS NOT EVIDENCE ABOUT GITHUB. It answers a poll after idlePark because that
// is how long GitHub held one when measured (72 polls an hour, 2026-10-21,
// docs/reference/records/assignment-protocol.md), and it gives each of GitHub's
// hosts its own name, so a connection's peer says which service it is for. It
// models no session expiry: what a killed control plane waits for at GitHub is
// measured against the real one.
const idlePark = 50 * time.Second

var (
	idleSetRoute     = regexp.MustCompile(`runnerscalesets/(\d+)(?:/(sessions)(?:/([^/]+))?)?/?$`)
	idleSetsRoute    = regexp.MustCompile(`runnerscalesets/?$`)
	idleTenantRoute  = regexp.MustCompile(`^/tenant/([^/]+)/`)
	idleInstallRoute = regexp.MustCompile(`^/orgs/([^/]+)/installation$`)
	idleGroupRoute   = regexp.MustCompile(`^/orgs/[^/]+/actions/runner-groups/\d+$`)
	idleQueueRoute   = regexp.MustCompile(`^/queue/(\d+)(?:/\d+)?$`)
)

// idleSet is one tier's scale set, and when its session last opened.
type idleSet struct {
	id     int
	org    string
	name   string
	group  string
	labels []string
	opened time.Time
}

// idleFake is the GitHub a measurement's control planes talk to. pipelines and
// broker are the base URLs it hands out for the Actions service and the message
// queue, which production serves from hosts of their own.
type idleFake struct {
	t         *testing.T
	key       *rsa.PrivateKey
	pipelines string
	broker    string
	park      time.Duration

	mu         sync.Mutex
	sets       map[int]*idleSet
	nextSet    int
	polls      int
	unexpected map[string]int
}

func newIdleFake(t *testing.T, pipelines, broker string, park time.Duration) *idleFake {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the admin token's key: %v", err)
	}

	return &idleFake{
		t: t, key: key, pipelines: pipelines, broker: broker, park: park,
		sets: map[int]*idleSet{}, nextSet: 100, unexpected: map[string]int{},
	}
}

// ServeHTTP answers the App handshake, the REST reads a starting control plane
// makes, the scale-set API and the long poll. Anything else is counted, so a
// driver can refuse a run that measured a deployment stuck on a missing route.
func (f *idleFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path

	switch {
	case strings.Contains(p, "/access_tokens"):
		w.WriteHeader(http.StatusCreated)
		fakeactions.WriteJSON(f.t, w, map[string]any{
			"token": "installation-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	case strings.HasSuffix(p, "/actions/runners/registration-token"):
		w.WriteHeader(http.StatusCreated)
		fakeactions.WriteJSON(f.t, w, map[string]any{
			"token": "registration-token", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	case p == "/actions/runner-registration" || strings.HasSuffix(p, "/actions/runner-registration"):
		f.register(w, r)
	case idleInstallRoute.MatchString(p):
		f.installation(w, idleInstallRoute.FindStringSubmatch(p)[1])
	case idleGroupRoute.MatchString(p):
		fakeactions.WriteJSON(f.t, w, map[string]any{
			"visibility": "all", "restricted_to_workflows": false, "selected_workflows": []string{},
		})
	case idleQueueRoute.MatchString(p):
		f.queue(w, r)
	case strings.Contains(p, "/runnergroups"):
		name := r.URL.Query().Get("groupName")
		fakeactions.WriteJSON(f.t, w, fakeactions.ListJSON(map[string]any{
			"id": 1, "name": name, "isDefaultGroup": name == scaleset.DefaultRunnerGroup,
		}))
	case strings.Contains(p, "/pools/0/agents"):
		fakeactions.WriteJSON(f.t, w, fakeactions.ListJSON())
	case idleSetsRoute.MatchString(p):
		f.scaleSets(w, r)
	case idleSetRoute.MatchString(p):
		f.scaleSet(w, r)
	default:
		f.refuse(w, r)
	}
}

func (f *idleFake) refuse(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.unexpected[r.Method+" "+r.URL.Path]++
	f.mu.Unlock()

	w.WriteHeader(http.StatusNotFound)
}

// register answers the Actions service's discovery with a tenant named for the
// organization, which is how every later request says whose it is.
func (f *idleFake) register(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode the runner registration: %v", err)
		w.WriteHeader(http.StatusBadRequest)

		return
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, &jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		Issuer:    "idle",
	})

	signed, err := token.SignedString(f.key)
	if err != nil {
		f.t.Errorf("sign the admin token: %v", err)
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	fakeactions.WriteJSON(f.t, w, map[string]any{
		"url": f.pipelines + "/tenant/" + path.Base(strings.TrimSuffix(body.URL, "/")) + "/", "token": signed,
	})
}

// installation answers `billet check`'s verification with billet's own
// permissions for an organization.
func (f *idleFake) installation(w http.ResponseWriter, org string) {
	inst := github.Installation{ID: 67890}
	inst.Account.Login, inst.Account.Type = org, "Organization"
	inst.Permissions = map[string]string{"metadata": "read", "organization_self_hosted_runners": "write"}

	fakeactions.WriteJSON(f.t, w, map[string]any{
		"id": inst.ID, "account": inst.Account, "permissions": inst.Permissions,
	})
}

func (f *idleFake) setJSON(s *idleSet) map[string]any {
	return fakeactions.ScaleSetJSON(s.id, s.name, s.group, s.labels...)
}

func (f *idleFake) scaleSets(w http.ResponseWriter, r *http.Request) {
	m := idleTenantRoute.FindStringSubmatch(r.URL.Path)
	if m == nil {
		f.refuse(w, r)

		return
	}

	org := m[1]

	switch r.Method {
	case http.MethodGet:
		name := r.URL.Query().Get("name")

		f.mu.Lock()
		values := []map[string]any{}

		for id := range f.sets {
			if s := f.sets[id]; s.org == org && (name == "" || s.name == name) {
				values = append(values, f.setJSON(s))
			}
		}
		f.mu.Unlock()

		fakeactions.WriteJSON(f.t, w, fakeactions.ListJSON(values...))
	case http.MethodPost:
		var body struct {
			Name   string `json:"name"`
			Group  string `json:"runnerGroupName"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode the scale-set create: %v", err)
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		group := body.Group
		if group == "" {
			group = scaleset.DefaultRunnerGroup
		}

		f.mu.Lock()
		s := &idleSet{id: f.nextSet, org: org, name: body.Name, group: group}
		for _, l := range body.Labels {
			s.labels = append(s.labels, l.Name)
		}

		f.sets[s.id] = s
		f.nextSet++
		out := f.setJSON(s)
		f.mu.Unlock()

		fakeactions.WriteJSON(f.t, w, out)
	default:
		f.refuse(w, r)
	}
}

func (f *idleFake) scaleSet(w http.ResponseWriter, r *http.Request) {
	m := idleSetRoute.FindStringSubmatch(r.URL.Path)

	id, err := strconv.Atoi(m[1])
	if err != nil {
		f.refuse(w, r)

		return
	}

	f.mu.Lock()
	s := f.sets[id]
	f.mu.Unlock()

	if s == nil {
		f.refuse(w, r)

		return
	}

	switch {
	case m[2] == "" && r.Method == http.MethodGet:
		fakeactions.WriteJSON(f.t, w, f.setJSON(s))
	case m[2] == "sessions" && m[3] == "" && r.Method == http.MethodPost:
		f.mu.Lock()
		s.opened = time.Now()
		f.mu.Unlock()

		fakeactions.WriteJSON(f.t, w, f.sessionJSON(s))
	case m[2] == "sessions" && m[3] != "" && r.Method == http.MethodPatch:
		fakeactions.WriteJSON(f.t, w, f.sessionJSON(s))
	case m[2] == "sessions" && m[3] != "" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		f.refuse(w, r)
	}
}

func (f *idleFake) sessionJSON(s *idleSet) map[string]any {
	return fakeactions.SessionJSON(fmt.Sprintf("3f8a1c22-0000-4000-8000-%012d", s.id), "billet-idle",
		f.setJSON(s), f.broker+"/queue/"+strconv.Itoa(s.id), "queue-token")
}

// queue parks a poll for the time GitHub holds one, and answers it with no
// message, or acknowledges one.
func (f *idleFake) queue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNoContent)

		return
	}

	f.mu.Lock()
	f.polls++
	f.mu.Unlock()

	timer := time.NewTimer(f.park)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-r.Context().Done():
	}

	w.WriteHeader(http.StatusAccepted)
}

// idleReport is what the driver reads from the fake: when each organization's
// sessions last opened, and every request it could not answer.
type idleReport struct {
	Polls      int                    `json:"polls"`
	Opened     map[string][]time.Time `json:"opened"`
	Unexpected map[string]int         `json:"unexpected"`
}

func (f *idleFake) report(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := idleReport{Polls: f.polls, Opened: map[string][]time.Time{}, Unexpected: f.unexpected}
	for _, s := range f.sets {
		if !s.opened.IsZero() {
			out.Opened[s.org] = append(out.Opened[s.org], s.opened)
		}
	}

	fakeactions.WriteJSON(f.t, w, out)
}

// idleCertificate is a throwaway authority's certificate for every host a
// control plane dials, with the authority's PEM for SSL_CERT_FILE.
func idleCertificate(t *testing.T, hosts []string) (tls.Certificate, []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the authority's key: %v", err)
	}

	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "billet idle measurement"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(0, 0, 7),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("sign the authority: %v", err)
	}

	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("read the authority back: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the leaf's key: %v", err)
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: hosts[0]}, DNSNames: hosts,
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(0, 0, 7),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("sign the leaf: %v", err)
	}

	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

// TestServeIdleFakeGitHub is the fake as a process, for the driver:
//
//	BILLET_MEASURE_IDLE_FAKE=:443 BILLET_MEASURE_IDLE_ADMIN=127.0.0.1:18080 \
//	BILLET_MEASURE_IDLE_CA=/srv/measure/ca.pem \
//	./integration.test -test.run '^TestServeIdleFakeGitHub$' -test.timeout 0
//
// It serves until SIGTERM. The report is on a plain listener of its own, so
// reading it adds nothing to the connections being counted.
func TestServeIdleFakeGitHub(t *testing.T) {
	addr := os.Getenv("BILLET_MEASURE_IDLE_FAKE")
	if addr == "" {
		t.Skip("BILLET_MEASURE_IDLE_FAKE is not set; scripts/measure-idle-control-planes.sh sets it")
	}

	cert, caPEM := idleCertificate(t, []string{
		"github.com", "api.github.com",
		"pipelines.actions.githubusercontent.com", "broker.actions.githubusercontent.com",
	})
	if err := os.WriteFile(os.Getenv("BILLET_MEASURE_IDLE_CA"), caPEM, 0o644); err != nil { //nolint:gosec // a throwaway authority's certificate, read by the measured processes
		t.Fatalf("write the authority: %v", err)
	}

	fake := newIdleFake(t, "https://pipelines.actions.githubusercontent.com",
		"https://broker.actions.githubusercontent.com", idlePark)

	ctx, stop := signal.NotifyContext(t.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}

	srv := &http.Server{
		Handler:           fake,
		ReadHeaderTimeout: time.Minute,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
	}

	go func() {
		if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	}()

	admin := &http.Server{
		Addr: os.Getenv("BILLET_MEASURE_IDLE_ADMIN"), Handler: http.HandlerFunc(fake.report),
		ReadHeaderTimeout: time.Minute,
	}

	go func() {
		if err := admin.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve the report: %v", err)
		}
	}()

	<-ctx.Done()

	if err := errors.Join(srv.Close(), admin.Close()); err != nil {
		t.Errorf("close: %v", err)
	}
}

// THE FAKE SPEAKS WHAT THE REAL CLIENT ASKS, so a client upgrade that moved a
// route fails here rather than as a measurement of a stuck deployment: the
// scale set is provisioned, its session opens, and the poll is answered empty.
func TestTheIdleFakeHoldsARealClientIdle(t *testing.T) {
	t.Parallel()

	fake := newIdleFake(t, "", "", time.Millisecond)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	fake.pipelines, fake.broker = srv.URL, srv.URL

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate the App's key: %v", err)
	}

	client, err := scaleset.New(scaleset.Config{
		Target:         github.OrganizationTarget("org-01"),
		GitHubURL:      srv.URL,
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
		})),
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	set, err := client.EnsureScaleSet(ctx, "idle-2vcpu", "", []string{"idle-2vcpu"})
	if err != nil {
		t.Fatalf("EnsureScaleSet: %v", err)
	}

	session, err := client.Session(ctx, set.ID, "billet-idle")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	if _, err := session.GetMessage(ctx, 0, 0); !errors.Is(err, server.ErrNoMessage) {
		t.Fatalf("an idle poll answered %v, want %v", err, server.ErrNoMessage)
	}

	if err := session.Close(ctx); err != nil {
		t.Fatalf("close the session: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.unexpected) != 0 {
		t.Fatalf("the client asked for what the fake does not answer: %v", fake.unexpected)
	}

	if fake.sets[set.ID].opened.IsZero() {
		t.Fatal("the session opened and the fake recorded no time for it")
	}
}
