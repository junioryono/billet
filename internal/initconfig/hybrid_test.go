package initconfig

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"

	"gopkg.in/yaml.v3"
)

func hybridParams() HybridParams {
	return HybridParams{
		Name:           "acme-ci",
		Region:         "us-west-2",
		Org:            "acme",
		ControllerName: "acme-ci-control-plane",
		LocalName:      "acme-ci-fc-1",
		LocalVCPU:      32,
		LocalMemory:    128 * config.GiB,
		CloudVCPU:      16,
		CloudMemory:    32 * config.GiB,
		Shapes:         ec2Shapes(),
		Ref:            "v0.5.0",
	}
}

func hybridFacts() *HybridFacts {
	return &HybridFacts{
		ControlPlanePrivateIP:          "10.60.0.10",
		LedgerVolumeID:                 "vol-0abc",
		SubnetID:                       "subnet-0abc",
		RunnerSecurityGroupID:          "sg-trusted",
		UntrustedRunnerSecurityGroupID: "sg-untrusted",
		AMIPayloadBucket:               "acme-ci-ami-payloads-123456789012",
	}
}

func mustGenerateHybrid(t *testing.T, p HybridParams) (HybridFiles, bool) {
	t.Helper()

	files, trusted, err := GenerateHybrid(p)
	if err != nil {
		t.Fatalf("GenerateHybrid: %v", err)
	}

	return files, trusted
}

// inventoryHosts parses the rendered inventory the way Ansible does and returns
// each host's variables, with the YAML anchor already expanded.
func inventoryHosts(t *testing.T, inventory string) map[string]map[string]any {
	t.Helper()

	var doc struct {
		All struct {
			Children map[string]struct {
				Hosts map[string]map[string]any `yaml:"hosts"`
			} `yaml:"children"`
		} `yaml:"all"`
	}
	if err := yaml.Unmarshal([]byte(inventory), &doc); err != nil {
		t.Fatalf("the inventory is not YAML: %v\n%s", err, inventory)
	}

	hosts := map[string]map[string]any{}
	for _, group := range doc.All.Children {
		for name, vars := range group.Hosts {
			hosts[name] = vars
		}
	}

	return hosts
}

// hostConfig re-encodes a host's billet_config and parses it as billet would,
// with the one stand-in a plan render needs.
func hostConfig(t *testing.T, vars map[string]any) *config.Config {
	t.Helper()

	raw, err := yaml.Marshal(vars["billet_config"])
	if err != nil {
		t.Fatalf("re-encode billet_config: %v", err)
	}

	body := strings.ReplaceAll(string(raw), HybridPlaceholder(HybridOutputControlPlanePrivateIP), "192.0.2.10")
	body = strings.Replace(body, "app_id: 0\n", "app_id: 1\n", 1)
	body = strings.Replace(body, "installation_id: 0\n", "installation_id: 1\n", 1)

	cfg, err := config.Parse("inventory billet_config", []byte(body))
	if err != nil {
		t.Fatalf("the host's billet_config does not load: %v\n%s", err, raw)
	}

	return cfg
}

// THE PLAN RENDER IS THE SHAPE BILLET WAS BUILT FOR, and every host's config
// loads through the same parser a file on disk takes.
func TestGenerateHybridPlanRenderLoadsOnBothHosts(t *testing.T) {
	t.Parallel()

	files, trusted := mustGenerateHybrid(t, hybridParams())
	if trusted {
		t.Fatal("no policy must render untrusted tiers")
	}

	for _, name := range []string{HybridTerraformFile, HybridInventoryFile, HybridSiteFile, HybridRequirementsFile} {
		body, ok := files[name]
		if !ok {
			t.Fatalf("no %s in the generation", name)
		}
		if first, _, _ := strings.Cut(body, "\n"); !strings.Contains(first, HybridMarker) {
			t.Errorf("%s does not open with the marker a re-run replaces on: %q", name, first)
		}
	}

	hosts := inventoryHosts(t, files[HybridInventoryFile])
	if len(hosts) != 2 {
		t.Fatalf("the inventory carries %d hosts, want the controller and the local host", len(hosts))
	}

	controller := hostConfig(t, hosts["acme-ci-control-plane"])
	local := hostConfig(t, hosts["acme-ci-fc-1"])

	// THE CONTROLLER: a server, the App, the off-site copy, no node yet.
	if controller.Server == nil || controller.Server.Listen != "192.0.2.10:7717" {
		t.Errorf("the controller must listen on the declared address, got %+v", controller.Server)
	}
	if controller.Backup == nil || controller.Backup.S3 == nil ||
		controller.Backup.S3.Bucket != "acme-ci-backups" || controller.Backup.S3.Prefix != hybridBackupPrefix ||
		controller.Backup.S3.Region != "us-west-2" {
		t.Errorf("the controller's backup block must name the root's composed bucket, prefix and region, got %+v",
			controller.Backup)
	}
	if controller.Node != nil {
		t.Errorf("the plan render must not carry the ec2 node; its certificate bundle does not exist yet")
	}
	if controller.Server.MaxVCPU != CeilingVCPU(32)+16 {
		t.Errorf("the deployment ceiling must be the local contribution plus the cloud budget, got %d",
			controller.Server.MaxVCPU)
	}

	// THE LOCAL HOST: a node only, with a certificate, and NEITHER server nor
	// github -- both absences are load-bearing.
	if local.Server != nil {
		t.Error("the local host must carry no server block: a certless node beside one mints the identity")
	}
	if local.GitHub != nil {
		t.Error("the local host must carry no github block: the App key must not reach the host running untrusted code")
	}
	if local.Node == nil || local.Node.Provider != config.ProviderFirecracker || local.Node.TLS == nil {
		t.Fatalf("the local host must be a firecracker node with a certificate bundle, got %+v", local.Node)
	}
	if local.Node.ServerAddr != controller.Server.Listen {
		t.Errorf("the local node dials %q but the controller listens on %q", local.Node.ServerAddr, controller.Server.Listen)
	}
	if local.Node.MaxVCPU != CeilingVCPU(32) {
		t.Errorf("the local contribution must be the host minus headroom, got %d", local.Node.MaxVCPU)
	}
	if local.Node.Firecracker == nil || local.Node.Firecracker.UntrustedBridge == "" {
		t.Error("an untrusted generation's firecracker node needs the untrusted bridge")
	}

	// ONE CATALOGUE, ON BOTH HOSTS, THROUGH THE ANCHOR.
	if len(controller.Tiers) == 0 || len(controller.Tiers) != len(local.Tiers) {
		t.Fatalf("both hosts must carry the same catalogue: %d vs %d tiers", len(controller.Tiers), len(local.Tiers))
	}
	for i, tier := range controller.Tiers {
		if !slices.Equal(tier.Providers, []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2}) {
			t.Errorf("tier %s: providers %v, want firecracker then ec2", tier.Label, tier.Providers)
		}
		fc, ok := tier.Launch[config.ProviderFirecracker]
		if !ok || fc.Image != DefaultFirecrackerImage {
			t.Errorf("tier %s: launch.firecracker must name the verified generation, got %+v", tier.Label, fc)
		}
		ec2, ok := tier.Launch[config.ProviderEC2]
		if !ok || ec2.Image != PlaceholderAMI || !slices.Equal(ec2.Command, []string{hybridRunnerCommand}) {
			t.Errorf("tier %s: launch.ec2 must carry the placeholder AMI and the runner command, got %+v", tier.Label, ec2)
		}
		if tier.Trust != config.WorkloadUntrusted {
			t.Errorf("tier %s: trust %q, want untrusted", tier.Label, tier.Trust)
		}
		if tier.Label != local.Tiers[i].Label {
			t.Errorf("tier %d differs between hosts: %q vs %q", i, tier.Label, local.Tiers[i].Label)
		}
	}
}

// EVERY PLACEHOLDER NAMES AN OUTPUT THE ROOT DECLARES. A placeholder for an
// output that does not exist is a phase 2 that can never fill it; an output
// nobody consumes is noise, so both directions are checked for the consumed
// set.
func TestGenerateHybridPlaceholdersNameDeclaredOutputs(t *testing.T) {
	t.Parallel()

	files, _ := mustGenerateHybrid(t, hybridParams())

	declared := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^output "([a-z_]+)"`).FindAllStringSubmatch(files[HybridTerraformFile], -1) {
		declared[m[1]] = true
	}

	used := HybridPlaceholders(files[HybridInventoryFile])
	if len(used) == 0 {
		t.Fatal("the plan render carries no placeholder at all, so nothing here is examined")
	}
	for _, name := range used {
		if !declared[name] {
			t.Errorf("the inventory waits for output %q, which terraform/main.tf does not declare", name)
		}
	}

	// The facts ParseTerraformOutput demands are declared too, even where the
	// inventory does not spell them (the payload bucket is the runbook's).
	for _, name := range []string{
		HybridOutputControlPlanePrivateIP, HybridOutputLedgerVolumeID, HybridOutputSubnetID,
		HybridOutputRunnerSecurityGroup, HybridOutputUntrustedRunnerSG, HybridOutputAMIPayloadBucket,
	} {
		if !declared[name] {
			t.Errorf("terraform/main.tf does not declare %q, which the prepare render demands", name)
		}
	}
}

// A DECLARED ADDRESS IS WRITTEN EVERYWHERE BEFORE THE APPLY, which is the
// point of declaring it, and reaches the root as the input that pins it.
func TestGenerateHybridDeclaredAddressReachesEverySite(t *testing.T) {
	t.Parallel()

	p := hybridParams()
	p.ControlPlaneIP = "10.60.0.10"
	files, _ := mustGenerateHybrid(t, p)

	if !strings.Contains(files[HybridTerraformFile], `control_plane_private_ip = "10.60.0.10"`) {
		t.Error("the root must declare the address to the module")
	}

	hosts := inventoryHosts(t, files[HybridInventoryFile])
	controller := hostConfig(t, hosts["acme-ci-control-plane"])
	local := hostConfig(t, hosts["acme-ci-fc-1"])
	if controller.Server.Listen != "10.60.0.10:7717" || local.Node.ServerAddr != "10.60.0.10:7717" {
		t.Errorf("listen %q and server_addr %q must both be the declared address",
			controller.Server.Listen, local.Node.ServerAddr)
	}
	if hosts["acme-ci-control-plane"]["ansible_host"] != "10.60.0.10" {
		t.Errorf("ansible_host must be the declared address, got %v", hosts["acme-ci-control-plane"]["ansible_host"])
	}
	if slices.Contains(HybridPlaceholders(files[HybridInventoryFile]), HybridOutputControlPlanePrivateIP) {
		t.Error("a declared address must leave no placeholder for it")
	}
}

// THE COMMISSION RENDER CARRIES THE NODE, THE AMI AND NO PLACEHOLDER.
func TestGenerateHybridCommissionRenderIsComplete(t *testing.T) {
	t.Parallel()

	p := hybridParams()
	p.Facts = hybridFacts()
	p.Commission = true
	p.AMI = "ami-0123456789abcdef0"
	p.AppID, p.InstallationID, p.ClientID = 7, 9, "Iv1.abc"
	files, _ := mustGenerateHybrid(t, p)

	if left := HybridPlaceholders(files[HybridInventoryFile]); len(left) > 0 {
		t.Fatalf("a filled render still waits for %v", left)
	}

	hosts := inventoryHosts(t, files[HybridInventoryFile])
	vars := hosts["acme-ci-control-plane"]
	if vars["billet_server_prepare_only"] != false || vars["billet_enable_node"] != true {
		t.Errorf("a commissioned controller must lift the hold and enable the node, got prepare_only=%v node=%v",
			vars["billet_server_prepare_only"], vars["billet_enable_node"])
	}
	if vars["billet_ledger_volume_id"] != "vol-0abc" {
		t.Errorf("the ledger volume id must be filled, got %v", vars["billet_ledger_volume_id"])
	}

	controller := hostConfig(t, vars)
	if controller.Node == nil || controller.Node.Provider != config.ProviderEC2 || controller.Node.TLS == nil {
		t.Fatalf("the commissioned controller must carry the ec2 orchestrator with a certificate, got %+v", controller.Node)
	}
	if controller.Node.EC2.SubnetID != "subnet-0abc" ||
		!slices.Equal(controller.Node.EC2.SecurityGroupIDs, []string{"sg-trusted"}) ||
		!slices.Equal(controller.Node.EC2.UntrustedSecurityGroupIDs, []string{"sg-untrusted"}) {
		t.Errorf("the ec2 placement must come from the facts, got %+v", controller.Node.EC2)
	}
	if controller.Node.MaxVCPU != 16 {
		t.Errorf("the orchestrator's contribution must be the cloud budget, got %d", controller.Node.MaxVCPU)
	}
	if controller.GitHub.AppID != 7 || controller.GitHub.InstallationID != 9 || controller.GitHub.ClientID != "Iv1.abc" {
		t.Errorf("the carried App identity must be written, got %+v", controller.GitHub)
	}
	for _, tier := range controller.Tiers {
		if tier.Launch[config.ProviderEC2].Image != "ami-0123456789abcdef0" {
			t.Errorf("tier %s: launch.ec2.image %q, want the AMI", tier.Label, tier.Launch[config.ProviderEC2].Image)
		}
	}
}

// THE PREPARE RENDER FILLS THE FACTS AND KEEPS THE HOLD: the certificate
// bundle the node needs does not exist until billet ca issue has run on the
// prepared host.
func TestGenerateHybridPrepareRenderKeepsTheHold(t *testing.T) {
	t.Parallel()

	p := hybridParams()
	p.Facts = hybridFacts()
	files, _ := mustGenerateHybrid(t, p)

	hosts := inventoryHosts(t, files[HybridInventoryFile])
	vars := hosts["acme-ci-control-plane"]
	if vars["billet_server_prepare_only"] != true || vars["billet_enable_node"] != false {
		t.Errorf("a prepared controller must keep the hold and no node, got prepare_only=%v node=%v",
			vars["billet_server_prepare_only"], vars["billet_enable_node"])
	}
	if vars["billet_ledger_volume_id"] != "vol-0abc" || vars["ansible_host"] != "10.60.0.10" {
		t.Errorf("the prepare render must fill the volume id and the address, got %v / %v",
			vars["billet_ledger_volume_id"], vars["ansible_host"])
	}
	if controller := hostConfig(t, vars); controller.Node != nil {
		t.Error("the prepare render must not carry the node")
	}
}

// A TRUSTED GENERATION NEEDS NO UNTRUSTED GROUP AND NO UNTRUSTED OUTPUT, and an
// untrusted one needs both: the root and the inventory have to agree on which.
func TestGenerateHybridTrustDecidesTheUntrustedSurface(t *testing.T) {
	t.Parallel()

	untrusted, _ := mustGenerateHybrid(t, hybridParams())
	if !strings.Contains(untrusted[HybridTerraformFile], `resource "aws_security_group" "untrusted_runner"`) ||
		!strings.Contains(untrusted[HybridTerraformFile], `output "`+HybridOutputUntrustedRunnerSG+`"`) {
		t.Error("an untrusted generation must create and output the untrusted runner group")
	}

	p := hybridParams()
	p.RunnerGroup, p.Workflows = "billet-trusted", []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"}
	p.Facts = hybridFacts()
	p.Commission = true
	p.AMI = "ami-0123456789abcdef0"
	trusted, isTrusted := mustGenerateHybrid(t, p)
	if !isTrusted {
		t.Fatal("a policy must render trusted tiers")
	}
	if strings.Contains(trusted[HybridTerraformFile], "untrusted_runner") {
		t.Error("a trusted generation must not create an untrusted runner group nobody launches into")
	}

	hosts := inventoryHosts(t, trusted[HybridInventoryFile])
	controller := hostConfig(t, hosts["acme-ci-control-plane"])
	if len(controller.Node.EC2.UntrustedSecurityGroupIDs) != 0 {
		t.Errorf("a trusted orchestrator must carry no untrusted group, got %v", controller.Node.EC2.UntrustedSecurityGroupIDs)
	}
	for _, tier := range controller.Tiers {
		if tier.Trust != config.WorkloadTrusted || tier.RunnerGroup != "billet-trusted" {
			t.Errorf("tier %s: %q in group %q, want trusted in billet-trusted", tier.Label, tier.Trust, tier.RunnerGroup)
		}
	}
}

// THE CATALOGUE FITS BOTH NODES. A shape that fits the cloud budget but not
// the local host's ceiling is dropped, because a tier the local host can never
// place is a label that always falls through to the cloud -- the opposite of
// what the order promises.
func TestGenerateHybridCatalogueFitsBothNodes(t *testing.T) {
	t.Parallel()

	p := hybridParams()
	p.LocalVCPU, p.LocalMemory = 8, 16*config.GiB // a ceiling of 6 vCPU: the 8-vCPU shape cannot fit
	files, _ := mustGenerateHybrid(t, p)

	hosts := inventoryHosts(t, files[HybridInventoryFile])
	controller := hostConfig(t, hosts["acme-ci-control-plane"])
	if len(controller.Tiers) != 1 || controller.Tiers[0].VCPU != 4 {
		t.Errorf("only the 4-vCPU tier fits a 6-vCPU local ceiling, got %+v", controller.Tiers)
	}

	p.LocalVCPU, p.LocalMemory = 2, 4*config.GiB
	if _, _, err := GenerateHybrid(p); err == nil || !strings.Contains(err.Error(), "no declared shape fits") {
		t.Errorf("a local host too small for any shape must be refused by name, got %v", err)
	}
}

// EVERY REFUSAL NAMES THE FLAG THAT CARRIED THE VALUE.
func TestGenerateHybridRefusals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		edit func(*HybridParams)
		want string
	}{
		{"a bad name", func(p *HybridParams) { p.Name = "Acme CI" }, "--name"},
		{"no region", func(p *HybridParams) { p.Region = "" }, "--region"},
		{"a bad region", func(p *HybridParams) { p.Region = "us west" }, "--region"},
		{"two hosts one name", func(p *HybridParams) { p.LocalName = p.ControllerName }, "--controller-name and --local-name"},
		{"a bad host name", func(p *HybridParams) { p.LocalName = "FC_1" }, "--local-name"},
		{"a bad address", func(p *HybridParams) { p.ControlPlaneIP = "10.60.0.10/32" }, "--control-plane-private-ip"},
		{"a bad cidr", func(p *HybridParams) { p.SSHIngressCIDRs = []string{"203.0.113.7"} }, "--ssh-ingress-cidr"},
		{"no local capacity", func(p *HybridParams) { p.LocalVCPU = 0 }, "--local-vcpu"},
		{"no budget", func(p *HybridParams) { p.CloudMemory = 0 }, "--max-vcpu and --max-memory"},
		{"no shapes", func(p *HybridParams) { p.Shapes = nil }, "--instance-type"},
		{"no ref", func(p *HybridParams) { p.Ref = "" }, "release"},
		{"commission without facts", func(p *HybridParams) { p.Commission = true }, "--commission needs --terraform-output"},
		{"commission without an ami", func(p *HybridParams) { p.Commission = true; p.Facts = hybridFacts() }, "--commission needs --ami"},
		{"an ipv6 cidr", func(p *HybridParams) { p.SSHIngressCIDRs = []string{"2001:db8::/32"} }, "not IPv4"},
		{"a cidr with host bits", func(p *HybridParams) { p.SSHIngressCIDRs = []string{"203.0.113.7/24"} }, "host bits"},
		{"a name longer than a label", func(p *HybridParams) { p.LocalName = strings.Repeat("a", 64) }, "--local-name"},
		{"a bad local user", func(p *HybridParams) { p.LocalAnsibleUser = "Root User" }, "--local-ansible-user"},
		{"a bad local image", func(p *HybridParams) { p.LocalImage = "ubuntu 2404" }, "--local-image"},
		{"a padded key name", func(p *HybridParams) { p.SSHKeyName = " my-key" }, "--key-name"},
		{"an ami before commission", func(p *HybridParams) { p.AMI = "ami-1" }, "--ami is read on the commission render"},
		{"a partial policy", func(p *HybridParams) { p.RunnerGroup = "billet-trusted" }, "--workflow"},
		{"half a cache", func(p *HybridParams) { p.Host.CacheListen = "172.31.0.1:7718" }, "--cache-listen and --cache-guest-endpoint"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := hybridParams()
			tc.edit(&p)

			_, _, err := GenerateHybrid(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, err)
			}
		})
	}
}

// THE SITE AND REQUIREMENTS PIN THE SAME RELEASE AS THE ROOT AND THE INVENTORY.
func TestGenerateHybridPinsOneRelease(t *testing.T) {
	t.Parallel()

	files, _ := mustGenerateHybrid(t, hybridParams())

	if !strings.Contains(files[HybridTerraformFile], "terraform/modules/billet?ref=v0.5.0") {
		t.Error("the root must pin the module to the release")
	}
	if !strings.Contains(files[HybridRequirementsFile], "version: v0.5.0") {
		t.Error("the collection must be pinned to the release")
	}
	if strings.Count(files[HybridInventoryFile], "billet_version: v0.5.0") != 2 {
		t.Error("both hosts must pin billet_version to the release")
	}

	var site []struct {
		Hosts string `yaml:"hosts"`
	}
	if err := yaml.Unmarshal([]byte(files[HybridSiteFile]), &site); err != nil {
		t.Fatalf("site.yml is not YAML: %v", err)
	}
	if len(site) != 2 || site[0].Hosts != "control_plane" || site[1].Hosts != "linux" {
		t.Errorf("site.yml must converge the control plane first, then the local host, got %+v", site)
	}
}

// ParseTerraformOutput demands every consumed output by name, and only the
// untrusted group when the generation is untrusted.
func TestParseTerraformOutput(t *testing.T) {
	t.Parallel()

	full := `{
  "control_plane_private_ip": {"sensitive": false, "type": "string", "value": "10.60.0.10"},
  "ledger_volume_id": {"sensitive": false, "type": "string", "value": "vol-0abc"},
  "subnet_id": {"sensitive": false, "type": "string", "value": "subnet-0abc"},
  "runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-trusted"},
  "untrusted_runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-untrusted"},
  "ami_payload_bucket": {"sensitive": false, "type": "string", "value": "acme-ci-ami-payloads-1"},
  "region": {"sensitive": false, "type": "string", "value": "us-west-2"}
}`

	got, err := ParseTerraformOutput([]byte(full), true)
	if err != nil {
		t.Fatalf("a complete output: %v", err)
	}
	want := HybridFacts{
		ControlPlanePrivateIP: "10.60.0.10", LedgerVolumeID: "vol-0abc", SubnetID: "subnet-0abc",
		RunnerSecurityGroupID: "sg-trusted", UntrustedRunnerSecurityGroupID: "sg-untrusted",
		AMIPayloadBucket: "acme-ci-ami-payloads-1",
	}
	if got != want {
		t.Errorf("facts %+v, want %+v", got, want)
	}

	missing := strings.Replace(full, `"ledger_volume_id"`, `"ledger_volume"`, 1)
	if _, err := ParseTerraformOutput([]byte(missing), true); err == nil || !strings.Contains(err.Error(), `"ledger_volume_id"`) {
		t.Errorf("a missing output must be refused by name, got %v", err)
	}

	noUntrusted := strings.Replace(full, `"untrusted_runner_security_group_id"`, `"other"`, 1)
	if _, err := ParseTerraformOutput([]byte(noUntrusted), false); err != nil {
		t.Errorf("a trusted generation must not demand the untrusted group: %v", err)
	}
	if _, err := ParseTerraformOutput([]byte(noUntrusted), true); err == nil {
		t.Error("an untrusted generation must demand the untrusted group")
	}

	empty := strings.Replace(full, `"value": "subnet-0abc"`, `"value": ""`, 1)
	if _, err := ParseTerraformOutput([]byte(empty), true); err == nil || !strings.Contains(err.Error(), `"subnet_id"`) {
		t.Errorf("an empty output must be refused by name, got %v", err)
	}

	if _, err := ParseTerraformOutput([]byte("not json"), true); err == nil {
		t.Error("garbage must be refused")
	}
}

// THE HOST-SIDE FACTS THE GENERATOR CANNOT SEE ARE INPUTS, NOT ASSUMPTIONS: the
// key pair the controller launches with, the account Ansible reaches owned
// hardware as, and the guest generation that hardware can boot. Each is
// rendered where it is consumed, and each absence says what it means.
func TestGenerateHybridHostSideInputs(t *testing.T) {
	t.Parallel()

	bare, _ := mustGenerateHybrid(t, hybridParams())
	if strings.Contains(bare[HybridTerraformFile], "\n  key_name = ") {
		t.Error("no key pair must leave key_name unset")
	}
	if !strings.Contains(bare[HybridTerraformFile], "Instance Connect") {
		t.Error("the root must say how a fresh image is reached without a key pair")
	}
	hosts := inventoryHosts(t, bare[HybridInventoryFile])
	if _, set := hosts["acme-ci-fc-1"]["ansible_user"]; set {
		t.Error("the local host must carry no ansible_user unless one was named; owned hardware has no `ubuntu` by default")
	}
	if hosts["acme-ci-control-plane"]["ansible_user"] != "ubuntu" {
		t.Error("the controller is a Canonical image, whose account is ubuntu")
	}

	p := hybridParams()
	p.SSHKeyName = "ops-key"
	p.LocalAnsibleUser = "operator"
	p.LocalImage = "ubuntu-2404-arm64@verified"
	files, _ := mustGenerateHybrid(t, p)
	if !strings.Contains(files[HybridTerraformFile], `key_name = "ops-key"`) {
		t.Error("a named key pair must reach the root")
	}
	hosts = inventoryHosts(t, files[HybridInventoryFile])
	if hosts["acme-ci-fc-1"]["ansible_user"] != "operator" {
		t.Errorf("the local user must be rendered, got %v", hosts["acme-ci-fc-1"]["ansible_user"])
	}
	local := hostConfig(t, hosts["acme-ci-fc-1"])
	for _, tier := range local.Tiers {
		if tier.Launch[config.ProviderFirecracker].Image != "ubuntu-2404-arm64@verified" {
			t.Errorf("tier %s: the named local image must be what every tier boots, got %q",
				tier.Label, tier.Launch[config.ProviderFirecracker].Image)
		}
	}
}
