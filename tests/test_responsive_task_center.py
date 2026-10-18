"""Regression coverage for real queue/IPC contention and stable task browsing."""
import copy,json,threading,time
from concurrent.futures import ThreadPoolExecutor
from multiprocessing.connection import Client
from pathlib import Path
import pytest,httpx
from PySide6.QtWidgets import QApplication,QWizard
from musicops.worker.client import WorkerClient,pipe_address,auth_key
from musicops.platforms import ipc_family,is_macos
from musicops.clients.http import HttpError
from musicops.clients.qbt_live import QbtSession
from test_task_center import window_for,job,wait
from test_workflows import service

@pytest.fixture(scope='module')
def app():return QApplication.instance() or QApplication([])


def test_poll_refresh_keeps_items_and_scroll_when_selected_row_is_above_view(app,tmp_path):
    window,client=window_for(tmp_path);window.resize(1100,750);window.show();window.stack.setCurrentIndex(3)
    rows=[job(str(i),created_at=str(i)) for i in range(200)]
    window.show_jobs({'jobs':rows,'total':200});app.processEvents()
    original=window.job_items['0'];window.job_table.setCurrentItem(original)
    bar=window.job_table.verticalScrollBar();bar.setValue(bar.maximum()//2);position=bar.value();assert position>0
    for tick in range(4):
        for row in rows:row['progress']=tick
        window.show_jobs({'jobs':copy.deepcopy(rows),'total':200});app.processEvents()
        assert window.job_items['0'] is original and bar.value()==position
    window.close()


def test_refresh_coalesces_slow_background_requests_but_accepts_user_command(app,tmp_path):
    window,client=window_for(tmp_path);entered=threading.Event();release=threading.Event();original=client.call
    def slow(command,payload):
        if command=='get_status':entered.set();release.wait(4)
        return original(command,payload)
    client.call=slow
    window.call('get_status',{'compact':True});assert entered.wait(1)
    for _ in range(10):window.call('get_status',{'compact':True})
    assert len(window.requests)==1
    window.call('parse_playlist',{'playlist_input':'123'})
    release.set();wait(app,window)
    assert sum(command=='get_status' for command,_ in client.calls)==1
    window.close()


def test_solver_has_dedicated_page_and_direct_navigation(app,tmp_path):
    window,_=window_for(tmp_path);window.setup(browser=True);app.processEvents()
    wizard=window.wizard
    assert 'FlareSolverr' in wizard.currentPage().title()
    assert wizard.solver_managed.isVisible()
    wizard.solver_managed.setChecked(True)
    assert not wizard.solver_url.isEnabled() and not wizard.solver_executable.isEnabled()
    wizard.close();window.close()


def test_solver_settings_save_from_current_page_without_advancing(app,tmp_path):
    window,client=window_for(tmp_path);window.setup(browser=True);app.processEvents()
    wizard=window.wizard;wizard.solver_managed.setChecked(True)
    wizard.button(QWizard.CustomButton2).click();wait(app,window)
    payload=next(p for command,p in client.calls if command=='save_setup')
    assert payload['browser_solver']['managed'] is True and not wizard.isVisible()
    window.close()


def test_continue_failed_job_requeues_instead_of_silently_skipping(service):
    row={'job_id':'failed-connection','scene':'playlist','config':service.config.current()['values'],
         'stage':'failed','review_kind':'error','resume_stage':'requesting','targets':[],'old_files':[]}
    service.workflows.put(row)
    result=service.workflows.bulk('resume',[row['job_id']])
    assert result['succeeded']==1
    updated=service.store.get('evidence','workflow:'+row['job_id'])
    assert updated['stage']=='requesting' and updated['review_kind'] is None


def test_continue_explicitly_recovers_legacy_running_checkpoint(service):
    row={'job_id':'legacy-running','scene':'playlist','config':service.config.current()['values'],
         'stage':'failed','review_kind':'error','resume_stage':'running','error_class':'AttributeError',
         'legacy_batch_requires_review':True,'targets':[{'raw_title':'Song','artists':['Artist'],'album':'Album'}],'old_files':[]}
    service.workflows.put(row)
    result=service.workflows.bulk('resume',[row['job_id']]);assert result['succeeded']==1
    assert service.store.get('evidence','workflow:'+row['job_id'])['stage']=='identifying'


def test_cyclic_stage_failure_uses_durable_job_and_keeps_real_checkpoint(service,monkeypatch):
    row={'job_id':'broken-provider','scene':'playlist','config':service.config.current()['values'],
         'stage':'identifying','targets':[],'old_files':[]}
    service.workflows.put(row)
    def broken(job):
        job['unexpected_cycle']=job
        service.workflows.put(job,stage='searching')
    monkeypatch.setattr(service.workflows,'_identifying',broken)
    service.workflows.advance()
    saved=service.store.get('evidence','workflow:'+row['job_id'])
    assert saved['stage']=='failed' and saved['resume_stage']=='identifying'
    assert 'unexpected_cycle' not in saved and saved['error_class']=='ValueError'
    assert any(event['phase']=='workflow_error' for event in service.store.events())


def test_qbt_connection_error_is_actionable_and_not_raw_connecterror(monkeypatch):
    class Session:
        def post(self,*a,**k):raise httpx.ConnectError('fixture private url must not appear')
        def close(self):pass
    monkeypatch.setattr(httpx,'Client',lambda **_:Session())
    with pytest.raises(HttpError,match='WebUI') as error:
        QbtSession({'base_url':'http://127.0.0.1:8080','credential_ref':'vault://fixture'},lambda *_:json.dumps({'username':'fixture','password':'fixture'}))
    assert error.value.error_class=='NETWORK_ERROR' and 'private url' not in str(error.value)


def test_queue_survives_finalization_failure_without_replaying_completed_action(tmp_path,monkeypatch):
    from musicops.app.service import AppService
    s=AppService(tmp_path/'isolated-profile');executions=[];event=s.store.event;broken=[False]
    def execute(command,args,cfg):executions.append(command);return {'status':'OK'}
    def sometimes_fails(phase,*args,**kwargs):
        if phase=='task_completed' and not broken[0]:broken[0]=True;raise ValueError('synthetic finalization failure')
        return event(phase,*args,**kwargs)
    monkeypatch.setattr(s,'_execute',execute);monkeypatch.setattr(s.store,'event',sometimes_fails)
    try:
        one=s.enqueue('synthetic-one',{});two=s.enqueue('synthetic-two',{})
        deadline=time.monotonic()+5
        while time.monotonic()<deadline:
            if s.store.get('tasks',two['task_id'])['state']=='completed' and s.active is None:break
            time.sleep(.02)
        assert executions==['synthetic-one','synthetic-two']
        assert s.store.get('tasks',one['task_id'])['state']=='completed'
        assert s.store.get('tasks',two['task_id'])['state']=='completed'
        assert s.thread.is_alive() and s.active is None
    finally:s.stop.set();s.thread.join(3)


@pytest.mark.parametrize('candidate',[None,{}, {'source':'orpheus','candidate_id':'orpheus:3'}])
def test_old_incomplete_download_candidate_requires_selection_before_any_network(service,candidate):
    from musicops.clients.http import PolicyBlocked
    with pytest.raises(PolicyBlocked,match='重新搜索'):
        service.workflows._requesting({'config':service.config.current()['values'],'selected':candidate})


def test_qbt_must_be_reachable_before_any_fl_request(service,monkeypatch):
    import musicops.clients.qbt_live as live
    def unreachable(*args,**kwargs):raise HttpError('NETWORK_ERROR','qBittorrent 未启动')
    monkeypatch.setattr(live,'live_client',unreachable)
    monkeypatch.setattr(service.workflows,'provider',lambda *a,**k:pytest.fail('FL must not be requested with unreachable qBittorrent'))
    row={'job_id':'no-spend','batch_id':'batch','config':service.config.current()['values'],
         'selected':{'source':'orpheus','candidate_id':'orpheus:3','resource_id':'3'}}
    with pytest.raises(HttpError,match='未启动'):service.workflows._requesting(row)
    assert service.store.list('token_reservations')==[]
