# billet-job-started.ps1: GitHub's job-started hook on a Windows image, selected by this
# extension. It says a job started, which the service script reads when the listener ends,
# and how long that took. Both are observation: a mark that cannot be written, or a missing
# or malformed timestamp, prints nothing and never turns into a failed workflow.
try {
	if (-not [string]::IsNullOrEmpty($env:BILLET_JOB_STARTED_MARK)) {
		Set-Content -LiteralPath $env:BILLET_JOB_STARTED_MARK -Value 'started' -Encoding ascii
	}
} catch {
	Write-Verbose 'no mark to leave'
}

try {
	$launch = [long]$env:BILLET_LAUNCH_EPOCH_NS
	$runner = [long]$env:BILLET_RUNNER_START_EPOCH_NS
	$now = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() * 1000000

	if ($launch -gt 0 -and $launch -le $runner -and $runner -le $now) {
		$toJob = [long](($now - $launch) / 1000000)
		$toRunner = [long](($runner - $launch) / 1000000)
		$fromRunner = [long](($now - $runner) / 1000000)
		Write-Output "billet timing: launch_to_job_start_ms=$toJob launch_to_runner_ms=$toRunner runner_to_job_start_ms=$fromRunner"
	}
} catch {
	Write-Verbose 'no timing to report'
}

exit 0
