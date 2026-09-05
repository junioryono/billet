package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

// THE SEAM NOTHING ELSE COVERS: the node runtime driving the CodeBuild backend.
//
// The codebuild package is tested thoroughly on its own against a fake API, and the
// allocator's placement is tested on its own against every provider kind. Neither can
// catch a mistake in how the two are WIRED — which is where this project's worst
// defect so far lived, the launch path agreeing with itself while sending the wrong
// thing.
//
// It needs no Docker, because the compute it launches is a fake HTTP endpoint rather
// than a daemon on this machine.
const codeBuildTier = "billet-4vcpu-codebuild"

// fakeCodeBuild is a CodeBuild and Parameter Store that records what it was asked.
type fakeCodeBuild struct {
	*httptest.Server

	t *testing.T

	mu sync.Mutex
	// builds is the fake's world, keyed by build id.
	builds map[string]*cbBuild
	// params is Parameter Store, keyed by name, and paramWritten is when each was
	// written, on the clock the stack hands the fake.
	params       map[string]string
	paramWritten map[string]time.Time
	// clock is what PutParameter stamps a write with; time.Now until a stack
	// replaces it with its steerable clock.
	clock func() time.Time
	// stopped records every id handed to StopBuild — the assertion a drain turns on.
	stopped []string
	// targets is every X-Amz-Target the client sent, so a test can prove that a
	// path did NOT call something.
	targets []string
	// unconfirmedStops keeps a stopped build in STOPPING for as long as anybody
	// looks, so a teardown runs out of polls and hands the question to custody —
	// the state a killed holder leaves behind.
	unconfirmedStops bool
	// ambiguousStarts makes StartBuild start the build and answer without an id,
	// which is the one success billet has to treat as a failure it cannot name.
	ambiguousStarts bool

	next int
}

type cbBuild struct {
	id      string
	status  string
	phase   string
	started time.Time
	env     map[string]string
}

func newFakeCodeBuild(t *testing.T) *fakeCodeBuild {
	t.Helper()

	f := &fakeCodeBuild{
		t:            t,
		builds:       map[string]*cbBuild{},
		params:       map[string]string{},
		paramWritten: map[string]time.Time{},
		clock:        time.Now,
		next:         1,
	}

	f.Server = httptest.NewServer(f)
	t.Cleanup(f.Close)

	return f
}

func (f *fakeCodeBuild) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.Header.Get("X-Amz-Target")

	// A BODY THE FAKE CANNOT READ IS REPORTED, not skipped. Every one of these
	// targets is driven by the request's own fields, so a decode failure would
	// otherwise surface as a build with no lease name — a wiring bug wearing the
	// costume of a scheduling one.
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		f.t.Errorf("decode a %s request body: %v", target, err)
		f.writeError(w, "SerializationException")

		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.targets = append(f.targets, target)

	switch {
	case strings.HasSuffix(target, ".StartBuild"):
		id := "billet-cb:" + string(rune('a'+f.next-1))
		f.next++

		b := &cbBuild{
			id: id, status: "IN_PROGRESS", phase: "BUILD",
			started: time.Now(), env: map[string]string{},
		}

		if raw, ok := in["environmentVariablesOverride"].([]any); ok {
			for _, e := range raw {
				m, ok := e.(map[string]any)
				if !ok {
					continue
				}

				b.env[cbString(m["name"])] = cbString(m["value"])
			}
		}

		f.builds[id] = b

		if f.ambiguousStarts {
			f.writeJSON(w, map[string]any{"build": map[string]any{}})

			return
		}

		f.writeJSON(w, map[string]any{"build": f.describe(b)})

	case strings.HasSuffix(target, ".StopBuild"):
		id := cbString(in["id"])
		f.stopped = append(f.stopped, id)

		if b, ok := f.builds[id]; ok {
			// A STOP IS A REQUEST. The fake models exactly that, so a Destroy that
			// claimed confirmation without polling would be visible.
			b.status = "STOPPING"
			f.writeJSON(w, map[string]any{"build": f.describe(b)})

			return
		}

		f.writeError(w, "ResourceNotFoundException")

	case strings.HasSuffix(target, ".BatchGetBuilds"):
		var found []map[string]any
		var missing []string

		if raw, ok := in["ids"].([]any); ok {
			for _, v := range raw {
				id := cbString(v)

				b, ok := f.builds[id]
				if !ok {
					missing = append(missing, id)

					continue
				}

				if b.status == "STOPPING" && !f.unconfirmedStops {
					b.status = "STOPPED"
					found = append(found, f.describeWith(b, "STOPPING"))

					continue
				}

				found = append(found, f.describe(b))
			}
		}

		f.writeJSON(w, map[string]any{"builds": found, "buildsNotFound": missing})

	case strings.HasSuffix(target, ".ListBuildsForProject"):
		ids := make([]string, 0, len(f.builds))
		for id := range f.builds {
			ids = append(ids, id)
		}

		f.writeJSON(w, map[string]any{"ids": ids})

	case strings.HasSuffix(target, ".PutParameter"):
		f.params[cbString(in["Name"])] = cbString(in["Value"])
		f.paramWritten[cbString(in["Name"])] = f.clock()

		f.writeJSON(w, map[string]any{"Version": 1})

	case strings.HasSuffix(target, ".DeleteParameter"):
		name := cbString(in["Name"])
		if _, ok := f.params[name]; !ok {
			f.writeError(w, "ParameterNotFound")

			return
		}

		delete(f.params, name)
		f.writeJSON(w, map[string]any{})

	case strings.HasSuffix(target, ".GetParametersByPath"):
		// THE VALUE IS THE CIPHERTEXT AWS RETURNS WITHOUT DECRYPTION, and a sweep
		// that asked for decryption is a test failure rather than a service the fake
		// obliges: the control plane must never hold a registration.
		if decrypt, ok := in["WithDecryption"].(bool); !ok || decrypt {
			f.t.Errorf("the sweep asked Parameter Store with WithDecryption=%v", in["WithDecryption"])
		}

		prefix := strings.TrimSuffix(cbString(in["Path"]), "/") + "/"

		var names []string

		for name := range f.params {
			if rel, ok := strings.CutPrefix(name, prefix); ok && rel != "" && !strings.Contains(rel, "/") {
				names = append(names, name)
			}
		}

		sort.Strings(names)

		params := make([]map[string]any, 0, len(names))
		for _, name := range names {
			params = append(params, map[string]any{
				"Name": name, "Type": "SecureString", "Value": "AQICAHh-CIPHERTEXT",
				// AWS's own write time, on the same steerable clock as the ledger so
				// the test's "past the window" moves both proofs together.
				"LastModifiedDate": float64(f.paramWritten[name].UnixNano()) / 1e9,
			})
		}

		f.writeJSON(w, map[string]any{"Parameters": params})

	default:
		f.writeError(w, "InvalidAction")
	}
}

// describe renders one build. THE PARAMETER_STORE VARIABLE IS ECHOED AS ITS NAME,
// which is what AWS documents and what the whole secret channel depends on — a fake
// that echoed the value would make the leak assertion pass against a service that
// leaks.
func (f *fakeCodeBuild) describe(b *cbBuild) map[string]any {
	return f.describeWith(b, b.status)
}

func (f *fakeCodeBuild) describeWith(b *cbBuild, status string) map[string]any {
	env := make([]map[string]any, 0, len(b.env))
	for name, value := range b.env {
		kind := "PLAINTEXT"
		if name == "ACTIONS_RUNNER_INPUT_JITCONFIG" {
			kind = "PARAMETER_STORE"
		}

		env = append(env, map[string]any{"name": name, "value": value, "type": kind})
	}

	return map[string]any{
		"id":            b.id,
		"buildStatus":   status,
		"buildComplete": status != "IN_PROGRESS" && status != "STOPPING",
		"currentPhase":  b.phase,
		"startTime":     float64(b.started.Unix()),
		"environment":   map[string]any{"environmentVariables": env},
	}
}

func (f *fakeCodeBuild) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")

	body, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	f.write(w, body)
}

func (f *fakeCodeBuild) writeError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(http.StatusBadRequest)
	f.write(w, []byte(`{"__type":"`+code+`","message":"scripted"}`))
}

// write answers, and REPORTS a failure to answer.
//
// A fake that silently writes nothing makes the client's own timeout the observed
// behaviour, so an assertion about a refusal passes for the wrong reason.
func (f *fakeCodeBuild) write(w http.ResponseWriter, body []byte) {
	if _, err := w.Write(body); err != nil {
		f.t.Errorf("write a response: %v", err)
	}
}

// cbString reads a request field the fake expects to be a string, naming the missing
// case rather than leaving it to comma-ok's zero value.
func cbString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}

	return s
}

// neverConfirmStops makes every StopBuild a request CodeBuild never reports
// terminal, which is what a teardown looks like from a node that then dies.
func (f *fakeCodeBuild) neverConfirmStops() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.unconfirmedStops = true
}

// startAmbiguously makes every StartBuild commit a build and lose its id, so the
// launch fails after something has started.
func (f *fakeCodeBuild) startAmbiguously() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ambiguousStarts = true
}

// finish ends a build the way the runner exiting does: on its own, succeeded,
// with a terminal status billet did not ask for.
func (f *fakeCodeBuild) finish(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if b, ok := f.builds[id]; ok {
		b.status = "SUCCEEDED"
	}
}

func (f *fakeCodeBuild) idOf(leaseName string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for id, b := range f.builds {
		if b.env["BILLET_INSTANCE_NAME"] == leaseName {
			return id, true
		}
	}

	return "", false
}

func (f *fakeCodeBuild) stoppedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.stopped...)
}

func (f *fakeCodeBuild) parameterCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.params)
}

func (f *fakeCodeBuild) sentTargets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.targets...)
}

// cbCreds is a static credential source for the e2e stack.
type cbCreds struct{}

func (cbCreds) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{AccessKeyID: "AKID", SecretAccessKey: "s"}, nil
}

// cbJIT mints registrations the same way the cloud stack's does.
type cbJIT struct {
	mu     sync.Mutex
	minted int
}

type cbRegistration struct{ name string }

func (r cbRegistration) Config() string     { return "jit-for-" + r.name }
func (r cbRegistration) RunnerName() string { return r.name }
func (r cbRegistration) ID() int64          { return 83 }

func (*cbJIT) Describe(context.Context, string, string) (*node.Set, []string, error) {
	return &node.Set{ID: 9, Name: codeBuildTier}, nil, nil
}

func (j *cbJIT) JITConfig(
	_ context.Context, _ int, runnerName, _ string,
) (node.Registration, error) {
	j.mu.Lock()
	j.minted++
	j.mu.Unlock()

	return cbRegistration{name: runnerName}, nil
}

func (*cbJIT) ValidateTrustedRunnerGroup(context.Context, string, string, []string) error { return nil }
func (*cbJIT) RemoveRunner(context.Context, string, int64, string) error                  { return nil }
func (*cbJIT) EnsureRunnerRemoved(context.Context, string) error                          { return nil }
func (*cbJIT) RecoverRunner(
	context.Context, string, string, int64, string,
) (node.RunnerRecovery, error) {
	return node.RunnerRecoveryTracked, nil
}

// The incarnations the CodeBuild stack's node processes register with.
const (
	firstProcess  = "process-one"
	secondProcess = "process-two"
)

// codeBuildStack is a control-plane ledger and a node runtime over this backend.
type codeBuildStack struct {
	alloc  *alloc.Allocator
	runner *node.Runner
	fake   *fakeCodeBuild
	tier   config.Tier
	host   string
	jit    *cbJIT
	// clock is the allocator's clock when the stack was built on one, so a test
	// can age a lease past its TTL, past the quarantine grace, or past the
	// registration sweep's service window without waiting them out. Nil for a
	// stack on the real clock.
	clock *offsetClock
}

// cbStackOpt varies how a CodeBuild stack is assembled.
type cbStackOpt func(*cbStackConfig)

type cbStackConfig struct {
	clock *offsetClock
	ttl   time.Duration
}

// cbOnClock builds the stack's allocator on a clock the test moves and a lease
// TTL short enough that "past its TTL" is one advance rather than a wait.
func cbOnClock(clock *offsetClock, ttl time.Duration) cbStackOpt {
	return func(c *cbStackConfig) { c.clock, c.ttl = clock, ttl }
}

func newCodeBuildStack(t *testing.T, opts ...cbStackOpt) *codeBuildStack {
	t.Helper()

	var sc cbStackConfig
	for _, o := range opts {
		o(&sc)
	}

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tier := config.Tier{
		Label:    codeBuildTier,
		Provider: config.ProviderCodeBuild,
		VCPU:     4,
		Memory:   7 * config.GiB,
		Image:    "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		GuestOS:  config.GuestLinux,
		Command:  []string{"./run.sh"},
		// TRUSTED, because untrusted work is REFUSED on this backend — a reserved
		// instance stays alive between builds and shares cached state across
		// projects. There is a separate test for that refusal.
		Trust: config.WorkloadTrusted,
		Workflows: []string{
			"acme/cloud/.github/workflows/ci.yml@refs/heads/main",
		},
		RunnerGroup: "trusted",
	}

	var allocOpts []alloc.Option
	if sc.clock != nil {
		allocOpts = append(allocOpts, alloc.WithClock(sc.clock.now))
	}
	if sc.ttl > 0 {
		allocOpts = append(allocOpts, alloc.WithLeaseTTL(sc.ttl))
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier}, allocOpts...)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const host = "aws-cb-1"

	shapes := []config.RemoteShape{
		{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 10000},
	}

	// WHAT THE NODE CONTRIBUTES IS A BUDGET, not this machine's hardware — the whole
	// reason a remote node has to declare it. And the FLEET is on the registration
	// because its capacity is shared: the ledger refuses a second live node naming it.
	// The registration names the PROCESS, as a node on the wire does, so a lease
	// can record who holds it and a restart can be told from a replacement. The
	// PATH is there too, so the control plane knows where to sweep.
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: host, Provider: config.ProviderCodeBuild,
		VCPU: 64, Memory: 256 * config.GiB, EC2Shapes: shapes,
		Incarnation:      firstProcess,
		CodeBuildJITPath: cbParameterPath, CodeBuildRegion: "us-west-2",
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	fake := newFakeCodeBuild(t)

	// THE FAKE'S PARAMETER STORE STAMPS WRITES WITH THE STACK'S CLOCK when there is
	// one, so a test that advances the ledger past the sweep's window advances the
	// parameter's own age with it — both proofs move together, as they would on a
	// real clock.
	if sc.clock != nil {
		fake.clock = sc.clock.now
	}

	jit := &cbJIT{}

	s := &codeBuildStack{
		alloc: a,
		fake:  fake,
		tier:  tier,
		host:  host,
		jit:   jit,
		clock: sc.clock,
	}
	s.runner = node.New(a, host, jit, s.provider(t), nil)

	return s
}

// cbParameterPath is where the stack's node stages registrations, and where the
// control plane's sweep looks.
const cbParameterPath = "/billet/e2e/jit"

// sweeper is the control plane's sweep over this stack's path, authorised by this
// stack's ledger — the same pairing cmd/billet makes.
func (s *codeBuildStack) sweeper(t *testing.T) (*codebuild.RegistrationSweeper, codebuild.ClosureLookup) {
	t.Helper()

	sw, err := codebuild.NewRegistrationSweeper("us-west-2", cbParameterPath, cbCreds{},
		codebuild.SweepWithHTTPClient(s.fake.Client()),
		codebuild.SweepWithClock(s.clock.now))
	if err != nil {
		t.Fatalf("NewRegistrationSweeper: %v", err)
	}

	codebuild.SetSweeperSSMEndpointForTest(sw, s.fake.URL+"/")

	closed := func(ctx context.Context, leaseID string) (codebuild.LeaseClosure, error) {
		c, err := s.alloc.LeaseClosure(ctx, leaseID)
		if err != nil {
			return codebuild.LeaseClosure{}, err
		}

		return codebuild.LeaseClosure{Known: c.Known, Terminal: c.Terminal, FinishedAt: c.FinishedAt}, nil
	}

	return sw, closed
}

// assignedLease reserves a lease and assigns it the job the caller is about to
// launch, the way the listener does: through Assign, which opens the history
// row a completion's result and a job's start are recorded on.
func (s *codeBuildStack) assignedLease(t *testing.T, requestID int64) *alloc.Lease {
	t.Helper()

	lease, err := s.alloc.Reserve(t.Context(), codeBuildTier)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := s.alloc.Assign(t.Context(), lease.ID, lease.Epoch, requestID, requestID); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	lease.RequestID, lease.RunID = requestID, requestID

	return lease
}

// A JOB REACHES A BUILD, AND THE LEDGER AND THE BUILD AGREE ABOUT WHICH ONE.
//
// The chain under test: the allocator reserves against a node whose contribution is a
// budget rather than hardware, charging the SHAPE it will buy; the node runtime binds
// the lease and refuses anything the lease does not authorise; and this backend starts
// a build carrying markers the lease can be recovered from — because a build cannot be
// tagged. Every one of those is tested alone elsewhere; none of those tests can see
// the joins.
func TestAJobReachesACodeBuildBuild(t *testing.T) {
	s := newCodeBuildStack(t)

	lease := s.assignedLease(t, 601)

	// THE LEASE IS CHARGED THE SHAPE, NOT THE TIER REQUEST. The tier asks for 4 vCPU
	// and 7 GiB and the declared compute type holds exactly that here, so what this
	// asserts is that the shape was SELECTED at all — an unselected one leaves the
	// instance type empty and the launch cannot pick a compute type.
	if lease.InstanceType != "BUILD_GENERAL1_MEDIUM" {
		t.Fatalf("the lease charged instance type %q, want the declared compute type; placement "+
			"did not charge the shape this node buys", lease.InstanceType)
	}

	if err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: 601, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	name := provider.InstanceName(lease.ID)

	id, ok := s.fake.idOf(name)
	if !ok {
		t.Fatalf("no build was started carrying the lease's marker %q", name)
	}

	// THE REGISTRATION REACHED PARAMETER STORE AND NOWHERE ELSE. A build that starts
	// without one registers no runner and the job stays queued while every signal
	// reports success — and the value reaching the launch request instead would put a
	// single-use credential in the console and in CloudTrail.
	if got := s.fake.parameterCount(); got != 1 {
		t.Fatalf("%d registrations were staged, want exactly 1", got)
	}

	// THE INVENTORY COMES FROM THE PROVIDER rather than from the ledger — the point
	// of sending it is to report something the control plane cannot see for itself.
	// Quarantined capacity is freed on the strength of this list.
	running, err := s.runner.Instances(t.Context())
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}

	if len(running) != 1 || running[0] != lease.ID {
		t.Fatalf("this host reports %v as running, want just the lease it launched (%s)",
			running, lease.ID)
	}

	// The job finishes and the build is ASKED to stop. A STOP IS A REQUEST, so the
	// backend polls to a terminal state before claiming one — and the fake keeps the
	// build in STOPPING for exactly one look, which is what makes that observable.
	if err := s.runner.Destroy(t.Context(), 601); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if got := s.fake.stoppedIDs(); len(got) != 1 || got[0] != id {
		t.Fatalf("stopped %v, want the build that was started (%s)", got, id)
	}

	// AND THE CREDENTIAL WENT WITH IT, once the build was over and nothing could read
	// it again.
	if got := s.fake.parameterCount(); got != 0 {
		t.Errorf("%d staged registration(s) outlived the build", got)
	}
}

// A RESTART RE-ADOPTS THE BUILD BY EXACT DEPLOYMENT, PROJECT AND LEASE IDENTITY,
// KEEPS ITS CAPACITY CHARGED, AND DOES NOT STOP IT.
//
// This is the property GitHub's behaviour forces: GitHub does not requeue a job
// whose runner vanished mid-execution, so tearing a build down on restart is a
// deliberate job failure rather than a recovery.
//
// THE SECOND RUNNER IS A GENUINELY NEW PROCESS over the same ledger and the same fake
// AWS, which is what makes this a restart rather than a second method call: the
// in-memory map of what this host launched is gone, and the only surviving link
// between the build and its lease is the marker the build carries.
func TestARestartReadoptsACodeBuildBuildWithoutStoppingIt(t *testing.T) {
	s := newCodeBuildStack(t)

	lease := s.assignedLease(t, 602)

	if err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: 602, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	id, ok := s.fake.idOf(provider.InstanceName(lease.ID))
	if !ok {
		t.Fatal("no build was started")
	}

	stoppedBefore := len(s.fake.stoppedIDs())

	// The process dies and a new one comes up over the same ledger.
	restarted := node.New(s.alloc, s.host, s.jit, s.provider(t), nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// IT DID NOT STOP THE BUILD. This is the assertion the whole test exists for.
	if got := s.fake.stoppedIDs(); len(got) != stoppedBefore {
		t.Fatalf("a restart stopped %v; GitHub does not requeue a job whose runner vanished "+
			"mid-execution, so that is a deliberate job failure", got)
	}

	// AND THE CAPACITY IS STILL CHARGED, asserted against the LEDGER rather than
	// against the provider.
	//
	// THE PROVIDER IS THE WRONG WITNESS HERE, and the first version of this test used
	// it: `Instances()` reads provider.List, which reports the build whether or not
	// the runner adopted anything — so the assertion passed with adoption deleted.
	// What the property is actually about is whether the LEASE survived a process
	// that no longer remembers launching it, because that is what a freed slot would
	// show up as.
	usage, err := s.alloc.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage after restart: %v", err)
	}

	if usage.VCPU != lease.VCPU || usage.Leases != 1 {
		t.Fatalf("after a restart the ledger charges %d vCPU across %d lease(s), want %d "+
			"across 1: the capacity of a build that is still running was freed",
			usage.VCPU, usage.Leases, lease.VCPU)
	}

	after, err := s.alloc.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("the lease is gone after a restart, so its capacity was released while its "+
			"build (%s) kept running: %v", id, err)
	}

	if after.Phase == alloc.PhaseDone || after.Phase == alloc.PhaseFailed {
		t.Fatalf("the lease reached %s after a restart; a terminal phase releases the "+
			"capacity of a build that is still executing somebody's job", after.Phase)
	}

	// AND THE HOST STILL REPORTS IT, which is what the control plane reconciles
	// against — a lease absent from this list has its quarantined capacity freed.
	running, err := restarted.Instances(t.Context())
	if err != nil {
		t.Fatalf("Instances after restart: %v", err)
	}

	if len(running) != 1 || running[0] != lease.ID {
		t.Fatalf("the restarted host reports %v, want the lease whose build (%s) is still "+
			"running", running, id)
	}

	// AND THE STAGED REGISTRATION SURVIVES, because the build may not have read it
	// yet — deleting it here is a runner that never registers.
	if got := s.fake.parameterCount(); got != 1 {
		t.Errorf("a restart removed the staged registration (%d remain)", got)
	}
}

// THE CONTROL PLANE SWEEPS A REGISTRATION A DEAD NODE LEFT BEHIND, ON THE LEDGER'S
// AUTHORITY ALONE, AND ONLY ONCE THE LEASE HAS BEEN CLOSED FOR LONGER THAN A BUILD
// CAN RUN.
//
// Two jobs reach two builds, so two registrations are staged. The node then dies
// with neither settled — nothing here calls Destroy or reaps — and the ledger later
// closes ONE lease, which is what a reap, a quarantine resolved on absent inventory,
// or an operator's force-release all end in. What the sweep must do is remove that
// lease's registration and no other, and not before the service window has passed:
// the same pass run immediately after the closing removes nothing, because a build
// for a lease closed a moment ago may still be reading its parameter.
//
// The fake's Parameter Store fails the test if the sweep asks for decryption, so the
// property that the control plane never holds a registration is asserted by running
// the pass at all.
func TestTheControlPlaneSweepsARegistrationADeadNodeLeftBehind(t *testing.T) {
	// ON A CLOCK THE TEST MOVES, because the window is forty-five hours. The TTL
	// is generous so nothing here expires while the clock jumps.
	clock := &offsetClock{}
	s := newCodeBuildStack(t, cbOnClock(clock, 7*24*time.Hour))

	dead := s.assignedLease(t, 606)
	if err := s.runner.Launch(t.Context(), dead,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: 606, Event: "push"}); err != nil {
		t.Fatalf("Launch(dead): %v", err)
	}

	alive := s.assignedLease(t, 607)
	if err := s.runner.Launch(t.Context(), alive,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: 607, Event: "push"}); err != nil {
		t.Fatalf("Launch(alive): %v", err)
	}

	if got := s.fake.parameterCount(); got != 2 {
		t.Fatalf("%d registrations staged, want 2", got)
	}

	// The node is gone. The ledger closes the first lease — at the CURRENT epoch,
	// because Launch bound it — and the second stays open.
	current, err := s.alloc.Lease(t.Context(), dead.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if err := s.alloc.Release(t.Context(), dead.ID, current.Epoch, alloc.PhaseFailed); err != nil {
		t.Fatalf("Release: %v", err)
	}

	sw, closed := s.sweeper(t)

	// TOO SOON: a build for a lease closed a moment ago may still be starting.
	report, err := sw.Sweep(t.Context(), closed)
	if err != nil {
		t.Fatalf("Sweep (too soon): %v", err)
	}

	if report.Removed != 0 || report.Kept != 2 {
		t.Fatalf("a sweep straight after the closing reported %+v; nothing may go before the "+
			"service window has passed", report)
	}

	if got := s.fake.parameterCount(); got != 2 {
		t.Fatalf("%d registrations remain after a sweep that should have removed nothing", got)
	}

	// PAST THE WINDOW: nothing can read the dead lease's registration again.
	clock.advance(codebuild.ServiceInventoryWindow + time.Minute)

	report, err = sw.Sweep(t.Context(), closed)
	if err != nil {
		t.Fatalf("Sweep (past the window): %v", err)
	}

	if report.Removed != 1 || report.Kept != 1 || report.Unaccounted != 0 {
		t.Fatalf("the sweep reported %+v; want exactly the dead lease's registration removed", report)
	}

	if got := s.fake.parameterCount(); got != 1 {
		t.Fatalf("%d registrations remain, want the live lease's alone", got)
	}

	s.fake.mu.Lock()
	_, aliveKept := s.fake.params[cbParameterPath+"/"+provider.InstanceName(alive.ID)]
	_, deadKept := s.fake.params[cbParameterPath+"/"+provider.InstanceName(dead.ID)]
	s.fake.mu.Unlock()

	if !aliveKept || deadKept {
		t.Fatalf("the wrong registration was removed: alive kept=%v, dead kept=%v", aliveKept, deadKept)
	}

	// AND THE LIVE LEASE'S CAPACITY WAS NEVER TOUCHED: the sweep deletes a
	// parameter, never a lease.
	usage, err := s.alloc.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Leases != 1 {
		t.Fatalf("%d lease(s) open after the sweep, want the live one", usage.Leases)
	}
}

// provider builds a provider over the stack's fake.
//
// ONE CONSTRUCTOR FOR EVERY PROCESS, so a restarted runner is built exactly as
// the first was: a "restart" that differs from the original in anything but its
// empty memory is testing a different program.
func (s *codeBuildStack) provider(t *testing.T) *codebuild.Provider {
	t.Helper()

	return newCodeBuildProvider(t, s.fake, "e2e-deployment", []config.RemoteShape{
		{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 10000},
	})
}

// A DRAIN WAITS FOR RUNNING WORK AND NEVER CALLS StopBuild.
//
// `destroyAll`'s includeRunning parameter is where the two cases are separated —
// a shutdown passes false and only `billet force-destroy` passes true — and this
// asserts the CONSEQUENCE at the API rather than the parameter: no StopBuild
// reached AWS.
func TestADrainNeverStopsARunningCodeBuildBuild(t *testing.T) {
	s := newCodeBuildStack(t)

	lease := s.assignedLease(t, 603)

	if err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: 603, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// The node stops. What a stop leaves behind is deliberate: the build keeps
	// running, the node keeps holding it, the lease stays CHARGED, and the next
	// control plane re-adopts it.
	if err := s.runner.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if got := s.fake.stoppedIDs(); len(got) != 0 {
		t.Fatalf("a sweep stopped %v while its lease was still open; freeing a slot whose "+
			"build is live is the overcommit the ordering exists to prevent", got)
	}

	for _, target := range s.fake.sentTargets() {
		if strings.HasSuffix(target, ".StopBuild") {
			t.Errorf("a drain sent %s", target)
		}
	}
}

// UNTRUSTED WORK NEVER REACHES THIS BACKEND, AND NOTHING IS MINTED FOR IT.
//
// The refusal is asked BEFORE anything irreversible happens: minting a registration
// and being refused afterwards leaves a runner registered on GitHub with nothing to
// consume it — one orphan per pull request, accumulating quietly.
func TestUntrustedWorkNeverReachesCodeBuild(t *testing.T) {
	s := newCodeBuildStack(t)

	// The tier's pool authority is what decides, not the assignment: GitHub may give
	// a registered scale-set runner a different job than the one that caused billet
	// to launch it.
	untrusted := s.tier
	untrusted.Trust = config.WorkloadUntrusted
	untrusted.Workflows = nil
	untrusted.RunnerGroup = ""

	lease := s.assignedLease(t, 604)

	err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(untrusted, config.ProviderCodeBuild),
		server.Job{RequestID: 604, Event: "pull_request"})
	if err == nil {
		t.Fatal("untrusted work reached the CodeBuild backend")
	}

	// The default stack is on-demand Linux with NO isolated network declared, so
	// untrusted work is refused because its absence is the refusal — and the message
	// names the fields an operator turns it on with, rather than a field that does
	// not exist. (An untrusted network plus a verified project is what admits it; see
	// the provider's untrustedNetwork tests.)
	if !strings.Contains(err.Error(), "untrusted_vpc_id") {
		t.Errorf("the refusal does not name the fields that would enable it, so an operator "+
			"cannot act on it: %v", err)
	}

	if got := s.jit.count(); got != 0 {
		t.Errorf("%d registration(s) were minted for work that was refused", got)
	}

	if got := s.fake.parameterCount(); got != 0 {
		t.Errorf("%d registration(s) were staged for work that was refused", got)
	}

	for _, target := range s.fake.sentTargets() {
		if strings.HasSuffix(target, ".StartBuild") {
			t.Errorf("a refused launch still sent %s", target)
		}
	}
}

// A LEASE THIS HOST'S BACKEND CANNOT SERVE IS REFUSED BEFORE ANYTHING IS MINTED.
func TestACodeBuildNodeRefusesALeaseItWasNotPlacedFor(t *testing.T) {
	s := newCodeBuildStack(t)

	lease := s.assignedLease(t, 605)

	// As though placement had chosen a bare-metal host for it.
	lease.Providers = []config.ProviderKind{config.ProviderFirecracker}

	err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderCodeBuild),
		server.Job{RequestID: 605, Event: "push"})
	if err == nil {
		t.Fatal("a lease that does not accept codebuild was launched anyway")
	}

	if got := s.jit.count(); got != 0 {
		t.Errorf("%d registration(s) were minted for a lease this host cannot serve", got)
	}

	if got := s.fake.parameterCount(); got != 0 {
		t.Errorf("%d registration(s) were staged for a lease this host cannot serve", got)
	}
}

// count reports how many registrations were minted.
func (j *cbJIT) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.minted
}
