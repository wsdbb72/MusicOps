"""Approved-root containment with Windows case, reserved names and reparse checks."""
import os
import re
import stat
from pathlib import Path, PureWindowsPath

from musicops.domain.errors import PathBlocked

RESERVED = {'CON', 'PRN', 'AUX', 'NUL', 'CLOCK$'} | {f'{prefix}{n}' for prefix in ('COM', 'LPT') for n in range(1, 10)}


def reparse(path):
    try:
        attrs = path.lstat()
    except FileNotFoundError:
        return False
    return path.is_symlink() or bool(getattr(attrs, 'st_file_attributes', 0) & getattr(stat, 'FILE_ATTRIBUTE_REPARSE_POINT', 0x400))


def within(path, root):
    try:
        Path(path).resolve().relative_to(Path(root).resolve())
        return True
    except (ValueError, OSError):
        return False


def check_components(path):
    for part in PureWindowsPath(str(path)).parts:
        if part in {'.', '..'}:
            raise PathBlocked('路径包含点或父目录分量')
        if part.endswith((' ', '.')) or re.search(r'[<>:"|?*\x00-\x1f]', part):
            raise PathBlocked('Windows路径含无效字符或末尾句点/空格')
        if part.split('.', 1)[0].upper() in RESERVED:
            raise PathBlocked('Windows保留设备名不能用作文件或目录')


def reject_reparse_chain(path, stop=None):
    current = Path(path).absolute()
    stop = Path(stop).absolute() if stop else None
    while True:
        if reparse(current):
            raise PathBlocked('路径含junction/symlink/reparse point，须人工确认范围')
        if current == stop or current == current.parent:
            break
        current = current.parent


def _filesystem_limit(root,name,fallback):
    parent=Path(root)
    while not parent.exists() and parent!=parent.parent:parent=parent.parent
    try:
        value=os.pathconf(parent,name)
        return value if value>0 else fallback
    except (OSError,ValueError,AttributeError):return fallback


def safe_destination(root, relative, *, max_length=None):
    raw = str(relative)
    windows = PureWindowsPath(raw)
    if not raw or windows.is_absolute() or windows.drive or raw.startswith(('/', '\\')):
        raise PathBlocked('目标必须为批准根目录下相对路径')
    if any(part in ('.','..') for part in re.split(r'[\\/]',raw)):
        raise PathBlocked('目标拒绝父目录/点路径')
    if os.name=='nt':check_components(raw)
    elif '\x00' in raw or '\\' in raw:
        raise PathBlocked('目标包含空字符或含义不明确的反斜线')
    root = Path(root).absolute()
    reject_reparse_chain(root)
    target = root.joinpath(*windows.parts)
    # POSIX libraries can already have valid paths far beyond MAX_PATH. Count
    # filesystem bytes there, Windows UTF-16 units on Windows, before lstat or
    # resolve can itself fail with ENAMETOOLONG.
    if os.name=='nt':
        length=lambda text:len(str(text).encode('utf-16-le'))//2
        path_limit=240 if max_length is None else max_length
        name_limit=255
    else:
        length=lambda text:len(os.fsencode(text))
        native_limit=_filesystem_limit(root,'PC_PATH_MAX',4096)-1
        path_limit=native_limit if max_length is None else min(max_length,native_limit)
        name_limit=_filesystem_limit(root,'PC_NAME_MAX',255)
    if any(length(part)>name_limit for part in windows.parts):
        raise PathBlocked('目标文件名超过当前文件系统的单个名称长度上限')
    if length(target)>path_limit:
        raise PathBlocked('目标超过保守Windows路径长度上限，请缩短名称' if os.name=='nt' and max_length is None else '目标超过当前文件系统或指定的路径长度上限')
    reject_reparse_chain(target, root)
    if not within(target, root) or target.resolve() == root.resolve():
        raise PathBlocked('目标解析后不在批准根目录内')
    # Existing spelling differs only by case => conflict rather than Windows silent aliasing.
    current = root
    for part in windows.parts:
        if current.is_dir():
            matches = [p.name for p in current.iterdir() if p.name.casefold() == part.casefold()]
            if matches and part not in matches and (os.name=='nt' or (current/part).exists()):
                raise PathBlocked('路径与现有文件/目录存在大小写碰撞')
        current = current / part
    return target


def validate_roots(paths):
    keys = ['library', 'source', 'work', 'backup', 'quarantine']
    roots = {}
    for key in keys:
        raw = paths.get(key)
        if not raw or not Path(raw).is_absolute():
            raise PathBlocked(f'{key}必须为绝对路径')
        root = Path(raw).absolute()
        reject_reparse_chain(root)
        roots[key] = root.resolve()
    for index, key in enumerate(keys):
        for other in keys[index + 1:]:
            if within(roots[key], roots[other]) or within(roots[other], roots[key]):
                raise PathBlocked(f'根目录{key}/{other}重叠，可能污染监控或来源区')
    return roots


def release_path(albumartist, album, title, *, track, disc=1, multidisc=False, edition='', release_id='', extension='flac'):
    """Pure preview with a complete raw-to-clean mapping, no mkdir."""
    mapping = {}
    def clean(value):
        original = str(value)
        result = re.sub(r'[<>:"/\\|?*\x00-\x1f]', '_', original).rstrip(' .') or '_'
        if result.split('.', 1)[0].upper() in RESERVED:
            result = '_' + result
        mapping[original] = result
        return result
    stable_edition = edition or (str(release_id)[:8] if release_id else '')
    components = [clean(albumartist), clean(album) + (f' [{clean(stable_edition)}]' if stable_edition else '')]
    if multidisc:
        components.append(f'CD{int(disc)}')
    components.append(f'{int(track):02d} {clean(title)}.{clean(extension.lstrip("."))}')
    return {'schema_version': 1, 'relative_path': '\\'.join(components), 'name_mapping': mapping}
