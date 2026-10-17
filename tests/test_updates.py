import hashlib
import io
import json
from pathlib import Path
import pytest
from musicops.updates import Updater, StagedUpdate, installer_name, launch_installer, trusted_url, verify_staged, LATEST, WINDOWS_HANDOFF


class Response(io.BytesIO):
    pass


def release(body=b'isolated synthetic installer', system='win32', version='0.4.4'):
    name = installer_name(version, system, 'arm64' if system == 'darwin' else 'AMD64')
    return {'tag_name': 'v' + version, 'draft': False, 'prerelease': False,
            'assets': [{'name': name, 'size': len(body), 'digest': 'sha256:' + hashlib.sha256(body).hexdigest(),
                        'browser_download_url': f'https://github.com/wsdbb72/MusicOps/releases/download/v{version}/{name}'}]}


class Opener:
    def __init__(self, data, body): self.data = data; self.body = body; self.calls = []
    def open(self, request, timeout):
        self.calls.append(request.full_url)
        return Response(json.dumps(self.data).encode() if request.full_url == LATEST else self.body)


@pytest.mark.parametrize('system,machine,suffix', [('win32','AMD64','Setup.exe'),('darwin','arm64','macOS-arm64.pkg')])
def test_stage_selects_only_native_installer_verifies_digest_and_reuses_cache(tmp_path, system, machine, suffix):
    body = b'isolated synthetic installer'; data = release(body, system)
    opener = Opener(data, body); progress = []
    updater = Updater(tmp_path/'updates', opener=opener, current='0.4.3', system=system, machine=machine)
    staged = updater.stage_latest(progress.append)
    assert staged.path.name.endswith(suffix) and staged.path.read_bytes() == body
    assert len(opener.calls) == 2 and progress
    assert updater.stage_latest() == staged and len(opener.calls) == 3
    assert not list(tmp_path.rglob('*.part'))


def test_same_or_older_release_does_not_download_or_launch(tmp_path):
    for version in ('0.4.3', '0.4.2'):
        opener = Opener(release(version=version), b'no download')
        assert Updater(tmp_path, opener, '0.4.3').stage_latest() is None and opener.calls == [LATEST]


@pytest.mark.parametrize('change', [
    lambda asset: asset.update(digest='sha256:'+'0'*64),
    lambda asset: asset.update(size=999999999999),
    lambda asset: asset.update(size=True),
    lambda asset: asset.update(browser_download_url='https://evil.example/MusicOps.exe'),
    lambda asset: asset.update(digest=''),
])
def test_untrusted_or_corrupt_asset_never_produces_a_staged_installer(tmp_path, change):
    body = b'fixture'; data = release(body); change(data['assets'][0])
    with pytest.raises(ValueError): Updater(tmp_path, Opener(data, body), '0.4.3', 'win32', 'AMD64').stage_latest()
    assert not list(tmp_path.rglob('*.exe')) and not list(tmp_path.rglob('*.part'))


@pytest.mark.parametrize('url', ['http://github.com/file','https://evil.example/file','https://github.com:444/file','https://me:secret@github.com/file'])
def test_unsafe_redirects_are_rejected(url):
    with pytest.raises(ValueError): trusted_url(url)


def test_intel_mac_has_no_new_installer():
    with pytest.raises(ValueError, match='Apple Silicon'): installer_name('0.4.4', 'darwin', 'x86_64')


def test_installer_handoff_rechecks_hash_and_waits_for_gui_exit(tmp_path):
    body = b'never actually executed'; path = tmp_path/'MusicOps-0.4.4-Setup.exe'; path.write_bytes(body)
    staged = StagedUpdate('0.4.4', path, hashlib.sha256(body).hexdigest(), len(body)); calls = []
    def launcher(*args, **kwargs): calls.append((args, kwargs)); return 'synthetic process'
    assert launch_installer(staged, tmp_path/'installed/MusicOps.exe', 'win32', launcher) == 'synthetic process'
    command = calls[0][0][0]
    assert command[0].endswith('powershell.exe') and '-WindowStyle' in command and command[command.index('-WindowStyle')+1] == 'Hidden'
    helper = tmp_path/'apply-update.ps1'; assert helper.read_text('utf-8-sig') == WINDOWS_HANDOFF
    assert helper.read_text('utf-8-sig').index('Get-Process') < helper.read_text('utf-8-sig').index('Start-Process')
    path.write_bytes(b'tampered')
    with pytest.raises(ValueError): launch_installer(staged, system='win32', launcher=launcher)
    assert len(calls) == 1


def test_arm_mac_opens_native_pkg_and_not_an_application_zip(tmp_path):
    body = b'fixture'; path = tmp_path/'MusicOps-0.4.4-macOS-arm64.pkg'; path.write_bytes(body)
    staged = StagedUpdate('0.4.4', path, hashlib.sha256(body).hexdigest(), len(body)); calls = []
    launch_installer(staged, system='darwin', launcher=lambda *a,**k: calls.append((a,k)))
    assert calls[0][0][0] == ['/usr/bin/open', str(path)]


def test_update_setting_migrates_and_is_editable_without_overwriting_other_settings(tmp_path):
    from musicops.config.schema import default_config, upgrade_config, validate_config
    original = default_config(tmp_path); original.pop('updates')
    migrated = upgrade_config(original, tmp_path); assert migrated['updates']['enabled'] is True
    migrated['updates']['enabled'] = False; validate_config(migrated)
    assert upgrade_config(migrated, tmp_path)['updates']['enabled'] is False


@pytest.mark.skipif(__import__('sys').platform!='win32',reason='Real Windows installer handoff helper')
def test_real_hidden_handoff_waits_for_old_process_then_starts_verified_probe(tmp_path):
    import os, subprocess, sys, time
    compiler=Path(os.environ.get('WINDIR','C:/Windows'))/'Microsoft.NET/Framework64/v4.0.30319/csc.exe'
    if not compiler.is_file():pytest.skip('Windows .NET compiler required')
    source=tmp_path/'Probe.cs';source.write_text('using System; using System.IO; class Probe { static void Main(string[] args) { File.WriteAllText(Path.Combine(AppDomain.CurrentDomain.BaseDirectory,"started.txt"),String.Join("|",args)); } }','utf-8')
    probe=tmp_path/'MusicOps-0.4.4-Setup.exe'
    result=subprocess.run([str(compiler),'/nologo','/target:winexe','/out:'+str(probe),str(source)],capture_output=True,timeout=30)
    assert result.returncode==0,result.stdout
    body=probe.read_bytes();update=StagedUpdate('0.4.4',probe,hashlib.sha256(body).hexdigest(),len(body))
    old=subprocess.Popen([sys.executable,'-c','import time;time.sleep(2)'],creationflags=0x08000000)
    helper=launch_installer(update,tmp_path/'Installed App/MusicOps.exe','win32',wait_pid=old.pid)
    try:
        assert old.poll() is None and not (tmp_path/'started.txt').exists()
        old.wait(10);assert helper.wait(20)==0,(tmp_path/'handoff-error.txt').read_text('utf-8-sig')
        deadline=time.monotonic()+5
        while not (tmp_path/'started.txt').exists() and time.monotonic()<deadline:time.sleep(.05)
        assert (tmp_path/'started.txt').read_text().startswith('/DIR=')
        assert 'Installed App' in (tmp_path/'started.txt').read_text() and not (tmp_path/'handoff-error.txt').exists()
    finally:
        if old.poll() is None:old.terminate();old.wait(5)
        if helper.poll() is None:helper.terminate();helper.wait(5)
