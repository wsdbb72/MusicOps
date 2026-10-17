"""Task tree contracts: real IDs only, stable bulk selection and bounded AI."""
import copy
import json
import os
import threading
import time

os.environ.setdefault('QT_QPA_PLATFORM','offscreen')
import pytest
from PySide6.QtCore import Qt
from PySide6.QtWidgets import QApplication
from musicops.config.schema import default_config
from musicops.gui.workbench import Workbench,SetupWizard
from musicops.app.service import AppService


@pytest.fixture(scope='module')
def app():return QApplication.instance() or QApplication([])


def job(job_id,group='identify',**fields):
    return {'job_id':job_id,'scene':'playlist','task_group':group,'task_type':'identification' if group=='identify' else 'search','stage':'identifying' if group=='identify' else 'searching','label':'Album '+job_id,'message':'等待匹配','detail':'','progress':0,**fields}


class Client:
    def __init__(self,base):
        self.calls=[];self.config=default_config(base,offline=False);self.config['workflow']['setup_done']=True;self.config['ai']['enabled']=True;self.rows=[];self.fail_bulk=False
    def call(self,command,payload):
        self.calls.append((command,copy.deepcopy(payload)))
        if command=='get_status':return {'config':{'values':self.config},'secret_refs':[]}
        if command=='list_workflows':return {'jobs':copy.deepcopy(self.rows),'total':len(self.rows)}
        if command=='bulk_workflows':
            if self.fail_bulk:raise RuntimeError('连接中断，操作结果请刷新核对')
            return {'succeeded':len(payload['job_ids']),'skipped':0,'failed':0,'results':[{'job_id':value,'status':'OK'} for value in payload['job_ids']]}
        if command=='get_workflow':return {'job':{**job(payload['job_id']),'targets':[{'target_id':'song-1','raw_title':'Track One','artists':['Artist'],'album':'Album'}],'source_outcomes':{'dicmusic':{'status':'NO_RESOURCE'},'orpheus':{'status':'NETWORK_ERROR'}}}}
        if command=='assist_workflows':return {'message':'AI 辅助建议已保存，请打开核对窗口','results':[{'job_id':value,'status':'READY','message':'已准备核对建议'} for value in payload['job_ids']]}
        if command=='save_setup':return {'config':{'values':self.config}}
        return {}


def wait(app,window):
    end=time.monotonic()+5
    while time.monotonic()<end:
        app.processEvents()
        if not window.requests:return
        time.sleep(.005)
    assert not window.requests


def window_for(base):
    client=Client(base);window=Workbench(base,client,autostart=False);window.config=client.config
    return window,client


def test_identification_and_search_are_distinct_expandable_groups(app,tmp_path):
    window,client=window_for(tmp_path);client.rows=[job('identify-1'),job('search-1','search')];window.show_jobs({'jobs':client.rows,'total':2})
    assert [window.job_table.topLevelItem(i).text(0) for i in range(2)]==['识别任务','搜索任务']
    assert window.job_items['identify-1'].parent() is window.job_groups['identify']
    assert window.job_items['search-1'].parent() is window.job_groups['search']
    item=window.job_items['identify-1'];item.setExpanded(True);wait(app,window)
    assert item.child(0).text(0)=='Track One' and item.child(0).text(1)=='Artist'
    assert not item.child(0).flags() & Qt.ItemIsUserCheckable
    window.job_table.setCurrentItem(item.child(0));assert window.selected_job()=='identify-1'
    assert 'DicMusic：检索完成，未找到资源' in window.job_detail.toPlainText() and 'Orpheus：网络连接失败' in window.job_detail.toPlainText()
    assert [payload for command,payload in client.calls if command=='get_workflow']==[{'job_id':'identify-1'}]
    window.close()


def test_group_checkbox_selects_only_visible_real_jobs_and_survives_refresh(app,tmp_path):
    window,client=window_for(tmp_path);client.rows=[job('a'),job('b'),job('c','search')];window.show_jobs({'jobs':client.rows,'total':3})
    window.job_groups['identify'].setCheckState(0,Qt.Checked)
    assert list(window.checked_jobs)==['a','b'] and window.job_groups['search'].checkState(0)==Qt.Unchecked
    window.job_groups['identify'].setExpanded(False);window.show_jobs({'jobs':list(reversed(client.rows)),'total':3})
    assert set(window.checked_jobs)=={'a','b'} and not window.job_groups['identify'].isExpanded()
    window.bulk_action('pause');wait(app,window)
    assert ('bulk_workflows',{'action':'pause','job_ids':['a','b']}) in client.calls
    assert not any(command=='pause_pipeline' for command,_ in client.calls)
    assert '成功 2 项' in window.job_detail.toPlainText();window.close()


def test_page_selection_retains_other_page_ids_and_clear_unchecks_all(app,tmp_path):
    window,client=window_for(tmp_path);window.show_jobs({'jobs':[job('page-one')],'total':201});window.job_select_page.setCheckState(Qt.Checked)
    window.job_offset=200;window.show_jobs({'jobs':[job('page-two','search')],'total':201});window.job_select_page.setCheckState(Qt.Checked)
    assert set(window.checked_jobs)=={'page-one','page-two'} and '其他页 1 项' in window.job_selection_note.text()
    window.bulk_action('resume');wait(app,window)
    payload=next(payload for command,payload in client.calls if command=='bulk_workflows');assert payload['job_ids']==['page-one','page-two']
    window.clear_checked_jobs();assert not window.checked_jobs and all(not action.isEnabled() for action in window.bulk_buttons);window.close()


def test_cancel_does_not_silently_operate_focused_unchecked_job(app,tmp_path):
    window,client=window_for(tmp_path);window.show_jobs({'jobs':[job('focused')],'total':1});assert window.selected_job()=='focused'
    window.cancel();assert not client.calls
    window.job_items['focused'].setCheckState(0,Qt.Checked);window.cancel();wait(app,window)
    assert ('bulk_workflows',{'action':'cancel','job_ids':['focused']}) in client.calls;window.close()


def test_bulk_selection_is_capped_and_failure_restores_controls(app,tmp_path):
    window,client=window_for(tmp_path)
    for page in range(3):
        window.show_jobs({'jobs':[job(str(number)) for number in range(page*200,(page+1)*200)],'total':600});window.job_select_page.setCheckState(Qt.Checked)
    assert len(window.checked_jobs)==500
    assert window.job_select_page.checkState()==Qt.PartiallyChecked
    client.fail_bulk=True;window.bulk_action('cancel');wait(app,window)
    assert not window.bulk_pending and all(action.isEnabled() for action in window.bulk_buttons) and '未完成' in window.job_detail.toPlainText()
    assert len(next(payload['job_ids'] for command,payload in client.calls if command=='bulk_workflows'))==500;window.close()


def test_ai_assistance_is_a_bounded_id_only_batch_and_persists_polling(app,tmp_path):
    window,client=window_for(tmp_path);client.rows=[job(str(number),detail='private-path',config={'password':'synthetic-private'}) for number in range(25)];window.show_jobs({'jobs':client.rows,'total':25});window.job_select_page.setCheckState(Qt.Checked)
    window.explain();wait(app,window);payload=next(payload for command,payload in client.calls if command=='assist_workflows')
    assert payload=={'job_ids':[str(number) for number in range(20)]}
    assert not any(command=='ai_analyze' for command,_ in client.calls)
    assert '本次辅助前 20 项' in window.job_detail.toPlainText() and '建议已保存' in window.job_detail.toPlainText()
    window.show_jobs({'jobs':client.rows,'total':25});assert '建议已保存' in window.job_detail.toPlainText();window.close()


def test_ai_assistance_never_sends_gui_details_or_private_paths(app,tmp_path):
    window,client=window_for(tmp_path);client.rows=[job('safe-id',label=r'Album E:\private\name.flac',message='https://private.example/?token=secret',detail='private local directory')]
    window.show_jobs({'jobs':client.rows,'total':1});window.job_select_page.setCheckState(Qt.Checked);window.explain();wait(app,window)
    assert next(payload for command,payload in client.calls if command=='assist_workflows')=={'job_ids':['safe-id']};window.close()


def test_paused_counts_and_background_activity_do_not_claim_work_is_running_or_completed(app,tmp_path):
    window,client=window_for(tmp_path);window.show_jobs({'jobs':[job('a',paused=True),job('b','search',paused=True)],'total':2,'active':0,'paused_count':2,'total_active':2,'finished':0})
    window.tasks['pending-callback']=lambda _:None;window.update_activity()
    assert '运行 0 · 暂停 2 · 完成 0' in window.job_counts.text() and window.activity.text()=='●  已暂停 2 项' and window.busy_bar.maximum()==100
    result=client.call('get_status',{});result['paused']=True;window.status(result);window.update_activity()
    assert window.activity.text()=='●  后台已暂停' and 'pending-callback' in window.tasks
    window.tasks.clear();wait(app,window);window.close()


def test_paused_stage_uses_clear_safe_boundary_text_and_categories_have_no_job_id(app,tmp_path):
    window,client=window_for(tmp_path);window.show_jobs({'jobs':[job('paused',paused=True),job('pending','search',paused=True,pause_pending=True),job('finished',stage='completed',paused=True)],'total':3})
    assert window.job_items['paused'].text(2)=='已暂停' and window.job_items['pending'].text(2)=='阶段结束后暂停'
    assert window.job_items['finished'].text(2)=='已完成'
    window.job_table.setCurrentItem(window.job_groups['search']);assert window.selected_job() is None
    window.review();window.restore();assert not client.calls;window.close()


def test_new_api_key_defaults_ai_to_enabled_but_user_can_turn_it_off(app,tmp_path):
    window,client=window_for(tmp_path);window.config['ai']['enabled']=False;wizard=SetupWizard(window)
    assert not wizard.ai_enabled.isChecked();wizard.ai_key.setText('synthetic-api-key');assert wizard.ai_enabled.isChecked()
    wizard.ai_enabled.setChecked(False);wizard.accept();wait(app,window)
    payload=next(payload for command,payload in client.calls if command=='save_setup');assert payload['ai']['enabled'] is False
    window.close()


@pytest.fixture
def service(tmp_path):
    service=AppService(tmp_path/'real-service',offline=False);service.stop.set();service.thread.join(5)
    current=service.config.current();cfg=copy.deepcopy(current['values']);cfg['workflow']['setup_done']=True;cfg['workflow']['netease_fallback']=False
    service.config.save(cfg,current['version']);os.makedirs(cfg['paths']['library'],exist_ok=True)
    yield service
    service.stop.set();service.thread.join(5)


class ServiceClient:
    """Real command dispatch and SQLite evidence, isolated from user profiles."""
    def __init__(self,service):self.service=service;self.calls=[]
    def call(self,command,payload):
        self.calls.append((command,copy.deepcopy(payload)));return self.service.dispatch(command,payload)


def wait_for(app,predicate):
    end=time.monotonic()+8
    while time.monotonic()<end:
        app.processEvents()
        if predicate():return
        time.sleep(.005)
    assert predicate()


def test_real_service_separates_identification_and_search_then_bulk_controls_only_checked(app,service):
    target={'target_id':'netease:isolated','source':'netease','raw_title':'Song','artists':['Artist'],'album':'Album','duration':200,'metadata_complete':True}
    service.store.put('target_tracks',target['target_id'],target)
    queued=service.dispatch('start_workflow',{'scene':'playlist','target_ids':[target['target_id']]});task=service.store.get('tasks',queued['task_id'])
    service._execute(task['command'],task['args'],task['config_snapshot']['values'])
    identification=next(row for row in service.workflows.rows() if row.get('task_type')=='identification');service.workflows.advance()
    identification=service.store.get('evidence','workflow:'+identification['job_id']);search=service.store.get('evidence','workflow:'+identification['search_job_id'])
    assert identification['stage']=='completed' and identification['task_group']=='identify' and search['task_group']=='search' and search['identification_job_id']==identification['job_id']
    client=ServiceClient(service);window=Workbench(service.base,client,autostart=False);window.config=service.config.current()['values'];window.show_jobs(service.dispatch('list_workflows'))
    assert window.job_items[identification['job_id']].parent().text(0)=='识别任务' and window.job_items[search['job_id']].parent().text(0)=='搜索任务'
    window.job_groups['search'].setCheckState(0,Qt.Checked);window.bulk_action('pause');wait(app,window)
    assert service.store.get('evidence','workflow:'+search['job_id'])['paused'] is True and window.job_items[search['job_id']].text(2)=='已暂停'
    assert service.workflows.next_job() is None and '已处理 1 项' in window.job_detail.toPlainText()
    window.bulk_action('resume');wait(app,window);assert not service.store.get('evidence','workflow:'+search['job_id'])['paused']
    window.job_items[search['job_id']].setExpanded(True);wait(app,window)
    assert window.job_items[search['job_id']].child(0).text(0)=='Song'
    window.bulk_action('cancel');wait(app,window)
    assert service.store.get('evidence','workflow:'+search['job_id'])['stage']=='cancelled' and service.store.get('evidence','workflow:'+identification['job_id'])['stage']=='completed'
    assert not service.store.list('token_reservations') and not service.store.list('transactions');window.close()


def test_real_service_manual_activity_expands_actual_site_results_and_errors(app,service,monkeypatch):
    current=service.config.current();cfg=copy.deepcopy(current['values']);cfg['providers']['open.cd']['enabled']=True;cfg['providers']['dicmusic']['enabled']=True;service.config.save(cfg,current['version'])
    class Provider:
        def __init__(self,site):self.site=site
        def search(self,query):
            if self.site=='dicmusic':return {'status':'NETWORK_ERROR','candidates':[]}
            return {'status':'OK','search_complete':True,'candidates':[{'candidate_id':'open.cd:isolated','source':'open.cd','resource_id':'1','title':'Artist - Album [FLAC]','artists':['Artist'],'media':{'format':'FLAC','encoding':'24bit'},'seeders':12}]}
    monkeypatch.setattr(service,'_real_pt',lambda site,cfg:Provider(site));service.stop.clear();service.thread=threading.Thread(target=service._loop,daemon=True);service.thread.start()
    client=ServiceClient(service);window=Workbench(service.base,client,autostart=False);window.config=service.config.current()['values'];window.album_query.setText('Artist Album');window.search_album()
    wait_for(app,lambda:window.album_results.rowCount()==1 and not window.tasks and not window.requests)
    window.timer.stop();window.show_jobs(service.dispatch('list_workflows'));activity=next(row for row in service.workflows.rows() if row.get('job_kind')=='activity');item=window.job_items[activity['job_id']]
    assert item.parent().text(0)=='搜索任务' and item.text(1)=='手动专辑检索' and activity['stage']=='completed'
    item.setExpanded(True);wait(app,window)
    assert item.child(0).text(0)=='Artist - Album [FLAC]' and item.child(0).text(2)=='OpenCD' and item.child(0).text(3)=='FLAC 24bit'
    assert 'DicMusic：网络连接失败' in window.job_detail.toPlainText() and not item.child(0).flags() & Qt.ItemIsUserCheckable
    assert '部分站点未完成检索' not in window.album_note.text();window.close()
    assert not service.store.list('token_reservations') and not service.store.list('transactions')


def test_real_service_catalog_activity_expands_album_candidates_without_prepared_tracks(app,service,monkeypatch):
    album={'candidate_id':'musicbrainz:release:isolated','source':'musicbrainz','entity_kind':'release','title':'Canonical Album','artists':['Canonical Artist'],'date':'2020'}
    monkeypatch.setattr(service,'_catalog_command',lambda command,args,cfg:{'status':'OK','candidates':[album]})
    queued=service.dispatch('identify_metadata',{'album':'Album'});service.stop.clear();service.thread=threading.Thread(target=service._loop,daemon=True);service.thread.start()
    wait_for(app,lambda:service.store.get('tasks',queued['task_id'])['state']=='completed')
    client=ServiceClient(service);window=Workbench(service.base,client,autostart=False);window.config=service.config.current()['values'];window.show_jobs(service.dispatch('list_workflows'));item=window.job_items['activity:'+queued['task_id']]
    assert item.parent().text(0)=='识别任务' and item.text(1)=='在线资料识别';item.setExpanded(True);wait(app,window)
    assert item.child(0).text(0)=='Canonical Album' and item.child(0).text(1)=='Canonical Artist' and item.child(0).text(2)=='MusicBrainz';window.close()
