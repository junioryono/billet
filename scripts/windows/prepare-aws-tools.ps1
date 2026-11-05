# prepare-aws-tools.ps1: Amazon's Windows AMI carries the modular AWS Tools for PowerShell,
# 429 AWS.Tools.* modules in Program Files\WindowsPowerShell\Modules, which is on every
# module path, and Azure's image carries none. Install-PowerShellModules.ps1 installs the
# AWSPowerShell module from the gallery without -AllowClobber, so it stops at "may override
# the existing commands" (Windows Server 2025, AWS.Tools 5.0.313, measured 2026-11-05). The
# template's module takes their place, as on GitHub's image, where a job finds AWSPowerShell
# and not the modular set: they are moved to a directory no module path names, and kept.
$ErrorActionPreference = 'Stop'

$modules = Join-Path $env:ProgramFiles 'WindowsPowerShell\Modules'
$aside = Join-Path $env:ProgramFiles 'Amazon\AWSToolsForPowerShell.OffModulePath'
New-Item -ItemType Directory -Force -Path $aside | Out-Null

foreach ($dir in @(Get-ChildItem -LiteralPath $modules -Directory -Filter 'AWS.Tools.*')) {
	Move-Item -LiteralPath $dir.FullName -Destination (Join-Path $aside $dir.Name)
}

# PROVED, NOT ASSUMED: the step it exists for refuses on the first command still visible.
$left = @(Get-Module -ListAvailable | Where-Object { $_.Name -like 'AWS.Tools.*' })
if ($left.Count -ne 0) {
	throw "$($left.Count) AWS.Tools modules are still on the module path, starting at $($left[0].ModuleBase)"
}
