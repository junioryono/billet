# ec2-layer.ps1: billet's layer over GitHub's steps on a Windows AMI (ADR-024): the
# runner, checked against its pinned digest before it is unpacked, the `runner` account
# and the entry points a tier names. Run by build-ami.ps1 as one step, as SYSTEM.
#
# Environment, for tests only: BILLET_AMI_ROOT, BILLET_AMI_IMAGE_ROOT (prefixes every
# path it writes on the machine) and BILLET_AMI_RUNNER_ZIP (a zip in place of the
# download).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$here = Split-Path -Parent $MyInvocation.MyCommand.Path
$root = $env:BILLET_AMI_ROOT
if ([string]::IsNullOrEmpty($root)) {
	$root = 'C:\billet-ami'
}

$imageRoot = $env:BILLET_AMI_IMAGE_ROOT
if ([string]::IsNullOrEmpty($imageRoot)) {
	$imageRoot = 'C:\'
}

$settings = Get-Content -Raw -LiteralPath (Join-Path $root 'build.json') | ConvertFrom-Json
$version = [string]$settings.runner_version
$digest = [string]$settings.runner_sha256

if ($version -cnotmatch '^\d{1,6}\.\d{1,6}\.\d{1,6}([-+][0-9A-Za-z.-]{1,32})?$') {
	throw "$version is not an actions/runner release version"
}

if ($digest -cnotmatch '^[0-9a-f]{64}$') {
	throw 'build.json names no digest for the runner'
}

$runner = Join-Path $imageRoot 'actions-runner'
$billet = Join-Path $imageRoot 'billet'
New-Item -ItemType Directory -Force -Path $runner, $billet | Out-Null

$zip = $env:BILLET_AMI_RUNNER_ZIP
if ([string]::IsNullOrEmpty($zip)) {
	[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
	$zip = Join-Path $root 'runner.zip'
	$asset = "actions-runner-win-x64-$version.zip"

	# RETRIED: a blip here would throw away every step that has already run.
	for ($i = 0; ; $i++) {
		try {
			Invoke-WebRequest -UseBasicParsing -OutFile $zip `
				-Uri "https://github.com/actions/runner/releases/download/v$version/$asset"

			break
		} catch {
			if ($i -ge 10) {
				throw
			}

			Start-Sleep -Seconds 15
		}
	}
}

# BEFORE IT IS UNPACKED: this runner executes every job the image serves.
$got = (Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash.ToLowerInvariant()
if ($got -cne $digest) {
	throw "the actions/runner archive hashes to $got, not $digest; refusing to unpack it"
}

Expand-Archive -LiteralPath $zip -DestinationPath $runner -Force

foreach ($name in 'billet-runner.ps1', 'billet-runner-service.ps1', 'billet-job-started.ps1') {
	Copy-Item -LiteralPath (Join-Path $here $name) -Destination $billet -Force
}

# AN ACCOUNT WITH A PASSWORD NOBODY KEEPS. billet-runner.ps1 sets one for its own boot
# before it logs the account on.
if ($null -eq $env:BILLET_AMI_IMAGE_ROOT) {
	$bytes = New-Object byte[] 24
	$random = [Security.Cryptography.RandomNumberGenerator]::Create()
	$random.GetBytes($bytes)
	$random.Dispose()
	$password = ConvertTo-SecureString ('aA1!' + [Convert]::ToBase64String($bytes)) -AsPlainText -Force
	New-LocalUser -Name 'runner' -Password $password -PasswordNeverExpires -AccountNeverExpires | Out-Null
	Add-LocalGroupMember -Group 'Administrators' -Member 'runner'

	# NOTHING ELSE THE BUILD LEFT RUNS AS A PROFILE: the template's step adds one to
	# this account's, for its own later steps, and it installs a module when loaded.
	$profileFile = $PROFILE.CurrentUserCurrentHost
	if (Test-Path -LiteralPath $profileFile) {
		Remove-Item -LiteralPath $profileFile -Force
	}
}

# THE RUNNER HAS TO RUN BEFORE THIS COUNTS AS SUCCESS: a damaged archive, or a runtime
# it cannot find, otherwise surfaces on somebody's first job.
if ($null -eq $env:BILLET_AMI_IMAGE_ROOT) {
	$said = & (Join-Path $runner 'bin\Runner.Listener.exe') --version
	if ($LASTEXITCODE -ne 0) {
		throw "Runner.Listener.exe --version exited $LASTEXITCODE"
	}

	Write-Output "runner $said"
}

# WHAT MAKES THIS A FINISHED IMAGE, last: the bootstrap refuses a machine that has it, so
# a build runs once on a machine and a billet image is no base for another.
Set-Content -LiteralPath (Join-Path $billet 'billet-image') -Value $version -Encoding ascii
