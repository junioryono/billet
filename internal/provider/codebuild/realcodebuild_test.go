package codebuild

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A real build, started, found, listed and stopped through real CodeBuild.
//
// THE STUB TESTS ASSERT WHAT BILLET SAYS; THESE ASSERT THAT WHAT IT SAYS MATCHES WHAT
// CODEBUILD DOES, which is the same argument realtart_test.go and realfirecracker_test.go
// make one backend over: the firecracker backend's two launch-killing defects survived
// every unit test and died on the first real launch. A fake models this repository's
// UNDERSTANDING of the API, and three of the nine things measured while writing this
// backend contradicted that understanding — most sharply, a build CodeBuild timed out
// reports `buildStatus: FAILED` with the timeout only in `phases[]`, so a predicate over
// the status alone could never fire for the case it existed for.
//
// WHAT IT NEEDS. Real AWS credentials in the environment (billet reads env-var or IMDSv2
// only), and a CodeBuild project it may start NO_SOURCE builds in. Both are named by
// environment variables so this can never run by accident: an acceptance test that
// silently starts billable compute in whatever account a contributor happens to be
// signed into is worse than one that skips.
//
// IT COSTS REAL MONEY, a few cents: BUILD_GENERAL1_SMALL, billed by the minute, and each
// case stops its build as soon as it has what it needs.

// realProject is the project these tests may use, or "" to skip them all.
func realProject(t *testing.T) string {
	t.Helper()

	project := os.Getenv("BILLET_TEST_CODEBUILD_PROJECT")
	if project == "" {
		t.Skip("set BILLET_TEST_CODEBUILD_PROJECT to a NO_SOURCE CodeBuild project this " +
			"test may start billable builds in, with AWS credentials in the environment")
	}

	if os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		t.Skip("no AWS credentials in the environment; billet reads env-var or IMDSv2 only")
	}

	return project
}

// realProvider builds a provider against real CodeBuild and real Parameter Store.
func realProvider(t *testing.T, project string) *Provider {
	t.Helper()

	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-west-2"
	}

	cfg := config.CodeBuildConfig{
		Region:                     region,
		Project:                    project,
		EnvironmentType:            config.CodeBuildLinuxContainer,
		AcceptExternalBuildCeiling: true,
		JITParameterPath:           "/billet/realtest/jit",
		// THE TIGHTEST CEILINGS THE SERVICE ALLOWS, because they size the inventory
		// walk: five minutes each makes `List` walk about an hour of history rather
		// than the 45 hours a default configuration implies.
		BuildTimeoutMinutes:  config.CodeBuildBuildFloorMinutes,
		QueuedTimeoutMinutes: config.CodeBuildQueuedFloorMinutes,
		ComputeTypes: []config.RemoteShape{{
			Type: "BUILD_GENERAL1_SMALL", VCPU: 2, Memory: 3 * config.GiB,
			PriceUSDPerHour: 300_000,
		}},
	}

	p, err := New(realOwner, cfg, WithCredentials(envCredentials{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return p
}

// realOwner marks every build these tests start, so anything left behind is
// attributable and nothing here can ever match an operator's own compute.
const realOwner = "dep-billetrealtest0"

// envCredentials reads the ordinary AWS environment variables.
//
// A LOCAL IMPLEMENTATION RATHER THAN ec2.DefaultCredentials, because this package
// deliberately declares its own interface over awssig.Credentials and depguard keeps the
// two provider packages apart — cmd/billet is where the real chain is adapted in.
type envCredentials struct{}

func (envCredentials) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}, nil
}

// realLease is a lease id no earlier run can have used.
//
// A FIXED ONE REPLAYS AN EARLIER BUILD, and the first version of this file used four
// constants — which is how the five-minute idempotency token turned into a test failure
// that looked like a provider bug. `idempotencyTokenFor` derives its token from the lease
// name and the compute type, so a second run inside that window sent an identical
// StartBuild and CodeBuild returned THE SAME BUILD as before: by then terminal, so
// `Running` was false and `List` correctly excluded it. Nothing was broken except the
// test's assumption that a name is free.
//
// That is worth keeping rather than only working around: it is live confirmation that
// the token deduplicates ACROSS PROCESSES, which is the property the launch path's
// no-retry-on-ambiguity rule is written against.
//
// THIRTY-TWO HEX CHARACTERS, because that is the shape of a real lease id and the
// parameter path and build markers are derived from it.
func realLease(t *testing.T) string {
	t.Helper()

	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate a lease id: %v", err)
	}

	return hex.EncodeToString(b[:])
}

// reapAfter guarantees that nothing this test starts outlives it.
//
// REGISTERED BEFORE THE LAUNCH, NOT AFTER, and that ordering is the whole point. A
// cleanup registered after a successful Launch cannot cover the case the launch path is
// most careful about: an AMBIGUOUS StartBuild returns an error while a build may already
// be running, so a test that registered its cleanup on the success path alone would
// leave billable compute behind on exactly the failure the provider exists to handle.
//
// IT PROVES BEFORE IT DELETES, in both directions, and a first version did neither.
// `Find` is eventually consistent, so ONE absent answer is not proof a build never
// started — it is polled. `Destroy` answering TeardownRequested explicitly means the
// compute is NOT yet proved stopped, so it is retried rather than believed. And the
// staged registration is removed only once the build is proved gone: deleting it while a
// build is still starting is the "runner that never registers" failure the provider's
// own contract is arranged against, and a cleanup that reproduced it would be teaching
// the wrong lesson in the file most likely to be copied.
//
// AND IT RUNS ON A DETACHED CONTEXT. t.Context() is cancelled JUST BEFORE cleanups run,
// so using it directly here would hand every teardown call a dead context — the one
// moment billable compute is most likely to be left running. WithoutCancel keeps the
// values and drops the cancellation.
func reapAfter(t *testing.T, p *Provider, name string) {
	t.Helper()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 8*time.Minute)
		defer cancel()

		gone, err := proveGone(ctx, p, name)
		if err != nil {
			t.Errorf("cleanup: %s may still be running: %v", name, err)

			return
		}

		if !gone {
			t.Errorf("cleanup: could not prove %s is stopped; check the project by hand", name)

			return
		}

		// ONLY NOW. A single-use registration left in Parameter Store is one nobody
		// finds until it matters, and a failed test is exactly when one gets left —
		// but a build that is still starting has not read it yet.
		if err := p.ReapStagedCredential(ctx, name); err != nil {
			t.Errorf("cleanup: staged registration for %s may remain: %v", name, err)
		}
	})
}

// proveGone polls until this lease's build is provably terminal, or provably absent
// across enough observations that a create cannot still be in flight.
//
// THE ABSENCE RULE IS THE PROVIDER'S OWN, one layer up: a remote inventory is eventually
// consistent, so a single miss is not evidence. Several consecutive misses across a
// window longer than a create takes to appear is as close as a test can get, and it
// REPORTS rather than assuming when it runs out of budget.
func proveGone(ctx context.Context, p *Provider, name string) (bool, error) {
	misses := 0

	for attempt := range 24 {
		if attempt > 0 {
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}

		inst, ok, err := p.Find(ctx, name)
		if err != nil {
			return false, err
		}

		if !ok {
			// A build that never started, or one not yet visible. Only a sustained
			// run of misses separates those.
			misses++
			if misses >= 6 {
				return true, nil
			}

			continue
		}

		misses = 0

		if inst.Terminal {
			return true, nil
		}

		// TeardownRequested is not proof, so this loops rather than returning.
		if _, err := p.Destroy(ctx, inst.ID); err != nil {
			return false, err
		}
	}

	return false, nil
}

// findWithin polls Find until the build appears, or the budget runs out.
func findWithin(
	ctx context.Context, t *testing.T, p *Provider, name string,
) (*provider.Instance, bool) {
	t.Helper()

	for attempt := range 12 {
		if attempt > 0 {
			time.Sleep(5 * time.Second)
		}

		inst, ok, err := p.Find(ctx, name)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}

		if ok {
			return inst, true
		}
	}

	return nil, false
}

// findBuildWithin is findWithin for the unprojected build, which carries the phase and
// the environment the secrecy assertions read.
func findBuildWithin(
	ctx context.Context, t *testing.T, p *Provider, name string,
) (build, bool) {
	t.Helper()

	for attempt := range 12 {
		if attempt > 0 {
			time.Sleep(5 * time.Second)
		}

		b, ok, err := p.findBuild(ctx, name)
		if err != nil {
			t.Fatalf("findBuild: %v", err)
		}

		if ok {
			return b, true
		}
	}

	return build{}, false
}

// THE WHOLE LIFECYCLE, AGAINST THE REAL API.
//
// Launch stages a SecureString and starts a build; Find matches it back by the lease
// marker alone (a build cannot be tagged, so those markers are the entire ownership
// boundary); List reports it as running; Destroy polls it to a terminal state and
// removes the registration. Every one of those is a claim about somebody else's API.
func TestARealBuildRunsTheWholeLifecycle(t *testing.T) {
	project := realProject(t)
	p := realProvider(t, project)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	name := provider.InstanceName(realLease(t))
	reapAfter(t, p, name)

	inst, err := p.Launch(ctx, provider.Spec{
		Name:      name,
		Image:     "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		VCPU:      2,
		Memory:    3 * config.GiB,
		Command:   []string{"true"},
		Trust:     provider.TrustTrusted,
		JITConfig: "BILLET-REAL-TEST-REGISTRATION",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	t.Logf("started build %s", inst.ID)

	// ALWAYS TEARDOWN, on every exit path: this is billable compute and a test that
	// fails must not leave it running.
	if !inst.Running {
		t.Errorf("a freshly started build reports Running=false")
	}

	// FOUND BY ITS LEASE MARKER, which is the only durable link between a running
	// build and the lease that authorised it.
	//
	// POLLED, because this inventory is eventually consistent and the provider says so
	// itself: asking once immediately after StartBuild makes the test fail on AWS's
	// replication rather than on anything billet did.
	found, ok := findWithin(ctx, t, p, name)
	if !ok {
		t.Fatal("Find never saw a build billet had just started; every ownership rule " +
			"on this backend rests on those environment markers surviving the round trip")
	}

	if found.ID != inst.ID {
		t.Errorf("Find = %s, want the build Launch started (%s)", found.ID, inst.ID)
	}

	// AND REPORTED BY List, which is what reconciliation reads — and which frees the
	// capacity of every lease ABSENT from its answer.
	var seen bool

	for attempt := range 12 {
		if attempt > 0 {
			time.Sleep(5 * time.Second)
		}

		listed, err := p.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		for _, i := range listed {
			if i.Name == name {
				seen = true
			}
		}

		if seen {
			break
		}
	}

	if !seen {
		t.Error("List never reported a running build billet owns; reconciliation reads " +
			"this answer and frees the capacity of every lease absent from it")
	}
}

// AND THE REGISTRATION NEVER LEAVES PARAMETER STORE.
//
// The whole secret channel rests on this, and no AWS document states it. Measured
// separately on 2026-08-31 and asserted here so it stays measured: what BatchGetBuilds
// reports for the JIT variable is the parameter's NAME, and the staged value appears
// nowhere in the response.
func TestARealBuildNeverEchoesItsRegistration(t *testing.T) {
	project := realProject(t)
	p := realProvider(t, project)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	const secret = "BILLET-REAL-TEST-REGISTRATION-MUST-NOT-APPEAR"

	name := provider.InstanceName(realLease(t))
	reapAfter(t, p, name)

	if _, err := p.Launch(ctx, provider.Spec{
		Name:      name,
		Image:     "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		VCPU:      2,
		Memory:    3 * config.GiB,
		Command:   []string{"true"},
		Trust:     provider.TrustTrusted,
		JITConfig: secret,
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	b, ok := findBuildWithin(ctx, t, p, name)
	if !ok {
		t.Fatal("findBuild never saw the build")
	}

	// THE ENVIRONMENT AS THE SERVICE REPORTS IT. A PARAMETER_STORE variable must come
	// back as its NAME; anything else and this backend's secret channel is not one.
	var jit envVar

	for _, e := range b.Environment.EnvironmentVariables {
		if e.Name == jitEnvVar {
			jit = e
		}
	}

	if jit.Name == "" {
		t.Fatal("the build carries no JIT variable at all, so the assertion below is vacuous")
	}

	if jit.Type != "PARAMETER_STORE" {
		t.Errorf("the JIT variable is typed %q, not PARAMETER_STORE — its value would then "+
			"be the registration itself, rendered in the console and in CloudTrail", jit.Type)
	}

	// THE EXACT NAME, not a prefix. A prefix match accepts a build pointing at a
	// SIBLING deployment's parameter, which is the one mistake this path must not make.
	if want := p.jitParameterName(name); jit.Value != want {
		t.Errorf("the JIT variable names %q, want exactly %q", jit.Value, want)
	}

	// AND THE SENTINEL APPEARS NOWHERE IN THE WHOLE BUILD RECORD, which is what the
	// claim actually is. Checking only the JIT variable's own value would still pass if
	// a regression rendered the registration into some OTHER field.
	for _, e := range b.Environment.EnvironmentVariables {
		if strings.Contains(e.Value, secret) {
			t.Errorf("the staged registration was echoed back in environment variable %q",
				e.Name)
		}
	}

	if raw, err := json.Marshal(b); err != nil {
		t.Errorf("marshal the build record: %v", err)
	} else if strings.Contains(string(raw), secret) {
		t.Error("the staged registration appears somewhere in the build record " +
			"BatchGetBuilds returned")
	}
}

// AND A DESTROY POLLS TO A TERMINAL STATE RATHER THAN BELIEVING StopBuild.
//
// A stop is a REQUEST. Measured: StopBuild against an already-terminal build SUCCEEDS
// rather than erroring, so its return value proves nothing about whether the compute is
// gone — TeardownStopped is the answer that frees capacity, and it may only follow an
// observed terminal state.
func TestARealDestroyProvesTheBuildIsOver(t *testing.T) {
	project := realProject(t)
	p := realProvider(t, project)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	name := provider.InstanceName(realLease(t))
	reapAfter(t, p, name)

	inst, err := p.Launch(ctx, provider.Spec{
		Name:      name,
		Image:     "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		VCPU:      2,
		Memory:    3 * config.GiB,
		Command:   []string{"true"},
		Trust:     provider.TrustTrusted,
		JITConfig: "BILLET-REAL-TEST-REGISTRATION",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	teardown, err := p.Destroy(ctx, inst.ID)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if teardown != provider.TeardownStopped {
		t.Fatalf("Destroy = %v, want TeardownStopped once the build settled", teardown)
	}

	// AND THE BUILD REALLY IS OVER, asked of the service rather than inferred from the
	// verdict billet just returned.
	b, ok, err := p.findBuild(ctx, name)
	if err != nil {
		t.Fatalf("findBuild after destroy: %v", err)
	}

	if !ok {
		t.Fatal("the build vanished from the inventory window entirely")
	}

	if !terminalStatus(b.BuildStatus) {
		t.Errorf("Destroy reported TeardownStopped while the build is %s, so capacity would "+
			"be freed for compute that is still running", b.BuildStatus)
	}

	// AND THE REGISTRATION IS GONE, which is the teardown path's other job: a
	// single-use credential left in Parameter Store is one nobody finds until it
	// matters.
	//
	// ASKED BY WRITING, NOT BY DELETING, and the first version of this assertion got
	// that exactly backwards. `deleteJITConfig` answers nil for a parameter that is
	// ALREADY ABSENT — deliberately, because that idempotency is what lets settlement
	// reap something the teardown path already removed — so `err == nil` says nothing
	// about whether anything was there. The real run is what exposed it: the assertion
	// fired against a correctly-cleaned parameter.
	//
	// `putJITConfig` is no-clobber (`Overwrite: false`), so a SUCCESSFUL write proves
	// the name was free. That also keeps the check inside the node's own grant, which
	// deliberately carries no ssm:GetParameter: billet writes and deletes
	// registrations, it never reads them back, and a test that needed a read would be
	// testing something the shipped policy cannot do.
	if err := p.putJITConfig(ctx, name, "probe"); err != nil {
		t.Errorf("the staged registration was still present after a confirmed teardown, "+
			"so a single-use credential outlives the build it was minted for: %v", err)
	}

	if err := p.deleteJITConfig(ctx, name); err != nil {
		t.Errorf("cleanup: remove the probe parameter: %v", err)
	}
}

// AND A DESTROY OF SOMETHING ALREADY GONE IS IDEMPOTENT, because billet retries
// teardowns and the ordinary completed-job path asks after the runner has already ended
// the build on its own.
func TestARealDestroyIsIdempotent(t *testing.T) {
	project := realProject(t)
	p := realProvider(t, project)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()

	name := provider.InstanceName(realLease(t))
	reapAfter(t, p, name)

	inst, err := p.Launch(ctx, provider.Spec{
		Name:      name,
		Image:     "aws/codebuild/amazonlinux-x86_64-standard:5.0",
		VCPU:      2,
		Memory:    3 * config.GiB,
		Command:   []string{"true"},
		Trust:     provider.TrustTrusted,
		JITConfig: "BILLET-REAL-TEST-REGISTRATION",
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if _, err := p.Destroy(ctx, inst.ID); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}

	teardown, err := p.Destroy(ctx, inst.ID)
	if err != nil {
		t.Errorf("a second Destroy of the same build failed: %v", err)
	}

	if teardown != provider.TeardownStopped {
		t.Errorf("second Destroy = %v, want TeardownStopped for a build already over",
			teardown)
	}
}
