package scripts_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/scripts"
)

const (
	windowsTemplate    = "build.windows-2025.pkr.hcl"
	windowsPlanFile    = "plan-windows-2025.json"
	windowsDifferences = "differences-windows.tsv"
)

// windowsStep is one thing GitHub's windows-2025 template does to the image, in the
// template's order. It is a type of its own because a Windows provisioner says other
// things than an ubuntu one: which account elevates it, and which exit codes pass.
type windowsStep struct {
	ID             string            `json:"id"`
	Kind           string            `json:"kind"`
	Env            map[string]string `json:"env,omitempty"`
	Script         string            `json:"script,omitempty"`
	Inline         []string          `json:"inline,omitempty"`
	Sources        []string          `json:"sources,omitempty"`
	Destination    string            `json:"destination,omitempty"`
	Download       bool              `json:"download,omitempty"`
	Elevated       bool              `json:"elevated,omitempty"`
	ValidExitCodes []int             `json:"valid_exit_codes,omitempty"`
	PauseBefore    string            `json:"pause_before,omitempty"`
}

// windowsTemplateVars are the template's variables as a billet build sets them: its
// own defaults, but for temp_dir, which is GitHub's Azure temporary disk and a drive
// an EC2 instance does not have.
var windowsTemplateVars = map[string]string{
	"image_folder":          `C:\image`,
	"helper_script_folder":  `C:\Program Files\WindowsPowerShell\Modules\`,
	"agent_tools_directory": `C:\hostedtoolcache\windows`,
	"imagedata_file":        `C:\imagedata.json`,
	"temp_dir":              `C:\temp`,
	"install_user":          "installer",
	"install_password":      "@install_password@",
	"image_os":              "win25",
	"image_version":         "@image_version@",
}

// windowsProvisionerAttributes is every attribute the reader understands, per kind.
// Anything else is refused: an attribute the plan does not carry is a semantic the
// driver would silently not have.
var windowsProvisionerAttributes = map[string]map[string]bool{
	"file": {"destination": true, "source": true, "sources": true, "direction": true},
	"powershell": {"environment_vars": true, "scripts": true, "inline": true, "elevated_user": true,
		"elevated_password": true, "execution_policy": true, "pause_before": true, "skip_clean": true},
	"windows-shell":   {"inline": true},
	"windows-restart": {"check_registry": true, "restart_check_command": true, "restart_timeout": true},
}

var windowsExitCodes = regexp.MustCompile(`(?m)^\s*valid_exit_codes\s*=\s*\[([0-9, ]*)\]\s*$`)

// windowsValue expands a template value: its escapes first, so a variable's own
// backslashes are not read as one.
func windowsValue(t *testing.T, s string) string {
	t.Helper()

	return expandWith(t, windowsTemplateVars, strings.ReplaceAll(s, `\\`, `\`))
}

// readWindowsPlan derives the plan from GitHub's windows-2025 template with the
// reader the ubuntu plans use, and refuses what it does not know as that one does.
func readWindowsPlan(t *testing.T, template string) []windowsStep {
	t.Helper()

	var plan []windowsStep

	seen := map[string]int{}
	add := func(step windowsStep) {
		seen[step.ID]++
		if n := seen[step.ID]; n > 1 {
			step.ID += "#" + strconv.Itoa(n)
		}
		plan = append(plan, step)
	}

	for _, block := range provisionerBlocks(t, template) {
		allowed, known := windowsProvisionerAttributes[block.kind]
		if !known {
			t.Fatalf("the template uses a %q provisioner, which the driver does not know", block.kind)
		}

		// A LIST OF NUMBERS, WHICH THE SHARED READER REFUSES, read here on its own: it
		// is the one the template has, and a step's verdict depends on it.
		var codes []int

		body := block.body
		if match := windowsExitCodes.FindStringSubmatch(body); match != nil {
			for _, code := range strings.Split(match[1], ",") {
				n, err := strconv.Atoi(strings.TrimSpace(code))
				if err != nil {
					t.Fatalf("an exit code the reader cannot read: %q", code)
				}
				codes = append(codes, n)
			}
			body = windowsExitCodes.ReplaceAllString(body, "")
		}

		attrs := blockAttributes(t, body)
		for name := range attrs {
			if !allowed[name] {
				t.Fatalf("a %s provisioner sets %s, which the driver does not implement", block.kind, name)
			}
		}

		switch block.kind {
		case "file":
			step := windowsStep{Kind: "file", Destination: windowsValue(t, attrs.one("destination"))}
			for _, source := range append(attrs.list("sources"), attrs.optional("source")...) {
				step.Sources = append(step.Sources,
					repoPathUnder(t, "images/windows/templates", windowsValue(t, source)))
			}
			step.Download = attrs.optionalOne("direction") == "download"
			step.ID = "file:" + step.Destination
			if step.Download {
				step.ID = "download:" + step.Sources[0]
			}
			add(step)
		case "powershell":
			env := map[string]string{}
			for _, pair := range attrs.list("environment_vars") {
				name, value, ok := strings.Cut(windowsValue(t, pair), "=")
				if !ok {
					t.Fatalf("an environment variable without a value: %q", pair)
				}
				env[name] = value
			}
			if len(env) == 0 {
				env = nil
			}
			base := windowsStep{Kind: "powershell", Env: env, ValidExitCodes: codes,
				Elevated:    attrs.optionalOne("elevated_user") != "",
				PauseBefore: attrs.optionalOne("pause_before")}
			scripts := attrs.list("scripts")
			for _, script := range scripts {
				step := base
				step.Script = repoPathUnder(t, "images/windows/templates", windowsValue(t, script))
				step.ID = path.Base(step.Script)
				add(step)
			}
			if inline := attrs.list("inline"); len(inline) > 0 {
				step := base
				for _, command := range inline {
					step.Inline = append(step.Inline, windowsValue(t, command))
				}
				step.ID = "inline:" + windowsInlineID(step.Inline)
				add(step)
			}
			if len(scripts) == 0 && len(attrs.list("inline")) == 0 {
				t.Fatalf("a powershell provisioner with neither scripts nor inline commands")
			}
		case "windows-shell":
			step := windowsStep{Kind: "windows-shell"}
			for _, command := range attrs.list("inline") {
				step.Inline = append(step.Inline, windowsValue(t, command))
			}
			step.ID = "shell:" + windowsInlineID(step.Inline)
			add(step)
		case "windows-restart":
			add(windowsStep{Kind: "windows-restart", ID: "restart"})
		}
	}

	return plan
}

func windowsInlineID(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))

	return hex.EncodeToString(sum[:])[:12]
}

func windowsTemplateText(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(runnerImagesDir, "upstream", "images", "windows", "templates", windowsTemplate))
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

// THE COMMITTED PLAN IS THE TEMPLATE'S: a template change from a pin bump shows up as a
// plan diff to review, never as a silent drift.
func TestTheWindowsPlanIsTheTemplates(t *testing.T) {
	t.Parallel()

	var want bytes.Buffer
	encoder := json.NewEncoder(&want)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(readWindowsPlan(t, windowsTemplateText(t))); err != nil {
		t.Fatal(err)
	}

	planPath := filepath.Join(runnerImagesDir, windowsPlanFile)
	if *updatePlan {
		if err := os.WriteFile(planPath, want.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("%s is not what %s says; rerun with -update-plan and review the difference",
			planPath, windowsTemplate)
	}
}

// EVERY FILE THE PLAN NAMES IS VENDORED, or is the one declaration read from its one
// copy: a script the plan runs that is not in the tree fails here, not during a build.
func TestEveryFileTheWindowsPlanNamesIsVendored(t *testing.T) {
	t.Parallel()

	for _, step := range readWindowsPlan(t, windowsTemplateText(t)) {
		if step.Download {
			continue
		}
		for _, file := range append(append([]string{}, step.Sources...), step.Script) {
			if file == "" || file == "images/windows/toolsets" {
				continue
			}
			if _, err := os.Stat(filepath.Join(runnerImagesDir, "upstream", file)); err != nil {
				t.Errorf("step %s names %s, which is not vendored: %v", step.ID, file, err)
			}
		}
	}
}

func readWindowsSkips(t *testing.T) map[string]string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(runnerImagesDir, windowsDifferences))
	if err != nil {
		t.Fatal(err)
	}

	skips := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[2] == "" {
			t.Fatalf("a difference that is not an action, a step and a reason: %q", line)
		}
		if fields[0] != "skip" {
			name, ok := strings.CutPrefix(fields[0], "prepare:")
			if !ok {
				t.Fatalf("a difference the driver does not know: %q", line)
			}
			if _, err := os.Stat(filepath.Join("windows", "prepare-"+name+".ps1")); err != nil {
				t.Errorf("%s names a script that is not there: %v", fields[0], err)
			}

			continue
		}
		if _, twice := skips[fields[1]]; twice {
			t.Fatalf("the step %s is listed twice", fields[1])
		}
		skips[fields[1]] = fields[2]
	}

	return skips
}

// WHAT THE DRIVER CANNOT RUN IS A STATED DIFFERENCE, and every difference names a step:
// the two directions that keep the list the whole statement of what billet leaves out.
func TestEveryWindowsDifferenceNamesAStepAndEveryUnrunnableStepIsOne(t *testing.T) {
	t.Parallel()

	plan := readWindowsPlan(t, windowsTemplateText(t))
	skips := readWindowsSkips(t)

	ids := map[string]bool{}
	for _, step := range plan {
		ids[step.ID] = true

		if _, skipped := skips[step.ID]; skipped {
			continue
		}

		switch {
		case step.Kind != "file" && step.Kind != "powershell":
			t.Errorf("the driver runs no %s step, and %s is not a difference", step.Kind, step.ID)
		case step.Download:
			t.Errorf("the driver downloads nothing to a machine billet does not have, and %s is "+
				"not a difference", step.ID)
		}
	}

	for id := range skips {
		if !ids[id] {
			t.Errorf("differences-windows.tsv names %s, which the plan does not have", id)
		}
	}
}

// windowsFloor is the declaration floor ADR-023 records: what a job on a Windows AMI
// finds, each by the step of GitHub's that installs it. A step dropped from here or
// skipped in the list is a claim in that record gone false.
var windowsFloor = []string{
	"Configure-PowerShell.ps1", "Install-PowerShellModules.ps1", "Install-Chocolatey.ps1",
	"Configure-BaseImage.ps1", "Configure-SystemEnvironment.ps1", "Install-PowershellCore.ps1",
	"Install-ChocolateyPackages.ps1", "Install-ActionsCache.ps1", "Install-Ruby.ps1",
	"Install-PyPy.ps1", "Install-Toolset.ps1", "Configure-Toolset.ps1", "Install-NodeJS.ps1",
	"Install-Git.ps1", "Install-GitHub-CLI.ps1",
}

// THE FLOOR RUNS, in the template's order, and what the record says it left out is not
// in it: Visual Studio needs a subscription, and a step installing a license-restricted
// component is the deployment's to accept.
func TestTheWindowsFloorRunsAndWhatItLeavesOutDoesNot(t *testing.T) {
	t.Parallel()

	plan := readWindowsPlan(t, windowsTemplateText(t))
	skips := readWindowsSkips(t)

	var order []string
	for _, step := range plan {
		order = append(order, step.ID)
	}

	last := -1
	for _, id := range windowsFloor {
		at := slices.Index(order, id)
		if at < 0 {
			t.Fatalf("the plan has no step %s", id)
		}
		if _, skipped := skips[id]; skipped {
			t.Errorf("%s is the floor ADR-023 records, and is skipped", id)
		}
		if at < last {
			t.Errorf("%s runs earlier than the floor's order says", id)
		}
		last = at
	}

	for _, id := range []string{
		"Install-VisualStudio.ps1", "Install-VSExtensions.ps1", "Install-AndroidSDK.ps1",
		"Install-Chrome.ps1", "Install-EdgeDriver.ps1", "Install-Firefox.ps1", "Install-CodeQLBundle.ps1",
		"Install-WindowsFeatures.ps1", "Install-Docker.ps1", "Install-WindowsUpdates.ps1",
		"RunAll-Tests.ps1",
	} {
		if _, skipped := skips[id]; !skipped {
			t.Errorf("%s is not in the floor and is not a difference", id)
		}
	}
}

// EVERY SCRIPT BILLET WRITES PARSES, by PowerShell's own parser, on a machine that has
// one. THAT PARSER IS PowerShell 7's, which accepts syntax the Windows PowerShell 5.1 these
// scripts run under does not, so this catches what is broken in both and not what is
// new in 7: a real build on Windows is what proves that (ADR-023).
func TestTheWindowsScriptsParse(t *testing.T) {
	t.Parallel()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can read PowerShell")
	}

	scripts, err := filepath.Glob(filepath.Join("windows", "*.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(scripts) < 5 {
		t.Fatalf("only %d scripts under windows/; the glob read nothing", len(scripts))
	}

	for _, script := range scripts {
		absolute, err := filepath.Abs(script)
		if err != nil {
			t.Fatal(err)
		}

		cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-Command", `
$errors = $null
$tokens = $null
[void][System.Management.Automation.Language.Parser]::ParseFile($env:BILLET_TEST_SCRIPT, [ref]$tokens, [ref]$errors)
if ($errors.Count -gt 0) { $errors | ForEach-Object { Write-Output $_.ToString() }; exit 1 }
`)
		cmd.Env = append(os.Environ(), "BILLET_TEST_SCRIPT="+absolute)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", script, err, out)
		}
	}
}

// windowsBuildFixture is a build tree under a temporary root, its plan and differences
// the caller's, and what to run the driver with.
func windowsBuildFixture(t *testing.T, plan []windowsStep, differences string) (string, []string) {
	t.Helper()

	root := t.TempDir()
	images := filepath.Join(root, "repo", "scripts", "runner-images")
	if err := os.MkdirAll(images, 0o755); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		filepath.Join(images, windowsPlanFile):    body,
		filepath.Join(images, windowsDifferences): []byte(differences),
		filepath.Join(root, "build.json"): []byte(`{"nonce":"` + strings.Repeat("a", 32) +
			`","runner_version":"2.337.0","runner_sha256":"` + strings.Repeat("b", 64) +
			`","image_version":"billet"}`),
		filepath.Join(root, "layer.ps1"): []byte("Add-Content -LiteralPath $env:BILLET_TEST_MARKS -Value layer\n"),
		filepath.Join(root, "shutdown.ps1"): []byte("Add-Content -LiteralPath $env:BILLET_TEST_MARKS -Value shutdown\n" +
			"Add-Content -LiteralPath $env:BILLET_AMI_CONSOLE -Value the-machine-ended\n"),
	} {
		if err := os.WriteFile(name, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return root, []string{
		"BILLET_AMI_ROOT=" + root,
		"BILLET_AMI_CONSOLE=" + filepath.Join(t.TempDir(), "console"),
		"BILLET_AMI_SHELL=pwsh",
		"BILLET_AMI_DWELL_ROUNDS=2",
		"BILLET_AMI_DWELL_SECONDS=0",
		"BILLET_AMI_FAIL_ROUNDS=2",
		"BILLET_AMI_LAYER=" + filepath.Join(root, "layer.ps1"),
		"BILLET_AMI_SHUTDOWN=" + filepath.Join(root, "shutdown.ps1"),
		"BILLET_TEST_MARKS=" + filepath.Join(root, "marks"),
		// THE DRIVER REMOVES THIS TREE RECURSIVELY, and a machine's own must not reach it.
		"TEMP_DIR=",
	}
}

func runWindowsDriver(t *testing.T, env []string) (string, string, error) {
	t.Helper()

	driver, err := filepath.Abs(filepath.Join("windows", "build-ami.ps1"))
	if err != nil {
		t.Fatal(err)
	}

	return runWindowsDriverAt(t, driver, env)
}

func runWindowsDriverAt(t *testing.T, driver string, env []string) (string, string, error) {
	t.Helper()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can run the driver")
	}

	cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", driver)
	cmd.Env = append(os.Environ(), env...)
	out, runErr := cmd.CombinedOutput()

	read := func(prefix string) string {
		for _, kv := range env {
			if name, value, ok := strings.Cut(kv, "="); ok && name == prefix {
				raw, err := os.ReadFile(value)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}

				return string(raw)
			}
		}

		return ""
	}
	if testing.Verbose() {
		t.Logf("driver output:\n%s", out)
	}

	return read("BILLET_AMI_CONSOLE"), read("BILLET_TEST_MARKS"), runErr
}

func markStep(id string) windowsStep {
	return windowsStep{ID: id, Kind: "powershell", Inline: []string{
		"Add-Content -LiteralPath $env:BILLET_TEST_MARKS -Value " + id,
	}}
}

// A BUILD THAT FINISHES runs its steps in order, leaves out a skipped one, runs the layer
// after them, cleans up before it announces anything, and only then ends the machine.
func TestTheWindowsDriverRunsThePlanAndAnnouncesSuccess(t *testing.T) {
	t.Parallel()

	scratch := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}

	first := markStep("one")
	first.Env = map[string]string{"TEMP_DIR": scratch}

	root, env := windowsBuildFixture(t,
		[]windowsStep{first, markStep("two"), markStep("three")},
		"# what billet does differently\nskip\ttwo\tnot carried\n")

	console, marks, err := runWindowsDriver(t, env)
	if err != nil {
		t.Fatalf("the driver: %v\nconsole:\n%s", err, console)
	}

	if got := strings.Fields(marks); !slices.Equal(got, []string{"one", "three", "layer", "shutdown"}) {
		t.Fatalf("the build ran %q", got)
	}
	if !strings.Contains(console, "billet-build-begin "+strings.Repeat("a", 32)) ||
		!strings.Contains(console, "verdict=ok") || !strings.Contains(console, "step=done") ||
		!strings.Contains(console, "components=") {
		t.Fatalf("no finished verdict on the console:\n%s", console)
	}
	if strings.Contains(console, "verdict=fail") {
		t.Fatalf("a failure on the console of a build that finished:\n%s", console)
	}
	// BILLET READS ONLY A CLOSED BLOCK, AND ONLY WHILE THE MACHINE RUNS.
	if !strings.Contains(console, "billet-build-end "+strings.Repeat("a", 32)) {
		t.Fatalf("the verdict's block never closes:\n%s", console)
	}
	if strings.LastIndex(console, "verdict=ok") > strings.Index(console, "the-machine-ended") {
		t.Fatalf("the verdict was said after the machine ended:\n%s", console)
	}
	if _, err := os.Stat(filepath.Join(root, "repo")); !os.IsNotExist(err) {
		t.Fatalf("the payload is still on the disk being imaged: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatalf("the template's scratch is still on the disk being imaged: %v", err)
	}
}

// A STEP THAT FAILS stops the build there, naming it by number, and nothing after it
// runs: not the next step, not the layer, and above all not the shutdown billet would
// read as a finished build.
func TestAWindowsStepThatFailsStopsTheBuildAndNeverShutsItDown(t *testing.T) {
	t.Parallel()

	failing := windowsStep{ID: "two", Kind: "powershell", Inline: []string{
		"Write-Output 'the step said why'", "exit 3",
	}}
	_, env := windowsBuildFixture(t, []windowsStep{markStep("one"), failing, markStep("three")}, "")

	console, marks, err := runWindowsDriver(t, env)
	if err == nil {
		t.Fatalf("the driver exited 0 for a build that failed:\n%s", console)
	}

	if got := strings.Fields(marks); !slices.Equal(got, []string{"one"}) {
		t.Fatalf("the build ran %q past its failure", got)
	}
	for _, want := range []string{"verdict=fail", "step=template", "template_step=2"} {
		if !strings.Contains(console, want) {
			t.Errorf("the verdict does not say %q:\n%s", want, console)
		}
	}

	// WHY IT FAILED IS ON THE ONE CHANNEL AN EGRESS-ONLY BUILDER HAS, ahead of the verdict
	// so the parser's last complete block is still the verdict.
	if at := strings.Index(console, "billet-build-log the step said why"); at < 0 ||
		at > strings.Index(console, "verdict=fail") {
		t.Errorf("the failing step's output is not on the console ahead of the verdict:\n%s", console)
	}
	if strings.Contains(console, "verdict=ok") {
		t.Fatalf("a success on the console of a build that failed:\n%s", console)
	}
}

// A CMDLET THAT FAILS FAILS ITS STEP, with no `exit` to say so: the preference that stops
// it does not cross from the driver's process into the step's, and `exit $LASTEXITCODE`
// with nothing having set a status reads as success.
func TestACmdletThatFailsFailsItsStepWithoutAnExit(t *testing.T) {
	t.Parallel()

	failing := windowsStep{ID: "two", Kind: "powershell", Inline: []string{
		"Move-Item -LiteralPath '/no/such/file' -Destination '/no/such/place'",
	}}
	_, env := windowsBuildFixture(t, []windowsStep{markStep("one"), failing, markStep("three")}, "")

	console, marks, err := runWindowsDriver(t, env)
	if err == nil || !strings.Contains(console, "verdict=fail") || !strings.Contains(console, "template_step=2") {
		t.Fatalf("a step whose cmdlet failed passed: %v\n%s", err, console)
	}
	if got := strings.Fields(marks); !slices.Equal(got, []string{"one"}) {
		t.Fatalf("the build ran %q past its failure", got)
	}
}

// A PREPARE RUNS JUST BEFORE ITS STEP, from billet's own directory, and a prepare naming a
// script the build does not carry stops the build before anything runs.
func TestAWindowsPrepareRunsBeforeItsStep(t *testing.T) {
	t.Parallel()

	_, env := windowsBuildFixture(t, []windowsStep{markStep("one"), markStep("two")},
		"prepare:no-such-thing\ttwo\twhatever\n")

	console, marks, err := runWindowsDriver(t, env)
	if err == nil || !strings.Contains(console, "verdict=fail") {
		t.Fatalf("a prepare naming no script passed: %v\n%s", err, console)
	}
	if marks != "" {
		t.Fatalf("the build ran %q before refusing its differences", marks)
	}

	// THE REAL ONE, ON THE STEP IT EXISTS FOR, as the list says.
	skips := readWindowsSkips(t)
	if _, skipped := skips["Install-PowerShellModules.ps1"]; skipped {
		t.Error("the step the AWS Tools prepare exists for is skipped")
	}

	raw, err := os.ReadFile(filepath.Join(runnerImagesDir, windowsDifferences))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "prepare:aws-tools\tInstall-PowerShellModules.ps1\t") {
		t.Error("the AWS Tools prepare is not before the step whose install it unblocks")
	}
}

// A PREPARE IS RUN, between the step before its own and its own, and one that fails fails
// the build there, however it fails.
func TestAWindowsPrepareRunsAndCanFailTheBuild(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("windows", "build-ami.ps1"))
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		prepare string
		want    []string
		fails   bool
	}{
		"succeeds": {
			prepare: "Add-Content -LiteralPath $env:BILLET_TEST_MARKS -Value prepared\n",
			want:    []string{"one", "prepared", "two", "layer", "shutdown"},
		},
		"throws": {prepare: "throw 'no'\n", want: []string{"one"}, fails: true},
		"exits":  {prepare: "exit 3\n", want: []string{"one"}, fails: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			// THE DRIVER FINDS A PREPARE BESIDE ITSELF, so this one runs from a copy.
			here := t.TempDir()
			driver := filepath.Join(here, "build-ami.ps1")
			if err := os.WriteFile(driver, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(here, "prepare-fixture.ps1"), []byte(tc.prepare), 0o600); err != nil {
				t.Fatal(err)
			}

			_, env := windowsBuildFixture(t, []windowsStep{markStep("one"), markStep("two")},
				"prepare:fixture\ttwo\tthe fixture\n")

			console, marks, err := runWindowsDriverAt(t, driver, env)
			if tc.fails != (err != nil) {
				t.Fatalf("the driver: %v, want a failure %v\n%s", err, tc.fails, console)
			}
			if got := strings.Fields(marks); !slices.Equal(got, tc.want) {
				t.Fatalf("the build ran %q, want %q", got, tc.want)
			}
			if tc.fails && !strings.Contains(console, "verdict=fail") {
				t.Fatalf("no failure on the console:\n%s", console)
			}
		})
	}
}

// THE SERVICE takes the registration out of its file before the listener runs, and ends
// the machine when a listener goes for good having run no job, and only then.
func TestTheWindowsServiceEndsAMachineThatRanNoJob(t *testing.T) {
	t.Parallel()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can run the service")
	}

	service, err := filepath.Abs(filepath.Join("windows", "billet-runner-service.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	hook, err := os.ReadFile(filepath.Join("windows", "billet-job-started.ps1"))
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		// statuses are what the listener exits with, one each run; none leaves no listener.
		statuses []int
		runHook  bool
		noFile   bool
		wantExit int
		wantRuns int
		wantStop bool
	}{
		"a job that ran":          {statuses: []int{100}, runHook: true, wantExit: 100, wantRuns: 1},
		"a job that failed":       {statuses: []int{101}, runHook: true, wantExit: 101, wantRuns: 1},
		"no job and a clean exit": {statuses: []int{0}, wantRuns: 1, wantStop: true},
		"a terminal error":        {statuses: []int{1}, wantRuns: 1, wantStop: true},
		"an unknown status":       {statuses: []int{9}, wantExit: 9, wantRuns: 1, wantStop: true},
		"a retry, then a job":     {statuses: []int{2, 100}, runHook: true, wantExit: 100, wantRuns: 2},
		"no listener":             {wantExit: 1, wantStop: true},
		"no registration":         {statuses: []int{100}, noFile: true, wantExit: 1, wantStop: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			image := t.TempDir()
			run := filepath.Join(image, "billet", "run")
			bin := filepath.Join(image, "actions-runner", "bin")
			for _, dir := range []string{run, bin} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(image, "billet", "billet-job-started.ps1"), hook, 0o600); err != nil {
				t.Fatal(err)
			}

			launch := filepath.Join(run, "launch.env")
			if !tc.noFile {
				if err := os.WriteFile(launch, []byte("ACTIONS_RUNNER_INPUT_JITCONFIG=a=b\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			runs := filepath.Join(image, "runs")
			stopped := filepath.Join(image, "stopped")
			if len(tc.statuses) > 0 {
				var script strings.Builder
				script.WriteString("#!/bin/sh\n")
				script.WriteString("test ! -e '" + launch + "' || echo still-there >>'" + runs + "'\n")
				script.WriteString("echo \"$ACTIONS_RUNNER_INPUT_JITCONFIG\" >>'" + runs + "'\n")
				if tc.runHook {
					script.WriteString("'" + pwsh + "' -NoProfile -NonInteractive -File \"$ACTIONS_RUNNER_HOOK_JOB_STARTED\"\n")
				}
				script.WriteString("case $(wc -l <'" + runs + "') in\n")
				for i, status := range tc.statuses {
					fmt.Fprintf(&script, "*%d) exit %d ;;\n", i+1, status)
				}
				script.WriteString("esac\nexit 99\n")
				if err := os.WriteFile(filepath.Join(bin, "Runner.Listener.exe"), []byte(script.String()), 0o700); err != nil {
					t.Fatal(err)
				}
			}

			stop := filepath.Join(image, "stop.ps1")
			if err := os.WriteFile(stop, []byte("Set-Content -LiteralPath '"+stopped+"' -Value stopped\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", service)
			cmd.Env = append(os.Environ(), "BILLET_AMI_IMAGE_ROOT="+image, "BILLET_AMI_SHUTDOWN="+stop)
			out, err := cmd.CombinedOutput()

			exit := 0
			if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
				exit = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("the service: %v\n%s", err, out)
			}
			if exit != tc.wantExit {
				t.Errorf("the service exited %d, want %d\n%s", exit, tc.wantExit, out)
			}

			if _, err := os.Stat(stopped); os.IsNotExist(err) == tc.wantStop {
				t.Errorf("the machine stopped: %v, want %v\n%s", err == nil, tc.wantStop, out)
			}

			if _, err := os.Stat(launch); !os.IsNotExist(err) {
				t.Errorf("the registration is still on the disk: %v", err)
			}

			said, err := os.ReadFile(runs)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			got := strings.Fields(string(said))
			if len(got) != tc.wantRuns {
				t.Fatalf("the listener ran %d times, want %d: %q", len(got), tc.wantRuns, got)
			}
			for _, line := range got {
				if line != "a=b" {
					t.Errorf("the listener saw %q, want the registration from its file, gone", line)
				}
			}
		})
	}
}

// A FILE STEP COPIES ITS SOURCES, and the declaration comes from the one copy billet
// vendors, held to its pin: an edit to it is an edit to every image built afterwards.
func TestAWindowsFileStepCopiesTheTreeAndHoldsTheDeclarationToItsPin(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		damage   bool
		wantFail bool
	}{
		"the pinned declaration":    {},
		"a declaration that is not": {damage: true, wantFail: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			into := filepath.Join(t.TempDir(), "image")
			step := windowsStep{ID: "file", Kind: "file", Destination: into,
				Sources: []string{"images/windows/assets", "images/windows/toolsets"}}

			root, env := windowsBuildFixture(t, []windowsStep{step}, "")

			repo := filepath.Join(root, "repo", "scripts", "runner-images", "upstream", "images", "windows", "assets")
			if err := os.MkdirAll(repo, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "asset.txt"), []byte("an asset\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			vendored := filepath.Join(root, "repo", "internal", "runnerimages")
			if err := os.MkdirAll(vendored, 0o755); err != nil {
				t.Fatal(err)
			}

			body, err := os.ReadFile(filepath.Join("..", "internal", "runnerimages", "toolset-2025.json"))
			if err != nil {
				t.Fatal(err)
			}
			pin, err := os.ReadFile(filepath.Join("..", "internal", "runnerimages", "pinned.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.damage {
				body = append(body, ' ')
			}
			if err := os.WriteFile(filepath.Join(vendored, "toolset-2025.json"), body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(vendored, "pinned.txt"), pin, 0o600); err != nil {
				t.Fatal(err)
			}

			console, _, err := runWindowsDriver(t, env)

			if tc.wantFail {
				if err == nil || !strings.Contains(console, "verdict=fail") {
					t.Fatalf("a declaration that is not the pinned one passed: %v\n%s", err, console)
				}

				return
			}

			if err != nil {
				t.Fatalf("the driver: %v\n%s", err, console)
			}
			if _, err := os.Stat(filepath.Join(into, "assets", "asset.txt")); err != nil {
				t.Errorf("the tree was not copied: %v", err)
			}
			if _, err := os.Stat(filepath.Join(into, "toolsets", "toolset-2025.json")); err != nil {
				t.Errorf("the declaration was not copied: %v", err)
			}
		})
	}
}

// THE LAYER CHECKS THE RUNNER BEFORE IT UNPACKS IT, against the digest the build was
// given, and installs what a tier's command names. Its account and the runner's own
// `--version` are Windows's, which the real build proves.
func TestTheWindowsLayerChecksTheRunnerBeforeItUnpacksIt(t *testing.T) {
	t.Parallel()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can run the layer")
	}

	layer, err := filepath.Abs(filepath.Join("windows", "ec2-layer.ps1"))
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	zipPath := filepath.Join(dir, "runner.zip")

	pack := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-Command",
		"Set-Content -LiteralPath (Join-Path $env:D 'run.cmd') -Value 'a runner'; "+
			"Compress-Archive -LiteralPath (Join-Path $env:D 'run.cmd') -DestinationPath $env:Z")
	pack.Env = append(os.Environ(), "D="+dir, "Z="+zipPath)
	if out, err := pack.CombinedOutput(); err != nil {
		t.Fatalf("make the archive: %v\n%s", err, out)
	}

	body, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)

	for name, tc := range map[string]struct {
		digest   string
		wantFail bool
	}{
		"the digest it was given": {digest: hex.EncodeToString(sum[:])},
		"another digest":          {digest: strings.Repeat("0", 64), wantFail: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, image := t.TempDir(), t.TempDir()
			settings := `{"nonce":"` + strings.Repeat("a", 32) + `","runner_version":"2.337.0","runner_sha256":"` +
				tc.digest + `","image_version":"billet"}`
			if err := os.WriteFile(filepath.Join(root, "build.json"), []byte(settings), 0o600); err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", layer)
			cmd.Env = append(os.Environ(), "BILLET_AMI_ROOT="+root, "BILLET_AMI_IMAGE_ROOT="+image,
				"BILLET_AMI_RUNNER_ZIP="+zipPath)
			out, err := cmd.CombinedOutput()

			unpacked := filepath.Join(image, "actions-runner", "run.cmd")
			mark := filepath.Join(image, "billet", "billet-image")

			if tc.wantFail {
				if err == nil {
					t.Fatalf("a runner that is not the pinned one was accepted:\n%s", out)
				}
				if _, statErr := os.Stat(unpacked); !os.IsNotExist(statErr) {
					t.Errorf("it was unpacked anyway: %v", statErr)
				}
				if _, statErr := os.Stat(mark); !os.IsNotExist(statErr) {
					t.Errorf("the image is marked finished: %v", statErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("the layer: %v\n%s", err, out)
			}
			for _, want := range []string{unpacked, mark} {
				if _, err := os.Stat(want); err != nil {
					t.Errorf("the layer did not install %s: %v", want, err)
				}
			}

			// BYTE FOR BYTE: the verifier holds each to its digest.
			for _, name := range []string{"billet-runner.ps1", "billet-runner-service.ps1", "billet-job-started.ps1"} {
				want, err := os.ReadFile(filepath.Join("windows", name))
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(image, "billet", name))
				if err != nil {
					t.Errorf("the layer did not install %s: %v", name, err)

					continue
				}
				if !bytes.Equal(got, want) {
					t.Errorf("the layer changed %s on its way", name)
				}
			}
		})
	}
}

// TIMING IS OBSERVATION: the hook prints one line when it can, and nothing, exit 0, when a
// timestamp is missing, malformed or reversed, because a failed hook would fail the job.
func TestTheWindowsJobHookIsOnlyEverAnObservation(t *testing.T) {
	t.Parallel()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can run the hook")
	}

	hook, err := filepath.Abs(filepath.Join("windows", "billet-job-started.ps1"))
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		launch, runner string
		wantLine       bool
	}{
		"ordered":     {launch: "1000000000", runner: "2000000000", wantLine: true},
		"reversed":    {launch: "2000000000", runner: "1000000000"},
		"a future":    {launch: "1000000000", runner: "9000000000000000000"},
		"malformed":   {launch: "not-a-number", runner: "2000000000"},
		"missing":     {},
		"zero launch": {launch: "0", runner: "2000000000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", hook)
			cmd.Env = append(os.Environ(), "BILLET_LAUNCH_EPOCH_NS="+tc.launch, "BILLET_RUNNER_START_EPOCH_NS="+tc.runner)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("the hook failed a job: %v\n%s", err, out)
			}
			if got := strings.Contains(string(out), "billet timing: launch_to_job_start_ms="); got != tc.wantLine {
				t.Errorf("printed a timing line %v, want %v:\n%s", got, tc.wantLine, out)
			}
			if tc.wantLine && !strings.Contains(string(out), " launch_to_runner_ms=1000 ") {
				t.Errorf("the line's arithmetic is wrong:\n%s", out)
			}
		})
	}
}

// AN EXIT CODE THE TEMPLATE NAMES IS A PASS, for the step that names it and no other.
func TestAWindowsStepPassesOnTheExitCodesTheTemplateNames(t *testing.T) {
	t.Parallel()

	rebooting := windowsStep{
		ID: "one", Kind: "powershell", ValidExitCodes: []int{0, 7},
		Inline: []string{"exit 7"},
	}
	_, env := windowsBuildFixture(t, []windowsStep{rebooting, markStep("two")}, "")

	console, marks, err := runWindowsDriver(t, env)
	if err != nil {
		t.Fatalf("the driver: %v\nconsole:\n%s", err, console)
	}
	if !strings.Contains(marks, "two") {
		t.Fatalf("the build stopped at a pass: %q", marks)
	}

	// AND A LATER STEP THAT EXITS THE SAME, NAMING NONE, FAILS.
	strict := windowsStep{ID: "two", Kind: "powershell", Inline: []string{"exit 7"}}
	_, env = windowsBuildFixture(t, []windowsStep{rebooting, strict, markStep("three")}, "")

	console, marks, err = runWindowsDriver(t, env)
	if err == nil || !strings.Contains(console, "template_step=2") {
		t.Fatalf("a code another step named let this one pass: %v\n%s", err, console)
	}
	if strings.Contains(marks, "three") {
		t.Fatalf("the build ran past its failure: %q", marks)
	}
}

// A DIFFERENCE NAMING NO STEP, AND A STEP THE DRIVER CANNOT RUN, each stop the build
// before anything runs or where it stands: a pin bump to review, not a line to ignore.
func TestAWindowsPlanThatHasMovedStopsTheBuild(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		plan        []windowsStep
		differences string
		wantMarks   []string
	}{
		"a difference naming no step": {
			plan:        []windowsStep{markStep("one")},
			differences: "skip\tgone\tno longer in the template\n",
		},
		"a kind the driver cannot run": {
			plan:      []windowsStep{markStep("one"), {ID: "restart", Kind: "windows-restart"}},
			wantMarks: []string{"one"},
		},
		"an action the driver does not know": {
			plan:        []windowsStep{markStep("one")},
			differences: "replace\tone\twith something\n",
		},
		"a difference with no reason": {
			plan:        []windowsStep{markStep("one")},
			differences: "skip\tone\t\n",
		},
		"a step listed twice": {
			plan:        []windowsStep{markStep("one")},
			differences: "skip\tone\tonce\nskip\tone\ttwice\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, env := windowsBuildFixture(t, tc.plan, tc.differences)

			console, marks, err := runWindowsDriver(t, env)
			if err == nil {
				t.Fatalf("the driver passed:\n%s", console)
			}
			if !strings.Contains(console, "verdict=fail") {
				t.Fatalf("no failure on the console:\n%s", console)
			}
			if got := strings.Fields(marks); !slices.Equal(got, tc.wantMarks) {
				t.Fatalf("the build ran %q, want %q", got, tc.wantMarks)
			}
		})
	}
}

// A SCRIPT STEP IS GITHUB'S FILE, run as Packer runs it: its exit status is the verdict,
// a cmdlet's complaint is not, and the template's variables reach it. Nearly every step
// the floor runs is this branch.
func TestAWindowsScriptStepRunsAsPackerRunsIt(t *testing.T) {
	t.Parallel()

	const relative = "images/windows/scripts/build/Install-Thing.ps1"

	for name, tc := range map[string]struct {
		script   string
		wantFail bool
		want     []string
	}{
		"a script that finishes": {
			script: "Add-Content -LiteralPath $env:BILLET_TEST_MARKS -Value \"ran-$env:THE_VERSION\"\n",
			want:   []string{"ran-billet", "layer", "shutdown"},
		},
		"a script a cmdlet complains in": {
			script: "Write-Error 'a complaint'\nAdd-Content -LiteralPath $env:BILLET_TEST_MARKS -Value carried-on\n",
			want:   []string{"carried-on", "layer", "shutdown"},
		},
		"a script that exits non-zero": {script: "exit 3\n", wantFail: true},
		"a script that throws":         {script: "throw 'no'\n", wantFail: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			step := windowsStep{ID: "Install-Thing.ps1", Kind: "powershell", Script: relative,
				Env: map[string]string{"THE_VERSION": "@image_version@"}}
			root, env := windowsBuildFixture(t, []windowsStep{step}, "")

			path := filepath.Join(root, "repo", "scripts", "runner-images", "upstream", filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.script), 0o600); err != nil {
				t.Fatal(err)
			}

			console, marks, err := runWindowsDriver(t, env)
			if tc.wantFail != (err != nil) || tc.wantFail != strings.Contains(console, "verdict=fail") {
				t.Fatalf("the driver: %v, want a failure %v\n%s", err, tc.wantFail, console)
			}
			if got := strings.Fields(marks); !slices.Equal(got, tc.want) {
				t.Fatalf("the build ran %q, want %q", got, tc.want)
			}
		})
	}
}

// WHAT FOLLOWS THE TEMPLATE CAN FAIL TOO, and each says where, with no verdict of success
// and, before the verdict, no shutdown.
func TestAWindowsBuildCanFailAfterItsTemplate(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		damage    func(root string) error
		wantStep  string
		wantMarks []string
		wantOK    bool
	}{
		"a layer that fails": {
			damage: func(root string) error {
				return os.WriteFile(filepath.Join(root, "layer.ps1"), []byte("exit 3\n"), 0o600)
			},
			wantStep: "layer", wantMarks: []string{"one"},
		},
		"a cleanup that cannot finish": {
			// A STEP TAKES THE FILE CLEANUP MUST REMOVE, so a success cannot be about this disk.
			damage: func(root string) error {
				return os.WriteFile(filepath.Join(root, "layer.ps1"), []byte("Remove-Item -LiteralPath '"+
					filepath.Join(root, "build.json")+"'\nAdd-Content -LiteralPath $env:BILLET_TEST_MARKS -Value layer\n"), 0o600)
			},
			wantStep: "cleanup", wantMarks: []string{"one", "layer"},
		},
		"a shutdown that fails": {
			damage: func(root string) error {
				return os.WriteFile(filepath.Join(root, "shutdown.ps1"), []byte("throw 'no'\n"), 0o600)
			},
			wantStep: "cleanup", wantMarks: []string{"one", "layer"}, wantOK: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			root, env := windowsBuildFixture(t, []windowsStep{markStep("one")}, "")
			if err := tc.damage(root); err != nil {
				t.Fatal(err)
			}

			console, marks, err := runWindowsDriver(t, env)
			if err == nil {
				t.Fatalf("the driver passed:\n%s", console)
			}
			if !strings.Contains(console, "verdict=fail") || !strings.Contains(console, "step="+tc.wantStep) {
				t.Errorf("the verdict does not say %s:\n%s", tc.wantStep, console)
			}
			if got := strings.Contains(console, "verdict=ok"); got != tc.wantOK {
				t.Errorf("a success on the console: %v, want %v\n%s", got, tc.wantOK, console)
			}
			if got := strings.Fields(marks); !slices.Equal(got, tc.wantMarks) {
				t.Errorf("the build ran %q, want %q", got, tc.wantMarks)
			}
		})
	}
}

// A BUILD.JSON NAMING NO BUILD STOPS THE DRIVER BEFORE IT TOUCHES ANYTHING.
func TestTheWindowsDriverRefusesSettingsNamingNoBuild(t *testing.T) {
	t.Parallel()

	root, env := windowsBuildFixture(t, []windowsStep{markStep("one")}, "")
	if err := os.WriteFile(filepath.Join(root, "build.json"), []byte(`{"nonce":"$(no)"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	console, marks, err := runWindowsDriver(t, env)
	if err == nil {
		t.Fatalf("the driver passed:\n%s", console)
	}
	if marks != "" || strings.Contains(console, "$(no)") {
		t.Fatalf("the driver went on: marks %q, console:\n%s", marks, console)
	}
}

// A PREPARE IS A STEP THE BUILD WILL RUN: on a step the plan has, not also skipped, and
// carried in what a build embeds, because the driver finds each out an hour too late.
func TestEveryWindowsPrepareIsOneTheBuildCanRun(t *testing.T) {
	t.Parallel()

	plan := readWindowsPlan(t, windowsTemplateText(t))
	skips := readWindowsSkips(t)

	files, err := scripts.AMIBuildWindows()
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(runnerImagesDir, windowsDifferences))
	if err != nil {
		t.Fatal(err)
	}

	prepares := 0

	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Split(line, "\t")

		name, ok := strings.CutPrefix(fields[0], "prepare:")
		if !ok {
			continue
		}

		prepares++

		if !slices.ContainsFunc(plan, func(s windowsStep) bool { return s.ID == fields[1] }) {
			t.Errorf("%s names %s, which the plan does not have", fields[0], fields[1])
		}
		if _, skipped := skips[fields[1]]; skipped {
			t.Errorf("%s is listed as a prepare and a skip", fields[1])
		}

		want := "windows/prepare-" + name + ".ps1"
		if !slices.ContainsFunc(files, func(f scripts.AMIBuildFile) bool { return f.Path == want }) {
			t.Errorf("%s names %s, which a build does not carry", fields[0], want)
		}
	}

	if prepares == 0 {
		t.Fatal("no prepare was read, so this held nothing")
	}
}

// THE ENTRY POINT REFUSES WHAT IT CANNOT CARRY, before it touches the machine, and never
// says the value. The rest of it is Windows's, and the acceptance record shows it running.
func TestTheWindowsEntryRefusesWhatCannotReachTheRunner(t *testing.T) {
	t.Parallel()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can run the entry point")
	}

	entry, err := filepath.Abs(filepath.Join("windows", "billet-runner.ps1"))
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		env  []string
		want string
	}{
		"no registration": {
			env:  []string{"ACTIONS_RUNNER_INPUT_JITCONFIG="},
			want: "no registration reached this script",
		},
		"a registration holding a newline": {
			env:  []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=secret\nvalue"},
			want: "ACTIONS_RUNNER_INPUT_JITCONFIG holds a newline",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", entry)
			cmd.Env = append(os.Environ(), tc.env...)

			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the entry point passed:\n%s", out)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Errorf("it said %s, want %q", out, tc.want)
			}
			if strings.Contains(string(out), "secret") {
				t.Errorf("it quoted the registration:\n%s", out)
			}
		})
	}
}

// A MARK THAT CANNOT BE WRITTEN FAILS NO JOB.
func TestTheWindowsJobHookSurvivesAMarkItCannotWrite(t *testing.T) {
	t.Parallel()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can run the hook")
	}

	hook, err := filepath.Abs(filepath.Join("windows", "billet-job-started.ps1"))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", hook)
	cmd.Env = append(os.Environ(), "BILLET_JOB_STARTED_MARK="+filepath.Join(t.TempDir(), "no", "such", "dir", "mark"))

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the hook failed a job: %v\n%s", err, out)
	}
}

// PAGING IS RESTORED ON THE DISK BEING IMAGED, before the verdict that says the disk is
// finished, and with no file: that branch runs only on Windows, so the verifier's report of
// a page file is what shows it working.
func TestTheWindowsDriverRestoresPagingBeforeItsVerdict(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("windows", "build-ami.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	driver := string(raw)

	restore := strings.Index(driver, "-Name PagingFiles -Value @('?:\\pagefile.sys') -Type MultiString")
	managed := strings.Index(driver, "AutomaticManagedPagefile = $true")
	verdict := strings.Index(driver, "Write-Console (Get-Block 'ok' 'done' '')")

	if restore < 0 || managed < 0 || verdict < 0 || restore > verdict || managed > verdict {
		t.Fatalf("paging is not restored before the verdict: %d, %d, %d", restore, managed, verdict)
	}
	if strings.Index(driver, "a page file appeared during the build") > restore {
		t.Error("the driver does not refuse a file before it restores paging")
	}
}
