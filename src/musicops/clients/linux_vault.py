"""Linux AES-GCM envelope; persist the private key separately from the profile."""
import os
import stat
from pathlib import Path
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from musicops.platforms import private_directory, lock_file

MAGIC=b'MusicOpsLinux1\0'

def master_key(create):
    raw=os.environ.get('MUSICOPS_VAULT_KEY_FILE','')
    if not raw:raise RuntimeError('Linux 需要设置 MUSICOPS_VAULT_KEY_FILE 并持久保存该密钥文件')
    path=Path(raw)
    if not path.is_absolute():raise RuntimeError('Linux 凭据密钥路径必须是绝对路径')
    private_directory(path.parent)
    with (path.parent/(path.name+'.lock')).open('a+b') as guard:
        os.chmod(guard.name,0o600)
        import fcntl
        fcntl.flock(guard.fileno(),fcntl.LOCK_EX)
        try:
            if not path.exists() and create:
                fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
                with os.fdopen(fd,'wb') as stream:stream.write(os.urandom(32));stream.flush();os.fsync(stream.fileno())
            try:
                fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
                with os.fdopen(fd,'rb') as stream:
                    info=os.fstat(stream.fileno())
                    if not stat.S_ISREG(info.st_mode) or info.st_uid!=os.getuid() or info.st_mode&0o077:raise RuntimeError('Linux 凭据密钥必须属于当前用户且权限为 600')
                    key=stream.read(33)
            except FileNotFoundError:raise RuntimeError('Linux 凭据主密钥缺失；请恢复原密钥，不能生成新密钥解密旧凭据') from None
            if len(key)!=32:raise RuntimeError('Linux 凭据主密钥损坏')
            return key
        finally:lock_file(guard,unlock=True)

def protect_linux(data,decrypt=False):
    if decrypt:
        if not data.startswith(MAGIC) or len(data)<len(MAGIC)+28:raise RuntimeError('凭据文件不属于 Linux 档案；请在新设备重新填写凭据')
        try:return AESGCM(master_key(False)).decrypt(data[len(MAGIC):len(MAGIC)+12],data[len(MAGIC)+12:],MAGIC)
        except RuntimeError:raise
        except Exception:raise RuntimeError('Linux 凭据解密失败；密钥不符或文件已损坏') from None
    nonce=os.urandom(12)
    return MAGIC+nonce+AESGCM(master_key(True)).encrypt(nonce,data,MAGIC)
