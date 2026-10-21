import importlib.util
import os
from pathlib import Path
import sys
import pytest
from musicops.clients import linux_vault
from musicops.config.schema import default_config

def test_container_paths_only_apply_to_live_container(tmp_path,monkeypatch):
    monkeypatch.setenv('MUSICOPS_CONTAINER','1')
    cfg=default_config(tmp_path,offline=False)
    assert cfg['paths']['library']=='/media/music'
    assert cfg['paths']['source']=='/media/MusicOps/downloads'
    assert default_config(tmp_path,offline=True)['paths']['library']!='/media/music'
    monkeypatch.delenv('MUSICOPS_CONTAINER')
    assert default_config(tmp_path,offline=False)['paths']['library']!='/media/music'

def test_linux_envelope_authentication(monkeypatch):
    key=b'x'*32
    monkeypatch.setattr(linux_vault,'master_key',lambda create:key)
    clear=b'synthetic-credential-only'
    encrypted=linux_vault.protect_linux(clear)
    assert clear not in encrypted and linux_vault.protect_linux(encrypted,True)==clear
    assert encrypted!=linux_vault.protect_linux(clear)
    with pytest.raises(RuntimeError,match='解密失败'):linux_vault.protect_linux(encrypted[:-1]+bytes([encrypted[-1]^1]),True)
    with pytest.raises(RuntimeError,match='不属于 Linux'):linux_vault.protect_linux(b'windows-profile',True)

@pytest.mark.skipif(sys.platform!='linux',reason='real Linux file modes, flock and ownership')
def test_linux_master_key_persistence_and_missing_key(tmp_path,monkeypatch):
    path=tmp_path/'keys'/'vault.key';monkeypatch.setenv('MUSICOPS_VAULT_KEY_FILE',str(path))
    value=linux_vault.protect_linux(b'synthetic')
    assert path.stat().st_mode&0o777==0o600
    assert linux_vault.protect_linux(value,True)==b'synthetic'
    path.chmod(0o644)
    with pytest.raises(RuntimeError,match='600'):linux_vault.protect_linux(value,True)
    path.chmod(0o600);path.unlink()
    with pytest.raises(RuntimeError,match='缺失'):linux_vault.protect_linux(value,True)
    assert not path.exists()

def test_web_password_persists_and_custom_file_does_not_generate(tmp_path,monkeypatch):
    spec=importlib.util.spec_from_file_location('container_entry',Path(__file__).resolve().parents[1]/'docker/entrypoint.py')
    entry=importlib.util.module_from_spec(spec);spec.loader.exec_module(entry)
    monkeypatch.delenv('MUSICOPS_WEB_PASSWORD_FILE',raising=False)
    one=entry.web_password(tmp_path)
    assert len(one)>=24 and entry.web_password(tmp_path)==one
    monkeypatch.setenv('MUSICOPS_WEB_PASSWORD_FILE',str(tmp_path/'missing'))
    with pytest.raises(FileNotFoundError):entry.web_password(tmp_path)

def test_container_engine_location(monkeypatch,tmp_path):
    from musicops.clients.managed_services import engine_root
    monkeypatch.setenv('MUSICOPS_ENGINE_ROOT',str(tmp_path))
    assert engine_root()==tmp_path
