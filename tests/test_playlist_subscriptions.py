import copy,json
import pytest
from musicops.app import subscriptions
from musicops.clients.http import PolicyBlocked
from test_workflows import service  # noqa: F401  (fixture)


def online(service):
    current=service.config.current();cfg=copy.deepcopy(current['values']);cfg['workflow']['setup_done']=True
    service.config.save(cfg,current['version']);return service.config.current()['values']


def reader(service,monkeypatch,songs,complete=True):
    calls=[]
    def refresh(args,cfg):
        calls.append(args)
        targets={t:{'target_id':t,'raw_title':t,'artists':['A'],'album':'B','snapshot_complete':complete} for t in songs}
        return {'status':'OK' if complete else 'SNAPSHOT_INCOMPLETE','complete':complete,'targets':list(targets.values()),'snapshots':[{'name':'通勤'}]}
    monkeypatch.setattr(service,'_refresh_real_playlists',refresh);return calls


def imports(service):
    return [t['args'] for t in service.store.list('tasks') if t['command']=='start_workflow']


def check(service,sid,cfg):
    return service._execute('check_subscription',{'subscription_id':sid},cfg)


def test_first_check_records_baseline_then_only_new_tracks_are_imported(service,monkeypatch):
    cfg=online(service);songs=['t1','t2'];calls=reader(service,monkeypatch,songs)
    sid=service.dispatch('save_subscription',{'playlist_input':'https://music.163.com/playlist?id=123'})['subscription']['subscription_id']
    first=check(service,sid,cfg)
    assert first['new_count']==0 and imports(service)==[] and calls[0]=={'playlist_input':'https://music.163.com/playlist?id=123','platform':'auto'}
    row=service.dispatch('list_subscriptions')['subscriptions'][0]
    assert row['known_count']==2 and row['name']=='通勤' and row['last_status']=='OK' and row['next_check_at']>0
    assert check(service,sid,cfg)['new_count']==0
    songs+=['t3','t4']
    result=check(service,sid,cfg)
    assert result['new_count']==2 and result['batch_id']
    assert [a['target_ids'] for a in imports(service)]==[['t3','t4']] and imports(service)[0]['scene']=='playlist'
    # Removed tracks never cause library changes, and seen tracks are not resubmitted.
    songs.remove('t1');service.store.put('evidence','workflow:'+'batch:'+result['batch_id'],{**service.store.get('evidence','workflow:batch:'+result['batch_id']),'stage':'completed'})
    assert check(service,sid,cfg)['new_count']==0 and len(imports(service))==1


def test_import_existing_and_deferral_while_another_playlist_batch_runs(service,monkeypatch):
    cfg=online(service);reader(service,monkeypatch,['a','b'])
    sid=service.dispatch('save_subscription',{'playlist_input':'123','import_existing':True,'prefer_full_album':True})['subscription']['subscription_id']
    monkeypatch.setattr(service,'enqueue',lambda *a,**k:(_ for _ in ()).throw(PolicyBlocked('已有任务正在准备或运行')))
    deferred=check(service,sid,cfg);row=service.store.get('evidence',subscriptions.PREFIX+sid)
    assert deferred['status']=='DEFERRED' and row['known_target_ids']==[] and row['next_check_at']-row['last_checked_ts']==subscriptions.DEFER_SECONDS
    monkeypatch.undo();reader(service,monkeypatch,['a','b'])
    done=check(service,sid,cfg)
    assert done['new_count']==2 and imports(service)[0]['prefer_full_album'] is True


def test_partial_first_read_keeps_baseline_open_and_failures_retry_soon(service,monkeypatch):
    cfg=online(service);reader(service,monkeypatch,['a'],complete=False)
    sid=service.dispatch('save_subscription',{'playlist_input':'123'})['subscription']['subscription_id']
    check(service,sid,cfg);row=service.store.get('evidence',subscriptions.PREFIX+sid)
    assert row['baseline_pending'] and row['known_target_ids']==['a']
    monkeypatch.setattr(service,'_refresh_real_playlists',lambda *a:(_ for _ in ()).throw(RuntimeError('network down')))
    failed=check(service,sid,cfg);row=service.store.get('evidence',subscriptions.PREFIX+sid)
    assert failed['status']=='FAILED' and row['next_check_at']-row['last_checked_ts']==subscriptions.DEFER_SECONDS and row['history'][0]['status']=='FAILED'


def test_scheduler_queues_one_due_check_and_respects_pause_and_interval(service,monkeypatch):
    online(service);reader(service,monkeypatch,['a'])
    one=service.dispatch('save_subscription',{'playlist_input':'111'})['subscription']['subscription_id']
    two=service.dispatch('save_subscription',{'playlist_input':'222'})['subscription']['subscription_id']
    service.dispatch('save_subscription',{'subscription_id':two,'enabled':False})
    assert subscriptions.schedule(service,now=1000)
    queued=[t for t in service.store.list('tasks') if t['command']=='check_subscription']
    assert [t['args']['subscription_id'] for t in queued]==[one]
    assert not subscriptions.schedule(service,now=2000)  # one check in flight at a time
    with pytest.raises(ValueError):service.dispatch('save_subscription',{'subscription_id':one,'interval_hours':0})
    with pytest.raises(ValueError):service.dispatch('save_subscription',{'playlist_input':'111'})
    with pytest.raises(ValueError):service.dispatch('save_subscription',{'playlist_input':'https://evil.example/x'})
    saved=service.dispatch('save_subscription',{'subscription_id':one,'interval_hours':24})['subscription']
    assert saved['interval_hours']==24
    assert service.dispatch('delete_subscription',{'subscription_id':one})['library_changes']==0
    assert [s['subscription_id'] for s in service.dispatch('list_subscriptions')['subscriptions']]==[two]


def test_check_now_and_commands_are_exposed_to_web_and_worker():
    from musicops.web.server import COMMANDS
    from musicops.worker.server import READ_COMMANDS,QUEUED_COMMANDS
    assert {'list_subscriptions','save_subscription','delete_subscription','check_subscription_now'}<=COMMANDS
    assert 'list_subscriptions' in READ_COMMANDS and {'check_subscription','check_subscription_now'}<=QUEUED_COMMANDS
    assert json.dumps(subscriptions.listing.__doc__ or '')  # module importable without a service

