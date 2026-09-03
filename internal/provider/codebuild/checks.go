package codebuild

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// The read-only lookups `billet check --provider codebuild` makes to prove a config
// can actually run a job, and to put the two external ceilings in front of an
// operator BEFORE a tier advertises capacity. The account's concurrency quota is
// the third thing they have to see and is read separately, in quota.go, because
// it needs credentials and a network where these do not.
//
// Each asks the narrowest question that proves something, chosen so the probe itself
// cannot cost anything: nothing here starts a build, and nothing here writes a
// parameter — a probe that staged a registration would leave a credential behind
// every time somebody ran a diagnostic.

// Ceilings is what a CodeBuild tier inherits from the service and cannot escape.
//
// REPORTED AS A CAPABILITY rather than buried in a config comment, because the
// backend's acceptance turns on it: an operator has to see the 36-hour cap and
// the 8-hour queued cap before work is admitted. The alternative is meeting the
// first one as a build that died at hour 36.
//
// THE CONCURRENCY QUOTA IS NOT HERE, and this comment used to claim it was. It
// is an ACCOUNT limit rather than a service one — read from Service Quotas by
// Quotas() in quota.go, which needs credentials and a network where everything
// below needs neither. Keeping the two apart is what lets `billet check` report
// these on a laptop with no AWS access at all, which is the one moment the
// sentence is most useful.
type Ceilings struct {
	// BuildMinutes and QueuedMinutes are what this node declared, which is what
	// billet sends. They are the OPERATOR's numbers.
	BuildMinutes  int
	QueuedMinutes int
	// ServiceBuildMinutes and ServiceQueuedMinutes are the service's own maxima,
	// so a report can say whether the declared numbers are the ceiling or a choice
	// inside it.
	ServiceBuildMinutes  int
	ServiceQueuedMinutes int
	// InventoryWindowMinutes is how far back List has to walk before an absence is
	// conclusive — derived from the declared ceilings, and worth reporting because
	// it is the cost a longer ceiling buys.
	InventoryWindowMinutes int
	// Reserved says whether this node draws on a reserved-capacity fleet, which
	// decides both the isolation story and the cost shape.
	Reserved bool
	// MacOS says whether this node runs Apple silicon, which is reserved-only.
	MacOS bool
}

// CeilingsFor reports what a configured node's jobs inherit, without calling AWS.
func CeilingsFor(cfg config.CodeBuildConfig) Ceilings {
	return Ceilings{
		BuildMinutes:           cfg.BuildTimeoutMinutes,
		QueuedMinutes:          cfg.QueuedTimeoutMinutes,
		ServiceBuildMinutes:    config.CodeBuildBuildCeilingMinutes,
		ServiceQueuedMinutes:   config.CodeBuildQueuedCeilingMinutes,
		InventoryWindowMinutes: cfg.InventoryWindowMinutes(),
		Reserved:               cfg.FleetARN != "",
		MacOS:                  cfg.EnvironmentType == config.CodeBuildMacARM,
	}
}

// ProjectReport is what a project turned out to be.
type ProjectReport struct {
	Name string
	ARN  string
	// EnvironmentType and ComputeType are the project's own settings. billet
	// OVERRIDES both on every launch, so a mismatch is reported rather than
	// refused — but it is worth seeing, because an operator reading the console
	// will see the project's numbers rather than billet's.
	EnvironmentType string
	ComputeType     string
	// SourceType should be NO_SOURCE. billet pins it on every launch, so this is
	// reported rather than refused; a project with a source configured is a sign
	// somebody is using it for something else.
	SourceType string
	// OwnerTag is the deployment identity tagged on the project, or empty. THE
	// PROJECT IS HALF THE OWNERSHIP BOUNDARY, because a build cannot be tagged, so
	// a project carrying another deployment's tag is the thing to catch here.
	OwnerTag string
	// RunnerWebhook is set when the project carries a WORKFLOW_JOB_QUEUED webhook.
	//
	// THE ONE FINDING THAT IS A REFUSAL RATHER THAN A REPORT. It means CodeBuild is
	// acquiring jobs too, and two schedulers on one job produce duplicate runners —
	// a failure that looks like GitHub misbehaving rather than like a configuration
	// mistake.
	RunnerWebhook bool
}

// FleetReport is what a reserved fleet turned out to be.
type FleetReport struct {
	Name string
	ARN  string
	// EnvironmentType must match the node's, or every launch is refused by AWS.
	EnvironmentType string
	ComputeType     string
	// BaseCapacity is how many builds can run at once, which is the number a
	// macOS tier's macos_vm_limit should be set to.
	BaseCapacity int
	// MaxCapacity is the scaling ceiling when one is configured, or zero.
	MaxCapacity int
	Status      string
	StatusNote  string
	// StatusContext is AWS's reason beside the status code, when it has one. An
	// ACTIVE fleet AWS cannot find an instance for reports INSUFFICIENT_CAPACITY
	// here and looks healthy by Status alone.
	StatusContext string
}

// DescribeProject reports what the configured project is.
//
// A PROJECT THAT RESOLVES TO NOTHING IS AN ERROR rather than an empty result: the
// check asked about a specific project, and "there is no such project" is the answer
// it needs.
func (p *Provider) DescribeProject(ctx context.Context) (ProjectReport, error) {
	var out batchGetProjectsResponse

	if err := p.api.call(ctx, "BatchGetProjects",
		map[string]any{"names": []string{p.cfg.Project}}, &out); err != nil {
		return ProjectReport{}, fmt.Errorf("codebuild: describe project %s: %w", p.cfg.Project, err)
	}

	if len(out.Projects) == 0 {
		return ProjectReport{}, fmt.Errorf("codebuild: project %s does not exist in %s; billet "+
			"does not create it — the terraform module or your own tooling does",
			p.cfg.Project, p.cfg.Region)
	}

	d := out.Projects[0]
	report := ProjectReport{
		Name:            d.Name,
		ARN:             d.ARN,
		EnvironmentType: d.Environment.Type,
		ComputeType:     d.Environment.ComputeType,
		SourceType:      d.Source.Type,
		RunnerWebhook:   hasRunnerWebhook(d),
	}

	for _, t := range d.Tags {
		if t.Key == projectOwnerTag {
			report.OwnerTag = t.Value
		}
	}

	return report, nil
}

// projectOwnerTag is the deployment identity a billet-owned project carries.
//
// THE SAME KEY THE ec2 BACKEND PUTS ON AN INSTANCE, so an operator reading tags
// across a deployment sees one name rather than two. It goes on the PROJECT here
// because a build cannot carry one.
const projectOwnerTag = "sh.billet.owner"

// hasRunnerWebhook reports whether a project would also acquire jobs itself.
//
// MATCHED ON THE FILTER RATHER THAN ON THE WEBHOOK'S EXISTENCE. A webhook alone is
// not the problem — a project could legitimately carry one for something else — and
// what makes two schedulers is specifically a WORKFLOW_JOB_QUEUED event filter,
// which is what CodeBuild's runner integration installs.
//
// A WEBHOOK WITH NO FILTER GROUPS COUNTS. CodeBuild's own tutorial creates the
// filter, but a webhook whose filters billet cannot read is one it cannot vouch for,
// and the safe direction on "two schedulers may be running" is to say so.
func hasRunnerWebhook(d projectDescription) bool {
	if d.Webhook == nil {
		return false
	}

	if len(d.Webhook.FilterGroups) == 0 {
		return true
	}

	for _, group := range d.Webhook.FilterGroups {
		for _, f := range group {
			if strings.EqualFold(f.Type, "EVENT") &&
				strings.Contains(strings.ToUpper(f.Pattern), "WORKFLOW_JOB_QUEUED") {
				return true
			}
		}
	}

	return false
}

// DescribeFleet reports what the configured reserved fleet is, or that none is
// configured.
func (p *Provider) DescribeFleet(ctx context.Context) (FleetReport, bool, error) {
	if p.cfg.FleetARN == "" {
		return FleetReport{}, false, nil
	}

	var out batchGetFleetsResponse

	if err := p.api.call(ctx, "BatchGetFleets",
		map[string]any{"names": []string{p.cfg.FleetARN}}, &out); err != nil {
		return FleetReport{}, false, fmt.Errorf("codebuild: describe fleet: %w", err)
	}

	if len(out.Fleets) == 0 {
		return FleetReport{}, false, errors.New("codebuild: node.codebuild.fleet_arn names a " +
			"fleet that does not exist; billet does not create one")
	}

	f := out.Fleets[0]
	report := FleetReport{
		Name:            f.Name,
		ARN:             f.ARN,
		EnvironmentType: f.EnvironmentType,
		ComputeType:     f.ComputeType,
		BaseCapacity:    f.BaseCapacity,
		Status:          f.Status.StatusCode,
		StatusNote:      f.Status.Message,
		StatusContext:   f.Status.Context,
	}

	if f.ScalingConfiguration != nil {
		report.MaxCapacity = f.ScalingConfiguration.MaxCapacity
	}

	return report, true, nil
}

// Problems reports what a live look found that an operator has to act on.
//
// SPLIT FROM THE DESCRIBES so the reports stay plain data and this stays the one
// place that judges them — which is also what lets `billet check` print the facts
// even when one of them is a problem.
//
// EXACTLY ONE OF THESE IS FATAL and the rest are warnings, and the distinction is
// whether billet would do the wrong thing or merely a surprising one. A competing
// runner webhook makes two schedulers acquire one job, which produces duplicate
// runners billet never escrowed capacity for; everything else is a mismatch an
// operator should see and that billet overrides on every launch anyway.
func (r ProjectReport) Problems(owner string) ([]string, []string) {
	var fatal, warnings []string

	if r.RunnerWebhook {
		fatal = append(fatal, fmt.Sprintf("project %s carries a WORKFLOW_JOB_QUEUED webhook, so "+
			"CodeBuild's own runner integration is acquiring jobs as well as billet. Two "+
			"schedulers on one job produce duplicate runners, which reads as GitHub "+
			"misbehaving. Delete the webhook (aws codebuild delete-webhook --project-name %s) "+
			"or give billet a project of its own", r.Name, r.Name))
	}

	switch {
	case r.OwnerTag == "":
		warnings = append(warnings, fmt.Sprintf("project %s carries no %s tag, so nothing "+
			"records which deployment owns it. A build cannot be tagged, so the project is "+
			"half of how billet tells its own compute from anybody else's — and List stops "+
			"builds", r.Name, projectOwnerTag))

	case r.OwnerTag != owner:
		warnings = append(warnings, fmt.Sprintf("project %s is tagged %s=%s but this deployment "+
			"is %s. Sharing a project between deployments means each one's reconciliation "+
			"sees the other's builds", r.Name, projectOwnerTag, r.OwnerTag, owner))
	}

	if r.SourceType != "" && r.SourceType != "NO_SOURCE" {
		warnings = append(warnings, fmt.Sprintf("project %s has source type %s; billet pins "+
			"NO_SOURCE on every launch, so this is unused — but it suggests the project is "+
			"also being used for something else", r.Name, r.SourceType))
	}

	return fatal, warnings
}

// Problems reports what a fleet's shape means for this node.
func (r FleetReport) Problems(cfg config.CodeBuildConfig) ([]string, []string) {
	var fatal, warnings []string

	if r.EnvironmentType != "" && r.EnvironmentType != string(cfg.EnvironmentType) {
		fatal = append(fatal, fmt.Sprintf("fleet %s is a %s fleet but node.codebuild."+
			"environment_type is %s; AWS refuses every launch that pairs them, so this tier "+
			"would advertise capacity and fail every job",
			r.Name, r.EnvironmentType, cfg.EnvironmentType))
	}

	// A FLEET PENDING DELETION STILL SERVES BUILDS, AND SAYING OTHERWISE CONTRADICTED
	// THE MESSAGE BILLET WAS PRINTING BESIDE IT.
	//
	// MEASURED: `DeleteFleet` moves a reserved fleet to PENDING_DELETION and AWS's own
	// status note — which billet quotes in this very diagnostic — reads "Fleets are
	// available to build projects while they are pending deletion." A real macOS job
	// then ran to green on a fleet in exactly that state. Calling it fatal refused a
	// deployment that works, which is the ADR-005 failure, and it did so while
	// displaying the sentence that says it is wrong.
	//
	// It is still a WARNING rather than silence, because the state is temporary and an
	// operator planning around it needs to know the floor is scheduled to go away.
	if strings.EqualFold(r.Status, "PENDING_DELETION") {
		warnings = append(warnings, fmt.Sprintf("fleet %s is %s (%s); it serves builds now, "+
			"but capacity disappears when AWS finishes the deletion and every launch after "+
			"that fails", r.Name, r.Status, r.StatusNote))
	} else if r.Status != "" && !strings.EqualFold(r.Status, "ACTIVE") &&
		!strings.EqualFold(r.Status, "CREATING") && !strings.EqualFold(r.Status, "UPDATING") &&
		!strings.EqualFold(r.Status, "ROTATING") {
		fatal = append(fatal, fmt.Sprintf("fleet %s is %s (%s), so it cannot serve builds",
			r.Name, r.Status, r.StatusNote))
	}

	// AN ACTIVE FLEET CAN HAVE NO MACHINE BEHIND IT, AND THE STATUS CODE DOES NOT
	// SAY SO. Measured 2026-09-02: a fresh MAC_ARM fleet reported ACTIVE nineteen
	// seconds after CreateFleet and then sat with `status.context =
	// INSUFFICIENT_CAPACITY` ("We currently do not have sufficient capacity for the
	// instance type you requested") for the best part of an hour, while every build
	// dispatched to it queued until the queued ceiling failed it — and this check
	// printed "capacity 1" with no warning. A WARNING rather than a refusal, because
	// the state is AWS's and temporary: the fleet is correctly configured, and the
	// operator's choices are to wait, to pick another compute type, or to delete it.
	//
	// ONLY A CONTEXT THAT SAYS SOMETHING THE CODE DOES NOT. A fleet pending
	// deletion reports `status.context = PENDING_DELETION` beside the same code
	// (measured 2026-09-02), and reading that as a capacity problem printed "is
	// PENDING_DELETION but AWS reports PENDING_DELETION … until an instance is
	// provisioned" about a fleet with a warm Mac that had just run a job.
	if r.StatusContext != "" && !strings.EqualFold(r.StatusContext, r.Status) {
		warnings = append(warnings, fmt.Sprintf("fleet %s is %s but AWS reports %s (%s); "+
			"until an instance is provisioned every build dispatched to it queues until "+
			"queued_timeout_minutes fails it, and nothing billet does changes that",
			r.Name, r.Status, r.StatusContext, r.StatusNote))
	}

	// THE CAPACITY IS THE NUMBER A MACOS TIER'S macos_vm_limit SHOULD BE, and
	// billet cannot read it from the config — which is exactly why config validation
	// asks for it rather than assuming Apple's per-host allowance. Reporting it here
	// is what turns that refusal into something an operator can satisfy.
	if r.BaseCapacity > 0 {
		warnings = append(warnings, fmt.Sprintf("fleet %s runs %d build(s) at once; a macOS "+
			"tier pinned to this node should set nodes[].macos_vm_limit to that number, and "+
			"billet will not assume Apple's per-host allowance applies to a fleet AWS operates",
			r.Name, r.BaseCapacity))
	}

	// A DECLARED COMPUTE TYPE THAT THE FLEET DOES NOT OFFER is a warning rather than
	// a refusal, because a fleet with attribute-based compute reports a resolved
	// type that need not match what an operator declared — and refusing a working
	// deployment over a string comparison against somebody else's enum is the
	// failure ADR-005 names.
	if r.ComputeType != "" {
		declared := false

		for _, ct := range cfg.ComputeTypes {
			if ct.Type == r.ComputeType {
				declared = true

				break
			}
		}

		if !declared {
			warnings = append(warnings, fmt.Sprintf("fleet %s reports compute type %s, which is "+
				"not in node.codebuild.compute_types; billet overrides the type on every "+
				"launch, so check that what it buys is what you meant to declare",
				r.Name, r.ComputeType))
		}
	}

	return fatal, warnings
}

// ConcurrencyProblems judges the macOS concurrency a deployment declared for this
// node against what the fleet can actually run at once.
//
// declared is the node's effective macOS cap — nodes[].macos_vm_limit, which every
// macOS tier pinned to the node is validated against — and a zero means the caller
// established none, in which case there is nothing to compare and nothing is said.
// THE CALLER DECIDES WHETHER THE COMPARISON APPLIES: a Linux fleet runs no macOS
// build whatever the policy says, so judging one against a limit that is legal to
// declare and used by nothing refuses a working deployment (cmd/billet gates on
// the environment type).
//
// A REFUSAL, NOT A WARNING, and the measurement is why. With a base capacity of 1
// and a declared limit of 2, billet did exactly what the config asked: it escrowed
// two jobs and started two builds, and the second sat QUEUED behind the busy Mac.
// GitHub withdraws an assignment that is not acquired within about five minutes,
// so that queued build was stopped, the job requeued, a fresh build queued, and the
// cycle repeated until the first job finished — every build correct, every one a
// StartBuild against the account's 30-wide queue, and the job's wall clock the
// first job's duration plus a requeue. A declared limit above the fleet is
// therefore capacity the fleet does not have, advertised to GitHub as if it did,
// and MAC_ARM offers no overflow to on-demand (`Fleet on-demand overflow behavior
// is not supported for MAC_ARM`, measured), so nothing on the AWS side can absorb
// it. The fix is one number in the config, which is what the message names.
//
// The bound is the fleet's BASE capacity. A scaling configuration's maximum is what
// AWS may grow to, not what it holds, and a build queued while the fleet scales is
// the same queue as above — so a limit the maximum covers is a WARNING that says
// so, for an operator who has measured their fleet scaling fast enough.
//
// AND A CAPACITY BILLET COULD NOT READ IS SAID, NOT PASSED. CreateFleet refuses a
// base capacity below one, so a zero here is a description that omitted the field
// rather than a fleet with no machines; "could not tell" is still not "fine", and
// the warning says which number went unchecked.
func (r FleetReport) ConcurrencyProblems(declared int) ([]string, []string) {
	if declared <= 0 {
		return nil, nil
	}

	if r.BaseCapacity <= 0 {
		return nil, []string{fmt.Sprintf("fleet %s reported no base capacity, so the declared "+
			"macos_vm_limit %d was not checked against it", r.Name, declared)}
	}

	if declared <= r.BaseCapacity {
		return nil, nil
	}

	if r.MaxCapacity >= declared {
		return nil, []string{fmt.Sprintf("this node declares macos_vm_limit %d; fleet %s holds "+
			"%d build(s) and may scale to %d, so every build past %d queues while it scales, "+
			"and GitHub withdraws a queued assignment after about five minutes and requeues it",
			declared, r.Name, r.BaseCapacity, r.MaxCapacity, r.BaseCapacity)}
	}

	remedy := fmt.Sprintf("set nodes[].macos_vm_limit to %d", r.BaseCapacity)
	if r.MaxCapacity > r.BaseCapacity {
		// BOTH ACCEPTED VALUES, because naming only the base contradicts the
		// warning branch above, which accepts anything up to the maximum.
		remedy = fmt.Sprintf("set nodes[].macos_vm_limit to %d, or to at most %d (the fleet's "+
			"scaling maximum) if you have measured it scaling fast enough",
			r.BaseCapacity, r.MaxCapacity)
	}

	return []string{fmt.Sprintf("this node declares macos_vm_limit %d but fleet %s runs %d "+
		"build(s) at once; the excess is advertised to GitHub as capacity the fleet does not "+
		"have, each job past the fleet's capacity queues behind it, GitHub withdraws a queued "+
		"assignment after about five minutes and requeues it, and MAC_ARM offers no "+
		"on-demand overflow — %s", declared, r.Name, r.BaseCapacity, remedy)}, nil
}

// CheckReachable proves this identity can reach the API and see its own project.
//
// ONE CALL, NO RETRY LADDER: check is interactive and rerunning it is cheaper than
// masking a flap.
func CheckReachable(ctx context.Context, cfg config.CodeBuildConfig, opts ...Option) error {
	p, err := New(reachabilityOwner, cfg, opts...)
	if err != nil {
		return err
	}

	if _, err := p.DescribeProject(ctx); err != nil {
		return err
	}

	return nil
}

// reachabilityOwner is the deployment identity a reachability probe constructs with.
//
// A PLACEHOLDER, BECAUSE NOTHING IT DOES IS SCOPED BY OWNER. New requires one — an
// empty identity is refused, since a provider that cannot tell its own compute from
// another billet's is the thing that destroys somebody else's work — and
// DescribeProject reads a project rather than acting on a build, so there is nothing
// here for an identity to protect. It is named rather than an empty string so a
// CloudTrail reader can see what made the call.
const reachabilityOwner = "billet-reachability-probe"
