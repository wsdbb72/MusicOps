"""Native macOS folder bundle; GUI and private worker share dependencies.

Build on each architecture with packaging/build_macos.py. No Developer ID
identity is configured: PyInstaller applies ad-hoc signatures only.
"""
from pathlib import Path
import os,platform,tomllib
from PyInstaller.utils.hooks import collect_data_files,collect_submodules

root=Path(SPECPATH).parent
version=tomllib.loads((root/'pyproject.toml').read_text('utf-8'))['project']['version']
architecture=os.environ.get('MUSICOPS_MAC_ARCH',platform.machine())
icon=Path(os.environ['MUSICOPS_MAC_ICON'])
if architecture not in ('arm64',):raise ValueError('Unsupported macOS architecture')
shared=collect_submodules('musicops')
data=collect_data_files('PySide6',includes=['Qt/plugins/imageformats/*','Qt/translations/qtbase_zh_CN.qm'])
data += [(str(root/'src/musicops/assets'),'musicops/assets')]
data += collect_data_files('opencc')
data += [(os.environ['MUSICOPS_MAC_LEGAL'],'musicops/legal')]

gui=Analysis([str(root/'packaging/gui_entry.py')],pathex=[str(root/'src')],binaries=[],datas=data,hiddenimports=shared,hookspath=[],runtime_hooks=[],excludes=[],noarchive=False)
worker=Analysis([str(root/'packaging/worker_entry.py')],pathex=[str(root/'src')],binaries=[],datas=[],hiddenimports=shared,hookspath=[],runtime_hooks=[],excludes=[],noarchive=False)
gui_pyz=PYZ(gui.pure)
worker_pyz=PYZ(worker.pure)
gui_exe=EXE(gui_pyz,gui.scripts,[],exclude_binaries=True,name='MusicOps',debug=False,strip=False,upx=False,console=False,argv_emulation=False,target_arch=architecture,codesign_identity=None)
worker_exe=EXE(worker_pyz,worker.scripts,[],exclude_binaries=True,name='MusicOps-worker',debug=False,strip=False,upx=False,console=True,target_arch=architecture,codesign_identity=None)
folder=COLLECT(gui_exe,worker_exe,gui.binaries,gui.datas,worker.binaries,worker.datas,strip=False,upx=False,name='MusicOps-macOS')
app=BUNDLE(folder,name='MusicOps.app',icon=str(icon),bundle_identifier='com.wsdbb72.MusicOps',version=version,codesign_identity=None,info_plist={
    'CFBundleName':'MusicOps','CFBundleDisplayName':'MusicOps','CFBundleShortVersionString':version,'CFBundleVersion':version,
    'LSMinimumSystemVersion':'15.0','LSBackgroundOnly':False,'NSHighResolutionCapable':True,'NSPrincipalClass':'NSApplication',
    'NSAppleScriptEnabled':False,'LSApplicationCategoryType':'public.app-category.music',
    'NSHumanReadableCopyright':'Copyright © MusicOps contributors. See bundled LICENSE.',
})
