import sqlite3
from pathlib import Path
import threading
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace
import httpx
import pytest
from test_workflows import service
from test_pipeline_progress import add


def test_reserved_running_stage_is_never_scheduled_again(service):
    add(service,'reserved','running','album')
    assert service.workflows.next_job() is None
    add(service,'ready','searching','search')
    assert service.workflows.next_job()['job_id']=='ready'


def test_finishing_ai_cannot_resurrect_concurrently_deleted_job(service,monkeypatch):
    from musicops.app.review_queue import finish
    row=add(service,'album','needs_review','album')
    service.store.put('evidence','ai-review:test',dict(job_id='album',state='running'))
    read=threading.Event();release=threading.Event();original=service.store.get
    def get(kind,key):
        result=original(kind,key)
        if key=='workflow:album' and threading.current_thread().name=='finishing-ai':
            read.set();assert release.wait(3)
        return result
    monkeypatch.setattr(service.store,'get',get)
    finishing=threading.Thread(name='finishing-ai',target=finish,args=(service,dict(args={'_review_queue_id':'ai-review:test'},state='completed')))
    deleting=threading.Thread(target=service.dispatch,args=('bulk_workflows',{'action':'delete','job_ids':['album']}))
    finishing.start();assert read.wait(2)
    deleting.start();deleting.join(.05);release.set();finishing.join(3);deleting.join(3)
    assert not finishing.is_alive() and not deleting.is_alive()
    saved=original('evidence','workflow:album')
    assert saved['hidden'] is True and saved['ai_review_state']=='completed'


def test_slow_go_start_does_not_block_browser_engine(service,tmp_path,monkeypatch):
    from musicops.clients.managed_services import ManagedServices
    import musicops.clients.managed_services as engines
    import musicops.clients.process_owner as ownership
    entered=threading.Event();release=threading.Event()
    for name in ('go-music-dl','flaresolverr'):
        path=tmp_path/'engines'/name/(name+'.exe');path.parent.mkdir(parents=True);path.write_bytes(b'synthetic-only')
    environments=[]
    class Process:
        def __init__(self,*_,**options):self.stopped=False;environments.append(options['env'])
        def poll(self):return 0 if self.stopped else None
    class Owner:
        def __init__(self,process):self.process=process
        def close(self):self.process.stopped=True
    def response(request):
        if request.url.path.endswith('/healthz'):
            entered.set();assert release.wait(3)
        return httpx.Response(200,json={})
    client=httpx.Client
    monkeypatch.setattr(engines.subprocess,'Popen',Process)
    monkeypatch.setattr(ownership,'ProcessOwner',Owner)
    monkeypatch.setattr(engines.httpx,'Client',lambda **kw:client(transport=httpx.MockTransport(response),**kw))
    manager=ManagedServices(tmp_path/'profile',SimpleNamespace(get=lambda *_:None),root=tmp_path/'engines')
    manager.default_storage=lambda:str(tmp_path/'MusicOps/work')
    try:
        with ThreadPoolExecutor(2) as pool:
            go=pool.submit(manager.ensure,'go-music-dl');assert entered.wait(2)
            try:
                browser=pool.submit(manager.ensure,'flaresolverr')
                assert browser.result(timeout=1)[0].startswith('http://127.0.0.1:')
            finally:release.set()
            assert go.result(timeout=2)[0].endswith('/music')
            assert all(Path(env['TEMP']).is_relative_to(tmp_path/'MusicOps/work') and env['TEMP']==env['TMP']==env['TMPDIR'] for env in environments)
    finally:release.set();manager.close()


def test_sqlite_connection_is_closed_when_initial_pragma_fails(tmp_path,monkeypatch):
    from musicops.state.store import StateStore
    class Connection:
        closed=False
        def execute(self,*_):raise sqlite3.OperationalError('synthetic locked database')
        def rollback(self):pass
        def close(self):self.closed=True
    connection=Connection();store=StateStore.__new__(StateStore);store.path=tmp_path/'state.sqlite'
    monkeypatch.setattr(sqlite3,'connect',lambda *_,**__:connection)
    with pytest.raises(sqlite3.OperationalError):
        with store.connection():pass
    assert connection.closed


def test_ai_pending_query_uses_partial_queue_index(service):
    with service.store.connection() as connection:
        rows=connection.execute("EXPLAIN QUERY PLAN SELECT payload FROM evidence WHERE id LIKE 'ai-review:%' AND json_extract(payload,'$.state')='pending' ORDER BY updated_at,id LIMIT 1").fetchall()
    assert 'ai_review_pending' in str([tuple(row) for row in rows])


def test_qbt_connection_test_does_not_wait_for_paused_music_mutex(service,monkeypatch):
    import musicops.clients.qbt as qbt
    monkeypatch.setattr(qbt,'readonly_preflight',lambda *_:dict(status='AUTH_REQUIRED',reason='login required'))
    service.paused=True;service.active='music-task'
    with ThreadPoolExecutor(1) as pool,service.mutex:
        assert pool.submit(service.dispatch,'test_qbt_connection',{}).result(timeout=2)['status']=='AUTH_REQUIRED'
    assert service.paused and service.active=='music-task'


def test_owned_service_start_honors_worker_shutdown(service):
    from musicops.app.service import TaskCancelled
    assert service.stop.is_set()
    with pytest.raises(TaskCancelled):service._managed_checkpoint('managed_service_start')


def test_paused_music_start_releases_engine_lock_before_waiting(service,tmp_path,monkeypatch):
    from musicops.clients.managed_services import ManagedServices
    ready=threading.Event();finished=threading.Event();results=[]
    service.stop.clear();service.paused=True;service.active='engine-start'
    service.store.put('tasks',service.active,dict(task_id=service.active,state='running'))
    def checkpoint(phase):
        if phase=='managed_service_ready' and threading.current_thread() is service.thread:ready.set()
        service._managed_checkpoint(phase)
    manager=ManagedServices(tmp_path/'engines-profile',SimpleNamespace(),checkpoint)
    def starting(*_,**__):
        manager.checkpoint('managed_service_start')
        return 'http://127.0.0.1:12345','synthetic-token'
    monkeypatch.setattr(manager,'_ensure_locked',starting)
    def music():
        results.append(manager.ensure('flaresolverr'));finished.set()
    service.thread=threading.Thread(target=music)
    try:
        service.thread.start();assert ready.wait(2)
        with ThreadPoolExecutor(1) as pool:
            assert pool.submit(manager.ensure,'flaresolverr').result(timeout=1)[0].startswith('http://127.0.0.1:')
        assert not finished.is_set(), 'Music must remain paused before using the ready engine'
        service.paused=False;service.thread.join(3)
        assert finished.is_set() and len(results)==1
    finally:
        service.paused=False;service.thread.join(3);service.stop.set();manager.close()


def test_startup_while_paused_still_honors_task_cancellation(service):
    from musicops.app.service import TaskCancelled
    service.stop.clear();service.paused=True;service.active='cancelled-engine-start'
    service.thread=threading.current_thread()
    service.store.put('tasks',service.active,dict(task_id=service.active,state='running',cancel_requested=True))
    try:
        with pytest.raises(TaskCancelled):service._managed_checkpoint('managed_service_start')
    finally:service.stop.set()
