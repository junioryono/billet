package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/version"
)

// HybridRunbookFile is the fifth file a hybrid generation writes: the ordered
// steps between the files, rendered here because only the CLI knows the flags
// it has to repeat.
const HybridRunbookFile = "RUNBOOK.md"

// hybridInputs are the parsed flags that DESCRIBE the deployment, kept so the
// runbook can print the exact command that renders the next phase.
type hybridInputs struct {
	name, region, org, runnerGroup string
	workflows                      []string
	controlPlaneIP                 string
	controllerName, localName      string
	localVCPU, cloudVCPU           int
	localMemory, cloudMemory       string
	instanceTypes, priceOverrides  []string
	sshIngress                     []string
	keyName, localUser, localImage string
	builder, cache                 bool
	localSite, cloudSite           string
	kernelImage, cephUser, cephKey string
	cacheListen, cacheGuest        string
	cfgPath                        string
	out                            string
}

// cmdInitHybrid writes the hybrid shape -- a Terraform root, an inventory with
// both hosts, a playbook, the collection pin and a runbook -- into a directory.
//
// A DIRECTORY, NEVER /etc/billet, so the two writers of a host's config keep
// their rules. Its own rule: a file is replaced only when its first line
// carries billet's marker (it is billet's own), otherwise the fresh generation
// lands beside it at <file>.new; --force replaces regardless.
//
// The facts a deployment needs arrive in three waves, and the flags select
// the render: nothing extra is the PLAN (placeholders for what the apply
// produces), --terraform-output is the PREPARE (filled, still held), and
// --commission adds the ec2 node and the AMI. The reason for the middle one
// is the certificate: the co-located node names a bundle that exists only
// after billet ca issue has run on the prepared host, and the role's billet
// check refuses a missing one.
func cmdInitHybrid(ctx context.Context, args []string) error {
	fs := newFlagSet("billet init hybrid")

	out := fs.String("out", "", "the directory to write the generation into (required)")
	name := fs.String("name", "billet", "the Terraform module's name prefix for every AWS resource")
	region := fs.String("region", "", "the AWS region for the controller and the fallback compute (required)")
	org := fs.String("org", "", "the GitHub organization these runners serve")
	group := fs.String("runner-group", "",
		"the GitHub runner group a trusted tier belongs to (omit both policy flags for untrusted tiers)")
	var workflows repeatedString
	fs.Var(&workflows, "workflow", "a workflow ref a trusted tier may run (repeatable)")
	cfgPath := addConfigFlag(fs)

	controlPlaneIP := fs.String("control-plane-private-ip", "",
		"the controller's private address, DECLARED so an instance replacement cannot change it; "+
			"empty lets AWS choose and fills the inventory from the apply's output")
	controllerName := fs.String("controller-name", "",
		"the controller's inventory and certificate name (default <name>-control-plane)")
	localName := fs.String("local-name", "",
		"the Firecracker host's inventory and certificate name (default <name>-fc-1)")

	localVCPU := fs.Int("local-vcpu", 0, "what the Firecracker host HAS (required); its contribution leaves headroom")
	localMemory := fs.String("local-memory", "", "what the Firecracker host has, e.g. 128GiB (required)")
	maxVCPU := fs.Int("max-vcpu", 0, "the cloud vCPU budget billet may run at once (required)")
	maxMemory := fs.String("max-memory", "", "the cloud memory budget, e.g. 64GiB (required)")
	var instanceTypes repeatedString
	fs.Var(&instanceTypes, "instance-type",
		"an EC2 shape the fallback may buy: TYPE (vcpu, memory and price fetched from AWS) or "+
			"TYPE=vcpu,memory,usd declared (repeatable, most preferred first)")
	var priceOverrides repeatedString
	fs.Var(&priceOverrides, "price", "override a fetched shape's price, as type=usd (repeatable)")
	var sshIngress repeatedString
	fs.Var(&sshIngress, "ssh-ingress-cidr",
		"an IPv4 CIDR that may SSH to the controller, for a workstation on the same network (repeatable)")
	keyName := fs.String("key-name", "",
		"the EC2 key pair the controller launches with; empty attaches none and the runbook reaches "+
			"the fresh image with EC2 Instance Connect")
	localUser := fs.String("local-ansible-user", "",
		"the account Ansible connects to the Firecracker host as; empty leaves it to your SSH configuration")
	localImage := fs.String("local-image", "",
		"the guest generation every tier boots on the Firecracker host (default "+
			initconfig.DefaultFirecrackerImage+", the x64 generation billet publishes)")
	cache := fs.Bool("cache", false,
		"give the cloud half an EBS+S3 site cache: the module creates the bucket and the rule "+
			"admitting runners to it, the orchestrator gains node.ebs_s3 and a node.cache listener, "+
			"and both hosts declare the site their storage belongs to (you supply the listener's "+
			"TLS pair)")
	localSite := fs.String("local-site", "",
		"the name of the place the Firecracker host is in, whose store is its Ceph cluster "+
			"(default "+initconfig.HybridDefaultLocalSite+"; with --cache)")
	cloudSite := fs.String("cloud-site", "",
		"the name of the place the cloud half is in, whose store is EBS and S3 (default the "+
			"region; with --cache)")
	builder := fs.Bool("builder", false,
		"grant the controller's role what `billet ami build` performs, so the image is built on "+
			"the controller rather than from a workstation holding your AWS credentials; it widens "+
			"the node role by exactly billet's builder document")

	kernelImage := fs.String("kernel-image", "", "the Firecracker host's pinned guest kernel")
	cephUser := fs.String("ceph-user", "", "the RADOS identity billet authenticates as, WITHOUT the `client.` prefix")
	cephKeyring := fs.String("ceph-keyring", "", "that identity's keyring")
	cacheListen := fs.String("cache-listen", "", "one literal, non-loopback address guests reach the cache on")
	cacheGuest := fs.String("cache-guest-endpoint", "", "the HTTP origin placed in guest metadata")

	tfOutput := fs.String("terraform-output", "",
		"the file `terraform output -json` wrote after the apply; fills every placeholder")
	commission := fs.Bool("commission", false,
		"the third render: lift the prepare-only hold and add the ec2 orchestrator to the controller "+
			"(needs --terraform-output; run after billet ca issue)")
	ami := fs.String("ami", "", "the AMI `billet ami build` produced, for every tier's launch.ec2 (with --commission)")
	force := fs.Bool("force", false, "replace a file in --out even when it is not billet's own")

	if err := parse(fs, args); err != nil {
		return err
	}

	// WHICH FLAGS WERE ACTUALLY PASSED, so the re-run command the runbook prints
	// repeats a --config the operator named and not the per-user default.
	setFlags := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	if *out == "" {
		return errors.New("--out is required: the generation is five files, and they belong together " +
			"in one directory")
	}
	if *controllerName == "" {
		*controllerName = *name + "-control-plane"
	}
	if *localName == "" {
		*localName = *name + "-fc-1"
	}
	if *commission && *tfOutput == "" {
		return errors.New("--commission needs --terraform-output: the ec2 node it adds names the " +
			"subnet and security groups only the apply can produce")
	}
	if *ami != "" && !*commission {
		return errors.New("--ami is read with --commission only; before it every tier carries the " +
			"placeholder, because nothing launches until the node exists")
	}
	if !*cache && (*localSite != "" || *cloudSite != "") {
		return errors.New("--local-site and --cloud-site name the two places a cache lives in, " +
			"so they are only read with --cache; without one this deployment declares no sites " +
			"at all and the names would be written nowhere")
	}
	if *commission && *ami == "" {
		return errors.New("--commission needs --ami: the render it produces advertises the ec2 " +
			"fallback, and a placeholder image would fail every job that reaches it; " +
			"`billet ami build` is the runbook's step before this one")
	}

	// THE CHEAP REFUSALS BEFORE ANY AWS CALL: a typoed memory must not pay a
	// signed round trip and then die naming nothing.
	localMem, err := parseRequiredMemory("--local-memory", *localMemory)
	if err != nil {
		return err
	}
	cloudMem, err := parseRequiredMemory("--max-memory", *maxMemory)
	if err != nil {
		return err
	}
	if *region == "" {
		return errors.New("--region is required: billet cannot choose which region the controller " +
			"and the fallback compute live in")
	}
	if err := config.CheckEC2Region(*region); err != nil {
		return fmt.Errorf("--region: %w", err)
	}

	shapes, err := hybridShapes(ctx, *region, instanceTypes, priceOverrides)
	if err != nil {
		return err
	}

	untrusted := *group == "" && len(workflows) == 0

	var facts *initconfig.HybridFacts
	if *tfOutput != "" {
		raw, err := os.ReadFile(*tfOutput)
		if err != nil {
			return fmt.Errorf("--terraform-output: %w", err)
		}
		f, err := initconfig.ParseTerraformOutput(raw, initconfig.HybridNeeds{
			Untrusted: untrusted, Cache: *cache,
		})
		if err != nil {
			return err
		}

		// THE OUTPUTS HAVE TO BE THIS ROOT'S, and the region is the one fact that
		// says so cheaply. Every rendering takes the region from --region while
		// every id here comes from an apply, so outputs from a root applied in
		// another region — or a re-render with --region retyped — produce a
		// config that signs against one region and names another's subnet,
		// security group, buckets and controller. Nothing downstream refuses it:
		// a cache generation eventually trips over the availability zone, and a
		// generation without one prints an AMI command that simply cannot work.
		if f.Region != *region {
			return fmt.Errorf("--terraform-output was written by a root in %s and this "+
				"generation is for %s: every id in it names resources in the other "+
				"region, so either re-render with --region %s or point "+
				"--terraform-output at this region's own outputs",
				f.Region, *region, f.Region)
		}

		facts = &f
	}

	ref, refNote := hybridRef()

	p := initconfig.HybridParams{
		Name:             *name,
		Region:           *region,
		Org:              *org,
		RunnerGroup:      *group,
		Workflows:        workflows,
		ControlPlaneIP:   *controlPlaneIP,
		ControllerName:   *controllerName,
		LocalName:        *localName,
		LocalVCPU:        *localVCPU,
		LocalMemory:      localMem,
		CloudVCPU:        *maxVCPU,
		CloudMemory:      cloudMem,
		Shapes:           shapes,
		SSHIngressCIDRs:  sshIngress,
		SSHKeyName:       *keyName,
		LocalAnsibleUser: *localUser,
		LocalImage:       *localImage,
		Builder:          *builder,
		Cache:            *cache,
		LocalSite:        *localSite,
		CloudSite:        *cloudSite,
		Host: initconfig.HostInputs{
			KernelImage:        *kernelImage,
			CephUser:           *cephUser,
			CephKeyringPath:    *cephKeyring,
			CacheListen:        *cacheListen,
			CacheGuestEndpoint: *cacheGuest,
		},
		Ref:        ref,
		Facts:      facts,
		Commission: *commission,
		AMI:        *ami,
	}

	// THE APP IDENTITY, carried from the file `billet github-app create` wrote
	// it into, under the same rules `billet init` applies: complete, and for
	// this organization.
	carried := false
	if raw, err := os.ReadFile(*cfgPath); err == nil {
		gb, _, ok := existingGitHubBlock(raw)
		switch {
		case ok && !gb.usable():
			fmt.Fprintf(os.Stdout, "NOTE: the App identity at %s is not complete (org %q, installation %d), "+
				"so it was NOT carried; re-run `billet github-app create` to record it together.\n\n",
				*cfgPath, gb.Org, gb.InstallationID)
		case ok && *org != "" && gb.Org != *org:
			fmt.Fprintf(os.Stdout, "NOTE: the App at %s belongs to %q, but this run is for %q; the identity "+
				"was NOT carried.\n\n", *cfgPath, gb.Org, *org)
		case ok:
			p.AppID, p.InstallationID, p.ClientID = gb.AppID, gb.InstallationID, gb.ClientID
			if p.Org == "" {
				p.Org = gb.Org
			}
			carried = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", *cfgPath, err)
	}

	files, trusted, err := initconfig.GenerateHybrid(p)
	if err != nil {
		return err
	}

	in := hybridInputs{
		name: *name, region: *region, org: p.Org, runnerGroup: *group, workflows: workflows,
		controlPlaneIP: *controlPlaneIP, controllerName: *controllerName, localName: *localName,
		localVCPU: *localVCPU, cloudVCPU: *maxVCPU, localMemory: *localMemory, cloudMemory: *maxMemory,
		instanceTypes: instanceTypes, priceOverrides: priceOverrides, sshIngress: sshIngress,
		keyName: *keyName, localUser: *localUser, localImage: *localImage, builder: *builder,
		cache: *cache, localSite: *localSite, cloudSite: *cloudSite,
		kernelImage: *kernelImage, cephUser: *cephUser, cephKey: *cephKeyring,
		cacheListen: *cacheListen, cacheGuest: *cacheGuest, out: *out,
	}
	if setFlags["config"] {
		in.cfgPath = *cfgPath
	}
	files[HybridRunbookFile] = renderHybridRunbook(in, p, trusted, carried)

	written, beside, err := writeHybridFiles(*out, files, *force)
	if err != nil {
		return err
	}

	report := os.Stdout
	fmt.Fprintf(report, "Wrote the %s render of the hybrid shape into %s:\n", hybridRenderName(p), *out)
	for _, f := range written {
		fmt.Fprintf(report, "  %s\n", f)
	}
	if len(beside) > 0 {
		fmt.Fprintf(report, "\nThese already existed and were NOT billet's own, so the fresh generation is "+
			"beside each at <file>.new; diff and merge deliberately, or re-run with --force:\n")
		for _, f := range beside {
			fmt.Fprintf(report, "  %s\n", f)
		}
	}
	if refNote != "" {
		fmt.Fprintf(report, "\n%s\n", refNote)
	}

	fmt.Fprintf(report, "\n%s\n", hybridNext(in, p, carried))

	return nil
}

// parseRequiredMemory parses a required byte-size flag by name.
func parseRequiredMemory(name, value string) (config.ByteSize, error) {
	if value == "" {
		return 0, fmt.Errorf("%s is required, e.g. 64GiB", name)
	}

	size, err := config.ParseByteSize(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}

	return size, nil
}

// hybridShapes resolves --instance-type entries: a bare TYPE is fetched from
// AWS (vcpu and memory always; the price unless --price overrides it), a
// TYPE=vcpu,memory,usd is declared and needs no credentials -- which is what
// a gate on a machine with no AWS account, and an operator who has already
// audited a shape, both need. The two forms are not mixed, so the fetch
// either happens for every shape or for none.
func hybridShapes(
	ctx context.Context, region string, raw, priceOverrides []string,
) ([]config.EC2InstanceType, error) {
	if len(raw) == 0 {
		return nil, errors.New("--instance-type is required at least once: billet ships no table of " +
			"EC2 shapes, so it must be told which ones the fallback may buy")
	}

	declared := 0
	for _, entry := range raw {
		if strings.Contains(entry, "=") {
			declared++
		}
	}

	switch {
	case declared == 0:
		overrides, err := parsePriceOverrides(priceOverrides)
		if err != nil {
			return nil, err
		}

		return resolveEC2Shapes(ctx, region, raw, overrides)

	case declared != len(raw):
		return nil, errors.New("--instance-type mixes fetched (TYPE) and declared (TYPE=vcpu,memory,usd) " +
			"shapes; use one form for every shape, so the fetch either happens for all or for none")
	}

	if len(priceOverrides) > 0 {
		return nil, errors.New("--price overrides a FETCHED price; a declared shape already carries its own")
	}

	shapes := make([]config.EC2InstanceType, 0, len(raw))
	seen := make(map[string]bool, len(raw))

	for _, entry := range raw {
		name, spec, _ := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("--instance-type %q names no shape", entry)
		}
		if seen[name] {
			return nil, fmt.Errorf("--instance-type %q is listed twice", name)
		}
		seen[name] = true

		fields := strings.Split(spec, ",")
		if len(fields) != 3 {
			return nil, fmt.Errorf("--instance-type %q needs vcpu,memory,usd — all three, because a "+
				"shape smaller than the lease chosen for it overcommits a budget nobody can see", entry)
		}

		shape, err := computeShape(name, fields)
		if err != nil {
			return nil, fmt.Errorf("--instance-type %q: %w", entry, err)
		}

		shapes = append(shapes, shape)
	}

	return shapes, nil
}

// hybridRef is the release every generated layer pins, and a note when this
// binary is not one.
//
// A DEVELOPMENT BUILD PINS main, said out loud: the role refuses `latest` and
// a moving target is what a pin exists to exclude, so the generation from a
// build that is not a release names what such a build is.
func hybridRef() (string, string) {
	v := version.Version()
	if version.IsRelease(v) {
		return v, ""
	}

	return "main", fmt.Sprintf("NOTE: this billet is %s, not a release, so the module, the collection "+
		"and billet_version are pinned to main. Replace every `main` with one vX.Y.Z before applying: "+
		"a moving target makes a converge non-deterministic.", v)
}

// hybridRenderName says which of the three renders this was.
func hybridRenderName(p initconfig.HybridParams) string {
	switch {
	case p.Commission:
		return "commission"
	case p.Facts != nil:
		return "prepare"
	}

	return "plan"
}

// writeHybridFiles lands every file under dir by the marker rule, returning
// what was written and what went beside an operator's own file.
func writeHybridFiles(dir string, files map[string]string, force bool) ([]string, []string, error) {
	var written, beside []string

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, rel := range names {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, nil, fmt.Errorf("create the directory for %s: %w", path, err)
		}

		existing, err := os.ReadFile(path)
		switch {
		case err == nil && !force && !hybridOwned(existing):
			// AN OPERATOR'S FILE IS NEVER REPLACED. Beside it, exclusive, so a
			// previous run's .new holding a half-finished merge is not truncated.
			side := path + ".new"
			f, err := os.OpenFile(side, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				if errors.Is(err, os.ErrExist) {
					return nil, nil, fmt.Errorf("%s already exists — it may hold a merge in progress. "+
						"Finish or remove it, then re-run", side)
				}

				return nil, nil, fmt.Errorf("create %s: %w", side, err)
			}
			if _, err := f.WriteString(files[rel]); err != nil {
				_ = f.Close()

				return nil, nil, fmt.Errorf("write %s: %w", side, err)
			}
			if err := f.Close(); err != nil {
				return nil, nil, fmt.Errorf("write %s: %w", side, err)
			}

			beside = append(beside, side)

		case err == nil || errors.Is(err, os.ErrNotExist):
			if err := commitConfig(path, []byte(files[rel]), 0o644); err != nil {
				return nil, nil, err
			}

			written = append(written, path)

		default:
			return nil, nil, fmt.Errorf("check %s: %w", path, err)
		}
	}

	return written, beside, nil
}

// hybridOwned reports whether a file's first line carries the marker: billet
// wrote it, and a re-run may replace it.
func hybridOwned(raw []byte) bool {
	first, _, _ := strings.Cut(string(raw), "\n")

	return strings.Contains(first, initconfig.HybridMarker)
}

// hybridFlags rebuilds the flags that describe this deployment, canonically,
// from the parsed values -- never from argv, for the reason generationFlags
// gives.
func hybridFlags(in hybridInputs) []string {
	var out []string

	add := func(name, value string) {
		if value != "" {
			out = append(out, "--"+name, value)
		}
	}
	addEach := func(name string, values []string) {
		for _, v := range values {
			out = append(out, "--"+name, v)
		}
	}

	add("name", in.name)
	add("region", in.region)
	add("org", in.org)
	add("runner-group", in.runnerGroup)
	addEach("workflow", in.workflows)
	add("config", in.cfgPath)
	add("control-plane-private-ip", in.controlPlaneIP)
	add("controller-name", in.controllerName)
	add("local-name", in.localName)
	out = append(out, "--local-vcpu", strconv.Itoa(in.localVCPU))
	add("local-memory", in.localMemory)
	out = append(out, "--max-vcpu", strconv.Itoa(in.cloudVCPU))
	add("max-memory", in.cloudMemory)
	addEach("instance-type", in.instanceTypes)
	addEach("price", in.priceOverrides)
	addEach("ssh-ingress-cidr", in.sshIngress)
	add("key-name", in.keyName)
	add("local-ansible-user", in.localUser)
	add("local-image", in.localImage)
	if in.builder {
		out = append(out, "--builder")
	}
	if in.cache {
		out = append(out, "--cache")
	}
	add("local-site", in.localSite)
	add("cloud-site", in.cloudSite)
	add("kernel-image", in.kernelImage)
	add("ceph-user", in.cephUser)
	add("ceph-keyring", in.cephKey)
	add("cache-listen", in.cacheListen)
	add("cache-guest-endpoint", in.cacheGuest)

	return out
}

// hybridNext is what to do after this render, on stdout.
func hybridNext(in hybridInputs, p initconfig.HybridParams, carried bool) string {
	switch {
	case p.Commission:
		return "Next: converge the controller alone, then the local host alone, and run `billet check` " +
			"on each. RUNBOOK.md, from step 7."
	case p.Facts != nil:
		return "Next: converge the controller alone with the App key (prepare-only), issue both " +
			"certificates on it, build the AMI, then the commission render. RUNBOOK.md, from step 4."
	}

	var b strings.Builder
	if !carried {
		b.WriteString("The App ids are zero. Mint the App first and generate again with --config " +
			"pointing at the file it wrote (RUNBOOK.md, step 1).\n\n")
	}
	tf := filepath.Join(in.out, "terraform")
	outputs := filepath.Join(in.out, "outputs.json")
	b.WriteString("Next: apply the Terraform root, save its outputs, and render the prepare phase:\n")
	fmt.Fprintf(&b, "  terraform -chdir=%s init && terraform -chdir=%s apply\n", shellArg(tf), shellArg(tf))
	fmt.Fprintf(&b, "  terraform -chdir=%s output -json > %s\n", shellArg(tf), shellArg(outputs))
	fmt.Fprintf(&b, "  billet init hybrid --out %s %s --terraform-output %s\n",
		shellArg(in.out), shellArgs(hybridFlags(in)), shellArg(outputs))
	fmt.Fprintf(&b, "%s has the whole order.", shellArg(filepath.Join(in.out, HybridRunbookFile)))

	return b.String()
}

// renderHybridRunbook is the ordered list of what to run between the files,
// with every command spelled out for THIS generation.
//
// THE ORDER IS THE ROLE'S, NOT A PREFERENCE. The controller is prepared before
// anything mints an identity, because the ledger volume has to be mounted and
// proved first; certificates are issued on the prepared host, because the node
// configs name bundles the role's billet check refuses to find missing; the AMI
// is built wherever the credentials for it are, which is a workstation unless
// --builder put the grant on the controller; and the local host is converged
// last, alone, because its certificate has to be installed by hand first.
func renderHybridRunbook(in hybridInputs, p initconfig.HybridParams, trusted, carried bool) string {
	// EVERY COMMAND HERE RUNS FROM THE DIRECTORY THIS FILE IS IN, so paths are
	// relative to it and the same runbook is right wherever the operator put
	// the generation.
	dir := "."
	tf := "terraform"
	flags := shellArgs(hybridFlags(in))
	// THE ADDRESS A GUEST DIALS, named here because the cache's certificate has
	// to be valid for it and an operator reading the runbook needs the string
	// rather than a description of it.
	cacheAddress := p.ControlPlaneIP
	if cacheAddress == "" {
		cacheAddress = "the control_plane_private_ip output"
	}

	wire := fmt.Sprintf("%s:%d", p.ControlPlaneIP, 7717)
	if p.ControlPlaneIP == "" {
		wire = "the control_plane_private_ip output, port 7717"
	}

	orgFlag := shellArg("<your-org>")
	if p.Org != "" {
		orgFlag = shellArg(p.Org)
	}

	trust := "`trust: untrusted`: a fork's pull request runs in a Firecracker guest on the untrusted bridge at home, or on its own EC2 instance in the untrusted security group. Add a trusted tier only with a runner group and workflow allowlist."
	if trusted {
		trust = "`trust: trusted`: only the workflows you allowlisted, from repositories your runner group permits, reach them. Launch authority is the tier's static trust, so an allowlisted workflow must never check out or run code you do not control."
	}

	var b strings.Builder

	fmt.Fprintf(&b, "<!-- %s for the %s deployment -->\n", initconfig.HybridMarker, p.Name)
	fmt.Fprintf(&b, "# Bringing up the %s hybrid deployment\n\n", p.Name)
	b.WriteString("One `runs-on` label means the machine at home if it is up and EC2 if it is not, with a small always-on controller. These are the steps between the files billet wrote, in the order they depend on each other, and every command runs from this directory. Every converge names one host with `-l`, because a bare run reaching a host before its certificate exists fails at `billet check` after a wasted drain.\n\n")
	fmt.Fprintf(&b, "The tiers are %s\n\n", trust)

	b.WriteString("## 1. Mint the GitHub App\n\n")
	if carried {
		b.WriteString("Done: the App identity was carried into the inventory from the config you pointed `--config` at. Do not create it again.\n\n")
	} else {
		b.WriteString("The App ids in the inventory are zero, and the role refuses a config that carries a zero `app_id` before anything reaches GitHub. Mint one into a file that holds nothing else, then generate again against it:\n\n")
		fmt.Fprintf(&b, "```bash\n(set -C; printf '%%s\\n' %s > %s)\nbillet github-app create --org %s --config %s\nbillet init hybrid --out %s %s --config %s\n```\n\n",
			shellArg(bootstrapSeed), bootstrapIdentity, orgFlag, bootstrapIdentity, shellArg(dir), flags, bootstrapIdentity)
		b.WriteString("`set -C` is why the first command refuses an existing file rather than truncating it: after a successful run it is the only local record of the App id, installation id and key path.\n\n")
	}

	b.WriteString("## 2. Apply the Terraform root\n\n")
	fmt.Fprintf(&b, "```bash\nterraform -chdir=%s init\nterraform -chdir=%s apply\nterraform -chdir=%s output -json > outputs.json\n```\n\n", tf, tf, tf)
	b.WriteString("It creates the controller with its retained ledger volume and auto-recovery, the fleet's node role, the backup bucket whose grant lands on that role, the AMI payload bucket")
	if !trusted {
		b.WriteString(", and the untrusted runner security group")
	}
	b.WriteString(". Read the plan: the ledger volume carries `prevent_destroy`, and `go run ./scripts/tfclassify` says what a later change costs a running deployment.\n\n")

	b.WriteString("## 3. Render the prepare phase\n\n")
	fmt.Fprintf(&b, "```bash\nbillet init hybrid --out %s %s --terraform-output outputs.json\n```\n\n", shellArg(dir), flags)
	b.WriteString("Every `<terraform output …>` placeholder in `inventory.yml` is filled. The controller entry stays `billet_server_prepare_only: true` and server-only: its ec2 node's certificate does not exist yet.\n\n")

	b.WriteString("## 4. Prepare the controller, with the App key\n\n")
	if p.SSHKeyName != "" {
		fmt.Fprintf(&b, "The controller launched with the %s key pair, so Ansible's ordinary SSH works as `ubuntu` once port 22 is reachable over the route you chose (docs/deploying/reaching-hosts.md).\n\n", shellArg(p.SSHKeyName))
	} else {
		b.WriteString("The controller launched with NO key pair, so a fresh image carries no operator key. Push one for sixty seconds with EC2 Instance Connect before each converge, over the route you chose (docs/deploying/reaching-hosts.md); or generate again with `--key-name` for ordinary SSH:\n\n")
		fmt.Fprintf(&b, "```bash\naws ec2-instance-connect send-ssh-public-key --region %s \\\n  --instance-id \"$(terraform -chdir=%s output -raw control_plane_instance_id)\" \\\n  --instance-os-user ubuntu --ssh-public-key file://~/.ssh/id_ed25519.pub\n```\n\n", shellArg(p.Region), tf)
	}
	fmt.Fprintf(&b, "```bash\nansible-galaxy collection install -r requirements.yml\nBILLET_GITHUB_PRIVATE_KEY_PATH=<the key github-app create wrote> \\\n  ansible-playbook -i inventory.yml site.yml -l %s\n```\n\n", shellArg(p.ControllerName))
	b.WriteString("The role demands the key because the config names it, and `billet_server_prepare_only` does not gate that refusal. Prepare-only mounts and proves the ledger volume, installs the binary and the units, and HOLDS both services, so nothing can mint a deployment identity yet.\n\n")

	b.WriteString("## 5. Issue both certificates on the controller\n\n")
	fmt.Fprintf(&b, "As the service user, so the identity and CA this mints on the ledger volume are owned by the account that will serve them:\n\n```bash\nsudo -u billet billet ca issue %s --config /etc/billet/billet.yaml --out /var/lib/billet/%s-tls\nsudo -u billet billet ca issue %s --config /etc/billet/billet.yaml --out /var/lib/billet/%s-tls\n```\n\n",
		shellArg(p.ControllerName), shellArg(p.ControllerName), shellArg(p.LocalName), shellArg(p.LocalName))
	fmt.Fprintf(&b, "Install the controller's own bundle root-owned at `/etc/billet/tls` (`node.crt`, `ca.crt` 0644, `node.key` 0600): `billet-node.service` runs as root and rewrites the bundle at renewal. Stream the local host's bundle host-to-host to its `/etc/billet/tls`, so the key never lands on a laptop. Both configs name exactly these paths, and dial %s.\n\n", wire)

	b.WriteString("## 6. Build the AMI\n\n")

	// THE IMAGE HAS TO TRUST THE CACHE BEFORE IT IS BUILT, which is why the
	// issuer is asked for here rather than beside the listener's own pair in
	// step 7.
	//
	// A guest's cache client speaks HTTPS to an endpoint on the controller's
	// PRIVATE address, so its certificate comes from a private issuer and no
	// public root signs it. `ami build --ca-cert` is what puts that issuer in the
	// image's host trust store. Without it the cache is not broken in any way an
	// operator sees: every request fails its TLS handshake, every job falls back
	// to a cold fetch, and the bucket, the listener, the site and the security
	// group rule this generation created are all correct and all unused.
	//
	// The AMI is what carries the anchor, so a re-issued CA means a rebuilt
	// image; a long-lived issuer is worth the trouble here.
	caCert := ""

	if p.Cache {
		caCert = " \\\n  --ca-cert cache-ca.pem"

		fmt.Fprintf(&b, "This generation has a cloud cache, so the image must trust the listener's issuer. The endpoint is on a private address (%s) and its certificate is signed by an issuer of yours, which no public root chains to — so `--ca-cert` below takes the PEM of that issuer and bakes it into the image's host trust store. Skip it and nothing reports an error: every guest's cache request fails its TLS handshake, every job fetches cold, and the bucket, listener and security-group rule this generation created go unused. Have the same issuer that will sign the pair in step 7, and put its PEM beside the command as `cache-ca.pem`.\n\n",
			cacheAddress)
	}

	// THE COMMAND HAS TO RUN WHERE THE CREDENTIALS ARE, and the two cases put
	// that in different places. A workstation build reads the three values out
	// of the Terraform state it has; a controller build has neither that
	// directory nor that state, so its command carries the values literally —
	// which is only possible once the apply has produced them.
	if p.Builder && p.Facts != nil {
		b.WriteString("**On the controller**, which carries the builder grant this generation asked for, so no machine outside the deployment needs AWS credentials. Run it over whichever route you converge with; the values are written out here because the controller has no copy of the Terraform state")

		if p.Cache {
			b.WriteString(", and `cache-ca.pem` has to be on the controller too, since that is where the command runs")
		}

		b.WriteString(":\n\n")
		fmt.Fprintf(&b, "```bash\nbillet ami build --region %s \\\n  --subnet %s \\\n  --security-group %s \\\n  --payload-bucket %s%s \\\n  --public-ip --base-image ami-<an EBS-backed Ubuntu 24.04 image in %s>\n```\n\n",
			shellArg(p.Region),
			shellArg(p.Facts.SubnetID),
			shellArg(p.Facts.RunnerSecurityGroupID),
			shellArg(p.Facts.AMIPayloadBucket),
			caCert,
			p.Region)
	} else {
		if p.Builder {
			b.WriteString("This generation asked for the builder grant, so once the apply has run the prepare render prints a command to run **on the controller** instead of this one. Until then, from a workstation with your own AWS credentials:\n\n")
		} else {
			b.WriteString("From a workstation with your own AWS credentials: the node role carries no builder grant. Generate with `--builder` to move this onto the controller instead.\n\n")
		}
		fmt.Fprintf(&b, "```bash\nbillet ami build --region %s \\\n  --subnet \"$(terraform -chdir=%s output -raw subnet_id)\" \\\n  --security-group \"$(terraform -chdir=%s output -raw runner_security_group_id)\" \\\n  --payload-bucket \"$(terraform -chdir=%s output -raw ami_payload_bucket)\"%s \\\n  --public-ip --base-image ami-<an EBS-backed Ubuntu 24.04 image in %s>\n```\n\n",
			shellArg(p.Region), tf, tf, tf, caCert, p.Region)
	}
	b.WriteString("Pass `--public-ip`: the created subnet's only route is an internet gateway, which is unusable without an address. The command boots the image it made and stamps it only after it proved itself.\n\n")

	b.WriteString("## 7. Render the commission phase, and converge both hosts\n\n")
	if p.Cache {
		// THE LISTENER APPEARS IN THIS RENDER, so its pair has to exist before the
		// converge that installs it: the role copies whatever the two variables
		// name to the paths the config carries, and refuses half a pair.
		fmt.Fprintf(&b, "This render adds the cloud cache, so the converge below needs the LEAF of the pair whose issuer step 6 baked into the image: the orchestrator terminates HTTPS for job instances that fetch across the VPC, and the certificate has to be valid for the address a guest dials (%s) and be signed by that same `cache-ca.pem`, or the anchor in the image matches nothing. Hand it over the way the App key was handed over, and the role installs it to the paths the config names:\n\n```bash\nexport BILLET_CACHE_TLS_CERT_PATH=<your cert.pem>\nexport BILLET_CACHE_TLS_KEY_PATH=<your key.pem>\n```\n\n",
			cacheAddress)
	}
	fmt.Fprintf(&b, "```bash\nbillet init hybrid --out %s %s --terraform-output outputs.json --commission --ami ami-<from step 6>\nansible-playbook -i inventory.yml site.yml -l %s\nansible-playbook -i inventory.yml site.yml -l %s\n```\n\n",
		shellArg(dir), flags, shellArg(p.ControllerName), shellArg(p.LocalName))
	b.WriteString("No key this time: the protected copy on the controller is enough. The commission render lifts the hold, adds the ec2 orchestrator beside the server, and writes the AMI into every tier's `launch.ec2.image`. The local host converges last, alone, once its bundle from step 5 is in place. Then `billet check --config /etc/billet/billet.yaml` on each.\n\n")

	b.WriteString("## 8. Prove the fallback\n\n")
	b.WriteString("Run a job on the label. Withdraw the local host (`billet drain` on it), run the same job again and watch it land on EC2 under the unchanged label, then confirm every instance disappeared. That exact sequence has completed against real GitHub and AWS.\n\n")

	b.WriteString("## 9. Rehearse recovery\n\n")
	b.WriteString("`billet local backup` on the controller writes the ledger, the deployment identity, the node-wire CA and the App key as one unit to the backup bucket. Restore it onto a machine holding nothing but the binary with `billet local restore --from-backup latest` before you call the deployment recoverable.\n")

	return b.String()
}
