# billet-runner-service.ps1: runs as `runner`, in the scheduled task billet-runner.ps1
# starts. It takes the registration out of the file it arrived in and runs GitHub's
# listener with the update loop internal/guestassets/runner-service.sh carries, status
# for status, so a tier's result means one thing on both operating systems.
#
# Environment, for tests only: BILLET_AMI_IMAGE_ROOT (where the image's paths begin) and
# BILLET_AMI_SHUTDOWN (a script run in place of ending the machine).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$imageRoot = $env:BILLET_AMI_IMAGE_ROOT
if ([string]::IsNullOrEmpty($imageRoot)) {
	$imageRoot = 'C:\'
}

$billet = Join-Path $imageRoot 'billet'
$file = [IO.Path]::Combine($billet, 'run', 'launch.env')
$root = Join-Path $imageRoot 'actions-runner'
$listener = [IO.Path]::Combine($root, 'bin', 'Runner.Listener.exe')

# WRITTEN BY THE JOB-STARTED HOOK. Only ever read to end a machine whose listener has
# already gone for good, so a job that removes it costs nothing but its own machine.
$env:BILLET_JOB_STARTED_MARK = [IO.Path]::Combine($billet, 'run', 'job-started')

function Stop-ThisMachine {
	if ([string]::IsNullOrEmpty($env:BILLET_AMI_SHUTDOWN)) {
		Stop-Computer -Force
	} else {
		& $env:BILLET_AMI_SHUTDOWN
	}
}

# A LISTENER THAT ENDS HAVING RUN NO JOB ENDS THE MACHINE, as a runner that cannot start
# does: nothing more will run here, and it would go on billing by the hour for a Windows
# licence. A job that ran is not ended from here, since its completion and its Destroy are
# on their way, and its lease is the control plane's either way.
function Exit-Listener([int]$status) {
	if (-not (Test-Path -LiteralPath $env:BILLET_JOB_STARTED_MARK)) {
		Stop-ThisMachine
	}

	exit $status
}

try {
	foreach ($line in Get-Content -LiteralPath $file) {
		$at = $line.IndexOf('=')
		if ($at -gt 0) {
			[Environment]::SetEnvironmentVariable($line.Substring(0, $at), $line.Substring($at + 1), 'Process')
		}
	}

	# GONE BEFORE ANY JOB STEP EXISTS: a step runs as this account and could read it.
	Remove-Item -LiteralPath $file -Force

	$env:ACTIONS_RUNNER_HOOK_JOB_STARTED = Join-Path $billet 'billet-job-started.ps1'
	$env:ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED = 'true'

	Set-Location -LiteralPath $root
	if (-not (Test-Path -LiteralPath $listener)) {
		throw 'no runner on this image'
	}
} catch {
	Exit-Listener 1
}

while ($true) {
	try {
		& $listener run
		$status = $LASTEXITCODE
	} catch {
		Exit-Listener 1
	}

	switch ($status) {
		{ $_ -ge 100 -and $_ -le 105 } { Exit-Listener $status }
		{ $_ -in 0, 1, 5 } { Exit-Listener 0 }
		2 { Start-Sleep -Seconds 5; continue }
		{ $_ -in 3, 4 } {
			$finished = Join-Path $root 'update.finished'
			for ($i = 0; $i -le 30; $i++) {
				if (Test-Path -LiteralPath $finished) {
					Remove-Item -LiteralPath $finished -Force

					break
				}

				Start-Sleep -Seconds 1
			}

			continue
		}
		7 {
			if ($env:ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE -eq '1') {
				Exit-Listener 7
			}

			Exit-Listener 0
		}
		default { Exit-Listener $status }
	}
}
