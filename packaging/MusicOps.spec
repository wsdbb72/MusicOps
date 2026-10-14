# Folder distribution, shared bundled interpreter and Qt image plugins.
from pathlib import Path
import os
from PyInstaller.utils.hooks import collect_data_files
root=Path(SPECPATH).parent
pyside_data=collect_data_files('PySide6',includes=['Qt/plugins/imageformats/*','Qt/translations/qtbase_zh_CN.qm'])
pyside_binaries=[]
pyside_hidden=[]
common_hidden=['musicops.gui.main','musicops.worker.server','musicops.app.service','musicops.providers.lrcapi','musicops.providers.netease','musicops.providers.pt','musicops.providers.workflow','musicops.media.ncm','musicops.media.tags','musicops.media.resources','musicops.media.validation','musicops.library.index','musicops.library.transactions','mutagen.flac','mutagen.wave','mutagen.mp3','mutagen.mp4','mutagen.oggvorbis','mutagen.oggopus','PIL.Image','PIL.JpegImagePlugin','PIL.PngImagePlugin','PIL.WebPImagePlugin']
common_hidden+=['musicops.providers.ai','musicops.providers.pt_readonly','musicops.providers.netease_readonly']
common_hidden+=['musicops.providers.catalog','musicops.clients.roon','musicops.startup']
gui=Analysis([str(root/'packaging'/'gui_entry.py')],pathex=[str(root/'src')],binaries=pyside_binaries,datas=pyside_data,hiddenimports=pyside_hidden+common_hidden,hookspath=[],runtime_hooks=[],excludes=[],noarchive=False)
worker=Analysis([str(root/'packaging'/'worker_entry.py')],pathex=[str(root/'src')],binaries=[],datas=[],hiddenimports=common_hidden,hookspath=[],runtime_hooks=[],excludes=[],noarchive=False)
# Qt 6.12 Windows wheel imports unversioned system ICU APIs. The custom Python
# runtime also contains a versioned ICU 78 with the same filename; automatic
# dependency lookup picked that incompatible DLL. Use Windows' own System32 ICU.
for analysis in (gui,worker):
    analysis.binaries=[item for item in analysis.binaries if Path(item[0]).name.lower() not in ('icuuc.dll','icudt78.dll')]
gui_pyz=PYZ(gui.pure)
worker_pyz=PYZ(worker.pure)
diagnostic=os.environ.get('MUSICOPS_BUILD_DIAGNOSTIC')=='1'
gui_exe=EXE(gui_pyz,gui.scripts,[],exclude_binaries=True,name='MusicOps',debug=False,bootloader_ignore_signals=False,strip=False,upx=False,console=diagnostic,disable_windowed_traceback=False)
worker_exe=EXE(worker_pyz,worker.scripts,[],exclude_binaries=True,name='MusicOps-worker',debug=False,bootloader_ignore_signals=False,strip=False,upx=False,console=diagnostic,disable_windowed_traceback=False)
bundle=COLLECT(gui_exe,worker_exe,gui.binaries,gui.datas,worker.binaries,worker.datas,strip=False,upx=False,name='MusicOps-Windows')
