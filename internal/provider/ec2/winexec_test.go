package ec2

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/scripts"
)

// windowsStubs shadows what only Windows has, so a rendered script's own logic runs on
// any machine with pwsh. A function wins over a cmdlet or a program of the same name.
// Each stub records to $env:STUB_LOG, and the serial port writes to $env:STUB_CONSOLE.
// A sleep with BILLET_STUB_SLEEP_ENDS set ends the script, which is how a script that
// reports for as long as the machine runs is let go.
const windowsStubs = `function Note([string]$what) { Add-Content -LiteralPath $env:STUB_LOG -Value $what }
function New-Object {
	if ($args -contains 'System.IO.Ports.SerialPort') {
		$port = [pscustomobject]@{}
		$port | Add-Member ScriptMethod Open { }
		$port | Add-Member ScriptMethod WriteLine { param($line) Add-Content -LiteralPath $env:STUB_CONSOLE -Value $line }
		return $port
	}
	if ($args[0] -like 'Security.AccessControl.*') { return $null }
	Microsoft.PowerShell.Utility\New-Object @args
}
function Start-Sleep { if ($env:BILLET_STUB_SLEEP_ENDS) { exit 9 } }
function Stop-Computer { Note 'stop' }
function Restart-Computer { Note 'restart' }
function shutdown.exe { Note "shutdown $args" }
function bcdedit { Note "bcdedit $args"; $global:LASTEXITCODE = 0 }
function Get-Acl { $a = [pscustomobject]@{}; $a | Add-Member ScriptMethod AddAccessRule { param($r) }; return $a }
function Set-Acl { Note 'set-acl' }
function Set-LocalUser { Note 'set-password' }
function New-ScheduledTaskAction { }
function Register-ScheduledTask { Note 'register' }
function Unregister-ScheduledTask { Note 'unregister' }
function Start-ScheduledTask { Note 'start'; if ($env:STUB_TASK_WRITES) { Copy-Item -LiteralPath $env:STUB_TASK_WRITES -Destination $env:STUB_RESULT } }
function Get-ScheduledTask { [pscustomobject]@{ State = 'Ready' } }
function Get-ScheduledTaskInfo { [pscustomobject]@{ LastTaskResult = [int]$env:STUB_TASK_RESULT } }
function Get-PSDrive { [pscustomobject]@{ Free = [long]$env:STUB_FREE_BYTES; Used = 1 } }
function Invoke-WebRequest { param([switch]$UseBasicParsing, $Uri, $OutFile) Note 'fetch'; Copy-Item -LiteralPath $env:STUB_FETCHES -Destination $OutFile }
function tar.exe { Note 'unpack'; Copy-Item -LiteralPath $env:STUB_SETTINGS -Destination $env:STUB_SETTINGS_TO; $global:LASTEXITCODE = 0 }
function powershell.exe { Note 'driver'; $global:LASTEXITCODE = 0 }
`

// runStubbed runs body under pwsh after the stubs, and answers its console, what the
// stubs noted, and its exit status.
func runStubbed(t *testing.T, dir, body string, env ...string) (string, string, int) {
	t.Helper()

	pwsh := lookPwsh(t)

	script := filepath.Join(dir, "under-test.ps1")
	if err := os.WriteFile(script, []byte(windowsStubs+body), 0o600); err != nil {
		t.Fatal(err)
	}

	console, log := filepath.Join(dir, "console"), filepath.Join(dir, "log")

	cmd := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-File", script)
	cmd.Env = append(append(os.Environ(), "STUB_CONSOLE="+console, "STUB_LOG="+log, "STUB_FREE_BYTES="+
		strconv.FormatInt(500<<30, 10), "STUB_TASK_RESULT=0"), env...)

	out, err := cmd.CombinedOutput()

	status := 0
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		status = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("pwsh: %v\n%s", err, out)
	}

	if testing.Verbose() {
		t.Logf("pwsh said:\n%s", out)
	}

	read := func(path string) string {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}

		return string(raw)
	}

	return read(console), read(log), status
}

func stripTags(script string) string {
	return strings.TrimSuffix(strings.TrimPrefix(script, "<powershell>\n"), "</powershell>\n")
}

// A BOOT SCRIPT WHOSE COMMAND FAILS ENDS THE MACHINE, whichever way it fails, and one that
// starts leaves it alone. The registration reaches the command in its environment.
func TestAWindowsBootEndsTheMachineOnlyIfItsCommandFails(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		command  string
		wantStop bool
	}{
		"a command that starts":  {command: "exit 0\n"},
		"a command that exits 3": {command: "exit 3\n", wantStop: true},
		"a command that throws":  {command: "throw 'no'\n", wantStop: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			seen := filepath.Join(dir, "seen")
			command := filepath.Join(dir, "command.ps1")
			body := "Set-Content -LiteralPath '" + seen + "' -Value \"$env:" + jitEnvVar +
				" $env:BILLET_LAUNCH_EPOCH_NS\"\n" + tc.command
			if err := os.WriteFile(command, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			spec := windowsSpec()
			spec.Command = []string{command}

			script, err := windowsUserData(spec, 1234)
			if err != nil {
				t.Fatal(err)
			}

			_, log, _ := runStubbed(t, dir, stripTags(script))

			if got := strings.Contains(log, "stop"); got != tc.wantStop {
				t.Errorf("the machine stopped: %v, want %v", got, tc.wantStop)
			}

			said, err := os.ReadFile(seen)
			if err != nil {
				t.Fatal(err)
			}
			if want := validSpec().JITConfig + " 1234"; strings.TrimSpace(string(said)) != want {
				t.Errorf("the command saw %q, want %q", said, want)
			}
		})
	}
}

// windowsVerifierFixture is a machine the verifier can run on: the entry points as the
// image carries them, unless one is damaged, and a task that writes what it is told.
func windowsVerifierFixture(t *testing.T, dir string, damage bool) string {
	t.Helper()

	script, _, err := windowsVerifyScript("x64", verifyProbeNonce)
	if err != nil {
		t.Fatal(err)
	}

	files, err := scripts.AMIBuildWindows()
	if err != nil {
		t.Fatal(err)
	}

	image := filepath.Join(dir, "image")
	if err := os.MkdirAll(image, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		name := filepath.Base(f.Path)
		if name != "billet-runner.ps1" && name != "billet-runner-service.ps1" && name != "billet-job-started.ps1" {
			continue
		}

		body := f.Body
		if damage && name == "billet-runner.ps1" {
			body = append(append([]byte{}, body...), '\n')
		}

		if err := os.WriteFile(filepath.Join(image, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return strings.NewReplacer(`C:\billet\verify`, filepath.Join(image, "verify"), `C:\billet\`, image+"/").
		Replace(stripTags(script))
}

// THE VERIFIER'S VERDICT, executed: ok is earned by a finished check of this verification,
// with the task's own status agreeing, and only then. What it prints is read back through
// the parser billet stamps by, so a key the schema drops would show here.
func TestTheWindowsVerifierEarnsItsVerdict(t *testing.T) {
	t.Parallel()

	const finished = "step=account\nstep=runner\nrunner=2.337.0\nstep=tools\nstep=environment\n" +
		"image_os=win25\nstep=toolcache\nstep=disk\nroot_free_gib=100\nstep=done\n"

	for name, tc := range map[string]struct {
		result      string
		resultNonce string
		taskResult  int
		damage      bool
		wantVerdict string
		wantStep    string
	}{
		"a finished check":       {result: finished, wantVerdict: verdictOK, wantStep: "done"},
		"a check that stopped":   {result: "step=account\nstep=tools\n", wantVerdict: verdictFail, wantStep: "tools"},
		"a task that failed":     {result: finished, taskResult: 1, wantVerdict: verdictFail, wantStep: "done"},
		"no check at all":        {wantVerdict: verdictFail, wantStep: "account"},
		"another build's result": {result: finished, resultNonce: strings.Repeat("b", 32), wantVerdict: verdictFail, wantStep: "account"},
		"a damaged entry point":  {result: finished, damage: true, wantVerdict: verdictFail, wantStep: "entry"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			body := windowsVerifierFixture(t, dir, tc.damage)

			_, schema, err := windowsVerifyScript("x64", verifyProbeNonce)
			if err != nil {
				t.Fatal(err)
			}

			env := []string{"STUB_TASK_RESULT=" + strconv.Itoa(tc.taskResult)}
			if tc.result != "" {
				fixture := filepath.Join(dir, "written")
				if err := os.WriteFile(fixture, []byte(tc.result), 0o600); err != nil {
					t.Fatal(err)
				}

				nonce := tc.resultNonce
				if nonce == "" {
					nonce = verifyProbeNonce
				}

				env = append(env, "STUB_TASK_WRITES="+fixture,
					"STUB_RESULT="+filepath.Join(dir, "image", "verify", "result-"+nonce+".txt"))
			}

			console, log, _ := runStubbed(t, dir, body, env...)

			report, ok := parseReport(console, verifyProbeNonce, schema)
			if !ok {
				t.Fatalf("billet cannot read this report:\n%s\nnotes:\n%s", console, log)
			}
			if report[reportVerdictKey] != tc.wantVerdict || report[reportStepKey] != tc.wantStep {
				t.Errorf("the report says %s at %s, want %s at %s:\n%s", report[reportVerdictKey],
					report[reportStepKey], tc.wantVerdict, tc.wantStep, console)
			}

			// ITS OWN END, whatever it found: the timed one is set first and called off last.
			if !strings.Contains(log, "shutdown /s /t") || !strings.Contains(log, "shutdown /a") || !strings.Contains(log, "stop") {
				t.Errorf("the verifier did not end itself:\n%s", log)
			}
		})
	}
}

// WHAT THE CHECK ASKS, by the lines that ask it. Each is a clause of the contract, and each
// runs only on Windows, so what a mutation would remove is all this can hold; the real
// build's report is what shows them answering.
func TestTheWindowsVerifierCheckAsksEveryClause(t *testing.T) {
	t.Parallel()

	script, schema, err := windowsVerifyScript("x64", verifyProbeNonce)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"$out = 'C:\\billet\\verify\\result-" + verifyProbeNonce + ".txt'\n",
		"foreach ($line in Get-Content -LiteralPath \"$dir\\result-" + verifyProbeNonce + ".txt\") {\n",
		"if ($env:USERNAME -ine 'runner') { throw 'this is not the runner account' }\n",
		"if (-not $id.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'the runner account is not elevated' }\n",
		"Ask-Tool 'runner' '" + windowsRunnerListener + "' '--version'\n",
		"Ask-Tool 'git' 'git' '--version'\n", "Ask-Tool 'pwsh' 'pwsh' '--version'\n",
		"Ask-Tool 'node' 'node' '--version'\n", "Ask-Tool 'python' 'python' '--version'\n",
		"Ask-Tool 'go' 'go' 'version'\n", "Ask-Tool 'ruby' 'ruby' '--version'\n",
		"Ask-Tool 'bash' 'bash' '--version'\n",
		"if ([string]@(& pwsh --version)[0] -cnotlike 'PowerShell 7.6.*') { throw 'pwsh is not the declared line' }\n",
		"if ($env:ImageOS -cne 'win25') { throw 'ImageOS is not win25' }\n",
		"if ($env:RUNNER_TOOL_CACHE -ine 'C:\\hostedtoolcache\\windows') {",
		"if ($env:AGENT_TOOLSDIRECTORY -ine $env:RUNNER_TOOL_CACHE) {",
		"if ($drive.Free -lt (" + strconv.Itoa(minVerifiedFreeGiB) + " * 1GB)) { throw 'the root is nearly full' }\n",
		"if ($paging.Count -eq 0) { throw 'this image booted with no page file' }\n",
		"Step 'done'\n",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the verifier does not say %q", want)
		}
	}

	if n := strings.Count(script, "result-"+verifyProbeNonce+".txt"); n != 2 {
		t.Errorf("the result file is named %d times, want the writer and the reader", n)
	}

	// THE GUEST'S OWN END COMES AFTER BILLET'S DEADLINE, or a slow image is shot mid-report.
	m := regexp.MustCompile(`shutdown\.exe /s /t (\d+)`).FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("the verifier sets no end of its own:\n%s", script)
	}
	if seconds, err := strconv.Atoi(m[1]); err != nil || seconds <= int(windowsVerifyWindow.Seconds()) {
		t.Errorf("the verifier ends itself after %s s, inside billet's %s", m[1], windowsVerifyWindow)
	}

	// EVERYTHING IT WRITES, THE SCHEMA READS BACK: parseReport drops a line it refuses, and
	// a blank step is all a failed check would then say.
	for _, pattern := range []string{`Say '(\w+)'`, `Ask-Tool '(\w+)'`, `Ask-Toolcache '(\w+)'`} {
		for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(script, -1) {
			if !schema.allows(match[1], "1") {
				t.Errorf("the schema drops %s, which the script writes", match[1])
			}
		}
	}

	found := 0

	for _, pattern := range []string{`Step '(\w+)'`, `\$step = '(\w+)'`} {
		for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(script, -1) {
			found++

			if !schema.allows(reportStepKey, match[1]) {
				t.Errorf("the schema drops the step %s, which the script names", match[1])
			}
		}
	}

	if found < len(windowsVerifySteps) {
		t.Errorf("read %d steps from the script and the schema has %d", found, len(windowsVerifySteps))
	}
}

// secondBootFixture is the second boot on a machine whose paths are under dir, its payload
// the bytes it will fetch.
func secondBootFixture(t *testing.T, dir string, digest *string) (string, []string) {
	t.Helper()

	payload := filepath.Join(dir, "fetched")
	if err := os.WriteFile(payload, []byte("the payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256([]byte("the payload"))

	spec := testWindowsBuild()
	spec.payloadSHA256 = hex.EncodeToString(sum[:])

	if digest != nil {
		spec.payloadSHA256 = *digest
	}

	first, err := windowsBootstrapScript(spec)
	if err != nil {
		t.Fatal(err)
	}

	second, _, ok := strings.Cut(strings.SplitN(first, "@'\n", 2)[1], "\n'@\n")
	if !ok {
		t.Fatalf("no second boot in the bootstrap:\n%s", first)
	}

	build := filepath.Join(dir, "build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		t.Fatal(err)
	}

	settings := filepath.Join(dir, "settings")
	if err := os.WriteFile(settings, []byte(`{"nonce":"`+verifyProbeNonce+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(build, "bootstrap.ps1"), []byte("the bootstrap"), 0o600); err != nil {
		t.Fatal(err)
	}

	return strings.NewReplacer(windowsBuildDir, build, windowsImageMark, filepath.Join(dir, "mark"),
			windowsPageFile, filepath.Join(dir, "pagefile.sys")).Replace(second),
		[]string{
			"BILLET_STUB_SLEEP_ENDS=1", "STUB_FETCHES=" + payload, "STUB_SETTINGS=" + settings,
			"STUB_SETTINGS_TO=" + filepath.Join(build, "build.json"),
		}
}

// THE SECOND BOOT, executed: everything that makes this the wrong payload or the wrong
// machine says so on the console under this build's nonce, and starts no driver.
func TestTheWindowsSecondBootStartsTheDriverOnlyOnWhatItChecked(t *testing.T) {
	t.Parallel()

	const fails = "bootstrap"

	wrong := strings.Repeat("0", 64)

	for name, tc := range map[string]struct {
		digest   *string
		mark     bool
		free     string
		settings string
		wantStep string
	}{
		"everything in order":   {},
		"another payload":       {digest: &wrong, wantStep: fails},
		"a finished machine":    {mark: true, wantStep: fails},
		"too little disk":       {free: "1", wantStep: fails},
		"another build's files": {settings: `{"nonce":"` + strings.Repeat("c", 32) + `"}`, wantStep: fails},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			body, env := secondBootFixture(t, dir, tc.digest)

			// THE IMAGE'S FIRST BOOT WOULD COLD-READ THIS FROM THE SNAPSHOT.
			if err := os.WriteFile(filepath.Join(dir, "pagefile.sys"), []byte("paged"), 0o600); err != nil {
				t.Fatal(err)
			}

			if tc.mark {
				if err := os.WriteFile(filepath.Join(dir, "mark"), []byte("2.337.0"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.free != "" {
				env = append(env, "STUB_FREE_BYTES="+tc.free)
			}
			if tc.settings != "" {
				if err := os.WriteFile(filepath.Join(dir, "settings"), []byte(tc.settings), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			console, log, _ := runStubbed(t, dir, body, env...)

			if !strings.Contains(log, "unregister") {
				t.Errorf("the task that started this boot is still registered:\n%s", log)
			}

			if tc.wantStep != "" {
				if strings.Contains(log, "driver") {
					t.Fatalf("the driver started:\n%s", log)
				}

				for _, want := range []string{
					buildBegin + " " + verifyProbeNonce, reportVerdictKey + "=" + verdictFail,
					reportStepKey + "=" + tc.wantStep, buildEnd + " " + verifyProbeNonce,
				} {
					if !strings.Contains(console, want) {
						t.Errorf("the console does not say %q:\n%s", want, console)
					}
				}

				return
			}

			if console != "" {
				t.Errorf("a boot in order reported:\n%s", console)
			}

			// THE URL AND THE PAYLOAD ARE GONE BEFORE THE DRIVER RUNS, and it is what ran last.
			if !strings.HasSuffix(strings.TrimSpace(log), "driver") {
				t.Errorf("the driver is not what the boot came to:\n%s", log)
			}
			for _, gone := range []string{
				filepath.Join("build", "bootstrap.ps1"), filepath.Join("build", "payload.tar.gz"), "pagefile.sys",
			} {
				if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
					t.Errorf("%s is still on the disk: %v", gone, err)
				}
			}
		})
	}
}

// THE PRESIGNED URL IS WRITTEN ONLY TO A DIRECTORY ALREADY CLOSED, and the reboot COM1 needs
// comes after everything that has to survive it.
func TestTheWindowsFirstBootClosesTheDirectoryBeforeItWritesTheURL(t *testing.T) {
	t.Parallel()

	script, err := windowsBootstrapScript(testWindowsBuild())
	if err != nil {
		t.Fatal(err)
	}

	order := []string{
		"$acl.SetAccessRuleProtection($true, $false)", "Set-Acl -LiteralPath '" + windowsBuildDir + "'",
		"Set-Content -LiteralPath '" + windowsBootstrap + "'", testPayloadURL, "Register-ScheduledTask",
		"AutomaticManagedPagefile = $false", "Win32_PageFileSetting | Remove-CimInstance",
		"-Name PagingFiles -Value @('') -Type MultiString",
		"bcdedit /ems '{current}' off", "bcdedit /bootems '{current}' off", "Restart-Computer -Force",
	}

	last := -1

	for _, want := range order {
		at := strings.Index(script, want)
		if at < 0 {
			t.Fatalf("the bootstrap does not say %q:\n%s", want, script)
		}
		if at < last {
			t.Errorf("%q comes too early in the bootstrap:\n%s", want, script)
		}

		last = at
	}

	for _, who := range []string{`'NT AUTHORITY\SYSTEM', 'BUILTIN\Administrators') {`} {
		if !strings.Contains(script, who) {
			t.Errorf("the directory does not name exactly %q", who)
		}
	}
}

// A BUILDER'S AND A VERIFIER'S USER DATA GO THROUGH THE RULE: a helper that refuses is
// nothing if a caller compresses around it.
func TestAWindowsScriptPastTheLimitBuysNothing(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("x", maxUserData+1)

	f := newFakeEC2(t)
	f.respond = func(action string, _ url.Values) (int, string) {
		if action == "DescribeImages" {
			return http.StatusOK, windowsImageReply
		}

		return http.StatusOK, defaultReply(action)
	}
	p := newTestProvider(t, f, nil)

	const says = "EC2Launch documents no compressed user data"

	if _, err := p.launchBuilder(t.Context(), testWindowsBuild(), oversized); err == nil ||
		!strings.Contains(err.Error(), says) {
		t.Errorf("a builder with a Windows script past the limit was answered %v", err)
	}

	if _, err := p.launchVerifier(t.Context(), VerifySpec{Image: "ami-0abc"},
		imageLayout{platform: imagePlatformWindows}, "x64", oversized, verifyProbeNonce); err == nil ||
		!strings.Contains(err.Error(), says) {
		t.Errorf("a verifier with a Windows script past the limit was answered %v", err)
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("%d instances were asked for", n)
	}
}

// THE COMMAND A TIER RUNS BY DEFAULT IS THE FILE THE IMAGE INSTALLS, the entry the verifier
// holds to its digest, which also says the registration's variable.
func TestAWindowsTiersCommandIsTheEntryPointTheImageCarries(t *testing.T) {
	t.Parallel()

	if config.WindowsRunnerCommand != `C:\billet\billet-runner.ps1` {
		t.Errorf("a Windows tier runs %q", config.WindowsRunnerCommand)
	}

	files, err := scripts.AMIBuildWindows()
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		if f.Path == scripts.WindowsRunnerEntry {
			if !strings.Contains(string(f.Body), "'"+jitEnvVar+"'") {
				t.Errorf("the entry point does not read %s", jitEnvVar)
			}

			return
		}
	}

	t.Fatalf("the build carries no %s", scripts.WindowsRunnerEntry)
}

// A STOPPED INSTANCE HAS ENDED, which lets a drain stop waiting on it and authorises
// nothing else; a state billet does not know has not.
func TestEC2ReportsAStoppedInstanceAsEnded(t *testing.T) {
	t.Parallel()

	for state, want := range map[string]bool{
		"running": false, "pending": false, "stopping": false, "shutting-down": false,
		"stopped": true, "something-new": false,
	} {
		f := newFakeEC2(t)
		f.respond = func(action string, _ url.Values) (int, string) {
			if action == "DescribeInstances" {
				return http.StatusOK, describeReply("", instanceXMLInState("i-123", "billet-lease-1", state))
			}

			return http.StatusOK, defaultReply(action)
		}

		list, err := newTestProvider(t, f, nil).List(t.Context())
		if err != nil || len(list) != 1 {
			t.Fatalf("%s: List = %v, %v", state, list, err)
		}
		if list[0].Ended != want {
			t.Errorf("%s: Ended = %v, want %v", state, list[0].Ended, want)
		}
	}
}

// WHY A BOOTSTRAP FAILED IS SAID, since it never reached the driver's logs.
func TestAWindowsBootstrapFailureNamesWhatCouldHaveFailed(t *testing.T) {
	t.Parallel()

	err := windowsBuildFailure("i-1", map[string]string{reportStepKey: bootstrapStep})
	if err == nil || !strings.Contains(err.Error(), "before the build started") ||
		!strings.Contains(err.Error(), "could not be fetched") {
		t.Errorf("the failure says %v", err)
	}

	err = windowsBuildFailure("i-1", map[string]string{reportStepKey: "layer"})
	if err == nil || !strings.Contains(err.Error(), windowsBuildDir+`\logs`) {
		t.Errorf("the failure does not send the operator to the logs: %v", err)
	}
}
