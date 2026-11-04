# prepare-aws-tools.ps1: Amazon's Windows AMI carries the AWS Tools for PowerShell in a
# directory on the machine's module path, and Azure's does not. Install-PowerShellModules.ps1
# installs AWSPowerShell from the gallery without -AllowClobber, so with Amazon's copy visible
# it stops at "may override the existing commands" (measured, 2026-11-04). The template's own
# copy takes its place: Amazon's stays on the disk and off the path.
$ErrorActionPreference = 'Stop'

$path = [Environment]::GetEnvironmentVariable('PSModulePath', 'Machine')
$kept = @($path.Split(';') | Where-Object { $_ -notlike '*\AWS Tools\*' -and $_ -ne '' })
[Environment]::SetEnvironmentVariable('PSModulePath', ($kept -join ';'), 'Machine')
