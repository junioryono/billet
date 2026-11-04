package ec2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/runnerimages"
	"github.com/junioryono/billet/scripts"
)

// windowsVerifyWindow bounds a Windows verification, from launch to a complete report:
// a Windows first boot and a scheduled task's logon before anything is asked.
const windowsVerifyWindow = 30 * time.Minute

// windowsVerifySteps are the parts of a Windows verification a report may name.
var windowsVerifySteps = []string{"entry", "account", "runner", "tools", "environment", "toolcache", "disk", "done"}

// windowsVerifyTools are what a job finds on PATH, the arguments that make each say its
// version, and the report key that carries the answer.
var windowsVerifyTools = []struct{ key, exe, args string }{
	{"git", "git", "--version"},
	{"pwsh", "pwsh", "--version"},
	{"node", "node", "--version"},
	{"python", "python", "--version"},
	{"go", "go", "version"},
	{"ruby", "ruby", "--version"},
	{"bash", "bash", "--version"},
}

// windowsToolVersion is what a declared toolcache line may be before it reaches the
// script, in the two spellings windows-2025's declaration uses: `3.13.*` and `3.3`.
var windowsToolVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*(\.\*)?$`)

// windowsVerifyScript is the user data that makes a machine booted from a Windows AMI
// assert the contract on itself, and the report schema that goes with it.
//
// EVERY TOOL IS ASKED THROUGH THE JOB'S OWN PATH: a scheduled task logging on `runner`,
// as billet-runner.ps1 starts the runner. The builder ran as SYSTEM with the template's
// environment, and present on disk is not findable by a job. That also proves the path
// itself: a password the local policy refuses, a right the account lacks or a profile
// that does not load is a job that never starts.
func windowsVerifyScript(arch, nonce string) (string, reportSchema, error) {
	schema := newReportSchema(reportBegin, reportEnd)

	if arch != "x64" {
		return "", schema, fmt.Errorf("ec2: billet builds no Windows image for %q", arch)
	}

	if !verifyNonce.MatchString(nonce) {
		return "", schema, fmt.Errorf("ec2: %q is not a verification nonce", nonce)
	}

	for _, step := range windowsVerifySteps {
		schema.steps[step] = struct{}{}
	}

	for _, key := range []string{
		reportVerdictKey, reportStepKey, "runner", "image_os", "tool_cache", "root_free_gib", "root_used_gib",
	} {
		schema.keys[key] = struct{}{}
	}

	files, err := scripts.AMIBuildWindows()
	if err != nil {
		return "", schema, fmt.Errorf("ec2: read the build's scripts: %w", err)
	}

	entries := map[string]string{
		scripts.WindowsRunnerEntry:   `C:\billet\billet-runner.ps1`,
		scripts.WindowsRunnerService: `C:\billet\billet-runner-service.ps1`,
	}

	var digests strings.Builder

	for _, f := range files {
		if path, ok := entries[f.Path]; ok {
			sum := sha256.Sum256(f.Body)
			fmt.Fprintf(&digests, "\t\t@('%s', '%s'),\n", path, hex.EncodeToString(sum[:]))

			delete(entries, f.Path)
		}
	}

	if len(entries) != 0 {
		return "", schema, fmt.Errorf("ec2: the build carries no entry point to compare an image with")
	}

	var tools strings.Builder

	for _, tool := range windowsVerifyTools {
		schema.keys[tool.key] = struct{}{}
		fmt.Fprintf(&tools, "Ask-Tool '%s' '%s' '%s'\n", tool.key, tool.exe, tool.args)
	}

	declared, err := runnerimages.LoadWindows()
	if err != nil {
		return "", schema, fmt.Errorf("ec2: %w", err)
	}

	if len(declared.Toolcache) == 0 {
		return "", schema, fmt.Errorf("ec2: the pinned windows-2025 declaration names no toolcache")
	}

	var lines strings.Builder

	for _, entry := range declared.Toolcache {
		if !toolcacheName.MatchString(entry.Name) || !toolcacheName.MatchString(entry.Arch) {
			return "", schema, fmt.Errorf("ec2: the declaration names a toolcache entry this "+
				"script will not interpolate: %q", entry.Name)
		}

		key := "tc_" + strings.ToLower(strings.NewReplacer("-", "_", "+", "_").Replace(entry.Name))
		if !reportKey.MatchString(key) {
			return "", schema, fmt.Errorf("ec2: no report key for the toolcache entry %q", entry.Name)
		}

		schema.keys[key] = struct{}{}

		prefixes := make([]string, 0, len(entry.Versions))

		for _, version := range entry.Versions {
			if !windowsToolVersion.MatchString(version) {
				return "", schema, fmt.Errorf("ec2: the declaration names a toolcache version this "+
					"script will not interpolate: %q", version)
			}

			prefixes = append(prefixes, "'"+strings.TrimSuffix(strings.TrimSuffix(version, "*"), ".")+".'")
		}

		fmt.Fprintf(&lines, "Ask-Toolcache '%s' '%s' '%s' @(%s)\n", key, entry.Name, entry.Arch,
			strings.Join(prefixes, ", "))
	}

	check := strings.NewReplacer(
		"@ENTRY@", windowsRunnerListener,
		"@TOOLS@", strings.TrimRight(tools.String(), "\n"),
		"@TOOLCACHE@", strings.TrimRight(lines.String(), "\n"),
		"@MIN_FREE_GIB@", strconv.Itoa(minVerifiedFreeGiB),
	).Replace(windowsVerifyCheck)

	return strings.NewReplacer(
		"@NONCE@", nonce,
		"@BEGIN@", reportBegin,
		"@END@", reportEnd,
		"@VERDICT@", reportVerdictKey,
		"@OK@", verdictOK,
		"@FAIL@", verdictFail,
		"@STEP@", reportStepKey,
		"@ENTRIES@", strings.TrimRight(digests.String(), ",\n"),
		"@ROUNDS@", strconv.Itoa(verifyDwellRounds),
		"@SECONDS@", strconv.Itoa(verifyDwellSeconds),
		"@CHECK@", check,
	).Replace(windowsVerifyMain), schema, nil
}

// windowsRunnerListener is the runner's own binary on a Windows image.
const windowsRunnerListener = `C:\actions-runner\bin\Runner.Listener.exe`

// windowsVerifyCheck runs as `runner`, and writes what it learns as it goes, so a check
// that stops leaves the step it stopped at and everything before it.
const windowsVerifyCheck = `$ErrorActionPreference = 'Stop'
$out = 'C:\billet\verify\result.txt'
function Say([string]$key, [string]$value) {
	$clean = $value -replace '[^\x20-\x7e]', ''
	if ($clean.Length -gt 180) { $clean = $clean.Substring(0, 180) }
	Add-Content -LiteralPath $out -Value "$key=$clean" -Encoding ascii
}
function Step([string]$name) { Add-Content -LiteralPath $out -Value "step=$name" -Encoding ascii }
function Ask-Tool([string]$key, [string]$exe, [string]$arguments) {
	$ErrorActionPreference = 'Continue'
	$said = @(& $exe $arguments 2>&1)
	if ($LASTEXITCODE -ne 0 -or $said.Count -eq 0) { throw "$exe answered $LASTEXITCODE" }
	Say $key ([string]$said[0])
}
function Ask-Toolcache([string]$key, [string]$name, [string]$arch, [string[]]$prefixes) {
	$found = @()
	foreach ($prefix in $prefixes) {
		$dirs = @(Get-ChildItem -LiteralPath (Join-Path $env:RUNNER_TOOL_CACHE $name) -Directory | Where-Object {
				$_.Name.StartsWith($prefix) -and (Test-Path -LiteralPath (Join-Path $_.FullName $arch)) -and (Test-Path -LiteralPath "$($_.FullName)\$arch.complete")
			})
		if ($dirs.Count -eq 0) { throw "no complete $name $prefix" }
		$found += $dirs[-1].Name
	}
	Say $key ($found -join ' ')
}
Step 'account'
$id = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if ($env:USERNAME -ine 'runner') { throw 'this is not the runner account' }
if (-not $id.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) { throw 'the runner account is not elevated' }
Step 'runner'
Ask-Tool 'runner' '@ENTRY@' '--version'
Step 'tools'
@TOOLS@
Step 'environment'
Say 'image_os' $env:ImageOS
Say 'tool_cache' $env:RUNNER_TOOL_CACHE
if ($env:ImageOS -cne 'win25') { throw 'ImageOS is not win25' }
if ($env:RUNNER_TOOL_CACHE -ine 'C:\hostedtoolcache\windows') { throw 'RUNNER_TOOL_CACHE is not the template''s' }
if ($env:AGENT_TOOLSDIRECTORY -ine $env:RUNNER_TOOL_CACHE) { throw 'AGENT_TOOLSDIRECTORY disagrees' }
Step 'toolcache'
@TOOLCACHE@
Step 'disk'
$drive = Get-PSDrive C
Say 'root_free_gib' ([string][math]::Round($drive.Free / 1GB, 1))
Say 'root_used_gib' ([string][math]::Round($drive.Used / 1GB, 1))
if ($drive.Free -lt (@MIN_FREE_GIB@ * 1GB)) { throw 'the root is nearly full' }
Step 'done'
`

// windowsVerifyMain is the user data, which EC2Launch runs as the local Administrator.
//
// ITS OWN STOP ENDS THE MACHINE, which launches with a shutdown behaviour of terminate:
// the same leak bound the Linux verifier has.
const windowsVerifyMain = `<powershell>
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$port = New-Object System.IO.Ports.SerialPort 'COM1', 115200, 'None', 8, 'One'
$port.Open()
$verdict = '@FAIL@'
$step = 'entry'
$report = New-Object System.Collections.Generic.List[string]
try {
	foreach ($pair in @(
@ENTRIES@
	)) {
		if ((Get-FileHash -Algorithm SHA256 -LiteralPath $pair[0]).Hash.ToLowerInvariant() -cne $pair[1]) {
			throw "$($pair[0]) is not the entry point this billet carries"
		}
	}
	$step = 'account'
	$dir = 'C:\billet\verify'
	New-Item -ItemType Directory -Force -Path $dir | Out-Null
	Set-Content -LiteralPath "$dir\check.ps1" -Encoding ascii -Value @'
@CHECK@
'@
	$acl = Get-Acl -LiteralPath $dir
	$acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule('runner', 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
	Set-Acl -LiteralPath $dir -AclObject $acl
	$bytes = New-Object byte[] 24
	$random = [Security.Cryptography.RandomNumberGenerator]::Create()
	$random.GetBytes($bytes)
	$random.Dispose()
	$password = 'aA1!' + [Convert]::ToBase64String($bytes)
	Set-LocalUser -Name 'runner' -Password (ConvertTo-SecureString $password -AsPlainText -Force)
	$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File $dir\check.ps1" -WorkingDirectory 'C:\actions-runner'
	Register-ScheduledTask -TaskName 'billet-verify' -Action $action -User 'runner' -Password $password -RunLevel Highest -Force | Out-Null
	Start-ScheduledTask -TaskName 'billet-verify'
	for ($i = 0; $i -lt 300; $i++) {
		Start-Sleep -Seconds 2
		if (Select-String -LiteralPath "$dir\result.txt" -Pattern '^step=done$' -Quiet -ErrorAction SilentlyContinue) { break }
		$task = Get-ScheduledTask -TaskName 'billet-verify'
		if ($i -gt 5 -and $task.State -eq 'Ready') { break }
	}
	foreach ($line in Get-Content -LiteralPath "$dir\result.txt") {
		if ($line -like 'step=*') {
			$step = $line.Substring(5)
		} else {
			$report.Add($line)
		}
	}
	if ($step -ceq 'done') { $verdict = '@OK@' }
} catch {
	$report.Add('')
}
for ($round = 0; $round -lt @ROUNDS@; $round++) {
	$port.WriteLine('@BEGIN@ @NONCE@')
	$port.WriteLine("@VERDICT@=$verdict")
	$port.WriteLine("@STEP@=$step")
	foreach ($line in $report) { $port.WriteLine($line) }
	$port.WriteLine('@END@ @NONCE@')
	Start-Sleep -Seconds @SECONDS@
}
Stop-Computer -Force
</powershell>
`
