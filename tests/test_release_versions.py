"""Catch release filename/version drift before a full installer build."""
import re
import tomllib
import os
import runpy
from pathlib import Path
import pytest

from musicops import __version__
from musicops.providers.catalog import USER_AGENT


def test_windows_installer_version_matches_application():
    root = Path(__file__).resolve().parents[1]
    version = tomllib.loads((root / 'pyproject.toml').read_text('utf-8'))['project']['version']
    installer = (root / 'packaging/MusicOps.iss').read_text('utf-8')
    assert __version__ == version
    assert re.search(r'^#define MusicOpsVersion "([^"]+)"', installer, re.M)[1] == version
    assert 'VersionInfoVersion={#MusicOpsVersion}.0' in installer
    assert 'OutputBaseFilename=MusicOps-{#MusicOpsVersion}-Setup' in installer
    assert USER_AGENT.startswith('MusicOps/' + version + ' ')


@pytest.mark.skipif(os.name != 'nt', reason='Windows PE resource encoding')
@pytest.mark.parametrize('filename', ['MusicOps.exe', 'MusicOps-worker.exe'])
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
