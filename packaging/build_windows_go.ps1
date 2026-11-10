# Build the Go Windows bundle (dist/MusicOps-Windows), run the Go smoke
# tests and the releasegate against it, and compile the Inno installer.
#
#   packaging/build_windows_go.ps1 -Version 0.4.33 [-Python python] [-Ffmpeg <path>] [-Ffprobe <path>] [-Compiler <ISCC.exe>]
#
# The version is passed in by the release workflow; it is the only place
# the release version enters this script (the 4 canonical version files
# are untouched). Go binaries carry it twice: the -ldflags -X version
# string (read by `musicops version` / `MusicOps.exe --version`) and the
# PE version resource (generated with goversioninfo into a temporary
# .syso next to each main package; the .syso files are removed after the
# build so a later dev build never inherits a stale version).
# The desktop shell builds with CGO_ENABLED=0: Wails' WebView2 loader is
# pure Go (github.com/wailsapp/go-webview2), verified by inspecting the
# built binary for the WebView2 frontend symbols.
param(
    [Parameter(Mandatory=$true)][string]$Version,
    [string]$Python='python',
    [string]$Ffmpeg='',
    [string]$Ffprobe='',
    [string]$Go='go',
    [string]$Compiler=''
)
$ErrorActionPreference='Stop'
if ($Version -notmatch '^\d+\.\d+\.\d+$') { throw "Version must have three numeric components, got '$Version'" }
$parts = $Version.Split('.') | ForEach-Object { [int]$_ }
if ($parts | Where-Object { $_ -gt 65535 }) { throw 'Windows version components must fit 16 bits' }

$projectRoot=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$coreRoot=Join-Path $projectRoot 'native/core'
$buildRoot=Join-Path $projectRoot 'work/build-go'
$distRoot=Join-Path $projectRoot 'dist'
$bundleRoot=Join-Path $distRoot 'MusicOps-Windows'
New-Item -ItemType Directory -Force -Path $buildRoot,$distRoot,$bundleRoot | Out-Null

# 1. Managed engines (go-music-dl, FlareSolverr, browser, driver).
$engineCache=if ($env:MUSICOPS_ENGINE_CACHE) { $env:MUSICOPS_ENGINE_CACHE } else { Join-Path $projectRoot 'work/engine-build' }
& $Python (Join-Path $PSScriptRoot 'prepare_engines.py') --output (Join-Path $projectRoot 'work/engines') --cache $engineCache
if ($LASTEXITCODE -ne 0) { throw 'Bundled engine build failed' }

# 2. PE version resources via goversioninfo (temporary .syso files).
# The CLI reads a versioninfo.json skeleton (positional argument) and
# the flags below override it; the .syso files are removed after the
# build so a later dev build never inherits a stale version.
$coreSyso=Join-Path $coreRoot 'cmd/musicops/resource.syso'
$shellSyso=Join-Path $coreRoot 'cmd/musicops-desktop/resource.syso'
$versionJson=Join-Path $buildRoot 'versioninfo.json'
@'
{
	"FixedFileInfo": {
		"FileVersion": { "Major": 0, "Minor": 0, "Patch": 0, "Build": 0 },
		"ProductVersion": { "Major": 0, "Minor": 0, "Patch": 0, "Build": 0 },
		"FileFlagsMask": "3f",
		"FileFlags": "00",
		"FileOS": "040004",
		"FileType": "01",
		"FileSubType": "00"
	},
	"StringFileInfo": {
		"Comments": "",
		"CompanyName": "wsdbb72",
		"FileDescription": "MusicOps 音乐库管理",
		"FileVersion": "",
		"InternalName": "",
		"LegalCopyright": "",
		"LegalTrademarks": "",
		"OriginalFilename": "",
		"PrivateBuild": "",
		"ProductName": "MusicOps",
		"ProductVersion": "",
		"SpecialBuild": ""
	},
	"VarFileInfo": {
		"Translation": { "LangID": 2052, "CharsetID": 1200 }
	}
}
'@ | Set-Content -LiteralPath $versionJson -Encoding utf8
function New-VersionResource([string]$Out, [string]$FileName, [string]$Icon) {
    $args=@('-o', $Out,
        '-ver-major', $parts[0], '-ver-minor', $parts[1], '-ver-patch', $parts[2], '-ver-build', '0',
        '-product-ver-major', $parts[0], '-product-ver-minor', $parts[1], '-product-ver-patch', $parts[2], '-product-ver-build', '0',
        '-company', 'wsdbb72', '-description', 'MusicOps 音乐库管理',
        '-file-version', "$Version.0", '-internal-name', ([IO.Path]::GetFileNameWithoutExtension($FileName)),
        '-original-name', $FileName, '-product-name', 'MusicOps', '-product-version', $Version,
        '-translation', '2052', '-charset', '1200', '-64', $versionJson)
    if ($Icon) { $args += @('-icon', $Icon) }
    & $Go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo @args
    if ($LASTEXITCODE -ne 0) { throw "goversioninfo failed for $FileName" }
}
# goversioninfo and the Go builds resolve module packages, so they must
# run from inside the Go module (native/core). The workflow invokes this
# script from the repo root, where there is no go.mod: without this,
# `go run <module package>` fails with "go.mod file not found" (release
# run 42595317232). All paths used below are absolute, so changing the
# working directory is safe.
Push-Location $coreRoot
try {
    New-VersionResource $coreSyso 'musicops-core.exe' ''
    New-VersionResource $shellSyso 'MusicOps.exe' (Join-Path $projectRoot 'packaging/assets/musicops.ico')

    # 3. Go binaries.
    $ldflags="-s -w -X github.com/wsdbb72/MusicOps/native/core/internal/version.Version=$Version"
    $env:GOOS='windows'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
    & $Go build -trimpath -ldflags $ldflags -o (Join-Path $bundleRoot 'musicops-core.exe') ./cmd/musicops
    if ($LASTEXITCODE -ne 0) { throw 'Go core build failed' }
    # Wails v2 only compiles the real desktop shell with the production tag;
    # without it the binary embeds a stub that shows "Wails applications will
    # not build without the correct build tags" at launch (0.4.33 shipped
    # broken this way). -tags desktop,production matches `wails build`.
    # The shell must also be a GUI-subsystem binary (-H=windowsgui),
    # otherwise Windows opens a console window next to the app (0.4.33
    # showed a cmd window on launch).
    $shellLdflags="$ldflags -H=windowsgui"
    & $Go build -tags desktop,production -trimpath -ldflags $shellLdflags -o (Join-Path $bundleRoot 'MusicOps.exe') ./cmd/musicops-desktop
    if ($LASTEXITCODE -ne 0) { throw 'Go desktop shell build failed' }
    if (Select-String -LiteralPath (Join-Path $bundleRoot 'MusicOps.exe') -Pattern 'will not build without the correct build tags' -SimpleMatch -Quiet) { throw 'Desktop shell was built without the Wails production tag' }
    # Regression guard for the 0.4.33 console window: the shell's PE
    # subsystem must be 2 (Windows GUI), not 3 (console).
    $shellBytes = [System.IO.File]::ReadAllBytes((Join-Path $bundleRoot 'MusicOps.exe'))
    $peOffset = [BitConverter]::ToInt32($shellBytes, 0x3C)
    $shellSubsystem = [BitConverter]::ToUInt16($shellBytes, $peOffset + 24 + 68)
    if ($shellSubsystem -ne 2) { throw "Desktop shell PE subsystem is $shellSubsystem, want 2 (Windows GUI); a console window would appear" }
} finally {
    Remove-Item -LiteralPath $coreSyso,$shellSyso -Force -ErrorAction SilentlyContinue
    Pop-Location
}

# 4. Bundle contents.
New-Item -ItemType Directory -Force -Path (Join-Path $bundleRoot 'tools') | Out-Null
Copy-Item -LiteralPath (Join-Path $projectRoot 'work/engines') -Destination (Join-Path $bundleRoot 'tools/services') -Recurse -Force
foreach ($tool in @(@{Source=$Ffmpeg;Name='ffmpeg.exe'},@{Source=$Ffprobe;Name='ffprobe.exe'})) {
    if ($tool.Source -and (Test-Path -LiteralPath $tool.Source)) { Copy-Item -LiteralPath $tool.Source -Destination (Join-Path $bundleRoot ('tools/'+$tool.Name)) }
}
if ($Ffmpeg) {
    $ffmpegResolved=(Get-Item -LiteralPath $Ffmpeg).LinkTarget
    if (-not $ffmpegResolved) { $ffmpegResolved=$Ffmpeg }
    $ffmpegDistribution=[IO.Path]::GetDirectoryName([IO.Path]::GetDirectoryName($ffmpegResolved))
    foreach ($notice in @('LICENSE','README.txt')) {
        $sourceNotice=Join-Path $ffmpegDistribution $notice
        if (Test-Path -LiteralPath $sourceNotice) { Copy-Item -LiteralPath $sourceNotice -Destination (Join-Path $bundleRoot ('tools/FFmpeg-'+$notice)) }
    }
    & $Ffmpeg -version | Set-Content -LiteralPath (Join-Path $bundleRoot 'tools/FFmpeg-version.txt') -Encoding utf8
}
& $Python (Join-Path $PSScriptRoot 'collect_go_licenses.py') $bundleRoot
if ($LASTEXITCODE -ne 0) { throw 'Go license collection failed' }
# The .iss installs LICENSE from the bundle root (no skipifsourcedoesntexist),
# as the old Python freeze step did; without this ISCC fails with
# 'Source file ...\dist\MusicOps-Windows\LICENSE does not exist'
# (release run 42596352926).
Copy-Item -LiteralPath (Join-Path $projectRoot 'LICENSE') -Destination $bundleRoot
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'THIRD-PARTY-NOTICES.md') -Destination $bundleRoot
Copy-Item -LiteralPath (Join-Path $PSScriptRoot '启动说明.txt') -Destination $bundleRoot
Copy-Item -LiteralPath (Join-Path $projectRoot 'docs/用户操作说明.md') -Destination $bundleRoot
Copy-Item -LiteralPath (Join-Path $projectRoot 'docs/运维与恢复说明.md') -Destination $bundleRoot
if (Test-Path -LiteralPath (Join-Path $projectRoot 'docs/packaging-build-source.json')) { Copy-Item -LiteralPath (Join-Path $projectRoot 'docs/packaging-build-source.json') -Destination $bundleRoot }

# 5. Manifest (same {path,size,sha256} shape as the Python build).
$manifest=Get-ChildItem -LiteralPath $bundleRoot -Recurse -File | ForEach-Object { [PSCustomObject]@{path=$_.FullName.Substring($bundleRoot.Length+1);size=$_.Length;sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash} }
$manifest | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $bundleRoot 'manifest.json') -Encoding utf8
Write-Output "Built: $bundleRoot"

# 6. Go smoke tests against the real bundle, then the releasegate.
$smokeReport=Join-Path $buildRoot 'smoke-windows.json'
$env:MUSICOPS_SMOKE_BUNDLE=$bundleRoot
$env:MUSICOPS_SMOKE_VERSION=$Version
$env:MUSICOPS_SMOKE_DESKTOP='MusicOps.exe'
$env:MUSICOPS_SMOKE_REQUIRE_MANIFEST='1'
$env:MUSICOPS_SMOKE_REQUIRE_ENGINES='1'
$env:MUSICOPS_SMOKE_REPORT=$smokeReport
Push-Location $coreRoot
try {
    & $Go test ./internal/smoke/ -run 'TestBundle|TestWindowsPEVersion|TestPackagedLegacyMigrate' -count=1 -v
    if ($LASTEXITCODE -ne 0) { throw 'Go bundle smoke tests failed' }
    & $Go run ./cmd/releasegate --bundle $bundleRoot --version $Version --layout windows --desktop MusicOps.exe --require-engines --require-manifest --smoke-report $smokeReport
    if ($LASTEXITCODE -ne 0) { throw 'Releasegate rejected the Windows bundle' }
} finally { Pop-Location }

# 7. Inno installer + installer acceptance (only when a compiler is given).
if ($Compiler) {
    $installerOut=Join-Path $distRoot 'installer'
    New-Item -ItemType Directory -Force -Path $installerOut | Out-Null
    & $Compiler /DAppVersion=$Version /DBundleDir=$bundleRoot /O$installerOut (Join-Path $PSScriptRoot 'MusicOps-go.iss')
    if ($LASTEXITCODE -ne 0) { throw 'Inno Setup compile failed' }
    $installer=Get-ChildItem -LiteralPath $installerOut -Filter '*-Setup.exe' | Select-Object -First 1
    if (-not $installer) { throw 'Inno Setup produced no installer' }
    Write-Output "Installer: $($installer.FullName)"
    $env:MUSICOPS_SMOKE_INSTALLER_BUNDLE=$bundleRoot
    $env:MUSICOPS_SMOKE_INSTALLER_COMPILER=$Compiler
    $env:MUSICOPS_SMOKE_INSTALLER_WORK=Join-Path $buildRoot 'installer-acceptance'
    $env:MUSICOPS_SMOKE_INSTALLER_EXE=$installer.FullName
    $env:MUSICOPS_SMOKE_INSTALLER_VERSION=$Version
    $env:MUSICOPS_SMOKE_INSTALLER_REPORT=Join-Path $buildRoot 'installer-windows.json'
    Push-Location $coreRoot
    try {
        & $Go test ./internal/smoke/ -run TestWindowsInstaller -count=1 -v
        if ($LASTEXITCODE -ne 0) { throw 'Windows installer acceptance failed' }
    } finally { Pop-Location }
} else {
    Write-Output 'No Inno compiler given (-Compiler); installer build skipped.'
}
