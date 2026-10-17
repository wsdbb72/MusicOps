"""Official-release updates, bounded downloads and hash-checked installer handoff."""
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import time
from urllib.parse import urlsplit
from urllib.request import Request, build_opener, HTTPRedirectHandler
import uuid

from musicops import __version__
from musicops.library.paths import reject_reparse_chain
from musicops.platforms import private_directory, user_data_root

REPO = 'wsdbb72/MusicOps'
LATEST = 'https://api.github.com/repos/' + REPO + '/releases/latest'
HOSTS = {'api.github.com', 'github.com', 'release-assets.githubusercontent.com', 'objects.githubusercontent.com'}
MAX_INSTALLER = 512 * 1024 * 1024


def version_tuple(value):
    if not isinstance(value, str) or not re.fullmatch(r'\d{1,6}\.\d{1,6}\.\d{1,6}', value):
        raise ValueError('更新版本格式无效')
    return tuple(map(int, value.split('.')))


def installer_name(version, system=None, machine=None):
    version_tuple(version); system = system or sys.platform; machine = (machine or platform.machine()).lower()
    if system == 'win32' and machine in {'amd64', 'x86_64'}:
        return f'MusicOps-{version}-Setup.exe'
    if system == 'darwin' and machine in {'arm64', 'aarch64'}:
        return f'MusicOps-{version}-macOS-arm64.pkg'
    raise ValueError('当前系统没有自动更新安装包；新版 Mac 仅支持 Apple Silicon')


def trusted_url(url):
    parsed = urlsplit(url)
    if parsed.scheme != 'https' or parsed.hostname not in HOSTS or parsed.username or parsed.password or parsed.port not in (None, 443):
        raise ValueError('更新下载地址不可信')
    return url


class SafeRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        trusted_url(newurl)
        return super().redirect_request(request, fp, code, msg, headers, newurl)


@dataclass(frozen=True)
class StagedUpdate:
    version: str
    path: Path
    sha256: str
    size: int


def verify_staged(update):
    reject_reparse_chain(update.path)
    if not update.path.is_file() or update.path.stat().st_size != update.size:
        raise ValueError('更新安装包不完整，请重新检查更新')
    with update.path.open('rb') as stream:
        if hashlib.file_digest(stream, 'sha256').hexdigest() != update.sha256:
            raise ValueError('更新安装包校验失败，请重新下载')


class Updater:
    def __init__(self, directory=None, opener=None, current=__version__, system=None, machine=None):
        self.directory = Path(directory) if directory else user_data_root() / 'MusicOps' / 'updates'
        self.opener = opener or build_opener(SafeRedirect())
        self.current = current; self.system = system; self.machine = machine

    def request(self, url):
        trusted_url(url)
        return self.opener.open(Request(url, headers={'User-Agent': 'MusicOps/' + self.current, 'Accept': 'application/vnd.github+json' if url == LATEST else 'application/octet-stream'}), timeout=20)

    def stage_latest(self, progress=lambda _: None):
        progress('正在检查 GitHub 更新…')
        with self.request(LATEST) as response:
            payload = response.read(2 * 1024 * 1024 + 1)
        if len(payload) > 2 * 1024 * 1024: raise ValueError('更新信息超出大小限制')
        release = json.loads(payload)
        tag = release.get('tag_name', ''); version = tag.removeprefix('v')
        if not tag.startswith('v') or release.get('draft') is not False or release.get('prerelease') is not False:
            raise ValueError('更新信息不是正式版本')
        if version_tuple(version) <= version_tuple(self.current):
            return None
        name = installer_name(version, self.system, self.machine)
        expected_url = f'https://github.com/{REPO}/releases/download/{tag}/{name}'
        assets = [a for a in release.get('assets', []) if a.get('name') == name]
        if len(assets) != 1: raise ValueError('新版尚未提供当前系统的安装包')
        asset = assets[0]; size = asset.get('size'); digest = asset.get('digest', '')
        if asset.get('browser_download_url') != expected_url or type(size) is not int or not 0 < size <= MAX_INSTALLER or not re.fullmatch('sha256:[a-f0-9]{64}', digest):
            raise ValueError('新版安装包缺少有效的来源或校验信息')
        reject_reparse_chain(self.directory); directory = private_directory(self.directory / version)
        update = StagedUpdate(version, directory / name, digest[7:], size)
        if update.path.exists():
            try: verify_staged(update); return update
            except ValueError: pass
        partial = directory / (name + '.' + uuid.uuid4().hex + '.part')
        try:
            received = 0; started = time.monotonic(); last_notice = -1
            with self.request(expected_url) as response, partial.open('xb') as output:
                while True:
                    chunk = response.read(256 * 1024)
                    if not chunk: break
                    received += len(chunk)
                    if received > size or time.monotonic() - started > 600: raise ValueError('更新下载大小或时间超出限制')
                    output.write(chunk)
                    percent = received * 100 // size
                    if percent != last_notice: progress(f'正在下载 MusicOps {version}：{percent}%'); last_notice = percent
                output.flush(); os.fsync(output.fileno())
            pending = StagedUpdate(version, partial, update.sha256, size); verify_staged(pending)
            reject_reparse_chain(update.path); os.replace(partial, update.path)
        finally:
            partial.unlink(missing_ok=True)
        return update


def launch_installer(update, executable=None, system=None, launcher=subprocess.Popen, wait_pid=None):
    verify_staged(update)
    system = system or sys.platform; executable = Path(executable or sys.executable).resolve()
    if system == 'win32':
        # Inno checks open files during initialization. Launch only after this
        # GUI has exited and the safely stopped worker has released its EXE.
        helper = update.path.parent / 'apply-update.ps1'
        payload = update.path.parent / ('handoff-' + uuid.uuid4().hex + '.json')
        reject_reparse_chain(helper); reject_reparse_chain(payload)
        helper.write_text(WINDOWS_HANDOFF, 'utf-8-sig')
        payload.write_text(json.dumps({'pid': wait_pid if wait_pid is not None else os.getpid(), 'installer': str(update.path),
            'sha256': update.sha256, 'size': update.size, 'directory': str(executable.parent)}), 'utf-8')
        powershell = Path(os.environ.get('SystemRoot', 'C:/Windows')) / 'System32/WindowsPowerShell/v1.0/powershell.exe'
        return launcher([str(powershell), '-NoProfile', '-NonInteractive', '-WindowStyle', 'Hidden',
                         '-ExecutionPolicy', 'Bypass', '-File', str(helper), '-Payload', str(payload)],
                        cwd=str(update.path.parent), creationflags=0x08000000)
    if system == 'darwin':
        return launcher(['/usr/bin/open', str(update.path)], start_new_session=True)
    raise ValueError('当前系统没有自动更新安装器')


WINDOWS_HANDOFF = r'''param([Parameter(Mandatory=$true)][string]$Payload)
$ErrorActionPreference = 'Stop'
try {
    $updateInfo = Get-Content -LiteralPath $Payload -Raw -Encoding UTF8 | ConvertFrom-Json
    $exitDeadline = [DateTime]::UtcNow.AddSeconds(600)
    while (Get-Process -Id $updateInfo.pid -ErrorAction SilentlyContinue) {
        if ([DateTime]::UtcNow -gt $exitDeadline) { throw 'MusicOps did not exit' }
        Start-Sleep -Milliseconds 200
    }
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $free = $true
        foreach ($name in @('MusicOps.exe','MusicOps-worker.exe')) {
            $candidate = Join-Path $updateInfo.directory $name
            if (Test-Path -LiteralPath $candidate) {
                try { $stream = [IO.File]::Open($candidate,'Open','Read','None'); $stream.Dispose() }
                catch { $free = $false }
            }
        }
        if (-not $free) { Start-Sleep -Milliseconds 300 }
    } until ($free -or [DateTime]::UtcNow -gt $deadline)
    if (-not $free) { throw 'MusicOps is still in use' }
    $installerInfo = Get-Item -LiteralPath $updateInfo.installer
    if (($installerInfo.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Invalid installer path' }
    $hashAlgorithm = [Security.Cryptography.SHA256]::Create()
    $installerStream = [IO.File]::OpenRead($updateInfo.installer)
    try { $actualHash = [BitConverter]::ToString($hashAlgorithm.ComputeHash($installerStream)).Replace('-','').ToLowerInvariant() }
    finally { $installerStream.Dispose(); $hashAlgorithm.Dispose() }
    if ($installerInfo.Length -ne $updateInfo.size -or $actualHash -ne $updateInfo.sha256) { throw 'Update checksum mismatch' }
    Start-Process -FilePath $updateInfo.installer -ArgumentList @('/DIR="' + $updateInfo.directory + '"')
} catch {
    ('Update handoff failed at line ' + $_.InvocationInfo.ScriptLineNumber + ': ' + $_.Exception.Message) | Set-Content -LiteralPath (Join-Path (Split-Path -LiteralPath $Payload) 'handoff-error.txt') -Encoding UTF8
    exit 1
}
'''
