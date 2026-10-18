"""Catch release filename/version drift before a full installer build."""
import re
import tomllib
from pathlib import Path

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
