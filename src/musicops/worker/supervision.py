"""Windows packaged-worker supervision outside the GUI process lifetime."""
import os,subprocess,sys
from pathlib import Path

SCRIPT=r'''param([string]$WorkerExecutable,[string]$Base)
$ErrorActionPreference='Stop'
$state=Join-Path $Base 'state'
$stopFile=Join-Path $state 'supervisor-stop'
$sha=[Security.Cryptography.SHA256]::Create()
try {$key=[BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($Base.ToLowerInvariant()))).Replace('-','')}
finally {$sha.Dispose()}
$mutex=New-Object Threading.Mutex($false,('Local\MusicOpsWorkerSupervisor-'+$key))
$owned=$false
try {
  try {$owned=$mutex.WaitOne(0)} catch [Threading.AbandonedMutexException] {$owned=$true}
  if (-not $owned) {return}
  $starts=New-Object 'Collections.Generic.List[DateTime]'
  $child=$null
  while (-not (Test-Path -LiteralPath $stopFile)) {
    $processes=@(Get-CimInstance Win32_Process -Filter "Name='MusicOps-worker.exe'" | Where-Object {
      $_.ExecutablePath -eq $WorkerExecutable -and $_.CommandLine -match ('--base\s+"?'+[regex]::Escape($Base)+'"?(?:\s|$)')
    })
    if ($processes.Count -eq 0) {
      $cutoff=[DateTime]::UtcNow.AddMinutes(-10)
      for ($i=$starts.Count-1;$i -ge 0;$i--) {if ($starts[$i] -lt $cutoff) {$starts.RemoveAt($i)}}
      if ($starts.Count -ge 5) {Start-Sleep -Seconds 30;continue}
      $code=$null
      if ($child -and $child.HasExited) {$code=$child.ExitCode}
      @{at=[DateTime]::UtcNow.ToString('o');event='worker_missing_restart';exit_code=$code} | ConvertTo-Json -Compress | Add-Content -LiteralPath (Join-Path $state 'supervisor.jsonl') -Encoding UTF8
      if (Test-Path -LiteralPath $stopFile) {break}
      $child=Start-Process -FilePath $WorkerExecutable -ArgumentList @('--base',('"'+$Base+'"')) -WorkingDirectory $Base -WindowStyle Hidden -PassThru
      $starts.Add([DateTime]::UtcNow)
    }
    Start-Sleep -Seconds 5
  }
} finally {if ($owned) {$mutex.ReleaseMutex()};$mutex.Dispose()}
'''


def start(base):
    if os.name!='nt' or not getattr(sys,'frozen',False):return
    base=Path(base);state=base/'state';state.mkdir(parents=True,exist_ok=True)
    (state/'supervisor-stop').unlink(missing_ok=True)
    worker=Path(sys.executable).parent/'MusicOps-worker.exe'
    if not worker.is_file():return
    script=state/'worker-supervisor.ps1'
    script.write_text(SCRIPT,encoding='utf-8-sig')
    powershell=Path(os.environ.get('SystemRoot','C:/Windows'))/'System32/WindowsPowerShell/v1.0/powershell.exe'
    command=[str(powershell),'-NoProfile','-NonInteractive','-WindowStyle','Hidden','-ExecutionPolicy','Bypass','-File',str(script),'-WorkerExecutable',str(worker),'-Base',str(base)]
    options=dict(stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    try:
        # A tool/launcher may own a Windows job object. Break away when the job
        # permits it, so its teardown does not also terminate our supervisor.
        try:subprocess.Popen(command,creationflags=0x08000000|0x01000000,**options)
        except OSError:subprocess.Popen(command,creationflags=0x08000000,**options)
    except OSError as exc:
        from musicops.worker.lifecycle import append
        append(base,'supervisor_start_failed',error_class=type(exc).__name__)


def stop(base):
    state=Path(base)/'state';state.mkdir(parents=True,exist_ok=True)
    (state/'supervisor-stop').write_text('intentional shutdown',encoding='utf-8')
