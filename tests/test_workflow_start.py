"""Preparation is visible and cancellable before any long library scan."""
import copy
import threading
import time
from pathlib import Path
from types import SimpleNamespace

import pytest

from musicops.app.service import AppService,TaskCancelled
from musicops.clients.http import PolicyBlocked
from musicops.library.index import index_library


@pytest.fixture
def preparation(tmp_path):
    service=AppService(tmp_path/'profile',offline=False)
    service.paused=True
    current=service.config.current();cfg=copy.deepcopy(current['values'])
    cfg['paths']['library']=str(tmp_path/'music');Path(cfg['paths']['library']).mkdir()
    service.config.save(cfg,current['version'])
    yield service,cfg
    service.stop.set();service.thread.join(3)


@pytest.mark.parametrize('scene',['playlist','complete','upgrade'])
def test_start_acceptance_exposes_preparation_before_worker_or_scan(preparation,monkeypatch,scene):
    service,cfg=preparation
    monkeypatch.setattr(service,'_index_real_library',lambda *a,**k:pytest.fail('acceptance must not scan'))
    response=service.dispatch('start_workflow',{'scene':scene,'target_ids':['target']},request_id='start-fixture')
    jobs=service.dispatch('list_workflows')
    assert response['accepted'] and response['job_id']==jobs['jobs'][0]['job_id']
    assert jobs['jobs'][0]['job_kind']=='batch' and jobs['jobs'][0]['stage']=='preparing'
    assert jobs['preparing_count']==jobs['active']==1 and jobs['finished']==0
    assert service.workflows.next_job() is None
    assert service.dispatch('start_workflow',{'scene':scene,'target_ids':['target']},request_id='start-fixture')['idempotent']
    with pytest.raises(PolicyBlocked,match='准备或运行'):
        service.dispatch('start_workflow',{'scene':scene,'target_ids':['target']})


def test_scan_progress_and_scan_error_are_visible_in_workflow_list(preparation,monkeypatch):
    service,cfg=preparation;entered=threading.Event();resume=threading.Event()
    def blocked_scan(root,**kwargs):
        kwargs['progress']({'scanned_count':37,'cached_count':12,'current_directory':'Artist/Album','phase':'scanning'})
        entered.set();assert resume.wait(3)
        raise OSError('read failed')
    monkeypatch.setattr('musicops.library.index.index_library',blocked_scan)
    accepted=service.dispatch('start_workflow',{'scene':'complete'});service.paused=False
    try:
        assert entered.wait(3)
        row=service.dispatch('list_workflows')['jobs'][0]
        assert row['stage']=='preparing' and row['scanned_count']==37 and row['cached_count']==12
        assert '37' in row['message'] and 'Artist/Album' in row['detail']
    finally:resume.set()
    for _ in range(100):
        result=service.dispatch('get_task',{'task_id':accepted['task_id']})['task']
        if result['state']=='failed':break
        time.sleep(.02)
    assert result['state']=='failed'
    row=service.dispatch('list_workflows')['jobs'][0]
    assert row['stage']=='failed' and 'OSError' in row['message'] and service.workflows.next_job() is None


def test_cancel_during_scan_stops_before_album_creation(preparation,monkeypatch):
    service,cfg=preparation
    def scan(root,**kwargs):
        batch=service.dispatch('list_workflows')['jobs'][0]
        service.dispatch('cancel_workflow',{'job_id':batch['job_id']})
        kwargs['progress']({'scanned_count':0,'cached_count':0,'current_directory':'','phase':'scanned'})
        pytest.fail('cancelled scan continued')
    monkeypatch.setattr('musicops.library.index.index_library',scan)
    accepted=service.dispatch('start_workflow',{'scene':'complete'});task=service.store.get('tasks',accepted['task_id'])
    with pytest.raises(TaskCancelled,match='取消'):service._execute('start_workflow',task['args'],cfg)
    assert service.workflows.rows()==[] and service.dispatch('list_workflows')['jobs'][0]['stage']=='cancelled'


def test_upgrade_start_never_hashes_whole_library(preparation,monkeypatch):
    import mutagen
    service,cfg=preparation;track=Path(cfg['paths']['library'])/'Song.flac';track.write_bytes(b'fixture')
    monkeypatch.setattr(mutagen,'File',lambda *a,**k:SimpleNamespace(tags={'title':['Song'],'album':['Album'],'artist':['Artist']},info=SimpleNamespace(length=5),pictures=[]))
    monkeypatch.setattr('musicops.app.pipeline.sha256_file',lambda *a:pytest.fail('prepare must not hash original audio'))
    result=service.workflows.start({'scene':'upgrade'},cfg)
    assert result['album_count']==1
    assert service.workflows.rows()[0]['stage']=='snapshotting'
    assert service.dispatch('list_workflows')['finished']==0


def test_metadata_cache_reuses_unchanged_audio_but_refreshes_sidecars(tmp_path,monkeypatch):
    import mutagen
    track=tmp_path/'Song.flac';track.write_bytes(b'fixture');calls=[]
    monkeypatch.setattr(mutagen,'File',lambda *a,**k:calls.append(a[0]) or SimpleNamespace(tags={'title':['Song'],'artist':['Artist']},info=SimpleNamespace(length=5),pictures=[]))
    first=index_library(tmp_path,metadata_only=True)[0];cache={first['file_id']:first};calls.clear()
    lyric=track.with_suffix('.lrc');lyric.write_text('[00:01]fixture',encoding='utf-8');progress=[]
    second=index_library(tmp_path,metadata_only=True,cached=cache,progress=progress.append)[0]
    assert calls==[] and second['file_id']==first['file_id'] and second['companions'][0]['type']=='lyrics'
    assert first['companions']==[] and progress[-1]['cached_count']==1
    lyric.unlink();track.write_bytes(b'changed fixture')
    third=index_library(tmp_path,metadata_only=True,cached={second['file_id']:second})[0]
    assert len(calls)==1 and third['companions']==[] and third['size']!=second['size']


def test_upgrade_recovers_legacy_duplicate_starts_without_replaying(preparation):
    service,cfg=preparation
    for task_id,state in [('old-running','recovery_required'),('old-duplicate','queued')]:
        service.store.put('tasks',task_id,{'task_id':task_id,'command':'start_workflow','state':state,'args':{'scene':'upgrade'},'config_snapshot':{'values':cfg}})
    service.workflows.recover()
    jobs=service.dispatch('list_workflows')
    assert len(jobs['jobs'])==2 and all(j['stage']=='needs_review' for j in jobs['jobs'])
    assert jobs['active']==0 and service.workflows.next_job() is None
    assert service.store.get('tasks','old-duplicate')['state']=='recovery_required'
    batch=service.dispatch('get_workflow',{'job_id':jobs['jobs'][0]['job_id']})['job']
    assert batch['job_kind']=='batch'
    with pytest.raises(ValueError,match='音质升级.*重新开始.*不会自动重放'):
        service.dispatch('review_workflow',{'job_id':batch['job_id'],'choice':'retry'})
    assert service.store.get('tasks','old-duplicate')['state']=='recovery_required'


@pytest.mark.parametrize('invalid',['schema','version','error','path'])
def test_metadata_cache_invalidates_old_or_changed_evidence(tmp_path,monkeypatch,invalid):
    import mutagen
    track=tmp_path/'Song.flac';track.write_bytes(b'fixture');calls=[]
    monkeypatch.setattr(mutagen,'File',lambda *a,**k:calls.append(a[0]) or SimpleNamespace(tags={'title':['Song'],'artist':['Artist']},info=SimpleNamespace(length=5),pictures=[]))
    row=index_library(tmp_path,metadata_only=True)[0];calls.clear()
    if invalid=='schema':row['metadata_index_version']=1
    elif invalid=='version':row['metadata_source_version'][1]+=1
    elif invalid=='error':row['read_error']='BUSY_CHANGED_DURING_INDEX'
    elif invalid=='path':row['path']=str(tmp_path/'Other.flac')
    refreshed=index_library(tmp_path,metadata_only=True,cached={row['file_id']:row})[0]
    assert len(calls)==1 and refreshed['path']==str(track) and refreshed['read_error'] is None


def test_orphan_legacy_album_jobs_are_held_but_new_batches_keep_pending_stages(preparation,monkeypatch):
    service,cfg=preparation
    common={'kind':'workflow','scene':'playlist','config':cfg,'targets':[],'old_files':[],'progress':0,'message':''}
    for job_id,stage,resume in [('old-pending','searching',None),('old-active','running','fallback'),('old-finished','completed',None)]:
        value={**common,'job_id':job_id,'batch_id':'legacy-batch','stage':stage}
        if resume:value['resume_stage']=resume
        service.workflows.put(value)
    service.workflows.put({**common,'job_kind':'batch','job_id':'batch:current','batch_id':'current','stage':'completed'})
    service.workflows.put({**common,'job_id':'new-pending','batch_id':'current','stage':'searching'})
    service.workflows.put({**common,'job_id':'new-active','batch_id':'current','stage':'running','resume_stage':'downloading'})
    # Recovery is a state-only action, even when an old task had already added
    # a torrent or consumed a token before the process stopped.
    service.store.put('download_tasks','existing-download',{'state':'seeding','resource_id':'fixture'})
    service.store.put('transactions','existing-transaction',{'state':'COMMITTED'})
    downloads=service.store.list('download_tasks');transactions=service.store.list('transactions')
    monkeypatch.setattr(service.workflows,'provider',lambda *a:pytest.fail('recovery contacted a PT site'))
    service.workflows.recover()
    rows={row['job_id']:row for row in service.workflows.rows()}
    assert rows['old-pending']['stage']=='needs_review' and rows['old-pending']['legacy_stage']=='searching'
    assert rows['old-pending']['resume_stage']=='searching' and rows['old-pending']['legacy_batch_requires_review']
    assert rows['old-active']['stage']=='needs_review' and rows['old-active']['legacy_stage']=='running'
    assert rows['old-active']['resume_stage']=='fallback' and rows['old-active']['review_kind']=='interrupted'
    assert rows['old-finished']['stage']=='completed' and 'legacy_stage' not in rows['old-finished']
    assert rows['new-pending']['stage']=='searching' and 'legacy_stage' not in rows['new-pending']
    assert rows['new-active']['stage']=='needs_review' and rows['new-active']['resume_stage']=='downloading'
    assert 'legacy_batch_requires_review' not in rows['new-active']
    assert service.workflows.next_job()['job_id']=='new-pending'
    assert service.store.list('download_tasks')==downloads and service.store.list('transactions')==transactions
    service.workflows.recover()
    assert service.store.get('evidence','workflow:old-active')['legacy_stage']=='running'


def test_malformed_batch_identity_cannot_auto_resume_orphan_albums(preparation):
    service,_=preparation
    service.workflows.put({'job_kind':'batch','job_id':'wrong-identity','batch_id':'old','stage':'completed'})
    service.workflows.put({'job_id':'legacy','batch_id':'old','stage':'fallback','scene':'playlist','message':''})
    service.workflows.recover()
    legacy=service.store.get('evidence','workflow:legacy')
    assert legacy['stage']=='needs_review' and legacy['legacy_stage']=='fallback'
    assert service.workflows.next_job() is None
