import importlib.util,plistlib
from pathlib import Path
from xml.etree import ElementTree as ET
import pytest

path=Path(__file__).resolve().parents[1]/'packaging/macos_installer.py'
spec=importlib.util.spec_from_file_location('macos_installer_materials',path);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)

@pytest.mark.parametrize('arch',['arm64','x86_64'])
def test_installer_is_one_system_app_payload_with_preserved_user_data(tmp_path,arch):
    directory=module.materials(tmp_path,'0.4.1',arch);root=ET.parse(directory/'Distribution.xml').getroot()
    assert root.find('options').get('hostArchitectures')==arch
    assert root.find('domains').attrib=={'enable_anywhere':'false','enable_currentUserHome':'false','enable_localSystem':'true'}
    assert root.find('./volume-check/allowed-os-versions/os-version').get('min')=='15.0'
    assert root.find('./pkg-ref/must-close/app').get('id')=='com.wsdbb72.MusicOps'
    components=plistlib.loads((directory/'components.plist').read_bytes())
    assert len(components)==1 and components[0]['RootRelativeBundlePath']=='MusicOps.app'
    assert not components[0]['BundleIsRelocatable'] and components[0]['BundleOverwriteAction']=='upgrade'
    script=(directory/'scripts/preinstall').read_text('utf-8')
    assert '/usr/bin/pgrep -x' in script and 'kill ' not in script and 'rm ' not in script
    assert all(token not in script for token in ('Application Support','Keychain','security ','installer-preservation'))
    assert 'sudo' not in script and 'MusicOps-worker' in script
    assert all((directory/'resources'/file).is_file() for file in ('Welcome.html','ReadMe.html','License.html'))

@pytest.mark.parametrize('version,arch',[('0.4.1;exit','arm64'),('../0.4.1','arm64'),('0.4.1','other'),('','x86_64')])
def test_installer_rejects_invalid_build_identity(tmp_path,version,arch):
    with pytest.raises(ValueError):module.materials(tmp_path,version,arch)
    assert not list(tmp_path.iterdir())
