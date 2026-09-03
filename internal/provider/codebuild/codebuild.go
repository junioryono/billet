// Package codebuild runs one AWS CodeBuild build per job.
//
// IT DOES NOT USE CODEBUILD'S OWN GITHUB ACTIONS RUNNER INTEGRATION, and that is
// the first thing to know about it. That feature is webhook-only — a "Runner
// project" needs a WORKFLOW_JOB_QUEUED webhook and a
// `codebuild-<project>-${{ github.run_id }}-…` label, and CodeBuild fetches the
// runner registration token itself during DOWNLOAD_SOURCE and overrides the
// buildspec — so AWS would own job detection, registration and scheduling, and
// billet's scale-set listener, capacity ledger, ordered fallback, custody and
// stable-label contract would all be bypassed. Checked against the API reference
// rather than inferred: there is no API-invoked form of it. So this backend starts
// an ordinary NO_SOURCE project with StartBuild and runs GitHub's own runner from
// billet's own JIT configuration, exactly as internal/provider/ec2 does inside an
// instance. See docs/adr-007-codebuild-provider.md.
//
// FOUR THINGS ARE UNLIKE EVERY OTHER BACKEND, and each of them shapes the code:
//
// A BUILD CANNOT BE TAGGED. CodeBuild tags exist on projects and report groups, so
// the per-instance `sh.billet.owner` tag the ec2 backend filters List on has no
// equivalent. Ownership is a DEDICATED, tagged project plus per-build markers sent
// as environment-variable overrides and read back through BatchGetBuilds — and
// because List feeds a loop that stops builds, a project shared with an ordinary
// CodeBuild workload is a way for billet to stop somebody else's build.
//
// THERE IS NO WAY TO LIST ONLY ACTIVE BUILDS, and history is kept for a year.
// ListBuildsForProject has no status filter, returns 100 ids a page newest-first,
// and errors if sortOrder is passed above 100 builds. So List and Find are a
// bounded page-walk, and what bounds them is CodeBuild's OWN enforced timeout: a
// build older than the declared build and queued ceilings cannot still be running.
//
// StartBuild's idempotencyToken IS VALID FOR FIVE MINUTES. ec2's ClientToken was
// measured still refusing a changed relaunch of the same lease long after the fact,
// which is what makes that backend's retry-and-fallback loop safe. Here, past five
// minutes, an identical retry starts a SECOND build — two runners for one job. So
// the token covers a fast transport-level retry and nothing above it retries a
// launch: an ambiguous failure ASKS, which is what provider.Find is for.
//
// AND A MANAGED BUILD HOST IS NOT A PER-JOB BOUNDARY. AWS documents a
// reserved-capacity instance as remaining alive between builds, shareable across
// projects, and as making cached data reachable by other projects in the account,
// by design — and macOS is reserved-only. So this backend refuses untrusted work
// outright rather than gating it on a network, because no security group fixes a
// machine that later runs somebody else's build.
package codebuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// Spec is the subset of provider.Spec this package's buildspec builder needs.
//
// DECLARED SEPARATELY SO THE BUILDSPEC CAN BE TESTED WITHOUT A PROVIDER, and
// deliberately WITHOUT the JIT config: the buildspec must not be able to carry the
// registration even by accident, and a type that does not hold it cannot.
type Spec struct {
	Name    string
	Command []string
}

// errNoCommand is what an empty tier command answers.
//
// REFUSED, NOT DEFAULTED, the rule every backend follows: an image's default boot
// does something, so a spec with no command produces a build that starts, reports
// its own success, and never registers a runner — while the job sits queued.
var errNoCommand = errors.New("codebuild: this launch has no command, so the build would run " +
	"to completion without ever starting a runner and the job would stay queued")

// Provider launches CodeBuild builds, one per job.
type Provider struct {
	log   *slog.Logger
	owner string
	cfg   config.CodeBuildConfig
	api   *client

	// now is the clock the inventory window is measured against, replaceable so a
	// test does not have to wait a day for a build to age out of it.
	now func() time.Time
	// sleep paces the teardown poll, replaceable so a test does not spend the
	// wall clock proving nothing about waiting.
	//
	// A REPLACEMENT STILL HONOURS THE CONTEXT, because that loop is bounded by
	// cancellation as well as by a count and one that ignored it would spin.
	sleep func(ctx context.Context, d time.Duration) error
}

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(p *Provider) { p.log = log }
}

// WithCredentials sets where AWS credentials come from. REQUIRED: there is no
// default, so a caller cannot end up signing with credentials it did not choose.
func WithCredentials(src CredentialSource) Option {
	return func(p *Provider) { p.api.setCreds(src) }
}

// WithHTTPClient sets the client used for API calls, for a test or for a deployment
// that needs a proxy.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.api.setHTTPClient(c) }
}

// New builds a CodeBuild provider. owner names this billet deployment and is
// written into every build it starts.
func New(owner string, cfg config.CodeBuildConfig, opts ...Option) (*Provider, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("codebuild: a provider needs the deployment identity that marks " +
			"its builds, or it cannot tell its own compute from another billet's")
	}

	// PREPARED THROUGH THE SAME FUNCTION config.Load USES, rather than by
	// reproducing part of it here.
	//
	// Validating a trimmed copy and then SIGNING with the original is the shape of
	// bug this constructor exists to prevent — ` us-west-2 ` passes the region rule,
	// dials the right host, and puts the spaces in the credential scope of every
	// request, which AWS answers with a 403 naming nothing. The first version did
	// the trimming by hand and reproduced only part of Load: it missed the
	// compute-type NAMES, which validation checks trimmed and the launch sends raw,
	// and the timeout DEFAULTS, without which a caller that omitted them passed
	// validation (zero means "not stated") and sent a zero override AWS refuses.
	//
	// THE SLICE IS CLONED FIRST, because Prepare writes into it and the caller keeps
	// it. A caller that can widen a shape list after construction can buy compute
	// the ledger never authorised — the reason ec2.New clones its security groups —
	// and trimming in place would additionally mutate a config it still holds.
	cfg.ComputeTypes = slices.Clone(cfg.ComputeTypes)
	// THE UNTRUSTED NETWORK SLICES ARE CLONED FOR THE SAME REASON, and it is a
	// security one: a caller that can widen the subnet or security-group list after
	// construction can move a fork's build onto a network billet never verified.
	cfg.UntrustedSubnetIDs = slices.Clone(cfg.UntrustedSubnetIDs)
	cfg.UntrustedSecurityGroupIDs = slices.Clone(cfg.UntrustedSecurityGroupIDs)
	cfg.Prepare()

	// THE SAFETY RULES CONFIG VALIDATION APPLIES ARE RE-APPLIED HERE. This
	// constructor is exported, so it cannot assume its configuration came through
	// config.Load — the alloc.New rule. The region is the least obvious of them and
	// the most important: it is interpolated into the default endpoint, so an
	// unvalidated one chooses the HOST a signed request is sent to.
	//
	// THE CEILING ACKNOWLEDGEMENT IS NOT AMONG THEM, deliberately. It gates a node
	// that will serve work rather than a provider a diagnostic can construct, and
	// `billet check --provider codebuild` builds one of these precisely to report
	// what those ceilings ARE.
	if errs := config.CheckCodeBuild(cfg); len(errs) > 0 {
		return nil, fmt.Errorf("codebuild: %w", errors.Join(errs...))
	}

	p := &Provider{
		log:   slog.Default(),
		owner: owner,
		cfg:   cfg,
		now:   time.Now,
		sleep: sleepFor,
		api:   newClient(cfg.Region, cfg.Endpoint, nil),
	}

	for _, opt := range opts {
		opt(p)
	}

	// AN OPTION MUST NOT BE ABLE TO PRODUCE A PANIC, and each of these fails LATER
	// and further from its cause than it looks: WithHTTPClient(nil) dereferences
	// here, WithLogger(nil) at the first line Launch logs, and WithCredentials(nil)
	// at the first signed call — on a path holding leases. billet bans panic
	// outright, because a control plane that panics drops every one of them.
	switch {
	case p.api.httpClient() == nil:
		return nil, errors.New("codebuild: WithHTTPClient was given no client")

	case p.log == nil:
		return nil, errors.New("codebuild: WithLogger was given no logger")

	// NO DEFAULT SOURCE, so this is a required option rather than an override. A
	// TYPED NIL is the one a plain `== nil` misses — it satisfies the interface and
	// dereferences at the first signed call.
	case p.api.creds() == nil || isNilValue(p.api.creds()):
		return nil, errors.New("codebuild: WithCredentials is required; this backend has no " +
			"default credential source, so nothing may sign with credentials a caller did " +
			"not choose")
	}

	// AFTER THE OPTIONS, so a client supplied by a caller is covered too.
	//
	// A REDIRECT MUST NOT CARRY A SIGNED REQUEST SOMEWHERE ELSE. The endpoint is
	// checked for https and then Go's client follows redirects by default — a 307
	// preserves the method and body, and the hop can be plaintext or another host
	// entirely, so everything the endpoint rule prevents happens one response later
	// to a URL nobody validated. AWS does not redirect, which is why this is a
	// refusal rather than a policy: a redirect from this endpoint is not the API
	// answering.
	redirecting := *p.api.httpClient()
	redirecting.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		// THE HOST AND NOTHING ELSE. A redirect target is chosen by whatever
		// answered, so its query and fragment are not billet's to render — the same
		// rule config.CheckCodeBuildEndpoint follows. The sentinel matters as much
		// as the host: net/http wraps this in a *url.Error, and THAT renders the
		// whole target.
		return fmt.Errorf("%w to host %q", errRedirected, req.URL.Hostname())
	}

	p.api.setHTTPClient(&redirecting)

	return p, nil
}

// sleepFor is the default pacing wait.
func sleepFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Kind reports the backend this is.
func (p *Provider) Kind() config.ProviderKind { return config.ProviderCodeBuild }

// untrustedVPC is the network an untrusted build must run on, taken from the node
// config and verified against the project at launch.
type untrustedVPC struct {
	VPCID            string
	SubnetIDs        []string
	SecurityGroupIDs []string
}

// untrustedNetwork is the ONE place that decides whether this backend may run
// untrusted work, and what network it must run on.
//
// Accepts AND the launch path are its only callers — the tart untrusted_isolation
// rule, one backend over: written as two functions, a network added to the config
// and not enforced at launch would admit untrusted work and then run it on the
// project's default network. Here the enforcement is a launch-time check that the
// project carries exactly the returned VPC (assertUntrustedNetwork), because
// StartBuild has no VPC override; keeping the admission decision and the network it
// requires in one function is what makes that check impossible to forget.
//
// It returns the VPC on success, or the refusal. The three refusals are the three
// things measured about this backend: a reserved fleet is shared between builds (so
// a fleetOverride, which also discards the project vpc, can never isolate a fork), a
// non-container environment is reserved-only, and an absent network is the refusal
// itself — the same shape as node.ec2.untrusted_security_group_ids.
func (p *Provider) untrustedNetwork() (untrustedVPC, error) {
	if p.cfg.FleetARN != "" {
		return untrustedVPC{}, errors.New("codebuild: refusing to run untrusted work on a " +
			"reserved-capacity fleet: AWS documents a reserved instance as staying alive " +
			"between builds and sharing cached data with other projects in the account, so a " +
			"fork's pull request is arbitrary code a later build inherits — and a fleetOverride " +
			"discards the project's vpc, so no network isolates it. macOS is reserved-only. Run " +
			"untrusted CodeBuild work on an on-demand container tier, or use firecracker or ec2")
	}

	if !p.cfg.EnvironmentType.Container() {
		return untrustedVPC{}, fmt.Errorf("codebuild: refusing to run untrusted work in "+
			"environment_type %s: it runs the job directly on a reserved-capacity machine "+
			"rather than in a per-build container, so the machine is shared between builds. "+
			"Untrusted CodeBuild work needs an on-demand container environment", p.cfg.EnvironmentType)
	}

	if !p.cfg.HasUntrustedNetwork() {
		return untrustedVPC{}, errors.New("codebuild: refusing to run untrusted work with no " +
			"isolated network: set node.codebuild.untrusted_vpc_id, untrusted_subnets and " +
			"untrusted_security_group_ids to a network that reaches only what a fork's code " +
			"should. A build container isolates the kernel, not the network, and its absence " +
			"is the refusal — the same rule as node.ec2.untrusted_security_group_ids")
	}

	return untrustedVPC{
		VPCID:            p.cfg.UntrustedVPCID,
		SubnetIDs:        p.cfg.UntrustedSubnetIDs,
		SecurityGroupIDs: p.cfg.UntrustedSecurityGroupIDs,
	}, nil
}

// Accepts reports whether this backend may run work of that trust class.
//
// UNTRUSTED IS ADMITTED ONLY ON AN ON-DEMAND CONTAINER TIER WITH A CONFIGURED
// ISOLATED NETWORK, and that is a narrower door than the ec2 backend's for a
// measured reason: a reserved-capacity fleet instance stays alive between builds
// and shares cached data with other projects in the account, by design, so no
// network fixes a machine a later build inherits — and macOS is reserved-only.
// An on-demand Linux build, by contrast, is a freshly-booted machine destroyed
// with the build (31 builds, 31 distinct host boot-ids, nothing surviving between
// them, measured 2026-09-02), which makes it a per-job boundary the way an ec2
// instance is — after which the network is the only remaining question, exactly
// as it is for ec2.
//
// THE DECISION AND THE NETWORK IT REQUIRES ARE ONE FUNCTION, untrustedNetwork, so
// the launch cannot admit a class this refuses or run it off the network this names.
//
// UNKNOWN IS REFUSED for the separate reason it always is: untrusted is a
// classification billet made, unknown means it could not classify the job at all,
// so there is no basis for choosing anything.
func (p *Provider) Accepts(trust provider.TrustClass) error {
	switch trust {
	case provider.TrustTrusted:
		return nil

	case provider.TrustUntrusted:
		_, err := p.untrustedNetwork()

		return err

	case provider.TrustUnknown:
		return errors.New("codebuild: refusing to run work billet could not classify: an " +
			"unrecognised event establishes no provenance, so there is no basis for deciding " +
			"whether this compute is a safe place for it")

	default:
		return fmt.Errorf("codebuild: refusing to run %s work", trust)
	}
}

// computeTypesFor lists the compute types that could hold a lease, in the
// operator's own order.
//
// IN THE OPERATOR'S ORDER, the rule a tier's provider list follows: the order is a
// preference and billet does not reorder it. Sorting by size would look thriftier
// and would quietly override an operator who listed a shape first for a reason
// billet cannot see.
//
// ALL OF THEM RATHER THAN THE FIRST, because the first can be unavailable — and on
// CodeBuild the way it is unavailable is a per-shape account quota, which is
// DEFAULT ONE. Giving up while the operator's own second choice sat unused in the
// list is the failure an availability backend exists to prevent.
func (p *Provider) computeTypesFor(spec provider.Spec) ([]config.RemoteShape, error) {
	var fits []config.RemoteShape

	selected := spec.InstanceType == ""

	for _, ct := range p.cfg.ComputeTypes {
		if !selected {
			if ct.Type != spec.InstanceType {
				continue
			}

			selected = true

			if ct.VCPU < spec.VCPU || ct.Memory < spec.Memory {
				return nil, fmt.Errorf("codebuild: the allocator selected compute type %q, but "+
					"this node now declares it as %d vCPU and %s, which does not hold %d vCPU "+
					"and %s", ct.Type, ct.VCPU, ct.Memory, spec.VCPU, spec.Memory)
			}
		}

		if ct.VCPU >= spec.VCPU && ct.Memory >= spec.Memory {
			fits = append(fits, ct)
		}
	}

	if !selected {
		return nil, fmt.Errorf("codebuild: the allocator selected compute type %q, which this "+
			"node did not register", spec.InstanceType)
	}

	if len(fits) > 0 {
		return fits, nil
	}

	declared := make([]string, 0, len(p.cfg.ComputeTypes))
	for _, ct := range p.cfg.ComputeTypes {
		declared = append(declared, fmt.Sprintf("%s (%d vCPU, %s)", ct.Type, ct.VCPU, ct.Memory))
	}

	// NAMES BOTH SIDES. The allocator has already escrowed this size against this
	// node, so reaching here means the ledger and the config disagree — and an
	// operator needs to see which number to change.
	return nil, fmt.Errorf("codebuild: no declared compute type holds %d vCPU and %s (declared: "+
		"%s); the allocator escrowed this size against this node, so either the tier or "+
		"node.codebuild.compute_types is wrong",
		spec.VCPU, spec.Memory, strings.Join(declared, ", "))
}

// idempotencyTokenFor is the key that makes a FAST retry safe.
//
// (lease name, compute type), the same pair ec2.clientTokenFor uses and for the
// same reason: keyed on the name alone, a fallback to a second shape would present
// the token of the first attempt and get that attempt's outcome — or a mismatch —
// so the fallback could never launch anything and would be a feature that looks
// implemented and is dead.
//
// AND IT IS ONLY GOOD FOR FIVE MINUTES. AWS documents the window, which is the
// single biggest difference from ec2's ClientToken — measured there as still
// refusing a changed relaunch long afterwards. So this bounds a retry inside one
// call and NOTHING above it may retry a launch: see Launch, which asks rather than
// retrying, and the api client, which does not retry an ambiguous StartBuild.
//
// The shape is hashed rather than appended for ec2's reason: the token has a length
// bound, this package keeps no table of compute types so a longer one AWS adds
// later must work, and a fixed-width digest removes the question rather than
// bounding it.
func idempotencyTokenFor(name, computeType string) string {
	sum := sha256.Sum256([]byte(computeType))

	return name + "-" + hex.EncodeToString(sum[:])[:12]
}

// runningState reports whether a build state means the job may still be executing.
//
// UNRECOGNISED COUNTS AS RUNNING, the same asymmetry every other backend uses and
// for the same reason: the caller destroys what is not running, and a state billet
// has never heard of is not evidence that a job is over.
func runningState(status string) bool {
	switch status {
	case "SUCCEEDED", "FAILED", "FAULT", "STOPPED", "TIMED_OUT":
		return false
	default:
		return true
	}
}

// terminalStatus reports that CodeBuild has POSITIVELY observed a build reach a
// state it cannot execute or return to running from.
//
// STRONGER THAN !runningState, and the zero value is deliberately not proof — the
// provider.Instance contract. A status billet does not recognise is neither
// terminal nor safe to treat as finished.
func terminalStatus(status string) bool {
	switch status {
	case "SUCCEEDED", "FAILED", "FAULT", "STOPPED", "TIMED_OUT":
		return true
	default:
		return false
	}
}

// TimedOut reports whether a terminal build was ended by CodeBuild's own timeout
// rather than by the job failing.
//
// REPORTED DISTINCTLY because the two send an operator to different places: a FAILED
// build is somebody's test, and a build the service ended at its ceiling is a limit
// this backend cannot lift. Filing the second as the first is how a fleet-level
// limitation is mistaken for a flaky suite.
//
// THE STATUS IS NOT WHERE THE TIMEOUT IS, and the first version read only that.
// MEASURED against real CodeBuild in us-west-2 on 2026-08-31 — a build with
// timeoutInMinutes 5 whose BUILD phase slept 400s came back:
//
//	buildStatus  FAILED
//	phases[BUILD].phaseStatus  TIMED_OUT
//	phases[BUILD].contexts[0]  BUILD_TIMED_OUT: Build has timed out.
//
// So `buildStatus == "TIMED_OUT"` was a predicate that could never fire for the case
// it exists for, and every build the ceiling ended would have been reported as a
// failing test. TIMED_OUT is still a DOCUMENTED build status, so it is kept rather
// than replaced: the question is asked of the status AND of every phase, because a
// rule about somebody else's API that only one of its two spellings satisfies is the
// same defect one spelling over.
func TimedOut(b build) bool {
	if b.BuildStatus == statusTimedOut {
		return true
	}

	for i := range b.Phases {
		if b.Phases[i].PhaseStatus == statusTimedOut {
			return true
		}
	}

	return false
}

// statusTimedOut is CodeBuild's own word, and it appears in two different fields.
const statusTimedOut = "TIMED_OUT"

// isNilValue reports whether an interface holds a typed nil.
//
// A TYPED NIL SATISFIES AN INTERFACE AND PANICS ON USE, and it is the one a plain
// `== nil` misses: WithCredentials((*something)(nil)) produces a non-nil interface
// that dereferences at the first signed call.
func isNilValue(v any) bool {
	return reflectIsNil(v)
}
