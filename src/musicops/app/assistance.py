"""Grounded AI proposals for an explicit, editable review. Never apply on generation."""
from musicops.app.recognition import RecognitionFallback
from musicops.app.workflow_actions import selection
from musicops.config.schema import digest
from musicops.domain.identity import strict_text
from musicops.state.store import utcnow
from musicops.clients.http import PolicyBlocked


def review_revision(job):
    tracks = job.get('prepared') or job.get('targets') or job.get('old_files') or []
    return digest({'stage': job.get('stage'), 'kind': job.get('review_kind'), 'hidden': bool(job.get('hidden')),
                   'tracks': [{k: r.get(k) for k in ('raw_title', 'artists', 'album', 'duration', 'file_id')} for r in tracks],
                   'choices': [{k: c.get(k) for k in ('candidate_id', 'source', 'title', 'artists', 'release', 'media')} for c in job.get('choices', [])]})


def _metadata(service, job, cfg):
    rows = job.get('prepared') or job.get('targets') or job.get('old_files') or []
    if not rows: return {'status': 'NEEDS_INPUT', 'message': '没有曲目资料，请手动填写或重新识别'}
    first = rows[0]
    found = service._recognize_metadata({'title': first.get('raw_title', ''), 'artists': first.get('artists', []),
        'album': first.get('album', ''), 'duration': first.get('duration')}, cfg, force=True)
    selected = found.get('selected_candidate')
    proposal = {'status': 'NEEDS_INPUT', 'kind': 'metadata', 'message': 'AI 未找到足够明确的真实目录证据，请手动选择或填写',
                'catalog_choices': found.get('candidates', []), 'lookup_links': found.get('lookup_links', []),
                'reason': found.get('ai_fallback', {}).get('reason', '')}
    if not selected: return proposal
    source_id = 'release:' + selected['release_id'] if selected.get('entity_kind') == 'recording' and selected.get('release_id') else selected.get('source_id')
    resolved = service._catalog_resolver(cfg).resolve_candidate(selected['source'], source_id).get('candidate')
    if not resolved: return proposal
    observed = resolved.get('tracklist', [])
    if not observed and selected.get('entity_kind') == 'recording': observed = [selected]
    metadata = []; changed = 0
    for row in rows:
        service._checkpoint('ai_review_mapping')
        matches = [track for track in observed if strict_text(track.get('raw_title') or track.get('title')) == strict_text(row.get('raw_title'))
                   and (not row.get('artists') or {strict_text(a) for a in row['artists']} == {strict_text(a) for a in track.get('artists', [])})
                   and (not row.get('duration') or track.get('duration') and abs(row['duration'] - track['duration']) <= 2)]
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
        context['disambiguation'] = str(candidate.get('media', {}))[:500]
        contexts.append(context)
    query = RecognitionFallback._query({'album': first.get('album', ''), 'artists': first.get('albumartist') or first.get('artists', []), 'title': ''})
    result = provider.recognize_candidate(query, contexts)
    ready = result['candidate_id'] is not None and result['confidence'] >= .9 and not result['ambiguous']
    return {'status': 'READY' if ready else 'NEEDS_INPUT', 'kind': 'candidate',
            'candidate_id': result['candidate_id'] if ready else None, 'confidence': result['confidence'],
            'reason': result['reason'], 'observed_count': len(contexts),
            'message': '已推荐现有候选，请核对艺人、发行与音质后采用' if ready else '候选仍有歧义，请在核对窗口选择发行'}


def assist_workflows(service, args, cfg):
    ids = selection(args.get('job_ids'), 20)
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
            if kind == 'candidate' and job.get('choices'):
                proposal = _candidate(service, job, cfg, provider)
            elif kind == 'metadata':
                proposal = _metadata(service, job, cfg)
            elif kind in {'search', 'error', 'interrupted'} or job['stage'] in {'failed', 'skipped'}:
                rows = job.get('targets') or job.get('old_files') or []; first = rows[0] if rows else {}
                found = service._recognize_metadata({'title': '', 'artists': first.get('artists', []), 'album': first.get('album', '')}, cfg, force=True)
                selected = found.get('selected_candidate')
                query = (' '.join(selected.get('artists', [])) + ' ' + str(selected.get('album') or selected.get('title') or '')).strip() if selected else ''
                proposal = {'status': 'READY' if query and len(query) <= 300 else 'NEEDS_INPUT', 'kind': 'search',
                            'search_query': query if len(query) <= 300 else '',
                            'reason': found.get('ai_fallback', {}).get('reason', ''),
                            'message': '已生成真实目录支持的搜索词，可编辑后重新搜索' if query else '请检查各站状态，修改搜索词或站点后重试'}
            else:
                proposal = {'status': 'NEEDS_INPUT', 'kind': kind, 'message': '此问题需要手动选择封面、曲序、歌词服务或发行版本，请打开核对窗口'}
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
        results.append({'job_id': job_id, 'label': job.get('label', ''), **{k: proposal.get(k) for k in ('status', 'message', 'kind')}})
    service.store.event('workflow_ai_assistance', {'selected_count': len(ids), 'ready': sum(r['status'] == 'READY' for r in results), 'music_writes': 0})
    return {'status': 'OK', 'results': results, 'requires_review': True, 'music_writes': 0,
            'message': f"已辅助核对 {len(results)} 项；请打开任务核对窗口检查并采用建议"}
