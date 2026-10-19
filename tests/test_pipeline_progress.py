import copy
import threading
import time
from concurrent.futures import ThreadPoolExecutor
import pytest

from test_workflows import service


@pytest.mark.parametrize('stage',['publishing','retiring'])
def test_verified_album_publication_precedes_search_and_streaming_backlog(service,stage):
    for i in range(25):add(service,'backlog'+str(i),'fallback','album')
    ready=add(service,'ready-to-publish',stage,'album')
    assert service.workflows.next_job()['job_id']==ready['job_id']


def add(service, identity, stage, task_type, **extra):
    row = dict(kind='workflow',job_id=identity,batch_id='batch',scene='playlist',
               config=copy.deepcopy(service.config.current()['values']),stage=stage,
               task_type=task_type,targets=[],old_files=[],label='Album',progress=0,message='',**extra)
    service.workflows.put(row)
    return row


def chain(service, stage='requesting'):
    add(service,'identity','completed','identification',search_job_id='search')
    add(service,'search','completed','search',identification_job_id='identity',processing_job_id='process')
    return add(service,'process',stage,'album',identification_job_id='identity',search_job_id='search')


def test_download_and_import_advance_before_older_search_backlog(service,monkeypatch):
    for i in range(25):add(service,'older'+str(i),'searching','search')
    chain(service)
    calls=[]
    for stage,next_stage in [('requesting','downloading'),('downloading','metadata'),('metadata','completed')]:
        def step(job,stage=stage,next_stage=next_stage):
            calls.append(stage);service.workflows.put(job,stage=next_stage)
        monkeypatch.setattr(service.workflows,'_'+stage,step)
    for _ in range(3):service.workflows.advance()
    assert calls==['requesting','downloading','metadata']
    assert service.workflows.next_job()['job_id'].startswith('older')


def test_waiting_download_does_not_prevent_other_work(service):
    job=chain(service,'downloading');service.workflows.put(job,next_poll=time.time()+120)
    add(service,'ready','searching','search')
    assert service.workflows.next_job()['job_id']=='ready'


def test_album_overview_uses_current_successor_and_selection(service):
    chain(service,'needs_review')
    overview=service.workflows.list(view='albums',state='review')
    assert [r['job_id'] for r in overview['jobs']]==['process']
    assert len(overview['jobs'][0]['chain'])==3 and overview['finished']==0
    assert service.workflows.list(view='albums',state='completed')['total']==0
    assert service.workflows.list(view='albums',state='review',selection=True)['job_ids']==['process']
    assert service.workflows.list(view='steps',state='completed')['total']==2


def test_hidden_successor_never_resurrects_completed_prerequisite(service):
    job=chain(service);service.workflows.put(job,hidden=True)
    assert service.workflows.list(view='albums')['total']==0
    assert service.workflows.list(view='steps')['total']==2


def test_old_configuration_snapshot_is_upgraded_without_changing_paths_or_refs(service,monkeypatch):
    job=add(service,'old','fallback','album');job['config'].pop('streaming');job['config'].pop('browser_solver')
    original=copy.deepcopy(job['config']);service.workflows.put(job)
    seen=[]
    def run(current):
        assert 'streaming' in current['config'] and 'browser_solver' in current['config']
        assert current['config']['paths']==original['paths'] and current['config']['qbt']==original['qbt']
        seen.append(current['job_id']);service.workflows.put(current,stage='needs_review')
    monkeypatch.setattr(service.workflows,'_fallback',run)
    service.workflows.advance()
    assert seen==['old']


def test_solver_test_is_independent_of_queue_mutex_and_pause(service,monkeypatch):
    service.paused=True;service.active='busy-music-task'
    monkeypatch.setattr(service.engines,'ensure',lambda *_:('http://127.0.0.1:8191','synthetic-only'))
    from musicops.clients.browser_solver import FlareSolverrClient
    monkeypatch.setattr(FlareSolverrClient,'check',lambda _:dict(status='OK',message='ready'))
    with ThreadPoolExecutor(1) as pool,service.mutex:
        future=pool.submit(service.dispatch,'test_browser_solver',{'settings':{'managed':True}})
        assert future.result(timeout=2)['status']=='OK'
    assert service.paused and service.active=='busy-music-task'


def test_real_fallback_with_legacy_snapshot_reports_source_failure_not_keyerror(service,monkeypatch):
    from musicops.clients.http import HttpError
    import musicops.providers.netease_match as match
    def unavailable(*_,**__):raise HttpError('AUTH_REQUIRED','synthetic login required')
    monkeypatch.setattr(match,'resolve_netease_target',unavailable)
    job=add(service,'old-fallback','fallback','album')
    job['config'].pop('streaming')
    job['targets']=[dict(target_id='track',raw_title='Song',artists=['Artist'],album='Album',duration=120)]
    service.workflows.put(job);service.workflows.advance()
    saved=service.store.get('evidence','workflow:old-fallback')
    assert saved['stage']=='needs_review' and saved.get('error_class')!='KeyError'
    assert next(iter(saved['streaming_outcomes'].values()))==[{'source':'netease','status':'AUTH_REQUIRED'}]


def test_whole_queue_query_is_literal_and_filters_before_pagination(service):
    for i in range(205):add(service,'old-'+str(i),'searching','search')
    row=add(service,'matching','requesting','album');service.workflows.put(row,label='Artist 100%_Album')
    for view in ('steps','albums'):
        result=service.workflows.list(view=view,query='100%_')
        assert result['total']==1 and result['jobs'][0]['job_id']=='matching'
        assert service.workflows.list(view=view,query='not here')['total']==0


def test_ai_waiting_never_hides_connection_failure(service):
    row=add(service,'failed','needs_review','album',ai_review_state='pending')
    service.workflows.put(row,message='qBittorrent connection unavailable')
    shown=service.workflows.list(view='albums')['jobs'][0]
    assert shown['message']=='qBittorrent connection unavailable' and shown['ai_review_message']
