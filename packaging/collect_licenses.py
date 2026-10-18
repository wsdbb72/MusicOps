"""Preserve bundled Python and installed dependency license texts."""
import importlib.metadata as metadata
import shutil
import sys
from pathlib import Path
out=Path(sys.argv[1])/'licenses';out.mkdir(parents=True,exist_ok=True)
playlist_license=Path(__file__).resolve().parents[1]/'docs/references/PlaylistOut-LICENSE.txt'
if playlist_license.is_file():
    (out/'PlaylistOut').mkdir(exist_ok=True);shutil.copy2(playlist_license,out/'PlaylistOut/LICENSE.txt')
installer_language=Path(__file__).resolve().parent/'Languages'
language_notices=out/'Inno-Setup-Translation';language_notices.mkdir(exist_ok=True)
for notice in ('LICENSE','README.md'):
    shutil.copy2(installer_language/notice,language_notices/notice)
python_license=Path(sys.base_prefix)/'LICENSE.txt'
if python_license.is_file():
    python_target=out/'Python';python_target.mkdir(exist_ok=True)
    shutil.copy2(python_license,python_target/'LICENSE.txt')
for name in ('PySide6','PySide6_Essentials','PySide6_Addons','shiboken6','mutagen','Pillow','cryptography','httpx','httpcore','certifi','h11','anyio','idna','cffi','pycparser','PyInstaller','qrcode','bottle','waitress','selenium','func-timeout','prometheus-client','requests','urllib3','websockets','packaging','xvfbwrapper','pefile','trio','trio-websocket','sortedcontainers','sniffio','outcome','attrs','PySocks','wsproto'):
    try:dist=metadata.distribution(name)
    except metadata.PackageNotFoundError:continue
    target=out/name;target.mkdir(exist_ok=True)
    (target/'package.txt').write_text(f'{name}=={dist.version}\n'+str(dist.metadata.get('License',''))+'\n'+str(dist.metadata.get('Home-page',''))+'\n',encoding='utf-8')
    for item in dist.files or []:
        candidate=dist.locate_file(item)
        if candidate.is_file() and candidate.suffix.lower() not in ('.py','.pyc','.pyo') and (any(part.lower() in ('license','licenses') for part in Path(item).parts) or candidate.name.lower().startswith(('license','copying','notice','copyright'))):
            dest=target/Path(item).name
            if not dest.exists():shutil.copy2(candidate,dest)
