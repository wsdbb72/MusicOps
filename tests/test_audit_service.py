import copy,json,time
from concurrent.futures import ThreadPoolExecutor
import pytest
from musicops.app.service import AppService
from musicops.clients.http import HttpError,PolicyBlocked,SafeHttpTransport,redact
from musicops.clients.vault import Vault,import_cookies
from musicops.config.schema import default_config,validate_config
from musicops.providers.ai import AIProvider

@pytest.fixture
def service(tmp_path):
    s=AppService(tmp_path/'profile')
    yield s
    s.stop.set();s.thread.join(3)

@pytest.mark.parametrize('path,value',[
    (('offline',),'false'),(('ai','enabled'),'true'),(('lyrics','read_timeout'),float('nan')),
    (('lyrics','max_attempts'),99),(('lyrics','cache_ttl'),-1),(('ai','temperature'),float('inf')),
    (('worker','concurrency'),2),(('netease','batch_size'),0),(('scheduler','health_time'),'25:00'),
    (('music_servers','roon','watched_folders'),['relative']),
])
def test_invalid_config_rejected_before_save(tmp_path,path,value):
    values=default_config(tmp_path)
    obj=values
    for k in path[:-1]:obj=obj[k]
    obj[path[-1]]=value
    with pytest.raises(ValueError):validate_config(values)

@pytest.mark.parametrize('key',['API_AUTH','api_key','access_token','client_secret'])
def test_plain_secret_aliases_rejected_and_redacted(tmp_path,key):
    values=default_config(tmp_path);values['ai'][key]='virtual-audit-secret'
    with pytest.raises(ValueError,match='明文秘密'):validate_config(values)
    assert redact({key:'virtual-audit-secret'})[key]=='[REDACTED]'
    assert redact({'token_budget':3,'snapshot_ids':['one']})=={'token_budget':3,'snapshot_ids':['one']}

def test_parallel_vault_updates_preserve_all_records_and_revisions(tmp_path):
    def save(i):Vault(tmp_path).set('vault://audit/'+str(i),'virtual-'+str(i),'private.invalid')
    with ThreadPoolExecutor(8) as pool:list(pool.map(save,range(24)))
    vault=Vault(tmp_path);assert len(vault.list_refs())==24
    with ThreadPoolExecutor(8) as pool:list(pool.map(lambda _:vault.set('vault://audit/shared','virtual-shared','private.invalid'),range(16)))
    assert vault.revision('vault://audit/shared')==16
    assert not list(vault.path.parent.glob('*.new'))

def test_cookie_host_only_does_not_cross_subdomain():
    cookies=[{'name':'sid','value':'virtual','domain':'private.invalid','hostOnly':True},
             {'name':'other','value':'virtual','domain':'.private.invalid','hostOnly':False}]
    assert [c['name'] for c in import_cookies(json.dumps(cookies),'sub.private.invalid')]==['other']

def test_bounded_http_stream_rejects_before_retaining_oversized_body(monkeypatch):
    import httpx
    seen=[]
    class Stream:
        status_code=200;headers={};url='https://private.invalid/path'
        def __enter__(self):return self
        def __exit__(self,*args):pass
        def iter_bytes(self):
            for n in range(10):seen.append(n);yield b'12345'
    class Client:
        def __init__(self,**kwargs):assert kwargs['verify'] and kwargs['follow_redirects'] is False
        def __enter__(self):return self
        def __exit__(self,*args):pass
        def stream(self,*args,**kwargs):return Stream()
    monkeypatch.setattr(httpx,'Client',Client)
    with pytest.raises(HttpError,match='大小上限'):SafeHttpTransport(['private.invalid'],False).request('GET',Stream.url,max_response_bytes=10)
    assert len(seen)==3

def test_compact_status_and_single_task_fetch(service):
    service.dispatch('pause_pipeline');response=service.dispatch('search',{'query':'virtual'})
    task=service.store.get('tasks',response['task_id']);task.update(state='completed',result={'large':'x'*20000});service.store.put('tasks',task['task_id'],task)
    status=service.dispatch('get_status',{'compact':True})
    assert 'result' not in status['tasks'][0] and 'args' not in status['tasks'][0]
    assert len(json.dumps(status))<len(json.dumps(service.dispatch('get_status')))
    assert service.dispatch('get_task',{'task_id':task['task_id']})['task']['result']==task['result']

@pytest.mark.parametrize('error,state',[('AUTH_REQUIRED','blocked'),('INPUT_TOO_LARGE','blocked'),('INVALID_PAYLOAD','failed')])
def test_exception_error_class_maps_to_recoverable_task_state(service,monkeypatch,error,state):
    def fail(*_):raise HttpError(error,'virtual failure')
    monkeypatch.setattr(service,'_execute',fail)
    response=service.dispatch('search',{'query':'virtual'})
    deadline=time.monotonic()+3
    while time.monotonic()<deadline:
        task=service.store.get('tasks',response['task_id'])
        if task['state'] not in ('queued','running'):break
        time.sleep(.02)
    assert task['state']==state and task['error_class']==error

def target(tid,pids,sids,complete=True):
    return {'target_id':tid,'playlist_ids':pids,'snapshot_ids':sids,'snapshot_id':sids[0],'snapshot_complete':complete,'raw_title':'Virtual'}

def test_complete_refresh_removes_stale_memberships_not_other_playlists(service):
    for t in [target('gone',['1'],['old1']),target('shared',['1','2'],['old1','old2']),target('keep',['2'],['old2'])]:service.store.put('target_tracks',t['target_id'],t)
    incoming=target('new',['1'],['new1'])
    result=service._persist_playlist_targets({'new':incoming},{'1'})
    assert result['removed_targets']==1 and result['stored_target_count']==3
    assert service.store.get('target_tracks','gone') is None
    assert service.store.get('target_tracks','shared')['playlist_ids']==['2']
    assert service.store.get('target_tracks','shared')['snapshot_ids']==['old2']

def test_incomplete_refresh_retains_prior_complete_target(service):
    previous=target('one',['1'],['old1']);service.store.put('target_tracks','one',previous)
    attempt=target('one',['1'],['partial'],False);attempt['raw_title']='Changed'
    outcome=service._persist_playlist_targets({'one':attempt},set())
    assert outcome['retained_complete_targets']==1
    assert service.store.get('target_tracks','one')['snapshot_id']=='old1'

def test_modified_or_unsaved_plan_is_blocked_before_transaction(service):
    cfg=service.config.current()['values']
    plan={'schema_version':1,'plan_id':'virtual-plan','reviewed':False,'operations':[]}
    service.store.put('change_plans',plan['plan_id'],plan)
    changed=copy.deepcopy(plan);changed.update(reviewed=True,operations=[{'destination':'unexpected'}])
    with pytest.raises(PolicyBlocked,match='预览不一致'):service._execute('submit_plan',{'plan':changed},cfg)
    assert service.store.list('transactions')==[]

def test_malformed_ai_text_is_controlled_response_error():
    with pytest.raises(ValueError,match='AI_INVALID_RESPONSE'):AIProvider._output({'output':[{'content':[{'type':'output_text','text':['bad']}]}]})

def test_mismatched_resource_cannot_be_approved_or_forged(service):
    from pathlib import Path
    cfg=service.config.current()['values'];audio=Path(cfg['paths']['library'])/'virtual.flac'
    audio.parent.mkdir(parents=True);audio.write_bytes(b'virtual audio')
    candidate={'resource_id':'virtual-resource','kind':'lyrics','lyrics':'[00:00.00]Virtual','identity_state':'mismatch','validation':{'valid':True}}
    service.store.put('resource_candidates',candidate['resource_id'],candidate)
    args={'path':str(audio),'kind':'lyrics','candidate':candidate,'approved':True}
    with pytest.raises(PolicyBlocked,match='VERSION_MISMATCH'):service._execute('prepare_resource',args,cfg)
    args['candidate']={**candidate,'identity_state':'matched'}
    with pytest.raises(PolicyBlocked,match='证据不一致'):service._execute('prepare_resource',args,cfg)
    assert service.store.list('change_plans')==[] and not audio.with_suffix('.lrc').exists()

def test_refreshed_complete_memberships_clear_old_aggregate_block(service):
    previous=target('one',['1','2'],['old1','old2'],False);service.store.put('target_tracks','one',previous)
    for sid in ('new1','old2'):service.store.put('playlist_snapshots',sid,{'snapshot_id':sid,'complete':True})
    service._persist_playlist_targets({'one':target('one',['1'],['new1'])},{'1'})
    assert service.store.get('target_tracks','one')['snapshot_complete'] is True

def test_partial_member_retention_does_not_restore_complete_deleted_member(service):
    previous=target('one',['1','2'],['old1','old2']);service.store.put('target_tracks','one',previous)
    service._persist_playlist_targets({'one':target('one',['2'],['partial2'],False)},{'1'})
    saved=service.store.get('target_tracks','one')
    assert saved['playlist_ids']==['2'] and saved['snapshot_ids']==['old2']

def test_unsaved_matched_resource_still_requires_manual_review(service):
    from pathlib import Path
    cfg=service.config.current()['values'];audio=Path(cfg['paths']['library'])/'virtual.flac';audio.parent.mkdir(parents=True);audio.write_bytes(b'virtual')
    with pytest.raises(PolicyBlocked,match='IDENTITY_REQUIRES_REVIEW'):service._execute('prepare_resource',{'path':str(audio),'kind':'lyrics','candidate':{'lyrics':'[00:00.00]Virtual','identity_state':'matched'}},cfg)

def test_candidate_budget_and_manifest_cannot_be_modified(service):
    authoritative={'candidate_id':'virtual','source':'orpheus','estimated_cost':1,'file_list':['a.flac'],'eligibility':'UNKNOWN'}
    service.store.put('candidates','virtual',authoritative)
    for field,value in [('estimated_cost',0),('file_list',[]),('eligibility','FREE')]:
        forged={**authoritative,field:value}
        with pytest.raises(PolicyBlocked,match='检索证据不一致'):service._fill_preflight(forged)

def test_disabled_provider_never_reaches_live_transport(service,monkeypatch):
    cfg=service.config.current()['values'];cfg['offline']=False
    for site in ('netease','dicmusic'):cfg['providers'][site]['enabled']=False
    calls=[]
    monkeypatch.setattr(SafeHttpTransport,'request',lambda *a,**k:calls.append(True))
    for command,args in [('pt_preflight',{'site':'dicmusic'}),('refresh_playlist',{'playlist_id':'900000001'})]:
        with pytest.raises(PolicyBlocked,match='停用'):service._execute(command,args,cfg)
    assert not calls

@pytest.mark.parametrize('command',['prepare_repair','prepare_resource'])
def test_original_changed_during_preparation_never_yields_plan(service,monkeypatch,command):
    from pathlib import Path
    import shutil
    cfg=copy.deepcopy(service.config.current()['values']);cfg['lyrics']['write_mode']='embedded'
    audio=Path(cfg['paths']['library'])/'virtual.flac';audio.parent.mkdir(parents=True);audio.write_bytes(b'original bytes')
    original_copy=shutil.copy2
    def competing_write(source,destination,*a,**kw):
        result=original_copy(source,destination,*a,**kw);Path(source).write_bytes(b'externally changed');return result
    monkeypatch.setattr(shutil,'copy2',competing_write)
    monkeypatch.setattr('musicops.media.tags.plan_tag_changes',lambda *a,**k:{'changes':{}})
    args={'path':str(audio),'changes':{},'kind':'lyrics','lyrics':'[00:00.00]Virtual','approved':True}
    with pytest.raises(HttpError,match='原件发生变化') as caught:service._execute(command,args,cfg)
    assert caught.value.error_class=='CONFLICT' and service.store.list('change_plans')==[]
    assert audio.read_bytes()==b'externally changed'
