param(
  [Parameter(Mandatory=$true)][string]$InstallRoot
)
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($InstallRoot).TrimEnd('\') + '\'
$services = [IO.Path]::GetFullPath((Join-Path $root 'tools\services')).TrimEnd('\') + '\'
if (-not (Test-Path -LiteralPath $services -PathType Container)) { exit 0 }
$managed = @(Get-Process -ErrorAction SilentlyContinue | Where-Object {
  $path = $null
  try { $path = $_.Path } catch {}
  $path -and [IO.Path]::GetFullPath($path).StartsWith($services, [StringComparison]::OrdinalIgnoreCase)
})
foreach ($process in $managed) {
  try { Stop-Process -Id $process.Id -Force -ErrorAction Stop } catch {}
}
$deadline = [DateTime]::UtcNow.AddSeconds(10)
do {
  $remaining = @(Get-Process -ErrorAction SilentlyContinue | Where-Object {
    $path = $null
    try { $path = $_.Path } catch {}
    $path -and [IO.Path]::GetFullPath($path).StartsWith($services, [StringComparison]::OrdinalIgnoreCase)
  })
  if ($remaining.Count -eq 0) { exit 0 }
  Start-Sleep -Milliseconds 100
} while ([DateTime]::UtcNow -lt $deadline)
[Console]::Error.WriteLine('MusicOps bundled helper processes are still running: ' + (($remaining | ForEach-Object { $_.ProcessName + ':' + $_.Id }) -join ', '))
exit 1
