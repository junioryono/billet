package codebuild

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// The regressions from the second adversarial review of this package. Each one is a
// path where the code agreed with itself and with an undocumented property of
// somebody else's API.

// AN EMPTY PAGE IS NOT THE END OF A PAGINATED ANSWER.
//
// `ListBuildsForProject` handing back no ids WITH a nextToken used to end the walk
// successfully, which turns an incomplete listing into a SHORT inventory — and `List`
// frees the capacity of every lease absent from its answer, so a build that lived only
// on a later page had its capacity resold while it kept running.
//
// THE BUILD IS ON THE PAGE AFTER THE EMPTY ONE, which is what makes this fail rather
// than merely count calls: an implementation that stops early reports an empty
// inventory, and the assertion is that the running build is IN it.
func TestAnEmptyPageWithATokenDoesNotEndTheWalk(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	const lease = "0123456789abcdef0123456789abcdef"

	name := provider.InstanceName(lease)
	f.addOwnedBuild("billet-linux:live", name)

	f.listPages = []listPage{
		{ids: nil, nextToken: "page-2"},
		{ids: []string{"billet-linux:live"}},
	}

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 1 || got[0].Name != name {
		t.Fatalf("List returned %d instance(s) (%+v); a page with no ids and a nextToken ended "+
			"the walk, so the running build on the next page is absent — and reconciliation "+
			"frees the capacity of everything absent from this answer", len(got), got)
	}

	if f.listCalls < 2 {
		t.Errorf("ListBuildsForProject was called %d time(s); the walk did not follow the empty "+
			"page's token", f.listCalls)
	}
}

// AND AN EMPTY PAGE THAT REPEATS A TOKEN IS STILL REFUSED, so the fix above did not
// open the loop the cycle guard exists to close: a listing that never ends stops
// reporting this host's inventory at all, and the capacity of anything quarantined on
// it is held until an operator intervenes.
func TestAnEmptyPageCannotLoopTheWalk(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	f.listPages = []listPage{
		{ids: nil, nextToken: "loop"},
		{ids: nil, nextToken: "loop"},
	}

	_, err := p.List(t.Context())
	if err == nil {
		t.Fatal("a listing that keeps handing back the same token was walked forever rather " +
			"than refused")
	}

	if !strings.Contains(err.Error(), "pagination token") {
		t.Errorf("the refusal does not say the listing could not be finished: %v", err)
	}
}

// TWO LIVE BUILDS UNDER ONE LEASE NAME IS REFUSED.
//
// A lease should only ever have one build, and the launch path is written so an
// ambiguous StartBuild never retries in order to keep that true. If it happens anyway,
// billet cannot tell which build the lease's runner is inside — capacity is charged
// once and custody is keyed by the lease, so reporting either would hand back the
// other's while its build kept running.
func TestTwoLiveBuildsUnderOneLeaseRefuseTheInventory(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	const lease = "0123456789abcdef0123456789abcdef"

	name := provider.InstanceName(lease)
	f.addOwnedBuild("billet-linux:one", name)
	f.addOwnedBuild("billet-linux:two", name)

	_, err := p.List(t.Context())
	if err == nil {
		t.Fatal("two live builds carrying one lease marker produced an inventory rather than a " +
			"refusal, so one of them is unaccounted for and its capacity is free to resell")
	}

	for _, want := range []string{"billet-linux:one", "billet-linux:two", nameEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// BATCHGETBUILDS IS REORDERED INTO THE ORDER IT WAS ASKED IN.
//
// ListBuildsForProject answers newest first and `Find` takes the newest TERMINAL match
// for a lease. BatchGetBuilds is documented as answering ABOUT the ids it was given and
// says nothing about ORDER — measured against real CodeBuild it does preserve it, which
// is exactly why the missing guarantee was invisible.
//
// BOTH BUILDS ARE TERMINAL HERE, deliberately, because that is the only case where the
// ORDER is what decides: with a live one present the live one wins whatever the order,
// so a test using two live builds would pass with the reorder deleted.
//
// THE FAKE ANSWERS IN REVERSE, which is what a service is free to do.
func TestFindTakesTheNewestTerminalBuildEvenWhenTheBatchAnswersOutOfOrder(t *testing.T) {
	f := newFakeAWS(t)
	f.reverseBatch = true
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("0123456789abcdef0123456789abcdef")

	// Ids sort ascending, so the listing puts :zzz first as the newest.
	f.addOwnedBuildWithStatus("billet-linux:aaa", name, "FAILED")
	f.addOwnedBuildWithStatus("billet-linux:zzz", name, "SUCCEEDED")

	b, ok, err := p.findBuild(t.Context(), name)
	if err != nil {
		t.Fatalf("findBuild: %v", err)
	}

	if !ok {
		t.Fatal("findBuild found nothing for a lease that has two builds")
	}

	if b.ID != "billet-linux:zzz" {
		t.Errorf("findBuild = %s, want the newest build billet-linux:zzz — the batch's own "+
			"order was taken as the listing's", b.ID)
	}
}

// A LIVE BUILD BEATS A NEWER TERMINAL ONE, AND THIS IS THE ONE THAT FREES A RUNNING
// JOB'S CAPACITY.
//
// Custody reads a terminal answer from Find as CAUSAL PROOF and settles the lease on
// it. So `Find` returning the newest match meant that a lease whose newest build was
// terminal — while an older one was still executing somebody's job — had its capacity
// handed straight back. `List` refuses duplicates precisely to prevent that; the
// targeted lookup beside it did not, and the targeted lookup is the one custody asks.
func TestFindReturnsTheLiveBuildWhenANewerDuplicateIsTerminal(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("0123456789abcdef0123456789abcdef")

	// :aaa is older and RUNNING; :zzz is newer and over. The listing hands back
	// :zzz first.
	f.addOwnedBuildWithStatus("billet-linux:aaa", name, "IN_PROGRESS")
	f.addOwnedBuildWithStatus("billet-linux:zzz", name, "SUCCEEDED")

	inst, ok, err := p.Find(t.Context(), name)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	if !ok {
		t.Fatal("Find found nothing for a lease with a running build")
	}

	if inst.ID != "billet-linux:aaa" {
		t.Errorf("Find = %s, want the RUNNING build billet-linux:aaa", inst.ID)
	}

	// THE VERDICT IS WHAT CUSTODY ACTS ON, so it is asserted rather than the id
	// alone: a Terminal answer here settles the lease and frees its capacity.
	if inst.Terminal {
		t.Error("Find reported a lease's compute as terminal while one of its builds was " +
			"still running, so custody would take that as proof and resell the capacity " +
			"underneath somebody's job")
	}

	if !inst.Running {
		t.Error("Find reported a running build as not running")
	}
}

// AND TWO LIVE BUILDS ARE REFUSED ON THE TARGETED PATH TOO, the same answer List gives:
// billet cannot tell which one the lease's runner is inside, and the caller's next move
// on a hit is to stop what comes back.
func TestFindRefusesTwoLiveBuildsForOneLease(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("0123456789abcdef0123456789abcdef")

	f.addOwnedBuild("billet-linux:aaa", name)
	f.addOwnedBuild("billet-linux:zzz", name)

	_, _, err := p.Find(t.Context(), name)
	if err == nil {
		t.Fatal("Find named one of two live builds for a lease rather than refusing, so the " +
			"caller would stop one and leave the other running")
	}

	for _, want := range []string{"billet-linux:aaa", "billet-linux:zzz", nameEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// AN AMBIGUOUS StartBuild IS NEVER RETRIED.
//
// A dropped connection, an unparseable body and a 5xx are all outcomes where the
// request MAY have been processed. Retrying leans entirely on the idempotency token,
// which is valid for FIVE MINUTES — a wall clock, against a caller-supplied
// *http.Client whose timeout billet does not bound. Correctness that depends on the
// retry landing inside a window billet does not control is not correctness.
//
// EXACTLY ONE REQUEST IS ASSERTED, not "the launch failed": every one of these fails
// the launch either way, and the defect is the second StartBuild.
func TestAnAmbiguousStartBuildIsNeverRetried(t *testing.T) {
	for name, fault := range map[string]apiFault{
		"a 500":           {status: 500, code: "InternalServerError"},
		"a 503":           {status: 503, code: "ServiceUnavailableException"},
		"a gateway error": {status: 502, code: "BadGateway"},
		"an unknown 5xx":  {status: 599, code: "SomethingElse"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAWS(t)
			p := newTestProvider(t, f, nil)

			// More faults than one attempt consumes, so a retrying client is visible.
			f.startErr = []apiFault{fault, fault, fault, fault}

			if _, err := p.Launch(t.Context(), launchSpec(
				provider.InstanceName("0123456789abcdef0123456789abcdef"))); err == nil {
				t.Fatal("Launch reported success against a StartBuild that never answered")
			}

			if got := countTarget(f, "StartBuild"); got != 1 {
				t.Errorf("StartBuild was sent %d times for one launch; the outcome of each was "+
					"ambiguous, so every attempt after the first may be a SECOND build for one "+
					"lease — capacity charged once, compute running twice", got)
			}

			// AND THE REGISTRATION SURVIVES, which is the half the count cannot see.
			//
			// A 5xx carries an API code, and the first version of `conclusiveness`
			// asked only "does this error carry a code" — so a server error that had
			// COMMITTED the build and lost its response was read as proof nothing
			// started, and the parameter that build was about to resolve was deleted.
			// The build then starts, registers nothing, and every signal billet has
			// says the launch merely failed. The retry policy already treats a 5xx as
			// ambiguous; the two must not disagree about the same response.
			if len(f.params) == 0 {
				t.Error("the staged registration was deleted after an ambiguous launch, so a " +
					"build that committed and lost its response will resolve nothing and " +
					"register no runner")
			}
		})
	}
}

// AND A CONCLUSIVE REFUSAL DOES DELETE IT, or the fix above would be a credential leak
// wearing the clothes of a safety rule.
//
// A 4xx is the service answering NO: the request was rejected before it acted, no build
// exists, and a single-use runner registration left lying in Parameter Store is a
// credential nobody finds until it matters.
func TestAConclusivelyRefusedLaunchDeletesItsRegistration(t *testing.T) {
	for name, fault := range map[string]apiFault{
		"an invalid input":  {status: 400, code: "InvalidInputException"},
		"a quota refusal":   {status: 400, code: "AccountLimitExceededException"},
		"a denied call":     {status: 403, code: "AccessDeniedException"},
		"an absent project": {status: 400, code: "ResourceNotFoundException"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAWS(t)
			p := newTestProvider(t, f, nil)

			f.startErr = []apiFault{fault}

			if _, err := p.Launch(t.Context(), launchSpec(
				provider.InstanceName("0123456789abcdef0123456789abcdef"))); err == nil {
				t.Fatal("Launch reported success against a refused StartBuild")
			}

			if len(f.params) != 0 {
				t.Errorf("a conclusively refused launch left %d staged registration(s) in "+
					"Parameter Store; no build exists to read them", len(f.params))
			}
		})
	}
}

// AND A THROTTLE STILL RETRIES, because AWS answered and its answer is that it did not
// act. Refusing to retry a refusal billet actually received would make a busy account
// fail launches it could have served, which is the opposite error and just as real.
func TestAThrottledStartBuildIsRetried(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	f.startErr = []apiFault{{status: 400, code: "ThrottlingException"}}

	if _, err := p.Launch(t.Context(), launchSpec(
		provider.InstanceName("0123456789abcdef0123456789abcdef"))); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := countTarget(f, "StartBuild"); got != 2 {
		t.Errorf("StartBuild was sent %d time(s); a throttle is a refusal billet received and "+
			"is the one outcome worth asking again about", got)
	}
}

// countTarget counts the requests the fake received for one action.
func countTarget(f *fakeAWS, action string) int {
	n := 0

	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, "."+action) {
			n++
		}
	}

	return n
}
