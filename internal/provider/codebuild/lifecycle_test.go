package codebuild

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// A LAUNCH STARTS ONE BUILD AND CARRIES ITS MARKERS.
//
// The happy path first, because a suite of refusals passes when the fixture is
// broken for an unrelated reason and proves nothing about any of the rules.
func TestALaunchStartsOneBuildCarryingItsOwnerAndLeaseMarkers(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	inst, err := p.Launch(t.Context(), launchSpec(name))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if inst.ID == "" {
		t.Error("the launch returned no build id, so nothing could ever be torn down")
	}

	if inst.Name != name {
		t.Errorf("the build's name is %q, want %q", inst.Name, name)
	}

	if !inst.Running {
		t.Error("a build that has just started is not reported as running")
	}

	if inst.Terminal {
		t.Error("a build that has just started is reported as terminal")
	}

	if len(f.builds) != 1 {
		t.Fatalf("the launch created %d builds, want exactly 1", len(f.builds))
	}

	for _, b := range f.builds {
		owner, ok := envValue(b.env, ownerEnvVar)
		if !ok || owner != testOwner {
			t.Errorf("the build carries owner marker %q (present=%v), want %q", owner, ok, testOwner)
		}

		lease, ok := envValue(b.env, nameEnvVar)
		if !ok || lease != name {
			t.Errorf("the build carries lease marker %q (present=%v), want %q", lease, ok, name)
		}
	}
}

// THE REGISTRATION NEVER TRAVELS IN A REQUEST BILLET SENDS TO CODEBUILD.
//
// This is the whole security claim of the backend, and it is asserted over EVERY
// recorded request body rather than over the one billet meant to put it in — because
// the failure this guards is a value reaching a request nobody was thinking about.
// The one place it is allowed is the PutParameter body, which is the channel.
func TestTheRegistrationOnlyEverTravelsToParameterStore(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	if _, err := p.Launch(t.Context(), launchSpec(name)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// And every read path too, because Find and List decode a response that echoes
	// the build's environment back.
	if _, _, err := p.Find(t.Context(), name); err != nil {
		t.Fatalf("Find: %v", err)
	}

	if _, err := p.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}

	staged := 0

	for _, r := range f.bodies() {
		carries := strings.Contains(r.body, theRegistration)

		if strings.HasSuffix(r.target, ".PutParameter") {
			if !carries {
				t.Error("the PutParameter request did not carry the registration, so nothing " +
					"would have been staged")
			}

			staged++

			continue
		}

		if carries {
			t.Errorf("%s carried the runner registration in its request body", r.target)
		}
	}

	if staged != 1 {
		t.Errorf("the registration was staged %d times, want exactly 1", staged)
	}
}

// AND THE LAUNCH REQUEST CARRIES THE PARAMETER'S NAME, TYPED AS A REFERENCE.
//
// The previous test proves the value did not travel. This proves the REFERENCE did,
// which is the other half: a launch that carried neither would start a build with no
// registration at all, and the leak assertion above would pass.
func TestTheLaunchRequestReferencesTheParameterRatherThanItsValue(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	if _, err := p.Launch(t.Context(), launchSpec(name)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	var start string

	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, ".StartBuild") {
			start = r.body
		}
	}

	if start == "" {
		t.Fatal("no StartBuild request was recorded")
	}

	if !strings.Contains(start, `"name":"`+jitEnvVar+`"`) {
		t.Errorf("the launch does not hand the runner its registration variable at all: %s", start)
	}

	if !strings.Contains(start, `"type":"PARAMETER_STORE"`) {
		t.Errorf("the registration variable is not typed PARAMETER_STORE, so its value would "+
			"be sent in the clear: %s", start)
	}

	if !strings.Contains(start, p.jitParameterName(name)) {
		t.Errorf("the launch does not name the staged parameter: %s", start)
	}
}

// AND IT NEVER REACHES THE BUILDSPEC EITHER, which is the request field most likely
// to accumulate one: a buildspec is generated shell, and shell is where a value gets
// interpolated by somebody being helpful.
func TestTheBuildspecNeverMentionsTheRegistrationVariable(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	spec, err := p.Buildspec(Spec{Name: "billet-abc", Command: []string{"./run.sh"}})
	if err != nil {
		t.Fatalf("Buildspec: %v", err)
	}

	if strings.Contains(spec, jitEnvVar) {
		t.Errorf("the buildspec names %s, so a command could echo it into a build log:\n%s",
			jitEnvVar, spec)
	}

	if strings.Contains(spec, theRegistration) {
		t.Errorf("the buildspec carries a registration value:\n%s", spec)
	}
}

// A STAGED REGISTRATION IS REMOVED WHEN THE LAUNCH FAILS, because nothing consumed
// it and an unmentioned leftover credential is what nobody finds until it matters.
func TestAFailedLaunchRemovesItsStagedRegistration(t *testing.T) {
	f := newFakeAWS(t)
	f.startErr = []apiFault{{status: http.StatusBadRequest, code: "InvalidInputException"}}

	p := newTestProvider(t, f, nil)

	if _, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc123"))); err == nil {
		t.Fatal("a refused StartBuild produced no launch error")
	}

	if len(f.params) != 0 {
		t.Errorf("a failed launch left %d staged registration(s) behind: %v",
			len(f.params), f.params)
	}
}

// AND A LAUNCH REFUSED BEFORE ANYTHING WAS STAGED STAGES NOTHING.
//
// The ordering matters in this direction too: a registration written before the
// checks would leave a credential behind for every tier misconfiguration, and the
// test above would still pass because it only proves cleanup happens.
func TestARefusedLaunchStagesNothing(t *testing.T) {
	for name, mutate := range map[string]func(*provider.Spec){
		"untrusted": func(s *provider.Spec) { s.Trust = provider.TrustUntrusted },
		"unknown trust": func(s *provider.Spec) {
			s.Trust = provider.TrustUnknown
		},
		"no command": func(s *provider.Spec) { s.Command = nil },
		"no jit":     func(s *provider.Spec) { s.JITConfig = "" },
		"oversize":   func(s *provider.Spec) { s.VCPU = 999 },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAWS(t)
			p := newTestProvider(t, f, nil)

			spec := launchSpec(provider.InstanceName("abc123"))
			mutate(&spec)

			if _, err := p.Launch(t.Context(), spec); err == nil {
				t.Fatal("the launch was accepted and it should not have been")
			}

			if len(f.params) != 0 {
				t.Errorf("a refused launch staged %d registration(s): %v", len(f.params), f.params)
			}

			if len(f.builds) != 0 {
				t.Errorf("a refused launch started %d build(s)", len(f.builds))
			}
		})
	}
}

// untrustedNet is the isolated network an untrusted on-demand tier declares.
var untrustedNet = func(cfg *config.CodeBuildConfig) {
	cfg.UntrustedVPCID = "vpc-0fork"
	cfg.UntrustedSubnetIDs = []string{"subnet-0fork"}
	cfg.UntrustedSecurityGroupIDs = []string{"sg-0fork"}
}

// UNTRUSTED WORK ON AN ON-DEMAND CONTAINER TIER IS REFUSED UNTIL A NETWORK IS
// DECLARED, and its absence is the refusal — the same shape as
// node.ec2.untrusted_security_group_ids.
//
// The message must name the fields, because an operator whose fork PRs stop running
// needs to know which knob turns them back on rather than reaching for one that does
// not exist.
func TestUntrustedIsRefusedWithNoIsolatedNetwork(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil) // on-demand LINUX_CONTAINER, no untrusted_*

	err := p.Accepts(provider.TrustUntrusted)
	if err == nil {
		t.Fatal("untrusted work was accepted with no isolated network")
	}

	for _, want := range []string{"untrusted_vpc_id", "untrusted_subnets", "untrusted_security_group_ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q, so an operator cannot act on it: %v", want, err)
		}
	}

	// AND UNKNOWN IS A DIFFERENT JUDGEMENT, so it gets a different sentence:
	// untrusted is a classification billet made, unknown means it could not
	// classify the job at all.
	unknown := p.Accepts(provider.TrustUnknown)
	if unknown == nil {
		t.Fatal("unclassified work was accepted")
	}

	if unknown.Error() == err.Error() {
		t.Error("untrusted and unclassified work get the same refusal, which conflates a " +
			"decision billet made with one it could not make")
	}

	if p.Accepts(provider.TrustTrusted) != nil {
		t.Error("trusted work was refused, so this backend can never run anything")
	}
}

// UNTRUSTED WORK ON A RESERVED FLEET IS REFUSED EVEN THOUGH THE FLEET IS WHAT MACOS
// NEEDS, because a reserved instance is shared between builds and a fleetOverride
// discards the project's network — so no isolation holds. A network cannot be
// declared beside a fleet (config refuses that), so the reachable shape is a fleet
// tier asked to run untrusted work, which Accepts refuses with the fleet reason.
func TestUntrustedIsRefusedOnAReservedFleet(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.EnvironmentType = config.CodeBuildMacARM
		cfg.PrivilegedMode = false
		cfg.FleetARN = "arn:aws:codebuild:us-west-2:000000000000:fleet/macs:11111111-1111-1111-1111-111111111111"
	})

	err := p.Accepts(provider.TrustUntrusted)
	if err == nil {
		t.Fatal("untrusted work was accepted on a reserved fleet")
	}

	for _, want := range []string{"reserved-capacity", "fleetOverride", "sharing cached data"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the fleet refusal does not name %q: %v", want, err)
		}
	}
}

// UNTRUSTED WORK IS ADMITTED ON AN ON-DEMAND CONTAINER TIER WITH A DECLARED
// NETWORK, and only then, AND THE LAUNCH VERIFIES THE PROJECT CARRIES IT. Accepts
// and the launch share one function (untrustedNetwork), so admission and the network
// it requires cannot drift; the launch check is what makes the config real, because
// StartBuild has no VPC override.
func TestUntrustedIsAdmittedAndTheLaunchVerifiesTheProjectNetwork(t *testing.T) {
	// The declared network matches the project → accepted and launched.
	t.Run("matching project network launches", func(t *testing.T) {
		f := newFakeAWS(t)
		f.projectVPC = &fakeVPC{
			vpcID: "vpc-0fork", subnets: []string{"subnet-0fork"}, securityGroupIDs: []string{"sg-0fork"},
		}
		p := newTestProvider(t, f, untrustedNet)

		if err := p.Accepts(provider.TrustUntrusted); err != nil {
			t.Fatalf("untrusted work was refused with a declared network: %v", err)
		}

		spec := launchSpec(provider.InstanceName("abc123"))
		spec.Trust = provider.TrustUntrusted
		if _, err := p.Launch(t.Context(), spec); err != nil {
			t.Fatalf("a matching untrusted launch was refused: %v", err)
		}

		if len(f.builds) != 1 {
			t.Fatalf("a verified untrusted launch started %d build(s), want 1", len(f.builds))
		}
	})

	// Order and duplicates are not a mismatch: AWS reports a set, and a set with a
	// repeated member is the same network.
	t.Run("reordered and duplicated ids still match", func(t *testing.T) {
		f := newFakeAWS(t)
		f.projectVPC = &fakeVPC{
			vpcID: "vpc-0fork", subnets: []string{"subnet-b", "subnet-a", "subnet-a"},
			securityGroupIDs: []string{"sg-0fork"},
		}
		p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
			untrustedNet(cfg)
			cfg.UntrustedSubnetIDs = []string{"subnet-a", "subnet-b"}
		})

		spec := launchSpec(provider.InstanceName("abc123"))
		spec.Trust = provider.TrustUntrusted
		if _, err := p.Launch(t.Context(), spec); err != nil {
			t.Fatalf("a set-equal network was refused over order or duplicates: %v", err)
		}
	})

	// EVERY WAY THE VERIFICATION CAN FAIL TO PROVE THE NETWORK refuses the launch,
	// with nothing staged and nothing started. Each row is a branch of
	// assertUntrustedNetwork; a fake that could not produce it would leave that
	// branch as prose.
	for name, arrange := range map[string]func(*fakeAWS){
		"no vpc on the project": func(f *fakeAWS) { f.projectVPC = nil },
		"different vpc": func(f *fakeAWS) {
			f.projectVPC = &fakeVPC{
				vpcID: "vpc-DEFAULT", subnets: []string{"subnet-0fork"}, securityGroupIDs: []string{"sg-0fork"},
			}
		},
		"extra subnet the config did not declare": func(f *fakeAWS) {
			f.projectVPC = &fakeVPC{
				vpcID: "vpc-0fork", subnets: []string{"subnet-0fork", "subnet-extra"},
				securityGroupIDs: []string{"sg-0fork"},
			}
		},
		"declared subnet missing from the project": func(f *fakeAWS) {
			f.projectVPC = &fakeVPC{
				vpcID: "vpc-0fork", subnets: []string{"subnet-other"}, securityGroupIDs: []string{"sg-0fork"},
			}
		},
		"extra security group the config did not declare": func(f *fakeAWS) {
			f.projectVPC = &fakeVPC{
				vpcID: "vpc-0fork", subnets: []string{"subnet-0fork"},
				securityGroupIDs: []string{"sg-0fork", "sg-wide-open"},
			}
		},
		"declared security group missing from the project": func(f *fakeAWS) {
			f.projectVPC = &fakeVPC{
				vpcID: "vpc-0fork", subnets: []string{"subnet-0fork"}, securityGroupIDs: []string{"sg-other"},
			}
		},
		"project does not exist": func(f *fakeAWS) { f.projectMissing = true },
		"BatchGetProjects fails": func(f *fakeAWS) {
			f.projectErr = []apiFault{{status: http.StatusInternalServerError, code: "InternalServerError"}}
		},
	} {
		t.Run("refuses: "+name, func(t *testing.T) {
			f := newFakeAWS(t)
			arrange(f)
			p := newTestProvider(t, f, untrustedNet)

			spec := launchSpec(provider.InstanceName("abc123"))
			spec.Trust = provider.TrustUntrusted
			if _, err := p.Launch(t.Context(), spec); err == nil {
				t.Fatal("an untrusted launch onto an unverified network was accepted")
			}

			if len(f.params) != 0 {
				t.Errorf("a refused untrusted launch staged %d registration(s)", len(f.params))
			}

			if len(f.builds) != 0 {
				t.Errorf("a refused untrusted launch started %d build(s)", len(f.builds))
			}
		})
	}
}

// A CALLER THAT WIDENS ITS NETWORK SLICES AFTER CONSTRUCTION CHANGES NOTHING. The
// provider clones them in New for the reason ec2.New clones its security groups:
// the declared network is what the launch verifies the project against, and a
// slice the caller still holds is a way to move that comparison after the fact.
func TestTheUntrustedNetworkCannotBeWidenedAfterConstruction(t *testing.T) {
	// The project carries a subnet the operator never declared. The declared
	// network says subnet-0fork, so the launch must refuse — unless the caller can
	// rewrite the declaration through the slice it kept, which is what a missing
	// clone would allow: the provider would then compare subnet-attacker against
	// subnet-attacker and start the build.
	f := newFakeAWS(t)
	f.projectVPC = &fakeVPC{
		vpcID: "vpc-0fork", subnets: []string{"subnet-attacker"}, securityGroupIDs: []string{"sg-0fork"},
	}

	subnets := []string{"subnet-0fork"}
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		untrustedNet(cfg)
		cfg.UntrustedSubnetIDs = subnets
	})

	subnets[0] = "subnet-attacker"

	spec := launchSpec(provider.InstanceName("abc123"))
	spec.Trust = provider.TrustUntrusted
	if _, err := p.Launch(t.Context(), spec); err == nil {
		t.Fatal("the declared network was rewritten through a slice the caller kept")
	}

	if len(f.builds) != 0 {
		t.Errorf("%d build(s) started on a rewritten network", len(f.builds))
	}
}

// A DESTROY POLLS TO A TERMINAL STATE BEFORE CLAIMING ONE.
//
// StopBuild is a REQUEST. The fake models that — a stop moves the build to STOPPING
// and only a later look finds STOPPED — so a Destroy that returned TeardownStopped
// on the strength of the request being accepted fails here.
func TestDestroyConfirmsTheBuildStoppedBeforeReportingItStopped(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	inst, err := p.Launch(t.Context(), launchSpec(name))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	teardown, err := p.Destroy(t.Context(), inst.ID)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if teardown != provider.TeardownStopped {
		t.Errorf("Destroy = %v, want TeardownStopped once the build reached a terminal state",
			teardown)
	}

	// AND IT ACTUALLY ASKED. Without this the assertion above passes against a
	// Destroy that returns TeardownStopped unconditionally.
	polls := 0

	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, ".BatchGetBuilds") {
			polls++
		}
	}

	if polls == 0 {
		t.Error("Destroy reported a confirmed stop without ever describing the build")
	}

	// THE CREDENTIAL GOES WITH IT, once the build is over and nothing can read it.
	if len(f.params) != 0 {
		t.Errorf("a completed teardown left %d staged registration(s): %v", len(f.params), f.params)
	}
}

// A TEARDOWN THAT CANNOT CONFIRM KEEPS THE CAPACITY CHARGED.
//
// Every ambiguous answer must come back TeardownRequested with NO error: an error
// makes the caller retry a stop forever, and TeardownStopped frees capacity for
// compute that may still be running somebody's deploy.
func TestAnUnconfirmedTeardownIsRequestedRatherThanStopped(t *testing.T) {
	t.Run("build unknown to codebuild", func(t *testing.T) {
		f := newFakeAWS(t)
		p := newTestProvider(t, f, nil)

		teardown, err := p.Destroy(t.Context(), "billet-linux:never-existed")
		if err != nil {
			t.Fatalf("destroying an unknown build errored: %v", err)
		}

		if teardown != provider.TeardownRequested {
			t.Errorf("Destroy = %v, want TeardownRequested: an id CodeBuild does not know is "+
				"not a build billet observed stop", teardown)
		}
	})

	t.Run("build never settles", func(t *testing.T) {
		f := newFakeAWS(t)
		p := newTestProvider(t, f, nil)

		inst, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc123")))
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}

		// The fake settles STOPPING on the next look, so a build that refuses to
		// settle has to be built by hand: pin it IN_PROGRESS through every poll.
		f.builds[inst.ID].status = "IN_PROGRESS"
		f.stopErr = []apiFault{{status: http.StatusBadRequest, code: "InvalidInputException"}}

		teardown, err := p.Destroy(t.Context(), inst.ID)
		if err != nil {
			t.Fatalf("Destroy: %v", err)
		}

		if teardown != provider.TeardownRequested {
			t.Errorf("Destroy = %v, want TeardownRequested for a build that never reached a "+
				"terminal state", teardown)
		}

		// AND THE CREDENTIAL STAYS, because the build may still read it.
		if len(f.params) != 1 {
			t.Errorf("a build still running had its registration removed: %v", f.params)
		}
	})

	t.Run("a cancelled poll is not a stop", func(t *testing.T) {
		f := newFakeAWS(t)
		p := newTestProvider(t, f, nil)

		inst, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc123")))
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}

		f.builds[inst.ID].status = "IN_PROGRESS"

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		// A cancelled context makes StopBuild itself fail, which is an error — the
		// point of this case is the one below it, where the stop lands and the poll
		// is what gets cancelled. The VERDICT is what matters here, so the error is
		// asserted to exist rather than discarded: a Destroy that returned
		// TeardownRequested with no error would be claiming it had looked.
		teardown, err := p.Destroy(ctx, inst.ID)
		if err == nil {
			t.Error("Destroy reported no error on a cancelled context, so nothing says why " +
				"the teardown is unconfirmed")
		}

		if teardown != provider.TeardownRequested {
			t.Errorf("Destroy = %v on a cancelled context, want TeardownRequested", teardown)
		}
	})
}

// LIST REPORTS RUNNING BUILDS AND EXCLUDES TERMINAL ONES.
//
// The exclusion is the liveStates half of the ec2 split: CodeBuild retains a year of
// history, so reporting terminal builds would hand reconciliation a year of corpses
// to stop on every pass.
func TestListReportsRunningBuildsAndNotHistory(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	live := provider.InstanceName("aaa")
	dead := provider.InstanceName("bbb")

	if _, err := p.Launch(t.Context(), launchSpec(live)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	deadInst, err := p.Launch(t.Context(), launchSpec(dead))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	f.builds[deadInst.ID].status = "SUCCEEDED"

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// AN ORDINARY OWNED RUNNING BUILD IS LISTED, which is the case the ec2 backend
	// had four tests about what List must NOT report and none about this — so a List
	// that dropped every build it owned passed all four.
	if len(got) != 1 {
		t.Fatalf("List returned %d builds, want exactly the running one: %+v", len(got), got)
	}

	if got[0].Name != live {
		t.Errorf("List reported %q, want the running build %q", got[0].Name, live)
	}
}

// A FOREIGN BUILD IN THE PROJECT IS SKIPPED, NOT REFUSED.
//
// A project is supposed to be billet's alone, and refusing over a build somebody
// started by hand would stop this node's sweep — which holds the capacity of
// everything quarantined on it until a person intervenes.
func TestAForeignBuildIsSkippedRatherThanStoppingTheSweep(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	mine := provider.InstanceName("aaa")
	if _, err := p.Launch(t.Context(), launchSpec(mine)); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	f.builds["billet-linux:foreign"] = &fakeBuild{
		id: "billet-linux:foreign", status: "IN_PROGRESS", phase: "BUILD",
		started: time.Now(),
		env: []envVar{
			{Name: ownerEnvVar, Value: "some-other-deployment", Type: "PLAINTEXT"},
			{Name: nameEnvVar, Value: "not-a-billet-name", Type: "PLAINTEXT"},
		},
	}

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("a build belonging to another deployment stopped the sweep: %v", err)
	}

	if len(got) != 1 || got[0].Name != mine {
		t.Errorf("List returned %+v, want only this deployment's build %q", got, mine)
	}
}

// BUT A BUILD CLAIMING TO BE THIS DEPLOYMENT'S AND NAMING NO LEASE FAILS THE WHOLE
// INVENTORY.
//
// This list frees the capacity of every lease absent from it, so a silently dropped
// row is capacity handed back for compute that is still running. The message has to
// be actionable, because failing closed stops the sweep.
func TestAnUnattributableOwnedBuildFailsTheWholeInventory(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	f.builds["billet-linux:orphan"] = &fakeBuild{
		id: "billet-linux:orphan", status: "IN_PROGRESS", phase: "BUILD",
		started: time.Now(),
		env: []envVar{
			{Name: ownerEnvVar, Value: testOwner, Type: "PLAINTEXT"},
			{Name: nameEnvVar, Value: "hand-edited", Type: "PLAINTEXT"},
		},
	}

	_, err := p.List(t.Context())
	if err == nil {
		t.Fatal("a build billet owns and cannot attribute was reported as accounted for")
	}

	for _, want := range []string{"billet-linux:orphan", "names no lease", "project of its own"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// FIND MATCHES THE LEASE MARKER EXACTLY.
//
// The caller's next move on a hit is to STOP the build, so a prefix match is a way
// to tear down the wrong one.
func TestFindMatchesTheLeaseMarkerExactly(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	short := provider.InstanceName("abc")
	long := provider.InstanceName("abcdef")

	shortInst, err := p.Launch(t.Context(), launchSpec(short))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	longInst, err := p.Launch(t.Context(), launchSpec(long))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got, ok, err := p.Find(t.Context(), short)
	if err != nil || !ok {
		t.Fatalf("Find(%s) = (%v, %v, %v)", short, got, ok, err)
	}

	if got.ID != shortInst.ID {
		t.Errorf("Find(%s) returned the build for %s", short, long)
	}

	got, ok, err = p.Find(t.Context(), long)
	if err != nil || !ok {
		t.Fatalf("Find(%s) = (%v, %v, %v)", long, got, ok, err)
	}

	if got.ID != longInst.ID {
		t.Errorf("Find(%s) returned the wrong build", long)
	}

	// AND AN ABSENCE IS NOT AN ERROR AND NOT A ZERO VALUE. The bool is explicit
	// because the caller's next move on a hit is to destroy.
	got, ok, err = p.Find(t.Context(), provider.InstanceName("nothing"))
	if err != nil {
		t.Fatalf("Find on an absent lease errored: %v", err)
	}

	if ok || got != nil {
		t.Errorf("Find on an absent lease returned (%v, %v)", got, ok)
	}
}

// FIND INCLUDES A TERMINAL BUILD AND LIST DOES NOT, which is the same split the ec2
// backend makes between findStates and liveStates: a targeted lookup wants the
// terminal record as CAUSAL PROOF for custody, while fleet inventory must not carry
// history.
func TestFindKeepsTheTerminalRecordThatListDrops(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	inst, err := p.Launch(t.Context(), launchSpec(name))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	f.builds[inst.ID].status = "SUCCEEDED"

	found, ok, err := p.Find(t.Context(), name)
	if err != nil || !ok {
		t.Fatalf("Find on a finished build = (%v, %v, %v); custody needs that record as proof",
			found, ok, err)
	}

	if !found.Terminal {
		t.Error("a SUCCEEDED build is not reported terminal, so custody has no causal proof")
	}

	if found.Running {
		t.Error("a SUCCEEDED build is reported running")
	}

	listed, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(listed) != 0 {
		t.Errorf("List reported a finished build: %+v", listed)
	}
}

// AN UNRECOGNISED STATUS COUNTS AS RUNNING AND NOT AS TERMINAL.
//
// The caller destroys what is not running, so a state billet has never heard of must
// not read as finished — and Terminal's zero value is deliberately not proof.
func TestAnUnrecognisedBuildStatusIsRunningAndNotTerminal(t *testing.T) {
	for _, status := range []string{"", "PENDING", "SOMETHING_AWS_ADDED_LATER"} {
		if !runningState(status) {
			t.Errorf("runningState(%q) = false; an unknown state is not evidence a job is over",
				status)
		}

		if terminalStatus(status) {
			t.Errorf("terminalStatus(%q) = true; the zero value must not be proof", status)
		}
	}

	for _, status := range []string{"SUCCEEDED", "FAILED", "FAULT", "STOPPED", "TIMED_OUT"} {
		if runningState(status) {
			t.Errorf("runningState(%q) = true", status)
		}

		if !terminalStatus(status) {
			t.Errorf("terminalStatus(%q) = false", status)
		}
	}

	// AND A PROVIDER TIMEOUT IS DISTINGUISHABLE FROM A FAILURE, which the backend's
	// acceptance requires: a build CodeBuild ended at its ceiling is not somebody's
	// broken test.
	//
	// THE SHAPE OF THE FIRST CASE IS THE MEASUREMENT, not a reading of the docs. A
	// real build in us-west-2 with timeoutInMinutes 5, whose BUILD phase slept 400s,
	// came back FAILED with the timeout recorded only in phases[]. A predicate over
	// buildStatus alone could never have fired for it, so every build the ceiling
	// ended would have been filed as a failing test.
	measured := build{BuildStatus: "FAILED", BuildComplete: true, Phases: []buildPhase{
		{PhaseType: "PROVISIONING", PhaseStatus: "SUCCEEDED"},
		{PhaseType: "BUILD", PhaseStatus: "TIMED_OUT"},
		{PhaseType: "COMPLETED"},
	}}

	if !TimedOut(measured) {
		t.Error("a build CodeBuild timed out reports buildStatus FAILED with the timeout only " +
			"in phases[], and billet read it as an ordinary failure — the fleet's own ceiling " +
			"filed as somebody's broken test")
	}

	// THE DOCUMENTED STATUS IS STILL ACCEPTED, because it is in AWS's own reference
	// and dropping it would leave a second spelling unhandled.
	if !TimedOut(build{BuildStatus: "TIMED_OUT", BuildComplete: true}) {
		t.Error("a build whose STATUS says TIMED_OUT is not reported as a provider timeout")
	}

	for _, status := range []string{"FAILED", "FAULT", "STOPPED", "SUCCEEDED"} {
		b := build{BuildStatus: status, BuildComplete: true, Phases: []buildPhase{
			{PhaseType: "BUILD", PhaseStatus: status},
			{PhaseType: "COMPLETED"},
		}}

		if TimedOut(b) {
			t.Errorf("TimedOut(%q) = true, so an ordinary failure would be filed as a provider "+
				"ceiling", status)
		}
	}
}

// A BUILD OLDER THAN THE INVENTORY WINDOW IS NOT WALKED, which is what keeps List
// from being O(a year of history).
func TestTheInventoryWalkStopsAtTheDeclaredWindow(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.BuildTimeoutMinutes = 60
		cfg.QueuedTimeoutMinutes = 5
	})

	name := provider.InstanceName("abc123")

	inst, err := p.Launch(t.Context(), launchSpec(name))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Inside the window: still reported.
	if got, err := p.List(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("List = (%+v, %v), want the fresh build", got, err)
	}

	// Older than build + queued + one hour of slack: outside.
	window := time.Duration(p.cfg.InventoryWindowMinutes()) * time.Minute
	f.builds[inst.ID].started = time.Now().Add(-window - time.Minute)

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("List reported a build older than the window: %+v", got)
	}
}

// A BUILD THAT HAS NOT STARTED YET IS INSIDE THE WINDOW.
//
// Zero is what CodeBuild reports for a SUBMITTED or QUEUED build, and treating that
// as ancient would drop from the inventory exactly the builds most likely to be
// about to run somebody's job.
func TestABuildWithNoStartTimeIsInsideTheWindow(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	inst, err := p.Launch(t.Context(), launchSpec(name))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	f.builds[inst.ID].started = time.Unix(0, 0)

	got, err := p.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 1 {
		t.Errorf("List dropped a build that has not started yet: %+v", got)
	}
}

// THE WALK NEVER ASKS FOR A SORT ORDER.
//
// AWS documents ListBuildsForProject as ERRORING when sortOrder is passed and the
// project has more than 100 builds — so a client that sends it works in every test
// against a small fake and breaks on the first busy project. The fake refuses it,
// which is what makes this checkable at all.
func TestTheWalkNeverRequestsASortOrder(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	if _, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc"))); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if _, err := p.List(t.Context()); err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, ".ListBuildsForProject") &&
			strings.Contains(r.body, "sortOrder") {
			t.Errorf("the walk asked for a sort order: %s", r.body)
		}
	}
}

// A REPEATED PAGINATION TOKEN IS REFUSED RATHER THAN LOOPED.
//
// A sweep that never returns stops reporting this host's inventory, and the capacity
// of anything quarantined on it is held until an operator intervenes. Checked against
// a CYCLE rather than a repeat of the immediately preceding token, because A,B,A,B
// defeats the narrower check.
func TestACyclingPaginationTokenIsRefused(t *testing.T) {
	f := newFakeAWS(t)

	// EVERY PAGE HOLDS A BUILD THE WALK WILL ACCEPT. A page of ids the fake does
	// not hold is a page entirely outside the window, which the walk correctly
	// stops on — so the first version of this test never reached the cycle and
	// passed against a client with no cycle guard at all.
	for _, id := range []string{"a", "b", "c"} {
		f.addOwnedBuild(id, provider.InstanceName(id))
	}

	f.listPages = []listPage{
		{ids: []string{"a"}, nextToken: "t1"},
		{ids: []string{"b"}, nextToken: "t2"},
		{ids: []string{"c"}, nextToken: "t1"},
	}

	p := newTestProvider(t, f, nil)

	_, err := p.List(t.Context())
	if err == nil {
		t.Fatal("a cycling pagination token did not stop the walk")
	}

	if !strings.Contains(err.Error(), "already given") {
		t.Errorf("the refusal does not say what happened: %v", err)
	}
}

// A LAUNCH FALLS BACK TO THE NEXT DECLARED COMPUTE TYPE ON A QUOTA REFUSAL.
//
// This matters more here than on ec2: CodeBuild's concurrent-build quota is per
// environment AND compute type, and the default is ONE — so the first declared type
// being full is the ordinary case rather than the unlucky one.
func TestALaunchFallsBackToTheNextComputeTypeOnAQuotaRefusal(t *testing.T) {
	f := newFakeAWS(t)
	f.startErr = []apiFault{{
		status: http.StatusBadRequest, code: "AccountLimitExceededException",
	}}

	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.ComputeTypes = []config.RemoteShape{
			{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 1},
			{Type: "BUILD_GENERAL1_LARGE", VCPU: 8, Memory: 15 * config.GiB, PriceUSDPerHour: 2},
		}
	})

	inst, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc123")))
	if err != nil {
		t.Fatalf("the launch gave up instead of trying the operator's second choice: %v", err)
	}

	if inst == nil {
		t.Fatal("the fallback returned no instance")
	}

	starts := 0

	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, ".StartBuild") {
			starts++
		}
	}

	if starts != 2 {
		t.Errorf("the launch made %d StartBuild attempts, want 2 (the refusal and the fallback)",
			starts)
	}

	if len(f.builds) != 1 {
		t.Errorf("the fallback left %d builds, want 1", len(f.builds))
	}
}

// AN AMBIGUOUS FAILURE NEVER TRIES ANOTHER SHAPE.
//
// THE ONE RULE THE FIVE-MINUTE IDEMPOTENCY WINDOW FORCES. On ec2 a fallback after an
// ambiguous failure is bounded by a ClientToken that was measured still refusing a
// changed relaunch long afterwards; here the token lapses in five minutes, so a
// second attempt after an answer billet cannot interpret is how one job becomes two
// builds — both registered with GitHub, one free to pick up unrelated work.
func TestAnAmbiguousLaunchFailureDoesNotTryAnotherShape(t *testing.T) {
	for name, fault := range map[string]apiFault{
		// Not a synchronous capacity verdict: the request may have committed.
		"server error": {status: http.StatusInternalServerError, code: "InternalServerError"},
		"unclassified": {status: http.StatusBadRequest, code: "SomethingNew"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeAWS(t)
			// Enough faults to answer every attempt, so a client that DID fall back
			// would visibly make more than one.
			f.startErr = []apiFault{fault, fault, fault, fault, fault, fault}

			p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
				cfg.ComputeTypes = []config.RemoteShape{
					{Type: "A", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 1},
					{Type: "B", VCPU: 8, Memory: 15 * config.GiB, PriceUSDPerHour: 2},
				}
			})

			if _, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc"))); err == nil {
				t.Fatal("an ambiguous failure produced a successful launch")
			}

			shapes := map[string]bool{}

			for _, r := range f.bodies() {
				if !strings.HasSuffix(r.target, ".StartBuild") {
					continue
				}

				for _, ct := range []string{`"computeTypeOverride":"A"`, `"computeTypeOverride":"B"`} {
					if strings.Contains(r.body, ct) {
						shapes[ct] = true
					}
				}
			}

			if len(shapes) > 1 {
				t.Errorf("an ambiguous failure was followed by a second compute type; the "+
					"idempotency token lapses in five minutes, so that is how one job becomes "+
					"two builds. Attempted: %v", shapes)
			}
		})
	}
}

// A BUDGET REFUSAL MAY TRY ANOTHER SHAPE, because nothing reached AWS.
func TestABudgetRefusalTriesTheNextShapeAndNeverReachesAWSFirst(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.ComputeTypes = []config.RemoteShape{
			{Type: "BIG", VCPU: 8, Memory: 15 * config.GiB, PriceUSDPerHour: 2},
			{Type: "SMALL", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 1},
		}
	})

	spec := launchSpec(provider.InstanceName("abc123"))

	asked := []string{}
	spec.AuthorizeShape = func(_ context.Context, t string, _ int, _ config.ByteSize) (bool, error) {
		asked = append(asked, t)

		return t != "BIG", nil
	}

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(asked) != 2 || asked[0] != "BIG" || asked[1] != "SMALL" {
		t.Errorf("the ledger was asked about %v, want BIG then SMALL in the operator's order",
			asked)
	}

	// THE REFUSED SHAPE MUST NOT HAVE REACHED AWS. A launch that asked the ledger
	// and sent the request anyway would reconcile the overspend afterwards, which is
	// the failure the capacity skill states as never reaching the API first.
	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, ".StartBuild") &&
			strings.Contains(r.body, `"computeTypeOverride":"BIG"`) {
			t.Error("a shape the ledger refused was sent to AWS anyway")
		}
	}
}

// THE IDEMPOTENCY TOKEN DIFFERS PER SHAPE.
//
// Keyed on the lease alone, a fallback would present the first attempt's token and
// get that attempt's outcome back — so the fallback could never launch anything and
// would be a feature that looks implemented and is dead.
func TestTheIdempotencyTokenDistinguishesShapesAndLeases(t *testing.T) {
	a := idempotencyTokenFor("billet-aaa", "BUILD_GENERAL1_MEDIUM")
	b := idempotencyTokenFor("billet-aaa", "BUILD_GENERAL1_LARGE")
	c := idempotencyTokenFor("billet-bbb", "BUILD_GENERAL1_MEDIUM")

	if a == b {
		t.Error("two shapes for one lease share a token, so a fallback could never launch")
	}

	if a == c {
		t.Error("two leases share a token")
	}

	if a != idempotencyTokenFor("billet-aaa", "BUILD_GENERAL1_MEDIUM") {
		t.Error("the token is not stable for one request, so a retry would not be deduplicated")
	}

	// THE LEASE STAYS LEGIBLE, because the token is what an operator has to work
	// with when they find a stray build in CloudTrail.
	if !strings.HasPrefix(a, "billet-aaa") {
		t.Errorf("the token %q does not begin with the lease name", a)
	}
}

// THE LAUNCH PINS NO_SOURCE AND BOTH CEILINGS ON EVERY REQUEST.
//
// A project edited underneath billet — or shared — would otherwise change what a
// build does and how far back List has to walk, silently.
func TestEveryLaunchPinsTheSourceAndBothCeilings(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.BuildTimeoutMinutes = 120
		cfg.QueuedTimeoutMinutes = 30
	})

	if _, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc"))); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	var start string

	for _, r := range f.bodies() {
		if strings.HasSuffix(r.target, ".StartBuild") {
			start = r.body
		}
	}

	for _, want := range []string{
		`"sourceTypeOverride":"NO_SOURCE"`,
		`"timeoutInMinutesOverride":120`,
		`"queuedTimeoutInMinutesOverride":30`,
		`"environmentTypeOverride":"LINUX_CONTAINER"`,
		`"privilegedModeOverride":true`,
	} {
		if !strings.Contains(start, want) {
			t.Errorf("the launch does not pin %s: %s", want, start)
		}
	}
}

// PRIVILEGED MODE IS NOT SENT WHERE THERE IS NO CONTAINER.
//
// An EC2 or macOS environment IS the machine, so asking for a container privilege
// there is billet asking about something that does not exist.
func TestPrivilegedModeIsNotSentForANonContainerEnvironment(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.EnvironmentType = config.CodeBuildMacARM
		cfg.PrivilegedMode = false
		cfg.FleetARN = "arn:aws:codebuild:us-west-2:000000000000:fleet/macs"
	})

	if _, err := p.Launch(t.Context(), launchSpec(provider.InstanceName("abc"))); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	for _, r := range f.bodies() {
		if !strings.HasSuffix(r.target, ".StartBuild") {
			continue
		}

		if strings.Contains(r.body, "privilegedModeOverride") {
			t.Errorf("a macOS launch asked for container privilege: %s", r.body)
		}

		if !strings.Contains(r.body, `"fleetOverride"`) {
			t.Errorf("a reserved-capacity launch did not name its fleet: %s", r.body)
		}
	}
}

// THE REAPER REMOVES A STAGED REGISTRATION WHATEVER PHASE THE BUILD IS IN, because its
// caller has already proved the compute is gone.
//
// THIS REPLACED A PHASE INFERENCE, and the phase mattered only while the provider was
// deciding for itself whether the build might still read the value. It cannot decide
// that soundly — the inventory is eventually consistent, so "I see no build" and "the
// build has not appeared" are the same observation — and getting it wrong early is a
// runner that never registers. Custody settlement is the one caller with a proof, so
// the predicate went and the contract carries the requirement instead.
func TestReapingRemovesTheStagedRegistrationWhateverThePhase(t *testing.T) {
	for _, phase := range []string{
		"SUBMITTED", "QUEUED", "PROVISIONING", "DOWNLOAD_SOURCE",
		"INSTALL", "BUILD", "COMPLETED", "SOMETHING_AWS_ADDED_LATER",
	} {
		t.Run(phase, func(t *testing.T) {
			f := newFakeAWS(t)
			p := newTestProvider(t, f, nil)

			name := provider.InstanceName("abc123")

			inst, err := p.Launch(t.Context(), launchSpec(name))
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}

			f.builds[inst.ID].phase = phase
			f.builds[inst.ID].status = "IN_PROGRESS"

			if len(f.params) == 0 {
				t.Fatal("the launch staged no registration, so the removal below proves nothing")
			}

			if err := p.ReapStagedCredential(t.Context(), name); err != nil {
				t.Fatalf("ReapStagedCredential: %v", err)
			}

			if len(f.params) != 0 {
				t.Errorf("in phase %s the registration survived a reap whose caller had proved "+
					"the compute gone, so it would sit in Parameter Store until the account's "+
					"quota", phase)
			}
		})
	}
}

// AND REAPING SOMETHING ALREADY GONE IS SUCCESS, because the teardown path removes the
// registration when it confirms a build terminal — so settlement's call is usually a
// second one about something absent, and an error there would report a cleanup failure
// on every ordinary job.
func TestReapingAnAbsentRegistrationSucceeds(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	if err := p.ReapStagedCredential(t.Context(), name); err != nil {
		t.Errorf("reaping a registration that was never staged failed: %v", err)
	}
}

// AND IT REFUSES AN EMPTY NAME rather than addressing the bare parameter path, which
// would delete somebody else's registration — the same guard deleteJITConfig makes.
func TestReapingRefusesAnEmptyName(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	if err := p.ReapStagedCredential(t.Context(), ""); err == nil {
		t.Error("reaping with no name was accepted, which addresses the parameter path itself")
	}
}

// AND THE TEARDOWN PATH STILL REMOVES IT ON ITS OWN, which is what makes settlement's
// call a second one rather than the only one: a build billet asked to stop and then
// confirmed terminal has its registration deleted there, so the ordinary completed-job
// path does not wait for custody.
func TestTeardownRemovesTheRegistrationWhenItConfirmsTheBuildOver(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")

	inst, err := p.Launch(t.Context(), launchSpec(name))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(f.params) == 0 {
		t.Fatal("the launch staged no registration, so the removal below proves nothing")
	}

	teardown, err := p.Destroy(t.Context(), inst.ID)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if teardown != provider.TeardownStopped {
		t.Fatalf("Destroy = %v, want TeardownStopped once the build settled", teardown)
	}

	if len(f.params) != 0 {
		t.Error("a confirmed teardown left the staged registration behind")
	}
}

// A LAUNCH WHOSE PARAMETER ALREADY EXISTS IS REFUSED RATHER THAN OVERWRITING.
//
// A parameter standing at this name means either a launch for this lease is in
// flight — replacing its registration strands whatever consumed the first one — or
// an earlier attempt's credential was never cleaned up, which an operator needs to
// know about. Refusing releases the lease so the job is reassigned.
func TestALaunchWillNotReplaceAnExistingStagedRegistration(t *testing.T) {
	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	name := provider.InstanceName("abc123")
	f.params[p.jitParameterName(name)] = "somebody else's registration"

	_, err := p.Launch(t.Context(), launchSpec(name))
	if err == nil {
		t.Fatal("a launch replaced a standing runner registration")
	}

	if !strings.Contains(err.Error(), "already stands") {
		t.Errorf("the refusal does not say what happened: %v", err)
	}

	if got := f.params[p.jitParameterName(name)]; got != "somebody else's registration" {
		t.Errorf("the standing registration was modified: %q", got)
	}

	if len(f.builds) != 0 {
		t.Errorf("a refused launch started %d build(s)", len(f.builds))
	}
}
