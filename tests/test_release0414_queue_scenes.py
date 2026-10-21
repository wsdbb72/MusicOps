import copy
import pytest
from test_workflow_start import preparation
from test_search_resilience import service,make_job
from test_task_center import app,window_for,wait
from PySide6.QtCore import Qt
from musicops.clients.http import PolicyBlocked


@pytest.mark.parametrize('existing,new',[('complete','playlist'),('playlist','complete')])
def test_independent_import_and_local_repair_can_be_submitted(preparation,existing,new):
    s,cfg=preparation
    s.workflows.put({'job_id':'existing','stage':'metadata','scene':existing,'task_type':'album','task_group':'identify'})
    accepted=s.dispatch('start_workflow',{'scene':new,'target_ids':['synthetic-selected-track']})
    assert accepted['accepted']
    assert s.store.get('evidence','workflow:existing')['stage']=='metadata'
    assert not s.store.list('download_tasks') and not s.store.list('token_reservations')


@pytest.mark.parametrize('existing,new',[('complete','complete'),('playlist','playlist'),('complete','upgrade'),('upgrade','playlist'),('fill','complete')])
def test_duplicate_and_replacement_batches_still_require_exclusion(preparation,existing,new):
    s,cfg=preparation
    s.workflows.put({'job_id':'existing','stage':'metadata','scene':existing})
    with pytest.raises(PolicyBlocked,match='准备或运行'):
        s.dispatch('start_workflow',{'scene':new,'target_ids':['synthetic-selected-track']})


def test_preparing_scan_still_blocks_other_scene(preparation):
    s,cfg=preparation
    s.workflows.put({'job_id':'existing','stage':'preparing','scene':'complete','job_kind':'batch'})
    with pytest.raises(PolicyBlocked):s.dispatch('start_workflow',{'scene':'playlist','target_ids':['track']})


def test_legacy_repair_group_is_corrected_without_changing_durable_state(service):
    row=make_job(service);row.update(scene='complete',task_type='album',task_group='identify',stage='metadata')
    service.workflows.put(row)
    assert service.workflows.public_row(row)['task_group']=='repair'
    assert service.store.get('evidence','workflow:'+row['job_id'])['task_group']=='identify'


def test_ready_download_progress_precedes_large_metadata_backlog_and_yields(service):
    flows=service.workflows
    for i in range(30):
        row=copy.deepcopy(make_job(service));row.update(job_id=f'metadata-{i}',scene='complete',stage='metadata');flows.put(row)
    row=copy.deepcopy(make_job(service));row.update(job_id='ready-download',stage='requesting');flows.put(row)
    assert flows.next_job()['job_id']=='ready-download'
    flows._io_turns=3
    assert flows.next_job()['stage']=='metadata'
    flows.put({'job_id':'verified-publication','scene':'playlist','stage':'publishing'})
    assert flows.next_job()['job_id']=='verified-publication'


def test_partial_playlist_cache_keeps_completeness_and_requires_explicit_selection(service):
    service.store.put('target_tracks','netease:1',{'target_id':'netease:1','raw_title':'Song','artists':['Artist'],'album':'Album','snapshot_complete':False})
    result=service.dispatch('get_playlist_targets',{})
    assert len(result['targets'])==1 and result['complete'] is False
    assert result['from_cache'] and result['network_used'] is False
    assert not service.store.list('download_tasks')


def test_two_slot_worker_reserves_a_network_peer_amid_metadata_backlog(service,monkeypatch):
    flows=service.workflows
    for i in range(3):
        row=copy.deepcopy(make_job(service));row.update(job_id=f'metadata-{i}',scene='complete',stage='metadata');flows.put(row)
    monkeypatch.setattr(flows,'parallel_limit',lambda:2)
    monkeypatch.setattr(service,'_checkpoint',lambda phase:None)
    seen=[]
    def stage(job):seen.append(job['resume_stage']);flows.put(job,stage='completed')
    monkeypatch.setattr(flows,'_metadata',stage);monkeypatch.setattr(flows,'_searching',stage)
    assert flows.advance()['parallel_count']==2
    assert sorted(seen)==['metadata','searching']


def test_gui_loads_partial_cache_without_selecting_or_starting_tasks(app,tmp_path):
    window,client=window_for(tmp_path);call=client.call
    def cached(command,payload):
        if command=='get_playlist_targets':
            client.calls.append((command,payload))
            return {'targets':[{'target_id':'netease:1','raw_title':'Song','artists':['Artist'],'album':'Album'}],'from_cache':True,'complete':False,'network_used':False}
        return call(command,payload)
    client.call=cached
    try:
        window.load_cached_playlist();wait(app,window)
        assert window.track_table.rowCount()==1
        assert window.track_table.item(0,0).checkState()==Qt.Unchecked
        assert '尚未重新联网' in window.playlist_note.text() and '未完全对账' in window.playlist_note.text()
        assert not any(command=='start_workflow' for command,_ in client.calls)
    finally:window.close()


def test_album_overview_exposes_scene_and_does_not_bury_search(service):
    flows=service.workflows
    for i in range(205):flows.put({'job_id':f'repair-{i}','scene':'complete','task_type':'album','stage':'metadata'})
    flows.put({'job_id':'visible-search','scene':'playlist','task_type':'search','stage':'searching'})
    result=flows.list(view='albums',state='active')
    assert result['jobs'][0]['job_id']=='visible-search'
    assert result['scene_active_counts']=={'complete':205,'playlist':1}
