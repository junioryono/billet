package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/initconfig"

	"gopkg.in/yaml.v3"
)

// hybridArgs is the flag set every hybrid test shares: DECLARED shapes, so no
// test reaches AWS, and a --config path the test owns.
func hybridArgs(out, cfg string) []string {
	return []string{
		"--out", out, "--config", cfg,
		"--name", "acme-ci", "--region", "us-west-2", "--org", "acme",
		"--control-plane-private-ip", "10.60.0.10",
		"--local-vcpu", "32", "--local-memory", "128GiB",
		"--max-vcpu", "16", "--max-memory", "32GiB",
		"--instance-type", "c7i.xlarge=4,8GiB,0.17",
		"--instance-type", "c7i.2xlarge=8,16GiB,0.34",
	}
}

// hybridOutputsJSON is what `terraform output -json` writes after an apply of
// the generated root, for an untrusted generation.
const hybridOutputsJSON = `{
  "control_plane_private_ip": {"sensitive": false, "type": "string", "value": "10.60.0.10"},
  "ledger_volume_id": {"sensitive": false, "type": "string", "value": "vol-0abc"},
  "subnet_id": {"sensitive": false, "type": "string", "value": "subnet-0abc"},
  "runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-trusted"},
  "untrusted_runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-untrusted"},
  "ami_payload_bucket": {"sensitive": false, "type": "string", "value": "acme-ci-ami-payloads-1"}
}`

// hybridCacheOutputsJSON adds the three facts a --cache root declares and a
// generation without one does not, so the prepare render can fill them.
const hybridCacheOutputsJSON = `{
  "control_plane_private_ip": {"sensitive": false, "type": "string", "value": "10.60.0.10"},
  "ledger_volume_id": {"sensitive": false, "type": "string", "value": "vol-0abc"},
  "subnet_id": {"sensitive": false, "type": "string", "value": "subnet-0abc"},
  "runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-trusted"},
  "untrusted_runner_security_group_id": {"sensitive": false, "type": "string", "value": "sg-untrusted"},
  "ami_payload_bucket": {"sensitive": false, "type": "string", "value": "acme-ci-ami-payloads-1"},
  "cache_bucket": {"sensitive": false, "type": "string", "value": "acme-ci-cache-1"},
  "cache_prefix": {"sensitive": false, "type": "string", "value": "billet-cache"},
  "availability_zone": {"sensitive": false, "type": "string", "value": "us-west-2a"}
}`

var hybridFileNames = []string{
	initconfig.HybridTerraformFile, initconfig.HybridInventoryFile, initconfig.HybridSiteFile,
	initconfig.HybridRequirementsFile, HybridRunbookFile,
}

func runInitHybrid(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var err error
	out := capture(t, func() { err = cmdInit(t.Context(), append([]string{"hybrid"}, args...)) })

	return out, err
}

func readHybrid(t *testing.T, dir, rel string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}

	return string(raw)
}

// THE FIVE FILES, EACH BILLET'S OWN, and the next step said.
func TestInitHybridWritesTheShape(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")

	printed, err := runInitHybrid(t, hybridArgs(out, filepath.Join(dir, "none.yaml"))...)
	if err != nil {
		t.Fatalf("init hybrid: %v", err)
	}

	for _, name := range hybridFileNames {
		body := readHybrid(t, out, name)
		if !hybridOwned([]byte(body)) {
			t.Errorf("%s does not open with the marker, so a re-run could not tell it from an operator's file", name)
		}
	}

	if !strings.Contains(printed, "plan render") || !strings.Contains(printed, "--terraform-output") {
		t.Errorf("the report must name the render and the next command:\n%s", printed)
	}
	if !strings.Contains(printed, "The App ids are zero") {
		t.Errorf("with no identity to carry, the report must say the App comes first:\n%s", printed)
	}

	// A development build pins main and says so; a release pins itself. Either
	// way every layer names the same thing.
	tf := readHybrid(t, out, initconfig.HybridTerraformFile)
	req := readHybrid(t, out, initconfig.HybridRequirementsFile)
	ref, _ := hybridRef()
	if !strings.Contains(tf, "?ref="+ref+`"`) || !strings.Contains(req, "version: "+ref) {
		t.Errorf("the root and the collection must pin %q", ref)
	}
}

// AN OPERATOR'S FILE IS NEVER REPLACED: the generation lands beside it, and a
// second run refuses to truncate that .new either.
func TestInitHybridLeavesAForeignFileBeside(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	inventory := filepath.Join(out, initconfig.HybridInventoryFile)

	theirs := "all:\n  hosts: {}\n# an operator's own inventory\n"
	if err := os.MkdirAll(out, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inventory, []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}

	printed, err := runInitHybrid(t, hybridArgs(out, filepath.Join(dir, "none.yaml"))...)
	if err != nil {
		t.Fatalf("init hybrid: %v", err)
	}

	if got := readHybrid(t, out, initconfig.HybridInventoryFile); got != theirs {
		t.Errorf("the operator's inventory was rewritten:\n%s", got)
	}
	if side := readHybrid(t, out, initconfig.HybridInventoryFile+".new"); !hybridOwned([]byte(side)) {
		t.Error("the fresh generation must land beside the operator's file")
	}
	if !strings.Contains(printed, "NOT billet's own") {
		t.Errorf("the report must say which file went beside:\n%s", printed)
	}

	// The other four were absent and are written outright.
	if !hybridOwned([]byte(readHybrid(t, out, initconfig.HybridSiteFile))) {
		t.Error("an absent file must be written")
	}

	// A .new holding a merge in progress is not truncated by the next run.
	if _, err := runInitHybrid(t, hybridArgs(out, filepath.Join(dir, "none.yaml"))...); err == nil ||
		!strings.Contains(err.Error(), "merge in progress") {
		t.Errorf("a second run must refuse the existing .new by name, got %v", err)
	}

	// --force replaces the operator's file after all.
	if _, err := runInitHybrid(t, append(hybridArgs(out, filepath.Join(dir, "none.yaml")), "--force")...); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if !hybridOwned([]byte(readHybrid(t, out, initconfig.HybridInventoryFile))) {
		t.Error("--force must replace the operator's file")
	}
}

// BILLET'S OWN FILE IS REPLACED BY A RE-RUN, which is how the three renders
// succeed one another in one directory.
func TestInitHybridReplacesItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	if _, err := runInitHybrid(t, hybridArgs(out, cfg)...); err != nil {
		t.Fatalf("first run: %v", err)
	}

	args := hybridArgs(out, cfg)
	for i := range args {
		if args[i] == "128GiB" {
			args[i] = "256GiB"
		}
	}
	if _, err := runInitHybrid(t, args...); err != nil {
		t.Fatalf("second run: %v", err)
	}

	inventory := readHybrid(t, out, initconfig.HybridInventoryFile)
	if !strings.Contains(inventory, "max_memory: 240GiB") {
		t.Errorf("the re-run must replace billet's own inventory with the new ceiling:\n%s", inventory)
	}
	if _, err := os.Stat(filepath.Join(out, initconfig.HybridInventoryFile+".new")); !os.IsNotExist(err) {
		t.Error("replacing billet's own file must leave no .new beside it")
	}
}

// THE PREPARE AND COMMISSION RENDERS FILL THE FACTS FROM THE APPLY, and a
// missing output is refused by name.
func TestInitHybridFillsFactsFromTerraformOutput(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	outputs := filepath.Join(dir, "outputs.json")
	if err := os.WriteFile(outputs, []byte(hybridOutputsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	printed, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	inventory := readHybrid(t, out, initconfig.HybridInventoryFile)
	if left := initconfig.HybridPlaceholders(inventory); len(left) > 0 {
		t.Errorf("the prepare render still waits for %v", left)
	}
	if !strings.Contains(inventory, "billet_ledger_volume_id: vol-0abc") ||
		!strings.Contains(inventory, "billet_server_prepare_only: true") {
		t.Errorf("the prepare render must fill the volume id and keep the hold:\n%s", inventory)
	}
	if !strings.Contains(printed, "prepare render") {
		t.Errorf("the report must name the render:\n%s", printed)
	}

	_, err = runInitHybrid(t, append(hybridArgs(out, cfg),
		"--terraform-output", outputs, "--commission", "--ami", "ami-0123456789abcdef0")...)
	if err != nil {
		t.Fatalf("commission: %v", err)
	}
	inventory = readHybrid(t, out, initconfig.HybridInventoryFile)
	if !strings.Contains(inventory, "billet_server_prepare_only: false") ||
		!strings.Contains(inventory, "billet_enable_node: true") ||
		!strings.Contains(inventory, "image: ami-0123456789abcdef0") ||
		!strings.Contains(inventory, "subnet_id: subnet-0abc") {
		t.Errorf("the commission render must lift the hold, add the node and the AMI:\n%s", inventory)
	}

	broken := strings.Replace(hybridOutputsJSON, `"subnet_id"`, `"subnet"`, 1)
	if err := os.WriteFile(outputs, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...); err == nil ||
		!strings.Contains(err.Error(), `"subnet_id"`) {
		t.Errorf("a missing output must be refused by name, got %v", err)
	}
}

// THE APP IDENTITY IS CARRIED from the file github-app create wrote, under
// init's rules.
func TestInitHybridCarriesTheAppIdentity(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "billet-app.yaml")
	seed := "github:\n  org: acme\n  app_id: 7\n  installation_id: 9\n  client_id: Iv1.abc\n  private_key_path: /somewhere/key.pem\n"
	if err := os.WriteFile(cfg, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	printed, err := runInitHybrid(t, hybridArgs(out, cfg)...)
	if err != nil {
		t.Fatalf("init hybrid: %v", err)
	}
	if strings.Contains(printed, "The App ids are zero") {
		t.Error("a carried identity must not be reported as absent")
	}

	var doc struct {
		All struct {
			Children struct {
				ControlPlane struct {
					Hosts map[string]struct {
						Config struct {
							GitHub struct {
								AppID          int64  `yaml:"app_id"`
								InstallationID int64  `yaml:"installation_id"`
								ClientID       string `yaml:"client_id"`
								PrivateKeyPath string `yaml:"private_key_path"`
							} `yaml:"github"`
						} `yaml:"billet_config"`
					} `yaml:"hosts"`
				} `yaml:"control_plane"`
			} `yaml:"children"`
		} `yaml:"all"`
	}
	if err := yaml.Unmarshal([]byte(readHybrid(t, out, initconfig.HybridInventoryFile)), &doc); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	host := doc.All.Children.ControlPlane.Hosts["acme-ci-control-plane"]
	if host.Config.GitHub.AppID != 7 || host.Config.GitHub.InstallationID != 9 || host.Config.GitHub.ClientID != "Iv1.abc" {
		t.Errorf("the identity was not carried: %+v", host.Config.GitHub)
	}
	// THE KEY PATH IS THE ROLE'S, never the scratch file's: the role installs
	// the key where the service reads it.
	if host.Config.GitHub.PrivateKeyPath != "/etc/billet/app-private-key.pem" {
		t.Errorf("private_key_path %q, want the service path", host.Config.GitHub.PrivateKeyPath)
	}

	// A file that exists and cannot be read is a refusal, not a silent zero.
	if err := os.Chmod(cfg, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(cfg, 0o600); err != nil {
			t.Logf("restore %s: %v", cfg, err)
		}
	})
	if os.Geteuid() != 0 {
		if _, err := runInitHybrid(t, hybridArgs(out, cfg)...); err == nil {
			t.Error("an unreadable --config must be refused rather than read as no identity")
		}
	}
}

// THE RUNBOOK'S STEPS ARE IN THE ORDER THE ROLE NEEDS, and every command names
// this generation's own hosts and flags.
func TestInitHybridRunbookOrder(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")

	if _, err := runInitHybrid(t, hybridArgs(out, filepath.Join(dir, "none.yaml"))...); err != nil {
		t.Fatalf("init hybrid: %v", err)
	}
	runbook := readHybrid(t, out, HybridRunbookFile)

	ordered := []string{
		"billet github-app create",
		"terraform -chdir=terraform apply",
		"--terraform-output outputs.json",
		"-l acme-ci-control-plane",
		"billet ca issue acme-ci-control-plane",
		"billet ca issue acme-ci-fc-1",
		"billet ami build",
		"--commission --ami",
		"-l acme-ci-fc-1",
		"billet check --config /etc/billet/billet.yaml",
		"billet drain",
		"billet local restore",
	}
	last := -1
	for _, step := range ordered {
		at := strings.Index(runbook, step)
		if at < 0 {
			t.Errorf("the runbook never says %q", step)

			continue
		}
		if at < last {
			t.Errorf("%q comes before the step that must precede it", step)
		}
		last = at
	}

	// The prepare-only converge carries the key, the commission one does not,
	// and both are said.
	if !strings.Contains(runbook, "BILLET_GITHUB_PRIVATE_KEY_PATH=") || !strings.Contains(runbook, "No key this time") {
		t.Error("the runbook must put the App key on the prepare-only converge and not on the commission one")
	}
	// The re-run commands repeat this generation's flags, canonically and
	// shell-quoted (a declared shape carries characters a shell would split).
	if !strings.Contains(runbook, "--instance-type 'c7i.xlarge=4,8GiB,0.17'") ||
		!strings.Contains(runbook, "--control-plane-private-ip 10.60.0.10") {
		t.Error("the runbook's re-run commands must carry the flags this generation was made with")
	}
	if !strings.Contains(runbook, "sudo -u billet billet ca issue") {
		t.Error("the certificates must be issued as the service user, so the minted identity is owned by it")
	}
	// No key pair was named, so the runbook has to say how the fresh image is
	// reached at all; with one, ordinary SSH is the answer and Instance Connect
	// is not mentioned as the way in.
	if !strings.Contains(runbook, "aws ec2-instance-connect send-ssh-public-key") {
		t.Error("without --key-name the runbook must reach the controller with EC2 Instance Connect")
	}
	if _, err := runInitHybrid(t, append(hybridArgs(out, filepath.Join(dir, "none.yaml")), "--key-name", "ops-key", "--force")...); err != nil {
		t.Fatalf("with a key: %v", err)
	}
	keyed := readHybrid(t, out, HybridRunbookFile)
	if !strings.Contains(keyed, "ops-key") || strings.Contains(keyed, "send-ssh-public-key") {
		t.Error("with --key-name the runbook must name the key and not the Instance Connect push")
	}
	if !strings.Contains(keyed, "--key-name ops-key") {
		t.Error("the re-run commands must repeat --key-name")
	}
}

// EVERY REFUSAL NAMES THE FLAG.
func TestInitHybridRefusals(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	base := hybridArgs(out, cfg)

	without := func(flag string) []string {
		var kept []string
		for i := 0; i < len(base); i++ {
			if base[i] == flag {
				i++

				continue
			}
			kept = append(kept, base[i])
		}

		return kept
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no --out", without("--out"), "--out is required"},
		{"no --region", without("--region"), "--region is required"},
		{"no --local-memory", without("--local-memory"), "--local-memory is required"},
		{"a bad memory", append(without("--max-memory"), "--max-memory", "lots"), "--max-memory"},
		{"no shapes", without("--instance-type"), "--instance-type is required"},
		{"commission without facts", append(base, "--commission"), "--commission needs --terraform-output"},
		{"an ami without commission", append(base, "--ami", "ami-1"), "--ami is read with --commission"},
		{"commission without an ami", append(base, "--terraform-output", filepath.Join(dir, "outputs.json"), "--commission"), "--commission needs --ami"},
		{"mixed shapes", append(base, "--instance-type", "c7i.4xlarge"), "mixes fetched"},
		{"a price on a declared shape", append(base, "--price", "c7i.xlarge=0.1"), "--price overrides a FETCHED"},
		{"a malformed declared shape", append(without("--instance-type"), "--instance-type", "c7i.xlarge=4,8GiB"), "needs vcpu,memory,usd"},
		{"a bad address", append(without("--control-plane-private-ip"), "--control-plane-private-ip", "ten"), "--control-plane-private-ip"},
		{"a missing outputs file", append(base, "--terraform-output", filepath.Join(dir, "nope.json")), "--terraform-output"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runInitHybrid(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal naming %q, got %v", tc.want, err)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Errorf("a refused run must write nothing, but %s exists", out)
			}
		})
	}
}

// THE BUILD COMMAND HAS TO RUN WHERE ITS CREDENTIALS ARE.
//
// With --builder the grant is on the controller's role, and the controller has
// no copy of the Terraform state — so a command reading `terraform -chdir=...
// output` there resolves nothing, while running it on the workstation is
// exactly the credentials --builder exists to remove. The prepare render is the
// first that CAN write the values literally, because that is when the apply has
// produced them.
func TestInitHybridBuilderRunbookRunsWhereTheCredentialsAre(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	outputs := filepath.Join(dir, "outputs.json")
	if err := os.WriteFile(outputs, []byte(hybridOutputsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// The PLAN render has no facts, so it keeps the workstation form and says
	// the prepare render will replace it.
	if _, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--builder")...); err != nil {
		t.Fatalf("plan render: %v", err)
	}
	plan := readHybrid(t, out, HybridRunbookFile)
	if !strings.Contains(plan, "terraform -chdir=terraform output -raw subnet_id") {
		t.Error("before the apply the build command must still read the outputs it can reach")
	}
	if !strings.Contains(plan, "prints a command to run **on the controller**") {
		t.Error("the plan render must say the controller-side command is coming")
	}

	// The PREPARE render carries the values literally and says where to run it.
	if _, err := runInitHybrid(t, append(hybridArgs(out, cfg),
		"--builder", "--terraform-output", outputs)...); err != nil {
		t.Fatalf("prepare render: %v", err)
	}
	prepared := readHybrid(t, out, HybridRunbookFile)
	if !strings.Contains(prepared, "**On the controller**") {
		t.Error("with the grant and the facts, the build runs on the controller")
	}
	if strings.Contains(prepared, "terraform -chdir=terraform output -raw subnet_id") {
		t.Error("the controller has no Terraform state, so the command must not read it")
	}
	// THE WHOLE COMMAND IS MATCHED, fences included, not its arguments one at a
	// time against the page.
	//
	// The prose under this step names `--public-ip` to explain why it is there,
	// so a `strings.Contains(runbook, "--public-ip")` is satisfied by the
	// sentence and stays green with the flag deleted from the command itself —
	// measured, by deleting it and watching the test pass. Locating the fenced
	// block instead needs a Markdown scanner, and a hand-written one cannot tell
	// a fence from a fence quoted inside a wider one. An exact match of the whole
	// block needs neither: prose cannot satisfy it, and no argument can go
	// missing, change or be added without breaking it.
	//
	// --public-ip is the argument worth the care: the generated subnet's only
	// route is an internet gateway, which an instance cannot use without an
	// address, so a command missing it fails in the subnet this same generation
	// created.
	want := "```bash\n" +
		"billet ami build --region us-west-2 \\\n" +
		"  --subnet subnet-0abc \\\n" +
		"  --security-group sg-trusted \\\n" +
		"  --payload-bucket acme-ci-ami-payloads-1 \\\n" +
		"  --public-ip --base-image ami-<an EBS-backed Ubuntu 24.04 image in us-west-2>\n" +
		"```"
	assertOneBuildCommand(t, prepared, want)
}

// assertOneBuildCommand proves the AMI build step carries exactly this command,
// and that it is the step's own live command.
//
// A match alone proves the text is somewhere on the page. Three cheap facts make
// it the right somewhere, without the Markdown parser that a hand-written
// scanner cannot correctly be: the step's heading appears exactly once, so a
// stale duplicate step cannot answer for the real one; the command appears
// exactly once, so a later section cannot; and this generator opens every fence
// with exactly three backticks, so a quoted example inside a wider fence — the
// one case a scanner would also miss — cannot exist to be matched.
func assertOneBuildCommand(t *testing.T, runbook, want string) {
	t.Helper()

	heading, n := "## 6. Build the AMI", 0

	for line := range strings.SplitSeq(runbook, "\n") {
		// EXACTLY THIS LINE, not a document containing the text: `## 6. Build the
		// AMI` is a substring of `### 6. Build the AMI`, and a count keyed on the
		// PRECEDING newline misses a duplicate at byte zero.
		if line == heading {
			n++
		}
	}

	if n != 1 {
		t.Fatalf("the runbook carries the build step %d times, want exactly one", n)
	}

	// THE CONTAINERS THE PROOF ASSUMES ABSENT, proved absent. The command below
	// is only the step's live command if nothing quotes it, and Markdown has
	// three ways to quote a triple-backtick block: a wider backtick fence, a
	// tilde fence, and an HTML comment. This generator emits neither fence.
	for _, quoted := range []string{"````", "~~~"} {
		if strings.Contains(runbook, quoted) {
			t.Errorf("this generator emits no %q, and the assertion below rests on that: "+
				"a command quoted inside one would satisfy it", quoted)
		}
	}

	// IT DOES EMIT ONE HTML COMMENT — the ownership marker `billet init hybrid`
	// reads back before it will replace this file — so the rule for that one is
	// that it closes on its own line and therefore contains no command. An
	// unclosed one, or a second, could swallow the rest of the page.
	comments := 0

	for _, line := range strings.Split(runbook, "\n") {
		opens := strings.Count(line, "<!--")
		comments += opens

		if opens > 0 && strings.Count(line, "-->") != opens {
			t.Errorf("an HTML comment that does not close on its own line can quote the "+
				"command below: %q", line)
		}
	}

	if comments != 1 {
		t.Errorf("the runbook carries %d HTML comments, want only the ownership marker", comments)
	}

	if n := strings.Count(runbook, want); n != 1 {
		t.Errorf("the build step must carry exactly this command, once, and carries it %d "+
			"times:\n%s\n\ngot:\n%s", n, want, runbook)
	}
}

// WITHOUT --builder IT STAYS A WORKSTATION COMMAND, and says so, however many
// facts are known.
func TestInitHybridWithoutTheBuilderTheBuildStaysOnAWorkstation(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	outputs := filepath.Join(dir, "outputs.json")
	if err := os.WriteFile(outputs, []byte(hybridOutputsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := runInitHybrid(t, append(hybridArgs(out, cfg), "--terraform-output", outputs)...); err != nil {
		t.Fatalf("prepare render: %v", err)
	}
	runbook := readHybrid(t, out, HybridRunbookFile)
	if !strings.Contains(runbook, "From a workstation with your own AWS credentials") {
		t.Error("without the grant the runbook must say whose credentials the build uses")
	}
	// The workstation command reads the state it has, and carries the same
	// public address the subnet's only route requires. Matched whole, for the
	// reason the controller-side one is.
	want := "```bash\n" +
		"billet ami build --region us-west-2 \\\n" +
		"  --subnet \"$(terraform -chdir=terraform output -raw subnet_id)\" \\\n" +
		"  --security-group \"$(terraform -chdir=terraform output -raw runner_security_group_id)\" \\\n" +
		"  --payload-bucket \"$(terraform -chdir=terraform output -raw ami_payload_bucket)\" \\\n" +
		"  --public-ip --base-image ami-<an EBS-backed Ubuntu 24.04 image in us-west-2>\n" +
		"```"
	assertOneBuildCommand(t, runbook, want)
}

// AN IMAGE THAT CANNOT TRUST THE CACHE IS A CACHE NOBODY USES.
//
// A guest's cache client speaks HTTPS to an endpoint on the controller's
// private address, so the listener's certificate comes from a private issuer
// and no public root chains to it. `ami build --ca-cert` is the only thing that
// puts that issuer in the image's host trust store, and the image is built in
// step 6 while the listener's own pair is not asked for until step 7 — so a
// runbook that mentions the pair and never the issuer produces a deployment
// where the bucket, the listener, the site and the security-group rule are all
// correct and every job still fetches cold, with nothing reporting an error.
//
// Mutation: dropping --ca-cert from either command fails this.
func TestInitHybridCacheRunbookTrustsTheListenersIssuer(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")
	outputs := filepath.Join(dir, "outputs.json")
	if err := os.WriteFile(outputs, []byte(hybridCacheOutputsJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	// THE WORKSTATION FORM, which is the default: no builder grant.
	if _, err := runInitHybrid(t, append(hybridArgs(out, cfg),
		"--cache", "--terraform-output", outputs)...); err != nil {
		t.Fatalf("prepare render: %v", err)
	}

	runbook := readHybrid(t, out, HybridRunbookFile)

	// The argument, in its place in the command, so a mention in prose cannot
	// satisfy it.
	want := "--payload-bucket \"$(terraform -chdir=terraform output -raw ami_payload_bucket)\"" +
		" \\\n  --ca-cert cache-ca.pem \\\n"
	if !strings.Contains(runbook, want) {
		t.Errorf("a cache generation must bake the listener's issuer into the image:\n%s", runbook)
	}

	// The step that builds the image has to say what the issuer is for, since
	// skipping it reports nothing; and the step that installs the listener has
	// to tie its leaf to that anchor, because an anchor signing nothing on the
	// listener is the same cold fetch.
	if !strings.Contains(runbook, "--ca-cert` below takes the PEM of that issuer") {
		t.Error("step 6 must say what the issuer is for")
	}

	if !strings.Contains(runbook, "signed by that same `cache-ca.pem`") {
		t.Error("step 7 must tie the leaf it asks for to the anchor step 6 baked in")
	}

	// AND THE CONTROLLER-SIDE FORM, where the file has to be on the controller,
	// because that is where the command runs.
	if _, err := runInitHybrid(t, append(hybridArgs(out, cfg),
		"--cache", "--builder", "--terraform-output", outputs, "--force")...); err != nil {
		t.Fatalf("builder prepare render: %v", err)
	}

	builder := readHybrid(t, out, HybridRunbookFile)
	if !strings.Contains(builder,
		"--payload-bucket acme-ci-ami-payloads-1 \\\n  --ca-cert cache-ca.pem \\\n") {
		t.Errorf("the controller-side build needs the issuer too:\n%s", builder)
	}

	if !strings.Contains(builder, "`cache-ca.pem` has to be on the controller too") {
		t.Error("the controller-side build must say where the issuer file has to be")
	}
}

// WITHOUT A CACHE THERE IS NO ISSUER TO TRUST, and asking for one would send an
// operator looking for a file no part of this generation produces.
func TestInitHybridWithoutACacheTheBuildAsksForNoIssuer(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "acme-ci")
	cfg := filepath.Join(dir, "none.yaml")

	if _, err := runInitHybrid(t, hybridArgs(out, cfg)...); err != nil {
		t.Fatalf("plan render: %v", err)
	}

	if runbook := readHybrid(t, out, HybridRunbookFile); strings.Contains(runbook, "--ca-cert") {
		t.Error("a generation with no cache has no private issuer to bake in")
	}
}
