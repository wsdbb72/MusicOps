"""Catch release filename/version drift before a full installer build.

The version is read by parsing the source files, never by importing the
package: the Go release flow does not install the Python app, so the test
must not depend on an installed ``musicops``.
"""
import re
import tomllib
import os
import runpy
from pathlib import Path
import pytest


def _read_version(root: Path) -> str:
    version = tomllib.loads((root / 'pyproject.toml').read_text('utf-8'))['project']['version']
    init = (root / 'src/musicops/__init__.py').read_text('utf-8')
    declared = re.search(r'^__version__ = "([^"]+)"', init, re.M)
    assert declared is not None, 'src/musicops/__init__.py declares no __version__'
    assert declared[1] == version, '__init__.py version does not match pyproject.toml'
    return version


def test_windows_installer_version_matches_application():
    root = Path(__file__).resolve().parents[1]
    version = _read_version(root)
    installer = (root / 'packaging/MusicOps.iss').read_text('utf-8')
    assert re.search(r'^#define MusicOpsVersion "([^"]+)"', installer, re.M)[1] == version
    assert 'VersionInfoVersion={#MusicOpsVersion}.0' in installer
    assert 'OutputBaseFilename=MusicOps-{#MusicOpsVersion}-Setup' in installer
    catalog = (root / 'src/musicops/providers/catalog.py').read_text('utf-8')
    assert "USER_AGENT = f'MusicOps/{__version__} " in catalog


def test_go_installer_takes_version_from_command_line():
    """The shipping Go installer must never hardcode a release version.

    MusicOps-go.iss carries only the dev fallback ``0.0.0-dev``; the real
    version arrives as /DAppVersion= from the release workflow (and from
    packaging/build_windows_go.ps1). It must also ship no Python worker
    and no _internal runtime.
    """
    root = Path(__file__).resolve().parents[1]
    installer = (root / 'packaging/MusicOps-go.iss').read_text('utf-8')
    assert re.search(r'^\s*#define AppVersion "([^"]+)"', installer, re.M)[1] == '0.0.0-dev'
    assert 'VersionInfoVersion={#AppVersion}.0' in installer
    assert 'OutputBaseFilename=MusicOps-{#AppVersion}-Setup' in installer
    assert 'MusicOpsVersion' not in installer
    # The [Files] section must ship no Python worker and no _internal
    # runtime. (The header comment and the upgrade/busy-check code may
    # still name them: legacy Python installs upgrade in place.)
    files = installer.split('[Files]', 1)[1].split('[Icons]', 1)[0]
    assert 'MusicOps-worker.exe' not in files
    assert '_internal' not in files
    assert 'musicops-core.exe' in files


@pytest.mark.skipif(os.name != 'nt', reason='Windows PE resource encoding')
# The Go build embeds the identical values via goversioninfo
# (packaging/build_windows_go.ps1); the release workflow verifies the real
# binaries' FileVersion after ISCC. This test keeps the resource content
# (company, description, version strings, filename) from drifting.
@pytest.mark.parametrize('filename', ['MusicOps.exe', 'musicops-core.exe'])
def test_windows_pe_resource_uses_project_version_and_correct_filename(filename):
    root = Path(__file__).resolve().parents[1]
    build = runpy.run_path(str(root / 'packaging/windows_version.py'))['windows_version']
    version = tomllib.loads((root / 'pyproject.toml').read_text('utf-8'))['project']['version']
    major, minor, patch = map(int, version.split('.'))
    resource = build(root, filename)
    assert resource.ffi.fileVersionMS == major << 16 | minor
    assert resource.ffi.fileVersionLS == patch << 16
    assert resource.ffi.productVersionMS == major << 16 | minor
    assert resource.ffi.productVersionLS == patch << 16
    encoded = resource.toRaw()
    for text in (version, version + '.0', filename):
        assert (text + '\0').encode('utf-16le') in encoded
    assert not (root / 'packaging/windows-version.txt').exists()
