import copy
import os
import time
import threading
from pathlib import Path
os.environ.setdefault('QT_QPA_PLATFORM','offscreen')
import pytest
from PySide6.QtCore import QTimer
from PySide6.QtWidgets import QApplication,QPushButton
from musicops.config.schema import default_config
from legacy_gui import MainWindow,PAGE_NAMES

@pytest.fixture(scope='module')
def app():return QApplication.instance() or QApplication([])

def wait_for(app,predicate,seconds=5):
    end=time.monotonic()+seconds
    while time.monotonic()<end:
        app.processEvents()
        if predicate():return
        time.sleep(.005)
    assert predicate(),'GUI异步结果等待超时'

class GuiFixtureClient:
    def __init__(self,base):self.calls=[];self.config={'config_id':'default','version':1,'values':default_config(base)};self.paused=False;self.tasks=[];self.base=base
    def ensure_running(self):pass
    def call(self,command,payload):
        self.calls.append((command,copy.deepcopy(payload)))
        if command in ('attach','get_status'):return {'config':copy.deepcopy(self.config),'tasks':self.tasks,'paused':self.paused}
        if command=='update_config':self.config={'config_id':'default','version':2,'values':copy.deepcopy(payload['values'])};return {'config':self.config}
        if command=='pause_pipeline':self.paused=True;return {'paused':True}
        if command=='resume_pipeline':self.paused=False;return {'paused':False}
        if command=='parse_playlist':return {'playlist_ids':['123'],'errors':[]}
        if command=='search':return {'candidates':[{'candidate_id':'demo:1','title':'演示','source':'netease'}]}
        if command in ('prepare_fill','prepare_repair','prepare_dedup'):
            return {'plan':{'plan_id':command,'operations':[{'source':'synthetic','destination':'demo.flac'}],'reviewed':False}}
        if command=='submit_plan':return {'tx_id':'virtual-tx','state':'committed'}
        if command=='request_rollback':return {'tx_id':payload['tx_id'],'state':'rolled_back'}
        if command=='slow':time.sleep(.25);return {'ok':True}
        if command=='queued':
            self.tasks.append({'task_id':'queued1','state':'completed','result':{'files':[{'file_id':'f1','path':'synthetic.flac'}]}});return {'accepted':True,'task_id':'queued1'}
        return {}

def test_V36_all_parameters_visual_save_no_network_and_snapshots(app,tmp_path):
    client=GuiFixtureClient(tmp_path);window=MainWindow(tmp_path,client,autostart=False);window._status(client.call('get_status',{}))
    assert window.pages.count()==len(PAGE_NAMES) and window.nav.count()==len(PAGE_NAMES)
    controls=window.editor.controls
    assert ('lyrics','primary','auth_secret_ref') in controls and ('scheduler','playlist_time') in controls and ('qbt','base_url') in controls
    controls[('lyrics','primary','base_url')][0].setText('https://new.example')
    window.findChild(QPushButton,'save_config').click();wait_for(app,lambda:window.pending==0)
    assert client.config['version']==2
    assert [name for name,_ in client.calls if name!='get_status']==['update_config']
    assert client.config['values']['lyrics']['primary']['base_url']=='https://new.example'
    window.close()

def test_V37_ipc_runs_off_gui_thread_and_queued_results_are_consumed(app,tmp_path):
    client=GuiFixtureClient(tmp_path);window=MainWindow(tmp_path,client,autostart=False);ticks=[];timer=QTimer();timer.setInterval(10);timer.timeout.connect(lambda:ticks.append(1));timer.start()
    entered=threading.Event();release=threading.Event();original=client.call;threads=[]
    def blocked(command,payload):
        if command=='slow':
            threads.append(threading.get_ident());entered.set()
            assert release.wait(10),'GUI did not release the test request'
            return {'ok':True}
        return original(command,payload)
    client.call=blocked
    try:
        window.request('slow',{})
        wait_for(app,lambda:entered.is_set() and len(ticks)>5)
        assert len(threads)==1 and threads[0]!=threading.get_ident()
    finally:
        release.set();timer.stop()
    wait_for(app,lambda:window.pending==0)
    window.request('queued',{},lambda r:window.result_to_page('音乐库与音质',r));wait_for(app,lambda:window.pending==0);window._status(client.call('get_status',{}))
    assert window.tables['音乐库与音质'].rowCount()==1 and not window.task_callbacks
    window.close()

def test_T18_three_mouse_workflows_plan_review_and_rollback(app,tmp_path):
    client=GuiFixtureClient(tmp_path);window=MainWindow(tmp_path,client,autostart=False)
    window.playlist_input.setPlainText('123');window.findChild(QPushButton,'parse_playlist').click();wait_for(app,lambda:window.pending==0)
    window.query.setText('演示');window.findChild(QPushButton,'search').click();wait_for(app,lambda:window.pending==0);window.tables['资源搜索'].selectRow(0)
    window.findChild(QPushButton,'candidate_plan').click();wait_for(app,lambda:window.pending==0);window.findChild(QPushButton,'execute_fill').click();wait_for(app,lambda:window.pending==0)
    window.repair_path.setText(str(tmp_path/'demo.wav'));selected,value,locked=window.field_controls['album'];selected.setChecked(True);value.setText('专辑修复');window.findChild(QPushButton,'prepare_repair').click();wait_for(app,lambda:window.pending==0);window.findChild(QPushButton,'execute_repair').click();wait_for(app,lambda:window.pending==0)
    window.findChild(QPushButton,'prepare_dedup').click();wait_for(app,lambda:window.pending==0);window.findChild(QPushButton,'execute_dedup').click();wait_for(app,lambda:window.pending==0)
    plans=[payload['plan'] for command,payload in client.calls if command=='submit_plan'];assert len(plans)==3 and all(p['reviewed'] for p in plans)
    window.tx_id.setText('virtual-tx');window.findChild(QPushButton,'request_rollback').click();wait_for(app,lambda:window.pending==0);assert window.last_results['request_rollback']['state']=='rolled_back'
    window.close()

def test_V38_pause_resume_without_seeding_and_V39_no_start_scan(app,tmp_path):
    client=GuiFixtureClient(tmp_path);window=MainWindow(tmp_path,client,autostart=True);wait_for(app,lambda:window.pending==0 and any(cmd=='get_status' for cmd,_ in client.calls))
    assert set(command for command,_ in client.calls)<= {'attach','get_status'}
    window.findChild(QPushButton,'pause_pipeline').click();wait_for(app,lambda:window.pending==0);assert client.paused
    window.findChild(QPushButton,'resume_pipeline').click();wait_for(app,lambda:window.pending==0);assert not client.paused
    assert not any(command in ('stop_seeding','index_library','search','demo') for command,_ in client.calls)
    window.close()

def test_navigation_shortcuts_resolve_page_names(app,tmp_path):
    client=GuiFixtureClient(tmp_path);window=MainWindow(tmp_path,client,autostart=False)
    for button,page in [('补齐歌单','歌单与补齐'),('修复库信息','库体检与修复'),('音质对比去重','音乐库与音质')]:
        window.findChild(QPushButton,button).click();assert window.nav.currentItem().text()==page
    window._repair_selected();assert window.nav.currentItem().text()=='库体检与修复'
    window.findChild(QPushButton,'转到歌词封面').click();assert window.nav.currentItem().text()=='歌词与封面'
    window.close()

def test_guided_secret_entry_stores_qbt_pair_as_json(app,tmp_path):
    client=GuiFixtureClient(tmp_path);window=MainWindow(tmp_path,client,autostart=False);window._status(client.call('get_status',{}))
    window.secret_purpose.setCurrentIndex(2);window.secret_username.setText('virtual-user');window.secret_value.setText('virtual-password')
    window.findChild(QPushButton,'set_secret').click();wait_for(app,lambda:window.pending==0)
    import json
    payload=next(p for c,p in client.calls if c=='set_secret');assert json.loads(payload['value'])=={'username':'virtual-user','password':'virtual-password'}
    assert payload['host']=='127.0.0.1' and window.secret_value.text()==''
    assert window.editor.controls[('offline',)][0].isEnabled()
    window.close()

@pytest.mark.skipif(os.name!='nt',reason='Windows AF_PIPE/DPAPI验收')
def test_V38_real_named_pipe_three_mouse_flows_and_rollback(app,tmp_path):
    from musicops.worker.client import WorkerClient
    from musicops.media.tags import read_tag_fields
    from musicops.config.schema import ConfigManager
    from musicops.state.store import StateStore
    base=tmp_path/'中文档案'
    # This test explicitly owns a synthetic archive; the production entry point
    # now creates online profiles and opens demos in a separate archive.
    ConfigManager(StateStore(base/'state'/'musicops.sqlite'),base)
    client=WorkerClient(base);window=MainWindow(base,client,autostart=True)
    def drained():
        if not window.pending and window.task_callbacks:window.refresh()
        return not window.pending and not window.task_callbacks
    def click(name):
        window.findChild(QPushButton,name).click();wait_for(app,drained,seconds=25)
    try:
        wait_for(app,lambda:window.config_version==1 and not window.pending,seconds=15)
        click('demo');assert window.last_results['demo'].get('evidence')=='synthetic_only'
        sources=list((base/'demo'/'source').glob('*.flac'));source_bytes={p.name:p.read_bytes() for p in sources}
        window.query.setText('星河');click('search');window.tables['资源搜索'].selectRow(0);click('candidate_plan');assert '资源搜索' in window.plans
        click('execute_fill');assert window.last_results['submit_plan'].get('state')=='COMMITTED'
        click('prepare_dedup');suggestions=window.last_results['prepare_dedup']['suggestions'];actionable=next((s for s in suggestions if s['action']=='quarantine_candidate'),None)
        assert actionable,'合成重复件应有同录音/发行及伴随资源一致证据'
        table=window.tables['音乐库与音质'];index=next(i for i in range(table.rowCount()) if table.item(i,0).data(256).get('quarantine_file_id')==actionable['quarantine_file_id']);table.selectRow(index)
        click('select_dedup');assert '音乐库与音质' in window.plans;click('execute_dedup');dedup_result=window.last_results['submit_plan'];assert dedup_result.get('state')=='COMMITTED'
        window.tx_id.setText(dedup_result['tx_id']);click('request_rollback');assert window.last_results['request_rollback']['state']=='ROLLED_BACK'
        original=base/'demo'/'library'/'示例艺人'/'离线演示专辑'/'01 晨光.flac';before=original.read_bytes();window.repair_path.setText(str(original));selected,value,lock=window.field_controls['album'];selected.setChecked(True);value.setText('离线修复演示')
        click('prepare_repair');assert window.plans['库体检与修复']['preview']['fields'][0]['before']==['离线演示专辑'];click('execute_repair');repair_result=window.last_results['submit_plan'];assert repair_result.get('state')=='COMMITTED' and read_tag_fields(original)['album']==['离线修复演示']
        window.tx_id.setText(repair_result['tx_id']);click('request_rollback');assert original.read_bytes()==before
        assert all(p.read_bytes()==source_bytes[p.name] for p in sources)
        second=MainWindow(base,WorkerClient(base),autostart=True);wait_for(app,lambda:second.config_version==1 and not second.pending,seconds=15)
        assert len(second.last_results['get_status']['tasks'])==len(client.call('list_tasks')['tasks']);second.close()
    finally:
        window.timer.stop();wait_for(app,lambda:window.pending==0,seconds=10);window.config_values.setdefault('worker',{})['keep_background']=True;window.close()
        try:client.call('shutdown_worker',{})
        except Exception:pass
