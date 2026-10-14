param([Parameter(Mandatory=$true)][string]$Python,[string]$Ffmpeg='',[string]$Ffprobe='')
$ErrorActionPreference='Stop'
$projectRoot=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$buildRoot=Join-Path $projectRoot 'work/build'
$distRoot=Join-Path $projectRoot 'dist'
New-Item -ItemType Directory -Force -Path $buildRoot,$distRoot | Out-Null
& $Python -m PyInstaller --noconfirm --clean --workpath $buildRoot --distpath $distRoot (Join-Path $PSScriptRoot 'MusicOps.spec')
if ($LASTEXITCODE -ne 0) { throw 'PyInstaller build failed' }
$bundleRoot=Join-Path $distRoot 'MusicOps-Windows'
New-Item -ItemType Directory -Force -Path (Join-Path $bundleRoot 'tools') | Out-Null
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
& $Python (Join-Path $PSScriptRoot 'collect_licenses.py') $bundleRoot
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'THIRD-PARTY-NOTICES.md') -Destination $bundleRoot
Copy-Item -LiteralPath (Join-Path $projectRoot 'docs/用户操作说明.md') -Destination $bundleRoot
Copy-Item -LiteralPath (Join-Path $projectRoot 'docs/运维与恢复说明.md') -Destination $bundleRoot
if (Test-Path -LiteralPath (Join-Path $projectRoot 'docs/packaging-build-source.json')) { Copy-Item -LiteralPath (Join-Path $projectRoot 'docs/packaging-build-source.json') -Destination $bundleRoot }
if (Test-Path -LiteralPath (Join-Path $projectRoot 'requirements.lock')) { Copy-Item -LiteralPath (Join-Path $projectRoot 'requirements.lock') -Destination $bundleRoot }
$manifest=Get-ChildItem -LiteralPath $bundleRoot -Recurse -File | ForEach-Object { [PSCustomObject]@{path=$_.FullName.Substring($bundleRoot.Length+1);size=$_.Length;sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash} }
$manifest | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $bundleRoot 'manifest.json') -Encoding utf8
Write-Output "Built: $bundleRoot"
