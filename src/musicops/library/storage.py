"""Library-adjacent working storage; seed payloads remain byte-for-byte intact."""
from pathlib import Path
from musicops.library.paths import reject_reparse_chain


def storage_paths(library):
    library=Path(library).absolute()
    if library==library.parent:raise ValueError('请选择音乐库子目录，不使用整个磁盘根目录')
    root=library.parent/'MusicOps'
    if root==library:root=library.parent/'MusicOps-data'
    reject_reparse_chain(root)
    return {key:str(root/name) for key,name in [('source','downloads'),('work','work'),('backup','backup'),('quarantine','quarantine'),('reports','reports')]}


def seed_root(library):
    return Path(storage_paths(library)['source']).parent/'seeding'
