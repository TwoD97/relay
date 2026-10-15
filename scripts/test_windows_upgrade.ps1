<# Disposable-account installer gate. This never opens Relay or contacts a host.
   It refuses existing user data and delegates the first install to the guarded
   fresh-install test. Pass an older installer and its matching runtime bundle
   to test a real version upgrade; otherwise the result is a reinstall gate. #>
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$Installer,
    [Parameter(Mandatory=$true)][string]$Bundle,
    [string]$PreviousInstaller,
    [string]$PreviousBundle,
    [string]$ExpectedDesktopVersion
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ([bool]$PreviousInstaller -ne [bool]$PreviousBundle) {
    throw 'PreviousInstaller and PreviousBundle must be supplied together.'
}
if (-not $ExpectedDesktopVersion) {
    $ExpectedDesktopVersion = (Get-Content -LiteralPath (Join-Path $PSScriptRoot '..\desktop\tauri.conf.json') -Raw | ConvertFrom-Json).version
}
if ($ExpectedDesktopVersion -notmatch '^\d+\.\d+\.\d+$') { throw 'ExpectedDesktopVersion must be a release version.' }
$Installer = (Resolve-Path -LiteralPath $Installer).ProviderPath
$Bundle = (Resolve-Path -LiteralPath $Bundle).ProviderPath
$ExpectedHash = ([IO.File]::ReadAllText("$Installer.sha256").Trim() -split '\s+')[0]
if ($ExpectedHash -notmatch '^[a-fA-F0-9]{64}$' -or (Get-FileHash -LiteralPath $Installer -Algorithm SHA256).Hash -ne $ExpectedHash) {
    throw 'Candidate installer checksum mismatch.'
}
$Data = Join-Path $env:LOCALAPPDATA 'RelayData'
$Config = Join-Path $env:APPDATA 'dev.relay.desktop'
$Profile = Join-Path $env:LOCALAPPDATA 'dev.relay.desktop'
foreach ($Path in @($Data, $Config, $Profile)) {
    if (Test-Path -LiteralPath $Path) {
        throw 'This upgrade test requires a disposable Windows account with no existing Relay data, appearance configuration, or WebView profile.'
    }
}
$InstallTest = Join-Path $PSScriptRoot 'test_windows_install.ps1'
if ($PreviousInstaller) {
    $PreviousInstaller = (Resolve-Path -LiteralPath $PreviousInstaller).ProviderPath
    $PreviousBundle = (Resolve-Path -LiteralPath $PreviousBundle).ProviderPath
    & $InstallTest -Installer $PreviousInstaller -Bundle $PreviousBundle
} else {
    & $InstallTest -Installer $Installer -Bundle $Bundle
}

$UninstallKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\Relay'
$BeforeVersion = (Get-ItemProperty -LiteralPath $UninstallKey).DisplayVersion
if ($PreviousInstaller -and ([Version]$BeforeVersion -ge [Version]$ExpectedDesktopVersion)) {
    throw 'The previous installer must have a lower desktop version for an upgrade gate.'
}
$State = Join-Path $Data 'relay-client'
foreach ($Path in @($State, $Config, $Profile)) {
    New-Item -ItemType Directory -Path $Path -Force | Out-Null
}
$Created = [DateTime]::UtcNow.ToString('o')
$Token = [Guid]::NewGuid().ToString('N')
$HostRecord = @{id='upgrade-test'; name='Disposable upgrade fixture'; target='fixture@upgrade.invalid'; port=22; status='disconnected'; stage='Saved fixture'; createdAt=$Created}
$Session = @{id=[Guid]::NewGuid().ToString('N'); createdAt=$Created; updatedAt=$Created; status='exited'; harness='claude'; agentMode='manual'; mastermindId=$Token; title='Saved coordinator'; workspace='Mastermind'; cwd='/tmp/relay-upgrade-fixture'; exitCode=0}
$Mastermind = @{format=1; revision=7; enabled=$false; config=@{hostId=$HostRecord.id; harness='claude'; agentMode='manual'; cwd=$Session.cwd}; status='disabled'; stage='Saved fixture'; session=$Session; updatedAt=$Created; phase='attached'; token=$Token; canonicalCwd=$Session.cwd; hostTarget=$HostRecord.target; hostPort=22; hostCreatedAt=$Created}
$UTF8 = New-Object System.Text.UTF8Encoding($false)
$Seeds = @{
    (Join-Path $State 'hosts.json') = (ConvertTo-Json -InputObject @($HostRecord) -Depth 10)
    (Join-Path $State 'mastermind.json') = (ConvertTo-Json -InputObject $Mastermind -Depth 10)
    (Join-Path $Config 'appearance.json') = '{"format":1,"preference":"light"}'
    (Join-Path $Profile 'relay-upgrade-fixture.txt') = 'Disposable WebView profile preservation marker'
}
$Hashes = @{}
foreach ($Path in $Seeds.Keys) {
    [IO.File]::WriteAllText($Path, $Seeds[$Path], $UTF8)
    $Hashes[$Path] = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash
}

# /UPDATE skips optional uninstall/data removal. /S is only used after proving
# this account has no existing install or Relay data above; no personal app is
# replaced or closed by this test.
$Process = Start-Process -FilePath $Installer -ArgumentList '/S', '/UPDATE' -Wait -PassThru
if ($Process.ExitCode -ne 0) { throw "Upgrade installer failed with exit $($Process.ExitCode)." }
foreach ($Path in $Hashes.Keys) {
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf) -or (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash -ne $Hashes[$Path]) {
        throw "Installer changed seeded account state: $([IO.Path]::GetFileName($Path))"
    }
}
$Installed = Join-Path $env:LOCALAPPDATA 'Relay'
$Desktop = Join-Path $Installed 'relay-desktop.exe'
$Runtime = Join-Path $Installed 'runtime'
$InstalledVersion = (Get-ItemProperty -LiteralPath $UninstallKey).DisplayVersion
$PEVersion = (Get-Item -LiteralPath $Desktop).VersionInfo.ProductVersion
if ($InstalledVersion -ne $ExpectedDesktopVersion -or $PEVersion -ne $ExpectedDesktopVersion) {
    throw 'Installed registry or PE desktop version does not match the expected release.'
}
$Seen = @{}
foreach ($Manifest in @('SHA256SUMS', 'SHA256SUMS.windows')) {
    foreach ($Line in [IO.File]::ReadAllLines((Join-Path $Bundle $Manifest))) {
        if ($Line -notmatch '^([a-fA-F0-9]{64})\s+\*?([A-Za-z0-9._-]+)$') { throw 'Invalid candidate runtime checksum manifest.' }
        $Name, $Digest = $Matches[2], $Matches[1]
        if ($Seen.ContainsKey($Name)) { throw 'Duplicate candidate runtime checksum entry.' }
        $Seen[$Name] = $true
        if ((Get-FileHash -LiteralPath (Join-Path $Runtime $Name) -Algorithm SHA256).Hash -ne $Digest) { throw "Upgraded payload mismatch: $Name" }
    }
}
foreach ($Name in @('relay-linux-amd64', 'relay-linux-arm64', 'relay-controller-windows-amd64.exe')) {
    if (-not $Seen.ContainsKey($Name)) { throw "Required upgraded payload missing: $Name" }
}
$Version = (& (Join-Path $Runtime 'relay-controller-windows-amd64.exe') version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $Version -ne [IO.File]::ReadAllText((Join-Path $Bundle 'VERSION')).Trim()) { throw 'Upgraded controller version mismatch.' }
$Shortcut = Join-Path ([Environment]::GetFolderPath('StartMenu')) 'Programs\Relay\Relay.lnk'
$Shell = New-Object -ComObject WScript.Shell
if (-not (Test-Path -LiteralPath $Shortcut) -or $Shell.CreateShortcut($Shortcut).TargetPath -ne $Desktop) { throw 'Start menu shortcut no longer targets the installed desktop.' }
$Kind = if ($PreviousInstaller) { 'version upgrade' } else { 'same-build reinstall' }
Write-Output "PASS: $Kind ($BeforeVersion -> $InstalledVersion); saved host, Mastermind identity, appearance, and profile markers unchanged; all three runtime payloads and native shortcut verified. No GUI, SSH, provider, or session was started."
