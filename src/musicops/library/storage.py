"""Library-adjacent working storage; seed payloads remain byte-for-byte intact."""
from pathlib import Path
from musicops.library.paths import reject_reparse_chain


def storage_paths(library,temporary=None):
    library=Path(library).absolute()
    if library==library.parent:raise ValueError('请选择音乐库子目录，不使用整个磁盘根目录')
    root=library.parent/'MusicOps'
    if root==library:root=library.parent/'MusicOps-data'
    reject_reparse_chain(root)
    paths={key:str(root/name) for key,name in [('source','downloads'),('work','work'),('backup','backup'),('quarantine','quarantine'),('reports','reports')]}
    if temporary and temporary.get('enabled'):
        selected=Path(temporary.get('directory',''))
        if not selected.is_absolute() or selected==selected.parent:raise ValueError('请为临时下载选择完整的专用文件夹，不使用整个磁盘根目录')
        reject_reparse_chain(selected);selected=selected.resolve();actual_library=library.resolve()
        if selected.is_relative_to(actual_library) or actual_library.is_relative_to(selected):raise ValueError('临时下载文件夹不能位于音乐库内，也不能包含音乐库')
        paths.update(source=str(selected/'downloads'),work=str(selected/'work'))
    return paths


def seed_root(library):
    return Path(storage_paths(library)['source']).parent/'seeding'
