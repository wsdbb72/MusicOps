import copy,datetime as dt,json,threading,time
from pathlib import Path
from concurrent.futures import ThreadPoolExecutor
import pytest
from musicops.state.store import StateStore,utcnow
from musicops.config.schema import ConfigManager,default_config
from musicops.clients.http import FakeTransport,SafeHttpTransport,PolicyBlocked,redact
from musicops.clients.vault import Vault,import_cookies
from musicops.app.service import AppService,sha
from musicops.worker.lock import WriterLock
from musicops.worker.client import WorkerClient

def wait_task(service,response,timeout=20):
    deadline=time.monotonic()+timeout
    while time.monotonic()<deadline:
        task=service.store.get('tasks',response['task_id'])
        if task['state'] not in ('queued','running','paused'):
            assert task['state']=='completed',(task.get('error'),task.get('error_class'))
            return task['result']
        time.sleep(.02)
    raise AssertionError('task timeout')

def test_state_damage_new_version_and_backup(tmp_path):
    broken=tmp_path/'broken.sqlite'; broken.write_bytes(b'broken-state')
    with pytest.raises(Exception): StateStore(broken)
    store=StateStore(tmp_path/'good.sqlite'); store.put('target_tracks','netease:1',{'artists':['A','B']}); store.backup(tmp_path/'copy.sqlite')
    assert StateStore(tmp_path/'copy.sqlite').get('target_tracks','netease:1')['artists']==['A','B']
    with store.connection() as c: c.execute('PRAGMA user_version=9')
    with pytest.raises(RuntimeError,match='SCHEMA_TOO_NEW'): StateStore(store.path)

def test_V11_token_concurrency_and_V12_unknown(tmp_path):
    store=StateStore(tmp_path/'s.sqlite'); at=utcnow()
    def reserve(r):
        try: return store.reserve_token('dicmusic',r,1,1,1,at)
        except RuntimeError: return None
    with ThreadPoolExecutor(2) as pool: results=list(pool.map(reserve,['r1','r2']))
    assert len([r for r in results if r])==1
    winner=next(r for r in results if r); store.token_result(winner['id'],'UNKNOWN')
    with pytest.raises(RuntimeError,match='对账'): store.reserve_token('dicmusic','r3',1,99,99,at)

def test_V36_config_versions_import_no_secret_and_snapshot(tmp_path):
    store=StateStore(tmp_path/'s.sqlite'); config=ConfigManager(store,tmp_path)
    first=config.current(); values=copy.deepcopy(first['values']); values['quality']['prefer_24bit']=True
    second=config.save(values,first['version']); assert second['version']==2 and not first['values']['quality']['prefer_24bit']
    with pytest.raises(RuntimeError,match='CONFIG_CONFLICT'): config.save(values,1)
    third=copy.deepcopy(second['values']); third['providers']['dicmusic']['credential_ref']='vault://pt/dic'; config.save(third,2)
    fourth=copy.deepcopy(third); fourth['providers']['dicmusic']['base_url']='https://new.example'; saved=config.save(fourth,3)
    assert saved['values']['providers']['dicmusic']['credential_ref']==''
    assert 'Authorization' not in json.dumps(config.export().get('secret_refs'))
    assert FakeTransport().requests==[]

def test_V24_vault_domain_raw_and_http_no_start(tmp_path):
    vault=Vault(tmp_path); value='complete raw virtual key'
    vault.set('vault://lyrics/private/authorization',value,'private.invalid')
    assert vault.get('vault://lyrics/private/authorization','private.invalid')==value
    with pytest.raises(RuntimeError,match='HOST_MISMATCH'): vault.get('vault://lyrics/private/authorization','public.invalid')
    assert value.encode() not in vault.path.read_bytes()
    assert value not in json.dumps(vault.list_refs())
    with pytest.raises(PolicyBlocked): SafeHttpTransport(['music.163.com']).request('GET','https://music.163.com')
    assert redact({'Authorization':value,'password':value,'Cookie':value})=={'Authorization':'[REDACTED]','password':'[REDACTED]','Cookie':'[REDACTED]'}
    cookies=import_cookies(json.dumps([{'name':'sid','value':'virtual','domain':'.private.invalid','path':'/','secure':True,'expires':1}]),'private.invalid')
    assert cookies[0]['secure'] and cookies[0]['path']=='/'

def test_V38_single_worker_IPC_attach_pause_config(tmp_path):
    base=tmp_path/'中文档案'; a=WorkerClient(base); b=WorkerClient(base)
    try:
        first=a.ensure_running(); second=b.ensure_running(); assert first['base']==second['base']
        with pytest.raises(RuntimeError,match='ALREADY_RUNNING'): WriterLock(base).acquire()
        assert a.call('pause_pipeline')['paused']; assert b.call('get_status')['paused']
        first_pause=a.call('pause_pipeline',request_id='stable-request'); second_pause=b.call('pause_pipeline',request_id='stable-request')
        assert first_pause==second_pause
        values=copy.deepcopy(first['config']['values']); values['quality']['prefer_24bit']=True
        assert b.call('update_config',{'values':values,'expected_version':first['config']['version']})['config']['version']==2
        assert a.call('resume_pipeline')['paused'] is False
    finally:
        try: a.call('shutdown_worker')
        except Exception: pass

def test_V33_V35_V40_end_to_end_fill_repair_dedup_and_rollback(tmp_path):
    service=AppService(tmp_path/'integration'); cfg=service.config.current()['values']
    try:
        demo=wait_task(service,service.dispatch('demo'))
        assert demo['comparison']['counts']=={'verified_present':1,'missing':1}
        found=wait_task(service,service.dispatch('search',{'query':'示例艺人 星河'})); assert len(found['candidates'])==4
        candidate=found['candidates'][0]
        plan=wait_task(service,service.dispatch('prepare_fill',{'candidate':candidate}))['plan']; plan['reviewed']=True
        tx=wait_task(service,service.dispatch('submit_plan',{'plan':plan})); assert tx['state']=='COMMITTED'
        token_before=service.store.list('token_reservations'); source_hash=sha(candidate['source_path'])
        repeat=wait_task(service,service.dispatch('submit_plan',{'plan':plan})); assert repeat['tx_id']==tx['tx_id']
        assert token_before==service.store.list('token_reservations')
        assert service._execute('compare',{},cfg)['counts']=={'verified_present':2}
        file=Path(cfg['paths']['library'])/candidate['destination']; old=sha(file)
        repair=wait_task(service,service.dispatch('prepare_repair',{'path':str(file),'changes':{'album':['修复专辑'],'artist':['不应写入']},'locks':['artist']}))['plan']
        assert repair['preview']['changes']=={'album':['修复专辑']}; repair['reviewed']=True
        changed=wait_task(service,service.dispatch('submit_plan',{'plan':repair})); assert sha(file)!=old
        rolled=wait_task(service,service.dispatch('request_rollback',{'tx_id':changed['tx_id']})); assert rolled['state']=='ROLLED_BACK' and sha(file)==old
        groups=wait_task(service,service.dispatch('prepare_dedup'))['suggestions']; selected=next(g['quarantine_file_id'] for g in groups if g['action']=='quarantine_candidate')
        dedup=wait_task(service,service.dispatch('prepare_dedup',{'selected_file_id':selected}))['plan']; dedup['reviewed']=True
        quarantine=wait_task(service,service.dispatch('submit_plan',{'plan':dedup})); assert quarantine['state']=='COMMITTED'
        restored=wait_task(service,service.dispatch('request_rollback',{'tx_id':quarantine['tx_id']})); assert restored['state']=='ROLLED_BACK'
        assert sha(candidate['source_path'])==source_hash
        assert service.store.list('download_tasks')
        assert all(r.get('seeding')!='stopped' for r in service.store.list('download_tasks'))
        resources=wait_task(service,service.dispatch('resources',{'title':'晨光','artist':'示例艺人','album':'离线演示专辑','duration':2}))
        assert resources['lyrics']['state']=='VALID_PAYLOAD' and resources['cover']['state']=='VALID_PAYLOAD'
        report=wait_task(service,service.dispatch('export_report')); assert Path(report['path']).is_file()
        assert report['report']['roon_state']=='pending'
    finally: service.stop.set(); service.thread.join(2)

def test_V36_task_snapshot_pause_cancel_and_path_block(tmp_path):
    s=AppService(tmp_path/'profile')
    try:
        s.dispatch('pause_pipeline'); response=s.dispatch('search',{'query':'q'}); before=s.config.current()
        changed=copy.deepcopy(before['values']); changed['quality']['prefer_24bit']=True; s.dispatch('update_config',{'values':changed,'expected_version':before['version']})
        task=s.store.get('tasks',response['task_id']); assert task['config_version']==1 and not task['config_snapshot']['values']['quality']['prefer_24bit']
        assert s.dispatch('cancel_task',{'task_id':task['task_id']})['task']['state']=='cancelled'
        with pytest.raises(PolicyBlocked): s._safe_path('E:/music')
    finally: s.stop.set(); s.thread.join(2)

def test_V23_V24_explicit_multi_host_binding_and_base_change(tmp_path):
    v=Vault(tmp_path); v.set('vault://lyrics/private/authorization','complete virtual raw',['lyrics.example.com','198.51.100.10'])
    assert v.get('vault://lyrics/private/authorization','198.51.100.10')=='complete virtual raw'
    with pytest.raises(RuntimeError): v.get('vault://lyrics/private/authorization','api.lrc.cx')
    c=ConfigManager(StateStore(tmp_path/'s.sqlite'),tmp_path); config=c.current(); values=copy.deepcopy(config['values']); values['lyrics']['direct_backup']['base_url']='http://127.0.0.1:28883'
    assert c.save(values,config['version'])['values']['lyrics']['direct_backup']['auth_secret_ref']==''

def test_V01_V33_multiple_playlists_shared_targets_and_snapshot(tmp_path):
    s=AppService(tmp_path/'profile')
    try:
        saved=s.dispatch('add_playlists',{'text':'https://music.163.com/playlist?id=10001\nhttps://music.163.com/#/playlist?id=10002'})
        assert saved['playlist_ids']==['10001','10002'] and saved['network_used'] is False
        wait_task(s,s.dispatch('demo')); result=wait_task(s,s.dispatch('refresh_playlist'))
        assert result['complete'] and len(result['snapshots'])==2
        assert len(s.store.list('target_tracks'))==2 and all(x['ordered_ids']==['1','2'] for x in result['snapshots'])
        assert len(s.store.list('playlist_snapshots'))==2
    finally:s.stop.set();s.thread.join(2)

def test_V36_secret_urls_rejected_and_blocked_result_not_success(tmp_path):
    c=ConfigManager(StateStore(tmp_path/'s.sqlite'),tmp_path)
    for group,key in [('qbt','base_url')]:
        values=copy.deepcopy(c.current()['values']); values[group][key]='http://user:virtual@127.0.0.1:8080'
        with pytest.raises(ValueError):c.save(values,c.current()['version'])
    values=copy.deepcopy(c.current()['values']);values['lyrics']['public_fallback']['cover_url']='https://api.lrc.cx/cover?passkey=virtual'
    with pytest.raises(ValueError):c.save(values,c.current()['version'])
    s=AppService(tmp_path/'profile')
    try:
        task=s.dispatch('search',{'query':'nothing'});deadline=time.monotonic()+5
        while time.monotonic()<deadline:
            result=s.store.get('tasks',task['task_id'])
            if result['state'] not in ('queued','running'):break
            time.sleep(.02)
        assert result['state']=='blocked' and result['result']['status']=='POLICY_BLOCKED'
    finally:s.stop.set();s.thread.join(2)

def test_V37_V40_cancel_after_download_decode_stops_publication(tmp_path,monkeypatch):
    s=AppService(tmp_path/'profile'); cfg=s.config.current()['values']; entered=threading.Event(); release=threading.Event()
    try:
        wait_task(s,s.dispatch('demo')); candidate=wait_task(s,s.dispatch('search'))['candidates'][0]
        plan=wait_task(s,s.dispatch('prepare_fill',{'candidate':candidate}))['plan'];plan['reviewed']=True
        validator=s._validator(cfg)
        class SlowValidator:
            def validate(self,path):
                entered.set();assert release.wait(10);return validator.validate(path)
        monkeypatch.setattr(s,'_validator',lambda cfg:SlowValidator())
        queued=s.dispatch('submit_plan',{'plan':plan});assert entered.wait(10)
        s.dispatch('cancel_task',{'task_id':queued['task_id']}); release.set(); deadline=time.monotonic()+10
        while time.monotonic()<deadline:
            task=s.store.get('tasks',queued['task_id'])
            if task['state']=='cancelled':break
            time.sleep(.02)
        assert task['state']=='cancelled' and s.store.list('transactions')==[]
        assert s.store.list('token_reservations')[0]['state']=='CONFIRMED'
        assert not (Path(cfg['paths']['library'])/candidate['destination']).exists()
        assert any(t.get('seeding')=='preserved' for t in s.store.list('download_tasks'))
    finally:release.set();s.stop.set();s.thread.join(2)

def test_T14_embedded_lyrics_cover_full_decode_and_rollback(tmp_path):
    s=AppService(tmp_path/'profile')
    try:
        wait_task(s,s.dispatch('demo')); current=s.config.current(); values=copy.deepcopy(current['values']);values['lyrics']['write_mode']='embedded';s.dispatch('update_config',{'values':values,'expected_version':current['version']})
        file=Path(values['paths']['library'])/'示例艺人'/'离线演示专辑'/'01 晨光.flac'; original=sha(file)
        for kind in ('lyrics','cover'):
            args={'path':str(file),'kind':kind,'approved':True,'lyrics':'[00:00.00]合成歌词','candidate':{'path':str(Path(values['paths']['source'])/'cover.jpg')}}
            prepared=wait_task(s,s.dispatch('prepare_resource',args));plan=prepared['plan'];assert plan['preview']['mode']=='embedded';plan['reviewed']=True
            tx=wait_task(s,s.dispatch('submit_plan',{'plan':plan}));assert tx['state']=='COMMITTED' and sha(file)!=original
            wait_task(s,s.dispatch('request_rollback',{'tx_id':tx['tx_id']}));assert sha(file)==original
    finally:s.stop.set();s.thread.join(2)
