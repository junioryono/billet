package initconfig

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// THE HYBRID SHAPE, GENERATED AS ONE UNIT. The README leads with it -- a small
// EC2 controller with a retained ledger volume, a Firecracker host at home
// dialling it over mTLS, an EC2 orchestrator beside the controller, and tiers
// that fill home first and the cloud second under one label -- and until this
// existed it was assembled by hand from three READMEs. The pieces that have to
// agree were spread across them: the controller's address in server.listen and
// every node's server_addr, the ledger volume id into the role, the backup
// bucket into backup.s3, the subnet and security groups into node.ec2, the AMI
// into every tier's launch.ec2, and the certificate step between commissioning
// and the first node converge. Each is one place to get wrong in a way that
// surfaces a converge later.
//
// THREE RENDERS OF ONE GENERATION, because the facts arrive in three waves.
// Before `terraform apply` nothing has an id, so the PLAN render carries
// placeholders that name the output that will fill them. After the apply the
// PREPARE render fills them from `terraform output -json`, and the controller
// entry stays server-only and prepare-only: the co-located ec2 node's config
// names a certificate bundle that exists only after `billet ca issue`, and the
// host role's `billet check` refuses a missing one (measured on a real
// deployment). The COMMISSION render adds the node, the AMI, and lifts the
// hold.

// HybridMarker is the first-line marker every generated file carries, in its
// own comment syntax, so a re-run can tell billet's own file from one the
// operator wrote and replace only the former.
const HybridMarker = "written by billet init hybrid"

// The files a generation writes, relative to the output directory. The runbook
// is rendered by the CLI, which knows the flags it has to repeat.
const (
	HybridTerraformFile    = "terraform/main.tf"
	HybridInventoryFile    = "inventory.yml"
	HybridSiteFile         = "site.yml"
	HybridRequirementsFile = "requirements.yml"
)

// The Terraform outputs the inventory consumes, by the exact name the generated
// root declares them under. A placeholder names one of these; ParseTerraformOutput
// demands them; the structural test proves the rendered root declares each.
const (
	HybridOutputControlPlanePrivateIP = "control_plane_private_ip"
	HybridOutputLedgerVolumeID        = "ledger_volume_id"
	HybridOutputSubnetID              = "subnet_id"
	HybridOutputRunnerSecurityGroup   = "runner_security_group_id"
	HybridOutputUntrustedRunnerSG     = "untrusted_runner_security_group_id"
	HybridOutputAMIPayloadBucket      = "ami_payload_bucket"
	HybridOutputCacheBucket           = "cache_bucket"
	HybridOutputCachePrefix           = "cache_prefix"
	HybridOutputAvailabilityZone      = "availability_zone"
)

// HybridPlaceholder is the text standing in for an apply-time fact in a plan
// render. It names the output that fills it, so an operator who meets one in a
// file knows exactly which command produces the value.
func HybridPlaceholder(output string) string {
	return "<terraform output " + output + ">"
}

// hybridPlaceholderPattern finds every placeholder in a rendered file.
var hybridPlaceholderPattern = regexp.MustCompile(`<terraform output ([a-z_]+)>`)

// HybridPlaceholders reports the output names every placeholder in text names,
// in order of appearance, so a test can prove each one is declared and a
// filled render carries none.
func HybridPlaceholders(text string) []string {
	matches := hybridPlaceholderPattern.FindAllStringSubmatch(text, -1)

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}

	return out
}

// HybridFacts are the apply-time values a filled render carries. The zero
// value means "not applied yet", and every empty field renders its placeholder.
type HybridFacts struct {
	ControlPlanePrivateIP          string
	LedgerVolumeID                 string
	SubnetID                       string
	RunnerSecurityGroupID          string
	UntrustedRunnerSecurityGroupID string
	AMIPayloadBucket               string
	// The cache facts, demanded only of a generation that asked for one.
	CacheBucket      string
	CachePrefix      string
	AvailabilityZone string
}

// HybridParams is everything `billet init hybrid` decided or was told.
type HybridParams struct {
	// Name is the Terraform module's name prefix: every AWS resource, the backup
	// bucket and the payload bucket derive from it.
	Name string
	// Region is where the controller and the fallback compute live.
	Region string
	// Org and the trusted-pool policy, exactly as for a single-host generation.
	// No policy renders `trust: untrusted` tiers, which the ec2 side serves from
	// an untrusted security group and the firecracker side from the untrusted
	// bridge.
	Org         string
	RunnerGroup string
	Workflows   []string

	// ControlPlaneIP is the controller's private address, DECLARED. Empty leaves
	// it to AWS and renders a placeholder that the prepare render fills; a
	// declared one is written into server.listen and every server_addr before
	// the apply, which is the point of declaring it.
	ControlPlaneIP string

	// ControllerName and LocalName are the two hosts' names in the inventory
	// and in their certificates. The controller's is also the co-located ec2
	// node's name.
	ControllerName string
	LocalName      string

	// LocalVCPU and LocalMemory are what the Firecracker host HAS; its
	// contribution is that minus headroom, the same rule a measured host gets.
	LocalVCPU   int
	LocalMemory config.ByteSize

	// CloudVCPU and CloudMemory are the cloud budget, which IS the orchestrator's
	// ceiling: there is no machine to withhold headroom from.
	CloudVCPU   int
	CloudMemory config.ByteSize

	// Shapes are the EC2 instance types billet may buy, each carrying what it
	// holds and its audited price.
	Shapes []config.EC2InstanceType

	// SSHIngressCIDRs open the controller's SSH port to the machine that runs
	// Ansible; empty opens nothing, which is right for a route that ends on the
	// controller itself (the Systems Manager agent, a cloudflared tunnel or
	// route running on the host). IPv4 and canonical, because the root's rule
	// is cidr_ipv4 and AWS normalises host bits into a permanent diff.
	SSHIngressCIDRs []string
	// SSHKeyName is the EC2 key pair the controller launches with. Empty
	// attaches none, and then the only way onto a fresh Canonical image is EC2
	// Instance Connect, which the runbook spells out; with a key, Ansible's
	// ordinary SSH works the moment the instance answers.
	SSHKeyName string
	// LocalAnsibleUser is the account Ansible connects to the Firecracker host
	// as. Empty writes none, leaving the operator's SSH configuration to say:
	// owned hardware has no reason to carry the cloud image's `ubuntu`.
	LocalAnsibleUser string
	// LocalImage is the guest generation every tier boots on the Firecracker
	// host. Empty is DefaultFirecrackerImage, the x64 generation billet
	// publishes; an operator with another architecture or their own signed
	// generation names it, because the generator cannot see that machine.
	LocalImage string

	// Cache turns on the EBS+S3 site cache for the cloud half: the module
	// creates the bucket, the orchestrator gains node.ebs_s3 and the node.cache
	// listener its job instances fetch through, and both hosts declare the site
	// their storage belongs to.
	//
	// THE TWO HALVES CACHE IN DIFFERENT PLACES, which is what makes the sites
	// necessary rather than decorative: the Firecracker host's generations live
	// in its own Ceph pools, and an EC2 job cannot reach them across the WAN, so
	// the cloud half needs a store of its own and cache keys are scoped by site.
	Cache bool
	// LocalSite and CloudSite name those two places. Empty defaults to "home"
	// and to the region, which is what an operator would write anyway.
	LocalSite string
	CloudSite string

	// Builder grants the controller's own role what `billet ami build`
	// performs, so the image can be built ON the controller instead of from a
	// workstation holding an operator's AWS credentials — a second machine to
	// keep trustworthy for one step, on a deployment whose controller may be
	// reachable only through a tunnel. Off by default: it widens the identity
	// every job's instance is launched by.
	Builder bool

	// Host carries the Firecracker host's inputs billet cannot detect.
	Host HostInputs

	// Ref is the release every layer pins: the module's ?ref=, the collection's
	// version and billet_version. `main` for a development build.
	Ref string

	// Facts fill the placeholders; nil is the plan render.
	Facts *HybridFacts
	// Commission adds the ec2 node to the controller and lifts the prepare-only
	// hold. It needs Facts.
	Commission bool
	// AMI is what every tier's launch.ec2.image becomes on the commission
	// render; empty writes PlaceholderAMI, which loads and fails at launch by
	// naming what to do.
	AMI string

	// The App identity carried from an existing config; zero leaves the ids for
	// `billet github-app create`.
	AppID          int64
	InstallationID int64
	ClientID       string
}

// HybridFiles is the generation: relative path to content.
type HybridFiles map[string]string

// Hybrid conventions shared by the renderers and the runbook.
const (
	hybridListenPort = 7717
	hybridTLSDir     = "/etc/billet/tls"
	// hybridRunnerCommand is what `billet ami build` installs as the runner
	// entrypoint on the image; an ec2 launch entry names it because the AMI's
	// default is not a runner.
	hybridRunnerCommand = "/usr/local/bin/billet-runner"
	// hybridDiskPerVCPU and hybridDiskFloor size each tier's root: the hybrid
	// guide's 160GiB for 8 vCPU, never below the 80GiB a 2 vCPU guest wants.
	hybridDiskPerVCPU = 20 * config.GiB
	hybridDiskFloor   = 80 * config.GiB
	// hybridBackupPrefix is the root module's default, restated so the config
	// and the grant name the same literal.
	hybridBackupPrefix = "billet-backups"
	// hybridCachePort is the port the module's cache rule opens from the runner
	// group to the control plane, and therefore the one the orchestrator's cache
	// listener binds. It is the root's cache_listen_port default; the two are one
	// number in two files, and a mismatch is a listener nothing may reach.
	hybridCachePort = 9443
	// Where the role installs the cache's TLS pair. The paths are the config's to
	// choose — the role copies billet_cache_tls_cert_src to whatever
	// node.cache.tls_cert names — and they must be absolute, because an EC2
	// listener's are.
	hybridCacheTLSCert = "/etc/billet/cache-tls/cert.pem"
	hybridCacheTLSKey  = "/etc/billet/cache-tls/key.pem"
	// HybridDefaultLocalSite is what the machine at home is called when the
	// operator does not say. A site is a PLACE, so the default names one.
	// Exported because the CLI prints it in the flag's help.
	HybridDefaultLocalSite = "home"
)

var (
	hybridNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	hybridHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// GenerateHybrid renders the files and reports whether the tiers are trusted.
//
// Every refusal here names the flag that carried the value, for the reason
// Generate's do: a value billet cannot turn into a runnable deployment must
// not surface later as a config-load error blaming a file billet wrote.
func GenerateHybrid(p HybridParams) (HybridFiles, bool, error) {
	if err := checkHybridParams(&p); err != nil {
		return nil, false, err
	}

	policy := Params{RunnerGroup: strings.TrimSpace(p.RunnerGroup)}
	for _, w := range p.Workflows {
		policy.Workflows = append(policy.Workflows, strings.TrimSpace(w))
	}
	if p.RunnerGroup != "" && policy.RunnerGroup == "" {
		return nil, false, errors.New("--runner-group: a runner group cannot be only whitespace")
	}

	trusted, err := policy.trusted()
	if err != nil {
		return nil, false, err
	}
	p.RunnerGroup, p.Workflows = policy.RunnerGroup, policy.Workflows

	localVCPU, localMemory := CeilingVCPU(p.LocalVCPU), CeilingMemory(p.LocalMemory)
	if localVCPU == 0 || localMemory == 0 {
		return nil, false, fmt.Errorf("--local-vcpu %d and --local-memory %s leave nothing for "+
			"billet once the host keeps its headroom; a Firecracker host needs more than that",
			p.LocalVCPU, p.LocalMemory)
	}

	catalogue := hybridTiers(p.Shapes, p.CloudVCPU, p.CloudMemory, localVCPU, localMemory)
	if len(catalogue) == 0 {
		return nil, false, fmt.Errorf("no declared shape fits both the cloud budget of %d vCPU and %s "+
			"and the local host's ceiling of %d vCPU and %s, so no tier could be placed on "+
			"either node — raise the budget, name a smaller --instance-type, or give the local "+
			"host more", p.CloudVCPU, p.CloudMemory, localVCPU, localMemory)
	}

	facts := hybridFactValues(p)
	wire := fmt.Sprintf("%s:%d", facts[HybridOutputControlPlanePrivateIP], hybridListenPort)

	ami := p.AMI
	if ami == "" {
		ami = PlaceholderAMI
	}

	tiers := renderHybridTiers(catalogue, p, trusted, ami)
	controller := renderHybridController(p, trusted, wire, facts, localVCPU, localMemory)
	local := renderHybridLocal(p, trusted, wire, localVCPU, localMemory)

	// PROVE BOTH LOAD before returning them, as Generate does. The anchor the
	// inventory uses is expanded here, and the one placeholder config cannot
	// carry as-is -- an address inside a host:port -- is stood in for, because
	// the point is the shape of what was rendered rather than the value AWS
	// has not chosen yet.
	for name, body := range map[string]string{
		"the controller's config": controller + "\ntiers:\n" + tiers,
		"the local host's config": local + "\ntiers:\n" + tiers,
	} {
		validation := strings.ReplaceAll(body, HybridPlaceholder(HybridOutputControlPlanePrivateIP), "192.0.2.10")
		validation = strings.Replace(validation, "app_id: 0\n", "app_id: 1\n", 1)
		validation = strings.Replace(validation, "installation_id: 0\n", "installation_id: 1\n", 1)
		if _, err := config.Parse(name, []byte(validation)); err != nil {
			return nil, false, fmt.Errorf("initconfig: %s billet generated is not valid: %w", name, err)
		}
	}

	files := HybridFiles{
		HybridTerraformFile:    renderHybridTerraform(p, trusted),
		HybridInventoryFile:    renderHybridInventory(p, trusted, facts, tiers, controller, local),
		HybridSiteFile:         renderHybridSite(p),
		HybridRequirementsFile: renderHybridRequirements(p),
	}

	return files, trusted, nil
}

// checkHybridParams refuses what cannot become a deployment, by flag name, and
// normalizes what something else will use.
func checkHybridParams(p *HybridParams) error {
	p.Name = strings.TrimSpace(p.Name)
	if !hybridNamePattern.MatchString(p.Name) {
		return fmt.Errorf("--name %q must be 2-31 lowercase letters, digits or hyphens starting "+
			"with a letter: it is the Terraform module's name and prefixes every AWS resource", p.Name)
	}

	if p.Region == "" {
		return errors.New("--region is required: billet cannot choose which region the " +
			"controller and the fallback compute live in")
	}
	if err := config.CheckEC2Region(p.Region); err != nil {
		return fmt.Errorf("--region: %w", err)
	}

	if p.Org != "" {
		if err := config.CheckOrg(p.Org); err != nil {
			return fmt.Errorf("--org: %w", err)
		}
	}

	// THE NAME IS WHAT billet ca issue MINTS, so config's own node-name rule is
	// applied here, before the runbook's step 5 would meet it, and the DNS-label
	// shape on top of it because the same string is an inventory hostname.
	for flag, name := range map[string]string{
		"--controller-name": p.ControllerName,
		"--local-name":      p.LocalName,
	} {
		if err := config.ValidateNodeName(flag, name); err != nil {
			return fmt.Errorf("%s: %w", flag, err)
		}
		if !hybridHostPattern.MatchString(name) || len(name) > 63 {
			return fmt.Errorf("%s %q must be a lowercase DNS label (letters, digits, hyphens, at most "+
				"63 characters): it is the host's inventory name and the name in its certificate",
				flag, name)
		}
	}
	if p.ControllerName == p.LocalName {
		return fmt.Errorf("--controller-name and --local-name are both %q; two hosts need two "+
			"names, because the control plane authorises a node by the name in its certificate",
			p.ControllerName)
	}

	if p.ControlPlaneIP != "" {
		if ip := net.ParseIP(p.ControlPlaneIP); ip == nil || ip.To4() == nil {
			return fmt.Errorf("--control-plane-private-ip %q is not an IPv4 address", p.ControlPlaneIP)
		}
	}

	for _, c := range p.SSHIngressCIDRs {
		ip, network, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("--ssh-ingress-cidr %q is not a CIDR: %w", c, err)
		}
		if ip.To4() == nil {
			return fmt.Errorf("--ssh-ingress-cidr %q is not IPv4; the controller's SSH rule is an "+
				"IPv4 rule, and this would fail at plan rather than here", c)
		}
		if network.String() != c {
			return fmt.Errorf("--ssh-ingress-cidr %q has host bits set; write it as %s, because AWS "+
				"normalises the rule and the plan would then diff forever", c, network)
		}
	}

	if p.SSHKeyName != "" && strings.TrimSpace(p.SSHKeyName) != p.SSHKeyName {
		return fmt.Errorf("--key-name %q carries padding; it names an EC2 key pair exactly", p.SSHKeyName)
	}
	if p.LocalAnsibleUser != "" && !hybridHostPattern.MatchString(p.LocalAnsibleUser) {
		return fmt.Errorf("--local-ansible-user %q must be a plain account name (lowercase letters, "+
			"digits, hyphens)", p.LocalAnsibleUser)
	}
	if p.LocalImage == "" {
		p.LocalImage = DefaultFirecrackerImage
	}

	// THE TWO PLACES, defaulted rather than demanded: an operator who turns the
	// cache on should not also have to invent two names. They are IDENTITIES —
	// a node reports its site and the control plane matches it exactly — so
	// padding is refused rather than trimmed, the same rule config applies.
	if p.LocalSite == "" {
		p.LocalSite = HybridDefaultLocalSite
	}
	if p.CloudSite == "" {
		p.CloudSite = p.Region
	}
	for flag, name := range map[string]string{
		"--local-site": p.LocalSite,
		"--cloud-site": p.CloudSite,
	} {
		if !hybridHostPattern.MatchString(name) {
			return fmt.Errorf("%s %q must be a lowercase name of letters, digits and hyphens: a "+
				"node reports its site and the control plane matches it exactly", flag, name)
		}
	}
	if p.LocalSite == p.CloudSite {
		return fmt.Errorf("--local-site and --cloud-site are both %q; two places need two names, "+
			"because a cache key is scoped by site and the two stores are not the same storage",
			p.LocalSite)
	}
	if idx := strings.IndexFunc(p.LocalImage, badImageRune); idx >= 0 {
		return fmt.Errorf("--local-image: %q contains whitespace or a control character", p.LocalImage)
	}

	if p.LocalVCPU <= 0 || p.LocalMemory <= 0 {
		return errors.New("--local-vcpu and --local-memory are required and positive: they " +
			"describe the Firecracker host, which is not the machine running this")
	}
	if p.CloudVCPU <= 0 || p.CloudMemory <= 0 {
		return errors.New("--max-vcpu and --max-memory are required and positive: they are the " +
			"cloud budget billet may run at once, and there is no host to detect it from")
	}
	if len(p.Shapes) == 0 {
		return errors.New("--instance-type is required at least once: billet ships no table of " +
			"EC2 shapes, so it must be told which ones the fallback may buy")
	}

	if err := checkHostInputs(p.Host); err != nil {
		return err
	}

	if p.Ref == "" {
		return errors.New("initconfig: a hybrid generation needs the release every layer pins")
	}

	if p.Commission && p.Facts == nil {
		return errors.New("--commission needs --terraform-output: the ec2 node it adds to the " +
			"controller names the subnet and security groups only the apply can produce")
	}
	if p.AMI != "" && !p.Commission {
		return errors.New("--ami is read on the commission render only; before it every tier " +
			"carries the placeholder, because nothing launches until the node exists")
	}
	// A COMMISSIONED DEPLOYMENT WITH THE PLACEHOLDER IS WORSE THAN A REFUSAL:
	// it lifts the hold, enables the orchestrator and advertises the fallback
	// tiers, and the first job that needs the cloud fails at launch on an image
	// that does not exist. The AMI exists by the time of this render (the
	// runbook builds it the step before), so it is required rather than
	// defaulted.
	if p.Commission && p.AMI == "" {
		return errors.New("--commission needs --ami: the render it produces advertises the ec2 " +
			"fallback, and a placeholder image would fail every job that reaches it")
	}
	if idx := strings.IndexFunc(p.AMI, badImageRune); idx >= 0 {
		return fmt.Errorf("--ami: %q contains whitespace or a control character", p.AMI)
	}

	return nil
}

// hybridFactValues resolves every consumed output to its value or placeholder.
func hybridFactValues(p HybridParams) map[string]string {
	f := HybridFacts{}
	if p.Facts != nil {
		f = *p.Facts
	}

	// A DECLARED ADDRESS WINS OVER AN OBSERVED ONE, and is known before any
	// apply: it is the input the root pins the instance to, so every site that
	// spells it -- listen, server_addr, ansible_host -- takes it from here.
	if p.ControlPlaneIP != "" {
		f.ControlPlanePrivateIP = p.ControlPlaneIP
	}

	pick := func(output, value string) string {
		if value != "" {
			return value
		}

		return HybridPlaceholder(output)
	}

	return map[string]string{
		HybridOutputCacheBucket:           pick(HybridOutputCacheBucket, f.CacheBucket),
		HybridOutputCachePrefix:           pick(HybridOutputCachePrefix, f.CachePrefix),
		HybridOutputAvailabilityZone:      pick(HybridOutputAvailabilityZone, f.AvailabilityZone),
		HybridOutputControlPlanePrivateIP: pick(HybridOutputControlPlanePrivateIP, f.ControlPlanePrivateIP),
		HybridOutputLedgerVolumeID:        pick(HybridOutputLedgerVolumeID, f.LedgerVolumeID),
		HybridOutputSubnetID:              pick(HybridOutputSubnetID, f.SubnetID),
		HybridOutputRunnerSecurityGroup:   pick(HybridOutputRunnerSecurityGroup, f.RunnerSecurityGroupID),
		HybridOutputUntrustedRunnerSG:     pick(HybridOutputUntrustedRunnerSG, f.UntrustedRunnerSecurityGroupID),
		HybridOutputAMIPayloadBucket:      pick(HybridOutputAMIPayloadBucket, f.AMIPayloadBucket),
	}
}

// hybridTiers is the catalogue both nodes can serve: one tier per declared
// shape that fits the cloud budget, kept only where it ALSO fits the local
// host's ceiling under the same shared-floor rule -- every tier is its own
// scale set and escrows one backed slot before it advertises, so the floor is
// one job of every tier at once, on each node separately.
func hybridTiers(
	shapes []config.EC2InstanceType, cloudVCPU int, cloudMemory config.ByteSize,
	localVCPU int, localMemory config.ByteSize,
) []tier {
	cloud := remoteTiers(shapes, cloudVCPU, cloudMemory, func(s config.RemoteShape) string {
		return fmt.Sprintf("billet-%dvcpu-ubuntu-2404", s.VCPU)
	})

	var (
		fit        []tier
		usedVCPU   int
		usedMemory config.ByteSize
	)

	for _, t := range cloud {
		if usedVCPU+t.vcpu > localVCPU || usedMemory+t.memory > localMemory {
			continue
		}

		fit = append(fit, t)
		usedVCPU += t.vcpu
		usedMemory += t.memory
	}

	return fit
}

// renderHybridTiers writes the catalogue as YAML list items at two-space
// indent, with ordered providers and one launch entry per backend.
func renderHybridTiers(ts []tier, p HybridParams, trusted bool, ami string) string {
	var b strings.Builder

	for i, t := range ts {
		if i > 0 {
			b.WriteString("\n")
		}

		disk := max(hybridDiskFloor, config.ByteSize(t.vcpu)*hybridDiskPerVCPU)

		fmt.Fprintf(&b, "  - label: %s\n", t.label)
		// THE ORDER DECIDES, at escrow: the allocator walks it most-preferred
		// first over the hosts that can serve the tier and have room, so a job
		// reaches the cloud only when home is full.
		b.WriteString("    providers: [firecracker, ec2]\n")
		b.WriteString("    guest_os: linux\n")
		fmt.Fprintf(&b, "    vcpu: %d\n    memory: %s\n    disk: %s\n", t.vcpu, t.memory, disk)
		b.WriteString("    launch:\n")
		fmt.Fprintf(&b, "      firecracker:\n        image: %s\n", yamlScalar(p.LocalImage))
		fmt.Fprintf(&b, "      ec2:\n        image: %s\n        command: [%s]\n",
			yamlScalar(ami), hybridRunnerCommand)
		renderTierPolicy(&b, Params{RunnerGroup: p.RunnerGroup, Workflows: p.Workflows}, trusted)
	}

	return b.String()
}

// hybridGitHubYAML renders the github block with whatever identity was carried.
func hybridGitHubYAML(p HybridParams) string {
	org := p.Org
	if org == "" {
		org = "<your-org>"
	}

	var b strings.Builder

	fmt.Fprintf(&b, "github:\n  org: %s\n", yamlScalar(org))
	if p.AppID == 0 {
		b.WriteString("  # Filled in by `billet github-app create`; the runbook's first step.\n")
	}
	fmt.Fprintf(&b, "  app_id: %d\n  installation_id: %d\n", p.AppID, p.InstallationID)
	if p.ClientID != "" {
		fmt.Fprintf(&b, "  client_id: %s\n", yamlScalar(p.ClientID))
	}
	fmt.Fprintf(&b, "  private_key_path: %s\n", serviceKeyPath)

	return b.String()
}

// hybridSitesYAML declares the two places this deployment stores caches in, or
// nothing when it keeps none.
//
// A SITE IS WHERE COMPUTE AND ITS STORAGE SHARE A FAST NETWORK, which is exactly
// why a hybrid deployment has two: the Firecracker host's generations live in
// its own Ceph pools and an EC2 job cannot reach them across the WAN. The
// control plane reads this list; it is what authorises each node's reported
// site, and cache keys are scoped by it.
func hybridSitesYAML(p HybridParams) string {
	if !p.Cache {
		return ""
	}

	return fmt.Sprintf(`
# WHERE THIS DEPLOYMENT KEEPS CACHES. Two places, because the two halves cannot
# reach each other's storage: the machine at home writes RBD images into its own
# Ceph pools, and the cloud half writes EBS snapshots with a pointer in S3. A
# node reports its site at registration and the control plane matches it
# EXACTLY, so these names and the node.site values below are one string.
sites:
  - name: %s
    store: ceph
  - name: %s
    store: ebs-s3
`, yamlScalar(p.LocalSite), yamlScalar(p.CloudSite))
}

// hybridCloudCacheYAML is the orchestrator's half of the cloud cache: the store
// its job instances read and write, and the listener they fetch through.
func hybridCloudCacheYAML(p HybridParams, facts map[string]string) string {
	// THE CONTROLLER'S OWN ADDRESS, taken from the same resolved fact server.listen
	// and every server_addr take it from, so the listener and the wire cannot name
	// two different machines.
	ip := facts[HybridOutputControlPlanePrivateIP]

	if !p.Cache {
		return ""
	}

	return fmt.Sprintf(`
  # THE CLOUD SITE'S STORE. Generations are EBS snapshots and the fenced pointer
  # is one S3 object; the zone is the subnet's, because a cache volume and the
  # instance consuming it must be in one zone. The region is the node's own —
  # billet refuses a cache in a different one.
  ebs_s3:
    region: %s
    availability_zone: %s
    bucket: %s
    prefix: %s

  # WHAT A JOB INSTANCE FETCHES THROUGH. One literal, non-loopback address —
  # the controller's, which is where this orchestrator runs — and HTTPS, because
  # the guest's bearer token crosses the VPC to reach it. The module's cache rule
  # is what admits the runner group to this port and nothing else.
  #
  # THE PAIR IS YOURS TO SUPPLY. The role installs whatever
  # billet_cache_tls_cert_src and billet_cache_tls_key_src name to these paths;
  # the certificate has to be valid for the address above, since that is what the
  # guest dials.
  cache:
    listen: %s
    guest_endpoint: %s
    tls_cert: %s
    tls_key: %s
`,
		yamlScalar(p.Region),
		yamlScalar(facts[HybridOutputAvailabilityZone]),
		yamlScalar(facts[HybridOutputCacheBucket]),
		yamlScalar(facts[HybridOutputCachePrefix]),
		yamlScalar(fmt.Sprintf("%s:%d", ip, hybridCachePort)),
		yamlScalar(fmt.Sprintf("https://%s:%d", ip, hybridCachePort)),
		hybridCacheTLSCert, hybridCacheTLSKey,
	)
}

// hybridSiteLineYAML is a node's own `site:` line, or nothing.
func hybridSiteLineYAML(p HybridParams, site string) string {
	if !p.Cache {
		return ""
	}

	return fmt.Sprintf("  site: %s\n", yamlScalar(site))
}

// hybridTLSYAML is the bundle `billet ca issue` writes, at the path the runbook
// installs it to, at the given indent.
func hybridTLSYAML(indent string) string {
	return fmt.Sprintf("%stls:\n%s  cert: %s/node.crt\n%s  key: %s/node.key\n%s  ca: %s/ca.crt\n",
		indent, indent, hybridTLSDir, indent, hybridTLSDir, indent, hybridTLSDir)
}

// renderHybridController is the controller's billet.yaml without its tiers:
// the server, the App, the off-site copy, and on the commission render the
// co-located ec2 orchestrator.
func renderHybridController(
	p HybridParams, trusted bool, wire string, facts map[string]string,
	localVCPU int, localMemory config.ByteSize,
) string {
	var b strings.Builder

	fmt.Fprintf(&b, `# The control plane, and on the commission render the EC2 orchestrator beside it.
server:
  # A CONCRETE ADDRESS, NOT A WILDCARD, so it is its own certificate subject
  # name and server.node_tls_hosts is unnecessary. Not loopback either: the
  # two roles are on different machines, so the wire is mTLS.
  listen: %s
  # DELIBERATELY NO bootstrap_listen. Its absence refuses enrollment over the
  # network; certificates come from `+"`billet ca issue`"+` on this host.
  state_dir: %s/server
  # THE DEPLOYMENT CEILING: the local host's contribution plus the cloud
  # budget, because both nodes escrow against it.
  max_vcpu: %d
  max_memory: %s

%s
# THE OFF-SITE COPY. The bucket, region and prefix are exactly what the
# Terraform root created and scoped the controller's grant to; a different
# prefix is denied rather than written somewhere unprotected.
backup:
  s3:
    bucket: %s-backups
    region: %s
    prefix: %s
%s`,
		yamlScalar(wire), serviceStateBase,
		localVCPU+p.CloudVCPU, localMemory+p.CloudMemory,
		hybridGitHubYAML(p),
		p.Name, yamlScalar(p.Region), hybridBackupPrefix,
		hybridSitesYAML(p),
	)

	if !p.Commission {
		return b.String()
	}

	fmt.Fprintf(&b, `
# THE EC2 ORCHESTRATOR, co-located with the control plane. It runs no jobs
# itself: it calls the EC2 API and the compute appears in the region. It dials
# the server on the same machine over mTLS, because the listener is not
# loopback, so it carries a certificate like any other node.
node:
  server_addr: %s
%s  provider: ec2
%s  state_dir: %s/node
  lock_dir: %s
  # REQUIRED for ec2 and equal to the cloud budget: there is no host to detect
  # a contribution from.
  max_vcpu: %d
  max_memory: %s
  ec2:
    region: %s
    subnet_id: %s
    security_group_ids:
      - %s
`,
		yamlScalar(wire), hybridTLSYAML("  "),
		hybridSiteLineYAML(p, p.CloudSite),
		serviceStateBase, serviceLockDir,
		p.CloudVCPU, p.CloudMemory,
		yamlScalar(p.Region), yamlScalar(facts[HybridOutputSubnetID]),
		yamlScalar(facts[HybridOutputRunnerSecurityGroup]),
	)

	if !trusted {
		fmt.Fprintf(&b, "    untrusted_security_group_ids:\n      - %s\n",
			yamlScalar(facts[HybridOutputUntrustedRunnerSG]))
	}

	b.WriteString("    # The shapes billet may buy, each DECLARING what it holds; VERIFY the\n")
	b.WriteString("    # prices, which only report exposure and never gate a job.\n")
	b.WriteString("    instance_types:\n")
	b.WriteString(ec2InstanceTypesYAML(p.Shapes))
	b.WriteString(hybridCloudCacheYAML(p, facts))

	return b.String()
}

// renderHybridLocal is the Firecracker host's billet.yaml without its tiers.
//
// NO server: AND NO github:, and the absence of both is load-bearing. A
// server block on a node re-arms "whichever role runs first mints the
// identity"; a github block makes the role demand the App key on the host
// that runs untrusted code.
func renderHybridLocal(
	p HybridParams, trusted bool, wire string, localVCPU int, localMemory config.ByteSize,
) string {
	return fmt.Sprintf(`# The Firecracker host: a NODE ONLY. There is no server: block, because a
# certless node beside one mints the deployment identity, and no github: block,
# because the App key must never reach the host that runs untrusted code.
node:
  # name is omitted: it comes from the certificate `+"`billet ca issue`"+` wrote.
  server_addr: %s
%s  provider: firecracker
%s  state_dir: %s/node
  lock_dir: %s
  # What this host CONTRIBUTES: what it has, minus the headroom the kernel,
  # Ceph and your shell need.
  max_vcpu: %d
  max_memory: %s
%s`,
		yamlScalar(wire), hybridTLSYAML("  "),
		hybridSiteLineYAML(p, p.LocalSite),
		serviceStateBase, serviceLockDir,
		localVCPU, localMemory,
		firecrackerNodeBlocks(trusted, p.Host),
	)
}

// indentYAML nests body under an inventory key, textually, so its comments
// survive -- the same rule AnsibleVars follows. A blank line stays blank.
func indentYAML(body string, indent string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(body, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			b.WriteString("\n")

			continue
		}
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteString("\n")
	}

	return b.String()
}

// renderHybridInventory writes the two hosts around one tier catalogue.
func renderHybridInventory(
	p HybridParams, trusted bool, facts map[string]string, tiers, controller, local string,
) string {
	var b strings.Builder

	render := "plan"
	switch {
	case p.Commission:
		render = "commission"
	case p.Facts != nil:
		render = "prepare"
	}

	fmt.Fprintf(&b, `# %s (%s render) for the %s deployment.
#
# TWO HOSTS, ONE ORDER: site.yml converges the control plane first, because the
# node wire negotiates a protocol range and the control plane is upgraded first.
# The runbook says which host to converge when; every step uses -l.
all:
  vars:
    # THE TIER CATALOGUE, DEFINED ONCE AND CONSUMED BY BOTH HOSTS. The server
    # reads it at startup and turns each tier into a GitHub scale set; the
    # Firecracker node reads it to know which golden images it must be able to
    # boot, and the host role's binary upgrade fails without one. An anchor
    # rather than a copy, so the two cannot drift.
    billet_tier_catalogue: &billet_tier_catalogue
%s
  children:
    control_plane:
      hosts:
        %s:
          # The controller's private address, reached however you chose in
          # docs/deploying/reaching-hosts.md. The same string is server.listen
          # and every node's server_addr; the Terraform root declares it.
          ansible_host: %s
          # Canonical's Ubuntu 24.04 AMI default user.
          ansible_user: ubuntu
          ansible_python_interpreter: /usr/bin/python3

          # The dedicated encrypted ledger volume the root created, by id and
          # never by device name: the role mounts it FAIL CLOSED, formats it only
          # when blank, proves it, and adds Requires= to billet-server.service.
          billet_ledger_volume_id: %s

`,
		HybridMarker, render, p.Name,
		indentYAML(tiers, "    "),
		p.ControllerName,
		yamlScalar(facts[HybridOutputControlPlanePrivateIP]),
		yamlScalar(facts[HybridOutputLedgerVolumeID]),
	)

	if p.Commission {
		b.WriteString(`          # COMMISSIONED: the server starts, and the co-located ec2 orchestrator
          # beside it. Both certificates are installed (the runbook's ca issue
          # step), so billet check no longer refuses the node's bundle.
          billet_server_prepare_only: false
          billet_enable_server: true
          billet_enable_node: true
`)
	} else {
		b.WriteString(`          # PREPARE ONLY, AND SERVER ONLY, until the commission render. The role
          # mounts and proves the ledger volume, installs the binary and the units,
          # and HOLDS both services, so nothing mints a deployment identity until
          # billet ca issue has run. The node block is absent because its
          # certificate bundle does not exist yet and the role's billet check
          # refuses a missing one.
          billet_server_prepare_only: true
          billet_enable_server: true
          billet_enable_node: false
`)
	}

	fmt.Fprintf(&b, `          # No Firecracker and no Ceph: a t-class controller runs no jobs.
          billet_firecracker_enabled: false
          billet_ceph_enabled: false

          # FETCHED BY VERSION, resolving this host's own architecture, and
          # billet_binary_src EMPTIED so a staged amd64 binary from the
          # environment can never land on an arm64 controller.
          billet_binary_src: ""
          billet_version: %s

          billet_config:
%s            tiers: *billet_tier_catalogue

    linux:
      hosts:
        %s:
          # The Firecracker host's address AS ANSIBLE REACHES IT: a LAN address
          # from a workstation on the same network, a Mesh or tunnel address from
          # CI. billet never needs it; only the converge does.
          ansible_host: <the address Ansible reaches this host on>
%s          ansible_python_interpreter: /usr/bin/python3

          # NODE ONLY. The control plane is %s.
          billet_enable_server: false
          billet_enable_node: true

          # THE CEPH DECISION IS YOURS AND IT IS DESTRUCTIVE. Left false, the role
          # expects a cluster this host can already reach. Set true only to create
          # one, and only with billet_ceph_devices naming every disk it may
          # consume — that path wipes them, and billet never infers a disk.
          billet_ceph_bootstrap: false
          # billet_ceph_mon_ip: 192.0.2.20
          # billet_ceph_devices: [/dev/disk/by-id/nvme-...]

          billet_version: %s

          billet_config:
%s            tiers: *billet_tier_catalogue
`,
		yamlScalar(p.Ref),
		indentYAML(controller, "            "),
		p.LocalName,
		hybridLocalUserYAML(p.LocalAnsibleUser),
		p.ControllerName,
		yamlScalar(p.Ref),
		indentYAML(local, "            "),
	)

	_ = trusted

	return b.String()
}

// hybridLocalUserYAML is the local host's ansible_user line, or the comment
// that explains its absence: owned hardware has no reason to carry the cloud
// image's account, and the generator cannot see that machine.
func hybridLocalUserYAML(user string) string {
	if user == "" {
		return "          # ansible_user is omitted: your SSH configuration, or --local-ansible-user, says\n" +
			"          # which account Ansible connects as. Owned hardware has no `ubuntu` by default.\n"
	}

	return fmt.Sprintf("          ansible_user: %s\n", yamlScalar(user))
}

// renderHybridTerraform writes the root that creates the controller, the
// fleet's IAM and network, the backup bucket, and what the runbook needs.
func renderHybridTerraform(p HybridParams, trusted bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, `# %s: the AWS half of the %s deployment.
#
# The billet root module creates the controller with its retained ledger
# volume and auto-recovery, the fleet's node role and trusted-runner security
# group, and the backup bucket whose grant lands on the role the co-located
# controller runs with. This file adds what the hybrid shape needs beside it.
#
# APPLIED BY HAND. A push should not start something that drains a control
# plane. Pin the module to the same release as the collection and the binary.

terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}

provider "aws" {
  region = %q
}

data "aws_caller_identity" "this" {}

module "billet" {
  source = "github.com/junioryono/billet//terraform/modules/billet?ref=%s"

  name = %q

`, HybridMarker, p.Name, p.Region, p.Ref, p.Name)

	if p.ControlPlaneIP != "" {
		fmt.Fprintf(&b, `  # DECLARED, so an instance replacement cannot change the address that is
  # server.listen, every node's server_addr and the inventory's ansible_host.
  control_plane_private_ip = %q
`, p.ControlPlaneIP)
	} else {
		b.WriteString(`  # NOT DECLARED, so AWS chooses the address at launch and an instance
  # replacement changes it silently. Declare it here once you know the subnet,
  # or generate again with --control-plane-private-ip; the prepare render
  # fills the inventory from the control_plane_private_ip output either way.
  # control_plane_private_ip = "10.60.0.10"
`)
	}

	b.WriteString(`
  # A BACKUP ON THE DISK IT PROTECTS IS NOT ONE. The archive is the ledger, the
  # deployment identity, the node-wire CA and the App key as one unit; this
  # bucket is where billet local backup copies it, and the grant is on the node
  # role the controller runs with.
  create_backup_bucket = true
`)

	if p.Cache {
		b.WriteString(`
  # THE EBS+S3 SITE CACHE. The bucket holds each generation's fenced pointer and
  # lease state; the generations themselves are EBS snapshots. Turning it on also
  # creates the one rule that admits the runner group to the controller's cache
  # port, and nothing else — the listener, its TLS pair and node.ebs_s3 are in
  # the inventory beside it.
  enable_cache = true
`)
	} else {
		b.WriteString(`
  # THE EBS+S3 SITE CACHE IS OFF. Generate with --cache to turn it on: the
  # module then creates the bucket and the rule admitting runners to the
  # controller's cache listener, and the orchestrator gains node.ebs_s3 and
  # node.cache. You supply the listener's TLS pair; nothing else changes.
  enable_cache = false
`)
	}

	if p.Builder {
		b.WriteString(`
  # THE BUILDER'S GRANT, so ` + "`billet ami build`" + ` runs on the controller with its
  # own instance role rather than from a workstation holding your AWS
  # credentials. It is additive: the builder's launches ride the node policy's
  # RunInstances, and the payload grant reaches only the objects billet stages.
  builder                = true
  builder_payload_bucket = aws_s3_bucket.ami_payloads.bucket
`)
	} else {
		b.WriteString(`
  # NO BUILDER GRANT. ` + "`billet ami build`" + ` therefore runs from a machine with AWS
  # credentials of its own; generate with --builder to move it onto the
  # controller, which widens the node role by exactly billet's builder document.
  # builder = true
`)
	}

	if len(p.SSHIngressCIDRs) > 0 {
		b.WriteString("\n  # The machine that runs Ansible, when it reaches the controller by SSH.\n")
		b.WriteString("  ssh_ingress_cidrs = [")
		for i, c := range p.SSHIngressCIDRs {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", c)
		}
		b.WriteString("]\n")
	} else {
		b.WriteString(`
  # No SSH ingress. A route that ENDS ON THE CONTROLLER needs none: the Systems
  # Manager agent, or a cloudflared tunnel or route running on this host, which
  # a WARP client then reaches (docs/deploying/reaching-hosts.md). A connector
  # elsewhere in the VPC, or a workstation on the same network, needs its own
  # source named here.
  ssh_ingress_cidrs = []
`)
	}

	if p.SSHKeyName != "" {
		fmt.Fprintf(&b, `
  # The key pair Ansible's SSH presents to a fresh Canonical image.
  key_name = %q
`, p.SSHKeyName)
	} else {
		b.WriteString(`
  # NO KEY PAIR, so a fresh Canonical image has no operator key at all: the
  # runbook reaches it with EC2 Instance Connect, which pushes a key for sixty
  # seconds under IAM. Name a key pair here (or --key-name) for ordinary SSH.
  # key_name = "my-key"
`)
	}

	b.WriteString("}\n")

	if !trusted {
		fmt.Fprintf(&b, `
# THE UNTRUSTED RUNNER GROUP, which the billet root deliberately leaves to you:
# the tiers are trust: untrusted, so a fork's pull request launches an instance
# in this group. Egress only, and nothing else in the VPC admits it -- the
# control plane's node wire refuses a caller with no certificate, and the cache
# rule admits the trusted group by identity. What a security group cannot do
# is deny: bound what a stranger's code can reach with your network's ACLs.
resource "aws_security_group" "untrusted_runner" {
  name        = "%s-untrusted-runner"
  description = "billet untrusted (fork pull-request) runners: egress only"
  vpc_id      = module.billet.vpc_id
  tags        = { Name = "%s-untrusted-runner" }
}

resource "aws_vpc_security_group_egress_rule" "untrusted_runner_all" {
  security_group_id = aws_security_group.untrusted_runner.id
  description       = "all egress (GitHub, registries, package mirrors)"
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "-1"
}
`, p.Name, p.Name)
	}

	fmt.Fprintf(&b, `
# WHERE billet ami build STAGES ITS INSTALLERS (--payload-bucket, required).
# Private, and expired after a week: the builder removes its own payload as
# each build ends, and this catches the build that died before it could.
resource "aws_s3_bucket" "ami_payloads" {
  bucket = "%s-ami-payloads-${data.aws_caller_identity.this.account_id}"
  tags   = { Name = "%s-ami-payloads" }
}

resource "aws_s3_bucket_public_access_block" "ami_payloads" {
  bucket                  = aws_s3_bucket.ami_payloads.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_server_side_encryption_configuration" "ami_payloads" {
  bucket = aws_s3_bucket.ami_payloads.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "ami_payloads" {
  bucket = aws_s3_bucket.ami_payloads.id

  rule {
    id     = "expire-stale-payloads"
    status = "Enabled"

    filter {}

    expiration {
      days = 7
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
  }
}

# EVERY FACT THE INVENTORY CONSUMES, by the name a placeholder there uses:
#   terraform output -json > ../outputs.json
# and billet init hybrid --terraform-output fills them in.
output %q {
  description = "The controller's private address: server.listen and every node's server_addr."
  value       = module.billet.control_plane_private_ip
}

output "node_wire_address" {
  description = "The host:port every node dials."
  value       = module.billet.node_wire_address
}

output %q {
  description = "The dedicated ledger volume; the host role mounts it fail closed as billet_ledger_volume_id."
  value       = module.billet.ledger_volume_id
}

output %q {
  description = "Where the fallback instances launch (node.ec2.subnet_id), and where billet ami build launches its builder."
  value       = module.billet.subnet_id
}

output %q {
  description = "The trusted-runner security group (node.ec2.security_group_ids)."
  value       = module.billet.runner_security_group_id
}
`, p.Name, p.Name,
		HybridOutputControlPlanePrivateIP, HybridOutputLedgerVolumeID,
		HybridOutputSubnetID, HybridOutputRunnerSecurityGroup)

	if !trusted {
		fmt.Fprintf(&b, `
output %q {
  description = "The untrusted-runner security group (node.ec2.untrusted_security_group_ids)."
  value       = aws_security_group.untrusted_runner.id
}
`, HybridOutputUntrustedRunnerSG)
	}

	if p.Cache {
		fmt.Fprintf(&b, `
output %q {
  description = "The cache bucket holding each generation's fenced pointer and lease state (node.ebs_s3.bucket)."
  value       = module.billet.cache_bucket
}

output %q {
  description = "The object prefix isolating this deployment inside that bucket (node.ebs_s3.prefix)."
  value       = module.billet.cache_prefix
}

output %q {
  description = "The subnet's zone. A cache volume and the instance consuming it must be in one zone (node.ebs_s3.availability_zone)."
  value       = module.billet.availability_zone
}
`, HybridOutputCacheBucket, HybridOutputCachePrefix, HybridOutputAvailabilityZone)
	}

	fmt.Fprintf(&b, `
output "backup_bucket" {
  description = "backup.s3.bucket; the inventory already names it, because the root composes the name."
  value       = module.billet.backup_bucket
}

output "backup_prefix" {
  description = "backup.s3.prefix, the literal the grant is scoped to."
  value       = module.billet.backup_prefix
}

output "region" {
  description = "The region everything above is in."
  value       = module.billet.region
}

output "control_plane_instance_id" {
  description = "The controller instance, for an EC2 Instance Connect or Systems Manager session."
  value       = module.billet.control_plane_instance_id
}

output %q {
  description = "Pass to billet ami build --payload-bucket."
  value       = aws_s3_bucket.ami_payloads.id
}
`, HybridOutputAMIPayloadBucket)

	return b.String()
}

// renderHybridSite writes the playbook: the control plane first, then the
// Firecracker host.
func renderHybridSite(p HybridParams) string {
	return fmt.Sprintf(`# %s for the %s deployment.
#
# THE CONTROL PLANE FIRST. The node wire negotiates a protocol range and the
# control plane is upgraded first, so a skew is survivable in this order and
# not the other. The runbook converges one host at a time with -l; a bare run
# reaching the local host before its certificate is installed fails at billet
# check after a wasted drain.
- name: billet control plane
  hosts: control_plane
  become: true
  gather_facts: true
  roles:
    - role: junioryono.billet.host

- name: billet Firecracker host
  hosts: linux
  become: true
  gather_facts: true
  roles:
    - role: junioryono.billet.host
`, HybridMarker, p.Name)
}

// renderHybridRequirements pins the collection to the same release as the
// module and the binary.
func renderHybridRequirements(p HybridParams) string {
	return fmt.Sprintf(`# %s for the %s deployment.
#
# ONE VERSION EVERYWHERE: this, the module's ?ref= in terraform/main.tf and
# billet_version in the inventory name the same release, or the node wire and
# the host role describe different software.
collections:
  - name: git+https://github.com/junioryono/billet.git#/ansible_collections/junioryono/billet/
    type: git
    version: %s
  - name: ansible.posix
    version: ">=1.5.4"
`, HybridMarker, p.Name, p.Ref)
}
