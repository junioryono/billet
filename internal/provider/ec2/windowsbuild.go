package ec2

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/runnerimages"
	"github.com/junioryono/billet/internal/runnerrelease"
	"github.com/junioryono/billet/scripts"
)

// WindowsAMIContract is what a Windows runner AMI must satisfy for this billet to use
// it, and goes up whenever one has to carry a newly required property. It counts apart
// from AMIContract, whose every number is about a Linux image (ADR-024).
//
// 1: the image is the steps of GitHub's windows-2025 template that
// differences-windows.tsv leaves in, with billet's EC2 layer after them, and a process
// started the way a job's runner is finds the runner, git, PowerShell 7, the default
// toolchains and every declared toolcache line.
//
// IT STAYS UNDER 4 UNTIL verify.go SAYS OTHERWISE: `contract >= 4` there means a Linux
// image's components record, which a Windows image has none of.
const WindowsAMIContract = 1

// windowsRunnerPlatform is actions/runner's asset name for the one Windows build
// billet installs. GitHub publishes windows-2025's declaration for x64 alone.
const windowsRunnerPlatform = "win-x64"

// The builder's root on a Windows build.
//
// THE BASE IMAGE LEAVES ALMOST NOTHING: Windows Server 2025's 30 GiB root held 21.3 GiB
// used and 8.2 free on a booted c7i.large (2026-11-04).
const (
	// DefaultWindowsBuilderDiskGiB is the root a Windows build gets when nobody says,
	// and through the job path's clamp every job's. Exported for the flag's help.
	DefaultWindowsBuilderDiskGiB = 150
	minWindowsBuilderFreeGiB     = 40
	minWindowsBuilderDiskGiB     = 75
)

// The builder's paths on a Windows build, which the payload's members and the scripts
// it carries agree on.
const (
	windowsBuildDir  = `C:\billet-ami`
	windowsBootstrap = windowsBuildDir + `\bootstrap.ps1`
	windowsPayload   = windowsBuildDir + `\payload.tar.gz`
	windowsSettings  = windowsBuildDir + `\build.json`
	windowsDriver    = windowsBuildDir + `\repo\scripts\windows\build-ami.ps1`
	windowsBuildTask = "billet-ami-build"
	windowsImageMark = `C:\billet\billet-image`
	windowsPageFile  = `C:\pagefile.sys`
)

// PinnedWindowsRunnerSHA256 is the digest billet pins for the pinned runner release's
// Windows archive, and whether it pins one.
func PinnedWindowsRunnerSHA256() (string, bool) {
	return runnerrelease.PinnedSHA256For(windowsRunnerPlatform)
}

// buildWindowsImage is BuildImage for a Windows base image: everything before the
// builder is launched differs, and everything after it is the one path (ADR-024).
func (p *Provider) buildWindowsImage(ctx context.Context, spec BuildSpec) (string, error) {
	switch {
	case spec.Arch != "x64":
		return "", fmt.Errorf("ec2: GitHub publishes the windows-2025 template for x64 alone, and "+
			"this build asks for %q", spec.Arch)
	case spec.CACertPEM != "":
		return "", fmt.Errorf("ec2: --ca-cert is the cache endpoint's issuer, and a Windows image " +
			"has no client for billet's caches")
	case len(spec.Accept) > 0:
		return "", fmt.Errorf("ec2: images.accept completes a Linux image with what its template " +
			"leaves out, and a Windows image is built with none of them (ADR-024)")
	case !runnerVersionPattern.MatchString(spec.RunnerVersion):
		return "", fmt.Errorf("ec2: %q is not an actions/runner release version", spec.RunnerVersion)
	case !runnerDigestPattern.MatchString(spec.RunnerSHA256):
		return "", fmt.Errorf("ec2: the runner's sha256 %q is not 64 lowercase hex digits; the build "+
			"checks the actions/runner archive against it before unpacking, and refuses to "+
			"install a runner it cannot check", spec.RunnerSHA256)
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("ec2: invent a build nonce: %w", err)
	}

	spec.nonce = hex.EncodeToString(raw)

	body, digest, err := windowsPayloadArchive(spec)
	if err != nil {
		return "", err
	}

	key, cleanup, err := p.stageArchive(ctx, &spec, body, digest)
	if err != nil {
		return "", err
	}

	defer func() {
		if err := cleanup(); err != nil {
			p.log.ErrorContext(ctx, "the staged build payload could not be removed and "+
				"is still costing money; delete it by hand",
				"bucket", spec.PayloadBucket, "key", key, "error", err)
		}
	}()

	script, err := windowsBootstrapScript(spec)
	if err != nil {
		return "", err
	}

	return p.buildFromScript(ctx, spec, script)
}

// windowsSettingsFile is build.json, which the build's scripts read.
func windowsSettingsFile(spec BuildSpec) ([]byte, error) {
	if !verifyNonce.MatchString(spec.nonce) {
		return nil, fmt.Errorf("ec2: %q is not a build nonce", spec.nonce)
	}

	return json.Marshal(struct {
		Nonce         string `json:"nonce"`
		RunnerVersion string `json:"runner_version"`
		RunnerSHA256  string `json:"runner_sha256"`
		ImageVersion  string `json:"image_version"`
	}{spec.nonce, spec.RunnerVersion, spec.RunnerSHA256, "billet"})
}

// windowsPayloadArchive is the tarball the builder fetches: GitHub's steps and the
// scripts that run them, at the paths they have in the tree, under C:\billet-ami.
//
// tar.exe IS WHAT UNPACKS IT, at System32 on Windows Server 2025 (2026-11-04), so the
// archive is the one packArchive writes for a Linux builder and no second format.
func windowsPayloadArchive(spec BuildSpec) ([]byte, string, error) {
	settings, err := windowsSettingsFile(spec)
	if err != nil {
		return nil, "", err
	}

	tree, err := scripts.AMIBuildWindows()
	if err != nil {
		return nil, "", fmt.Errorf("ec2: read the build's scripts: %w", err)
	}

	const repo = "billet-ami/repo/"

	files := make([]archiveMember, 0, len(tree)+3)
	for _, f := range tree {
		files = append(files, archiveMember{repo + "scripts/" + f.Path, 0o644, f.Body})
	}

	toolset, err := runnerimages.ToolsetBytesFor(runnerimages.Windows2025)
	if err != nil {
		return nil, "", fmt.Errorf("ec2: %w", err)
	}

	file, err := runnerimages.ToolsetFileFor(runnerimages.Windows2025)
	if err != nil {
		return nil, "", fmt.Errorf("ec2: %w", err)
	}

	files = append(files,
		archiveMember{repo + "internal/runnerimages/" + file, 0o644, toolset},
		archiveMember{repo + "internal/runnerimages/pinned.txt", 0o644, runnerimages.PinnedFile()},
		archiveMember{"billet-ami/build.json", 0o644, settings},
	)

	return packArchive("billet-ami", 0o755, files)
}

// windowsBootstrapScript is the builder's user data: switch EMS off, reboot, and on
// the next boot fetch the payload, check it, unpack it and start the build.
//
// THE REBOOT IS WHAT THE CONSOLE COSTS. The stock AMI boots with EMS on, so SAC owns the
// port and `\\.\COM1` does not exist; EC2Launch reaches the console through a private
// ioctl on its own driver, which nothing else can rely on. With EMS off and a reboot,
// COM1 appears, and lines written to it reach GetConsoleOutput while the port stays open
// (c7i.large, Windows Server 2025, 2026-11-04). The setting stays in the image, which is
// how its verifier reports, and costs a job instance's SAC, which nothing reaches through
// an egress-only group anyway.
//
// NO PAGE FILE IS EVER WRITTEN ON THE MACHINE BEING IMAGED. A snapshot's blocks reach a new
// volume as they are read, and Session Manager reads a page file left in the image: a job's
// boot sat 5 min 6 s there, 369 s of uptime to user data against 91 s with none and, once a
// warm reboot had read them, under a second. GitHub takes back an assignment left unstarted
// at five minutes, so with that file no job ever started (c7i.xlarge, 2026-11-04). So paging
// goes off in this reboot, the second boot removes the file, and build-ami.ps1 puts paging
// back as the last thing before its verdict, leaving Windows to create a new file on each
// job's first boot.
//
// A TASK, AND IT UNREGISTERS ITSELF BEFORE IT DOES ANYTHING: EC2Launch runs user data on
// the first boot only, and an unplanned restart after this runs nothing, so billet's
// deadline ends it rather than a build resumed from nowhere (ADR-022's rule).
//
// NOT GZIPPED. cloud-init documents compressed user data and EC2Launch does not, so this
// has to fit EC2's 16384 bytes as it is; launchBuilder refuses it otherwise.
func windowsBootstrapScript(spec BuildSpec) (string, error) {
	if spec.payloadURL == "" || !runnerDigestPattern.MatchString(spec.payloadSHA256) {
		return "", fmt.Errorf("ec2: the build's payload must be staged before its bootstrap " +
			"is rendered")
	}

	if !verifyNonce.MatchString(spec.nonce) {
		return "", fmt.Errorf("ec2: %q is not a build nonce", spec.nonce)
	}

	url, err := psQuote(spec.payloadURL)
	if err != nil {
		return "", fmt.Errorf("ec2: the payload URL is not one this build will quote: %w", err)
	}

	second := strings.NewReplacer(
		"@NONCE@", spec.nonce,
		"@URL@", url,
		"@DIGEST@", spec.payloadSHA256,
		"@MIN_FREE_GIB@", strconv.Itoa(minWindowsBuilderFreeGiB),
		"@PAYLOAD@", windowsPayload,
		"@SETTINGS@", windowsSettings,
		"@DRIVER@", windowsDriver,
		"@BOOTSTRAP@", windowsBootstrap,
		"@TASK@", windowsBuildTask,
		"@MARK@", windowsImageMark,
		"@PAGEFILE@", windowsPageFile,
		"@BEGIN@", buildBegin,
		"@END@", buildEnd,
		"@VERDICT@", reportVerdictKey,
		"@FAIL@", verdictFail,
		"@STEP@", reportStepKey,
		"@TEMPLATE_STEP@", buildTemplateStepKey,
		"@COMPONENTS@", buildComponentsKey,
	).Replace(windowsBootstrapSecond)

	first := strings.NewReplacer(
		"@DIR@", windowsBuildDir,
		"@BOOTSTRAP@", windowsBootstrap,
		"@TASK@", windowsBuildTask,
		"@SECOND@", second,
	).Replace(windowsBootstrapFirst)

	return first, nil
}

// windowsBootstrapFirst is the first boot, which can report nothing: there is no COM1
// until the reboot it asks for. So a failure here stops the machine, which billet reads as
// a builder that powered off with no verdict, rather than billing a licence to its deadline.
//
// THE SECOND SCRIPT HOLDS A PRESIGNED URL, a credential, so the directory is opened to
// SYSTEM and the administrators alone before it is written, and the URL is good for an
// hour. It is deleted before the build starts.
const windowsBootstrapFirst = `<powershell>
$ErrorActionPreference = 'Stop'
try {
New-Item -ItemType Directory -Force -Path '@DIR@' | Out-Null
$acl = New-Object Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true, $false)
foreach ($who in 'NT AUTHORITY\SYSTEM', 'BUILTIN\Administrators') {
	$acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($who, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
}
Set-Acl -LiteralPath '@DIR@' -AclObject $acl
Set-Content -LiteralPath '@BOOTSTRAP@' -Encoding ascii -Value @'
@SECOND@
'@
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File @BOOTSTRAP@'
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -Priority 4
Register-ScheduledTask -TaskName '@TASK@' -Action $action -Trigger $trigger -Settings $settings -User 'NT AUTHORITY\SYSTEM' -RunLevel Highest -Force | Out-Null
Set-CimInstance -InputObject (Get-CimInstance Win32_ComputerSystem) -Property @{ AutomaticManagedPagefile = $false }
Get-CimInstance Win32_PageFileSetting | Remove-CimInstance
$memory = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Memory Management'
Set-ItemProperty -LiteralPath $memory -Name PagingFiles -Value @('') -Type MultiString
Remove-ItemProperty -LiteralPath $memory -Name ExistingPageFiles -ErrorAction SilentlyContinue
bcdedit /ems '{current}' off | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'bcdedit could not switch EMS off' }
bcdedit /bootems '{current}' off | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'bcdedit could not switch boot EMS off' }
Restart-Computer -Force
} catch {
	Stop-Computer -Force
}
</powershell>
`

// windowsBootstrapSecond is the second boot. A failure says so with this build's nonce,
// in the verdict build-ami.ps1 would have printed, so a payload that did not arrive ends
// the wait at once rather than at billet's deadline. It never shuts the machine down.
//
// THE DRIVER RETURNING SAYS WHAT HAPPENED ONLY WHILE IT STILL EXISTS. It removes itself
// before it announces success and reports its own failures, and a shutdown may end it
// before this script: reporting then would read a finished build as a failed one,
// and billet takes a failure said after a success as the last word.
const windowsBootstrapSecond = `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
function Stop-WithFailure([string]$step) {
	$port = New-Object System.IO.Ports.SerialPort 'COM1', 115200, 'None', 8, 'One'
	$port.Open()
	while ($true) {
		foreach ($line in '@BEGIN@ @NONCE@', '@VERDICT@=@FAIL@', "@STEP@=$step", '@TEMPLATE_STEP@=', '@COMPONENTS@=', '@END@ @NONCE@') {
			$port.WriteLine($line)
		}
		Start-Sleep -Seconds 15
	}
}
try {
	Unregister-ScheduledTask -TaskName '@TASK@' -Confirm:$false
	if (Test-Path -LiteralPath '@MARK@') { throw 'this machine already records a finished billet build' }
	if (Test-Path -LiteralPath '@PAGEFILE@') { Remove-Item -LiteralPath '@PAGEFILE@' -Force }
	if ((Get-PSDrive C).Free -lt (@MIN_FREE_GIB@ * 1GB)) { throw 'the root has too little free space' }
	[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
	for ($i = 0; ; $i++) {
		try {
			Invoke-WebRequest -UseBasicParsing -Uri @URL@ -OutFile '@PAYLOAD@'
			break
		} catch {
			if ($i -ge 30) { throw }
			Start-Sleep -Seconds 10
		}
	}
	if ((Get-FileHash -Algorithm SHA256 -LiteralPath '@PAYLOAD@').Hash.ToLowerInvariant() -cne '@DIGEST@') { throw 'the payload is not the one staged' }
	& tar.exe -xzf '@PAYLOAD@' -C 'C:\'
	if ($LASTEXITCODE -ne 0) { throw 'the payload did not unpack' }
	Remove-Item -LiteralPath '@PAYLOAD@' -Force
	if ((Get-Content -Raw -LiteralPath '@SETTINGS@' | ConvertFrom-Json).nonce -cne '@NONCE@') { throw 'the settings name another build' }
	Remove-Item -LiteralPath '@BOOTSTRAP@' -Force
} catch {
	Stop-WithFailure 'bootstrap'
}
& powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File '@DRIVER@'
if (Test-Path -LiteralPath '@DRIVER@') { Stop-WithFailure 'start' }
Start-Sleep -Seconds 3600
`

// windowsBuildFailure is the error a Windows build's failure verdict becomes.
func windowsBuildFailure(id string, report map[string]string) error {
	where := report[reportStepKey]

	if n := report[buildTemplateStepKey]; n != "" {
		if name, ok := windowsPlanStepName(n); ok {
			where += ", at step " + n + " of GitHub's template (" + name + ")"
		}
	}

	if where == bootstrapStep {
		return fmt.Errorf("ec2: the builder %s reported that its build failed in %s, before the "+
			"build started: its root had too little free space, the payload could not be "+
			"fetched or was not the one staged, its settings named another build, the machine "+
			"already records a finished billet build, or the task that started it could not be "+
			"removed", id, where)
	}

	return fmt.Errorf("ec2: the builder %s reported that its build failed in %s; the end of the "+
		"step's output is logged above, and billet terminates the builder", id, where)
}

// logWindowsBuildTail logs what the driver put on the console ahead of a failure's verdict,
// which is the only account of it a builder in an egress-only group can give. LOGGED AND
// NOT PUT IN THE ERROR, since a guest wrote it. Printable and bounded already by the driver,
// and held to that here, because this is what the console says and not what it should.
func (p *Provider) logWindowsBuildTail(id, console string) {
	const prefix = "billet-build-log "

	// mostLines holds both of the driver's logs at its 40 lines each, and longest what it
	// cuts a line to.
	const (
		mostLines = 80
		longest   = 240
	)

	var lines []string

	for _, line := range strings.Split(console, "\n") {
		text, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), prefix)
		if ok && printableASCII(text) && len(text) <= longest {
			lines = append(lines, text)
		}
	}

	if len(lines) > mostLines {
		lines = lines[len(lines)-mostLines:]
	}

	for _, text := range lines {
		p.log.Warn("the builder's failed step said", "instance", id, "line", text)
	}
}

// windowsPlanStepName is the id of step n (counted from 1) of the Windows plan.
func windowsPlanStepName(n string) (string, bool) {
	i, err := strconv.Atoi(n)
	if err != nil || i < 1 {
		return "", false
	}

	files, err := scripts.AMIBuildWindows()
	if err != nil {
		return "", false
	}

	for _, f := range files {
		if f.Path != scripts.WindowsPlan {
			continue
		}

		var steps []struct {
			ID string `json:"id"`
		}

		if err := json.NewDecoder(bytes.NewReader(f.Body)).Decode(&steps); err != nil || i > len(steps) {
			return "", false
		}

		return steps[i-1].ID, true
	}

	return "", false
}
