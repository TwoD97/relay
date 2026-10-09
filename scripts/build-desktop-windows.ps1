[CmdletBinding()]
param(
    [string]$SourceRoot,
    [string]$StageRoot = (Join-Path $env:LOCALAPPDATA 'RelayBuild\public-desktop'),
    [string]$NoticesPath,
    [switch]$CompileOnly,
    [switch]$StageOnly
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Set-StrictMode -Version Latest
if (-not $SourceRoot) { $SourceRoot = Split-Path -Parent $PSScriptRoot }

# Build on a native Windows drive. Reading the checkout through a UNC share is
# supported; invoking npm/cargo with a UNC current directory is not reliable.
$SourceRoot = (Resolve-Path -LiteralPath $SourceRoot).ProviderPath
$StageRoot = [IO.Path]::GetFullPath($StageRoot)
if ($StageRoot.StartsWith('\\')) { throw 'StageRoot must be on a native Windows drive.' }
$SourcePrefix = $SourceRoot.TrimEnd('\') + '\'
$StagePrefix = $StageRoot.TrimEnd('\') + '\'
if ($StagePrefix.StartsWith($SourcePrefix, [StringComparison]::OrdinalIgnoreCase) -or
    $SourcePrefix.StartsWith($StagePrefix, [StringComparison]::OrdinalIgnoreCase)) {
    throw 'The build stage must be separate from and outside the source checkout.'
}
# Stage cleanup is restricted to a directory explicitly owned by this builder.
# Never follow a junction into a checkout or another application's files.
$Ancestor = $StageRoot
while ($Ancestor) {
    if (Test-Path -LiteralPath $Ancestor) {
        if (((Get-Item -LiteralPath $Ancestor -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw 'The build stage and its parents must not be symbolic links or junctions.'
        }
    }
    $Ancestor = Split-Path -Parent $Ancestor
}
$DesktopSource = Join-Path $SourceRoot 'desktop'
$BundleSource = Join-Path $SourceRoot 'dist'
if (-not $NoticesPath) { $NoticesPath = Join-Path $DesktopSource 'resources\RUST_THIRD_PARTY_NOTICES.txt' }
if (-not (Test-Path -LiteralPath $NoticesPath -PathType Leaf)) { throw 'Generate desktop notices with --target x86_64-pc-windows-msvc before building Windows.' }
if (-not ([IO.File]::ReadAllText($NoticesPath).Contains('Target: x86_64-pc-windows-msvc'))) {
    throw 'The supplied notices file was not generated for x86_64-pc-windows-msvc.'
}

$RequiredArtifacts = @('relay-linux-amd64', 'relay-linux-arm64')
if (-not $CompileOnly) { $RequiredArtifacts += 'relay-controller-windows-amd64.exe' }
$Manifest = @{}
$ManifestNames = @('SHA256SUMS')
if (-not $CompileOnly) { $ManifestNames += 'SHA256SUMS.windows' }
foreach ($ManifestName in $ManifestNames) {
    foreach ($Line in [IO.File]::ReadAllLines((Join-Path $BundleSource $ManifestName))) {
        if ($Line -notmatch '^([a-fA-F0-9]{64})\s+\*?([A-Za-z0-9._-]+)$') { throw 'Invalid runtime checksum manifest.' }
        if ($Manifest.ContainsKey($Matches[2])) { throw 'Duplicate runtime checksum entry.' }
        $Manifest[$Matches[2]] = $Matches[1]
    }
}
foreach ($Name in $RequiredArtifacts) {
    $Artifact = Join-Path $BundleSource $Name
    if (-not $Manifest.ContainsKey($Name)) { throw "Missing checksum for $Name." }
    if (-not (Test-Path -LiteralPath $Artifact -PathType Leaf)) { throw "Missing runtime artifact: $Name." }
    if ((Get-FileHash -LiteralPath $Artifact -Algorithm SHA256).Hash -ne $Manifest[$Name]) { throw "Runtime checksum mismatch: $Name." }
}

New-Item -ItemType Directory -Force -Path $StageRoot | Out-Null
$StageMarker = Join-Path $StageRoot '.relay-desktop-stage.json'
if (Test-Path -LiteralPath $StageMarker) {
    if (((Get-Item -LiteralPath $StageMarker -Force).Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Invalid build-stage marker.' }
    $Owner = Get-Content -LiteralPath $StageMarker -Raw | ConvertFrom-Json
    if ($Owner.format -ne 1 -or $Owner.application -ne 'relay-desktop' -or $Owner.source -ne $SourceRoot) { throw 'The build stage belongs to another source checkout.' }
} else {
    if (@(Get-ChildItem -LiteralPath $StageRoot -Force).Count -ne 0) { throw 'Existing unmarked build stage: choose a new empty StageRoot. Existing files were preserved.' }
    @{format=1; application='relay-desktop'; source=$SourceRoot} | ConvertTo-Json | Set-Content -LiteralPath $StageMarker -Encoding UTF8
}
$ManagedTrees = @('src', 'assets', 'icons', 'permissions', 'resources')
$SourceTrees = @('src', 'assets', 'icons', 'permissions')
$SourceFiles = @('Cargo.toml', 'Cargo.lock', 'build.rs', 'rust-toolchain.toml', 'tauri.conf.json', 'tauri.windows.conf.json', 'package.json', 'package-lock.json')
function Assert-PlainTree([string]$Path) {
    $Item = Get-Item -LiteralPath $Path -Force
    if (($Item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Build input contains a symbolic link or junction: $Path"
    }
    if ($Item.PSIsContainer) {
        # Check each entry before descending; Get-ChildItem -Recurse can follow
        # junctions on some PowerShell versions before reporting their attributes.
        foreach ($Child in Get-ChildItem -LiteralPath $Path -Force) {
            Assert-PlainTree $Child.FullName
        }
    }
}
foreach ($Name in $SourceTrees) {
    $InputPath = Join-Path $DesktopSource $Name
    if (-not (Test-Path -LiteralPath $InputPath -PathType Container)) { throw "Missing source directory: $Name" }
    Assert-PlainTree $InputPath
}
foreach ($Name in $SourceFiles) {
    $InputPath = Join-Path $DesktopSource $Name
    if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) { throw "Missing source file: $Name" }
    Assert-PlainTree $InputPath
}
foreach ($Name in $RequiredArtifacts + $ManifestNames + @('VERSION', 'LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.txt', 'README.md')) {
    if (-not (Test-Path -LiteralPath (Join-Path $BundleSource $Name) -PathType Leaf)) { throw "Missing bundle file: $Name" }
    Assert-PlainTree (Join-Path $BundleSource $Name)
}
if (Test-Path -LiteralPath (Join-Path $BundleSource 'docs')) { Assert-PlainTree (Join-Path $BundleSource 'docs') }
Assert-PlainTree $NoticesPath
foreach ($Name in $SourceFiles) {
    $File = Join-Path $StageRoot $Name
    if (Test-Path -LiteralPath $File) {
        Assert-PlainTree $File
        if ((Get-Item -LiteralPath $File).PSIsContainer) { throw "Expected a stage configuration file: $Name" }
    }
}
foreach ($Name in $ManagedTrees) {
    $Tree = Join-Path $StageRoot $Name
    if (Test-Path -LiteralPath $Tree) { Assert-PlainTree $Tree }
}
# Replace managed inputs instead of merging: deleted source assets and obsolete
# runtime files must not remain in a release. Cargo's target cache stays intact.
foreach ($Name in $ManagedTrees) {
    $Tree = Join-Path $StageRoot $Name
    if (Test-Path -LiteralPath $Tree) { Remove-Item -LiteralPath $Tree -Recurse -Force }
}
foreach ($Name in $SourceFiles) {
    $File = Join-Path $StageRoot $Name
    # Remove the old directory entry first so a hard link cannot redirect a write.
    if (Test-Path -LiteralPath $File) { Remove-Item -LiteralPath $File -Force }
    Copy-Item -LiteralPath (Join-Path $DesktopSource $Name) -Destination $File
}
foreach ($Name in $SourceTrees) {
    Copy-Item -LiteralPath (Join-Path $DesktopSource $Name) -Destination $StageRoot -Recurse -Force
}
$Resources = Join-Path $StageRoot 'resources'
$Runtime = Join-Path $Resources 'runtime'
New-Item -ItemType Directory -Force -Path $Runtime | Out-Null
foreach ($Name in $RequiredArtifacts + $ManifestNames + @('VERSION', 'LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES.md', 'THIRD_PARTY_NOTICES.txt', 'README.md')) {
    Copy-Item -LiteralPath (Join-Path $BundleSource $Name) -Destination (Join-Path $Runtime $Name) -Force
}
foreach ($Name in $RequiredArtifacts) {
    if ((Get-FileHash -LiteralPath (Join-Path $Runtime $Name) -Algorithm SHA256).Hash -ne $Manifest[$Name]) {
        throw "Staged runtime checksum mismatch: $Name."
    }
}
if (Test-Path -LiteralPath (Join-Path $BundleSource 'docs')) {
    Copy-Item -LiteralPath (Join-Path $BundleSource 'docs') -Destination $Runtime -Recurse -Force
}
Copy-Item -LiteralPath $NoticesPath -Destination (Join-Path $Resources 'RUST_THIRD_PARTY_NOTICES.txt') -Force
if ($StageOnly) {
    Write-Output "Verified and staged desktop inputs at $StageRoot. No tools, installer or application were started."
    return
}

$CargoBin = Join-Path $env:USERPROFILE '.cargo\bin'
$env:PATH = "$CargoBin;C:\Program Files\nodejs;$env:PATH"
Push-Location $StageRoot
try {
    if (-not $CompileOnly) {
        $ExpectedVersion = [IO.File]::ReadAllText((Join-Path $Runtime 'VERSION')).Trim()
        $NativeVersion = (& (Join-Path $Runtime 'relay-controller-windows-amd64.exe') version | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or $NativeVersion -ne $ExpectedVersion) {
            throw 'The native Windows controller version does not match the runtime bundle.'
        }
    }
    & cargo.exe fmt --check
    if ($LASTEXITCODE -ne 0) { throw 'Rust formatting failed.' }
    & cargo.exe test --locked
    if ($LASTEXITCODE -ne 0) { throw 'Native Windows unit tests failed.' }
    & cargo.exe clippy --locked --all-targets -- -D warnings
    if ($LASTEXITCODE -ne 0) { throw 'Native Windows Clippy checks failed.' }
    if ($CompileOnly) {
        & cargo.exe build --release --locked
        if ($LASTEXITCODE -ne 0) { throw 'Native Windows release compilation failed.' }
        Write-Output "Compiled native Windows app at $StageRoot\target\release\relay-desktop.exe. No installer was built or installed."
        return
    }
    & npm.cmd ci --no-audit --no-fund
    if ($LASTEXITCODE -ne 0) { throw 'Installing pinned Tauri CLI dependencies failed.' }
    & .\node_modules\.bin\tauri.cmd build --bundles nsis --ci -- --locked
    if ($LASTEXITCODE -ne 0) { throw 'Building the Windows installer failed.' }
    $Installers = @(Get-ChildItem -LiteralPath (Join-Path $StageRoot 'target\release\bundle\nsis') -Filter '*-setup.exe' | Sort-Object LastWriteTime -Descending)
    if ($Installers.Count -eq 0) { throw 'No NSIS installer was produced.' }
    $Output = Join-Path $BundleSource 'relay-desktop-windows-amd64-setup.exe'
    Copy-Item -LiteralPath $Installers[0].FullName -Destination $Output -Force
    $Digest = (Get-FileHash -LiteralPath $Output -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$Output.sha256", "$Digest  relay-desktop-windows-amd64-setup.exe`n", [Text.UTF8Encoding]::new($false))
    Write-Output "Built unsigned native Windows installer: $Output"
    Write-Output 'No application was installed or launched by this build script.'
} finally {
    Pop-Location
}
