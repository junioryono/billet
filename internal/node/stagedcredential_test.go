package node

import (
	"context"
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
)

// reapingProvider is a fakeProvider that also stages a credential outside its compute.
//
// IT JUDGES NOTHING. The rule under test is billet's — "settlement reaps what the
// backend staged" — so the fake records the call and returns what it is told. A fake
// that decided for itself whether reaping was allowed would make the assertion below be
// about the fake, which is the failure `billet-testing` records twice over.
type reapingProvider struct {
	*fakeProvider

	reaped  []string
	reapErr error

	// terminalAfterLaunch makes whatever a failed-but-started launch recorded come
	// back TERMINAL, which is the state destroyStray confirms on without calling
	// Destroy — the one proof-bearing exit that used to skip the reap.
	terminalAfterLaunch bool
}

func (p *reapingProvider) ReapStagedCredential(_ context.Context, name string) error {
	p.reaped = append(p.reaped, name)

	return p.reapErr
}

// Launch defers to the shared fake and then, if asked, marks what it recorded terminal.
//
// HERE RATHER THAN ON fakeProvider, because this is one scenario's need and the shared
// fake is used by a whole suite: a field there that only this test sets is a field the
// next reader has to rule out.
func (p *reapingProvider) Launch(
	ctx context.Context, spec provider.Spec,
) (*provider.Instance, error) {
	inst, err := p.fakeProvider.Launch(ctx, spec)

	if p.terminalAfterLaunch {
		p.terminate(spec.Name)
	}

	return inst, err
}

// A LEASE THAT SETTLES REAPS WHAT ITS BACKEND STAGED OUTSIDE THE COMPUTE.
//
// A backend that cannot hand a secret to the compute it launches has to stage the
// runner registration somewhere else — CodeBuild's `StartBuild` has no field for one, so
// it goes into an SSM SecureString — and destroying the compute does not remove it.
//
// THIS DRIVES THE RUNNER, not the provider method. The provider's own coverage proves
// the deletion works; it cannot prove anything CALLS it, and the defect this test exists
// for was exactly that: `TidyStagedRegistration`'s doc comment said the node's tending
// sweep invoked it and nothing in production did, so every lease that settled without a
// Destroy left its credential behind until the account's Parameter Store quota.
func TestSettlementReapsACredentialStagedOutsideTheCompute(t *testing.T) {
	p := &reapingProvider{fakeProvider: &fakeProvider{
		kind: config.ProviderEC2, asyncTeardown: true,
	}}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 21, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), 21); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}

	name := provider.InstanceName(lease.ID)

	// NOTHING IS REAPED BEFORE THE PROOF ARRIVES, which is the half that makes the
	// assertion after it mean something: a reap that ran on every sweep would delete a
	// registration a build has not read yet, and that is a runner which never
	// registers.
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend while the compute was still visible: %v", err)
	}

	if len(p.reaped) != 0 {
		t.Fatalf("the staged credential was reaped while the compute was still running: %v",
			p.reaped)
	}

	p.terminate(name)

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the backend reported the compute terminal: %v", err)
	}

	// The lease settled, which is the proof the reap was authorised by.
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Fatalf("terminal proof left the lease held, so settlement never ran: %v", err)
	}

	if len(p.reaped) == 0 {
		t.Fatal("the lease settled and nothing asked the backend to remove the registration " +
			"it staged outside the compute; on codebuild that credential sits in Parameter " +
			"Store until the account's quota")
	}

	// BY NAME, because the name is the only durable link between a staged credential
	// and the lease that authorised it — a reap of the wrong one deletes another
	// build's registration.
	if p.reaped[0] != name {
		t.Errorf("settlement reaped %q, want the lease's own instance name %q", p.reaped[0], name)
	}
}

// AND AN AMBIGUOUS LAUNCH WHOSE COMPUTE IS ALREADY OVER REAPS TOO, which was the one
// proof-bearing exit that did not.
//
// `destroyStray` confirms on an explicit TERMINAL RECORD without calling Destroy at all
// — right, because there is nothing to destroy — and the launch path then returns
// without entering custody. So the two places that remove a staged credential were both
// skipped: Destroy, where the ordinary teardown removes it, and custody's settlement,
// where the reaper runs. For a CodeBuild launch that failed ambiguously and whose build
// was already over by the time `Find` looked, the SecureString the launch path
// deliberately PRESERVES then stayed forever.
//
// THE LAUNCH IS MADE TO FAIL AFTER STARTING SOMETHING, which is the ambiguous outcome
// this whole path exists for, and the instance is terminal before the cleanup looks.
func TestAnAmbiguousLaunchWhoseComputeIsAlreadyOverReapsItsCredential(t *testing.T) {
	p := &reapingProvider{fakeProvider: &fakeProvider{
		kind:         config.ProviderEC2,
		launchErr:    errors.New("the launch's response was lost"),
		startsAnyway: true,
	}}
	p.terminalAfterLaunch = true
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 24, Event: "push"})
	if err == nil {
		t.Fatal("Launch reported success against a provider that failed after starting")
	}

	// NOT CUSTODY. If this took the custody path the reap would happen at settlement
	// and this test would be covering the case the previous one already covers.
	if errors.Is(err, server.ErrCustody) {
		t.Fatalf("the failed launch went into custody, so this test is not exercising the "+
			"confirmed non-custody exit: %v", err)
	}

	if len(p.reaped) == 0 {
		t.Fatal("a failed launch was cleaned up on explicit terminal proof and nothing " +
			"removed the registration it had staged; on codebuild that SecureString stays " +
			"in Parameter Store forever, and repeated fast failures reach the account quota")
	}

	if p.reaped[0] != provider.InstanceName(lease.ID) {
		t.Errorf("reaped %q, want the lease's own instance name %q",
			p.reaped[0], provider.InstanceName(lease.ID))
	}
}

// AND A REAP THAT FAILS DOES NOT HOLD THE CAPACITY.
//
// The release is what frees the slot, and a leftover single-use credential must not be
// the reason a lease stays charged — the compute is already proved gone, so refusing to
// settle would strand capacity over litter. It is reported instead.
func TestAFailedReapStillSettlesTheLease(t *testing.T) {
	p := &reapingProvider{
		fakeProvider: &fakeProvider{kind: config.ProviderEC2, asyncTeardown: true},
		reapErr:      errors.New("parameter store is unreachable"),
	}
	a, host := newAllocatorWithEC2Host(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 22, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), 22); !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy = %v, want ErrCustody", err)
	}

	p.terminate(provider.InstanceName(lease.ID))

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the compute was proved gone: %v", err)
	}

	if len(p.reaped) == 0 {
		t.Fatal("nothing attempted the reap, so this test proves nothing about its failure")
	}

	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("a failed credential reap held the lease's capacity, and the compute was "+
			"already proved gone: %v", err)
	}
}

// AND A BACKEND THAT STAGES NOTHING IS NEVER ASKED, which is why the contract is a type
// assertion rather than a method on every provider: four of the five backends hand the
// registration to the compute directly and have nothing to reap.
func TestABackendThatStagesNothingIsNotAskedToReap(t *testing.T) {
	p := &fakeProvider{kind: config.ProviderDocker}

	if _, ok := any(p).(provider.StagedCredentialReaper); ok {
		t.Fatal("the plain fake implements StagedCredentialReaper, so the assertion below " +
			"cannot distinguish a backend that stages nothing")
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 23, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if err := r.Destroy(t.Context(), 23); err != nil && !errors.Is(err, server.ErrCustody) {
		t.Fatalf("Destroy: %v", err)
	}

	// The settlement path runs and must not panic or refuse over a provider with no
	// reaper. Tend is idempotent, so calling it is enough.
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend on a provider that stages nothing outside its compute: %v", err)
	}
}
