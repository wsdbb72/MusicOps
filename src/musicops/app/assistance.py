"""Grounded AI proposals for an explicit, editable review. Never apply on generation."""
import json
from musicops.app.recognition import RecognitionFallback
from musicops.app.source_identity import is_streaming
from musicops.app.workflow_actions import selection
from musicops.config.schema import digest
from musicops.domain.identity import strict_text
from musicops.state.store import utcnow
from musicops.clients.http import PolicyBlocked


def review_revision(job):
    tracks = job.get('prepared') or job.get('targets') or job.get('old_files') or []
    return digest({'stage': job.get('stage'), 'kind': job.get('review_kind'), 'hidden': bool(job.get('hidden')),
                   'search_query':job.get('search_query'),'search_sites':job.get('search_sites'),
                   'resume_stage':job.get('resume_stage'),'error_class':job.get('error_class'),
                   'selected':(job.get('selected') or {}).get('candidate_id'),'batch_id':job.get('batch_id'),
                   'catalog_choices':[{k:c.get(k) for k in ('candidate_id','source','source_id','release_id')} for c in job.get('catalog_choices',[])],
                   'tracks': [{k: r.get(k) for k in ('raw_title', 'artists', 'album', 'duration', 'file_id')} for r in tracks],
                   'choices': [{k: c.get(k) for k in ('candidate_id', 'source', 'title', 'artists', 'release', 'media')} for c in job.get('choices', [])]})


def _metadata(service, job, cfg, provider=None):
    rows = job.get('prepared') or job.get('targets') or job.get('old_files') or []
    if not rows: return {'status': 'NEEDS_INPUT', 'message': '没有曲目资料，请手动填写或重新识别'}
    first = rows[0]
    found = service._recognize_metadata({'title': first.get('raw_title', ''), 'artists': first.get('artists', []),
        'album': first.get('album', ''), 'duration': first.get('duration'), 'filename_hint': str(first.get('path') or '').replace('\\','/').rsplit('/',1)[-1],
        'untrusted_metadata':is_streaming(first)}, cfg, force=True)
    selected = found.get('selected_candidate')
    if not selected and len(found.get('candidates',[]))==1:
        candidate=found['candidates'][0]
        if RecognitionFallback._has_unique_exact(RecognitionFallback._query({'title':first.get('raw_title',''),'artists':first.get('artists',[]),'album':first.get('album',''),'duration':first.get('duration')}),[candidate]):selected=candidate
    proposal = {'status': 'NEEDS_INPUT', 'kind': 'metadata', 'message': 'AI 未找到足够明确的真实目录证据，请手动选择或填写',
                'catalog_choices': found.get('candidates', []), 'lookup_links': found.get('lookup_links', []),
                'reason': found.get('ai_fallback', {}).get('reason', '')}
    if not selected:
        web=found.get('ai_fallback',{})
        if web.get('web_sources') and web.get('web_queries'):
            proposal.update(status='NEEDS_REVIEW',message='AI 联网找到了资料线索，但本机尚未核实完整发行；可查看来源并手动填入，不能自动确认',
                            web_queries=web['web_queries'],reason=web.get('web_reason') or proposal['reason'])
        return proposal
    source_id = 'release:' + selected['release_id'] if selected.get('entity_kind') == 'recording' and selected.get('release_id') else selected.get('source_id')
    resolved = service._catalog_resolver(cfg).resolve_candidate(selected['source'], source_id).get('candidate')
    if not resolved: return proposal
    observed = resolved.get('tracklist', [])
    if not observed and selected.get('entity_kind') == 'recording': observed = [selected]
    metadata = []; changed = 0
    aliases={}
    unmatched=[(index,row) for index,row in enumerate(rows) if not any(strict_text(track.get('raw_title') or track.get('title'))==strict_text(row.get('raw_title')) and {strict_text(a) for a in track.get('artists',[])}=={strict_text(a) for a in row.get('artists',[])} for track in observed)]
    claimed_tracks=[t for t in observed if any(strict_text(t.get('raw_title') or t.get('title'))==strict_text(r.get('raw_title')) and {strict_text(a) for a in t.get('artists',[])}=={strict_text(a) for a in r.get('artists',[])} for r in rows)]
    if unmatched and hasattr(provider,'map_album_tracks'):
        for start in range(0,len(unmatched),12):
            service._checkpoint('ai_album_alias_mapping')
            batch=unmatched[start:start+12]
            possible=[t for t in observed if t.get('duration') and any(r.get('duration') and abs(r['duration']-t['duration'])<=2 for _,r in batch)]
            if not possible:continue
            def context(row):return {'title':row.get('raw_title') or row.get('title') or '', 'artists':row.get('artists',[]),'duration':row.get('duration')}
            local=[context(r) for _,r in batch];actual=[context(t) for t in possible]
            key='ai-album-map:'+digest({'local':local,'observed':actual,'ai':cfg['ai']})
            saved=service.store.get('evidence',key)
            mapping=saved['mapping'] if saved else provider.map_album_tracks(local,actual)
            if not saved:service.store.put('evidence',key,{'mapping':mapping})
            for item in mapping:
                position,row=batch[item['local']];track=possible[item['observed']]
                if row.get('duration') and track.get('duration') and abs(row['duration']-track['duration'])<=2 and possible[item['observed']] not in claimed_tracks:
                    aliases[position]=track;claimed_tracks.append(track)
    for position,row in enumerate(rows):
        service._checkpoint('ai_review_mapping')
        matches = [track for track in observed if strict_text(track.get('raw_title') or track.get('title')) == strict_text(row.get('raw_title'))
                   and (not row.get('artists') or {strict_text(a) for a in row['artists']} == {strict_text(a) for a in track.get('artists', [])})
                   and (not row.get('duration') or track.get('duration') and abs(row['duration'] - track['duration']) <= 2)]
        if not matches and position in aliases:matches=[aliases[position]]
        entry = {'title': row.get('raw_title', ''), 'artist': ' / '.join(row.get('artists', [])),
                 'album': row.get('album', ''), 'duration': str(row.get('duration') or '')}
        if len(matches) == 1:
            track = matches[0]
            values = {'title': track.get('raw_title') or track.get('title'), 'artist': ' / '.join(track.get('artists', [])),
                      'album': resolved.get('album') or resolved.get('title'), 'duration': str(track.get('duration') or row.get('duration') or '')}
            entry.update({k: str(v) for k, v in values.items() if v})
            changed += 1
        metadata.append(entry)
    if changed:
        proposal.update(status='READY', metadata=metadata, mapped_count=changed,
                        message=f'已从真实发行曲目表映射 {changed} / {len(rows)} 首，请核对黄色建议后保存',
                        source=resolved.get('source'), source_id=resolved.get('source_id'),
                        confidence=found.get('ai_fallback', {}).get('confidence'), evidence=RecognitionFallback._context_candidate(resolved))
    return proposal


def _candidate(service, job, cfg, provider):
    tracks = job.get('targets') or job.get('old_files') or []; first = tracks[0] if tracks else {}
    choices = job.get('choices', [])[:20]
    contexts = []
    for candidate in choices:
        context = RecognitionFallback._context_candidate(candidate)
        context['disambiguation'] = json.dumps({k:candidate.get('media',{}).get(k) for k in ('format','encoding','sample_rate','bits_per_sample','bitrate','channels')},ensure_ascii=False)[:500]
        contexts.append(context)
    query = RecognitionFallback._query({'album': first.get('album', ''), 'artists': first.get('albumartist') or first.get('artists', []), 'title': ''})
    key='ai-choice:'+digest({'query':query,'candidates':contexts,'ai':cfg['ai']})
    cached=service.store.get('evidence',key)
    import time
    if cached and time.time()-cached.get('at',0)<86400:result=cached['result']
    else:
        result=provider.recognize_candidate(query,contexts)
        service.store.put('evidence',key,{'kind':'ai_choice_cache','at':time.time(),'result':result})
    ready = result['candidate_id'] is not None and result['confidence'] >= .9 and not result['ambiguous']
    return {'status': 'READY' if ready else 'NEEDS_INPUT', 'kind': 'candidate',
            'candidate_id': result['candidate_id'] if ready else None, 'confidence': result['confidence'],
            'reason': result['reason'], 'observed_count': len(contexts),
            'message': '已推荐现有候选，请核对艺人、发行与音质后采用' if ready else '候选仍有歧义，请在核对窗口选择发行'}


def assist_workflows(service, args, cfg):
    ids = selection(args.get('job_ids'), 10000)
    provider, unavailable = RecognitionFallback(cfg, service._catalog_resolver(cfg), service.vault.get, service._checkpoint)._provider()
    if unavailable: raise PolicyBlocked('请先在连接与设置中启用 AI，并填写服务地址、模型与 API Key')
    results = []
    for job_id in ids:
        service._checkpoint('ai_review_next')
        with service.mutex:
            job = service.store.get('evidence', 'workflow:' + job_id)
            if not job or job.get('hidden') or job['stage'] not in {'needs_review', 'failed', 'skipped'}:
                results.append({'job_id': job_id, 'status': 'SKIPPED', 'message': '该任务没有等待核对的问题'}); continue
            revision = review_revision(job)
        try:
            kind = job.get('review_kind')
            if kind in ('error','interrupted') and job.get('error_class') in ('NETWORK_ERROR','TRANSIENT_ERROR','BROWSER_SOLVER_UNAVAILABLE','SOURCE_UNAVAILABLE') and job.get('resume_stage') in ('downloading','fallback','searching','notifying') and not job.get('retry_count'):
                proposal={'status':'READY','kind':'retry','message':'瞬时连接问题，沿用已有下载记录重试一次，不额外调用 AI'}
            elif kind == 'candidate' and job.get('choices'):
                proposal = _candidate(service, job, cfg, provider)
            elif kind == 'metadata':
                proposal = _metadata(service, job, cfg, provider)
            elif kind=='search' or job.get('task_type')=='search' and (kind in {'error','interrupted'} or job['stage'] in {'failed','skipped'}):
                rows = job.get('targets') or job.get('old_files') or []; first = rows[0] if rows else {}
                found = service._recognize_metadata({'title': first.get('raw_title',''), 'artists': first.get('artists', []), 'album': first.get('album', ''),'duration':first.get('duration'),'untrusted_metadata':True}, cfg, force=True)
                selected = found.get('selected_candidate')
                query = (' '.join(selected.get('artists', [])) + ' ' + str(selected.get('album') or selected.get('title') or '')).strip() if selected else ''
                current_query=job.get('search_query') or (' '.join(first.get('artists',[]))+' '+first.get('album',''))
                if strict_text(query)==strict_text(current_query):query=''
                report=found.setdefault('ai_fallback',{})
                if not query and not (report.get('suggested_queries') or report.get('queries')) and hasattr(provider,'recognition_queries'):
                    # A correctly identified album can still need different PT
                    # keywords. Identity success must not disable this fallback.
                    source_query=RecognitionFallback._query({'title':'','artists':first.get('artists',[]),'album':first.get('album','')})
                    contexts=[RecognitionFallback._context_candidate(c) for c in found.get('candidates',[])[:12]]
                    key='ai-search-queries:'+digest({'query':source_query,'candidates':contexts,'ai':cfg['ai']})
                    cached=service.store.get('evidence',key)
                    alternate=cached['result'] if cached else provider.recognition_queries(source_query,contexts)
                    if not cached:service.store.put('evidence',key,{'result':alternate})
                    report['suggested_queries']=alternate['queries']
                proposal = {'status': 'READY' if query and len(query) <= 300 else 'NEEDS_INPUT', 'kind': 'search',
                            'search_query': query if len(query) <= 300 else '',
                            'reason': found.get('ai_fallback', {}).get('reason', ''),
                            'message': '已生成真实目录支持的搜索词，可编辑后重新搜索' if query else '请检查各站状态，修改搜索词或站点后重试'}
                web=found.get('ai_fallback',{})
                alternatives=web.get('suggested_queries') or web.get('queries') or []
                if not query and alternatives:
                    previous_query=job.get('search_query') or (' '.join(first.get('artists',[]))+' '+first.get('album',''))
                    suggestions=[(' '.join(v.get('artists',[]))+' '+str(v.get('album') or v.get('title') or '')).strip() for v in alternatives]
                    candidate_query=next((value for value in suggestions if strict_text(value)!=strict_text(previous_query)), '')
                    if candidate_query and len(candidate_query)<=300:proposal.update(status='READY',search_query=candidate_query,message='AI 已提供别名搜索词，重新检索后仍核实实际候选，不直接确认身份')
                if not selected and web.get('web_sources') and web.get('web_queries'):
                    first_query=web['web_queries'][0]
                    suggested=(' '.join(first_query['artists'])+' '+(first_query['album'] or first_query['title'])).strip()
                    proposal.update(status='NEEDS_REVIEW',search_query=suggested if len(suggested)<=300 else '',
                        lookup_links=found.get('lookup_links',[]),web_queries=web['web_queries'],reason=web.get('web_reason',''),
                        message='AI 联网找到了检索线索，尚未核实完整发行；请查看来源并编辑搜索词后重试')
            else:
                advice=provider.recovery_advice({'stage':job['stage'],'review_kind':kind,'resume_stage':job.get('resume_stage'),
                    'error_class':job.get('error_class'),'album':str(job.get('label') or '')[:200],
                    'tracks':[RecognitionFallback._context_candidate({'title':r.get('raw_title',''),'artists':r.get('artists',[]),'duration':r.get('duration')}) for r in (job.get('targets') or job.get('old_files') or job.get('prepared') or [])[:12]]}) if hasattr(provider,'recovery_advice') else {}
                proposal = {'status':'NEEDS_INPUT','kind':kind,'reason':advice.get('reason',''),
                    'message':advice.get('reason') or '已保留现有证据；此问题需要补充封面、曲序、歌词服务或发行版本，请打开核对窗口'}
        except Exception as exc:
            from musicops.app.service import TaskCancelled
            if isinstance(exc, TaskCancelled): raise
            proposal = {'status': 'FAILED', 'message': 'AI 辅助未完成：' + str(getattr(exc, 'error_class', type(exc).__name__))}
        with service.mutex:
            current = service.store.get('evidence', 'workflow:' + job_id)
            if not current or current.get('hidden') or review_revision(current) != revision:
                results.append({'job_id': job_id, 'status': 'STALE', 'message': '任务已改变，本次建议未保存'}); continue
            proposal.update(revision=revision, created_at=utcnow(), requires_review=True)
            service.workflows.put(current, ai_assistance=proposal)
        applied=False
        if args.get('apply_verified') is True and proposal.get('status')=='READY':
            try:
                if proposal.get('kind')=='retry':
                    from musicops.app.workflow_actions import retry
                    retry(service.workflows,current);applied=True
                elif proposal.get('kind')=='metadata' and proposal.get('mapped_count')==len(proposal.get('metadata') or []) and proposal.get('mapped_count',0)>0:
                    service.workflows.review(job_id,'edit_metadata',metadata=proposal['metadata'],expected_revision=revision);applied=True
                elif proposal.get('kind')=='candidate' and any(c.get('candidate_id')==proposal.get('candidate_id') and c.get('identity_exact') for c in current.get('choices',[])):
                    service.workflows.review(job_id,proposal['candidate_id'],expected_revision=revision);applied=True
                elif proposal.get('kind')=='search' and current.get('review_kind')=='search':
                    service.workflows.review(job_id,'research',query=proposal['search_query'],expected_revision=revision);applied=True
            except (ValueError,PolicyBlocked):pass
        if applied:
            updated=service.store.get('evidence','workflow:'+job_id)
            if updated:service.workflows.put(updated,ai_assistance={**proposal,'status':'APPLIED','requires_review':False})
        results.append({'job_id': job_id, 'label': job.get('label', ''), **{k: proposal.get(k) for k in ('status', 'message', 'kind')},
                        **({'status':'APPLIED','message':'可靠证据已核对并提交，继续后续处理'} if applied else {})})
    service.store.event('workflow_ai_assistance', {'selected_count': len(ids), 'ready': sum(r['status'] == 'READY' for r in results), 'music_writes': 0})
    return {'status': 'OK', 'results': results, 'requires_review': any(r['status'] not in ('APPLIED','SKIPPED') for r in results), 'music_writes': 0,
            'message': f"已辅助核对 {len(results)} 项；已提交 {sum(r['status']=='APPLIED' for r in results)} 项，其余请打开核对窗口检查"}
