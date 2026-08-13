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
// required rather than detected. They are NOT a spending limit, though it is easy
// to read them as one: the allocator charges a job the size its tier asked for
// while this backend buys the first declared shape that FITS, so shapes larger
// than their tiers multiply the real spend (#47).
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
	"reflect"
	"regexp"
	"slices"
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

	// images caches what one DescribeImages lookup says about an AMI: its root
	// device, and every non-root EBS mapping billet will restate at launch. Asked
	// once per image because it cannot change under a given AMI id.
	mu     sync.Mutex
	images map[string]imageLayout
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

	// NORMALIZED ONCE, BEFORE ANYTHING READS IT, which config.applyDefaults does
	// for a config that came through Load and nothing does for one that did not.
	// Validating a trimmed copy and then SIGNING with the original is the exact
	// shape of the bug this constructor exists to prevent: ` us-west-2 ` passed
	// the region rule, dialled the right host, and put the spaces in the credential
	// scope of every request, which AWS answers with a 403 naming nothing.
	cfg.Region = strings.TrimSpace(cfg.Region)
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)

	// THE SAFETY RULES CONFIG VALIDATION APPLIES TO THIS BACKEND'S NETWORK AND
	// IDENTITY ARE RE-APPLIED HERE — region, endpoint and security groups. This
	// constructor is exported, so it cannot assume its configuration came through
	// config.Load, the same reason alloc.New re-applies its own. It is not every
	// rule Load enforces: the subnet and the instance shapes are still only checked
	// there, because getting them wrong fails a launch loudly rather than sending a
	// credential somewhere.
	//
	// THE REGION IS ONE OF THEM, which is not obvious: it is interpolated into the
	// default endpoint below, so an unvalidated one is a way to choose the HOST a
	// signed request is sent to. Measured: `x@attacker.example/?` yields a url
	// whose host is `attacker.example`.
	//
	// IN A FIXED ORDER, so a config with two problems always reports the same one
	// first. A map here made the message depend on iteration order.
	if err := config.CheckEC2Region(cfg.Region); err != nil {
		return nil, fmt.Errorf("ec2: %w", err)
	}

	for _, check := range []struct {
		field    string
		groups   []string
		required bool
	}{
		{"security_group_ids", cfg.SecurityGroupIDs, true},
		{"untrusted_security_group_ids", cfg.UntrustedSecurityGroupIDs, false},
	} {
		if errs := config.CheckEC2SecurityGroups(check.field, check.groups, check.required); len(errs) > 0 {
			return nil, fmt.Errorf("ec2: %w", errors.Join(errs...))
		}
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpointFor(cfg.Region)
	}

	// THE EFFECTIVE ENDPOINT, whether it was configured or derived. Checking only
	// the configured one left the derived path — the one almost every deployment
	// takes — unchecked.
	if err := config.CheckEC2Endpoint(endpoint); err != nil {
		return nil, fmt.Errorf("ec2: %w", err)
	}

	// CLONED, because the caller keeps the slices otherwise. `NodePolicy.Clone`
	// exists for exactly this: a caller that can widen a security group after
	// construction can move a fork's job onto a privileged network AFTER the
	// validation that was supposed to prevent it.
	cfg.SecurityGroupIDs = slices.Clone(cfg.SecurityGroupIDs)
	cfg.UntrustedSecurityGroupIDs = slices.Clone(cfg.UntrustedSecurityGroupIDs)
	cfg.InstanceTypes = slices.Clone(cfg.InstanceTypes)

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
		images: make(map[string]imageLayout),
	}

	for _, opt := range opts {
		opt(p)
	}

	// AN OPTION MUST NOT BE ABLE TO PRODUCE A PANIC — and this guarded ONE option
	// while claiming the invariant, which is the shape of mistake this repository
	// keeps finding: the rule was stated and then applied to the case that
	// prompted it.
	//
	// WithHTTPClient(nil) reaches a dereference here. WithLogger(nil) survives
	// construction and panics at the first line Launch or Destroy logs, and
	// WithCredentials(nil) at the first signed call — later, further from the
	// cause, and on a path holding leases. billet bans panic outright, because a
	// control plane that panics drops every one of them.
	switch {
	case p.api.http == nil:
		return nil, errors.New("ec2: WithHTTPClient was given no client")

	case p.log == nil:
		return nil, errors.New("ec2: WithLogger was given no logger")

	// A TYPED NIL IS STILL A NIL, and it is the one an interface hides. A plain
	// `== nil` is false for (*IMDSCredentials)(nil) — the interface has a type — so
	// it passed here and dereferenced at the first signed call, which is exactly
	// the later panic this switch exists to prevent.
	case p.api.creds == nil || isNilValue(p.api.creds):
		return nil, errors.New("ec2: WithCredentials was given no credential source")
	}

	// AFTER THE OPTIONS, so a client supplied by a caller is covered too.
	//
	// A REDIRECT MUST NOT CARRY A SIGNED REQUEST SOMEWHERE ELSE. The endpoint is
	// checked for https and then Go's client follows redirects by default — a 307
	// preserves the method and body, and the hop can be plaintext or another host
	// entirely, so everything the endpoint rule prevents happens one response
	// later to a URL nobody validated. Measured before this existed: three signed
	// requests reached the redirect target, one per retry.
	//
	// AWS does not redirect, which is the reason this is a refusal rather than a
	// policy: a redirect from this endpoint is not the API answering.
	client := *p.api.http
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		// THE HOST AND NOTHING ELSE. A redirect target is chosen by whatever
		// answered, so its query and fragment are not billet's to render — the same
		// rule CheckEC2Endpoint follows.
		//
		// AND THE ERROR IS A SENTINEL, because naming the host here is not enough
		// on its own: net/http wraps whatever this returns in a *url.Error, and
		// THAT type renders the whole redirect target including its query. The call
		// boundary recognises the sentinel and replaces the wrapper rather than
		// wrapping it further.
		return fmt.Errorf("%w to host %q", errRedirected, req.URL.Hostname())
	}

	p.api.http = &client

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

	encodedUserData := base64.StdEncoding.EncodeToString([]byte(userData))

	// REFUSED HERE RATHER THAN BY EC2. A tier's command has no bound of its own and
	// the registration is appended to it, so a config that validates can still
	// produce a script the service will always reject.
	//
	// IT DOES NOT SAVE THE REGISTRATION, and an earlier version of this comment
	// claimed it did: the runner mints one before it ever calls Launch, so by here
	// it exists whatever this decides. What a local refusal buys is that NOTHING
	// WAS SENT — so the failure is unambiguous, `destroyStray` has nothing to look
	// for, and the lease is released instead of being held in custody because
	// absence from one Find is not proof nothing started.
	//
	// THE RAW SCRIPT IS WHAT IS MEASURED. EC2 documents the 16 KiB limit against
	// the data before encoding.
	if len(userData) > maxUserData {
		return nil, fmt.Errorf("ec2: the boot script for %s is %d bytes and ec2 accepts %d; "+
			"the tier's command is too long to carry", spec.Name, len(userData), maxUserData)
	}

	params := url.Values{}
	params.Set("Action", "RunInstances")
	params.Set("ImageId", spec.Image)
	params.Set("InstanceType", instanceType.Type)
	params.Set("MinCount", "1")
	params.Set("MaxCount", "1")
	params.Set("UserData", encodedUserData)

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

	// AN INSTANCE PROFILE IS A CREDENTIAL, AND UNTRUSTED WORK DOES NOT GET ONE.
	//
	// The one-hop metadata limit above stops a container inside the job reaching
	// IMDS. It does not stop the job: a workflow step runs directly on the
	// instance, so a fork's pull request could read the role's temporary
	// credentials out of the metadata service and take them away. The isolation
	// this backend offers untrusted work is the kernel and the machine, and an
	// instance profile reaches straight past both.
	//
	// A deployment that genuinely needs untrusted jobs to hold AWS credentials has
	// to say so with something that does not exist yet, which is the right amount
	// of friction for that decision.
	if p.cfg.InstanceProfile != "" && spec.Trust == provider.TrustTrusted {
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

	if err := p.setBlockDevices(ctx, params, spec); err != nil {
		return nil, err
	}

	var out runInstancesResponse

	if err := p.api.call(ctx, params, &out); err != nil {
		// THE SERVICE'S MESSAGE IS DROPPED FOR THIS ONE ACTION, not filtered.
		//
		// RunInstances is the only request that carries a credential: the
		// registration travels as user data. An endpoint that echoes a rejected
		// parameter — AWS is unlikely to, a proxy or a configured endpoint might —
		// would put a live runner registration into an error that goes back through
		// the node's command result and into logs.
		//
		// SUBSTITUTING THE SECRET OUT WAS TRIED FIRST AND IS NOT ENOUGH. What
		// travels on the wire is the base64 script inside a form-encoded body, so a
		// body echoed verbatim contains neither the raw registration nor the raw
		// base64 — it contains `%3D` where the padding was. Any list of encodings
		// is a guess at what an intermediary did, and billet already learned this
		// on the GitHub onboarding code: a secret out of its field is an opaque
		// string, so the fix is to write the message yourself rather than to filter
		// somebody else's.
		//
		// The CODE survives, because it comes from AWS's own fixed enumeration and
		// is the actionable half — InvalidParameterValue, UnauthorizedOperation,
		// InsufficientInstanceCapacity. Every other action keeps its message.
		return nil, fmt.Errorf("ec2: launch %s: %w", spec.Name, withoutMessage(err))
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

// survivesTermination reports whether a mapping SAYS its volume outlives the
// instance.
//
// BOTH SPELLINGS, because XML booleans have two: xs:boolean's lexical space is
// "true"/"false"/"1"/"0". Whether EC2 ever emits the digits is not something this
// package can answer — nothing in it has spoken to a real AWS account — so the
// choice is between handling a form that may never arrive and misreading the
// operator's intent if it does. One comparison, and it fails toward honouring
// what the image said.
//
// SPELLING THEM OUT IS THE COST OF DECODING THIS AS A STRING, which is the part
// worth knowing before someone "simplifies" it. encoding/xml runs a bool field
// through strconv.ParseBool, so a bool would have understood all four lexemes for
// free — but it also decodes an ABSENT element into the zero value, making absent
// and "false" indistinguishable. Those two get opposite treatment now (absent is
// billet's to decide, false is the operator's to keep), so the string decode is
// forced, and this function puts back the leniency that decision threw away.
//
// That much is measured rather than assumed, and it is measurable precisely
// because it is Go's behaviour rather than AWS's:
//
//	<ebs></ebs>                       bool=false  string=""
//	<ebs><d>false</d></ebs>           bool=false  string="false"
//	<ebs><d>0</d></ebs>               bool=false  string="0"
//	<ebs><d>1</d></ebs>               bool=true   string="1"
func survivesTermination(flag string) bool {
	return flag == "false" || flag == "0"
}

// imageDevice is one block device billet will state explicitly at launch.
type imageDevice struct {
	name string
	// keep is the literal DeleteOnTermination billet will send for this device.
	// NEVER EMPTY — that is the entire point of the type.
	keep string
}

// imageLayout is what one DescribeImages lookup tells billet about an AMI.
type imageLayout struct {
	root string
	// devices are the non-root EBS mappings, in the order the image declared
	// them, each carrying the flag billet will send for it.
	devices []imageDevice
}

// setBlockDevices states what happens to EVERY volume the instance launches with.
//
// NOTHING HERE IS LEFT TO A DEFAULT, and that is the whole design. An unstated
// DeleteOnTermination is not a small ambiguity: AWS documents the answer twice,
// and the two answers disagree for exactly the case billet is in — a non-root
// volume, attached at launch, through the API.
//
//   - [2] gives a complete inheritance model — the default is true for the root
//     volume and false for ATTACHED volumes, an AMI inherits the setting from the
//     instance it was made from, and a launch inherits it from the AMI. Read
//     literally, a silent non-root mapping PRESERVES its volume, and billet leaks
//     one disk per job forever.
//   - [1] carries a launch-time defaults table whose "data volume, at launch, CLI"
//     row says Delete. Read literally, the same mapping is harmless.
//
// Reviewers split on whether those genuinely conflict or can be reconciled, which
// is itself the argument for this function: the disagreement was about somebody
// else's undocumented edge, neither reading was measurable from here, and the bill
// for guessing wrong arrives monthly and silently. So billet stops having an
// opinion. Every device it launches carries an explicit flag, and no default —
// documented, contradictory, or discovered later — governs anything billet
// launches. #53.
//
// AN EXPLICIT false IS PASSED THROUGH ON EVERY DEVICE EXCEPT THE ROOT. Where a
// NON-ROOT mapping stated the flag, that is the operator talking about their own
// AMI, and silently deleting a volume somebody deliberately marked to keep would
// destroy whatever the job wrote to it — irreversibly, against a leak that is
// tagged, warned about, and deletable any month you like. When one arm is
// reversible and the other is not, take the reversible one. Those are warned about
// instead, once per image.
//
// THE ROOT IS BILLET'S, AND IS ALWAYS SENT true. That is not an exception carved
// out to be convenient; it is the one device billet already owns. billet chooses
// its size for the tier, it is the disk the runner boots, and it exists only for
// the length of one job — AWS itself only permits size, type and this flag to be
// modified for a root volume, which is very nearly the list of things billet sets.
// An image whose root says "keep" would leave a full OS disk behind for EVERY job
// on the tier, which is the largest leak available here and the least likely to be
// deliberate. billet overrides it and SAYS SO, which is the difference that makes
// the asymmetry honest: billet is silent when it merely fills a gap the image left,
// and speaks whenever it contradicts something the image actually said.
//
// THE DEVICE NAME IS ASKED FOR RATHER THAN ASSUMED, which is what the lookup buys
// beyond this. A block device mapping naming a device that is not the AMI's root
// does not fail: EC2 attaches an ADDITIONAL empty volume, the root stays whatever
// size the image was built at, and the launch reports success. So a tier asking
// for 300GiB would run out of disk mid-job while an unused 300GiB volume sat
// beside it, billed. `/dev/sda1` and `/dev/xvda` are both common and neither is
// safe to guess.
//
// [1]: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/preserving-volumes-on-termination.html
// [2]: https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/block-device-mapping-concepts.html
func (p *Provider) setBlockDevices(ctx context.Context, params url.Values, spec provider.Spec) error {
	layout, err := p.imageLayout(ctx, spec.Image)
	if err != nil {
		return err
	}

	params.Set("BlockDeviceMapping.1.DeviceName", layout.root)
	params.Set("BlockDeviceMapping.1.Ebs.DeleteOnTermination", "true")

	if spec.Disk > 0 {
		// ROUNDED UP. EBS sizes in whole GiB, and rounding down would hand a tier
		// that asked for 80GiB a 79GiB disk — the direction that fails a job rather
		// than costing a fraction of a cent.
		gib := (int64(spec.Disk) + int64(config.GiB) - 1) / int64(config.GiB)

		params.Set("BlockDeviceMapping.1.Ebs.VolumeSize", strconv.FormatInt(gib, 10))
	}

	// FROM 2, because the root took 1, and CONTIGUOUS because that is what every
	// official SDK emits for a query-API list. What EC2 does with a GAP is not
	// documented anywhere billet can point at — so this does not rely on knowing:
	// a dense list is correct under every possible gap semantics, which is the
	// cheaper thing to depend on than a claim about somebody else's parser.
	for i, d := range layout.devices {
		n := strconv.Itoa(i + 2)

		params.Set("BlockDeviceMapping."+n+".DeviceName", d.name)
		params.Set("BlockDeviceMapping."+n+".Ebs.DeleteOnTermination", d.keep)
	}

	return nil
}

// imageLayout reports an AMI's root device and the mappings billet will restate,
// asking once per image.
//
// The warning about volumes the image asks to keep is emitted HERE rather than at
// launch, so it is said once per image like the lookup, instead of once per job.
func (p *Provider) imageLayout(ctx context.Context, image string) (imageLayout, error) {
	p.mu.Lock()
	cached, ok := p.images[image]
	p.mu.Unlock()

	if ok {
		return cached, nil
	}

	params := url.Values{}
	params.Set("Action", "DescribeImages")
	params.Set("ImageId.1", image)

	var out describeImagesResponse

	if err := p.api.call(ctx, params, &out); err != nil {
		return imageLayout{}, fmt.Errorf("ec2: describe image %s to state its block devices: %w",
			image, err)
	}

	if len(out.Images) == 0 {
		return imageLayout{}, fmt.Errorf("ec2: image %s does not exist in %s, or this deployment "+
			"cannot see it", image, p.cfg.Region)
	}

	layout := imageLayout{root: out.Images[0].RootDeviceName}
	if layout.root == "" {
		return imageLayout{}, fmt.Errorf("ec2: image %s reports no root device name, so billet "+
			"cannot size its root volume without attaching a second disk by mistake", image)
	}

	for _, bd := range out.Images[0].BlockDevices {
		// NOT EVERY MAPPING IS AN EBS VOLUME. A mapping with no <ebs> child is an
		// instance-store or suppressed device, and sending Ebs.DeleteOnTermination
		// for one asks EC2 about a volume that does not exist. A device with no name
		// cannot be restated at all.
		if bd.DeviceName == "" || bd.EBS == nil {
			continue
		}

		stated := survivesTermination(bd.EBS.DeleteOnTermination)

		// THE ROOT IS OVERRIDDEN, AND THAT IS THE ONE CASE BILLET ANNOUNCES ITSELF
		// FOR. setBlockDevices always sends true for it, so nothing is collected
		// here — but an image that explicitly asked to keep its root is having a
		// stated intent reversed, and finding that out from a missing volume is
		// worse than reading it once.
		if bd.DeviceName == layout.root {
			if stated {
				p.log.Warn("this image asks to keep its ROOT volume, and billet is overriding "+
					"that to delete: the root is the disk billet sizes and boots for one job, "+
					"and keeping it would leave a full OS disk behind for every job on this tier",
					"image", image, "device", bd.DeviceName)
			}

			continue
		}

		keep := "true"

		if stated {
			// THE IMAGE SAID SO, so billet says it back rather than overruling it.
			keep = "false"

			p.log.Warn("this image attaches a volume that outlives its instance, and billet is "+
				"launching it that way because the image asked; every job on this tier will "+
				"leave one behind, billed until somebody goes looking",
				"image", image, "device", bd.DeviceName)
		}

		layout.devices = append(layout.devices, imageDevice{name: bd.DeviceName, keep: keep})
	}

	p.mu.Lock()
	p.images[image] = layout
	p.mu.Unlock()

	return layout, nil
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
// is a real difference from the container backend where `docker rm --force`
// finishes the job. An instance sits in `shutting-down` for a minute or two
// afterwards, and the listener treats a successful Destroy as its proof that the
// compute is gone before it releases the lease.
//
// SO THE CONTRACT IS NOT MET HERE, AND THE GAP IS AN EXECUTION OVERLAP RATHER
// THAN A ROUNDING ERROR (#46).
//
// An earlier version of this comment argued the purpose was still served, because
// terminate is irreversible and the next launch is a different machine, so the
// only thing briefly exceeded was the BILL. That argument is wrong, and this file
// contradicts it further down: runningState documents `shutting-down`
// as a state where "the job may still be executing", which is exactly why List
// asks for it. A terminate request proves EVENTUAL termination. It does not prove
// the guest has stopped, and an instance takes a minute or two to get there.
//
// The consequence is not money. Destroy is reached on paths where the guest IS
// still working — a drain, a custody teardown, an operator killing a job — and
// the listener releases the lease on success, so another job can start while the
// old guest is still finishing a deploy, a migration, or an artifact upload. Two
// concurrent effects on something outside billet, which is worse than the
// over-commit the rule was written to prevent.
//
// WAITING HERE IS NOT THE FIX EITHER: a node executes one command at a time, so
// blocking teardown for the minute or two AWS takes would stall every launch
// queued behind it. The fix is to keep the capacity charged in a terminating
// state while something confirms quiescence out of band — which is what custody
// already does for compute billet cannot account for, and is a change to shared
// node machinery rather than to this backend. #46 carries it.
//
// Until then the instance does at least stay in this host's inventory, because
// `shutting-down` is one of the states List asks for. That does not un-release
// the lease, which is already terminal; it means the sweep sees the instance,
// calls Destroy again, and finds it idempotent.
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

	// "REQUESTED", because that is what happened. Logging "terminated" here is the
	// same conflation this function's comment exists to correct — the machine is
	// still running for a minute or two afterwards.
	p.log.Info("requested instance termination", "instance", id)

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
				name, _ := item.tag(nameTag)

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
				// A MISSING ID IS ITS OWN DIAGNOSIS. Blaming the Name tag for it
				// sends an operator to the wrong field.
				if item.InstanceID == "" {
					return nil, fmt.Errorf(
						"ec2: the api described an instance with this deployment's owner tag "+
							"(%s=%s) and no instance id, which billet can neither account for "+
							"nor destroy; refusing to report an inventory it is missing from",
						ownerTag, p.owner)
				}

				// AND A NAME THAT CANNOT IDENTIFY A LEASE IS NO BETTER THAN AN
				// ABSENT ONE. Asking only whether the tag EXISTED let `<value/>`
				// through as a name of "", and a name billet never assigned through
				// as one it cannot map — both landing in the inventory as though
				// they were accounted for.
				//
				// THE MESSAGE HAS TO BE ACTIONABLE, because failing closed stops
				// this node's sweep until somebody intervenes. That is the right
				// direction — the alternative sells a running machine's capacity
				// twice, silently — but only if an operator is told which instance
				// and what to do about it. Both remedies are named, and either takes
				// a minute.
				// A WELL-FORMED NAME NAMING A LEASE THAT NO LONGER EXISTS IS
				// DELIBERATELY ACCEPTED. That is an ORPHAN, which is a state the
				// callers already handle — Sweep destroys it and releases nothing,
				// because there is nothing to release. What this refuses is a name
				// that identifies no lease AT ALL, which is unattributable rather
				// than merely stale.
				if _, ours := provider.LeaseOf(name); !ours {
					return nil, fmt.Errorf(
						"ec2: instance %s carries this deployment's owner tag (%s=%s) but its "+
							"%s tag is %q, which names no lease, so billet cannot account for "+
							"it and will not report an inventory it is missing from: either "+
							"terminate that instance or restore its %s tag to billet-<lease-id>",
						item.InstanceID, ownerTag, p.owner, nameTag, name, nameTag)
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

// defaultEndpointFor derives the regional endpoint when none was configured.
//
// THE SUFFIX IS NOT THE SAME IN EVERY PARTITION. The region rule deliberately
// admits partitions billet has never run in, so it accepts `cn-north-1` — and the
// commercial suffix would then derive `ec2.cn-north-1.amazonaws.com`, a host that
// does not exist. AWS China is reached at `amazonaws.com.cn`. GovCloud is not a
// special case: it uses the commercial suffix.
//
// A config can always override this outright, and one in a partition billet has
// not been taught about should.
func defaultEndpointFor(region string) string {
	suffix := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		suffix = "amazonaws.com.cn"
	}

	return "https://ec2." + region + "." + suffix + "/"
}

// maxUserData is what EC2 accepts, measured before base64 encoding.
const maxUserData = 16 * 1024

// withoutMessage strips the service's own prose from an API error, keeping its
// code.
//
// For the one action that sends a credential. The code is from AWS's fixed
// enumeration and is what an operator acts on; the message is free text billet
// did not write, on a request whose body contains a live runner registration.
func withoutMessage(err error) error {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return err
	}

	// AND THE CODE IS CHECKED RATHER THAN TRUSTED. "AWS's fixed enumeration" is an
	// assumption about somebody else's response, and the endpoint is configurable —
	// so a reply that puts the echoed request body in <Code> instead of <Message>
	// would walk straight through a function whose whole job is to stop that. A
	// real code is a short identifier; anything else is not one, whatever it is.
	code := apiErr.Code
	if !awsErrorCode.MatchString(code) {
		code = "(unrecognised error code)"
	}

	return &apiError{Code: code, Status: apiErr.Status,
		Message: "(message withheld: this request carries a runner registration)"}
}

// awsErrorCode is the shape of an EC2 error code — a short identifier, never
// prose and never a payload.
var awsErrorCode = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,63}$`)

// errRedirected marks a refusal to follow a redirect, so the api boundary can
// discard net/http's *url.Error wrapper — which renders the whole redirect
// target, query included — instead of passing it on.
var errRedirected = errors.New("ec2: refusing to follow a redirect: a signed request carries a " +
	"session token, and the ec2 api does not redirect")

// isNilValue reports whether an interface holds a nil pointer, map, slice, func
// or channel — the values that satisfy an interface and then panic on use.
func isNilValue(v any) bool {
	rv := reflect.ValueOf(v)

	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func,
		reflect.Chan, reflect.UnsafePointer, reflect.Interface:
		return rv.IsNil()
	default:
		return false
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
