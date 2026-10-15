"""Small platform boundary for private profiles, authenticated IPC and locking."""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import stat
import sys
import tempfile


def runtime_platform():return sys.platform
def is_windows():return runtime_platform().startswith('win')
def is_macos():return runtime_platform()=='darwin'


def user_data_root(override=None):
    if override:return Path(override)
    if is_macos():return Path.home()/'Library'/'Application Support'
    if is_windows():return Path(os.environ.get('LOCALAPPDATA') or Path.home()/'AppData'/'Local')
    return Path(os.environ.get('XDG_DATA_HOME') or Path.home()/'.local'/'share')


def private_directory(path):
    path=Path(path)
    if path.is_symlink():raise RuntimeError('私有运行目录不能是符号链接')
    path.mkdir(mode=0o700,parents=True,exist_ok=True)
    if not is_windows():
        info=path.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.getuid():raise RuntimeError('私有运行目录不属于当前账户')
        path.chmod(0o700)
    return path


def ipc_family():return 'AF_PIPE' if is_windows() else 'AF_UNIX'


def ipc_address(base):
    base=str(Path(base).resolve())
    if is_windows():
        key=hashlib.sha256((base.lower()+os.environ.get('USERNAME','')).encode()).hexdigest()[:24]
        return r'\\.\pipe\MusicOps-'+key
    # macOS sockaddr_un is short. Never embed the possibly long profile path.
    # /tmp is resolved once; the per-user directory is verified against symlinks.
    root=Path('/tmp').resolve() if is_macos() else Path(tempfile.gettempdir()).resolve()
    directory=private_directory(root/('musicops-'+str(os.getuid())))
    key=hashlib.sha256((base+'\0'+str(os.getuid())).encode()).hexdigest()[:24]
    address=str(directory/(key+'.sock'))
    if len(os.fsencode(address))>=100:raise RuntimeError('本机 IPC 地址过长，请检查临时目录设置')
    return address


def prepare_ipc_socket(base):
    """Called only after the profile writer lock is held; removes a stale socket."""
    if is_windows():return
    path=Path(ipc_address(base))
    try:info=path.lstat()
    except FileNotFoundError:return
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid!=os.getuid():raise RuntimeError('IPC 地址不是当前账户的套接字，拒绝替换')
    path.unlink()


def secure_ipc_socket(address):
    if not is_windows():Path(address).chmod(0o600)


def lock_file(stream,unlock=False):
    stream.seek(0)
    if is_windows():
        import msvcrt
        msvcrt.locking(stream.fileno(),msvcrt.LK_UNLCK if unlock else msvcrt.LK_NBLCK,1)
    else:
        import fcntl
        fcntl.flock(stream.fileno(),fcntl.LOCK_UN if unlock else fcntl.LOCK_EX|fcntl.LOCK_NB)
