import copy,json,os,time
os.environ.setdefault('QT_QPA_PLATFORM','offscreen')
import pytest
from PySide6.QtCore import Qt,QTimer
from PySide6.QtWidgets import QApplication,QLabel,QPushButton,QWizard,QLineEdit,QMessageBox,QInputDialog,QFileDialog
from musicops.config.schema import default_config
from musicops.gui.workbench import Workbench,SetupWizard

@pytest.fixture(scope='module')
def app():return QApplication.instance() or QApplication([])
class Client:
    def __init__(self,base):
        self.calls=[];self.config=default_config(base,offline=False);self.config['workflow']['setup_done']=True
    def call(self,command,payload):
        self.calls.append((command,copy.deepcopy(payload)))
        if command=='get_status':return {'config':{'values':self.config},'tasks':[],'secret_refs':[]}
        if command=='list_workflows':return {'jobs':[],'total':0}
        if command=='save_setup':return {'config':{'values':self.config}}
        if command=='slow':time.sleep(.2)
        return {}
def wait(app,condition):
    deadline=time.monotonic()+3
    while time.monotonic()<deadline:
        app.processEvents()
        if condition():return
        time.sleep(.005)
    assert condition()

def test_product_has_three_workflows_and_no_mode_or_internal_credential_fields(app,tmp_path):
    window=Workbench(tmp_path,Client(tmp_path),autostart=False);window.status(window.client.call('get_status',{}));window.timer.stop()
    assert window.nav.count()==5 and [window.nav.item(i).text() for i in range(3)]==['歌单入库','资料补全','音质升级']
    texts=' '.join(w.text() for w in window.findChildren(QLabel)+window.findChildren(QPushButton))
    assert all(term not in texts for term in ('正式联网','演示模式','离线测试','vault://','config_version','事务字段'))
    window.close()

def test_skippable_wizard_retains_blank_saved_secrets_and_optional_red(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config;window.available_refs=['vault://qbt/local/credentials'];wizard=SetupWizard(window)
    assert len(wizard.pageIds())==8 and isinstance(wizard.page(0),__import__('PySide6.QtWidgets',fromlist=['QWizardPage']).QWizardPage)
    assert not wizard.streaming_enabled.isChecked() and wizard.streaming_cookie.text()==''
    assert not wizard.site_fields['redacted'][0].isChecked()
    assert wizard.qbt_pass.text()=='' and wizard.ai_key.text()=='' and wizard.lyric_key.text()==''
    wizard.accept();wait(app,lambda:not window.requests)
    payload=next(p for c,p in client.calls if c=='save_setup')
    assert payload['qbt']['password']=='' and payload['sites']['redacted']['cookie_json']=='' and not payload['sites']['redacted']['enabled']
    window.close()

def test_full_playlist_selection_has_no_1000_track_truncation(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config
    targets=[{'target_id':str(i),'raw_title':'Song','artists':['Artist'],'album':'Album'} for i in range(4061)]
    window.loaded_tracks({'targets':targets,'complete':False});assert window.track_table.rowCount()==4061
    window.select_all();assert all(window.track_table.item(i,0).checkState()==Qt.Checked for i in range(4061))
    window.start('playlist');wait(app,lambda:not window.requests)
    payload=next(p for c,p in client.calls if c=='start_workflow');assert len(payload['target_ids'])==4061
    assert '未完全对账' in window.playlist_note.text();window.close()

def test_long_request_does_not_freeze_ui_event_loop(app,tmp_path):
    window=Workbench(tmp_path,Client(tmp_path),autostart=False);ticks=[];timer=QTimer();timer.setInterval(10);timer.timeout.connect(lambda:ticks.append(1));timer.start()
    window.call('slow',{});wait(app,lambda:not window.requests);timer.stop();assert len(ticks)>=8;window.close()

def test_fl_all_disables_count_and_serializes_explicit_unlimited_flag(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config
    wizard=SetupWizard(window);wizard.fl_limit.setValue(7);wizard.fl_unlimited.setChecked(True)
    assert not wizard.fl_limit.isEnabled() and wizard.fl_limit.value()==7
    wizard.accept();wait(app,lambda:not window.requests)
    payload=next(p for c,p in client.calls if c=='save_setup')
    assert payload['workflow']['fl_unlimited'] is True and payload['workflow']['fl_per_batch']==7
    window.close()

def test_all_saved_credentials_show_masked_state_without_sending_masks(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config
    cfg=window.config;cfg['providers']['dicmusic']['credential_ref']='vault://pt/dicmusic/cookies';cfg['netease']['credential_ref']='vault://netease/user/cookies';cfg['music_servers']['navidrome']['password_ref']='vault://nav/password'
    window.available_refs=[cfg['providers']['dicmusic']['credential_ref'],cfg['netease']['credential_ref'],cfg['qbt']['credential_ref'],cfg['lyrics']['primary']['auth_secret_ref'],cfg['ai']['auth_secret_ref'],cfg['catalog']['discogs']['credential_ref'],cfg['music_servers']['navidrome']['password_ref']]
    wizard=SetupWizard(window)
    fields=[wizard.site_fields['dicmusic'][1],wizard.netease_cookie,wizard.qbt_user,wizard.qbt_pass,wizard.lyric_key,wizard.ai_key,wizard.discogs,wizard.nav_pass]
    assert all(field.text()=='' and '••••' in field.placeholderText() and '已保存' in field.placeholderText() for field in fields)
    assert all(field.echoMode()==QLineEdit.Password for field in fields)
    wizard.accept();wait(app,lambda:not window.requests)
    payload=next(p for c,p in client.calls if c=='save_setup')
    assert payload['sites']['dicmusic']['cookie_json']==payload['netease_cookie_json']==payload['qbt']['password']==payload['ai']['key']==payload['discogs_token']==payload['servers']['navidrome']['password']==''
    assert '••••' not in str(payload)
    window.close()

def test_new_cookie_and_key_remain_masked_and_replace_saved_value(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config
    wizard=SetupWizard(window);cookie='[{"name":"session","value":"synthetic-session","domain":"dicmusic.com"}]'
    wizard.site_fields['dicmusic'][1].setPlainText(cookie);wizard.ai_key.setText('synthetic-key')
    assert wizard.site_fields['dicmusic'][1].echoMode()==QLineEdit.Password and wizard.ai_key.echoMode()==QLineEdit.Password
    wizard.accept();wait(app,lambda:not window.requests)
    payload=next(p for c,p in client.calls if c=='save_setup')
    assert payload['sites']['dicmusic']['cookie_json']==cookie and payload['ai']['key']=='synthetic-key'
    window.close()


def test_large_cookie_json_is_not_silently_truncated_on_import_or_save(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config
    wizard=SetupWizard(window)
    cookies=[{'name':'session','value':'x'*40000,'domain':'dicmusic.com','path':'/'}]
    text=json.dumps(cookies,indent=2);wizard.site_fields['dicmusic'][1].setPlainText(text)
    assert json.loads(wizard.site_fields['dicmusic'][1].toPlainText())==cookies
    wizard.accept();wait(app,lambda:not window.requests)
    payload=next(p for c,p in client.calls if c=='save_setup')
    assert json.loads(payload['sites']['dicmusic']['cookie_json'])==cookies
    window.close()

def test_pending_task_is_polled_by_id_even_when_missing_from_compact_status(app,tmp_path):
    class BusyClient(Client):
        def call(self,command,payload):
            self.calls.append((command,copy.deepcopy(payload)))
            if command=='start_workflow':return {'accepted':True,'task_id':'older-workflow-start'}
            if command=='get_task':return {'task':{'task_id':payload['task_id'],'state':'completed','result':{'message':'已创建 2 张专辑任务'}}}
            if command=='get_status':return {'config':{'values':self.config},'tasks':[{'task_id':f'newer-{i}','state':'completed'} for i in range(200)],'secret_refs':[]}
            if command=='list_workflows':return {'jobs':[],'total':0}
            return {}
    client=BusyClient(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config;window.start('complete')
    wait(app,lambda:'已创建 2 张专辑任务' in window.notice.text())
    assert ('get_task',{'task_id':'older-workflow-start'}) in client.calls and not window.tasks
    wait(app,lambda:not window.requests);window.close()

def test_batch_preparation_is_visible_in_task_center(app,tmp_path):
    window=Workbench(tmp_path,Client(tmp_path),autostart=False)
    window.show_jobs({'jobs':[{'job_id':'batch-1','job_kind':'batch','scene':'complete','label':'准备资料检查','stage':'preparing','progress':40,'message':'已检查 200 个音频文件'}],'total':1,'active':1})
    item=window.job_items['batch-1']
    assert item.parent().text(0)=='识别任务' and '批次' in item.text(1) and '检查本地库' in item.text(2)
    assert '200' in item.text(4);window.close()

def test_interrupted_batch_review_returns_to_scene_without_generic_retry(app,tmp_path,monkeypatch):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);messages=[]
    monkeypatch.setattr(QMessageBox,'information',lambda *args:messages.append(args[2]))
    window.review_dialog({'job_id':'batch-1','job_kind':'batch','scene':'upgrade','stage':'needs_review','review_kind':'interrupted'})
    assert messages and '重新开始' in messages[0] and window.nav.currentRow()==2
    assert not any(command=='review_workflow' for command,_ in client.calls);window.close()

def test_manual_search_keeps_literal_input_and_never_starts_download(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False)
    query='  Artist - Album [24bit]  ';window.album_query.setText(query);window.search_album()
    wait(app,lambda:not window.requests)
    calls=[(command,payload) for command,payload in client.calls if command=='manual_album_search']
    assert calls==[('manual_album_search',{'mode':'literal','query':query,'artist':''})]
    assert not window.album_artist.isEnabled() and not any(command in ('download','start_workflow','request_download') for command,_ in client.calls)
    window.close()

def test_manual_precise_search_confirms_catalogue_candidate_before_site_search(app,tmp_path,monkeypatch):
    class SearchClient(Client):
        def call(self,command,payload):
            if command=='manual_album_search':
                self.calls.append((command,copy.deepcopy(payload)))
                return {'status':'OK','candidates':[]} if payload.get('candidate_id') else {'status':'NEEDS_CHOICE','albums':[{'candidate_id':'release-1','title':'Album','artists':['Artist'],'source':'musicbrainz'}]}
            return super().call(command,payload)
    monkeypatch.setattr(QInputDialog,'getItem',lambda parent,title,prompt,items,*args:(items[0],True))
    client=SearchClient(tmp_path);window=Workbench(tmp_path,client,autostart=False)
    window.album_precise.setChecked(True);window.album_query.setText('Album');window.album_artist.setText('Artist')
    assert window.album_artist.isEnabled();window.search_album();wait(app,lambda:len([c for c,p in client.calls if c=='manual_album_search'])==2 and not window.requests)
    payloads=[payload for command,payload in client.calls if command=='manual_album_search']
    assert 'candidate_id' not in payloads[0] and payloads[1]=={**payloads[0],'candidate_id':'release-1'} and payloads[0]['mode']=='precise'
    window.close()

def test_playlist_platform_and_file_import_send_selection_and_clear_old_tracks(app,tmp_path,monkeypatch):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False)
    assert window.playlist_platform.count()==11
    window.playlist_platform.setCurrentIndex(window.playlist_platform.findData('spotify'));window.playlist.setText('playlist-id')
    window.loaded_tracks({'targets':[{'target_id':'old','raw_title':'Old song','artists':[]}],'complete':True})
    window.parse();assert not window.targets and window.track_table.rowCount()==0;wait(app,lambda:not window.requests)
    path=str(tmp_path/'export.csv');monkeypatch.setattr(QFileDialog,'getOpenFileName',lambda *args:(path,''))
    window.import_playlist();wait(app,lambda:not window.requests)
    assert [p for c,p in client.calls if c=='refresh_playlist']==[{'playlist_input':'playlist-id','platform':'spotify'},{'file_path':path,'platform':'spotify'}]
    window.close()

def test_optional_platform_authorization_is_masked_and_saved_without_mask(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config
    ref='vault://playlist/spotify/token';window.config['playlist_sources']['spotify']['credential_ref']=ref;window.available_refs=[ref]
    wizard=SetupWizard(window);field=wizard.playlist_credentials['spotify']
    assert field.text()=='' and '已保存' in field.placeholderText() and field.echoMode()==QLineEdit.Password
    wizard.accept();wait(app,lambda:not window.requests)
    payload=next(p for c,p in client.calls if c=='save_setup')
    assert payload['playlist_credentials']['spotify']=='' and '••••' not in str(payload)
    window.close()

def test_recovered_connection_clears_only_its_transient_notice(app,tmp_path):
    class ConnectingClient(Client):
        failures=1
        def call(self,command,payload):
            if command=='get_status' and self.failures:
                self.failures-=1;raise ConnectionRefusedError(61,'Connection refused')
            return super().call(command,payload)
    client=ConnectingClient(tmp_path);window=Workbench(tmp_path,client,autostart=False)
    window.call('get_status',{'compact':True},window.status);wait(app,lambda:not window.requests)
    assert '重新连接后台' in window.notice.text() and 'Errno' not in window.notice.text()
    window.call('get_status',{'compact':True},window.status);wait(app,lambda:not window.requests)
    assert window.notice.text()==''
    window._connection_notice='需要处理：[Errno 61] Connection refused';window.notice.setText('任务未完成：请核对发行')
    window.call('get_status',{'compact':True},window.status);wait(app,lambda:not window.requests)
    assert window.notice.text()=='任务未完成：请核对发行'
    window.close()

def test_exit_background_closes_window_only_after_worker_acknowledgement(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.show()
    window.exit_background();wait(app,lambda:not window.requests)
    assert ('shutdown_worker',{}) in client.calls and not window.isVisible() and not window.timer.isActive()

def test_busy_background_prevents_exit_and_does_not_cancel_tasks(app,tmp_path):
    class BusyClient(Client):
        def call(self,command,payload):
            self.calls.append((command,copy.deepcopy(payload)))
            if command=='shutdown_worker':raise RuntimeError('当前任务尚未到安全退出点')
            return super().call(command,payload)
    client=BusyClient(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.show()
    window.exit_background();wait(app,lambda:not window.requests)
    assert window.isVisible() and window.timer.isActive() and '取消对应任务' in window.notice.text()
    assert not any(command in ('cancel_task','cancel_workflow') for command,payload in client.calls)
    window.close()
