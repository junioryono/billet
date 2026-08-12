// Package ec2 launches one instance per job in a cloud region.
//
// THIS IS THE AVAILABILITY STORY, NOT A PROOF OF THE ABSTRACTION. A tier that
// lists `providers: [firecracker, ec2]` may be placed on either, so losing the
// bare-metal host does not take the `runs-on` label down with it — which is the
// difference between self-hosted CI you can rely on and self-hosted CI you can
// rely on until the power goes out.
//
// TWO THINGS ABOUT THIS BACKEND ARE UNLIKE EVERY OTHER ONE, and both follow from
// the compute being somewhere else:
//
// The node running it is an ORCHESTRATOR. It holds credentials and calls an API;
// nothing runs on the machine billet is running on. So what that machine has is
// not what this node can offer, and `node.max_vcpu` / `node.max_memory` are
// required rather than detected — they are a spending limit, and billet has no
// standing to choose one.
//
// And the instance IS the isolation boundary, so this backend can run fork
// pull-request code that the docker backend must refuse. That boundary is the
// kernel, though, and not the network: untrusted work runs only once a separate
// security group has been described for it.
package ec2

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// ownerTag carries the DEPLOYMENT identity of the billet that launched an
// instance — not its node name, which defaults to the hostname and is therefore
// shared by every billet installation on one machine.
//
// This tag is what List filters on, and List feeds a loop that terminates, so a
// tag two installations could both carry is a way for one to destroy the other's
// live jobs.
const ownerTag = "sh.billet.owner"

// nameTag is where the instance's billet name lives. EC2 has no name of its own
// — "Name" is a tag by convention — and this one encodes the lease, which is the
// only durable link between a running instance and the lease that authorised it.
const nameTag = "Name"

// jitEnvVar is what the GitHub runner reads its single-use registration from.
const jitEnvVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// liveStates are the instance states billet asks the API about.
//
// TERMINATED IS EXCLUDED, and that is the one real difference from the docker
// backend, where a stopped container still counts because it holds its name, its
// volumes and its disk. A terminated instance holds nothing: it is not billed, it
// blocks no name, and EC2 keeps reporting it for up to an hour purely as
// history. Including them would give reconciliation an hour's worth of corpses to
// re-terminate on every pass.
var liveStates = []string{"pending", "running", "shutting-down", "stopping", "stopped"}

// Provider launches EC2 instances, one per job.
type Provider struct {
	log   *slog.Logger
	owner string
	cfg   config.EC2Config
	api   *client

	// rootDevices caches an AMI's root device name, which billet has to ask for
	// before it can resize a root volume. It does not change for a given AMI.
	mu          sync.Mutex
	rootDevices map[string]string
}

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(p *Provider) { p.log = log }
}

// WithCredentials sets where AWS credentials come from. The default is the
// environment, then this instance's own IAM role.
func WithCredentials(src CredentialSource) Option {
	return func(p *Provider) { p.api.creds = src }
}

// WithHTTPClient sets the client used for API calls, for a test or for a
// deployment that needs a proxy.
func WithHTTPClient(c *http.Client) Option {
	return func(p *Provider) { p.api.http = c }
}

// apiTimeout bounds one API call.
//
// Generous next to the calls themselves, which answer in well under a second,
// because the thing being bounded is a stall rather than the work. It is still
// far inside the plane's command timeout, so a wedged region surfaces as a launch
// failure the listener can hand capacity back for, rather than as a command the
// control plane gives up on and calls custody.
const apiTimeout = 30 * time.Second

// New builds an EC2 provider. owner names this billet deployment and is written
// onto every instance it starts.
func New(owner string, cfg config.EC2Config, opts ...Option) (*Provider, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("ec2: a provider needs the deployment identity that tags its " +
			"instances, or it cannot tell its own compute from another billet's")
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = "https://ec2." + cfg.Region + ".amazonaws.com/"
	}

	// RE-APPLIED HERE, not merely validated at load. This constructor is exported,
	// so it cannot assume its configuration came through config.Load — and the
	// rule it enforces is that a signed request carrying a session token does not
	// go out in plaintext, which is not a rule worth having one entry point for.
	if strings.TrimSpace(cfg.Endpoint) != "" {
		if err := config.CheckEC2Endpoint(strings.TrimSpace(cfg.Endpoint)); err != nil {
			return nil, fmt.Errorf("ec2: %w", err)
		}
	}

	if _, err := url.Parse(endpoint); err != nil {
		return nil, fmt.Errorf("ec2: node.ec2.endpoint %q is not a url: %w", endpoint, err)
	}

	p := &Provider{
		log:   slog.Default(),
		owner: owner,
		cfg:   cfg,
		api: &client{
			http:     &http.Client{Timeout: apiTimeout},
			endpoint: endpoint,
			region:   cfg.Region,
			creds:    DefaultCredentials(),
		},
		rootDevices: make(map[string]string),
	}

	for _, opt := range opts {
		opt(p)
	}

	return p, nil
}

// Kind reports the backend this is.
func (p *Provider) Kind() config.ProviderKind { return config.ProviderEC2 }

// Accepts reports whether this backend may run work of that trust class.
//
// UNTRUSTED IS PERMITTED HERE, unlike the container backend, because a whole
// instance is a real isolation boundary: fork pull-request code gets its own
// kernel and its own machine, and the machine is destroyed afterwards.
//
// BUT ONLY ONCE ITS NETWORK HAS BEEN DESCRIBED. The boundary an instance provides
// is the kernel, not the VPC — a fork's job in the same security group as
// everything else reaches whatever that group reaches, which on a subnet somebody
// already had is usually a good deal more than they are picturing. Defaulting to
// the trusted group would be deciding that on their behalf, silently, in the
// direction that cannot be undone once a job has run.
//
// UNKNOWN is refused outright, and that is not the same judgement. Untrusted is a
// classification billet made; unknown means it could not classify the job at all,
// so there is no basis for choosing either group.
func (p *Provider) Accepts(trust provider.TrustClass) error {
	switch trust {
	case provider.TrustTrusted:
		return nil

	case provider.TrustUntrusted:
		if len(p.cfg.UntrustedSecurityGroupIDs) > 0 {
			return nil
		}

		return errors.New("ec2: refusing to run untrusted work until it has a network of its " +
			"own: an instance isolates the kernel but not the VPC, so set " +
			"node.ec2.untrusted_security_group_ids to a group that reaches only what a fork's " +
			"pull request should be able to reach")

	case provider.TrustUnknown:
		return errors.New("ec2: refusing to run work billet could not classify: an unrecognised " +
			"event establishes no provenance, so there is no basis for choosing which network " +
			"to place it on")

	default:
		return fmt.Errorf("ec2: refusing to run %s work", trust)
	}
}

// securityGroups reports which groups a workload of that trust class gets.
func (p *Provider) securityGroups(trust provider.TrustClass) []string {
	if trust == provider.TrustUntrusted {
		return p.cfg.UntrustedSecurityGroupIDs
	}

	return p.cfg.SecurityGroupIDs
}

// instanceTypeFor picks the shape to buy for a lease.
//
// THE FIRST DECLARED SHAPE THAT FITS, in the operator's order, which is the same
// rule a tier's provider list follows: the order is a preference and billet does
// not reorder it. Picking the "smallest" instead would look thriftier and would
// quietly override an operator who listed a compute-optimised shape first for a
// reason billet cannot see.
func (p *Provider) instanceTypeFor(spec provider.Spec) (config.EC2InstanceType, error) {
	for _, it := range p.cfg.InstanceTypes {
		if it.VCPU >= spec.VCPU && it.Memory >= spec.Memory {
			return it, nil
		}
	}

	declared := make([]string, 0, len(p.cfg.InstanceTypes))
	for _, it := range p.cfg.InstanceTypes {
		declared = append(declared, fmt.Sprintf("%s (%d vCPU, %s)", it.Type, it.VCPU, it.Memory))
	}

	// NAMES BOTH SIDES. The allocator has already escrowed this size against this
	// node, so reaching here means the config promised capacity in shapes it never
	// declared — and an operator needs to see the gap rather than the conclusion.
	return config.EC2InstanceType{}, fmt.Errorf(
		"ec2: no declared instance type holds %d vCPU and %s (declared: %s); "+
			"node.max_vcpu and node.max_memory promised the allocator capacity that "+
			"node.ec2.instance_types cannot supply",
		spec.VCPU, spec.Memory, strings.Join(declared, ", "))
}

// Launch starts one instance running the job its JIT config names.
func (p *Provider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	if spec.Name == "" {
		return nil, errors.New("ec2: a spec needs a name")
	}

	if spec.Image == "" {
		return nil, fmt.Errorf("ec2: %s has no image; this backend reads the tier's image as an "+
			"AMI id", spec.Name)
	}

	if spec.JITConfig == "" {
		return nil, fmt.Errorf("ec2: %s has no JIT config, so nothing would register", spec.Name)
	}

	// Checked again here, not only via Accepts. A caller is expected to ask first
	// so a refusal costs no runner registration, but a backend that only refuses
	// when asked politely is not a boundary.
	if err := p.Accepts(spec.Trust); err != nil {
		return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
	}

	// REFUSED, not defaulted, for the same reason every backend refuses it: an
	// image's default boot does something, so a spec with no command produces an
	// instance that starts, reports success, and never registers a runner.
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf("ec2: %s has no command, so the image would boot without ever "+
			"starting a runner and the job would stay queued", spec.Name)
	}

	instanceType, err := p.instanceTypeFor(spec)
	if err != nil {
		return nil, err
	}

	userData, err := p.userData(spec)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Set("Action", "RunInstances")
	params.Set("ImageId", spec.Image)
	params.Set("InstanceType", instanceType.Type)
	params.Set("MinCount", "1")
	params.Set("MaxCount", "1")
	params.Set("UserData", base64.StdEncoding.EncodeToString([]byte(userData)))

	// THE NAME IS THE IDEMPOTENCY KEY, and this is what makes an ambiguous launch
	// safe. A RunInstances that commits and loses its response is the exact case
	// the Provider interface warns about — and here billet can do better than
	// asking afterwards, because AWS will honour the token and return the SAME
	// instance rather than starting a second one. The name encodes the lease, so
	// it is unique by construction and never reused.
	params.Set("ClientToken", spec.Name)

	p.setNetwork(params, spec.Trust)
	p.setTags(params, spec.Name)

	// IMDSv2 REQUIRED, ONE HOP. The hop limit is what keeps a container running
	// inside the job from reaching the instance metadata service at all, which is
	// where the user data — and any instance profile — is readable.
	params.Set("MetadataOptions.HttpTokens", "required")
	params.Set("MetadataOptions.HttpPutResponseHopLimit", "1")

	if p.cfg.InstanceProfile != "" {
		params.Set("IamInstanceProfile.Name", p.cfg.InstanceProfile)
	}

	if p.cfg.Spot {
		params.Set("InstanceMarketOptions.MarketType", "spot")
		// ONE-TIME AND TERMINATE. A persistent request would relaunch the instance
		// after a reclaim, and the job that was running on it is gone — GitHub does
		// not requeue a job whose runner vanished, so the replacement would come up
		// with nothing to do and hold a lease until something reaped it.
		params.Set("InstanceMarketOptions.SpotOptions.SpotInstanceType", "one-time")
		params.Set("InstanceMarketOptions.SpotOptions.InstanceInterruptionBehavior", "terminate")
	}

	if err := p.setRootVolume(ctx, params, spec); err != nil {
		return nil, err
	}

	var out runInstancesResponse

	if err := p.api.call(ctx, params, &out); err != nil {
		return nil, fmt.Errorf("ec2: launch %s: %w", spec.Name, err)
	}

	if len(out.Instances) == 0 {
		return nil, fmt.Errorf("ec2: launch %s was accepted but described no instance", spec.Name)
	}

	id := out.Instances[0].InstanceID
	if id == "" {
		return nil, fmt.Errorf("ec2: launch %s reported no instance id", spec.Name)
	}

	p.log.Info("launched instance", "runner", spec.Name, "instance", id,
		"type", instanceType.Type, "image", spec.Image, "spot", p.cfg.Spot)

	return &provider.Instance{ID: id, Name: spec.Name, Running: true}, nil
}

// setNetwork puts the instance in a subnet with the groups its trust class earns.
//
// TWO SPELLINGS BECAUSE EC2 HAS TWO. A public address can only be requested
// through a network interface block, and a request carrying both a top-level
// SubnetId and an interface is refused — so the choice of spelling follows
// assign_public_ip rather than being a style preference.
func (p *Provider) setNetwork(params url.Values, trust provider.TrustClass) {
	groups := p.securityGroups(trust)

	if !p.cfg.AssignPublicIP {
		params.Set("SubnetId", p.cfg.SubnetID)

		for i, g := range groups {
			params.Set("SecurityGroupId."+strconv.Itoa(i+1), g)
		}

		return
	}

	params.Set("NetworkInterface.1.DeviceIndex", "0")
	params.Set("NetworkInterface.1.SubnetId", p.cfg.SubnetID)
	params.Set("NetworkInterface.1.AssociatePublicIpAddress", "true")
	params.Set("NetworkInterface.1.DeleteOnTermination", "true")

	for i, g := range groups {
		params.Set("NetworkInterface.1.SecurityGroupId."+strconv.Itoa(i+1), g)
	}
}

// setTags labels the instance so billet can find its own compute again.
func (p *Provider) setTags(params url.Values, name string) {
	params.Set("TagSpecification.1.ResourceType", "instance")
	params.Set("TagSpecification.1.Tag.1.Key", nameTag)
	params.Set("TagSpecification.1.Tag.1.Value", name)
	params.Set("TagSpecification.1.Tag.2.Key", ownerTag)
	params.Set("TagSpecification.1.Tag.2.Value", p.owner)

	// AND ON THE VOLUME TOO. A root volume that outlives a failed termination is
	// billed until somebody finds it, and an untagged one is not findable — the
	// same orphan problem the instance tags exist for, one resource down.
	params.Set("TagSpecification.2.ResourceType", "volume")
	params.Set("TagSpecification.2.Tag.1.Key", nameTag)
	params.Set("TagSpecification.2.Tag.1.Value", name)
	params.Set("TagSpecification.2.Tag.2.Key", ownerTag)
	params.Set("TagSpecification.2.Tag.2.Value", p.owner)
}

// setRootVolume states what happens to the root disk, and resizes it when the
// tier asked for a size.
//
// IT RUNS ON EVERY LAUNCH, EVEN WITH NO SIZE TO SET, because DeleteOnTermination
// is the half that always matters. Left unstated, whatever the AMI was built with
// governs — and an AMI built with it false leaves a root volume behind for every
// job billet ever runs on it, billed indefinitely, discoverable only by hunting
// tags. Stating it costs one DescribeImages per AMI, cached for the life of the
// process.
//
// THE DEVICE NAME IS ASKED FOR RATHER THAN ASSUMED, which is what that call buys.
// A block device mapping naming a device that is not the AMI's root does not
// fail: EC2 attaches an ADDITIONAL empty volume, the root stays whatever size the
// image was built at, and the launch reports success. So a tier asking for 300GiB
// would run out of disk mid-job while an unused 300GiB volume sat beside it,
// billed. `/dev/sda1` and `/dev/xvda` are both common and neither is safe to
// guess.
func (p *Provider) setRootVolume(ctx context.Context, params url.Values, spec provider.Spec) error {
	device, err := p.rootDevice(ctx, spec.Image)
	if err != nil {
		return err
	}

	params.Set("BlockDeviceMapping.1.DeviceName", device)
	params.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")

	if spec.Disk <= 0 {
		return nil
	}

	// ROUNDED UP. EBS sizes in whole GiB, and rounding down would hand a tier that
	// asked for 80GiB a 79GiB disk — the direction that fails a job rather than
	// costing a fraction of a cent.
	gib := (int64(spec.Disk) + int64(config.GiB) - 1) / int64(config.GiB)

	params.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.FormatInt(gib, 10))

	return nil
}

// rootDevice reports an AMI's root device name, asking once per image.
func (p *Provider) rootDevice(ctx context.Context, image string) (string, error) {
	p.mu.Lock()
	cached, ok := p.rootDevices[image]
	p.mu.Unlock()

	if ok {
		return cached, nil
	}

	params := url.Values{}
	params.Set("Action", "DescribeImages")
	params.Set("ImageId.1", image)

	var out describeImagesResponse

	if err := p.api.call(ctx, params, &out); err != nil {
		return "", fmt.Errorf("ec2: describe image %s to size its root volume: %w", image, err)
	}

	if len(out.Images) == 0 {
		return "", fmt.Errorf("ec2: image %s does not exist in %s, or this deployment cannot "+
			"see it", image, p.cfg.Region)
	}

	device := out.Images[0].RootDeviceName
	if device == "" {
		return "", fmt.Errorf("ec2: image %s reports no root device name, so billet cannot size "+
			"its root volume without attaching a second disk by mistake", image)
	}

	p.mu.Lock()
	p.rootDevices[image] = device
	p.mu.Unlock()

	return device, nil
}

// userData is the boot script that starts the runner.
//
// THE CREDENTIAL IS IN USER DATA, and that is a different trade from the one the
// container backend refuses. There, the JIT config must stay out of argv because
// the host is SHARED — every other job on that machine could read it. Here the
// instance holds exactly one job, is destroyed with it, and the registration is
// that job's own, single-use, and consumed by the runner before any workflow step
// runs. What user data must not become is a channel to anything ELSE, which is
// why the metadata service is pinned to IMDSv2 with a one-hop limit: a container
// inside the job cannot reach it at all.
func (p *Provider) userData(spec provider.Spec) (string, error) {
	// A PLAIN SINGLE-QUOTED ASSIGNMENT, which is the one construct with no
	// parsing left in it. Inside single quotes a POSIX shell expands nothing at
	// all, and the only character that needs handling is the quote itself.
	//
	// The first version used a quoted heredoc inside `$( )` — which reads as
	// safer, since a quoted delimiter also suppresses every expansion — and a
	// test that RAN the generated script found it is not: a single quote in the
	// value confused the parser scanning for the closing paren, and /bin/sh died
	// with "unexpected EOF" on a later line. A boot script that fails to parse is
	// an instance that starts, registers nothing, and reports success.
	jit, err := shellQuote(spec.JITConfig)
	if err != nil {
		return "", fmt.Errorf("ec2: the JIT config for %s cannot be carried in a boot script: %w",
			spec.Name, err)
	}

	command, err := shellCommand(spec.Command)
	if err != nil {
		return "", fmt.Errorf("ec2: %s: %w", spec.Name, err)
	}

	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\numask 077\n")
	b.WriteString(jitEnvVar + "=" + jit + "\n")
	b.WriteString("export " + jitEnvVar + "\n")
	b.WriteString("exec " + command + "\n")

	return b.String(), nil
}

// shellQuote renders a value as a single-quoted shell word.
//
// Single quotes suppress every form of expansion the shell has, and a single
// quote inside the value is closed, escaped and reopened — the only sequence that
// works, because there is no escape character inside single quotes.
//
// A NUL is the one byte no shell can carry, so it is refused rather than
// truncating the value at it. That matters most for the registration: a
// credential cut short produces a runner that cannot register, which surfaces as
// a job that stays queued while everything reports success.
func shellQuote(v string) (string, error) {
	if strings.ContainsRune(v, 0) {
		return "", errors.New("the value contains a null byte, which no shell can carry")
	}

	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'", nil
}

// shellCommand renders a command as a single-quoted shell word list.
func shellCommand(argv []string) (string, error) {
	quoted := make([]string, 0, len(argv))

	for _, arg := range argv {
		q, err := shellQuote(arg)
		if err != nil {
			return "", fmt.Errorf("a command argument cannot be carried in a boot script: %w", err)
		}

		quoted = append(quoted, q)
	}

	return strings.Join(quoted, " "), nil
}

// Destroy terminates an instance, whether or not it is still running.
//
// Idempotent: an id that is already gone is success. Teardown runs on paths that
// have already failed once, and erroring there turns recoverable state into stuck
// state.
//
// IT RETURNS WHEN THE REQUEST IS ACCEPTED, NOT WHEN THE MACHINE IS GONE, and that
// is a real difference from the container backend, where `docker rm --force`
// finishes the job. An instance sits in `shutting-down` for a while afterwards.
//
// What makes that safe is the state filter above: `shutting-down` is one of the
// states List asks for, and runningState counts it as running — so the instance
// stays in this host's INVENTORY until EC2 has finished with it, and the control
// plane goes on charging its capacity to this node. The capacity comes back when
// the machine is provably gone rather than when billet asked for it to go, which
// is the rule custody follows everywhere else.
//
// Waiting here instead would be worse: a node executes one command at a time, so
// blocking teardown on a poll would stall every other launch and destroy behind
// it.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("ec2: destroy needs an instance id")
	}

	params := url.Values{}
	params.Set("Action", "TerminateInstances")
	params.Set("InstanceId.1", id)

	if err := p.api.call(ctx, params, nil); err != nil {
		// MATCHED ON THE API'S OWN CODE, never on its prose. An instance that is
		// already gone is the idempotent case, and it is the one thing here that
		// must not be reported as a failure — a caller reads a destroy error as
		// "the compute may still exist" and keeps holding the capacity.
		if code, ok := codeOf(err); ok && code == "InvalidInstanceID.NotFound" {
			return nil
		}

		return fmt.Errorf("ec2: destroy %s: %w", id, err)
	}

	p.log.Info("terminated instance", "instance", id)

	return nil
}

// Find reports the instance with that name, and whether there was one.
func (p *Provider) Find(ctx context.Context, name string) (*provider.Instance, bool, error) {
	found, err := p.describe(ctx, filter{name: "tag:" + nameTag, values: []string{name}})
	if err != nil {
		return nil, false, err
	}

	// COMPARED EXACTLY AFTERWARDS even though an EC2 tag filter is an exact match
	// rather than docker's substring. A tag filter DOES honour `*` as a wildcard,
	// so a name carrying one would match more than itself — and the caller's next
	// move on a hit is to terminate.
	for _, inst := range found {
		if inst.Name == name {
			return inst, true, nil
		}
	}

	return nil, false, nil
}

// List reports every instance this backend is running for billet.
func (p *Provider) List(ctx context.Context) ([]*provider.Instance, error) {
	return p.describe(ctx)
}

// filter is one DescribeInstances filter, before it is given a number.
//
// NUMBERED WHERE THEY ARE RENDERED RATHER THAN AT THE CALL SITE, because the
// query API numbers a list from 1 with no gaps. A caller that hard-coded
// `Filter.2` for its own filter left `List` — which adds none — emitting
// `Filter.1` and `Filter.3`, and a gap is not a filter the API silently ignores
// in a direction anybody wants: a DescribeInstances that drops its state filter
// returns an hour of terminated instances, and reconciliation reads those as
// compute to account for.
type filter struct {
	name   string
	values []string
}

// describe runs DescribeInstances with billet's owner tag plus any extra filters,
// following pagination to the end.
func (p *Provider) describe(ctx context.Context, extra ...filter) ([]*provider.Instance, error) {
	var instances []*provider.Instance

	filters := append([]filter{
		{name: "tag:" + ownerTag, values: []string{p.owner}},
		{name: "instance-state-name", values: liveStates},
	}, extra...)

	token := ""
	// EVERY TOKEN, NOT ONLY THE LAST ONE. Comparing against the immediately
	// preceding token catches a token that repeats itself and misses a CYCLE —
	// A, B, A, B — which loops forever. A sweep that never returns stops reporting
	// this host's inventory, and the capacity of anything quarantined on it is
	// held until an operator intervenes.
	seen := map[string]struct{}{}

	for {
		params := url.Values{}
		params.Set("Action", "DescribeInstances")

		for i, f := range filters {
			n := strconv.Itoa(i + 1)
			params.Set("Filter."+n+".Name", f.name)

			for j, v := range f.values {
				params.Set("Filter."+n+".Value."+strconv.Itoa(j+1), v)
			}
		}

		if token != "" {
			params.Set("NextToken", token)
		}

		var out describeInstancesResponse

		if err := p.api.call(ctx, params, &out); err != nil {
			return nil, fmt.Errorf("ec2: list billet instances: %w", err)
		}

		for _, r := range out.Reservations {
			for _, item := range r.Instances {
				name, ok := item.tag(nameTag)

				// AN INCOMPLETE INVENTORY IS NOT A SHORTER ONE, and the whole
				// function fails rather than omitting a row.
				//
				// This list is what the control plane reconciles against, and it
				// frees quarantined capacity for any lease ABSENT from it. So a
				// silently dropped instance is capacity handed back for a machine
				// that is still running — the exact failure the inventory exists to
				// prevent, caused by the report meant to prevent it.
				//
				// An owner-tagged instance with no name is billet's compute that
				// cannot be matched to a lease, and one with no id is compute
				// nothing can destroy. Both need an operator, and the docker
				// backend fails its own List for the same reason.
				if !ok || item.InstanceID == "" {
					return nil, fmt.Errorf(
						"ec2: instance %q carries this deployment's owner tag but no usable "+
							"%s tag, so billet cannot match it to a lease or account for it; "+
							"refusing to report an inventory it is missing from",
						item.InstanceID, nameTag)
				}

				instances = append(instances, &provider.Instance{
					ID:      item.InstanceID,
					Name:    name,
					Running: runningState(item.State.Name),
				})
			}
		}

		// PAGINATION IS NOT OPTIONAL HERE. These results feed reconciliation and
		// teardown, so a truncated list reads as "that lease is not running on this
		// node" — which frees capacity for an instance that is still executing a
		// job, and destroys nothing, so nobody finds out until the bill.
		if out.NextToken == "" {
			return instances, nil
		}

		if _, repeated := seen[out.NextToken]; repeated {
			return nil, fmt.Errorf("ec2: the api returned a pagination token it had already "+
				"given; refusing to loop (%d instances so far)", len(instances))
		}

		seen[out.NextToken] = struct{}{}
		token = out.NextToken
	}
}

// runningState reports whether an instance state means the job may still be
// executing.
//
// UNRECOGNISED COUNTS AS RUNNING, the same asymmetry the container backend uses
// and for the same reason: the caller destroys what is not running, and a state
// billet has never heard of is not evidence that a job is over.
//
// `stopped` is the one that looks alive and is not. billet never stops an
// instance, so a stopped one was stopped by somebody else and will not resume on
// its own — adopting it holds a lease open forever for a job that cannot finish.
func runningState(state string) bool {
	switch state {
	case "stopped", "terminated":
		return false
	default:
		return true
	}
}

// preflightOwner tags nothing. It is the deployment identity CheckReachable
// filters on, chosen so the query matches no instances: the point of the call is
// the CALL, not its result.
const preflightOwner = "billet-preflight"

// CheckReachable proves a set of credentials can reach the EC2 API in a region.
//
// WHAT IT PROVES, EXACTLY: that credentials resolve, that the region and endpoint
// name something that answers, and that this identity is permitted to call
// DescribeInstances. It issues one read-only request whose filter deliberately
// matches nothing.
//
// WHAT IT DOES NOT PROVE: permission to RunInstances or TerminateInstances, that
// the subnet and security groups exist, or that the AMI is visible. Those need
// either a dry-run launch, which is a write-shaped call billet should not make
// from a diagnostic, or more permissions than a check ought to require. An
// operator is told the difference rather than left to infer it.
func CheckReachable(ctx context.Context, cfg config.EC2Config, opts ...Option) error {
	p, err := New(preflightOwner, cfg, opts...)
	if err != nil {
		return err
	}

	if _, err := p.List(ctx); err != nil {
		return err
	}

	return nil
}
