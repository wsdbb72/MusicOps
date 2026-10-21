import copy
import threading
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace

from test_search_resilience import service, make_job


def seed(service, count, stage='identifying'):
    template=make_job(service)
    with service.store.connection() as connection:
        connection.execute('DELETE FROM evidence WHERE id=?',('workflow:'+template['job_id'],))
    for number in range(count):
        row=copy.deepcopy(template)
        row.update(job_id=f'parallel-{number}',stage=stage)
        service.workflows.put(row)


def test_bounded_wave_claims_each_album_once_and_propagates_task_context(service,monkeypatch):
    flows=service.workflows;seed(service,8)
    monkeypatch.setattr(flows,'parallel_limit',lambda:4)
    service._task_local.task_id='coordinator'
    # The fixture stops its real scheduler; use a synthetic stage checkpoint.
    monkeypatch.setattr(service,'_checkpoint',lambda phase:None)
    barrier=threading.Barrier(4);seen=[];lock=threading.Lock()
    def identify(job):
        assert service._task_local.task_id=='coordinator'
        with lock:seen.append(job['job_id'])
        barrier.wait(5)
        flows.put(job,stage='completed')
    monkeypatch.setattr(flows,'_identifying',identify)
    assert flows.advance()['parallel_count']==4
    assert len(seen)==len(set(seen))==4
    assert sum(j['stage']=='identifying' for j in flows.rows())==4
    assert flows.advance()['parallel_count']==4
    assert len(seen)==len(set(seen))==8
    assert service._task_local.task_id=='coordinator'
    assert flows.advance()['idle']


def test_mutating_stage_keeps_exclusive_turn(service,monkeypatch):
    flows=service.workflows;seed(service,4)
    row=copy.deepcopy(flows.rows()[0]);row.update(job_id='publication',stage='publishing');flows.put(row)
    monkeypatch.setattr(service,'_checkpoint',lambda phase:None)
    seen=[]
    monkeypatch.setattr(flows,'_publishing',lambda job:(seen.append(job['job_id']),flows.put(job,stage='completed')))
    monkeypatch.setattr(flows,'_identifying',lambda job:seen.append('unsafe-overlap'))
    assert flows.advance()['job_id']=='publication'
    assert seen==['publication']
    assert sum(j['stage']=='identifying' for j in flows.rows())==4


def test_failure_isolated_without_losing_other_claimed_albums(service,monkeypatch):
    flows=service.workflows;seed(service,4)
    monkeypatch.setattr(flows,'parallel_limit',lambda:4)
    monkeypatch.setattr(service,'_checkpoint',lambda phase:None)
    def identify(job):
        if job['job_id']=='parallel-0':raise ValueError('synthetic failure')
        flows.put(job,stage='completed')
    monkeypatch.setattr(flows,'_identifying',identify)
    assert flows.advance()['parallel_count']==4
    stages={j['job_id']:j['stage'] for j in flows.rows()}
    assert stages.pop('parallel-0')=='failed'
    assert set(stages.values())=={'completed'}


def test_search_requests_overlap_across_sites_but_never_on_same_site(service,monkeypatch):
    flows=service.workflows;rows=[]
    for number in range(2):
        row=copy.deepcopy(make_job(service));row['job_id']=f'search-{number}'
        row['config']['workflow']['source_order']=['open.cd','dicmusic'] if number==0 else ['dicmusic','open.cd']
        rows.append(row)
    barrier=threading.Barrier(2);lock=threading.Lock();active=set();overlap=[];calls=[]
    def provider(site,cfg):
        def search(query):
            with lock:
                assert site not in active
                active.add(site);calls.append((site,query))
                if len(active)==2:overlap.append(True)
            if query!='Album':barrier.wait(5)
            with lock:active.remove(site)
            return {'status':'NO_RESOURCE','candidates':[]}
        return SimpleNamespace(search=search)
    monkeypatch.setattr(flows,'provider',provider)
    with ThreadPoolExecutor(max_workers=2) as pool:
        list(pool.map(flows._searching,rows))
    assert overlap and len(calls)==8
    assert all(row['stage']=='completed' for row in rows)
    assert not service.store.list('token_reservations')
    assert not service.store.list('download_tasks')


def test_parallel_selector_respects_parent_pause_and_poll_deadline(service):
    flows=service.workflows;seed(service,4)
    for row,changes in zip(sorted(flows.rows(),key=lambda row:row['job_id']),[
        {'paused':True},{'identification_job_id':'missing-parent'},
        {'next_poll':10**12},{'stage':'identifying','scene':'fill'}]):
        flows.put(row,**changes)
    assert flows.next_job(network_only=True) is None


def test_efficiency_worker_bound(service):
    assert 2<=service.workflows.parallel_limit()<=4


def test_streaming_wave_limits_downloads_and_shares_room_with_search(service,monkeypatch):
    flows=service.workflows;seed(service,5,stage='fallback')
    row=copy.deepcopy(flows.rows()[0]);row.update(job_id='waiting-search',stage='searching');flows.put(row)
    monkeypatch.setattr(flows,'parallel_limit',lambda:4)
    monkeypatch.setattr(service,'_checkpoint',lambda phase:None)
    seen=[];barrier=threading.Barrier(3)
    def stage(job):
        seen.append(job['resume_stage']);barrier.wait(5);flows.put(job,stage='completed')
    monkeypatch.setattr(flows,'_fallback',stage);monkeypatch.setattr(flows,'_searching',stage)
    assert flows.advance()['parallel_count']==3
    assert sorted(seen)==['fallback','fallback','searching']
    assert sum(j['stage']=='fallback' for j in flows.rows())==3


def test_claimed_wave_keeps_safe_shutdown_guard_until_all_peers_finish(service,monkeypatch):
    flows=service.workflows;seed(service,4)
    monkeypatch.setattr(flows,'parallel_limit',lambda:4)
    monkeypatch.setattr(service,'_checkpoint',lambda phase:None)
    service.active='synthetic-coordinator'
    barrier=threading.Barrier(5);release=threading.Event()
    def identify(job):
        barrier.wait(5);assert release.wait(5);flows.put(job,stage='completed')
    monkeypatch.setattr(flows,'_identifying',identify)
    with ThreadPoolExecutor(max_workers=1) as pool:
        future=pool.submit(flows.advance);barrier.wait(5)
        import pytest
        try:
            with pytest.raises(RuntimeError,match='安全退出点'):
                service.dispatch('shutdown_worker',{})
        finally:release.set()
        assert future.result(timeout=5)['parallel_count']==4


def test_paused_claim_does_not_start_provider_or_migration(service,monkeypatch):
    flows=service.workflows;seed(service,1)
    row=flows.rows()[0];flows.put(row,paused=True,stage='running',resume_stage='identifying')
    monkeypatch.setattr(service,'_checkpoint',lambda phase:None)
    monkeypatch.setattr(flows,'_identifying',lambda job:(_ for _ in ()).throw(AssertionError('must stay paused')))
    assert flows._advance_job(row,None)['paused']
    assert flows.rows()[0]['stage']=='identifying'


def test_coordinator_cancellation_cancels_each_claim_before_provider(service,monkeypatch):
    from musicops.app.service import TaskCancelled
    flows=service.workflows;seed(service,4)
    monkeypatch.setattr(flows,'parallel_limit',lambda:4)
    def cancel(phase):raise TaskCancelled('synthetic cancellation')
    monkeypatch.setattr(service,'_checkpoint',cancel)
    monkeypatch.setattr(flows,'_identifying',lambda job:(_ for _ in ()).throw(AssertionError('cancelled stage executed')))
    assert flows.advance()['parallel_count']==4
    assert {job['stage'] for job in flows.rows()}=={'cancelled'}
