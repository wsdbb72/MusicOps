"""The installer language and its redistribution grant are source dependencies."""
import hashlib
from pathlib import Path
import subprocess
import sys


ROOT=Path(__file__).resolve().parents[1]
LANGUAGE=ROOT/'packaging/Languages'


def test_windows_installer_uses_vendored_language_from_its_source_directory():
    script=(ROOT/'packaging/MusicOps.iss').read_text('utf-8-sig')
    assert 'MessagesFile: "{#SourcePath}\\Languages\\ChineseSimplified.isl"' in script
    assert 'compiler:Languages\\ChineseSimplified.isl' not in script
    assert (LANGUAGE/'ChineseSimplified.isl').is_file()


def test_vendored_translation_is_pinned_and_has_its_complete_grant():
    text=(LANGUAGE/'ChineseSimplified.isl').read_text('utf-8-sig')
    assert hashlib.sha256(text.replace('\r\n','\n').encode('utf-8')).hexdigest()=='bf0751fa176569c6faa2f6e17ed2734617bef325d5cc06eae030fdd0258ee778'
    license=(LANGUAGE/'LICENSE').read_text('utf-8')
    assert 'MIT License' in license and 'Copyright (c) 2019 - 2020 kirakira' in license
    assert 'THE SOFTWARE IS PROVIDED "AS IS"' in license
    reference=(LANGUAGE/'README.md').read_text('utf-8')
    assert '1ff90acc4ed4aee82b1cda43253243deee3daed4' in reference


def test_runtime_license_collection_keeps_installer_translation_grant(tmp_path):
    subprocess.run([sys.executable,str(ROOT/'packaging/collect_licenses.py'),str(tmp_path)],
                   check=True,capture_output=True,encoding='utf-8',errors='replace')
    collected=tmp_path/'licenses/Inno-Setup-Translation'
    for name in ('LICENSE','README.md'):
        assert (collected/name).read_bytes()==(LANGUAGE/name).read_bytes()


def test_source_release_requires_the_vendored_language_license():
    workflow=(ROOT/'.github/workflows/desktop-release.yml').read_text('utf-8')
    assert "'/packaging/Languages/LICENSE'" in workflow
    notices=(ROOT/'packaging/THIRD-PARTY-NOTICES.md').read_text('utf-8')
    assert 'licenses/Inno-Setup-Translation/' in notices
