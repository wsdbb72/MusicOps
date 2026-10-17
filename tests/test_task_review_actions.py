import copy
import pytest
from test_task_controls import controls, album, activity, read_job, control
from musicops.app.assistance import assist_workflows, review_revision
from musicops.app.recognition import RecognitionFallback
from musicops.clients.http import PolicyBlocked
from musicops.app.search_status import search_album


def test_delete_removes_descendant_rows_without_erasing_music_or_download_evidence(controls):
    service = controls
    parent = album(service, stage='completed')
    child = album(service, 'search', stage='searching', task_type='search', identification_job_id=parent['job_id'])
    process = album(service, 'process', stage='running', task_type='album', search_job_id=child['job_id'])
    unrelated = album(service, 'other')
    for table in ('download_tasks', 'transactions'):
        service.store.put(table, 'preserve', {'sentinel': table})
    result = control(service, 'delete', [parent['job_id']])
    assert result['succeeded'] == 1 and result['results'][0]['affected_count'] == 3
    assert {j['job_id'] for j in service.workflows.rows()} == {unrelated['job_id']}
    assert service.workflows.list()['total'] == 1
    assert read_job(service, 'process')['cancel_requested']
    service.workflows.put(process, stage='validating')
    assert read_job(service, 'process')['hidden'] and read_job(service, 'process')['stage'] == 'cancelled'
    for table in ('download_tasks', 'transactions'):
        assert service.store.get(table, 'preserve')['sentinel'] == table
    with pytest.raises(ValueError): service.dispatch('get_workflow', {'job_id': parent['job_id']})


def test_delete_running_activity_retains_request_cancellation_and_cannot_reappear(controls):
    service = controls; job_id, task_id = activity(service)
    task = service.store.get('tasks', task_id); task['state'] = 'running'; service.store.put('tasks', task_id, task)
    stale = read_job(service, job_id)
    assert control(service, 'delete', [job_id])['succeeded'] == 1
    assert service.store.get('tasks', task_id)['cancel_requested']
    service.workflows.put(stale, stage='completed', message='old callback')
    assert not service.workflows.list()['jobs'] and not service.workflows.next_job()


def test_retry_search_uses_current_configuration_and_preserves_id(controls):
    service = controls
    row = album(service, stage='needs_review', task_type='search', review_kind='search', resume_stage='searching', choices=[{'candidate_id':'stale'}], next_poll=99999999999)
    current = service.config.current(); cfg = copy.deepcopy(current['values']); cfg['providers']['orpheus']['enabled'] = True; service.config.save(cfg, current['version'])
    assert control(service, 'retry', [row['job_id']])['succeeded'] == 1
    retried = read_job(service, row['job_id'])
    assert retried['stage'] == 'searching' and retried['config']['providers']['orpheus']['enabled']
    assert retried['choices'] == [] and retried['retry_count'] == 1 and retried['next_poll'] == 0
    assert control(service, 'retry', [row['job_id']])['skipped'] == 1


def test_search_retry_and_delete_do_not_treat_identification_parent_as_descendant(controls):
    service = controls
    parent = album(service, 'identify-parent', stage='completed', task_type='identification', search_job_id='search-child')
    child = album(service, 'search-child', stage='needs_review', task_type='search', review_kind='search', identification_job_id=parent['job_id'])
    assert control(service, 'retry', [child['job_id']])['succeeded'] == 1
    result = control(service, 'delete', [child['job_id']])
    assert result['results'][0]['affected_count'] == 1
    assert not read_job(service, parent['job_id']).get('hidden')
    assert read_job(service, parent['job_id'])['stage'] == 'completed'


def test_retry_downloading_reuses_download_and_does_not_discard_selected_resource(controls):
    service = controls; resource = {'candidate_id': 'observed'}
    row = album(service, stage='failed', task_type='album', review_kind='error', resume_stage='downloading', selected=resource)
    service.store.put('download_tasks', row['job_id'], {'state': 'DOWNLOADING', 'hash': 'existing'})
    assert control(service, 'retry', [row['job_id']])['succeeded'] == 1
    assert read_job(service, row['job_id'])['selected'] == resource
    assert service.store.get('download_tasks', row['job_id'])['hash'] == 'existing'


def test_retry_mixed_selection_reports_manual_and_transaction_blocks(controls):
    service = controls
    album(service, 'search', stage='failed', task_type='search', review_kind='error')
    album(service, 'metadata', stage='needs_review', review_kind='metadata')
    album(service, 'publish', stage='failed', review_kind='error', resume_stage='publishing')
    album(service, 'finished', stage='completed')
    result = control(service, 'retry', ['search', 'metadata', 'publish', 'finished', 'missing'])
    assert (result['succeeded'], result['skipped'], result['failed']) == (1, 3, 1)
    assert '核对窗口' in result['results'][1]['message'] and '事务' in result['results'][2]['message']


def test_activity_retry_creates_fresh_request_once_with_current_settings(controls):
    service = controls; job_id, task_id = activity(service)
    task = service.store.get('tasks', task_id); task['state'] = 'failed'; service.store.put('tasks', task_id, task)
    service.workflows.put(read_job(service, job_id), stage='failed')
    first = control(service, 'retry', [job_id]); new_id = first['results'][0]['retry_job_id']
    assert new_id != job_id and read_job(service, new_id)['stage'] == 'queued'
    assert control(service, 'retry', [job_id])['skipped'] == 1


def test_manual_search_review_requeues_with_explicit_keyword_and_sites(controls):
    service = controls
    current = service.config.current(); cfg = copy.deepcopy(current['values']); cfg['providers']['orpheus']['enabled'] = True; service.config.save(cfg, current['version'])
    row = album(service, stage='needs_review', task_type='search', review_kind='candidate', choices=[{'candidate_id': 'observed'}])
    revision = review_revision(row)
    service.dispatch('review_workflow', {'job_id': row['job_id'], 'choice': 'research', 'query': 'My own keyword', 'sites': ['orpheus'], 'expected_revision': revision})
    saved = read_job(service, row['job_id'])
    assert saved['stage'] == 'searching' and saved['search_query'] == 'My own keyword' and saved['search_sites'] == ['orpheus']
    with pytest.raises(PolicyBlocked, match='改变'):
        service.dispatch('review_workflow', {'job_id': row['job_id'], 'choice': 'research', 'query': 'Again', 'sites': ['orpheus'], 'expected_revision': revision})


def test_changed_library_blocks_retry(controls):
    service = controls; row = album(service, stage='failed', task_type='search')
    row['config']['paths']['work'] += '-old'; service.workflows.put(row)
    result = control(service, 'retry', [row['job_id']])
    assert result['skipped'] == 1 and read_job(service, row['job_id'])['stage'] == 'failed'


def test_bounded_empty_search_fallback_deduplicates_results_and_does_not_retry_failed_sites():
    calls = []
    class Provider:
        def search(self, query):
            calls.append(query)
            return {'status': 'NO_MATCH', 'candidates': []} if len(calls) == 1 else {'status': 'OK', 'candidates': [{'source': 'orpheus', 'candidate_id': 'x'}, {'source': 'orpheus', 'candidate_id': 'x'}]}
    result = search_album(Provider, 'Artist Album', 'Album')
    assert calls == ['Artist Album', 'Album'] and len(result['candidates']) == 1
    for failure in ('AUTH_FAILED', 'RATE_LIMITED', 'NETWORK_ERROR'):
        calls.clear()
        class Failed:
            def search(self, query): calls.append(query); return {'status': failure, 'candidates': []}
        assert search_album(Failed, 'Artist Album', 'Album')['status'] == failure and len(calls) == 1


@pytest.mark.parametrize('suffix',['2020 Remastered','Deluxe Edition','Live','周年版'])
def test_extra_edition_markers_remain_manual_candidates(suffix):
    from musicops.app.album_search import exact_album
    candidate={'source':'open.cd','title':'Artist - Album ('+suffix+')','artists':['Artist']}
    assert not exact_album(candidate,'Album',['Artist'])


def test_ai_candidate_assistance_recommends_existing_resource_without_confirming_or_downloading(controls, monkeypatch):
    service = controls; contexts = []
    class AI:
        def recognize_candidate(self, query, candidates):
            contexts.append((query, candidates)); return {'candidate_id': 'observed', 'confidence': .98, 'ambiguous': False, 'reason': 'Observed identity matches'}
    monkeypatch.setattr(RecognitionFallback, '_provider', lambda self: (AI(), None))
    row = album(service, stage='needs_review', task_type='search', review_kind='candidate',
                detail='private-path must not be sent', choices=[{'candidate_id': 'observed', 'source': 'orpheus', 'title': 'Synthetic Album', 'artists': ['Synthetic Artist'], 'url': 'https://private.example/?passkey=secret', 'media': {'format': 'FLAC'}}])
    result = assist_workflows(service, {'job_ids': [row['job_id']]}, service.config.current()['values'])
    saved = read_job(service, row['job_id'])
    assert result['music_writes'] == 0 and saved['stage'] == 'needs_review' and not saved.get('processing_job_id')
    assert saved['ai_assistance']['candidate_id'] == 'observed' and saved['ai_assistance']['requires_review']
    assert all(value not in str(contexts) for value in ('private-path', 'passkey', 'private.example', 'config'))


def test_ai_metadata_maps_fetched_tracklist_and_keeps_original_rows_editable(controls, monkeypatch):
    service = controls
    monkeypatch.setattr(RecognitionFallback, '_provider', lambda self: (object(), None))
    track = {'title': 'Synthetic Song', 'artists': ['Synthetic Artist'], 'duration': 120}
    release = {'source': 'musicbrainz', 'source_id': 'release:observed', 'entity_kind': 'release', 'title': 'Observed Album', 'artists': ['Synthetic Artist'], 'tracklist': [track]}
    monkeypatch.setattr(service, '_recognize_metadata', lambda *a, **k: {'selected_candidate': release, 'candidates': [release], 'ai_fallback': {'confidence': .99}})
    class Resolver:
        def resolve_candidate(self, source, source_id): return {'candidate': release}
    monkeypatch.setattr(service, '_catalog_resolver', lambda cfg: Resolver())
    row = album(service, stage='needs_review', review_kind='metadata'); row['targets'][0]['album'] = ''; service.workflows.put(row)
    assist_workflows(service, {'job_ids': [row['job_id']]}, service.config.current()['values'])
    saved = read_job(service, row['job_id'])
    assert saved['ai_assistance']['metadata'][0]['album'] == 'Observed Album'
    assert saved['targets'][0]['album'] == '' and saved['stage'] == 'needs_review'
    public = service.dispatch('get_workflow', {'job_id': row['job_id']})['job']
    assert public['ai_assistance']['revision'] == public['review_revision']


def test_stale_ai_result_is_not_applied_to_deleted_task(controls, monkeypatch):
    service = controls; row = album(service, stage='needs_review', review_kind='candidate', choices=[{'candidate_id':'x','source':'orpheus','title':'Album','artists':['Artist']}])
    class AI:
        def recognize_candidate(self, *args):
            control(service, 'delete', [row['job_id']]); return {'candidate_id': 'x', 'confidence': 1, 'ambiguous': False, 'reason': 'stale'}
    monkeypatch.setattr(RecognitionFallback, '_provider', lambda self: (AI(), None))
    result = assist_workflows(service, {'job_ids': [row['job_id']]}, service.config.current()['values'])
    assert result['results'][0]['status'] == 'STALE' and not read_job(service, row['job_id']).get('ai_assistance')


def test_ai_web_metadata_leads_are_editable_without_claiming_confirmed_release(controls, monkeypatch):
    service=controls;monkeypatch.setattr(RecognitionFallback,'_provider',lambda self:(object(),None))
    query={'title':'Synthetic Song','artists':['Synthetic Artist'],'album':'Observed Album'}
    monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**k:{'selected_candidate':None,'candidates':[],
        'lookup_links':[{'source':'discogs','url':'https://www.discogs.com/release/123'}],
        'ai_fallback':{'web_queries':[query],'web_sources':[{'url':'https://www.discogs.com/release/123'}],'web_reason':'Observed web source'}})
    row=album(service,stage='needs_review',review_kind='metadata');row['targets'][0]['album']='';service.workflows.put(row)
    assist_workflows(service,{'job_ids':[row['job_id']]},service.config.current()['values'])
    saved=read_job(service,row['job_id']);proposal=saved['ai_assistance']
    assert proposal['status']=='NEEDS_REVIEW' and proposal['web_queries']==[query] and proposal['requires_review']
    assert saved['targets'][0]['album']=='' and saved['stage']=='needs_review' and not saved.get('selected')
