# build-ami.ps1: build a Windows runner AMI on the EC2 builder by running the steps of
# GitHub's windows-2025 template that billet carries, then billet's EC2 layer (ADR-026).
#
# Started by the bootstrap in user data on the builder's second boot, the first with
# COM1, and it ends in one of two ways, each a block on the console, where billet reads
# it:
#
#   billet-build-begin <nonce>
#   verdict=ok|fail
#   step=<the part of the build that was running>
#   template_step=<the template step that was running, on a failure in it>
#   components=
#   billet-build-end <nonce>
#
# repeated for a dwell and then a guest-initiated shutdown on success, and for as long
# as the machine runs on failure, so a stop is never mistaken for success. Each step's
# own output goes to a log, so it cannot push the verdict out of EC2's 64 KiB console
# tail: the console gets a line per step and, on a failure, a bounded tail of that step's
# log ahead of the verdict, because a builder in an egress-only group has no other way to
# say why. billet reads none of it, only the framed block. The nonce guards against
# accidental output; it is not a secret from a build step, which as SYSTEM can read the
# user data.
#
# PowerShell 5.1, the one Windows ships: this runs before PowerShell 7 is installed.
#
# Environment, for tests only: BILLET_AMI_ROOT, BILLET_AMI_CONSOLE (a file in place of
# COM1), BILLET_AMI_SHELL (what runs a step), BILLET_AMI_DWELL_ROUNDS,
# BILLET_AMI_DWELL_SECONDS, BILLET_AMI_FAIL_ROUNDS (a failure's dwell, unending unless
# set), BILLET_AMI_LAYER (a script in place of the EC2 layer), and BILLET_AMI_SHUTDOWN
# (a script in place of the tail that ends the machine).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

function Get-Setting([string]$name, [string]$default) {
	$value = [Environment]::GetEnvironmentVariable($name)
	if ([string]::IsNullOrEmpty($value)) {
		return $default
	}

	return $value
}

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$root = Get-Setting 'BILLET_AMI_ROOT' 'C:\billet-ami'
$repo = Join-Path $root 'repo'
$runnerImages = Join-Path (Join-Path $repo 'scripts') 'runner-images'
$upstream = Join-Path $runnerImages 'upstream'
$logs = Join-Path $root 'logs'
$shell = Get-Setting 'BILLET_AMI_SHELL' 'powershell.exe'
$dwellRounds = [int](Get-Setting 'BILLET_AMI_DWELL_ROUNDS' '8')
$dwellSeconds = [int](Get-Setting 'BILLET_AMI_DWELL_SECONDS' '15')
$failRounds = [int](Get-Setting 'BILLET_AMI_FAIL_ROUNDS' '0')
$consoleFile = Get-Setting 'BILLET_AMI_CONSOLE' ''

$settings = Get-Content -Raw -LiteralPath (Join-Path $root 'build.json') | ConvertFrom-Json
$nonce = [string]$settings.nonce
if ($nonce -cnotmatch '^[0-9a-f]{32}$') {
	throw 'build.json names no build nonce'
}

# ONE PORT, HELD OPEN FOR THE BUILD. SerialPort.Close discards what the driver has not
# sent: a line written just before a Close never reached GetConsoleOutput, and thirty
# written to a port held open all did (c7i.large, Windows Server 2025, 2026-11-04).
$script:port = $null

function Open-Console {
	if ($consoleFile -ne '') {
		return
	}

	$script:port = New-Object System.IO.Ports.SerialPort 'COM1', 115200, 'None', 8, 'One'
	$script:port.Open()
}

function Write-Console([string[]]$lines) {
	if ($consoleFile -ne '') {
		Add-Content -LiteralPath $consoleFile -Value $lines -Encoding ascii

		return
	}

	foreach ($line in $lines) {
		$script:port.WriteLine($line)
	}
}

function Get-Block([string]$verdict, [string]$step, [string]$templateStep) {
	return @(
		"billet-build-begin $nonce",
		"verdict=$verdict",
		"step=$step",
		"template_step=$templateStep",
		'components=',
		"billet-build-end $nonce"
	)
}

# Write-LogTail puts the end of a failed step's output on the console, printable and cut
# short: at most 40 lines of 240 from each of its two logs, well inside the 64 KiB tail.
function Write-LogTail([string]$label) {
	foreach ($suffix in 'out', 'err') {
		$path = Join-Path $logs "$label.$suffix"
		if (-not (Test-Path -LiteralPath $path)) {
			continue
		}

		foreach ($line in @(Get-Content -LiteralPath $path -Tail 40)) {
			$clean = ([string]$line) -replace '[^\x20-\x7e]', ''
			if ($clean.Length -gt 240) {
				$clean = $clean.Substring(0, 240)
			}

			Write-Console @("billet-build-log $clean")
		}
	}
}

# A FAILURE HOLDS THE MACHINE, printing its verdict for as long as it runs: billet reads
# the verdict and terminates it, and a stop could only be read as a success.
function Stop-WithFailure([string]$step, [string]$templateStep) {
	if ($script:label -ne '') {
		try {
			Write-LogTail $script:label
		} catch {
			Write-Console @('billet-build-log the step left no log to read')
		}
	}

	$round = 0
	while ($true) {
		Write-Console (Get-Block 'fail' $step $templateStep)
		$round++
		if ($failRounds -gt 0 -and $round -ge $failRounds) {
			exit 1
		}

		Start-Sleep -Seconds $dwellSeconds
	}
}

# Set-StepEnvironment gives a step what a new logon would: the machine's and the
# account's variables as they are now, since steps change them for the steps after, and
# what the template names for this step. Packer gets the first by reconnecting.
function Set-StepEnvironment($step) {
	if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
		foreach ($scope in 'Machine', 'User') {
			$vars = [Environment]::GetEnvironmentVariables($scope)
			foreach ($name in $vars.Keys) {
				if ($name -ine 'Path') {
					[Environment]::SetEnvironmentVariable($name, [string]$vars[$name], 'Process')
				}
			}
		}

		$machine = [Environment]::GetEnvironmentVariable('Path', 'Machine')
		$account = [Environment]::GetEnvironmentVariable('Path', 'User')
		[Environment]::SetEnvironmentVariable('Path', "$machine;$account", 'Process')
	}

	if ($null -ne $step.env) {
		foreach ($property in $step.env.PSObject.Properties) {
			$value = ([string]$property.Value).Replace('@image_version@', [string]$settings.image_version)
			[Environment]::SetEnvironmentVariable($property.Name, $value, 'Process')
		}
	}
}

# Invoke-Step runs one script as Packer's powershell provisioner does: in a new
# PowerShell that loads the account's profile, its exit status the step's verdict.
function Invoke-Step([string]$label, [string[]]$body, [int[]]$valid) {
	$script:label = $label
	$wrapper = Join-Path $logs "$label.ps1"
	$lines = @(
		"`$ProgressPreference = 'SilentlyContinue'"
	) + $body + @(
		'exit $LASTEXITCODE'
	)
	Set-Content -LiteralPath $wrapper -Value $lines -Encoding UTF8

	$arguments = "-NoLogo -NonInteractive -ExecutionPolicy Bypass -File `"$wrapper`""
	$process = Start-Process -FilePath $shell -ArgumentList $arguments -NoNewWindow -PassThru `
		-RedirectStandardOutput (Join-Path $logs "$label.out") `
		-RedirectStandardError (Join-Path $logs "$label.err")

	# THE HANDLE IS TAKEN BEFORE THE WAIT, or ExitCode reads back empty once the process
	# has gone, which no comparison below would catch as a failure.
	$null = $process.Handle
	$process.WaitForExit()
	if ($valid -notcontains $process.ExitCode) {
		throw "the step $label exited $($process.ExitCode)"
	}
}

# Copy-Source copies one of a file step's sources to where the template sends it. The
# declaration is read from the one copy billet vendors, held to its pin here: an edit to
# it is an edit to every image built afterwards.
function Copy-Source([string]$source, [string]$destination) {
	if ($source -eq 'images/windows/toolsets') {
		$file = Join-Path (Join-Path (Join-Path $repo 'internal') 'runnerimages') 'toolset-2025.json'
		$pin = Join-Path (Split-Path -Parent $file) 'pinned.txt'
		$want = ''
		foreach ($line in Get-Content -LiteralPath $pin) {
			if ($line -like 'windows-2025 *') {
				$want = $line.Substring('windows-2025 '.Length).Trim()
			}
		}

		$got = (Get-FileHash -Algorithm SHA256 -LiteralPath $file).Hash.ToLowerInvariant()
		if ($want -eq '' -or $got -cne $want) {
			throw "the vendored toolset hashes to $got and pinned.txt names '$want'"
		}

		$into = Join-Path $destination 'toolsets'
		New-Item -ItemType Directory -Force -Path $into | Out-Null
		Copy-Item -LiteralPath $file -Destination $into

		return
	}

	Copy-Item -LiteralPath (Join-Path $upstream ($source -replace '/', [IO.Path]::DirectorySeparatorChar)) `
		-Destination $destination -Recurse -Force
}

function Read-Differences {
	$found = @{ skips = @{}; prepares = @{} }
	foreach ($line in Get-Content -LiteralPath (Join-Path $runnerImages 'differences-windows.tsv')) {
		if ($line -eq '' -or $line.StartsWith('#')) {
			continue
		}

		$fields = $line.Split("`t")
		if ($fields.Count -ne 3 -or $fields[2] -eq '') {
			throw "a difference that is not an action, a step and a reason: $line"
		}

		if ($found.skips.ContainsKey($fields[1]) -or $found.prepares.ContainsKey($fields[1])) {
			throw "the step $($fields[1]) is listed twice"
		}

		if ($fields[0] -ceq 'skip') {
			$found.skips[$fields[1]] = $fields[2]
		} elseif ($fields[0] -cmatch '^prepare:[a-z0-9-]+$') {
			$script = Join-Path $here ('prepare-' + $fields[0].Substring('prepare:'.Length) + '.ps1')
			if (-not (Test-Path -LiteralPath $script)) {
				throw "the difference $($fields[0]) names a script this build does not carry"
			}

			$found.prepares[$fields[1]] = $script
		} else {
			throw "a difference this build does not know: $line"
		}
	}

	return $found
}

function Invoke-Template {
	$plan = @(Get-Content -Raw -LiteralPath (Join-Path $runnerImages 'plan-windows-2025.json') | ConvertFrom-Json)
	if ($plan.Count -eq 1 -and $plan[0] -is [array]) {
		$plan = $plan[0]
	}

	if ($plan.Count -eq 0) {
		throw 'the plan has no steps'
	}

	$differences = Read-Differences
	$skips = $differences.skips
	$prepares = $differences.prepares

	# AN ENTRY THAT NAMES NO STEP STOPS THE BUILD BEFORE ANYTHING RUNS: it is a pin bump
	# to review, not a line to ignore.
	$ids = @{}
	foreach ($step in $plan) {
		$ids[[string]$step.id] = $true
	}

	foreach ($id in @($skips.Keys) + @($prepares.Keys)) {
		if (-not $ids.ContainsKey($id)) {
			throw "differences-windows.tsv names $id, which the plan does not have"
		}
	}

	$number = 0
	foreach ($step in $plan) {
		$number++
		$script:templateStep = "$number"
		$script:label = ''
		$id = [string]$step.id
		$label = ('{0:d3}' -f $number)

		if ($skips.ContainsKey($id)) {
			Write-Output "=== [$number/$($plan.Count)] $id skipped: $($skips[$id]) ==="

			continue
		}

		Write-Output "=== [$number/$($plan.Count)] $id at $((Get-Date).ToUniversalTime().ToString('o')) ==="
		Write-Console @("billet-build-progress $number/$($plan.Count) $id $((Get-Date).ToUniversalTime().ToString('o'))")
		Set-StepEnvironment $step

		if ($prepares.ContainsKey($id)) {
			Invoke-Step "$label-prepare" @("& '$($prepares[$id].Replace("'", "''"))'") @(0)
			Set-StepEnvironment $step
		}

		$valid = @(0)
		if ($null -ne $step.valid_exit_codes) {
			$valid = @($step.valid_exit_codes | ForEach-Object { [int]$_ })
		}

		switch ([string]$step.kind) {
			'file' {
				New-Item -ItemType Directory -Force -Path ([string]$step.destination) | Out-Null
				foreach ($source in $step.sources) {
					Copy-Source ([string]$source) ([string]$step.destination)
				}
			}
			'powershell' {
				if ($step.script) {
					$path = Join-Path $upstream (([string]$step.script) -replace '/', [IO.Path]::DirectorySeparatorChar)
					Invoke-Step $label @("& '$($path.Replace("'", "''"))'") $valid
				} else {
					# STOPPING ON A CMDLET'S FAILURE, WHICH GITHUB'S SCRIPTS DO NOT GET: they are
					# written for Packer's default, and Configure-BaseImage.ps1 carries on past
					# Set-ExecutionPolicy's complaint about the step's own -ExecutionPolicy
					# (measured 2026-11-04). An inline `Move-Item` has nothing else to notice it,
					# and `exit` with nothing having set a status reads as success.
					Invoke-Step $label (@("`$ErrorActionPreference = 'Stop'") + @($step.inline | ForEach-Object { [string]$_ })) $valid
				}
			}
			default {
				# THE PLAN CARRIES WHAT THE RUNNER CANNOT DO ONLY AS A SKIP: a step of a
				# kind the template gains in a pin bump stops the build here.
				throw "the step $id is a $($step.kind), which this build does not run"
			}
		}
	}

	$script:templateStep = ''
}

New-Item -ItemType Directory -Force -Path $logs | Out-Null
Start-Transcript -LiteralPath (Join-Path $logs 'build.log') -Append | Out-Null

Open-Console
$script:step = 'start'
$script:templateStep = ''
$script:label = ''

try {
	$script:step = 'template'
	Invoke-Template

	$script:step = 'layer'
	$layer = Get-Setting 'BILLET_AMI_LAYER' (Join-Path $here 'ec2-layer.ps1')
	Invoke-Step 'layer' @("& '$($layer.Replace("'", "''"))'") @(0)

	# THE PAYLOAD GOES BEFORE THE VERDICT, so a success is about the disk as it will be
	# imaged. This file runs from memory and takes its logs' directory with it.
	$script:step = 'cleanup'
	$script:label = ''
	Stop-Transcript | Out-Null

	# THE TEMPLATE'S SCRATCH IS AZURE'S THROWAWAY D: DRIVE THERE AND THE IMAGED ROOT HERE,
	# so what its steps downloaded would otherwise ship in every image.
	if (-not [string]::IsNullOrEmpty($env:TEMP_DIR) -and (Test-Path -LiteralPath $env:TEMP_DIR)) {
		Remove-Item -LiteralPath $env:TEMP_DIR -Recurse -Force
	}

	# EC2LAUNCH'S COPY OF THE USER DATA HOLDS THE PRESIGNED URL: the first boot's script
	# ended in a reboot, so nothing cleaned it up, and an hour-old URL is still a signature.
	foreach ($dir in "$env:SystemRoot\system32\config\systemprofile\AppData\Local\Temp",
		"$env:SystemDrive\Users\Administrator\AppData\Local\Temp") {
		# ABSENT IS THE ONE ANSWER THAT PASSES: a listing that fails would otherwise read as
		# nothing to remove, and the image would carry the signature.
		if (Test-Path -LiteralPath $dir) {
			Get-ChildItem -LiteralPath $dir -Filter 'EC2Launch*' -Directory -ErrorAction Stop |
				Remove-Item -Recurse -Force
		}
	}

	# PAGING COMES BACK WITH NO FILE, which is the state a job's first boot has to find: the
	# bootstrap says why this machine ran without one. Windows creates the file at that boot.
	if ([Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT) {
		if (Test-Path -LiteralPath 'C:\pagefile.sys') {
			throw 'a page file appeared during the build, and a job would read it from the snapshot'
		}

		Set-ItemProperty -LiteralPath 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Memory Management' `
			-Name PagingFiles -Value @('?:\pagefile.sys') -Type MultiString
		Set-CimInstance -InputObject (Get-CimInstance Win32_ComputerSystem) -Property @{ AutomaticManagedPagefile = $true }
	}

	Remove-Item -LiteralPath (Join-Path $root 'repo') -Recurse -Force
	Remove-Item -LiteralPath (Join-Path $root 'build.json') -Force
	if ($consoleFile -eq '') {
		Remove-Item -LiteralPath $root -Recurse -Force
	}

	$script:step = 'done'
} catch {
	Write-Output "the build failed at $($script:step): $($_ | Out-String)"
	Stop-WithFailure $script:step $script:templateStep
}

for ($round = 0; $round -lt $dwellRounds; $round++) {
	Write-Console (Get-Block 'ok' 'done' '')
	Start-Sleep -Seconds $dwellSeconds
}

# GUEST-INITIATED, WHICH IS WHAT BILLET READS A FINISHED BUILD'S STOP BY.
#
# reset, NOT sysprep: ADR-026 has what each costs a job's first boot. Either way an
# instance launched from the image runs its own user data (2026-11-04).
#
# ITS EXIT STATUS SAYS NOTHING: EC2Launch answered 1 and "Shutdown was initiated but did
# not complete" for a machine that stopped moments later with
# Client.InstanceInitiatedShutdown (EC2Launch 2.5.2, 2026-11-04). So what fails this tail
# is the machine still being up, and a failure is one whenever it is said.
try {
	$shutdown = Get-Setting 'BILLET_AMI_SHUTDOWN' ''
	if ($shutdown -ne '') {
		& $shutdown
	} else {
		$ErrorActionPreference = 'Continue'
		& "$env:ProgramFiles\Amazon\EC2Launch\EC2Launch.exe" reset --clean --shutdown 2>&1 | Out-Null
		$ErrorActionPreference = 'Stop'
		Start-Sleep -Seconds 900
		throw 'the machine was told to shut down and is still running'
	}
} catch {
	Stop-WithFailure 'cleanup' ''
}
