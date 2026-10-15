import copy,os,time
os.environ.setdefault('QT_QPA_PLATFORM','offscreen')
import pytest
from PySide6.QtCore import Qt,QTimer
from PySide6.QtWidgets import QApplication,QLabel,QPushButton,QWizard
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
    assert window.nav.count()==4 and [window.nav.item(i).text() for i in range(3)]==['歌单入库','资料补全','音质升级']
    texts=' '.join(w.text() for w in window.findChildren(QLabel)+window.findChildren(QPushButton))
    assert all(term not in texts for term in ('正式联网','演示模式','离线测试','vault://','config_version','事务字段'))
    window.close()

def test_skippable_wizard_retains_blank_saved_secrets_and_optional_red(app,tmp_path):
    client=Client(tmp_path);window=Workbench(tmp_path,client,autostart=False);window.config=client.config;window.available_refs=['vault://qbt/local/credentials'];wizard=SetupWizard(window)
    assert len(wizard.pageIds())==6 and isinstance(wizard.page(0),__import__('PySide6.QtWidgets',fromlist=['QWizardPage']).QWizardPage)
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
