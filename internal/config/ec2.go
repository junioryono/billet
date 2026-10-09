package config

import (
	"errors"
	"fmt"
	"net/url"
	pathpkg "path"
	"strconv"
	"strings"
)

// EC2Config configures the cloud backend: one instance per job, in one subnet.
//
// EVERY FIELD HERE IS A PLACEMENT DECISION SOMEBODY HAS TO MAKE, and none of the
// load-bearing ones is defaulted. billet cannot pick a subnet, and a wrong guess
// is either a job that cannot reach GitHub or one that can reach a production
// database.
type EC2Config struct {
	// Region is which AWS region to launch in. It also selects the API endpoint,
	// so an ordinary install configures no host of its own.
	Region string `yaml:"region"`
	// Endpoint overrides the API endpoint billet derives from Region — for a VPC
	// interface endpoint, a non-commercial partition, or a test.
	Endpoint string `yaml:"endpoint,omitempty"`

	// SubnetID is where instances are launched. Its route to GitHub is the
	// operator's to arrange: a private subnet needs a NAT gateway, a public one
	// needs AssignPublicIP.
	SubnetID string `yaml:"subnet_id"`
	// SecurityGroupIDs apply to trusted work.
	SecurityGroupIDs []string `yaml:"security_group_ids"`
	// UntrustedSecurityGroupIDs apply to fork pull-request work, and their
	// ABSENCE is what refuses it.
	//
	// A whole instance is a real isolation boundary, which is why this backend can
	// run code billet cannot vouch for at all — but that boundary is the KERNEL,
	// not the network. A fork's job in the same security group as everything else
	// reaches whatever that group reaches, which on a subnet somebody already had
	// is usually more than they are picturing. So untrusted work runs only once
	// its network has been described separately, rather than defaulting onto the
	// trusted group because nobody said otherwise.
	UntrustedSecurityGroupIDs []string `yaml:"untrusted_security_group_ids,omitempty"`
	// AssignPublicIP gives instances a public address, for a subnet with no NAT
	// gateway. A runner that cannot reach GitHub registers and then does nothing.
	AssignPublicIP bool `yaml:"assign_public_ip,omitempty"`

	// InstanceProfile is the IAM role TRUSTED instances receive. OPTIONAL, and
	// empty is the right answer unless a job genuinely needs AWS credentials: an
	// instance profile is readable from inside the guest, so it is a credential
	// handed to whatever the job runs.
	//
	// UNTRUSTED WORK NEVER GETS IT, whatever this says. A fork's pull request runs
	// its steps directly on the instance, so it could read the role's temporary
	// credentials out of the metadata service — past the isolation that lets this
	// backend run untrusted work at all.
	InstanceProfile string `yaml:"instance_profile,omitempty"`

	// InstanceTypes are the shapes billet may buy, each DECLARING what it holds,
	// because billet ships no table of EC2 instance types.
	//
	// A table would be out of date within a quarter — AWS adds types continuously
	// — and being out of date here means launching a machine that does not fit a
	// lease the allocator has already escrowed. Declaring them keeps the fleet's
	// cost surface in the operator's own file, which is where a spending decision
	// belongs anyway.
	InstanceTypes []EC2InstanceType `yaml:"instance_types"`

	// Spot buys interruptible capacity.
	//
	// DEFAULTS OFF, which reverses the assumption this backend was filed under.
	// It exists so one `runs-on` label survives the bare-metal host going away,
	// and GitHub does not requeue a job whose runner vanished mid-execution — so a
	// spot reclaim is a FAILED BUILD rather than a retry. Defaulting to spot would
	// make the failover path the unreliable one, which is the opposite of what a
	// failover is for. An operator who would rather have a cheap build that
	// sometimes dies says so here.
	Spot bool `yaml:"spot,omitempty"`
	// InterruptionQueueURL receives EventBridge's EC2 Spot interruption warnings.
	// Required with Spot: without it a reclaim is an unexplained failed build.
	InterruptionQueueURL string `yaml:"interruption_queue_url,omitempty"`
	// NodeName is filled from the effective node identity before the provider is
	// constructed. It is not a second operator-configured identity.
	NodeName string `yaml:"-"`
}

// validateEC2Node reports everything wrong with a cloud node.
func (c *Config) validateEC2Node() []error {
	var errs []error

	// WHAT IT WILL BUY, BECAUSE THERE IS NOTHING TO MEASURE.
	//
	// Every other backend runs jobs on this machine, so an unset contribution
	// means "everything I can detect" and detection answers correctly. An ec2 node
	// calls an API and the compute appears in a region, so detection would report
	// whatever small instance holds this process — and billet would advertise that
	// to GitHub as the capacity of an entire cloud, placing one job and then
	// looking full. That is the failover this backend exists for, silently not
	// working.
	//
	// Required rather than defaulted: billet has no standing to choose how much to
	// buy on somebody's account. Placement charges the declared shape that will be
	// purchased, including a fallback, so these are hard resource budgets rather
	// than estimates made from the smaller tier request.
	if c.Node.MaxVCPU <= 0 {
		errs = append(errs, errors.New(
			"node.max_vcpu is required when provider is ec2: there is no machine to detect it "+
				"from, because the compute this node launches runs in a region rather than on "+
				"this host, and billet will not choose how much to buy on your behalf"))
	}

	if c.Node.MaxMemory <= 0 {
		errs = append(errs, errors.New(
			"node.max_memory is required when provider is ec2: there is no machine to detect it "+
				"from, because the compute this node launches runs in a region rather than on "+
				"this host, and billet will not choose how much to buy on your behalf"))
	}

	if c.Node.EC2 == nil {
		errs = append(errs, errors.New("node.ec2 is required when provider is ec2"))

		return errs
	}

	e := c.Node.EC2

	if err := CheckEC2Region(e.Region); err != nil {
		errs = append(errs, err)
	}

	if strings.TrimSpace(e.SubnetID) == "" {
		errs = append(errs, errors.New("node.ec2.subnet_id is required; billet cannot choose "+
			"which network a runner should be able to reach"))
	}

	if err := CheckEC2Endpoint(e.Endpoint); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, CheckEC2SecurityGroups("security_group_ids", e.SecurityGroupIDs, true)...)
	errs = append(errs, CheckEC2SecurityGroups(
		"untrusted_security_group_ids", e.UntrustedSecurityGroupIDs, false)...)

	errs = append(errs, e.instanceTypeErrors()...)

	if e.Spot && e.InterruptionQueueURL == "" {
		errs = append(errs, errors.New("node.ec2.interruption_queue_url is required when spot is "+
			"enabled, so a reclaim is recorded before the instance disappears"))
	}
	if !e.Spot && e.InterruptionQueueURL != "" {
		errs = append(errs, errors.New("node.ec2.interruption_queue_url is set while spot is off; "+
			"the queue would be consumed for compute this node never buys"))
	}
	if err := CheckSQSQueueURL(e.InterruptionQueueURL, e.Region); err != nil {
		errs = append(errs, err)
	}
	if c.Node.Name != "" {
		e.NodeName = c.Node.Name
	}
	if e.NodeName != "" {
		if err := CheckSQSQueueNode(e.InterruptionQueueURL, e.NodeName); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// normalize trims the values billet later uses verbatim.
//
// THE SAME REASON NODE NAMES ARE NORMALIZED FIRST. Validation trimmed these to
// CHECK them and everything else used the raw string, so `region: "  us-west-2  "`
// passed the shape check and was then signed with its padding — a 403 naming
// nothing. YAML strips whitespace from a plain scalar but keeps it inside quotes,
// so this is reachable from an ordinary-looking file.
func (e *EC2Config) normalize() {
	if e == nil {
		return
	}

	e.Region = strings.TrimSpace(e.Region)
	e.Endpoint = strings.TrimSpace(e.Endpoint)
	e.SubnetID = strings.TrimSpace(e.SubnetID)
	e.InstanceProfile = strings.TrimSpace(e.InstanceProfile)
	e.InterruptionQueueURL = strings.TrimSpace(e.InterruptionQueueURL)
	e.NodeName = trimNodeName(e.NodeName)

	for i := range e.SecurityGroupIDs {
		e.SecurityGroupIDs[i] = strings.TrimSpace(e.SecurityGroupIDs[i])
	}

	for i := range e.UntrustedSecurityGroupIDs {
		e.UntrustedSecurityGroupIDs[i] = strings.TrimSpace(e.UntrustedSecurityGroupIDs[i])
	}

	for i := range e.InstanceTypes {
		e.InstanceTypes[i].Type = strings.TrimSpace(e.InstanceTypes[i].Type)
	}
}

// CheckSQSQueueURL refuses a warning queue that cannot be signed safely.
//
// THE SUFFIX IS SELECTED BY THE REGION, NEVER OFFERED AS A CHOICE. This admitted
// either partition's suffix for every region, so `cn-north-1` accepted
// `sqs.cn-north-1.amazonaws.com` — which is not a host — while `billet init iam`
// derived the queue ARN's partition from the same region and rendered a correct
// `arn:aws-cn:sqs:...`. Everything an operator can see is then right: the policy
// applies, the queue exists, the node starts. Behind it the node signs a
// ReceiveMessage for cn-north-1 and sends it to a name that does not resolve, so
// the two-minute spot warning never arrives, every reclaim becomes an unexplained
// failed job, and the lease stays charged until it expires. The queue host, the
// region billet signs for and the partition billet is authorised in have to be one
// partition, and only the region can decide which.
func CheckSQSQueueURL(raw, region string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return errors.New("node.ec2.interruption_queue_url is not a url billet can dial")
	}
	if u.User != nil {
		return errors.New("node.ec2.interruption_queue_url must not carry a username or password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("node.ec2.interruption_queue_url must not carry a query or fragment")
	}
	if u.Hostname() == "" || u.EscapedPath() == "" || u.EscapedPath() == "/" {
		return errors.New("node.ec2.interruption_queue_url must name a queue path")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return errors.New("node.ec2.interruption_queue_url must use https; only loopback may use http")
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}

	// A PORT IS PART OF WHAT GETS DIALLED, and Hostname() drops it. The SQS client
	// addresses the queue URL's Host, port included, so `sqs.<region>.<suffix>:1`
	// matched the standard host exactly and then connected to port 1. AWS serves this
	// on 443 and the loopback exception above is what lets a test point somewhere
	// else, so an explicit 443 is the only port worth accepting.
	//
	// COMPARED AS A NUMBER, NOT AS TEXT. `:0443` is a decimal 443 that net.LookupPort
	// resolves to 443 and every client dials as 443 (measured), so refusing it on the
	// spelling would take a reachable config away at load — which is a worse failure
	// than the one this check exists for. url.Parse has already refused a port that is
	// not digits, so the only way Atoi fails here is a number too big to be one.
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n != 443 {
			return errors.New("node.ec2.interruption_queue_url must not name a port other " +
				"than 443: the host is what billet dials, and SQS answers nowhere else")
		}
	}

	// ASCII FIRST, THEN FOLDED. strings.ToLower is Unicode-aware and U+0130 folds to
	// an ASCII `i`, so 63 of them are 126 bytes of host that become 63 characters
	// passing every rule below — while the name actually dialled is neither what was
	// measured nor what was checked. An internationalised name reaches DNS as its
	// xn-- form, so that is the form to write here.
	if !isHostname(u.Hostname()) {
		return errors.New("node.ec2.interruption_queue_url's host is not a hostname: every " +
			"hostname label has to begin and end alphanumeric, hold only ASCII letters, " +
			"digits and hyphens, and be 63 characters at most, with 253 for the whole " +
			"name. A name outside that resolves nowhere, so the queue would be " +
			"unreachable rather than wrong — write an internationalised name in its " +
			"xn-- form, which is what goes on the wire in any case")
	}

	host := strings.ToLower(u.Hostname())
	suffix := AWSDNSSuffix(region)
	standard := host == "sqs."+region+"."+suffix || host == region+".queue."+suffix

	// THE VPC-ENDPOINT FORM IS MATCHED BY SUFFIX, because the labels in front are the
	// operator's endpoint id and billet cannot know them. What sits there needs no
	// separate check: isHostname has already held the whole name to valid labels, and
	// this cuts on a dot boundary, so a match leaves one or more of them. It does not
	// require AWS's `vpce-` spelling — the DNS suffixes were measured, and an
	// endpoint's own label cannot be without owning an endpoint.
	_, private := strings.CutSuffix(host, ".sqs."+region+".vpce."+suffix)

	if !standard && !private {
		return fmt.Errorf("node.ec2.interruption_queue_url must name an SQS endpoint in "+
			"node.ec2.region %q: sqs.%s.%s, the legacy %s.queue.%s, or "+
			"<endpoint>.sqs.%s.vpce.%s; the other partition's suffix names no host in "+
			"this region, and billet signs every queue request for this region",
			region, region, suffix, region, suffix, region, suffix)
	}

	return nil
}

// CheckSQSQueueNode makes the one-queue-per-node topology enforceable from
// independently deployed node configs: distinct node names imply distinct queue
// URLs rather than relying on an operator remembering not to share one.
func CheckSQSQueueNode(raw, node string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || pathpkg.Base(u.Path) != node {
		return fmt.Errorf("node.ec2.interruption_queue_url must name a queue whose name is exactly the effective node name %q", node)
	}

	return nil
}

// CheckEC2SecurityGroups refuses a list billet cannot safely launch against.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckEC2Endpoint and for the same
// reason: the provider's constructor is exported and cannot assume its
// configuration came through Load.
//
// BOTH HALVES OF THE RULE LIVE HERE. An earlier version exported only the
// blank-entry half, so the constructor accepted a config with NO trusted group at
// all — and RunInstances without a group lets EC2 pick the VPC's default, which in
// a VPC somebody already had usually permits a good deal more than they are
// picturing. A rule split across two places has an entry point that does not
// enforce it, which is the thing exporting it was meant to fix.
//
// `required` is false for the untrusted list, where EMPTY IS MEANINGFUL: its
// absence is what refuses fork pull-request work.
func CheckEC2SecurityGroups(field string, groups []string, required bool) []error {
	var errs []error

	if required && len(groups) == 0 {
		errs = append(errs, fmt.Errorf("node.ec2.%s needs at least one group; it is what decides "+
			"what a running job can reach, so billet will not fall back to the VPC's default",
			field))
	}

	// EVERY BLANK, not the first: an operator fixing one and re-running to find
	// the next is the failure mode Validate exists to avoid.
	for i, g := range groups {
		if strings.TrimSpace(g) == "" {
			errs = append(errs, fmt.Errorf(
				"node.ec2.%s[%d] is empty; an empty string is not a security group, and on the "+
					"untrusted list a non-empty list is what admits fork pull-request work",
				field, i))
		}
	}

	return errs
}

// CheckEC2Region refuses a region that is not one.
//
// EXPORTED, because a region is not only an address: it is interpolated into the
// DEFAULT ENDPOINT HOST, and it is part of the scope every request is signed with.
// The first of those is why the provider's constructor re-applies it — measured,
// a region of `x@attacker.example/?` produces a default endpoint whose host is
// `attacker.example`, and the signed request and its session token go there.
//
// A SHAPE RATHER THAN A LIST. An allowlist goes stale the next time AWS opens a
// region, and being stale means refusing a config that is correct. The shape
// catches the mistake people make, which is dropping the hyphens, and still
// admits partitions billet has never run in.
func CheckEC2Region(region string) error {
	region = strings.TrimSpace(region)

	if region == "" {
		return errors.New("node.ec2.region is required")
	}

	if !awsRegionRe.MatchString(region) {
		return fmt.Errorf(
			"node.ec2.region %q does not look like an aws region (expected something like "+
				"us-west-2); it is signed into every request and interpolated into the default "+
				"endpoint, so an endpoint override cannot compensate for a typo here", region)
	}

	return nil
}

// CheckEC2Endpoint refuses an endpoint that would carry a credential in the clear
// or send a signed request somewhere billet did not mean.
//
// NOTHING HERE RENDERS THE ENDPOINT, and that is the rule rather than an
// oversight. Every attempt to render it safely was wrong in a new way:
// interpolating it printed a password; wrapping url.Parse's error printed one too,
// because *url.Error embeds the whole URL; and url.Redacted masks only a
// HIERARCHICAL url's password, so it leaves an opaque one
// (`http:alice:secret@host`) and any `?token=` query completely intact. Both
// measured. Naming the field and the failed component tells an operator
// everything they can act on and cannot leak anything.
//
// LOOPBACK IS THE EXCEPTION to https, and it is billet's existing rule rather than
// a new one: a loopback wire has no certificates at all, because there the trust
// boundary is the machine itself.
func CheckEC2Endpoint(endpoint string) error {
	return checkSignedEndpoint("node.ec2.endpoint", endpoint)
}

// instanceTypeErrors reports everything wrong with the shapes a cloud node may
// buy.
//
// A ZERO IS A FORGOTTEN FIELD, NOT AN UNKNOWN TO BE TOLERATED. These numbers are
// what billet matches an already-escrowed lease against, so a shape that
// understates itself launches a machine smaller than the work reserved for it and
// over-commits a host nobody can inspect.
func (e *EC2Config) instanceTypeErrors() []error {
	errs := CheckRemoteShapes(ProviderEC2, e.InstanceTypes)
	for i := range e.InstanceTypes {
		if e.InstanceTypes[i].PriceUSDPerHour <= 0 {
			errs = append(errs, fmt.Errorf("node.ec2.instance_types[%d]: "+
				"price_usd_per_hour must be more than zero", i))
		}
	}

	return errs
}
