<# Fresh-account CI gate. Intentionally refuses to replace an existing Relay install.
   Interactive WebView2/SSH checks are performed separately by test_windows_desktop.ps1. #>
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$Installer,
    [Parameter(Mandatory=$true)][string]$Bundle
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$Installer = (Resolve-Path -LiteralPath $Installer).ProviderPath
$Bundle = (Resolve-Path -LiteralPath $Bundle).ProviderPath
$Installed = Join-Path $env:LOCALAPPDATA 'Relay'
$PreviousInstall = Test-Path -LiteralPath $Installed
foreach ($Hive in @('HKCU:', 'HKLM:')) {
    foreach ($View in @('Software', 'Software\WOW6432Node')) {
        foreach ($Key in @('relay\Relay', 'Microsoft\Windows\CurrentVersion\Uninstall\Relay')) {
            $PreviousInstall = $PreviousInstall -or (Test-Path "$Hive\$View\$Key")
        }
        # NSIS also detects older WiX products by display name and publisher.
        $UninstallRoot = "$Hive\$View\Microsoft\Windows\CurrentVersion\Uninstall"
        if (Test-Path $UninstallRoot) {
            foreach ($Key in Get-ChildItem -LiteralPath $UninstallRoot) {
                $Values = Get-ItemProperty -LiteralPath $Key.PSPath
                if ($Values.PSObject.Properties['DisplayName'] -and $Values.DisplayName -eq 'Relay') {
                    $PreviousInstall = $true
                }
            }
        }
    }
}
if ($PreviousInstall) {
    throw 'This fresh-install test requires a disposable Windows account with no existing Relay installation.'
}
$Expected = ([IO.File]::ReadAllText("$Installer.sha256").Trim() -split '\s+')[0]
if ($Expected -notmatch '^[a-fA-F0-9]{64}$' -or (Get-FileHash -LiteralPath $Installer -Algorithm SHA256).Hash -ne $Expected) {
    throw 'Installer checksum mismatch.'
}
$Process = Start-Process -FilePath $Installer -ArgumentList '/S' -Wait -PassThru
if ($Process.ExitCode -ne 0) { throw "Installer failed with exit $($Process.ExitCode)." }
foreach ($Name in @('relay-desktop.exe', 'uninstall.exe', 'RUST_THIRD_PARTY_NOTICES.txt')) {
    if (-not (Test-Path -LiteralPath (Join-Path $Installed $Name) -PathType Leaf)) { throw "Installed file is missing: $Name" }
}
$Runtime = Join-Path $Installed 'runtime'
$Seen = @{}
foreach ($Name in @('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.txt')) {
    if (-not (Test-Path -LiteralPath (Join-Path $Runtime $Name) -PathType Leaf)) { throw "Installed attribution is missing: $Name" }
}
foreach ($Manifest in @('SHA256SUMS', 'SHA256SUMS.windows')) {
    foreach ($Line in [IO.File]::ReadAllLines((Join-Path $Bundle $Manifest))) {
        if ($Line -notmatch '^([a-fA-F0-9]{64})\s+\*?([A-Za-z0-9._-]+)$') { throw 'Invalid expected bundle manifest.' }
        $Name = $Matches[2]
        $Digest = $Matches[1]
        if ($Seen.ContainsKey($Name)) { throw 'Duplicate bundle entry.' }
        $Seen[$Name] = $true
        if ((Get-FileHash -LiteralPath (Join-Path $Runtime $Name) -Algorithm SHA256).Hash -ne $Digest) { throw "Installed payload mismatch: $Name" }
    }
}
foreach ($Name in @('relay-linux-amd64', 'relay-linux-arm64', 'relay-controller-windows-amd64.exe')) {
    if (-not $Seen.ContainsKey($Name)) { throw "Required payload missing: $Name" }
}
$Version = (& (Join-Path $Runtime 'relay-controller-windows-amd64.exe') version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $Version -ne [IO.File]::ReadAllText((Join-Path $Bundle 'VERSION')).Trim()) { throw 'Installed controller version mismatch.' }
$Shortcut = Join-Path ([Environment]::GetFolderPath('StartMenu')) 'Programs\Relay\Relay.lnk'
$Shell = New-Object -ComObject WScript.Shell
if (-not (Test-Path -LiteralPath $Shortcut) -or $Shell.CreateShortcut($Shortcut).TargetPath -ne (Join-Path $Installed 'relay-desktop.exe')) { throw 'Start menu shortcut does not target native Relay.' }
Write-Output "PASS: installer, native shortcut, notices, all runtime payload hashes and controller version ($Version). No GUI, WSL, provider login or session started."
