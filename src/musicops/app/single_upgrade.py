"""User-selected single -> complete release; never infer Album from file count."""
import copy
import uuid
from pathlib import Path
from musicops.domain.identity import strict_text
from musicops.domain.editions import recording_kinds
from musicops.clients.http import PolicyBlocked,HttpError
from musicops.domain.errors import PathBlocked


def matches(old,track,album=''):
    from musicops.app.pipeline import identity
    return (identity(old)==identity(track) and bool(old.get('duration') and track.get('duration'))
            and abs(old['duration']-track['duration'])<=2
            and recording_kinds(old.get('album','')+' '+old.get('raw_title',''))==recording_kinds(album+' '+track.get('raw_title',track.get('title',''))))


def full_album(candidate,old):
    tracks=candidate.get('tracklist') or []
    if str(candidate.get('release_type','')).casefold()!='album' or not candidate.get('tracklist_complete') or not tracks:return False
    album=candidate.get('album') or candidate.get('title','')
    if len(tracks)<=len(old) and strict_text(album)==strict_text(old[0].get('album','')):return False
    if any(not t.get('duration') or not t.get('artists') or not (t.get('title') or t.get('raw_title')) for t in tracks):return False
    return any(len([t for t in tracks if matches(row,t,candidate.get('album') or candidate.get('title',''))])==1 for row in old)


def identify(workflows,job):
    old=job['old_files'];cfg=job['config'];resolver=workflows.s._catalog_resolver(cfg)
    choice=job.get('completion_choice')
    if choice:
        resolved=resolver.resolve_candidate(choice['source'],choice['source_id']).get('candidate') or {}
        if not full_album(resolved,old):raise PolicyBlocked('所选发行不是可核验的完整专辑，或没有对应原单曲录音；原版保留')
        approved=next((c for c in job.get('catalog_choices',[]) if c.get('candidate_id')==choice.get('candidate_id')),None)
        if not approved:raise PolicyBlocked('所选完整发行不属于本任务的已核对候选')
        tracks=resolved['tracklist'];album=resolved.get('album') or resolved['title']
        targets=[{**copy.deepcopy(t),'raw_title':t.get('raw_title') or t['title'],'album':album,'albumartist':resolved.get('artists',[]),'source':'catalog',
                  'target_id':str(uuid.uuid5(uuid.NAMESPACE_URL,job['job_id']+':full:'+str(i))),
                  'track':str(t.get('position') or i+1),'disc':str(t.get('disc') or 1)} for i,t in enumerate(tracks)]
        represented=[r for r in old if len([t for t in tracks if matches(r,t,album)])==1]
        retire=bool(job.get('retire_single_requested') and len(represented)==len(old))
        job.update(targets=targets,expected_album_tracks=copy.deepcopy(targets),target_count=len(targets),single_release=copy.deepcopy(resolved),
                   retire_single=retire,single_match_count=len(represented),single_unmatched_count=len(old)-len(represented),label=album+' · '+' / '.join(resolved.get('artists',[])))
        if in_library(workflows,job,targets):
            workflows.put(job,stage='completed',progress=100,message='对应完整专辑已在本地资料库中，无需重复下载；原单曲保留，可在去重合并页核对')
            return
        workflows._queue_search(job,'已确认完整专辑，开始检索；'+('核验入库后原单曲移入恢复备份' if retire else '保留原单曲发行'))
        return
    first=old[0]
    found=workflows.s._recognize_metadata({'title':first.get('raw_title',''),'artists':first.get('artists',[]),'album':'','duration':first.get('duration')},cfg,force=True)
    seeds=([found['selected_candidate']] if found.get('selected_candidate') else [])+found.get('candidates',[])
    pending=[];seen=set();recordings=set();verified={}
    for seed in seeds:
        if seed.get('source')=='musicbrainz' and seed.get('recording_id') and seed['recording_id'] not in recordings and len(recordings)<3:
            recordings.add(seed['recording_id']);workflows.checkpoint(job,'single_recording_releases')
            try:
                recording=resolver.resolve_candidate('musicbrainz','recording:'+seed['recording_id']).get('candidate') or {}
                if matches(first,recording):pending.extend({'source':'musicbrainz',**r} for r in recording.get('releases',[])[:12])
            except (HttpError,ValueError,OSError):pass
        if seed.get('source_id','').startswith('release:'):pending.append(seed)
    for candidate in pending:
        key=(candidate.get('source'),candidate.get('source_id'))
        if not all(key) or key in seen:continue
        if len(seen)>=16:break
        seen.add(key);workflows.checkpoint(job,'single_full_release')
        try:resolved=resolver.resolve_candidate(*key).get('candidate') or {}
        except (HttpError,ValueError,OSError):continue
        # A recording search relation, matching names/artists and full durations
        # are required. File count alone is never evidence of an album release.
        if full_album(resolved,old):
            resolved['single_matched_tracks']=sum(len([t for t in resolved['tracklist'] if matches(r,t,resolved.get('album') or resolved.get('title',''))])==1 for r in old)
            verified[key]=resolved
    workflows.put(job,stage='needs_review',review_kind='metadata',catalog_choices=list(verified.values()),lookup_links=found.get('lookup_links',[]),
        message='请选择要升级到的完整专辑发行' if verified else '尚未找到可核验的完整专辑，原单曲保留',
        detail='仅列出目录标记为 Album、完整曲目表和录音时长可对应的发行；候选范围有限。请确认专辑、年份和版本后继续。未被完整专辑收录的 B 面或特别录音会保留在原单曲中。')


def in_library(workflows,job,targets):
    root=Path(job['config']['paths']['library']).resolve();groups={}
    from musicops.app.pipeline import album_root
    for row in workflows.store.list('library_files'):
        path=Path(row.get('path',''))
        if row.get('location_state')!='library' or not path.is_absolute() or not path.is_relative_to(root):continue
        if strict_text(row.get('album',''))!=strict_text(targets[0]['album']):continue
        try:
            from musicops.library.paths import reject_reparse_chain
            reject_reparse_chain(path)
            info=path.stat()
            if (info.st_size,info.st_mtime_ns)!=(row.get('size'),row.get('mtime_ns')):continue
        except (OSError,RuntimeError,PathBlocked):continue
        groups.setdefault(str(album_root(row,root)),[]).append(row)
    return any(all(len([r for r in rows if matches(t,r,t['album'])])==1 for t in targets) for rows in groups.values())


def validate(job,rows,validator):
    from musicops.app.pipeline import no_downgrade
    expected=job.get('expected_album_tracks') or []
    if not expected or len(rows)!=len(expected):raise PolicyBlocked('完整专辑曲目数量不符，原单曲保留')
    if not job.get('single_release') or not full_album(job['single_release'],job['old_files']):raise PolicyBlocked('缺少已确认的完整发行证据，原单曲保留')
    for track in expected:
        if len([r for r in rows if matches(track,r,job['album'])])!=1:raise PolicyBlocked('完整专辑缺曲或录音版本不一致，原单曲保留')
    compared=0
    for original in job['old_files']:
        same=[r for r in rows if matches(original,r,job['album'])]
        if not same:
            if job.get('retire_single'):raise PolicyBlocked('完整专辑未包含全部原单曲，不能移出原版')
            continue
        if len(same)!=1:raise PolicyBlocked('原单曲在新专辑中对应不唯一，原版保留')
        check=validator.validate(original['path'])
        if check.get('decode_state')!='passed' or not no_downgrade(check['media_info'],same[0]['media_info']):raise PolicyBlocked('完整专辑中的对应歌曲音质下降或原文件未通过检查，原单曲保留')
        compared+=1
    if not compared:raise PolicyBlocked('完整专辑没有可对应的原单曲，原版保留')
