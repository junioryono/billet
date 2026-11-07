# prepare-documents.ps1: the build runs as SYSTEM, whose Documents folder does not exist on
# Amazon's AMI, and PowerShell's $profile is then an empty string, since it is made from a
# folder lookup that answers nothing for a folder that is not there. Configure-PowerShell.ps1
# writes the profile that loads PowerHTML into that path, and with it empty its New-Item and
# Add-Content fail without failing the step; Install-PyPy.ps1 then stops on ConvertFrom-HTML
# (Windows Server 2025, measured 2026-11-05). Packer's account has the folder from its logon.
$ErrorActionPreference = 'Stop'

$documents = [Environment]::GetFolderPath('MyDocuments', [Environment+SpecialFolderOption]::DoNotVerify)
if ([string]::IsNullOrEmpty($documents)) {
	throw 'this account names no Documents folder at all'
}

New-Item -ItemType Directory -Force -Path $documents | Out-Null

# THE NEXT POWERSHELL READS IT AT ITS START, which is the one the step runs in.
if ([string]::IsNullOrEmpty((& powershell.exe -NoProfile -NonInteractive -Command '$PROFILE'))) {
	throw "a new PowerShell still has no profile path with $documents in place"
}
