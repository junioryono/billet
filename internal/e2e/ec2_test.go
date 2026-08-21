package e2e

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
)

// THE SEAM NOTHING ELSE COVERS: the node runtime driving the cloud backend.
//
// The ec2 package is tested thoroughly on its own, against a fake API, and the
// allocator's placement is tested on its own against every provider kind. Neither
// can catch a mistake in how the two are WIRED, which is where this project's
// worst defect so far lived — the launch path agreeing with itself while sending
// the wrong thing.
//
// It needs no Docker, unlike the rest of this package, because the compute it
// launches is a fake HTTP endpoint rather than a daemon on this machine. So it
// runs everywhere, including on a machine that has never had Docker installed.
const ec2Tier = "billet-8vcpu-cloud"

// fakeCloud is an EC2 query API that records what it was asked and answers
// plausibly.
type fakeCloud struct {
	*httptest.Server

	mu sync.Mutex
	// launched maps the Name tag of every instance started to its id, which is
	// what a test asserts against — the name is the only durable link between a
	// running instance and the lease that authorised it.
	launched map[string]string
	// terminated records the ids handed to TerminateInstances.
	terminated []string
	// live is what DescribeInstances reports as running, keyed by id.
	live map[string]string
	// shuttingDown is what has been asked to terminate but has not finished.
	shuttingDown map[string]string
	// userData holds the boot script of the last launch, decoded.
	userData string

	next int
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()

	f := &fakeCloud{
		launched:     map[string]string{},
		live:         map[string]string{},
		shuttingDown: map[string]string{},
	}

	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)

			return
		}

		params, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse body: %v", err)

			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		var reply string

		switch params.Get("Action") {
		case "DescribeImages":
			// SHAPED LIKE A REAL RESPONSE. This reported a root device name and
			// nothing else, which no image measured against a live account does —
			// they report the root device TYPE and describe that device in the block
			// device mapping. billet now requires evidence that a root is EBS-backed
			// (#54), and this fixture described an image it correctly refuses.
			reply = `<DescribeImagesResponse><imagesSet><item><imageId>ami-cloud</imageId>` +
				`<rootDeviceName>/dev/xvda</rootDeviceName>` +
				`<rootDeviceType>ebs</rootDeviceType>` +
				`<blockDeviceMapping><item><deviceName>/dev/xvda</deviceName><ebs>` +
				`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
				`</blockDeviceMapping>` +
				`</item></imagesSet></DescribeImagesResponse>`

		case "RunInstances":
			f.next++
			id := "i-" + strings.Repeat("0", 3) + string(rune('a'+f.next))
			name := params.Get("TagSpecification.1.Tag.1.Value")
			f.launched[name] = id
			f.live[id] = name

			if raw, decErr := base64.StdEncoding.DecodeString(params.Get("UserData")); decErr == nil {
				f.userData = string(raw)
			}

			reply = `<RunInstancesResponse><instancesSet><item><instanceId>` + id +
				`</instanceId><instanceState><name>pending</name></instanceState>` +
				`</item></instancesSet></RunInstancesResponse>`

		case "TerminateInstances":
			// ASYNCHRONOUS, LIKE THE REAL ONE. TerminateInstances returns when the
			// request is ACCEPTED; the instance then sits in shutting-down until
			// EC2 finishes with it. A fake that deleted the row here would make
			// billet look synchronous and hide what the inventory reports in the
			// window that actually exists.
			id := params.Get("InstanceId.1")
			f.terminated = append(f.terminated, id)
			f.shuttingDown[id] = f.live[id]
			delete(f.live, id)

			reply = `<TerminateInstancesResponse/>`

		case "DescribeInstances":
			// HONOURING THE STATE FILTER, so the fake cannot flatter billet by
			// returning rows it never asked for — and finding that filter BY NAME,
			// because assuming its position is the same class of mistake billet
			// itself made when it hard-coded Filter.2.
			wanted := map[string]bool{}

			for n := 1; ; n++ {
				name := params.Get(fmt.Sprintf("Filter.%d.Name", n))
				if name == "" {
					break
				}

				if name != "instance-state-name" {
					continue
				}

				for i := 1; ; i++ {
					v := params.Get(fmt.Sprintf("Filter.%d.Value.%d", n, i))
					if v == "" {
						break
					}

					wanted[v] = true
				}
			}

			var b strings.Builder

			b.WriteString(`<DescribeInstancesResponse><reservationSet><item><instancesSet>`)

			if wanted["running"] {
				for id, name := range f.live {
					b.WriteString(`<item><instanceId>` + id + `</instanceId>` +
						`<instanceState><name>running</name></instanceState><tagSet>` +
						`<item><key>Name</key><value>` + name + `</value></item>` +
						`</tagSet></item>`)
				}
			}

			if wanted["shutting-down"] {
				for id, name := range f.shuttingDown {
					b.WriteString(`<item><instanceId>` + id + `</instanceId>` +
						`<instanceState><name>shutting-down</name></instanceState><tagSet>` +
						`<item><key>Name</key><value>` + name + `</value></item>` +
						`</tagSet></item>`)
				}
			}

			b.WriteString(`</instancesSet></item></reservationSet></DescribeInstancesResponse>`)
			reply = b.String()

		default:
			// REFUSED RATHER THAN ANSWERED AS A DESCRIBE. A fake that treats an
			// unrecognised action as the harmless one hides a caller asking for
			// something nobody meant it to ask for.
			t.Errorf("the fake was asked for an action it does not implement: %q",
				params.Get("Action"))
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		if _, err := io.WriteString(w, reply); err != nil {
			t.Errorf("write reply: %v", err)
		}
	}))

	t.Cleanup(f.Close)

	return f
}

func (f *fakeCloud) idOf(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, ok := f.launched[name]

	return id, ok
}

func (f *fakeCloud) terminatedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.terminated...)
}

// stillShuttingDown reports whether an instance has been asked to terminate and
// has not finished.
func (f *fakeCloud) stillShuttingDown(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.shuttingDown[id]

	return ok
}

// finishTerminating is EC2 completing a shutdown on its own clock.
func (f *fakeCloud) finishTerminating() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.shuttingDown = map[string]string{}
}

func (f *fakeCloud) bootScript() string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.userData
}

// cloudJIT mints registrations without a GitHub organization, and COUNTS them.
//
// The count is the point. Both refusal tests below assert that nothing was
// launched, and that alone would stay green if the refusal moved to AFTER the
// registration was minted — which is the failure their comments claim to prevent:
// a live runner registration on GitHub with nothing to consume it, one per
// refused pull request, accumulating quietly.
type cloudJIT struct {
	mu     sync.Mutex
	minted int
}

func (j *cloudJIT) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.minted
}

type cloudRegistration struct{ name string }

func (r cloudRegistration) Config() string     { return "jit-for-" + r.name }
func (r cloudRegistration) RunnerName() string { return r.name }
func (r cloudRegistration) ID() int64          { return 71 }

func (*cloudJIT) Describe(context.Context, string, string) (*node.Set, []string, error) {
	return &node.Set{ID: 7, Name: ec2Tier}, nil, nil
}

func (j *cloudJIT) JITConfig(
	_ context.Context, _ int, runnerName, _ string,
) (node.Registration, error) {
	j.mu.Lock()
	j.minted++
	j.mu.Unlock()

	return cloudRegistration{name: runnerName}, nil
}

func (*cloudJIT) ValidateTrustedRunnerGroup(context.Context, string, []string) error { return nil }
func (*cloudJIT) RemoveRunner(context.Context, string, int64, string) error          { return nil }
func (*cloudJIT) EnsureRunnerRemoved(context.Context, string) error                  { return nil }
func (*cloudJIT) RecoverRunner(context.Context, string, string, int64, string) (node.RunnerRecovery, error) {
	return node.RunnerRecoveryTracked, nil
}

// cloudStack is a control-plane ledger and a node runtime over the cloud backend.
type cloudStack struct {
	alloc  *alloc.Allocator
	runner *node.Runner
	cloud  *fakeCloud
	jit    *cloudJIT
	tier   config.Tier
}

func newCloudStack(t *testing.T) *cloudStack {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	tier := config.Tier{
		Label:    ec2Tier,
		Provider: config.ProviderEC2,
		VCPU:     8,
		Memory:   16 * config.GiB,
		Disk:     80 * config.GiB,
		Image:    "ami-cloud",
		GuestOS:  config.GuestLinux,
		Command:  []string{"./run.sh"},
		Trust:    config.WorkloadTrusted,
		Workflows: []string{
			"acme/cloud/.github/workflows/ci.yml@refs/heads/main",
		},
		RunnerGroup: "trusted",
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB},
		[]config.Tier{tier})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const host = "aws-1"
	shapes := []config.EC2InstanceType{
		{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB},
	}

	// WHAT THE NODE CONTRIBUTES IS A BUDGET, not this machine's hardware — the
	// whole reason an ec2 node has to declare it.
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: host, Provider: config.ProviderEC2, VCPU: 64, Memory: 256 * config.GiB,
		EC2Shapes: shapes,
	}); err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}

	cloud := newFakeCloud(t)

	prov, err := ec2.New("e2e-deployment", config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         cloud.URL,
		SubnetID:         "subnet-e2e",
		SecurityGroupIDs: []string{"sg-e2e"},
		InstanceTypes:    shapes,
	},
		ec2.WithHTTPClient(cloud.Client()),
		ec2.WithCredentials(ec2.StaticCredentials{AccessKeyID: "AKID", SecretAccessKey: "s"}))
	if err != nil {
		t.Fatalf("ec2.New: %v", err)
	}

	jit := &cloudJIT{}

	return &cloudStack{
		alloc:  a,
		runner: node.New(a, host, jit, prov, nil),
		cloud:  cloud,
		jit:    jit,
		tier:   tier,
	}
}

// assignedLease reserves capacity and moves it to where a launch may begin.
//
// A LEASE DOES NOT GO STRAIGHT FROM RESERVED TO LAUNCHING. Reserve leaves it in
// `capacity`, which is escrow a listener is holding to OFFER GitHub; the
// assignment is what turns an offer into work, and the state machine refuses the
// step that skips it. In production the listener does this when GitHub assigns
// the job, so a test that launched from `capacity` would be exercising a path no
// deployment can reach.
func (s *cloudStack) assignedLease(t *testing.T) *alloc.Lease {
	t.Helper()

	lease, err := s.alloc.Reserve(t.Context(), ec2Tier)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := s.alloc.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseAssigned); err != nil {
		t.Fatalf("Advance to assigned: %v", err)
	}

	return lease
}

// A JOB REACHES A CLOUD INSTANCE, AND THE LEDGER AND THE INSTANCE AGREE ABOUT
// WHICH ONE.
//
// The chain under test: the allocator reserves against a node whose contribution
// is a budget rather than hardware, the node runtime binds the lease and refuses
// to launch anything the lease does not authorise, and the cloud backend starts a
// machine carrying a name the lease can be recovered from. Every one of those is
// tested alone elsewhere; none of those tests can see the joins.
func TestAJobReachesACloudInstance(t *testing.T) {
	s := newCloudStack(t)

	lease := s.assignedLease(t)

	if err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderEC2),
		server.Job{RequestID: 501, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// NAMED AFTER THE LEASE. Nothing writes "this instance belongs to that lease"
	// anywhere, so after a crash the name is all reconciliation has.
	name := provider.InstanceName(lease.ID)

	id, ok := s.cloud.idOf(name)
	if !ok {
		t.Fatalf("no instance was started carrying the lease's name %q", name)
	}

	// AND THE REGISTRATION REACHED THE GUEST. A machine that boots without one
	// registers no runner, and the job stays queued while every signal reports
	// success — the failure this project has already been bitten by once.
	if script := s.cloud.bootScript(); !strings.Contains(script, "jit-for-"+name) {
		t.Errorf("the boot script does not carry this runner's registration:\n%s", script)
	}

	// THE INVENTORY IS WHAT THE CONTROL PLANE RECONCILES AGAINST, and it comes
	// from the PROVIDER rather than from the ledger — the point of sending it is
	// to report something the control plane cannot see for itself. Quarantined
	// capacity is freed on the strength of this list, so a lease missing from it
	// is capacity handed back while a machine still runs.
	running, err := s.runner.Instances(t.Context())
	if err != nil {
		t.Fatalf("Instances: %v", err)
	}

	if len(running) != 1 || running[0] != lease.ID {
		t.Fatalf("this host reports %v as running, want just the lease it launched (%s)",
			running, lease.ID)
	}

	// The job finishes, and the instance is ASKED to go.
	//
	// A TERMINATE IS A REQUEST, NOT A COMPLETION. Unlike `docker rm --force`,
	// TerminateInstances returns once the request is accepted and the instance sits
	// in shutting-down for a while afterwards. So the node answers ErrCustody: it
	// is holding this lease's capacity until the machine is provably gone, and the
	// listener must not release it.
	//
	// THIS TEST USED TO PIN THE OPPOSITE, and called it a documented property —
	// Destroy reported plain success inside that window and the listener released
	// on it. That is #46, and the reason it is worse than an over-commit is that
	// Destroy is reached while the guest is still working: a drain, a custody
	// teardown, an operator killing a job. A second job could start for the same
	// repository while the first was still finishing a deploy.
	err = s.runner.Destroy(t.Context(), 501)
	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy answered %v; while the machine is still shutting down it must "+
			"answer ErrCustody, or the listener releases the capacity underneath it", err)
	}

	if got := s.cloud.terminatedIDs(); len(got) != 1 || got[0] != id {
		t.Fatalf("terminated %v, want the instance that was started (%s)", got, id)
	}

	// AND THE WINDOW IS REAL IN THIS FAKE, which is what makes the assertion above
	// mean anything: a fake that finished the termination synchronously would let
	// a Destroy that claimed confirmation pass.
	if !s.cloud.stillShuttingDown(id) {
		t.Fatal("the fake finished the termination synchronously, so this test can no longer " +
			"observe the window Destroy returns inside")
	}

	// WHAT THE INVENTORY ASSERTS IS THIS PACKAGE'S HALF. What the control plane
	// then does with it — holding quarantined capacity for anything still reported
	// — is Reconcile's, and is tested against the ledger in internal/alloc.

	during, err := s.runner.Instances(t.Context())
	if err != nil {
		t.Fatalf("Instances after destroy: %v", err)
	}

	if len(during) != 1 || during[0] != lease.ID {
		t.Errorf("this host reports %v while its instance is still shutting down, want the "+
			"lease to stay accounted for until the machine is gone", during)
	}

	// And once EC2 finishes, it leaves the inventory — which is what frees the
	// capacity a step later, in Reconcile, where the ledger is what gets asserted.
	s.cloud.finishTerminating()

	after, err := s.runner.Instances(t.Context())
	if err != nil {
		t.Fatalf("Instances after termination completed: %v", err)
	}

	if len(after) != 0 {
		t.Errorf("this host still reports %v after its instance was gone", after)
	}
}

// A LEASE THIS HOST'S BACKEND CANNOT SERVE IS REFUSED BEFORE ANYTHING IS MINTED.
//
// The lease carries the backends it was reserved for, snapshotted so that editing
// the config underneath an in-flight lease cannot reclassify it. A cloud node
// handed a lease that does not name ec2 must refuse rather than launch — and
// refuse before minting a registration, since one with nothing to consume it is
// an orphan on GitHub that billet will never clean up.
func TestACloudNodeRefusesALeaseItWasNotPlacedFor(t *testing.T) {
	s := newCloudStack(t)

	lease := s.assignedLease(t)

	// As though placement had chosen a bare-metal host for it.
	lease.Providers = []config.ProviderKind{config.ProviderFirecracker}

	err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderEC2),
		server.Job{RequestID: 502, Event: "push"})
	if err == nil {
		t.Fatal("a cloud node launched a lease that does not accept its backend")
	}

	if _, started := s.cloud.idOf(provider.InstanceName(lease.ID)); started {
		t.Error("a refused launch still started an instance")
	}

	// AND BEFORE ANYTHING WAS MINTED. A registration with nothing to consume it is
	// an orphan on GitHub that billet never cleans up.
	if n := s.jit.count(); n != 0 {
		t.Errorf("a refused launch minted %d runner registration(s)", n)
	}
}

// FORK PULL-REQUEST WORK IS REFUSED UNTIL IT HAS A NETWORK OF ITS OWN, and the
// refusal happens before a registration is minted.
//
// An instance isolates the kernel, which is why this backend may run untrusted
// code at all — but not the VPC, so without a security group described for it the
// work does not run. This asserts the whole path refuses, not just Accepts.
func TestUntrustedWorkDoesNotReachTheCloudWithoutItsOwnNetwork(t *testing.T) {
	s := newCloudStack(t)
	s.tier.Trust = config.WorkloadUntrusted

	lease := s.assignedLease(t)

	err := s.runner.Launch(t.Context(), lease,
		nodeapi.TierSpecOf(s.tier, config.ProviderEC2),
		server.Job{RequestID: 503, Event: "pull_request"})
	if err == nil {
		t.Fatal("a fork pull request reached the cloud with no network described for it")
	}

	if _, started := s.cloud.idOf(provider.InstanceName(lease.ID)); started {
		t.Error("a refused launch still started an instance")
	}

	// AND BEFORE ANYTHING WAS MINTED, which is the half that matters most for a
	// pull request: every refused one would otherwise leave a live registration.
	if n := s.jit.count(); n != 0 {
		t.Errorf("a refused launch minted %d runner registration(s)", n)
	}
}
