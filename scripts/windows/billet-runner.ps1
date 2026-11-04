# billet-runner.ps1: the entry point a Windows tier names (ADR-023). EC2Launch runs
# billet's user data as the local Administrator, measured on Windows Server 2025, with
# the registration in this process's environment, and this starts the runner as the
# local administrator `runner`, GitHub's runneradmin in all but name.
#
# THE REGISTRATION REACHES THE RUNNER IN NO PROCESS'S ARGUMENT LIST (#352). A scheduled
# task inherits nothing from this process, so it travels in a file only `runner` can
# read among the accounts that run anything here, which the service script reads and
# deletes before it starts the runner: every account on this machine is an administrator,
# so the deletion is what keeps it from a job's steps, which run as `runner`.
#
# THE PASSWORD IS THIS BOOT'S ALONE. The account has none anyone knows in the image, a
# scheduled task needs one to log it on, and it goes to the task through a cmdlet's
# parameter rather than an argument list.
#
# IT RETURNS ONCE THE TASK RUNS: EC2Launch waits for user data, and a job's length is no
# business of EC2Launch's. The job's end is the node's to act on, as on every backend.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$account = 'runner'
$dir = 'C:\billet\run'
$file = Join-Path $dir 'launch.env'
$task = 'billet-runner'

$passed = @(
	'ACTIONS_RUNNER_INPUT_JITCONFIG',
	'BILLET_LAUNCH_EPOCH_NS',
	'ACTIONS_RUNNER_RETURN_VERSION_DEPRECATED_EXIT_CODE'
)

if ([string]::IsNullOrEmpty($env:ACTIONS_RUNNER_INPUT_JITCONFIG)) {
	throw 'billet-runner: no registration reached this script, so no runner would start'
}

$lines = New-Object System.Collections.Generic.List[string]
foreach ($name in $passed) {
	$value = [Environment]::GetEnvironmentVariable($name)
	if ([string]::IsNullOrEmpty($value)) {
		continue
	}

	# A VALUE HOLDING A NEWLINE CANNOT TRAVEL ONE LINE PER VARIABLE, so it refuses the
	# launch, naming the variable and never the value.
	if ($value.Contains("`n") -or $value.Contains("`r")) {
		throw "billet-runner: $name holds a newline, which cannot reach the runner"
	}

	$lines.Add("$name=$value")
}

$lines.Add("BILLET_RUNNER_START_EPOCH_NS=$([DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() * 1000000)")

# THE ACL IS ON THE DIRECTORY BEFORE THE FILE EXISTS, and inherited: a file created and
# restricted afterwards is readable for the moment between the two.
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$acl = New-Object Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true, $false)
foreach ($who in 'NT AUTHORITY\SYSTEM', 'BUILTIN\Administrators', $account) {
	$acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule(
				$who, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
}
Set-Acl -LiteralPath $dir -AclObject $acl
Set-Content -LiteralPath $file -Value $lines -Encoding ascii

$bytes = New-Object byte[] 24
$random = [Security.Cryptography.RandomNumberGenerator]::Create()
$random.GetBytes($bytes)
$random.Dispose()
$password = 'aA1!' + [Convert]::ToBase64String($bytes)
Set-LocalUser -Name $account -Password (ConvertTo-SecureString $password -AsPlainText -Force)

$action = New-ScheduledTaskAction -Execute 'powershell.exe' `
	-Argument '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File C:\billet\billet-runner-service.ps1' `
	-WorkingDirectory 'C:\actions-runner'

# NO EXECUTION LIMIT: a task's default is three days, after which Windows would end the
# runner under a job still running.
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries `
	-DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $task -Action $action -Settings $settings -User $account `
	-Password $password -RunLevel Highest -Force | Out-Null
Start-ScheduledTask -TaskName $task
