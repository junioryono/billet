package scripts_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
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
		if len(fields) != 3 || fields[0] != "skip" || fields[2] == "" {
			t.Fatalf("a difference that is not skip, a step and a reason: %q", line)
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

// EVERY SCRIPT BILLET WRITES PARSES as what runs it, PowerShell 5.1's language by way of
// PowerShell's own parser, on a machine that has one.
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
		filepath.Join(root, "layer.ps1"):    []byte("Add-Content -LiteralPath $env:BILLET_TEST_MARKS -Value layer\n"),
		filepath.Join(root, "shutdown.ps1"): []byte("Add-Content -LiteralPath $env:BILLET_TEST_MARKS -Value shutdown\n"),
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
	}
}

func runWindowsDriver(t *testing.T, env []string) (string, string, error) {
	t.Helper()

	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("no pwsh on this machine, so nothing here can run the driver")
	}

	driver, err := filepath.Abs(filepath.Join("windows", "build-ami.ps1"))
	if err != nil {
		t.Fatal(err)
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
