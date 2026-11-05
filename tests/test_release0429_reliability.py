import copy,json,sqlite3,threading,time
from types import SimpleNamespace
import pytest
from musicops.clients.http import PolicyBlocked
from musicops.domain.errors import PolicyBlocked as DomainPolicy
from musicops.state.store import StateStore
from musicops.app.runtime_config import refresh
from musicops.config.schema import default_config
from musicops.domain.download_formats import allowed


def test_policy_blocked_is_one_type_and_ipc_preserves_reason():
    from musicops.worker.server import error_response
    assert PolicyBlocked is DomainPolicy
    assert error_response(DomainPolicy('原文件保留'))['error']=='原文件保留'


def test_plan_lookup_uses_expression_index_after_upgrade(tmp_path):
    path=tmp_path/'state.sqlite';store=StateStore(path)
    store.put('transactions','transaction',{'plan_id':'wanted','state':'COMMITTED'})
    store=StateStore(path)
    with store.connection() as c:
        plan=c.execute("EXPLAIN QUERY PLAN SELECT 1 FROM transactions WHERE json_extract(payload,'$.plan_id')=? LIMIT 1",('wanted',)).fetchall()
        assert 'idx_transactions_plan_id' in str([tuple(r) for r in plan])
        assert c.execute('PRAGMA busy_timeout').fetchone()[0]==30000


def test_safe_policy_refresh_preserves_paths_and_active_quality(tmp_path):
    old=default_config(tmp_path);current=copy.deepcopy(old)
    current['paths']['library']='/different';current['ai']['enabled']=True
    current['catalog']['web_enabled']=False;current['quality']['dsd_policy']='reject'
    job={'config':copy.deepcopy(old),'resume_stage':'publishing','prepared':[{}]}
    assert set(refresh(job,{'values':current,'version':7}))=={'catalog','ai'}
    assert job['config']['paths']==old['paths'] and job['config']['quality']==old['quality']
    assert job['config_revision']==7
    job={'config':copy.deepcopy(old),'stage':'searching'}
    assert 'quality' in refresh(job,{'values':current,'version':7})


def test_upgrade_does_not_reenable_legacy_batch_ai_or_web_opt_outs(tmp_path):
    current=default_config(tmp_path);current['ai']['enabled']=True
    old=copy.deepcopy(current);old['ai']['enabled']=False;old['catalog']['web_enabled']=False
    job={'config':old,'stage':'metadata'}
    refresh(job,{'values':current,'version':7},baseline_revision=7)
    assert not job['config']['ai']['enabled'] and not job['config']['catalog']['web_enabled']
    # Saving an unrelated setting must not re-enable the expensive services.
    current['updates']['enabled']=False
    refresh(job,{'values':current,'version':8},baseline_revision=7)
    assert not job['config']['ai']['enabled'] and not job['config']['catalog']['web_enabled']
    current['ai']['model']='a new explicitly configured model'
    refresh(job,{'values':current,'version':9},baseline_revision=7)
    assert job['config']['ai']['enabled'] and not job['config']['catalog']['web_enabled']


def test_seeding_auth_failure_is_cached_until_credential_revision_changes(tmp_path,monkeypatch):
    from musicops.app.library_safety import seeding_roots
    import musicops.clients.qbt_live as qbt
    revision=[1];calls=[]
    def fail(*args):calls.append(1);raise ValueError('private')
    monkeypatch.setattr(qbt,'QbtSession',fail)
    service=SimpleNamespace(vault=SimpleNamespace(list_refs=lambda:[{'ref':'vault://qbt','revision':revision[0]}],get=lambda *_:None))
    cfg={'offline':False,'qbt':{'base_url':'http://localhost','credential_ref':'vault://qbt'}}
    for _ in range(10):
        with pytest.raises(DomainPolicy):seeding_roots(service,cfg)
    assert len(calls)==1
    revision[0]=2
    with pytest.raises(DomainPolicy):seeding_roots(service,cfg)
    assert len(calls)==2


@pytest.mark.parametrize('text',['DSD64','SACD ISO','Album [DSF]','DFF'])
def test_reject_dsd_before_download(text):
    assert not allowed({'title':text},{'quality':{'dsd_policy':'reject'}})
    assert allowed({'title':text},{'quality':{'dsd_policy':'preserve'}})


def test_queue_survives_failure_of_the_recovery_write(monkeypatch):
    from musicops.app.service import AppService
    service=object.__new__(AppService);service.stop=threading.Event();service.active=None
    calls=[]
    def run():
        calls.append(1)
        if len(calls)==1:raise sqlite3.OperationalError('database is locked')
        service.stop.set()
    writes=[]
    def event(*args):
        writes.append(1)
        if len(writes)==1:raise sqlite3.OperationalError('database is locked')
    service._run_loop=run;service.store=SimpleNamespace(event=event)
    # Accelerate backoff while retaining the actual recovery state machine.
    class Stop:
        def is_set(self):return service.stop_real.is_set()
        def set(self):service.stop_real.set()
        def wait(self,_):return service.stop_real.wait(.001)
    service.stop_real=service.stop;service.stop=Stop()
    service._loop()
    assert len(calls)==2 and len(writes)==2 and service._queue_error is None


def test_event_archive_preserves_all_mutation_evidence(tmp_path):
    from musicops.state.event_archive import archive
    import gzip
    store=StateStore(tmp_path/'state.sqlite')
    old=store.event('task_completed',{'result':'old routine'})
    protected=store.event('file_published',{'result':'original audit'})
    recent=store.event('task_completed',{'result':'recent routine'})
    with store.connection() as c:c.execute("UPDATE events SET at='2000-01-01T00:00:00+00:00' WHERE seq IN (?,?)",(old,protected))
    result=archive(store)
    assert result['archived']==1
    with gzip.open(result['archive'],'rt',encoding='utf-8') as stream:assert json.loads(stream.readline())['seq']==old
    assert {r['seq'] for r in store.events()}>={protected,recent}
    assert archive(store)['archived']==0


def test_album_lookup_survives_legacy_directory_and_traditional_names(tmp_path):
    from musicops.library.album_lookup import candidates,key
    from musicops.library.catalog import root_key
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite')
    legacy={'album':'反深代詞','artists':['周深'],'directory':str(root/'周深 - 反深代詞'),'file_ids':[]}
    with store.connection() as c:
        c.execute('INSERT INTO library_albums VALUES(?,?,?)',(root_key(root),'legacy',json.dumps(legacy)))
        c.execute('DROP INDEX library_album_lookup')
    store=StateStore(store.path)
    assert candidates(store,root,'反深代词',['周深'])[0]['directory']==legacy['directory']
    assert key('Album (Live)',['Artist'])!=key('Album',['Artist'])


def test_auth_circuit_opens_once_and_credential_change_retries(monkeypatch):
    from musicops.clients.qbt_live import login,_auth_failures
    from musicops.clients.http import HttpError
    _auth_failures.clear();calls=[]
    def post(*args,**kwargs):calls.append(1);return SimpleNamespace(status_code=401,text='')
    client=SimpleNamespace(post=post)
    for _ in range(10):
        with pytest.raises(HttpError) as error:login(client,'http://fixture',{'username':'a','password':'old'})
        assert error.value.error_class=='AUTH_FAILED'
    assert len(calls)==1
    with pytest.raises(HttpError):login(client,'http://fixture',{'username':'a','password':'new'})
    assert len(calls)==2
    _auth_failures.clear()


def test_supervisor_script_parses_on_windows(tmp_path):
    import os,subprocess
    if os.name!='nt':pytest.skip('Windows supervisor')
    from musicops.worker.supervision import SCRIPT
    script=tmp_path/'supervisor.ps1';script.write_text(SCRIPT,encoding='utf-8-sig')
    env=os.environ.copy();env['MUSICOPS_PARSE_SCRIPT']=str(script)
    result=subprocess.run(['powershell','-NoProfile','-NonInteractive','-Command',"$t=$null;$e=$null;[System.Management.Automation.Language.Parser]::ParseFile($env:MUSICOPS_PARSE_SCRIPT,[ref]$t,[ref]$e)|Out-Null;if($e.Count){$e;exit 1}"],env=env,capture_output=True,text=True)
    assert result.returncode==0,result.stdout+result.stderr


def test_busy_upsert_retries_without_duplicating_records(tmp_path,monkeypatch):
    import contextlib
    store=StateStore(tmp_path/'state.sqlite');original=store.connection
    @contextlib.contextmanager
    def short_wait():
        with original() as connection:
            connection.execute('PRAGMA busy_timeout=10');yield connection
    monkeypatch.setattr(store,'connection',short_wait)
    blocker=sqlite3.connect(store.path,check_same_thread=False);blocker.execute('BEGIN IMMEDIATE')
    release=threading.Timer(.1,lambda:(blocker.rollback(),blocker.close()));release.start()
    try:store.put('evidence','unique',{'value':'saved'})
    finally:release.join()
    assert store.get('evidence','unique')['value']=='saved'
    with store.connection() as c:assert c.execute('SELECT COUNT(*) FROM evidence').fetchone()[0]==1
