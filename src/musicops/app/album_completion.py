"""Resolve a complete release before treating a partial local album as complete."""
import uuid
from musicops.domain.identity import strict_text
from musicops.media.source_labels import effective_album_artists

def manifest(workflows,job):
    from musicops.app.pipeline import identity
    old=job['old_files'];first=old[0];cfg=job['config'];resolver=workflows.s._catalog_resolver(cfg)
    query={'title':first.get('raw_title',''),'artists':effective_album_artists(first),'album':first.get('album',''),'duration':first.get('duration')}
    found=workflows.s._recognize_metadata(query,cfg,force=True);verified={}
    candidates=[job['completion_choice']] if job.get('completion_choice') else ([found['selected_candidate']] if found.get('selected_candidate') else [])+found.get('candidates',[])[:8]
    for candidate in candidates:
        workflows.checkpoint(job,'album_manifest')
        source_id='release:'+candidate['release_id'] if candidate.get('entity_kind')=='recording' and candidate.get('release_id') else candidate.get('source_id')
        if not source_id:continue
        resolved=resolver.resolve_candidate(candidate['source'],source_id).get('candidate') or {}
        tracks=resolved.get('tracklist') or [];album=resolved.get('album') or resolved.get('title')
        if not resolved.get('tracklist_complete') or not tracks or strict_text(album)!=strict_text(first.get('album')):continue
        matches=[]
        for row in old:
            same=[t for t in tracks if identity(t)==identity(row) and t.get('duration') and row.get('duration') and abs(t['duration']-row['duration'])<=2]
            if len(same)!=1:break
            matches.append(same[0])
        if len(matches)!=len(old):continue
        # Equivalent editions with the exact same manifest need no duplicate choice.
        key=tuple((identity(t),t.get('duration'),str(t.get('disc')),str(t.get('position'))) for t in tracks)
        verified[key]=(resolved,tracks)
    if len(verified)!=1:
        workflows.put(job,stage='needs_review',review_kind='metadata',catalog_choices=found.get('candidates',[]),lookup_links=found.get('lookup_links',[]),
            message='未取得唯一完整发行曲目表，不能将现有部分歌曲当作整张专辑',detail='可核对专辑名、艺人和版本后重试；现有音频保留。');return
    resolved,tracks=next(iter(verified.values()));missing=[t for t in tracks if not any(identity(t)==identity(r) and t.get('duration') and r.get('duration') and abs(t['duration']-r['duration'])<=2 for r in old)]
    if not missing:workflows.put(job,stage='completed',progress=100,message='已核验完整发行曲目表，本地专辑没有缺曲');return
    targets=[]
    for i,track in enumerate(tracks):
        if not track.get('duration') and (job.get('complete_release') or {}).get('source_id')==resolved.get('source_id'):
            prior=next((t for t in job.get('targets',[]) if identity(t)==identity(track) and str(t.get('track'))==str(track.get('position') or i+1) and str(t.get('disc'))==str(track.get('disc') or 1)),None)
            if prior and prior.get('duration'):track={**track,'duration':prior['duration']}
        targets.append({**track,'raw_title':track.get('raw_title') or track.get('title',''),'album':first['album'],'source':'catalog',
                        'target_id':str(uuid.uuid5(uuid.NAMESPACE_URL,job['job_id']+':'+str(i))),
                        'disc':str(track.get('disc') or 1),'track':str(track.get('position') or i+1)})
    job.update(targets=targets,expected_album_tracks=targets,target_count=len(targets),matched_count=len(old),album_completion_missing=len(missing),
               complete_release={'source':resolved.get('source'),'source_id':resolved.get('source_id'),'track_count':len(tracks)})
    if any(not t.get('duration') for t in targets):
        workflows.put(job,stage='needs_review',review_kind='metadata',message='完整专辑曲目表已取得，部分曲目缺少完整时长，请补充后继续');return
    workflows._queue_search(job,'已核验完整曲目表：缺少 '+str(len(missing))+' 首，开始检索整张专辑；入库时只补缺曲')
