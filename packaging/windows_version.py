"""Generate both Windows version resources from the release's single version."""
from pathlib import Path
import re
import tomllib


def windows_version(root: Path, filename: str):
    version = tomllib.loads((Path(root) / 'pyproject.toml').read_text('utf-8'))['project']['version']
    if not re.fullmatch(r'\d+\.\d+\.\d+', version):
        raise ValueError('Windows release version must have three numeric components')
    numbers = tuple(map(int, version.split('.'))) + (0,)
    if any(value > 65535 for value in numbers):
        raise ValueError('Windows version components must fit 16 bits')
    from PyInstaller.utils.win32.versioninfo import (
        FixedFileInfo, StringFileInfo, StringStruct, StringTable,
        VarFileInfo, VarStruct, VSVersionInfo,
    )
    return VSVersionInfo(
        ffi=FixedFileInfo(filevers=numbers, prodvers=numbers, mask=0x3f,
                          flags=0, OS=0x40004, fileType=1, subtype=0, date=(0, 0)),
        kids=[StringFileInfo([StringTable('080404B0', [
            StringStruct('CompanyName', 'wsdbb72'),
            StringStruct('FileDescription', 'MusicOps 音乐库管理'),
            StringStruct('FileVersion', version + '.0'),
            StringStruct('InternalName', Path(filename).stem),
            StringStruct('OriginalFilename', filename),
            StringStruct('ProductName', 'MusicOps'),
            StringStruct('ProductVersion', version),
        ])]), VarFileInfo([VarStruct('Translation', [2052, 1200])])],
    )
