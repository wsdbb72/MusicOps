"""Explicit playlist Single -> Album choice before any search or download."""
import copy
import uuid
from musicops.app.single_upgrade import discover,full_album,matches,in_library,album_context
from musicops.clients.http import PolicyBlocked
from musicops.domain.identity import strict_text


def covers_all(release,tracks):
    album=album_context(release)
    return (release.get('tracklist_complete') and bool(tracks) and
            all(len([t for t in release.get('tracklist',[]) if matches(row,t,album)])==1 for row in tracks))


def offer(workflows,job):
    if job.get('scene')!='playlist' or not job.get('prefer_full_album') or job.get('album_scope_decision'):return False
    rows=job.get('targets') or []
    if not rows:return False
    first=rows[0];cfg=job['config'];resolver=workflows.s._catalog_resolver(cfg)
    query={'title':first.get('raw_title',''),'artists':first.get('artists',[]),'album':first.get('album',''),'duration':first.get('duration')}
    found=None;release=job.get('identified_release')
    if not release:
        # Use normal API/website resolution first. AI remains the existing
        # fallback, rather than an extra call for every known album.
        found=workflows.s._recognize_metadata(query,cfg)
        seeds=([found['selected_candidate']] if found.get('selected_candidate') else [])+found.get('candidates',[])
        verified={};seen=set()
        for seed in seeds:
            source=seed.get('source');ident=('release:'+seed['release_id']) if seed.get('entity_kind')=='recording' and seed.get('release_id') else seed.get('source_id')
            if not source or not ident or (source,ident) in seen:continue
            if len(seen)>=5:break
            if seed.get('album') and strict_text(seed['album'])!=strict_text(first.get('album','')):continue
            seen.add((source,ident));workflows.checkpoint(job,'playlist_release_type')
            candidate=resolver.resolve_candidate(source,ident).get('candidate') or {}
            if strict_text(candidate.get('album') or candidate.get('title'))==strict_text(first.get('album')) and covers_all(candidate,rows):verified[(source,ident)]=candidate
        types={str(r.get('release_type','')).casefold() for r in verified.values()}
        if len(types)==1 and types=={'single'}:release=next(iter(verified.values()))
        elif verified and types=={'album'}:release=next(iter(verified.values()))
    if not release or str(release.get('release_type','')).casefold()!='single' or not covers_all(release,rows):
        workflows.put(job,album_scope_decision='original',album_scope_note='未确认是 Single 发行，按原专辑继续；不凭曲目数量推断单曲')
        return False
    # Keep recording IDs and secondary-type evidence separate from the original
    # platform fields. An unlabelled live release is not a studio recording.
    evidence=[]
    for row in rows:
        track=next(t for t in release['tracklist'] if matches(row,t,album_context(release)))
        evidence.append({**copy.deepcopy(row),'recording_id':track.get('recording_id'),'album':album_context(release)})
    job['album_scope_verified_single']=True
    # Recording search ignores the Single album name; existing API results and
    # official recording relations supply the parent Album candidates.
    lookup=resolver.identify({**query,'album':''})
    if not lookup.get('candidates'):
        lookup=workflows.s._recognize_metadata({**query,'album':''},cfg,found=lookup,force=True)
    lookup,choices=discover(workflows,job,evidence,lookup)
    choices=[c for c in choices if full_album(c,evidence) and covers_all(c,evidence)]
    if not choices:
        workflows.put(job,album_scope_decision='original',album_scope_note='已识别 Single，暂未找到包含全部所选录音的完整专辑，保留原发行继续')
        return False
    workflows.put(job,stage='needs_review',review_kind='album_scope',catalog_choices=choices,
        playlist_single_release=copy.deepcopy(release),playlist_original_targets=copy.deepcopy(rows),
        playlist_scope_recordings=evidence,
        lookup_links=lookup.get('lookup_links',[]),message='已匹配到单曲发行：请选择保留单曲，或下载收录它的完整专辑',
        detail='以下完整发行已核验曲目、艺人与时长。请确认年份、版本和曲目数；选择前不会创建搜索或下载任务，AI 不代替你决定下载范围。')
    return True


def choose(workflows,job,choice):
    if job.get('scene')!='playlist' or job.get('stage')!='needs_review' or job.get('review_kind')!='album_scope':raise PolicyBlocked('完整专辑选项已改变，请重新打开核对窗口')
    if choice=='keep_single':
        workflows.put(job,album_scope_decision='single',stage='identifying',review_kind=None,catalog_choices=[],message='已确认保留原单曲发行，等待检索')
        return {'status':'OK'}
    ident=choice.removeprefix('playlist_album:')
    selected=next((c for c in job.get('catalog_choices',[]) if c.get('candidate_id')==ident),None)
    if not selected:raise ValueError('请选择本任务中已核验的完整专辑')
    workflows.put(job,album_scope_decision='full_pending',album_scope_choice=copy.deepcopy(selected),stage='identifying',review_kind=None,
        message='已选择完整专辑，正在重新核验完整曲目表')
    return {'status':'OK'}


def prepare(workflows,job):
    if job.get('album_scope_decision')!='full_pending':return False
    choice=job['album_scope_choice'];original=job.get('playlist_scope_recordings') or job.get('playlist_original_targets') or []
    if not original:raise PolicyBlocked('原单曲目标缺失，请重新创建本批次')
    resolved=workflows.s._catalog_resolver(job['config']).resolve_candidate(choice['source'],choice['source_id']).get('candidate') or {}
    if not full_album(resolved,original) or not covers_all(resolved,original):
        workflows.put(job,album_scope_decision=None,stage='needs_review',review_kind='album_scope',catalog_choices=[],
            message='所选完整专辑证据已改变，可保留原单曲发行继续，或重新创建任务查找完整专辑')
        return True
    album=resolved.get('album') or resolved['title']
    targets=[{**copy.deepcopy(t),'raw_title':t.get('raw_title') or t['title'],'album':album,'albumartist':copy.deepcopy(resolved['artists']),
              'source':'catalog','track':str(t.get('position') or i+1),'disc':str(t.get('disc') or 1),
              'target_id':str(uuid.uuid5(uuid.NAMESPACE_URL,job['job_id']+':playlist-full:'+str(i)))} for i,t in enumerate(resolved['tracklist'])]
    job.update(targets=targets,expected_album_tracks=copy.deepcopy(targets),album_manifest_verified=True,album_scope_decision='full',
        album_scope_release=copy.deepcopy(resolved),target_count=len(targets),label=album+' · '+' / '.join(resolved['artists']))
    if in_library(workflows,job,targets):
        workflows.put(job,stage='completed',progress=100,message='所选完整专辑已在库中，已跳过重复下载；原单曲文件保留')
        return True
    with workflows.s.mutex:
        if reserve(workflows,job,resolved):return True
        workflows._queue_search(job,'已确认下载完整专辑（'+str(len(targets))+' 首），开始四站检索')
    return True


def reserve(workflows,job,release):
    """Several singles may belong to the same Album; share one import job."""
    from pathlib import Path
    from musicops.config.schema import digest
    # target IDs vary per job; only canonical recording/position evidence belongs
    # to the reservation key.
    key='playlist-album-import:'+digest({'root':str(Path(job['config']['paths']['library']).resolve()),
        'source':release['source'],'release':release['source_id'],
        'prefer_24bit':job.get('prefer_24bit',True),'edition_policy':job['config']['workflow'].get('edition_policy'),
        'tracks':[{k:t.get(k) for k in ('raw_title','artists','duration','track','disc')} for t in job['targets']]})
    with workflows.s.mutex:
        previous=workflows.store.get('evidence',key) or {}
        owner=workflows.store.get('evidence','workflow:'+str(previous.get('job_id','')))
        latest=owner;seen=set()
        while latest and latest['job_id'] not in seen:
            seen.add(latest['job_id']);next_id=latest.get('processing_job_id') or latest.get('search_job_id')
            if not next_id:break
            latest=workflows.store.get('evidence','workflow:'+next_id)
        if owner and owner['job_id']!=job['job_id'] and latest and latest.get('stage') not in ('cancelled','skipped','restored','completed'):
            merged={r['target_id']:r for r in owner.get('merged_playlist_targets',[]) if r.get('target_id')}
            merged.update({r['target_id']:copy.deepcopy(r) for r in job['playlist_original_targets'] if r.get('target_id')})
            workflows.put(owner,merged_playlist_targets=list(merged.values()))
            workflows.put(job,stage='skipped',merged_into_job_id=owner['job_id'],message='已合并到现有《'+job['targets'][0]['album']+'》完整专辑任务，跳过重复检索与下载；请查看该专辑任务进度')
            return True
        workflows.store.put('evidence',key,{'job_id':job['job_id'],'kind':'playlist_album_import'})
    return False


def validate(job,rows):
    if job.get('album_scope_decision')!='full':return
    expected=job.get('expected_album_tracks') or []
    if not expected or len(rows)!=len(expected) or not covers_all({'album':job.get('album'),'tracklist_complete':True,'tracklist':rows},expected):
        raise PolicyBlocked('下载内容不是所选完整专辑的完整录音曲目表，保留文件等待核对')
