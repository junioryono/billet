# billet-runner-service.ps1: runs as `runner`, in the scheduled task billet-runner.ps1
# starts. It takes the registration out of the file it arrived in and runs GitHub's
# listener with the update loop internal/guestassets/runner-service.sh carries, status
# for status, so a tier's result means one thing on both operating systems.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$file = 'C:\billet\run\launch.env'
foreach ($line in Get-Content -LiteralPath $file) {
	$at = $line.IndexOf('=')
	if ($at -gt 0) {
		[Environment]::SetEnvironmentVariable($line.Substring(0, $at), $line.Substring($at + 1), 'Process')
	}
}

# GONE BEFORE ANY JOB STEP EXISTS: a step runs as this account and could read it.
Remove-Item -LiteralPath $file -Force

$env:ACTIONS_RUNNER_HOOK_JOB_STARTED = 'C:\billet\billet-job-started.ps1'
$env:ACTIONS_RUNNER_RETURN_JOB_RESULT_FOR_HOSTED = 'true'

$root = 'C:\actions-runner'
Set-Location -LiteralPath $root

while ($true) {
	& "$root\bin\Runner.Listener.exe" run
	$status = $LASTEXITCODE

	switch ($status) {
		{ $_ -ge 100 -and $_ -le 105 } { exit $status }
		{ $_ -in 0, 1, 5 } { exit 0 }
		2 { Start-Sleep -Seconds 5; continue }
		{ $_ -in 3, 4 } {
			for ($i = 0; $i -le 30; $i++) {
				if (Test-Path -LiteralPath "$root\update.finished") {
					Remove-Item -LiteralPath "$root\update.finished" -Force

					break
				}

				Start-Sleep -Seconds 1
			}

			continue
		}
		7 {
			if ($env:ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE -eq '1') {
				exit 7
			}

			exit 0
		}
		default { exit $status }
	}
}
