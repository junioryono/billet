package nodeclient_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
)

// registerStub is a control plane that answers /v1/register however a test needs
// and records what the node sent.
//
// HAND-ROLLED RATHER THAN THE REAL PLANE, because what is under test is how the
// node behaves against a control plane that is BROKEN or OLDER than it — states
// the real plane cannot be asked to produce, and states no test should have to
// build a deliberately wrong control plane to reach.
type registerStub struct {
	t *testing.T
	// status and body are what /v1/register answers.
	status int
	body   any

	got nodeapi.RegisterRequest
}

func (s *registerStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()

	if r.URL.Path != "/v1/register" {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	if err := json.NewDecoder(r.Body).Decode(&s.got); err != nil {
		s.t.Errorf("decode the registration: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.status)

	if err := json.NewEncoder(w).Encode(s.body); err != nil {
		s.t.Errorf("encode the answer: %v", err)
	}
}

func dialStub(t *testing.T, stub *registerStub) *nodeclient.Client {
	t.Helper()

	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	return c
}

func testRegistration() nodeclient.Registration {
	return nodeclient.Registration{
		Provider:   config.ProviderDocker,
		Deployment: "d",
		VCPU:       8,
		Memory:     32 * config.GiB,
	}
}

// THE NODE SENDS ITS RANGE AND ITS BUILD, because nothing downstream can invent
// them.
//
// The control plane records exactly what arrives here and `billet status` reports
// exactly what it recorded, so a field dropped at this end is a fleet that reads
// as converged while it is not — and there is no error anywhere along the way.
func TestTheNodeAnnouncesItsRangeAndItsRelease(t *testing.T) {
	t.Parallel()

	stub := &registerStub{
		t:      t,
		status: http.StatusOK,
		body: nodeapi.RegisterResponse{
			Version: nodeapi.Version, LeaseTTLSeconds: 60, PollSeconds: 30,
		},
	}

	c := dialStub(t, stub)

	if err := c.Register(t.Context(), testRegistration()); err != nil {
		t.Fatalf("register: %v", err)
	}

	if stub.got.Version != nodeapi.Version || stub.got.MinVersion != nodeapi.MinVersion {
		t.Errorf("the node announced protocol %d-%d, want %s",
			stub.got.MinVersion, stub.got.Version, nodeapi.Self())
	}

	// NOT AN EXACT STRING. It comes from the build, so naming one here would only
	// assert what the test had just been told; an EMPTY value is what a field that
	// never got filled in looks like, and that is the failure.
	if stub.got.Release == "" {
		t.Error("the node named no release, so a control plane cannot tell an operator " +
			"which build this host is running")
	}
}

// A CONTROL PLANE ANSWERING OUTSIDE THIS NODE'S RANGE IS BROKEN, NOT BUSY.
//
// Retrying cannot change what it answers, so this stops the node — an error the
// loop treats as an outage would leave a process that looks alive, never works,
// and crashes nothing that would draw attention.
func TestTheNodeRefusesAVersionItDoesNotSpeak(t *testing.T) {
	t.Parallel()

	stub := &registerStub{
		t:      t,
		status: http.StatusOK,
		body: nodeapi.RegisterResponse{
			Version: nodeapi.Version + 7, LeaseTTLSeconds: 60, PollSeconds: 30,
		},
	}

	c := dialStub(t, stub)

	err := c.Register(t.Context(), testRegistration())
	if err == nil {
		t.Fatal("the node accepted a protocol version it does not implement")
	}

	if !errors.Is(err, nodeclient.ErrRefused) {
		t.Errorf("the node treated a broken control plane as an outage it can retry: %v", err)
	}

	// VALIDATED BEFORE IT IS PUBLISHED, the same rule the TTL already follows.
	// Storing the timings and then reporting the error leaves a janitor already
	// running against a registration this node has just rejected.
	if got := c.LeaseTTL(); got != 0 {
		t.Errorf("a rejected registration published a lease TTL of %s", got)
	}

	if got := c.WireVersion(); got != 0 {
		t.Errorf("a rejected registration published protocol %d", got)
	}
}

// AN OLD CONTROL PLANE REJECTS THE SHAPE OF THIS BODY, AND SAYS SO IN JSON.
//
// It decodes requests with DisallowUnknownFields, so a newer node's registration
// dies in its decoder BEFORE any version check it has can run. What comes back is
// `json: unknown field "min_version"` — true, permanent, and unactionable on its
// own. The node's own range is what turns it into "upgrade the control plane
// first", and it is added unconditionally rather than matched out of the message,
// because a message is the one part of this a stranger's build controls.
func TestARefusedRegistrationNamesThisNodesRange(t *testing.T) {
	t.Parallel()

	stub := &registerStub{
		t:      t,
		status: http.StatusBadRequest,
		body: nodeapi.ErrorResponse{
			Code:    nodeapi.CodeRefused,
			Message: `json: unknown field "min_version"`,
		},
	}

	c := dialStub(t, stub)

	err := c.Register(t.Context(), testRegistration())
	if err == nil {
		t.Fatal("a rejected registration reported success")
	}

	if !errors.Is(err, nodeclient.ErrRefused) {
		t.Errorf("the refusal was not permanent, so the node retries an old control plane "+
			"forever: %v", err)
	}

	for _, want := range []string{
		nodeapi.Self().String(),
		strconv.Itoa(nodeapi.Version),
		`unknown field "min_version"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q, so it does not say which side to "+
				"upgrade: %v", want, err)
		}
	}
}
