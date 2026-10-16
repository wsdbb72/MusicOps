"""Exercise real Inno installation/upgrade behavior using an isolated test AppId.

The tiny payload is a synthetic .NET executable, never a user's MusicOps process.
Set INNO_ISCC to an Inno Setup 6.5+ compiler to run these Windows-only checks.
"""
from __future__ import annotations

import hashlib
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time
import uuid

import pytest

pytestmark = pytest.mark.skipif(sys.platform != 'win32', reason='Native Windows installer acceptance')
ROOT = Path(__file__).resolve().parents[1]


def run_process(arguments, *, timeout=90):
    return subprocess.run(list(map(str, arguments)), capture_output=True, text=True, encoding='utf-8', errors='replace',
                          creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0), timeout=timeout)


@pytest.fixture(scope='module')
def tiny_bundle(tmp_path_factory):
    compiler = os.environ.get('INNO_ISCC') or shutil.which('ISCC')
    csc = Path(os.environ.get('WINDIR', 'C:/Windows'))/'Microsoft.NET/Framework64/v4.0.30319/csc.exe'
    if not compiler or not csc.is_file():
        pytest.skip('Inno Setup 7 and the Windows .NET C# compiler are required')
    directory = tmp_path_factory.mktemp('installer-fixture')
    source = directory/'Synthetic.cs'
    source.write_text('using System.Threading; class Synthetic { static void Main() { Thread.Sleep(60000); } }', 'utf-8')
    bundle = directory/'bundle'
    (bundle/'_internal/musicops/assets').mkdir(parents=True)
    (bundle/'licenses').mkdir()
    result = run_process([csc, '/nologo', '/target:winexe', '/out:'+str(bundle/'MusicOps.exe'), source])
    assert result.returncode == 0, result.stdout+result.stderr
    shutil.copyfile(bundle/'MusicOps.exe', bundle/'MusicOps-worker.exe')
    shutil.copyfile(ROOT/'src/musicops/assets/musicops.ico', bundle/'_internal/musicops/assets/musicops.ico')
    for name in ('LICENSE', 'THIRD-PARTY-NOTICES.md', '启动说明.txt', 'licenses/fixture.txt', '_internal/payload.txt'):
        (bundle/name).write_text('new synthetic payload', 'utf-8')
    return Path(compiler), bundle


class Installation:
    def __init__(self, installer, target, identity):
        self.installer, self.target, self.identity = installer, target, identity
        self.counter = 0
        self.keypath = 'Software\\Microsoft\\Windows\\CurrentVersion\\Uninstall\\'+identity+'_is1'

    def run(self, *, directory=None):
        self.counter += 1
        log = self.installer.parent/('install-'+str(self.counter)+'.log')
        arguments = [self.installer, '/SP-', '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART',
                     '/NOICONS', '/TASKS=', '/LOG='+str(log)]
        if directory is not None:
            arguments.append('/DIR='+str(directory))
        result = run_process(arguments)
        raw = log.read_bytes() if log.exists() else b''
        encoding = 'utf-16' if raw.startswith((b'\xff\xfe', b'\xfe\xff')) else 'utf-8-sig'
        return result.returncode, raw.decode(encoding, errors='replace')

    def registry(self, name, value=None):
        import winreg
        with winreg.CreateKeyEx(winreg.HKEY_CURRENT_USER, self.keypath, access=winreg.KEY_ALL_ACCESS|winreg.KEY_WOW64_64KEY) as key:
            if value is not None:
                winreg.SetValueEx(key, name, 0, winreg.REG_SZ, value)
            return winreg.QueryValueEx(key, name)[0]


@pytest.fixture
def installation(tiny_bundle, tmp_path):
    import winreg
    compiler, bundle = tiny_bundle
    # Inno hashes long application IDs into uninstall-key names. Keep this short
    # so the test exercises the same literal registration scheme as production.
    identity = 'MusicOps.Test.'+uuid.uuid4().hex[:12]
    result = run_process([compiler, '/Q', '/DAppIdentity='+identity, '/DBundleDir='+str(bundle),
                          '/O'+str(tmp_path/'installer'), ROOT/'packaging/MusicOps.iss'])
    assert result.returncode == 0, result.stdout+result.stderr
    instance = Installation(next((tmp_path/'installer').glob('*-Setup.exe')), tmp_path/'app', identity)
    try:
        yield instance
    finally:
        uninstaller = instance.target/'unins000.exe'
        if uninstaller.is_file():
            result = run_process([uninstaller, '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART'])
            assert result.returncode == 0, result.stdout+result.stderr
        # This key is a fresh UUID test identity; never touch MusicOps.Desktop.
        try:
            with winreg.OpenKey(winreg.HKEY_CURRENT_USER, instance.keypath, 0,
                                winreg.KEY_ALL_ACCESS|winreg.KEY_WOW64_64KEY) as key:
                winreg.DeleteKey(key, '')
        except FileNotFoundError:
            pass


def preserve_user_data(target):
    paths = {'profile/settings.json': b'{"synthetic":"existing settings"}',
             'profile/vault.bin': os.urandom(64),
             'profile/tasks.sqlite': b'synthetic task database must survive',
             'tools/custom-tool.exe': b'configured media tool must survive'}
    for relative, value in paths.items():
        path = target/relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value)
    return paths


def assert_preserved(target, paths):
    assert all((target/relative).read_bytes() == value for relative, value in paths.items())


def test_real_fresh_install_then_detected_update_preserves_data_and_location(installation):
    app = installation
    code, log = app.run(directory=app.target)
    assert code == 0 and 'MusicOps install mode=fresh' in log
    saved = preserve_user_data(app.target)
    (app.target/'_internal/payload.txt').write_text('obsolete payload', 'utf-8')
    app.registry('DisplayVersion', '0.3.0')
    code, log = app.run()  # No /DIR: detection must find the first install.
    assert code == 0 and 'MusicOps install mode=upgrade; detected version=0.3.0' in log
    assert Path(app.registry('InstallLocation')).resolve() == app.target.resolve()
    assert (app.target/'_internal/payload.txt').read_text('utf-8') == 'new synthetic payload'
    assert_preserved(app.target, saved)


def test_registered_update_refuses_second_directory(installation):
    app = installation
    assert app.run(directory=app.target)[0] == 0
    saved = preserve_user_data(app.target)
    other = app.target.with_name('duplicate-install')
    assert app.run(directory=other)[0] != 0
    assert not other.exists()
    assert_preserved(app.target, saved)


def test_downgrade_is_refused_before_replacing_files(installation):
    app = installation
    assert app.run(directory=app.target)[0] == 0
    app.registry('DisplayVersion', '99.0.0')
    sentinel = app.target/'_internal/payload.txt'
    sentinel.write_text('newer application payload', 'utf-8')
    assert app.run()[0] != 0
    assert sentinel.read_text('utf-8') == 'newer application payload'
    assert app.registry('DisplayVersion') == '99.0.0'


def test_running_worker_blocks_update_without_terminating_it(installation):
    app = installation
    assert app.run(directory=app.target)[0] == 0
    saved = preserve_user_data(app.target)
    exe = app.target/'MusicOps-worker.exe'
    before = hashlib.sha256(exe.read_bytes()).digest()
    worker = subprocess.Popen([str(exe)], creationflags=subprocess.CREATE_NO_WINDOW)
    try:
        time.sleep(0.2)
        assert worker.poll() is None
        code, log = app.run()
        assert code != 0 and 'update blocked by busy or inaccessible executable' in log
        assert worker.poll() is None
        assert hashlib.sha256(exe.read_bytes()).digest() == before
        assert_preserved(app.target, saved)
    finally:
        worker.terminate()  # Only the test's synthetic sleeping fixture.
        worker.wait(timeout=10)
    assert app.run()[0] == 0
    assert_preserved(app.target, saved)


def test_portable_directory_is_detected_as_update(installation, tiny_bundle):
    app = installation
    shutil.copytree(tiny_bundle[1], app.target)
    saved = preserve_user_data(app.target)
    code, log = app.run(directory=app.target)
    assert code == 0 and 'MusicOps install mode=upgrade' in log
    assert Path(app.registry('InstallLocation')).resolve() == app.target.resolve()
    assert_preserved(app.target, saved)


def test_stale_install_registration_does_not_prevent_fresh_install(installation):
    app = installation
    app.registry('InstallLocation', str(app.target.with_name('missing-old-install')))
    app.registry('DisplayVersion', '99.0.0')
    code, log = app.run(directory=app.target)
    assert code == 0 and 'MusicOps install mode=fresh' in log
    assert Path(app.registry('InstallLocation')).resolve() == app.target.resolve()
