"""Deliberate reuse of an existing album folder across historical layouts."""
from pathlib import Path
from musicops.library.album_lookup import candidates
from musicops.library.paths import reject_reparse_chain
from musicops.clients.http import PolicyBlocked


def offer(workflows,job):
    if job['scene']!='playlist' or job.get('selected_edition') or job.get('existing_album_files') or job.get('existing_album_decision'):return False
    found=candidates(workflows.store,job['config']['paths']['library'],job['album'],job['albumartists'])
    if not found:return False
    if len(found)==1 and job.get('prepared'):
        from musicops.library.index import sha256_file
        from musicops.library.catalog import unchanged_library_file
        root=job['config']['paths']['library']
        rows=[workflows.store.get('library_files',fid) for fid in found[0]['file_ids']]
        try:
            if rows and all(r and unchanged_library_file(r,root) for r in rows):
                hashes={sha256_file(r['path']) for r in rows}
                if all(sha256_file(r['path']) in hashes for r in job['prepared']):
                    choose(workflows,job,'existing_album:'+found[0]['album_id'])
                    workflows.put(job,message='已核验音频字节完全一致，自动复用既有目录并保留已有录音')
                    return True
        except (OSError,PolicyBlocked):pass
    workflows.put(job,stage='needs_review',review_kind='existing_album',existing_album_choices=found,
                  message='音乐库已有同名专辑：请选择补入既有目录，或明确保留独立版本',
                  detail='名称已做繁简、空格及标点归一化；这只是候选，不代表同一发行。补入时保留已有录音，不覆盖现有音源。')
    return True


def choose(workflows,job,choice):
    if choice=='separate_album_version':
        workflows.put(job,existing_album_decision='separate',stage='publishing',review_kind=None,message='用户选择保留独立发行目录')
        return {'status':'OK'}
    if not isinstance(choice,str) or not choice.startswith('existing_album:'):raise ValueError('请选择现有专辑目录或独立版本')
    ident=choice.split(':',1)[1];root=Path(job['config']['paths']['library']).resolve()
    found=candidates(workflows.store,root,job['album'],job['albumartists'])
    album=next((r for r in found if r['album_id']==ident),None)
    if not album:raise PolicyBlocked('既有专辑目录已改变，请重新打开核对')
    directory=Path(album['directory']);reject_reparse_chain(directory,root)
    if not directory.is_dir() or not directory.resolve().is_relative_to(root):raise PolicyBlocked('既有专辑目录不在音乐库内')
    rows=[workflows.store.get('library_files',fid) for fid in album['file_ids']]
    if not rows or any(not r for r in rows):raise PolicyBlocked('既有专辑索引不完整，请先重新扫描')
    from musicops.library.catalog import unchanged_library_file
    if any(not unchanged_library_file(r,root) for r in rows):raise PolicyBlocked('既有音源已改变，请先重新扫描音乐库')
    from musicops.app.pipeline import identity
    for incoming in job['prepared']:
        matches=[r for r in rows if identity(r)==identity(incoming)]
        if len(matches)>1 or matches and (not incoming.get('duration') or not matches[0].get('duration') or abs(incoming['duration']-matches[0]['duration'])>2):
            raise PolicyBlocked('同名歌曲存在多份或录音时长不同，请选择独立版本或先在去重维护中核对')
    # Folder reuse alone is insufficient: music servers group by embedded
    # album/albumartist tags. Harmonize only our unpublished private copies;
    # retain the original per-track artists and every existing library file.
    if job['album']!=album['album'] or job['albumartists']!=album['artists']:
        from musicops.media.tags import apply_tag_changes,read_tag_fields
        prepared=Path(job.get('prepared_root') or '')
        if not job.get('prepared_root'):raise PolicyBlocked('整理副本缺少归属记录，请重新准备后合并')
        for incoming in job['prepared']:
            path=Path(incoming['path']);reject_reparse_chain(path)
            if not path.resolve().is_relative_to(prepared.resolve()):raise PolicyBlocked('拒绝改写整理副本之外的音频')
            apply_tag_changes(path,{'album':album['album'],'albumartist':album['artists']})
            incoming['tags']=read_tag_fields(path);incoming['album']=album['album']
        job['album']=album['album'];job['albumartists']=album['artists']
    workflows.put(job,existing_album_decision='reuse',existing_album_directory=str(directory.relative_to(root)),
                  existing_album_files=rows,stage='publishing',review_kind=None,message='用户确认补入既有目录，已有录音保留')
    return {'status':'OK'}
