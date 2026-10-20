"""Select a front image belonging to this album, excluding unrelated photographs."""
from pathlib import Path
from musicops.library.paths import reject_reparse_chain
from musicops.media.chinese import alias_key
from musicops.media.resources import validate_cover

class AmbiguousCover(ValueError):pass


def local_cover(parents,album,artists,min_side=300,*,strict=False):
    names={alias_key(album),alias_key(' - '.join([' & '.join(artists),album]))}
    found=[]
    for parent in set(map(Path,parents)):
        reject_reparse_chain(parent)
        if not parent.is_dir():continue
        for path in parent.iterdir():
            if path.suffix.casefold() not in ('.jpg','.jpeg','.png','.webp') or not path.is_file() or path.stat().st_size>10_000_000:continue
            stem=alias_key(path.stem);priority=0 if stem=='front' else 1 if stem in ('cover','folder') else 2 if stem in names else None
            if priority is None:continue
            reject_reparse_chain(path);data=path.read_bytes();check=validate_cover(data,min_side)
            if check['valid']:found.append((priority,check['sha256'],data,path))
    if not found:return None
    level=min(row[0] for row in found);candidates={row[1]:row for row in found if row[0]==level}
    if len(candidates)!=1:
        if strict:raise AmbiguousCover('本地有多个不同的同优先级正面封面，需要选择')
        return None
    _,sha,data,path=next(iter(candidates.values()))
    from musicops.providers.cover_download import Artwork
    return Artwork(data,provider='local',path=str(path),sha256=sha,selection='album_front')
