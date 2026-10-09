<# Exercises release staging with disposable files; requires no compiler or app. #>
[CmdletBinding()]
param([string]$Builder)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if (-not $Builder) { $Builder = Join-Path $PSScriptRoot 'build-desktop-windows.ps1' }
$Builder = (Resolve-Path -LiteralPath $Builder).ProviderPath
$Root = Join-Path ([IO.Path]::GetTempPath()) ('relay-stage-test-' + [Guid]::NewGuid().ToString('N'))
$Source = Join-Path $Root 'checkout'
$Stage = Join-Path $Root 'stage'
$Notices = Join-Path $Root 'notices.txt'
function Assert-True($Condition, [string]$Message) { if (-not $Condition) { throw $Message } }
function Stage-Fixture([string]$Destination) {
    & $Builder -SourceRoot $Source -StageRoot $Destination -NoticesPath $Notices -CompileOnly -StageOnly | Out-Null
}
function Assert-Rejected([scriptblock]$Action, [string]$Expected) {
    $Failure = $null
    try { & $Action } catch { $Failure = $_.Exception.Message }
    Assert-True ($Failure -and $Failure.Contains($Expected)) "Expected rejection '$Expected'; got '$Failure'"
}
try {
    New-Item -ItemType Directory -Path $Root, $Source, (Join-Path $Source 'desktop'), (Join-Path $Source 'dist') | Out-Null
    foreach ($Name in @('src', 'assets', 'icons', 'permissions')) {
        New-Item -ItemType Directory -Path (Join-Path $Source "desktop\$Name") | Out-Null
        Set-Content -LiteralPath (Join-Path $Source "desktop\$Name\current.txt") -Value 'current'
    }
    foreach ($Name in @('Cargo.toml', 'Cargo.lock', 'build.rs', 'rust-toolchain.toml', 'tauri.conf.json', 'tauri.windows.conf.json', 'package.json', 'package-lock.json')) {
        Set-Content -LiteralPath (Join-Path $Source "desktop\$Name") -Value 'disposable fixture'
    }
    $Manifest = @()
    foreach ($Name in @('relay-linux-amd64', 'relay-linux-arm64')) {
        $File = Join-Path $Source "dist\$Name"
        Set-Content -LiteralPath $File -Value "disposable $Name"
        $Manifest += (Get-FileHash -LiteralPath $File -Algorithm SHA256).Hash + '  ' + $Name
    }
    Set-Content -LiteralPath (Join-Path $Source 'dist\SHA256SUMS') -Value $Manifest
    foreach ($Name in @('VERSION', 'LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.txt', 'README.md')) {
        Set-Content -LiteralPath (Join-Path $Source "dist\$Name") -Value 'fixture'
    }
    Set-Content -LiteralPath $Notices -Value 'Target: x86_64-pc-windows-msvc'
    Set-Content -LiteralPath (Join-Path $Source 'desktop\assets\removed.txt') -Value 'obsolete'
    Stage-Fixture $Stage
    Assert-True (Test-Path -LiteralPath (Join-Path $Stage '.relay-desktop-stage.json')) 'Missing stage ownership marker'
    foreach ($Name in @('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.txt')) {
        Assert-True (Test-Path -LiteralPath (Join-Path $Stage "resources\runtime\$Name")) "Missing staged attribution: $Name"
    }
    New-Item -ItemType Directory -Path (Join-Path $Stage 'target') | Out-Null
    Set-Content -LiteralPath (Join-Path $Stage 'target\cache-sentinel') -Value 'retain'
    Set-Content -LiteralPath (Join-Path $Stage 'resources\runtime\stale.exe') -Value 'obsolete'
    Remove-Item -LiteralPath (Join-Path $Source 'desktop\assets\removed.txt')
    Stage-Fixture $Stage
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $Stage 'assets\removed.txt'))) 'Deleted source asset survived staging'
    Assert-True (-not (Test-Path -LiteralPath (Join-Path $Stage 'resources\runtime\stale.exe'))) 'Stale runtime payload survived staging'
    Assert-True ((Get-Content -LiteralPath (Join-Path $Stage 'target\cache-sentinel')) -eq 'retain') 'Cargo cache was removed'
    Assert-Rejected { Stage-Fixture (Join-Path $Source 'stage') } 'separate from and outside'
    Assert-Rejected { Stage-Fixture $Root } 'separate from and outside'
    $Foreign = Join-Path $Root 'foreign'
    New-Item -ItemType Directory -Path $Foreign | Out-Null
    Set-Content -LiteralPath (Join-Path $Foreign 'sentinel') -Value 'preserve'
    Assert-Rejected { Stage-Fixture $Foreign } 'unmarked build stage'
    Assert-True ((Get-Content -LiteralPath (Join-Path $Foreign 'sentinel')) -eq 'preserve') 'Foreign files changed'
    $Marker = Join-Path $Stage '.relay-desktop-stage.json'
    $OriginalMarker = Get-Content -LiteralPath $Marker -Raw
    @{format=1; application='relay-desktop'; source=$Foreign} | ConvertTo-Json | Set-Content -LiteralPath $Marker
    Assert-Rejected { Stage-Fixture $Stage } 'another source checkout'
    Set-Content -LiteralPath $Marker -Value $OriginalMarker
    $Junction = Join-Path $Stage 'assets\external'
    New-Item -ItemType Junction -Path $Junction -Target $Foreign | Out-Null
    try {
        Assert-Rejected { Stage-Fixture $Stage } 'symbolic link or junction'
        Assert-True ((Get-Content -LiteralPath (Join-Path $Foreign 'sentinel')) -eq 'preserve') 'Junction target changed'
    } finally {
        # Delete only the junction itself, never recurse through it.
        [IO.Directory]::Delete($Junction)
    }
    $Junction = Join-Path $Source 'desktop\assets\external'
    New-Item -ItemType Junction -Path $Junction -Target $Foreign | Out-Null
    try {
        Assert-Rejected { Stage-Fixture $Stage } 'symbolic link or junction'
        Assert-True (Test-Path -LiteralPath (Join-Path $Stage 'assets\current.txt')) 'Source junction failure changed the prior stage'
    } finally { [IO.Directory]::Delete($Junction) }
    $Config = Join-Path $Stage 'Cargo.toml'
    Remove-Item -LiteralPath $Config
    New-Item -ItemType HardLink -Path $Config -Target (Join-Path $Foreign 'sentinel') | Out-Null
    Stage-Fixture $Stage
    Assert-True ((Get-Content -LiteralPath (Join-Path $Foreign 'sentinel')) -eq 'preserve') 'Staging wrote through a hard-linked configuration file'
    Remove-Item -LiteralPath (Join-Path $Source 'desktop\Cargo.toml')
    Assert-Rejected { Stage-Fixture $Stage } 'Cargo.toml'
    Assert-True (Test-Path -LiteralPath (Join-Path $Stage 'assets\current.txt')) 'Missing input changed the prior stage'
    Set-Content -LiteralPath (Join-Path $Source 'desktop\Cargo.toml') -Value 'disposable fixture'
    # Validate before cleanup: a damaged source bundle leaves the prior stage intact.
    Set-Content -LiteralPath (Join-Path $Source 'dist\relay-linux-amd64') -Value 'corrupt'
    Assert-Rejected { Stage-Fixture $Stage } 'checksum mismatch'
    Assert-True (Test-Path -LiteralPath (Join-Path $Stage 'assets\current.txt')) 'Validation failure removed the working stage'
    Write-Output 'PASS: clean restaging, cache preservation, source separation, ownership, junction safety and checksum failure.'
} finally {
    if (Test-Path -LiteralPath $Root) { Remove-Item -LiteralPath $Root -Recurse -Force }
}
