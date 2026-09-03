package codebuild

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// assertUntrustedNetwork refuses an untrusted launch unless the project carries
// exactly the isolated network the config declared.
//
// WHY THE PROJECT AND NOT THE BUILD: StartBuild has no vpcConfig override, so a
// build's network is the project's, and a fleetOverride discards even that (which
// is why untrustedNetwork refuses a fleet before reaching here). The declared
// node.codebuild.untrusted_* is what the terraform module created the project with;
// this proves the running project still matches, so an operator who pointed billet
// at a different project, or whose project's VPC drifted, cannot run a fork on a
// network billet never verified.
//
// SET EQUALITY, NOT SUBSET: a project reaching a subnet or security group the
// config did not declare is a network the operator did not review, which is the
// thing untrusted isolation exists to prevent — so more is as wrong as less.
func (p *Provider) assertUntrustedNetwork(ctx context.Context) error {
	want, err := p.untrustedNetwork()
	if err != nil {
		return err
	}

	var out batchGetProjectsResponse
	if err := p.api.call(ctx, "BatchGetProjects",
		map[string]any{"names": []string{p.cfg.Project}}, &out); err != nil {
		// FAILS CLOSED. billet could not confirm the isolation, so it does not start
		// a fork's build on faith — the message drops the API's prose for the same
		// reason the launch error does, since this runs on the untrusted path.
		return fmt.Errorf("codebuild: could not verify the untrusted network on project %s "+
			"before an untrusted launch; refusing rather than running a fork on an unverified "+
			"network", p.cfg.Project)
	}

	if len(out.Projects) == 0 {
		return fmt.Errorf("codebuild: project %s does not exist, so its untrusted network "+
			"cannot be verified", p.cfg.Project)
	}

	got := out.Projects[0].VpcConfig
	if got == nil {
		return fmt.Errorf("codebuild: project %s carries no vpc configuration, but this tier is "+
			"untrusted and node.codebuild declares an isolated network (%s); a fork's build "+
			"would run on the default network. Create the project with the untrusted vpc, "+
			"subnets and security groups", p.cfg.Project, want.VPCID)
	}

	if got.VpcID != want.VPCID ||
		!sameStringSet(got.Subnets, want.SubnetIDs) ||
		!sameStringSet(got.SecurityGroupIDs, want.SecurityGroupIDs) {
		return fmt.Errorf("codebuild: project %s runs builds on vpc %s (subnets %s, groups %s), "+
			"but node.codebuild declares the untrusted network as vpc %s (subnets %s, groups "+
			"%s); refusing to run a fork on a network billet did not verify", p.cfg.Project,
			got.VpcID, strings.Join(got.Subnets, ","), strings.Join(got.SecurityGroupIDs, ","),
			want.VPCID, strings.Join(want.SubnetIDs, ","), strings.Join(want.SecurityGroupIDs, ","))
	}

	return nil
}

// sameStringSet reports whether two slices hold the same values, order and
// duplicates aside.
func sameStringSet(a, b []string) bool {
	x := slices.Clone(a)
	y := slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)

	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// reflectIsNil is isNilValue's implementation, kept apart so codebuild.go does not
// import reflect for one call.
func reflectIsNil(v any) bool {
	if v == nil {
		return true
	}

	rv := reflect.ValueOf(v)

	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func,
		reflect.Interface, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// pageSize is what both ListBuildsForProject and BatchGetBuilds cap at, so one page
// of ids is exactly one lookup.
const pageSize = 100

// teardownPollInterval and teardownPollLimit bound the wait for a stopped build to
// reach a terminal state.
//
// BOUNDED BECAUSE THE CALLER IS A SERIAL COMMAND QUEUE. A node executes one command
// at a time and each command's timeout starts when it is QUEUED, so a teardown that
// waited indefinitely would stall every launch behind it — the failure the capacity
// skill records for fan-out against a serial queue. Running out of polls is not an
// error: it returns TeardownRequested, which keeps the capacity charged and hands
// the question to custody, which is exactly what that state is for.
const (
	teardownPollInterval = 2 * time.Second
	teardownPollLimit    = 10
)

// Launch starts one build running the job its JIT config names.
func (p *Provider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	if spec.Name == "" {
		return nil, errors.New("codebuild: a spec needs a name")
	}

	if spec.JITConfig == "" {
		return nil, fmt.Errorf("codebuild: %s has no JIT config, so nothing would register",
			spec.Name)
	}

	// Checked again here, not only via Accepts. A caller is expected to ask first so
	// a refusal costs no runner registration, but a backend that only refuses when
	// asked politely is not a boundary.
	if err := p.Accepts(spec.Trust); err != nil {
		return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
	}

	// THE NETWORK IS VERIFIED AGAINST THE PROJECT BEFORE ANYTHING IS STAGED, because
	// CodeBuild has no StartBuild VPC override: a fork's build runs on whatever
	// network the project carries, so admitting untrusted work in Accepts is only
	// half of it — the launch must prove the project's vpcConfig is exactly the
	// isolated network the config declared, or a fork runs on the trusted default.
	// It fails CLOSED: a project billet cannot read, or one whose network does not
	// match, refuses the launch rather than starting a build billet cannot vouch for.
	if spec.Trust == provider.TrustUntrusted {
		if err := p.assertUntrustedNetwork(ctx); err != nil {
			return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
		}
	}

	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("%w (job %s)", errNoCommand, spec.Name)
	}

	if p.cfg.JITParameterPath == "" {
		return nil, errNoJITPath
	}

	computeTypes, err := p.computeTypesFor(spec)
	if err != nil {
		return nil, err
	}

	// BUILT BEFORE ANYTHING IS SENT, so a tier whose command cannot be carried
	// safely fails with nothing started — the lease is released and the job is
	// reassigned, rather than being held in custody because an absence from one
	// lookup is not proof nothing began.
	buildspec, err := p.Buildspec(Spec{Name: spec.Name, Command: spec.Command})
	if err != nil {
		return nil, err
	}

	// THE REGISTRATION IS STAGED BEFORE THE BUILD EXISTS, because CodeBuild resolves
	// the parameter while preparing the environment: a build started first would
	// race its own credential and fail to register for a reason nothing names.
	if err := p.putJITConfig(ctx, spec.Name, spec.JITConfig); err != nil {
		return nil, err
	}

	inst, outcome, err := p.startOne(ctx, spec, computeTypes, buildspec)
	if err == nil {
		return inst, nil
	}

	// THE CREDENTIAL IS ONLY LITTER IF NOTHING STARTED, and that is exactly what the
	// two outcomes separate.
	//
	// A CONCLUSIVE refusal is AWS saying it rejected the request — no build exists, so
	// the staged registration can never be read and leaving it is an unmentioned
	// credential nobody finds until it matters.
	//
	// An AMBIGUOUS failure is the opposite, and deleting there is the expensive
	// mistake: a StartBuild that committed and lost its response leaves a build that
	// will resolve the parameter when it reaches its environment-preparation phase —
	// possibly minutes later, since it may sit QUEUED — and a deleted parameter makes
	// that build register nothing while every signal says the launch merely failed.
	// The parameter stays. Custody's tending resolves the lease against Find and
	// reaps it on settlement; if this process dies first, the control plane's sweep
	// (sweep.go) removes it once the LEDGER has held the lease terminal for longer
	// than any build could still be running — never on this backend's own inventory.
	//
	// THE FIRST VERSION DELETED ON EVERY ERROR, including the case its own comment
	// documents as "something started and billet cannot name it".
	if outcome == launchConclusive {
		// ON A DETACHED CONTEXT, because the launch's may already be cancelled —
		// which is precisely when a cleanup is most needed and least likely to run.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
		defer cancel()

		if cerr := p.deleteJITConfig(cctx, spec.Name); cerr != nil {
			p.log.Warn("a launch was refused and its staged runner registration could not be "+
				"removed; delete the parameter by hand", "runner", spec.Name, "error", cerr)
		}

		return nil, err
	}

	p.log.Warn("a launch failed ambiguously, so a build for this lease may be starting; "+
		"leaving its staged runner registration in place until the lease settles",
		"runner", spec.Name, "error", err)

	return nil, err
}

// launchOutcome says whether a failed launch PROVED that nothing started.
//
// THE ZERO VALUE IS THE SAFE ONE, deliberately, exactly as provider.Teardown's is: a
// path that forgets to classify reports "I could not prove it" and the credential is
// preserved. The opposite default deletes a parameter a live build still needs, and
// nothing recovers from that — the build registers nothing and the job queues until
// GitHub gives up.
type launchOutcome int

const (
	// launchAmbiguous means the request may have committed. The staged registration
	// stays and the tending sweep resolves it.
	launchAmbiguous launchOutcome = iota
	// launchConclusive means AWS rejected the request, having started nothing.
	launchConclusive
)

// startOne walks the declared compute types until one starts.
//
// THE LOOP ADVANCES ON EXACTLY ONE CONDITION — AWS refusing SYNCHRONOUSLY for want
// of capacity or quota, having started nothing — and that narrowness is the whole
// safety argument. An AMBIGUOUS failure is the opposite: trying again after one
// could leave two builds carrying this lease's name, both registered with GitHub,
// one free to pick up unrelated work. And here the token cannot rescue that, because
// it expires in five minutes.
func (p *Provider) startOne(
	ctx context.Context, spec provider.Spec, computeTypes []config.RemoteShape, buildspec string,
) (*provider.Instance, launchOutcome, error) {
	refusals := make([]string, 0, len(computeTypes))

	for _, ct := range computeTypes {
		if spec.AuthorizeShape != nil {
			ok, err := spec.AuthorizeShape(ctx, ct.Type, ct.VCPU, ct.Memory)
			if err != nil {
				// THE LEDGER COULD NOT DECIDE, and nothing was sent — so this is
				// conclusive about AWS even though it is a failure. The parameter is
				// litter.
				return nil, launchConclusive, fmt.Errorf(
					"codebuild: authorize compute type %s for %s: %w", ct.Type, spec.Name, err)
			}

			if !ok {
				refusals = append(refusals, ct.Type+": budget")
				p.log.Warn("this fallback compute type would exceed the node or deployment "+
					"budget; trying the next declared one",
					"runner", spec.Name, "type", ct.Type)

				continue
			}
		}

		inst, err := p.startBuild(ctx, spec, ct, buildspec)
		if err == nil {
			return inst, launchConclusive, nil
		}

		code, coded := codeOf(err)
		if !coded || !capacityRefusal(code) {
			// A CODED REFUSAL FROM AWS IS CONCLUSIVE — the service answered and said
			// no, so no build exists. Anything else may have committed: a dropped
			// connection, an unparseable body, exhausted 5xx retries, or a 200 whose
			// body named no build. Those keep the credential.
			return nil, conclusiveness(err), err
		}

		refusals = append(refusals, ct.Type+": "+code)

		p.log.Warn("aws will not start this compute type right now; trying the next declared one",
			"runner", spec.Name, "type", ct.Type, "code", code)
	}

	// EVERY SHAPE WAS REFUSED SYNCHRONOUSLY, so nothing started at all — the one
	// exhaustion case that is conclusive.
	//
	// NAMES EVERY SHAPE IT TRIED. "Limit exceeded" alone leaves an operator unable to
	// tell whether billet gave up on the first entry or exhausted the list, and those
	// call for different actions — raise a quota, or declare another shape.
	return nil, launchConclusive, fmt.Errorf("codebuild: launch %s: every declared compute "+
		"type that fits was refused (%s). CodeBuild's concurrent-build quota is per "+
		"environment and compute type and defaults to ONE, so check it before assuming this "+
		"is transient", spec.Name, joinRefusals(refusals))
}

// conclusiveness reports whether an error PROVES that AWS started nothing.
//
// A 4xx REFUSAL IS THE SERVICE ANSWERING NO — the request was rejected before it acted,
// so no build exists and the staged registration can never be read. Everything else is
// ambiguous by construction: a transport failure may have delivered the request and
// lost the response, and errAmbiguousStart is billet's own name for a 200 that named no
// build.
//
// A 5xx IS NOT A REFUSAL, AND CALLING IT ONE WAS A WAY TO STRAND A BUILD. The first
// version asked only "does this error carry a code", and an AWS 500 carries one — so a
// server error that had COMMITTED the build and lost its response was read as proof
// nothing started, and Launch deleted the parameter that build was about to resolve.
// The build then starts, finds no registration, registers nothing, and every signal
// billet has says the launch merely failed. The retry policy one file over already
// treats a 5xx as ambiguous and refuses to retry it; the two must not disagree about
// the same response, and the deletion is the side where disagreeing is expensive.
//
// A REFUSED REDIRECT IS CONCLUSIVE, and it is the one non-API case that is: billet
// refuses it in CheckRedirect, so the request never left for the target — what
// answered was a redirect rather than the API, and the signed request reached only
// the endpoint billet chose.
func conclusiveness(err error) launchOutcome {
	if errors.Is(err, errAmbiguousStart) {
		return launchAmbiguous
	}

	if errors.Is(err, errRedirected) {
		return launchConclusive
	}

	if apiErr, ok := errors.AsType[*apiError](err); ok && apiErr.Status < http.StatusInternalServerError {
		return launchConclusive
	}

	return launchAmbiguous
}

// errAmbiguousStart is a StartBuild that succeeded and named no build.
//
// A SENTINEL, because the caller has to tell it from a refusal and matching prose is
// how that comparison rots. A 200 with no id means something started and billet cannot
// name it, which is the one success that must be treated as a failure — and as the most
// ambiguous kind.
var errAmbiguousStart = errors.New("codebuild: StartBuild accepted a launch and named no build")

func joinRefusals(refusals []string) string {
	if len(refusals) == 0 {
		return "no compute type was attempted"
	}

	return strings.Join(refusals, ", ")
}

// capacityRefusal reports whether a code means "not this shape, here, now" — AWS
// refusing synchronously, having started nothing.
//
// WHAT MAKES A FALLBACK SAFE, and the whole reason it is a named list rather than a
// status range. These are AWS's own verdict that the request was rejected, so
// another shape is a fresh question; anything else may have committed.
//
//	AccountLimitExceededException  the account's concurrent-build quota for that
//	                               environment and compute type is full. THE MOST
//	                               LIKELY ONE BY FAR, because the default quota is
//	                               1 — which is why a fallback matters here even
//	                               more than it does on ec2.
//	ResourceAlreadyExistsException the idempotency token matched a build already
//	                               started for this exact (lease, shape). Not a
//	                               capacity problem, and it is NOT listed: the
//	                               token is doing its job, and treating it as a
//	                               reason to try another shape would launch a
//	                               second build for one job.
func capacityRefusal(code string) bool {
	return code == "AccountLimitExceededException"
}

// startBuild asks CodeBuild for one build of one compute type.
func (p *Provider) startBuild(
	ctx context.Context, spec provider.Spec, ct config.RemoteShape, buildspec string,
) (*provider.Instance, error) {
	in := map[string]any{
		"projectName": p.cfg.Project,
		// THE KEY THAT MAKES A FAST RETRY SAFE, and only a fast one — five minutes.
		// See idempotencyTokenFor.
		"idempotencyToken":        idempotencyTokenFor(spec.Name, ct.Type),
		"buildspecOverride":       buildspec,
		"computeTypeOverride":     ct.Type,
		"environmentTypeOverride": string(p.cfg.EnvironmentType),
		// BOTH CEILINGS ARE STATED RATHER THAN INHERITED FROM THE PROJECT, so what
		// bounds a job is the number this node's operator declared and the number
		// the inventory window is derived from. A project edited underneath billet
		// would otherwise change how far back List has to walk, silently.
		"timeoutInMinutesOverride":       p.cfg.BuildTimeoutMinutes,
		"queuedTimeoutInMinutesOverride": p.cfg.QueuedTimeoutMinutes,
		"environmentVariablesOverride":   p.environmentOverrides(spec),
	}

	// THE SOURCE IS PINNED TO NO_SOURCE ON EVERY LAUNCH. billet clones nothing —
	// the runner does the checkout inside the job — and a project that acquired a
	// source (an operator edit, a shared project) would have CodeBuild fetch a
	// repository before the runner starts, on a machine holding a registration.
	in["sourceTypeOverride"] = "NO_SOURCE"

	if p.cfg.FleetARN != "" {
		in["fleetOverride"] = map[string]any{"fleetArn": p.cfg.FleetARN}
	}

	// STATED ONLY FOR A CONTAINER ENVIRONMENT. config refuses privileged_mode
	// elsewhere, and sending it anyway would be billet asking for a privilege that
	// does not exist on the machine it is asking about.
	if p.cfg.EnvironmentType.Container() {
		in["privilegedModeOverride"] = p.cfg.PrivilegedMode
	}

	if spec.Image != "" {
		in["imageOverride"] = spec.Image
	}

	// THE LOG GROUP IS PINNED ON EVERY LAUNCH, and it used to be sent only when the
	// operator named one.
	//
	// WHY THAT MATTERED: the build role's IAM grant is scoped to a group ARN, and with
	// no override the PROJECT's own configuration decided where the build wrote. For a
	// project billet created they agree — CodeBuild's default is
	// /aws/codebuild/<project>, which is what config.LogGroupName derives — but for an
	// ADOPTED or edited project carrying a custom group they do not, and the result is
	// a role that cannot write its own logs. Pinning it makes billet's policy true by
	// construction rather than true if nobody edited the project, which is the same
	// reason the source type, both timeouts and the environment are pinned here too.
	in["logsConfigOverride"] = map[string]any{
		"cloudWatchLogs": map[string]any{
			"status":    "ENABLED",
			"groupName": p.cfg.LogGroupName(),
		},
	}

	// callCreating, NOT call: a StartBuild whose outcome billet cannot see must not
	// be retried, because the request may already have started a build. See
	// client.callCreating — the idempotency token bounds that risk to five minutes
	// and billet does not control how long an ambiguous attempt takes.
	var out startBuildResponse
	if err := p.api.callCreating(ctx, "StartBuild", in, &out); err != nil {
		return nil, p.wrapLaunchError(spec, ct, err)
	}

	if out.Build.ID == "" {
		// A 200 WITH NO ID IS NOT A LAUNCH THAT DID NOT HAPPEN. Something started and
		// billet cannot name it, which is the ambiguous case: refusing here and saying
		// what to look for is the only honest answer, and Find is what the caller uses
		// to resolve it.
		//
		// IT WRAPS A SENTINEL so the caller can tell it from a refusal without matching
		// prose — and the caller's decision is whether to delete a credential a build
		// may be about to read.
		return nil, fmt.Errorf("%w: the launch of %s was accepted; a build may be RUNNING "+
			"under environment marker %s=%s — look for it before retrying",
			errAmbiguousStart, spec.Name, nameEnvVar, spec.Name)
	}

	p.log.Info("started a build", "runner", spec.Name, "build", out.Build.ID, "type", ct.Type)

	return p.instanceFrom(out.Build), nil
}

// environmentOverrides are the markers and the credential reference one build gets.
//
// TWO PLAINTEXT MARKERS AND ONE REFERENCE, and the split is the security property.
// The owner and the lease name are not secrets and must be readable by billet
// without decrypting anything — they are how List and Find tell billet's builds from
// anybody else's, since a build cannot be tagged. The REGISTRATION is a
// PARAMETER_STORE reference, so what reaches the console, the build metadata and
// CloudTrail's request rendering is the parameter's NAME.
func (p *Provider) environmentOverrides(spec provider.Spec) []map[string]any {
	return []map[string]any{
		{"name": ownerEnvVar, "value": p.owner, "type": "PLAINTEXT"},
		{"name": nameEnvVar, "value": spec.Name, "type": "PLAINTEXT"},
		{
			"name":  jitEnvVar,
			"value": p.jitParameterName(spec.Name),
			"type":  "PARAMETER_STORE",
		},
	}
}

// wrapLaunchError adds what billet was doing without adding what it was carrying.
//
// THE API'S MESSAGE IS DROPPED FOR THE ONE ACTION THAT SENDS A CREDENTIAL. StartBuild
// carries the parameter's name rather than its value, so the exposure is smaller
// here than on ec2's user data — but the request also carries the whole generated
// buildspec, and a service that echoes a rejected request back is not a thing to
// find out about from a log. The CODE is from a fixed enumeration and is what an
// operator acts on.
func (p *Provider) wrapLaunchError(
	spec provider.Spec, ct config.RemoteShape, err error,
) error {
	if code, ok := codeOf(err); ok {
		return fmt.Errorf("codebuild: launch %s as %s: %w",
			spec.Name, ct.Type, &apiError{Code: code, Status: statusOf(err)})
	}

	return fmt.Errorf("codebuild: launch %s as %s: %w", spec.Name, ct.Type, withoutMessage(err))
}

func statusOf(err error) int {
	if apiErr, ok := errors.AsType[*apiError](err); ok {
		return apiErr.Status
	}

	return 0
}

// withoutMessage keeps a transport error's shape without its prose.
//
// A transport error's text carries the URL it was working on, which for this action
// is the endpoint billet signed for — safe — but net/http composes that text from
// whatever answered. The sentinel path is already handled in the client; this is the
// backstop for anything else on the one action that sends a credential reference.
func withoutMessage(err error) error {
	if errors.Is(err, errRedirected) {
		return err
	}

	return errors.New("the api could not be reached")
}

// instanceFrom is what billet knows about one build.
func (p *Provider) instanceFrom(b build) *provider.Instance {
	name, _ := envValue(b.Environment.EnvironmentVariables, nameEnvVar)

	return &provider.Instance{
		ID:       b.ID,
		Name:     name,
		Running:  runningState(b.BuildStatus),
		Terminal: terminalStatus(b.BuildStatus),
	}
}

// Destroy stops a build and waits for it to actually stop.
//
// IDEMPOTENT: a build that is already gone or already finished is success, because
// teardown runs on paths that have already failed once and an error there turns a
// recoverable state into a stuck one.
//
// IT POLLS TO A TERMINAL STATE BEFORE CLAIMING ONE. StopBuild is a REQUEST — the
// same lesson `tart stop` taught one backend over, and the same lesson
// TerminateInstances taught the other: reading the state once immediately
// afterwards catches a build mid-transition and reports it as still running, while
// returning TeardownStopped on the strength of the request being accepted frees
// capacity for a job that is still executing somebody's deploy. Only a status
// CodeBuild positively reports as terminal earns TeardownStopped; everything else —
// including running out of polls — returns TeardownRequested, which keeps the
// capacity charged and hands the question to custody.
func (p *Provider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	if id == "" {
		return provider.TeardownRequested, errors.New("codebuild: destroy needs a build id")
	}

	if err := p.api.call(ctx, "StopBuild", map[string]any{"id": id}, nil); err != nil {
		code, ok := codeOf(err)

		switch {
		// A BUILD AWS HAS FORGOTTEN IS NOT AN ERROR AND IS NOT PROOF EITHER. Not an
		// error, because retrying a stop against an id CodeBuild does not know
		// accomplishes nothing forever. Not proof, because billet did not observe a
		// terminal state — the confirmation below is what does.
		case ok && code == "ResourceNotFoundException":
			return provider.TeardownRequested, nil

		// AND A BUILD THAT WAS ALREADY OVER IS THE ORDINARY RACE, not a failure:
		// billet asks for a teardown on completion, and the runner exiting ends the
		// build on its own. The poll below still has to confirm it.
		case ok && code == "InvalidInputException":
			return p.confirmStopped(ctx, id)

		default:
			return provider.TeardownRequested, fmt.Errorf("codebuild: destroy %s: %w", id, err)
		}
	}

	p.log.Info("requested a build stop", "build", id)

	return p.confirmStopped(ctx, id)
}

// confirmStopped polls one build until CodeBuild reports it terminal.
func (p *Provider) confirmStopped(ctx context.Context, id string) (provider.Teardown, error) {
	for attempt := range teardownPollLimit {
		if attempt > 0 {
			if err := p.sleep(ctx, teardownPollInterval); err != nil {
				// A CANCELLED WAIT IS NOT A STOPPED BUILD. The caller's context
				// ending says nothing about the compute, so the capacity stays
				// charged — and the reason travels with it, because a teardown
				// that reports nothing is indistinguishable from one nobody has
				// got to yet.
				return provider.TeardownRequested, fmt.Errorf(
					"codebuild: wait to confirm build %s stopped: %w", id, err)
			}
		}

		builds, notFound, err := p.batchGet(ctx, []string{id})
		if err != nil {
			// COULD NOT ASK IS NOT AN ANSWER. TeardownRequested keeps the capacity
			// charged and lets the ordinary sweep resolve it, which is what custody
			// is for — and the error goes with it, or an expired credential reads as
			// a teardown that is merely taking its time, forever.
			return provider.TeardownRequested, fmt.Errorf(
				"codebuild: confirm build %s stopped: %w", id, err)
		}

		// AN ID CODEBUILD DOES NOT KNOW IS NOT A CONFIRMED STOP. Build history is
		// retained for a year, so an id genuinely absent from a batch is billet
		// asking about something that never existed under this project — which is
		// not the same fact as a build having ended, and must not free capacity.
		if len(notFound) > 0 {
			return provider.TeardownRequested, nil
		}

		for _, b := range builds {
			if b.ID != id {
				continue
			}

			if terminalStatus(b.BuildStatus) {
				// THE CREDENTIAL GOES ONLY AFTER THE BUILD IS OVER, which is the
				// one ordering that cannot strand a runner: a build still starting
				// may not have read the parameter yet.
				if err := p.deleteJITConfig(ctx, b.name()); err != nil {
					p.log.Warn("a build ended and its staged runner registration could not be "+
						"removed; delete the parameter by hand", "build", id, "error", err)
				}

				return provider.TeardownStopped, nil
			}
		}
	}

	// OUT OF POLLS IS NOT A FAILURE. A node runs one command at a time and each
	// command's timeout starts when it is QUEUED, so waiting here indefinitely would
	// stall every launch behind it. The capacity stays charged and custody resolves
	// it out of band, which is exactly the shape ec2's teardown settled on.
	p.log.Info("a stopped build has not reached a terminal state yet; keeping its capacity "+
		"charged and leaving it to the sweep", "build", id)

	return provider.TeardownRequested, nil
}

// name reports the lease name a build carries in its environment.
func (b build) name() string {
	name, _ := envValue(b.Environment.EnvironmentVariables, nameEnvVar)

	return name
}

// Find reports the build with that name, and whether there was one.
//
// A WALK RATHER THAN A LOOKUP, because CodeBuild offers no way to ask. There is no
// tag to filter on and no status filter on the listing, so the only route is recent
// history — bounded by the fact that CodeBuild itself ends a build once the declared
// ceilings elapse.
//
// IT INCLUDES TERMINAL BUILDS, unlike List, and that difference is the same one the
// ec2 backend makes between findStates and liveStates: a targeted lookup wants the
// terminal record as CAUSAL PROOF for custody, while fleet inventory must not carry
// a year of history for reconciliation to re-tear-down.
func (p *Provider) Find(ctx context.Context, name string) (*provider.Instance, bool, error) {
	b, ok, err := p.findBuild(ctx, name)
	if err != nil || !ok {
		return nil, false, err
	}

	return p.instanceFrom(b), true, nil
}

// findBuild is Find without the projection, so a caller that needs the build's
// PHASE can have it.
//
// The phase is what establishes that a registration has been consumed, and
// provider.Instance deliberately carries no such field — it is the narrow contract
// every backend agrees on. Projecting first and then wanting the phase back is how
// the tidy path would end up guessing from Running and Terminal alone.
func (p *Provider) findBuild(ctx context.Context, name string) (build, bool, error) {
	if name == "" {
		return build{}, false, errors.New("codebuild: find needs a name")
	}

	var (
		live     build
		liveID   string
		terminal build
		haveTerm bool
	)

	err := p.walk(ctx, func(b build) error {
		// COMPARED EXACTLY. The marker is read out of a response, so this is the
		// only thing standing between "the build for this lease" and "a build whose
		// marker happens to start the same way" — and the caller's next move on a
		// hit is to STOP it.
		if b.name() != name {
			return nil
		}

		// A LIVE BUILD BEATS A TERMINAL ONE, WHATEVER THE ORDER, and taking the
		// newest match was a way to free a running build's capacity.
		//
		// Custody reads a terminal answer here as CAUSAL PROOF and settles the lease
		// on it. So if a lease somehow has two builds and the NEWEST is terminal
		// while an older one is still running, returning the newest hands back the
		// capacity of compute that is still executing somebody's job — the exact
		// failure `List` refuses duplicates to prevent, arriving through the targeted
		// lookup that did not. Every match is examined rather than the first.
		if !terminalStatus(b.BuildStatus) {
			// TWO LIVE BUILDS ARE REFUSED, the same answer List gives: billet cannot
			// tell which one the lease's runner is inside, capacity is charged once,
			// and the caller's next move is to stop what comes back.
			if liveID != "" {
				return fmt.Errorf(
					"codebuild: builds %s and %s in project %s are both live and both carry "+
						"%s=%s, so billet cannot tell which one this lease's runner is inside "+
						"and will not name either. Stop one of them (aws codebuild stop-build "+
						"--id …) and billet will reconcile the survivor",
					liveID, b.ID, p.cfg.Project, nameEnvVar, name)
			}

			live, liveID = b, b.ID

			return nil
		}

		// THE NEWEST TERMINAL ONE, which is what the walk's order gives — and what
		// makes that TRUE rather than hoped for is batchGet restoring the listing's
		// order. ListBuildsForProject is documented as descending by build number;
		// BatchGetBuilds is documented as answering about the ids it was given and
		// NOT as answering in that order. MEASURED against real CodeBuild on
		// 2026-08-31 it does preserve request order — asked in reverse it answered in
		// reverse — which is exactly why the missing guarantee was invisible.
		if !haveTerm {
			terminal, haveTerm = b, true
		}

		return nil
	})
	if err != nil {
		return build{}, false, err
	}

	// THE LIVE ONE IF THERE IS ONE. A terminal record is only proof that THIS lease's
	// compute is gone when nothing for this lease is still running.
	if liveID != "" {
		return live, true, nil
	}

	if haveTerm {
		return terminal, true, nil
	}

	return build{}, false, nil
}

// List reports every build this backend is running for billet.
//
// THE INPUT TO RECONCILIATION, which frees the capacity of every lease ABSENT from
// it. So the dangerous failure is not an error — an error stops reconciliation — but
// an answer that is short, empty and successful.
//
// IT FAILS WHOLE RATHER THAN SHORT. A build carrying this deployment's owner marker
// whose lease marker names no lease is compute billet cannot account for, and
// omitting it would hand back the capacity of something still running. The message
// names the build and both remedies, because failing closed stops this node's sweep
// until somebody intervenes — which is the right direction only if an operator is
// told what to do.
//
// TERMINAL BUILDS ARE EXCLUDED, the liveStates half of the ec2 split: CodeBuild
// retains a year of history and reconciliation would otherwise be handed a year of
// corpses to stop on every pass.
func (p *Provider) List(ctx context.Context) ([]*provider.Instance, error) {
	var instances []*provider.Instance

	// TWO LIVE BUILDS UNDER ONE LEASE NAME IS REFUSED, not reported twice and not
	// deduplicated.
	//
	// A lease should only ever have one build; the launch path is written so an
	// ambiguous StartBuild never retries precisely to keep that true. If it happens
	// anyway, billet cannot tell which build the lease's runner is inside — custody is
	// keyed by lease id, so one entry would silently overwrite the other and the
	// capacity of whichever lost would be handed back while its build kept running.
	// Refusing the whole inventory stops reconciliation instead, which is the safe
	// direction and the same answer this walk already gives an undescribable build.
	seen := make(map[string]string)

	err := p.walk(ctx, func(b build) error {
		if terminalStatus(b.BuildStatus) {
			return nil
		}

		name := b.name()

		if first, dup := seen[name]; dup {
			return fmt.Errorf(
				"codebuild: builds %s and %s in project %s are both live and both carry "+
					"%s=%s, so billet cannot tell which one this lease's runner is inside. "+
					"Its capacity is charged once and custody is keyed by the lease, so "+
					"reporting either would hand back the other's. Stop one of them (aws "+
					"codebuild stop-build --id …) and billet will reconcile the survivor",
				first, b.ID, p.cfg.Project, nameEnvVar, name)
		}

		seen[name] = b.ID

		if _, ours := provider.LeaseOf(name); !ours {
			return fmt.Errorf(
				"codebuild: build %s in project %s carries this deployment's owner marker "+
					"(%s=%s) but its %s marker is %q, which names no lease, so billet cannot "+
					"account for it and will not report an inventory it is missing from: either "+
					"stop that build or, if it is not billet's, give this deployment a project "+
					"of its own — a build cannot be tagged, so a shared project is how billet "+
					"comes to stop somebody else's work",
				b.ID, p.cfg.Project, ownerEnvVar, p.owner, nameEnvVar, name)
		}

		instances = append(instances, p.instanceFrom(b))

		return nil
	})
	if err != nil {
		return nil, err
	}

	return instances, nil
}

// walk visits every build in the inventory window that carries this deployment's
// owner marker.
//
// THE WINDOW IS WHAT MAKES THIS FINITE. ListBuildsForProject has no status filter
// and CodeBuild retains a year of history, so walking to the end would be O(a
// year's jobs) on every sweep. What bounds it is the service's OWN enforced
// timeout: a build that started longer ago than the declared build and queued
// ceilings plus slack cannot still be running, because CodeBuild ended it. The
// operator's declared ceiling therefore sizes this walk, which is why a tighter one
// is cheaper.
//
// SORT ORDER IS NOT REQUESTED. AWS documents ListBuildsForProject as descending by
// build number by default AND as ERRORING if sortOrder is passed when the project
// has more than 100 builds — so asking for the order billet wants is what breaks at
// scale, and the default is what works.
//
// A PAGE IS ONLY ABANDONED ONCE EVERY BUILD IN IT IS OUTSIDE THE WINDOW. Stopping
// at the first old build would be wrong: the listing is ordered by build NUMBER,
// which is start order, and a build that queued for hours starts later than one
// numbered after it. Requiring the whole page keeps that reordering from truncating
// the walk.
func (p *Provider) walk(ctx context.Context, visit func(build) error) error {
	now := p.now()

	// TWO CUTOFFS, AND THEY ARE NOT THE SAME QUESTION.
	//
	// `liveBefore` decides whether one build could still be running: a build that
	// started longer ago than the declared build and queued ceilings plus slack
	// cannot be, because CodeBuild ended it.
	//
	// `abandonBefore` decides whether the WALK may stop, and it is deliberately
	// older. The listing is ordered by build NUMBER, which is submission order, and
	// a build that queued for hours starts LATER than a build submitted after it —
	// so a page of higher-numbered builds can all have started outside the window
	// while a lower-numbered one, still queued when they ran, is inside it.
	// Stopping on the first such page would drop a build that may be executing
	// somebody's job. The maximum a start can lag a submission is the queued
	// ceiling, so subtracting it again makes the stop condition sound: if every
	// build on a page started before this, every lower-numbered build was submitted
	// before that too and cannot still be running.
	liveBefore := now.Add(-time.Duration(p.cfg.InventoryWindowMinutes()) * time.Minute)
	abandonBefore := liveBefore.Add(-time.Duration(p.queuedCeilingMinutes()) * time.Minute)

	token := ""
	// EVERY TOKEN, NOT ONLY THE LAST ONE. Comparing against the immediately
	// preceding token catches a token that repeats itself and misses a CYCLE —
	// A, B, A, B — which loops forever. A sweep that never returns stops reporting
	// this host's inventory, and the capacity of anything quarantined on it is held
	// until an operator intervenes.
	seen := map[string]struct{}{}

	for {
		in := map[string]any{"projectName": p.cfg.Project}
		if token != "" {
			in["nextToken"] = token
		}

		var page listBuildsForProjectResponse
		if err := p.api.call(ctx, "ListBuildsForProject", in, &page); err != nil {
			return fmt.Errorf("codebuild: list builds in project %s: %w", p.cfg.Project, err)
		}

		// AN EMPTY PAGE IS NOT THE END OF THE ANSWER, and treating it as one turned an
		// incomplete pagination response into a SHORT SUCCESSFUL inventory — after
		// which reconciliation frees the capacity of every lease that existed only on
		// a later page, while its build keeps running. What ends a listing is the
		// absence of a token; a page with no ids and a token is a page to walk past.
		//
		// THE CYCLE GUARD STILL APPLIES, because an empty page that hands back a token
		// billet has already followed is the loop this walk is bounded against.
		if len(page.IDs) == 0 {
			if page.NextToken == "" {
				return nil
			}

			if _, repeat := seen[page.NextToken]; repeat {
				return fmt.Errorf("codebuild: listing builds in project %s revisited a "+
					"pagination token, so the walk would not end; billet will not report an "+
					"inventory from a listing it cannot finish", p.cfg.Project)
			}

			seen[page.NextToken] = struct{}{}
			token = page.NextToken

			continue
		}

		builds, notFound, err := p.batchGet(ctx, page.IDs)
		if err != nil {
			return err
		}

		// AN ID THE PROJECT JUST LISTED AND THE BATCH CANNOT DESCRIBE IS NOT AN
		// ABSENT BUILD — it is billet unable to say anything about a build in its
		// own project, one API call after being told the build exists. Build
		// history is retained for a year, so this is not the ordinary
		// aged-out case.
		//
		// IT FAILS THE WALK, because the alternative is the exact failure the
		// provider contract warns about: `List` frees the capacity of every lease
		// ABSENT from it, so silently skipping an undescribable build hands back
		// the capacity of compute that may still be running. An error stops
		// reconciliation, which is the safe direction; the first version of this
		// discarded the not-found list entirely.
		if len(notFound) > 0 {
			return fmt.Errorf("codebuild: project %s listed build(s) %v that BatchGetBuilds "+
				"could not describe; billet cannot account for them and will not report an "+
				"inventory they are missing from — retry, and if it persists check whether "+
				"another principal is deleting build history for this project",
				p.cfg.Project, notFound)
		}

		for _, b := range builds {
			if startedBefore(b, liveBefore) {
				continue
			}

			// SOMEBODY ELSE'S BUILD IN THIS PROJECT IS SKIPPED, NOT REFUSED. A
			// project is supposed to be billet's alone, and refusing a foreign
			// build would stop this node's sweep over something an operator may
			// have started once by hand. What IS refused is a build claiming to be
			// THIS deployment's and naming no lease — see List.
			if owner, ok := envValue(b.Environment.EnvironmentVariables, ownerEnvVar); !ok ||
				owner != p.owner {
				continue
			}

			if err := visit(b); err != nil {
				return err
			}
		}

		// THE WHOLE PAGE HAS TO BE PAST THE ABANDON CUTOFF, not merely its last
		// entry and not merely outside the live window.
		//
		// SOUND ONLY BECAUSE batchGet PROVED THE RESPONSE ACCOUNTS FOR EVERY ID.
		// With the two response lists allowed to differ from the request, a page
		// whose ids came back undescribable produced an empty `builds`, which read
		// as "nothing here is recent" and stopped the walk. The first version
		// claimed the lengths were equal "by construction" and they were not —
		// nothing in the protocol says the two lists cover the request. What makes
		// it true is accountsForEvery, which refuses a response that omits, repeats
		// or invents an id.
		stop := true

		for _, b := range builds {
			if !startedBefore(b, abandonBefore) {
				stop = false

				break
			}
		}

		if stop {
			return nil
		}

		if page.NextToken == "" {
			return nil
		}

		if _, repeated := seen[page.NextToken]; repeated {
			return fmt.Errorf("codebuild: the api returned a pagination token it had already "+
				"given for project %s; refusing to loop", p.cfg.Project)
		}

		seen[page.NextToken] = struct{}{}
		token = page.NextToken
	}
}

// queuedCeilingMinutes is the declared queued ceiling, or the service maximum when
// nothing declared one.
func (p *Provider) queuedCeilingMinutes() int {
	if p.cfg.QueuedTimeoutMinutes > 0 {
		return p.cfg.QueuedTimeoutMinutes
	}

	return config.CodeBuildQueuedCeilingMinutes
}

// startedBefore reports whether a build began before the window opened.
//
// A BUILD WITH NO START TIME IS INSIDE THE WINDOW, deliberately. Zero is what a
// build that has not started yet reports — SUBMITTED or QUEUED — and treating that
// as ancient would drop from the inventory the very builds most likely to be about
// to run somebody's job.
func startedBefore(b build, cutoff time.Time) bool {
	if b.StartTime <= 0 {
		return false
	}

	sec := int64(b.StartTime)
	nsec := int64((b.StartTime - float64(sec)) * 1e9)

	return time.Unix(sec, nsec).Before(cutoff)
}

// batchGet describes up to pageSize builds, reporting which ids CodeBuild did not
// know.
//
// THE NOT-FOUND LIST IS RETURNED RATHER THAN DISCARDED. CodeBuild answers a batch
// with two lists, and folding an absent id into "a build whose fields are all zero"
// would give it a zero status — which runningState reads as RUNNING, forever.
func (p *Provider) batchGet(ctx context.Context, ids []string) ([]build, []string, error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}

	if len(ids) > pageSize {
		return nil, nil, fmt.Errorf("codebuild: asked about %d builds and BatchGetBuilds accepts "+
			"%d", len(ids), pageSize)
	}

	var out batchGetBuildsResponse
	if err := p.api.call(ctx, "BatchGetBuilds", map[string]any{"ids": ids}, &out); err != nil {
		return nil, nil, fmt.Errorf("codebuild: describe builds: %w", err)
	}

	if err := accountsForEvery(ids, out); err != nil {
		return nil, nil, err
	}

	return inRequestOrder(ids, out.Builds), out.BuildsNotFound, nil
}

// inRequestOrder returns the described builds in the order they were asked about.
//
// BECAUSE THE CALLER'S ORDER CARRIES MEANING. ListBuildsForProject answers newest
// first, and Find takes the first match as the most recent build for a lease — a
// claim that is only true if the batch has not reordered them. AWS documents
// BatchGetBuilds as answering about the ids it was given and says nothing about
// order, and measured, it happens to preserve it; that coincidence is what made the
// missing guarantee invisible rather than what makes the code correct.
//
// SOUND BECAUSE accountsForEvery ALREADY PROVED A BIJECTION: every requested id is
// described exactly once or reported absent, so this is a permutation and never a
// filter.
func inRequestOrder(ids []string, builds []build) []build {
	if len(builds) < 2 {
		return builds
	}

	byID := make(map[string]build, len(builds))
	for _, b := range builds {
		byID[b.ID] = b
	}

	out := make([]build, 0, len(builds))
	for _, id := range ids {
		if b, ok := byID[id]; ok {
			out = append(out, b)
		}
	}

	return out
}

// accountsForEvery proves the response says something about every id that was asked
// about, and nothing about any other.
//
// "THE TWO LISTS ARE EQUAL BY CONSTRUCTION" WAS AN ASSUMPTION, NOT A FACT, and the
// walk's stop condition was written on top of it. Nothing in the protocol guarantees
// that `builds` plus `buildsNotFound` covers the request: an id omitted from BOTH is a
// build the walk never sees, and because List frees the capacity of every lease absent
// from its answer, that is capacity handed back for compute that may still be running.
// Rejecting a non-empty buildsNotFound — which is what the first version did — catches
// only the case AWS bothered to report.
//
// EXACT ACCOUNTING RATHER THAN A COUNT. A duplicate would make the totals add up while
// leaving one id unaccounted for, and an id billet never asked about means the response
// is not an answer to this request at all.
func accountsForEvery(ids []string, out batchGetBuildsResponse) error {
	asked := make(map[string]bool, len(ids))
	for _, id := range ids {
		asked[id] = true
	}

	seen := make(map[string]bool, len(ids))

	for _, group := range [][]string{buildIDs(out.Builds), out.BuildsNotFound} {
		for _, id := range group {
			switch {
			case !asked[id]:
				return fmt.Errorf("codebuild: BatchGetBuilds answered about build %q, which was "+
					"not asked about; billet cannot treat that as an answer to this request "+
					"and will not report an inventory derived from it", id)

			case seen[id]:
				return fmt.Errorf("codebuild: BatchGetBuilds answered twice about build %q; the "+
					"response cannot be reconciled with the request, and an inventory derived "+
					"from it could omit a build that is still running", id)
			}

			seen[id] = true
		}
	}

	if len(seen) == len(asked) {
		return nil
	}

	missing := make([]string, 0, len(asked)-len(seen))
	for _, id := range ids {
		if !seen[id] {
			missing = append(missing, id)
		}
	}

	return fmt.Errorf("codebuild: BatchGetBuilds said nothing about build(s) %v — neither "+
		"described nor reported absent — so billet cannot account for them and will not report "+
		"an inventory they are missing from", missing)
}

// buildIDs is the ids a batch described.
//
// A DESCRIBED BUILD WITH NO ID IS ITSELF UNACCOUNTABLE, and it lands in the missing set
// rather than being skipped: the empty string cannot match anything asked about, so the
// accounting above refuses it as an unexpected id. That is the right direction — a row
// billet cannot name is a row it cannot attribute to a lease.
func buildIDs(builds []build) []string {
	ids := make([]string, 0, len(builds))
	for _, b := range builds {
		ids = append(ids, b.ID)
	}

	return ids
}

// ReapStagedCredential removes a lease's staged runner registration.
//
// provider.StagedCredentialReaper. A CodeBuild build cannot be handed a secret —
// `StartBuild` has no field for one — so the registration lives in Parameter Store and
// OUTLIVES the build. Destroying the compute does not remove it, which is what makes
// this a contract rather than part of teardown.
//
// THE CALLER'S PROOF IS THE AUTHORISATION, and the first version tried to derive its
// own. It asked `findBuild` and deleted when the build looked consumed OR when no build
// was found at all — and that second branch is the unsafe one: a `StartBuild` that
// committed and lost its response leaves a build the inventory has not listed yet, so
// "I cannot find it" is indistinguishable from "it has not appeared", and deleting there
// strands exactly the build the ambiguous-launch path deliberately keeps the parameter
// for. Billet calls this only where the compute is already PROVED gone — custody
// settlement, and the failed-launch cleanup that confirms on an explicit terminal
// record — so there is nothing left to ask. See provider.StagedCredentialReaper: the
// precondition is the proof rather than any one call site.
//
// AND IT WAS NEVER CALLED, which is the defect underneath the unsafe one: its own doc
// comment said the node's tending sweep invoked it and nothing in production did. So
// every lease that settled without a Destroy — which is the ordinary case here, since
// the runner exits and the build ends on its own — left its parameter behind until
// somebody hit the account's Parameter Store quota.
//
// AN ABSENT PARAMETER IS SUCCESS, because the teardown path deletes it when it confirms
// a build terminal and this is then a second call about something already gone.
func (p *Provider) ReapStagedCredential(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("codebuild: reaping a staged registration needs a name")
	}

	return p.deleteJITConfig(ctx, name)
}
