"""Platform boundaries: portable Windows behavior and private macOS runtime.

Real macOS tests require an explicitly supplied, unlocked CI test keychain.
They never create or delete the user's login keychain.
"""
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import stat
import subprocess
import sys
from types import SimpleNamespace

import pytest

from musicops import platforms,startup
from musicops.clients import vault
from musicops.worker.client import WorkerClient,pipe_address
from musicops.worker.lock import WriterLock


def test_windows_pipe_address_contract_is_unchanged(tmp_path,monkeypatch):
    monkeypatch.setattr(platforms,'runtime_platform',lambda:'win32')
    expected=hashlib.sha256((str(tmp_path.resolve()).lower()+os.environ.get('USERNAME','')).encode()).hexdigest()[:24]
    assert pipe_address(tmp_path)==r'\\.\pipe\MusicOps-'+expected
    assert platforms.ipc_family()=='AF_PIPE'


def test_macos_profile_uses_application_support_and_ignores_app_neighbor(tmp_path,monkeypatch):
    monkeypatch.setattr(platforms,'runtime_platform',lambda:'darwin')
    app=tmp_path/'MusicOps.app/Contents/MacOS';(app/'profile').mkdir(parents=True)
    assert startup.default_profile(app/'MusicOps',tmp_path/'support')==tmp_path/'support/MusicOps/profile'
    monkeypatch.setattr(Path,'home',classmethod(lambda cls:tmp_path/'home'))
    assert platforms.user_data_root()==tmp_path/'home/Library/Application Support'


def test_macos_profile_does_not_follow_a_remembered_profile_inside_app(tmp_path,monkeypatch):
    monkeypatch.setattr(startup,'is_macos',lambda:True)
    app_profile=tmp_path/'MusicOps.app/profile';(app_profile/'state').mkdir(parents=True);(app_profile/'state/musicops.sqlite').touch()
    data=tmp_path/'data/MusicOps';data.mkdir(parents=True);(data/'active-profile.json').write_text(json.dumps({'profile':str(app_profile)}))
    assert startup.default_profile(tmp_path/'MusicOps.app/Contents/MacOS/MusicOps',tmp_path/'data')==data/'profile'


def test_macos_authenticated_envelope_has_random_nonce_and_rejects_tampering(monkeypatch):
    monkeypatch.setattr(vault,'is_macos',lambda:True)
    calls=[]
    def master(create):calls.append(create);return b'x'*32
    monkeypatch.setattr(vault,'_mac_master_key',master)
    clear=b'fixture credential value'
    one=vault.protect(clear);two=vault.protect(clear)
    assert one!=two and clear not in one and one.startswith(vault._MAC_MAGIC)
    assert vault.protect(one,True)==clear and calls[-1] is False
    with pytest.raises(RuntimeError,match='解密失败'):vault.protect(one[:-1]+bytes([one[-1]^1]),True)
    with pytest.raises(RuntimeError,match='文件格式'):vault.protect(b'not a macOS vault',True)


def test_keychain_failure_has_no_plaintext_fallback(monkeypatch):
    monkeypatch.setattr(vault,'is_macos',lambda:True)
    def unavailable(_):raise RuntimeError('Keychain unavailable')
    monkeypatch.setattr(vault,'_mac_master_key',unavailable)
    with pytest.raises(RuntimeError,match='Keychain unavailable'):vault.protect(b'fixture')


def test_keychain_creation_rereads_winning_key_and_never_overwrites(monkeypatch):
    monkeypatch.delenv('MUSICOPS_KEYCHAIN_PATH',raising=False);monkeypatch.setattr(vault,'_MAC_KEYS',{})
    stored=[];commands=[]
    def run(command,**kwargs):
        commands.append(command)
        if command[1]=='find-generic-password':return SimpleNamespace(returncode=0 if stored else 44,stdout=stored[0] if stored else '')
        assert command[1]=='add-generic-password' and '-U' not in command
        stored.append(command[command.index('-w')+1]);return SimpleNamespace(returncode=0,stdout='')
    monkeypatch.setattr(subprocess,'run',run)
    first=vault._mac_master_key(True)
    assert len(first)==32 and len(commands)==3
    assert vault._mac_master_key(False)==first and len(commands)==3


def test_fcntl_lock_path_does_not_import_windows_only_module(tmp_path,monkeypatch):
    monkeypatch.setattr(platforms,'is_windows',lambda:False);operations=[]
    monkeypatch.setitem(sys.modules,'fcntl',SimpleNamespace(LOCK_UN=8,LOCK_EX=2,LOCK_NB=4,flock=lambda fd,flags:operations.append(flags)))
    with (tmp_path/'lock').open('w+b') as stream:
        platforms.lock_file(stream);platforms.lock_file(stream,unlock=True)
    assert operations==[6,8]


def test_macos_frozen_worker_is_sibling_without_exe(tmp_path,monkeypatch):
    import musicops.worker.client as module
    monkeypatch.setattr(module,'is_windows',lambda:False)
    monkeypatch.setattr(sys,'frozen',True,raising=False);monkeypatch.setattr(sys,'executable',str(tmp_path/'MusicOps.app/Contents/MacOS/MusicOps'))
    client=WorkerClient(tmp_path/'profile');answers=[FileNotFoundError(),{'attached':True}]
    def call(*args,**kwargs):
        result=answers.pop(0)
        if isinstance(result,Exception):raise result
        return result
    monkeypatch.setattr(client,'call',call);commands=[]
    monkeypatch.setattr(module.subprocess,'Popen',lambda command,**kwargs:commands.append((command,kwargs)))
    monkeypatch.setattr(module.time,'sleep',lambda _:None)
    assert client.ensure_running()=={'attached':True}
    assert commands[0][0][0]==str(tmp_path/'MusicOps.app/Contents/MacOS/MusicOps-worker')
    assert commands[0][1]['creationflags']==0


mac_only=pytest.mark.skipif(sys.platform!='darwin',reason='Requires real macOS Keychain/AF_UNIX runner')


def _ci_keychain():
    if not os.environ.get('MUSICOPS_KEYCHAIN_PATH'):pytest.skip('Explicit temporary CI keychain required; never touch login keychain in tests')


@mac_only
def test_macos_real_keychain_ciphertext_decrypts_in_fresh_process(tmp_path):
    _ci_keychain();value=b'musicops-fixture-only-keychain-check';encrypted=vault.protect(value)
    assert value not in encrypted
    source="import base64,sys;from musicops.clients.vault import protect;assert protect(base64.b64decode(sys.stdin.buffer.read()),True)==b'musicops-fixture-only-keychain-check';print('verified')"
    env=os.environ.copy();env['PYTHONPATH']=str(Path(__file__).resolve().parents[1]/'src')
    checked=subprocess.run([sys.executable,'-c',source],input=base64.b64encode(encrypted),capture_output=True,env=env,timeout=30)
    assert checked.returncode==0 and checked.stdout.strip()==b'verified'
    local=vault.Vault(tmp_path/'profile');local.set('vault://fixture/mac','fixture value','official.invalid')
    assert local.path.name=='secrets.keychain' and b'fixture value' not in local.path.read_bytes()
    assert local.get('vault://fixture/mac','official.invalid')=='fixture value'
    with pytest.raises(RuntimeError,match='CREDENTIAL_HOST_MISMATCH'):local.get('vault://fixture/mac','third-party.invalid')
    assert stat.S_IMODE(local.path.stat().st_mode)==0o600


@mac_only
def test_macos_real_unix_worker_authentication_and_private_permissions(tmp_path):
    _ci_keychain();base=tmp_path/('long-profile-'*10)/'profile';client=WorkerClient(base)
    try:
        assert client.ensure_running()['config']['version']==1
        address=Path(pipe_address(base))
        assert len(os.fsencode(address))<100 and stat.S_IMODE(address.parent.stat().st_mode)==0o700
        assert stat.S_IMODE(address.stat().st_mode)==0o600
        from multiprocessing import AuthenticationError
        from multiprocessing.connection import Client
        with pytest.raises(AuthenticationError):Client(str(address),family='AF_UNIX',authkey=b'wrong-fixture-authentication-key')
        assert client.call('get_status',{'compact':True})['schema_version']==1
        client.call('set_secret',{'ref':'vault://fixture/ipc','value':'fixture-ipc-value','host':'official.invalid'})
        assert any(r['ref']=='vault://fixture/ipc' for r in client.call('get_status',{'compact':True})['secret_refs'])
    finally:
        try:client.call('shutdown_worker')
        except OSError:pass


def test_native_worker_keeps_serving_when_an_authenticated_client_is_idle(tmp_path):
    """An idle GUI connection used to block every Windows named-pipe client."""
    if platforms.is_macos():_ci_keychain()
    from multiprocessing.connection import Client
    from musicops.worker.client import auth_key
    from concurrent.futures import ThreadPoolExecutor
    import time
    base=tmp_path/'concurrent-native';client=WorkerClient(base,timeout=5);idle=None
    try:
        client.ensure_running()
        idle=Client(pipe_address(base),family=platforms.ipc_family(),authkey=auth_key(base))
        start=time.monotonic()
        with ThreadPoolExecutor(max_workers=6) as pool:
            results=list(pool.map(lambda _:WorkerClient(base,timeout=5).call('get_status',{'compact':True}),range(6)))
        assert len(results)==6 and all(r['schema_version']==1 for r in results)
        assert time.monotonic()-start<5
    finally:
        if idle:idle.close()
        try:client.call('shutdown_worker')
        except OSError:pass


@mac_only
def test_macos_stale_socket_cleanup_refuses_regular_files(tmp_path):
    address=Path(pipe_address(tmp_path/'profile'))
    sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
    try:sock.bind(str(address))
    finally:sock.close()
    platforms.prepare_ipc_socket(tmp_path/'profile');assert not address.exists()
    address.write_text('fixture regular file')
    try:
        with pytest.raises(RuntimeError,match='拒绝替换'):platforms.prepare_ipc_socket(tmp_path/'profile')
        assert address.read_text()=='fixture regular file'
    finally:address.unlink()


@mac_only
def test_macos_writer_lock_excludes_a_second_process(tmp_path):
    lock=WriterLock(tmp_path).acquire()
    source="import sys;from musicops.worker.lock import WriterLock\ntry: WriterLock(sys.argv[1]).acquire()\nexcept RuntimeError: print('locked')"
    env=os.environ.copy();env['PYTHONPATH']=str(Path(__file__).resolve().parents[1]/'src')
    try:
        checked=subprocess.run([sys.executable,'-c',source,str(tmp_path)],capture_output=True,env=env,timeout=15)
        assert checked.returncode==0 and checked.stdout.strip()==b'locked'
    finally:lock.release()
