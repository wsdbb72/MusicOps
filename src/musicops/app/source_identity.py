"""Recover unreliable playlist labels before giving up PT album search."""
import copy
from musicops.domain.identity import strict_text
from musicops.app.recognition import RecognitionFallback

PLATFORMS={'netease','qqmusic','qishui','kugou','kuwo','migu','bilibili','applemusic','spotify','youtube'}

def is_streaming(row):return row.get('source') in PLATFORMS

def recover(workflows,job):
    rows=job.get('targets') or []
    if job.get('scene')!='playlist' or not rows or not any(is_streaming(r) for r in rows) or job.get('source_identity_attempted'):return False
    first=rows[0]
    if not first.get('raw_title') or not first.get('duration'):return False
    cfg=job['config']
    if not cfg.get('catalog',{}).get('enabled') and not cfg.get('ai',{}).get('enabled'):return False
    job['source_identity_attempted']=True;workflows.put(job)
    query={'title':first['raw_title'],'artists':first.get('artists',[]),'album':first.get('album',''),'duration':first['duration'],'untrusted_metadata':True}
    found=workflows.s._recognize_metadata(query,cfg)
    job.update(catalog_choices=found.get('candidates',[]),lookup_links=found.get('lookup_links',[]),ai_fallback=found.get('ai_fallback',{}),identity_lookup_attempts=found.get('identity_lookup_attempts',[]))
    if found.get('ai_fallback',{}).get('calls'):job['ai_search_attempted']=True
    selected=found.get('selected_candidate') or RecognitionFallback.unique_recording(query,found.get('candidates',[]))
    if selected:
        source_id='release:'+selected['release_id'] if selected.get('entity_kind')=='recording' and selected.get('release_id') else selected['source_id']
        resolved=workflows.s._catalog_resolver(cfg).resolve_candidate(selected['source'],source_id).get('candidate')
        if resolved and resolved.get('tracklist_complete'):
            mapping=[];used=set()
            for row in rows:
                matches=[(i,t) for i,t in enumerate(resolved.get('tracklist',[])) if i not in used
                    and strict_text(t.get('raw_title') or t.get('title'))==strict_text(row.get('raw_title'))
                    and {strict_text(a) for a in t.get('artists',[])}=={strict_text(a) for a in row.get('artists',[])}
                    and t.get('duration') and row.get('duration') and abs(t['duration']-row['duration'])<=2]
                if len(matches)!=1:break
                i,track=matches[0];used.add(i);mapping.append(track)
            album=resolved.get('album') or resolved.get('title')
            if len(mapping)==len(rows) and album:
                changed=any(strict_text(r.get('album'))!=strict_text(album) for r in rows)
                if changed:
                    for row,track in zip(rows,mapping):
                        row.setdefault('source_metadata_original',{k:copy.deepcopy(row.get(k)) for k in ('raw_title','artists','album','duration')})
                        row.update(album=album,artists=copy.deepcopy(track['artists']),metadata_complete=True)
                        if row.get('albumartist'):row['albumartist']=copy.deepcopy(resolved.get('artists') or track['artists'])
                    job['recognition_evidence']={'source':resolved['source'],'source_id':resolved['source_id'],'tracklist_verified':True,'basis':'title_artist_duration'}
                    workflows.put(job,stage='searching',search_query=None,label=' / '.join(rows[0]['artists'])+' · '+album,
                        message='平台专辑资料已从真实曲目表纠正，重新检索音源')
                    return True
                workflows.put(job);return False
    # Title-only leads require artist/recording verification by the existing AI
    # metadata queue (or manual review if AI is unavailable/ambiguous).
    plausible=[c for c in found.get('candidates',[]) if c.get('entity_kind')=='recording'
        and strict_text(c.get('title'))==strict_text(first['raw_title']) and c.get('duration') and abs(c['duration']-first['duration'])<=2]
    if selected or plausible:
        workflows.put(job,stage='needs_review',review_kind='metadata',message='平台专辑或歌手资料存在冲突，已登记曲目身份核对',
            detail='已按歌名与歌手、仅歌名检索真实目录；继续核实完整时长、版本和发行，不能仅凭同名下载。')
        return True
    workflows.put(job);return False
