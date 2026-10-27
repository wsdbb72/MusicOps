"""Move loose albums at the library root into ``Artist/Album`` folders.

Local completion repairs an album in place. Afterwards, an album that sits
directly in the library root (``/root/陈奕迅 - 阴天快乐/`` or loose files in
``/root``) is renamed to ``/root/陈奕迅/阴天快乐``. Joined credits become one
folder such as ``A／B`` instead of ``A feat. B``. A folder rename keeps CUE
sheets, logs and scans together, needs no copy and is atomic on one disk.

The move is skipped, never forced, when anything is uncertain: the folder
holds audio from another album, the target already exists, files are still
seeding, the downloader cannot be checked, or the rename would cross disks.
The intent is recorded before renaming so an interrupted move is resumed or
recognized as already done.
"""
from __future__ import annotations
import os,re
from pathlib import Path
from musicops.library.index import AUDIO_EXTENSIONS
from musicops.library.paths import reparse,reject_reparse_chain

JOINER='／'  # Full-width solidus: reads as A/B but is a legal single folder name everywhere.
SPLIT=re.compile(r'\s*(?:&|＆|/|／|;|；|,|，|、|×|\bfeat\.|\bfeat\b|\bft\.|\bfeaturing\b)\s*',re.I)
UNKNOWN={'','unknown','unknown artist','未知','未知歌手','未知艺人','[unknown]'}


def folder_artist(artists):
    """``['A feat. B']`` and ``['A','B']`` both become ``A／B``."""
    names=[];seen=set()
    for credit in artists or ():
        for part in SPLIT.split(str(credit)):
            name=part.strip()
            if name and name.casefold() not in seen:seen.add(name.casefold());names.append(name)
    return JOINER.join(names)


def _skip(job,reason):
    job['relocation']={'state':'skipped','reason':reason};return None


def plan(workflows,job,paths):
    """Decide where this completed album belongs, or record why it stays."""
    from musicops.app.pipeline import album_root,release_directory
    cfg=job['config'];root=Path(cfg['paths']['library']).absolute()
    if not cfg['workflow'].get('artist_folders',True):return _skip(job,'已关闭按音乐人整理文件夹')
    artist=folder_artist(job.get('albumartists') or [])
    if artist.casefold() in UNKNOWN or not str(job.get('album') or '').strip():return _skip(job,'缺少可靠的专辑艺人或专辑名')
    paths=[Path(p).absolute() for p in paths]
    if not paths:return _skip(job,'没有音频文件')
    folders={album_root({'path':str(p)},root) for p in paths}
    if len(folders)!=1:return _skip(job,'专辑分散在多个文件夹')
    source=folders.pop()
    loose=source==root
    if not loose and source.parent!=root:return None  # Already inside an artist (or deeper) folder.
    target=root/release_directory(root,artist,job['album'],int(job.get('total_discs') or 1))
    if not loose and str(target).casefold()==str(source).casefold():return None
    if target.exists() and (not target.is_dir() or any(target.iterdir())):return _skip(job,'目标文件夹已存在：'+str(target.relative_to(root)))
    for path in [source,target.parent]:
        if path.exists():reject_reparse_chain(path,root)
    owned={str(p) for p in paths}
    if not loose:
        for item in source.rglob('*'):
            if reparse(item):return _skip(job,'专辑文件夹含链接或重解析点')
            if item.is_file() and item.suffix.casefold() in AUDIO_EXTENSIONS and str(item.absolute()) not in owned:
                return _skip(job,'文件夹中还有其他专辑的音频')
    from musicops.app.library_safety import seeding_roots
    from musicops.clients.http import PolicyBlocked
    try:roots=seeding_roots(workflows.s,cfg)
    except PolicyBlocked:return _skip(job,'无法确认 qBittorrent 做种目录，暂不移动')
    moving=[source] if not loose else paths
    if any(p==r or p.is_relative_to(r) or r.is_relative_to(p) for p in moving for r in roots):return _skip(job,'文件仍在做种，暂不移动')
    files=[] if not loose else [str(p) for p in paths]+[str(p.with_suffix('.lrc')) for p in paths if p.with_suffix('.lrc').is_file()]
    return {'state':'intent','from':str(source),'to':str(target),'loose_files':files}


def relocate(workflows,job,paths):
    """Perform (or resume) the recorded move; returns how old paths map to new ones."""
    record=job.get('relocation')
    if not record or record.get('state') not in ('intent','done'):
        record=plan(workflows,job,paths)
        if not record:
            workflows.put(job,relocation=job.get('relocation'));return {'dirs':{},'files':{}}
        workflows.put(job,relocation=record)
    source,target=Path(record['from']),Path(record['to'])
    if record['state']=='intent':
        target.parent.mkdir(parents=True,exist_ok=True)
        try:
            if record['loose_files']:
                target.mkdir(exist_ok=True)
                for name in record['loose_files']:
                    old=Path(name);new=target/old.name
                    if old.exists():os.rename(old,new)
                    elif not new.exists():raise FileNotFoundError(name)
            elif source.exists():
                os.rename(source,target)
            elif not target.is_dir():raise FileNotFoundError(str(source))
        except OSError as exc:
            # Cross-device or permission problems leave the album where it is.
            workflows.put(job,relocation={'state':'skipped','reason':'无法移动文件夹（'+type(exc).__name__+'），保留原位置'});return {'dirs':{},'files':{}}
        record={**record,'state':'done'};workflows.put(job,relocation=record)
    if record['loose_files']:return {'dirs':{},'files':{str(Path(name)):str(target/Path(name).name) for name in record['loose_files']}}
    return {'dirs':{str(source):str(target)},'files':{}}


def moved(path,mapping):
    path=Path(path)
    if str(path) in mapping['files']:return Path(mapping['files'][str(path)])
    for old,new in mapping['dirs'].items():
        if path.is_relative_to(old):return Path(new)/path.relative_to(old)
    return path


def loose_at_root(row,root):
    """True when an album folder (or the file itself) sits directly in the library root."""
    from musicops.app.pipeline import album_root
    root=Path(root).absolute();folder=album_root(row,root)
    return folder==root or folder.parent==root
