package nodeplane

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// releasedProtocol is the wire a node built before this control plane speaks.
//
// TAKEN FROM MinVersion RATHER THAN WRITTEN AS 12, so that lifting the floor in a
// later release moves these tests with it instead of leaving them asserting a
// bridge that no longer exists.
const releasedProtocol = nodeapi.MinVersion

// recordingRegistrar answers everything and keeps the last registration, so a
// test can assert what the plane decided rather than only that it did not error.
//
// It JUDGES NOTHING. The negotiation it is used to test lives in the plane, and a
// fake that re-implemented any of it would be a test of the fake.
type recordingRegistrar struct {
	countingRegistrar

	mu sync.Mutex
	// epochs are handed out in order, so a test can stage a registration that
	// arrives after a newer one has already installed its epoch.
	epochs []int64
	last   alloc.NodeRegistration
	calls  int
}

func (r *recordingRegistrar) RegisterNode(
	_ context.Context, reg alloc.NodeRegistration,
) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.last = reg
	r.calls++

	if len(r.epochs) > 0 {
		epoch := r.epochs[0]
		r.epochs = r.epochs[1:]

		return epoch, nil
	}

	return int64(r.calls), nil
}

func (r *recordingRegistrar) recorded() alloc.NodeRegistration {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.last
}

func (r *recordingRegistrar) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.calls
}

// releasedNode is the registration a node built before the wire became a range
// sends: one version, no floor, no release.
func releasedNode() nodeapi.RegisterRequest {
	return nodeapi.RegisterRequest{
		Version:     releasedProtocol,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "n1-1",
	}
}

// A PAIR WITH NO OVERLAP IS REFUSED, AND THE REFUSAL NAMES WHICH SIDE TO UPGRADE.
//
// Both directions, because they need opposite actions and the operator reading
// the message has only one of the two builds in front of them. "Upgrade whichever
// is older" — what this used to say — is not something a person can act on
// without going and finding out what the other end speaks.
func TestAWireRangeWithNoOverlapIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		minVersion int
		version    int
		wantAdvice string
	}{
		{
			name:       "a node older than the oldest protocol still supported",
			minVersion: 1,
			version:    nodeapi.MinVersion - 1,
			wantAdvice: "UPGRADE THAT NODE",
		},
		{
			name:       "a node rolled ahead of its control plane",
			minVersion: nodeapi.Version + 1,
			version:    nodeapi.Version + 2,
			wantAdvice: "UPGRADE THIS CONTROL PLANE FIRST",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := testPlane(t)

			_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
				Version:    tc.version,
				MinVersion: tc.minVersion,
				Node:       "n1",
				Provider:   config.ProviderDocker,
				Deployment: deployment,
				VCPU:       8,
				Memory:     32 * config.GiB,
			})
			if err == nil {
				t.Fatalf("a node speaking %d-%d was accepted by a control plane speaking %s",
					tc.minVersion, tc.version, nodeapi.Self())
			}

			// PERMANENT, which is the half that decides what the node does next. A
			// node retries anything that might heal; a range that does not overlap
			// cannot, so a refusal reported as an outage leaves it reconnecting
			// forever instead of telling somebody to upgrade something.
			if !errors.Is(err, ErrRefused) {
				t.Errorf("the refusal was not permanent: %v", err)
			}

			if !strings.Contains(err.Error(), tc.wantAdvice) {
				t.Errorf("the refusal does not say %q, so it does not name which side to "+
					"upgrade: %v", tc.wantAdvice, err)
			}

			// BOTH RANGES, because the advice above is only checkable against them.
			declared, ok := nodeapi.DeclaredRange(tc.minVersion, tc.version)
			if !ok {
				t.Fatalf("this case is meant to declare a real range, and %d-%d is not one",
					tc.minVersion, tc.version)
			}

			for _, want := range []string{declared.String(), nodeapi.Self().String()} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name the range %s: %v", want, err)
				}
			}
		})
	}
}

// A DECLARATION THAT IS NOT A RANGE IS REFUSED BEFORE ANY SIDE EFFECT.
//
// Normalising it — which the first version did — settles on a version the peer
// has just said it does not implement, and everything registration does after
// that point is a side effect on the fleet: the ledger epoch, the supersession
// of whatever process held the name, the inventory snapshot. So this asserts
// what did NOT happen as well as the error, because an error value is the
// cheapest thing a function produces and usually the least of what it does.
func TestADeclarationThatIsNotARangeIsRefusedBeforeAnythingHappens(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		minVersion int
		version    int
	}{
		{"a floor above the newest", nodeapi.Version + 1, nodeapi.MinVersion},
		{"a negative floor", -3, nodeapi.Version},
		{"no version at all", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := &recordingRegistrar{}
			p := testPlane(t, WithRegistrar(reg))

			// A LIVE REGISTRATION FIRST, VOUCHING FOR ITS INVENTORY, so the
			// assertions below are about this request rather than about an empty
			// plane. The inventory is the part the first version of this test
			// missed: `releasedNode` carries none, so nothing could observe that a
			// refusal had cleared it.
			live := releasedNode()
			live.InventoryKnown = true
			live.Instances = []string{"l1"}

			if _, err := p.Register(t.Context(), live); err != nil {
				t.Fatalf("seed register: %v", err)
			}

			if !p.reconciledNode("n1") {
				t.Fatal("the seed registration did not vouch for its inventory, so this test " +
					"cannot observe a refusal clearing it")
			}

			calls := reg.callCount()

			_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
				Version:     tc.version,
				MinVersion:  tc.minVersion,
				Node:        "n1",
				Provider:    config.ProviderDocker,
				Deployment:  deployment,
				VCPU:        8,
				Memory:      32 * config.GiB,
				Incarnation: "impostor",
			})
			if err == nil {
				t.Fatalf("a node declaring minimum %d and newest %d was accepted",
					tc.minVersion, tc.version)
			}

			if !errors.Is(err, ErrRefused) {
				t.Errorf("the refusal was not permanent: %v", err)
			}

			// AND IT SAYS WHAT IS ACTUALLY WRONG. Deleting the guard leaves the
			// request refused ANYWAY — an empty range fails the overlap check —
			// so "an error came back" is a test that agrees with the bug. What is
			// lost is the diagnostic: the operator of a client sending a broken
			// declaration is told to upgrade a node whose build is not old.
			if !strings.Contains(err.Error(), "not a range") {
				t.Errorf("the refusal does not say the declaration is the problem: %v", err)
			}

			if strings.Contains(err.Error(), "UPGRADE THAT NODE") {
				t.Errorf("a node with a contradictory declaration is told its build is too "+
					"old, which is not what is wrong with it: %v", err)
			}

			// NOTHING REACHED THE LEDGER, so no epoch moved and no quarantine was
			// resolved against a host this plane cannot describe.
			if got := reg.callCount(); got != calls {
				t.Errorf("the ledger was written %d more time(s) for a registration that "+
					"was refused", got-calls)
			}

			// AND THE PROCESS THAT WAS ALREADY REGISTERED STILL HOLDS THE NAME.
			// Supersession is what resolves in-flight commands and hands custody
			// over; a refused registration must not have triggered it.
			if _, _, err := p.Poll(t.Context(), "n1", "n1-1"); errors.Is(err, ErrSuperseded) {
				t.Error("a refused registration superseded the process that held the name")
			}

			// AND ITS INVENTORY IS STILL VOUCHED FOR. beginRegistration clears it
			// and only a SUCCESSFUL registration puts it back, so a refusal that
			// reached that far leaves a live host's inventory unknown until it
			// registers again — and a completion may only settle a lease from
			// absence while it is known. That is capacity staying charged for
			// compute that is provably gone, caused by a request billet rejected.
			if !p.reconciledNode("n1") {
				t.Error("a refused registration cleared the live host's inventory; until it " +
					"registers again no completion can settle a lease from absence")
			}
		})
	}
}

// THE RELEASED PROTOCOL STILL REGISTERS, WHICH IS THE WHOLE POINT.
//
// Every node in the field speaks MinVersion and knows nothing about a range. An
// equality check refuses all of them the instant the control plane is replaced —
// permanently, because a refusal is not something a node retries — so upgrading
// the control plane would take the fleet down until every host was replaced in
// the same maintenance window.
func TestANodeOnTheReleasedProtocolStillRegisters(t *testing.T) {
	t.Parallel()

	reg := &recordingRegistrar{}
	p := testPlane(t, WithRegistrar(reg))

	res, err := p.Register(t.Context(), releasedNode())
	if err != nil {
		t.Fatalf("a node on the released protocol was refused: %v", err)
	}

	if res.Version != releasedProtocol {
		t.Errorf("the control plane answered a %d node with version %d; it must serve the "+
			"version they agreed on, not its own preference",
			releasedProtocol, res.Version)
	}

	if res.MinVersion != nodeapi.MinVersion || res.MaxVersion != nodeapi.Version {
		t.Errorf("the response reports the control plane speaks %d-%d, want %s",
			res.MinVersion, res.MaxVersion, nodeapi.Self())
	}

	got := reg.recorded()

	if got.WireVersion != releasedProtocol {
		t.Errorf("the ledger recorded protocol %d, want %d", got.WireVersion, releasedProtocol)
	}

	// NOT ZERO. A build that declared no floor implements exactly the one version
	// it named, and recording a floor of zero says this host is holding open every
	// version back to nothing — which is read straight off by the report that
	// decides when an old protocol may be retired.
	if got.WireMin != releasedProtocol || got.WireMax != releasedProtocol {
		t.Errorf("the ledger recorded the node's range as %d-%d, want %d-%d",
			got.WireMin, got.WireMax, releasedProtocol, releasedProtocol)
	}

	if got.Release != "" {
		t.Errorf("a node that named no release was recorded as %q", got.Release)
	}
}

// THE HIGHEST BOTH SPEAK, so a converged fleet actually moves off the old wire.
func TestTheNegotiatedVersionIsTheHighestBothSpeak(t *testing.T) {
	t.Parallel()

	reg := &recordingRegistrar{}
	p := testPlane(t, WithRegistrar(reg))

	res, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		MinVersion: nodeapi.MinVersion,
		Release:    "v9.9.9",
		Node:       "n1",
		Provider:   config.ProviderDocker,
		Deployment: deployment,
		VCPU:       8,
		Memory:     32 * config.GiB,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if res.Version != nodeapi.Version {
		t.Errorf("two builds that both speak %d negotiated %d", nodeapi.Version, res.Version)
	}

	if got := reg.recorded(); got.Release != "v9.9.9" {
		t.Errorf("the ledger recorded release %q, want v9.9.9", got.Release)
	}
}

// A MISSING RELEASE IS RECORDED, NOT REFUSED.
//
// It decides nothing about capacity, fencing, identity, custody or destruction —
// it is what `billet status` names when it says which hosts to upgrade — so
// taking a working machine out of the fleet over it would cost far more than the
// field is worth. What the ledger must not do is invent one: an empty release is
// stored empty, and the version beside it is what lets the report tell an old
// build with none to give from a current one that owes it.
func TestAMissingReleaseIsRecordedRatherThanRefused(t *testing.T) {
	t.Parallel()

	reg := &recordingRegistrar{}
	p := testPlane(t, WithRegistrar(reg))

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		MinVersion: nodeapi.MinVersion,
		Node:       "n1",
		Provider:   config.ProviderDocker,
		Deployment: deployment,
		VCPU:       8,
		Memory:     32 * config.GiB,
	}); err != nil {
		t.Fatalf("a node that named no release was refused: %v", err)
	}

	got := reg.recorded()

	if got.Release != "" {
		t.Errorf("the ledger invented release %q for a node that named none", got.Release)
	}

	if got.WireVersion != nodeapi.Version {
		t.Errorf("the ledger recorded protocol %d, want %d; without it the report cannot "+
			"tell a build that owes a release from one that has none",
			got.WireVersion, nodeapi.Version)
	}
}

// A NODE CANNOT WRITE ITS OWN ROW INTO THE OPERATOR'S REPORT.
//
// The release is the one free-form string a node puts into `billet status`, and
// a node is authenticated without being trusted to behave. Stored verbatim it is
// a newline away from forging a whole line of that report — a laggard rendering
// itself, or another host, as converged, in the exact output an operator reads
// to decide whether a protocol is safe to retire.
func TestANodesReleaseCannotForgeTheOperatorsReport(t *testing.T) {
	t.Parallel()

	reg := &recordingRegistrar{}
	p := testPlane(t, WithRegistrar(reg))

	forged := "v1.0.0\n          ghost-node    protocol 13  v9.9.9\x1b[2J"

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		MinVersion: nodeapi.MinVersion,
		Release:    forged,
		Node:       "n1",
		Provider:   config.ProviderDocker,
		Deployment: deployment,
		VCPU:       8,
		Memory:     32 * config.GiB,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	assertSafeRelease(t, reg.recorded().Release)

	// AND THE HONEST PREFIX IS STILL THERE, so a sanitiser that returned a
	// constant — which would pass every assertion below — does not.
	if got := reg.recorded().Release; !strings.HasPrefix(got, "v1.0.0") {
		t.Errorf("the real version was lost while sanitising: %q", got)
	}
}

// assertSafeRelease pins the output to the production allowlist, byte by byte.
//
// NAMING THE CHARACTERS THAT MUST NOT SURVIVE IS THE WEAK VERSION, and it was
// what this test first did: checking for newline, carriage return and ESC leaves
// a regression that admits tab, backspace, BEL, DEL, the C1 controls or a bidi
// override passing cleanly, every one of which can still rewrite what an
// operator sees. An allowlist has no such gap — anything the production rule
// does not name is a failure here without this test having to have thought of it.
func assertSafeRelease(t *testing.T, got string) {
	t.Helper()

	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-+_()?"

	for i := range len(got) {
		if !strings.ContainsRune(allowed, rune(got[i])) {
			t.Errorf("byte %d of the stored release is %q, which is not in the allowlist: %q",
				i, got[i], got)
		}
	}

	if len(got) > maxRelease {
		t.Errorf("the release reached the ledger at %d bytes, over the %d-byte bound",
			len(got), maxRelease)
	}
}

// EVERY HOSTILE SHAPE, INCLUDING THE ONE THE TRUNCATION ITSELF CREATES.
//
// safeRelease bounds by BYTES and then ranges by RUNE, so a multi-byte sequence
// straddling the bound is left invalid. That is safe — an invalid byte ranges as
// RuneError and falls to the default arm — but it is safe by consequence rather
// than by construction, which is exactly the kind of thing that stops being true
// when somebody reorders two lines.
func TestSafeReleaseAdmitsNothingOutsideItsAllowlist(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"a newline forging a row", "v1\n          ghost  protocol 13"},
		{"an ANSI escape", "v1\x1b[2Jv2"},
		{"a carriage return overwriting the line", "v1\rgone"},
		{"a tab realigning the columns", "v1\tghost"},
		{"a backspace", "v1\bv2"},
		{"a bell", "v1\av2"},
		{"DEL and the C1 controls", "v1\u007f\u0085\u009bv2"},
		{"a right-to-left override", "v1\u202ev2"},
		{"a zero-width space", "v1\u200bv2"},
		{"a NUL", "v1\x00v2"},
		{"multi-byte runes straddling the byte bound",
			strings.Repeat("a", maxRelease-1) + strings.Repeat("é", 8)},
		{"far longer than the bound", strings.Repeat("v1.2.3-", 200)},
		{"nothing at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assertSafeRelease(t, safeRelease(tc.raw))
		})
	}
}

// AND AN ORDINARY VERSION SURVIVES INTACT, so none of the above is satisfied by
// a sanitiser that simply throws everything away.
func TestSafeReleaseKeepsARealVersion(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"v0.3.26",
		"v1.2.3-rc.1+build.5",
		"(devel)",
		"(unknown)",
		"v1.2.3_local",
	} {
		if got := safeRelease(want); got != want {
			t.Errorf("safeRelease(%q) = %q; a real version must survive unchanged", want, got)
		}
	}
}

// EVERY RETURN PATH ANSWERS THE NEGOTIATED VERSION, including the early one.
//
// A registration overtaken by a newer one from the same node returns before
// anything else runs, and it used to answer this control plane's own preference.
// That tells a node it agreed to a version it does not implement, on the one path
// where nothing further happens that could correct it.
func TestAnOvertakenRegistrationStillAnswersTheNegotiatedVersion(t *testing.T) {
	t.Parallel()

	// The second registration is handed a LOWER epoch than the first, which is
	// what "a newer registration got there first" looks like from here.
	reg := &recordingRegistrar{epochs: []int64{5, 2}}
	p := testPlane(t, WithRegistrar(reg))

	if _, err := p.Register(t.Context(), releasedNode()); err != nil {
		t.Fatalf("first register: %v", err)
	}

	res, err := p.Register(t.Context(), releasedNode())
	if err != nil {
		t.Fatalf("overtaken register: %v", err)
	}

	if res.Version != releasedProtocol {
		t.Errorf("an overtaken registration answered version %d, want the negotiated %d",
			res.Version, releasedProtocol)
	}
}

// AND THE BRIDGE ACTUALLY CARRIES WORK, which is the claim the rest of this file
// only sets up.
//
// A node on the released protocol has to keep doing everything it did before: be
// chosen, take a launch, report it, own its lease, and be destroyed. Proving the
// registration is accepted proves only that the fleet reconnects; a rollout is
// not safe until the accepted node still runs jobs.
func TestAReleasedProtocolNodeStillLaunchesAndDestroys(t *testing.T) {
	p := testPlane(t, WithCommandTimeout(5*time.Second), WithRegistrar(&recordingRegistrar{}))

	if _, err := p.Register(t.Context(), releasedNode()); err != nil {
		t.Fatalf("register: %v", err)
	}

	lease := testLease()
	launched := make(chan error, 1)

	go func() { launched <- p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}) }()

	cmd, took, err := p.Poll(t.Context(), "n1", "n1-1")
	if err != nil || !took {
		t.Fatalf("a node on protocol %d was never given the launch: took=%v err=%v",
			releasedProtocol, took, err)
	}

	if cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("dispatched %q, want a launch", cmd.Kind)
	}

	if err := p.Result("n1", "n1-1",
		nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("report the launch: %v", err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}

	if lease.Node != "n1" {
		t.Fatalf("the launch bound the lease to %q, want n1", lease.Node)
	}

	if !p.OwnsForTest("l1", "n1", "n1-1") {
		t.Fatal("the launch recorded no owner, so nothing can end this lease")
	}

	destroyed := make(chan error, 1)

	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 7) }()

	answerOneCommand(t, p, "n1-1")

	select {
	case err := <-destroyed:
		if err != nil {
			t.Fatalf("destroy: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the destroy never returned")
	}

	if p.OwnsForTest("l1", "n1", "n1-1") {
		t.Error("ownership outlived the compute it described")
	}
}
