import copy
from types import SimpleNamespace
import pytest
from musicops.app.recognition import RecognitionFallback
from musicops.app.source_identity import recover
from musicops.clients.qbt import QbtClient
from musicops.clients.http import FakeTransport,Response
from musicops.config.schema import default_config,upgrade_config
from musicops.clients.metainfo import parse_metainfo
from musicops.state.store import StateStore
from test_ai_recognition import QUERY,Resolver,Model,candidate,found,recognizer
from test_providers import torrent,free,qbt_row,response
from test_workflows import service
from test_task_center import app


def test_untrusted_album_is_hint_and_unique_recording_avoids_ai(tmp_path):
    item=candidate(album='Correct Album',release_id='release-1')
    model=Model();resolver=Resolver(found([item]))
    out=recognizer(tmp_path,resolver,model).identify({**QUERY,'album':'Incorrect Album','untrusted_metadata':True})
    assert out['selected_candidate']['album']=='Correct Album'
    assert out['query']['album']=='Incorrect Album' and not model.requests and len(resolver.requests)==1


def test_wrong_artist_allows_title_only_lookup_but_no_automatic_selection(tmp_path):
    item=candidate(artists=['Actual Artist'],album='Actual Album',release_id='release-1')
    resolver=Resolver(found(),found(),found([item]));cfg=default_config(tmp_path,offline=False)
    cfg['ai']['enabled']=False
    out=RecognitionFallback(cfg,resolver,lambda *_:None).identify({**QUERY,'artists':['Incorrect Artist'],'untrusted_metadata':True})
    assert [(r['artists'],r['album']) for r in resolver.requests]==[(['Incorrect Artist'],'Album'),(['Incorrect Artist'],''),([], '')]
    assert out['candidates'][0]['retrieval_mode']=='title_only' and out['selected_candidate'] is None
    assert out['requires_review'] and out['ai_fallback']['calls']==0


def test_ambiguous_title_releases_are_not_picked_automatically(tmp_path):
    items=[candidate(i,album='Different Album',release_id=str(i)) for i in (1,2)]
    resolver=Resolver(found(items),found(items),found(items));cfg=default_config(tmp_path,offline=False)
    out=RecognitionFallback(cfg,resolver,lambda *_:None).identify({**QUERY,'untrusted_metadata':True})
    assert out['selected_candidate'] is None and len(out['candidates'])==2


def test_relaxed_lookup_does_not_repeat_network_failures(tmp_path):
    resolver=Resolver(found(status='NETWORK_ERROR'));cfg=default_config(tmp_path,offline=False)
    out=RecognitionFallback(cfg,resolver,lambda *_:None).identify({**QUERY,'untrusted_metadata':True})
    assert len(resolver.requests)==1 and out['status']=='NETWORK_ERROR'


def test_no_title_only_selection_without_artist_and_duration():
    item=candidate(release_id='release-1')
    assert RecognitionFallback.unique_recording({**QUERY,'artists':[]},[item]) is None
    assert RecognitionFallback.unique_recording({**QUERY,'duration':None},[item]) is None


def source_job(service):
    row={'target_id':'source-1','source':'netease','raw_title':'Song','artists':['Artist'],'album':'Wrong Album','duration':123}
    job={'job_id':'source-job','kind':'workflow','scene':'playlist','stage':'searching','task_type':'search','config':service.config.current()['values'],'targets':[row],'old_files':[],'label':'Old'}
    service.workflows.put(job);return job


def test_source_album_repair_maps_every_track_and_keeps_original(service,monkeypatch):
    job=source_job(service);job['config']['catalog']['enabled']=True
    item=candidate(album='Actual Album',release_id='release-1')
    release={**item,'source_id':'release:release-1','tracklist_complete':True,'tracklist':[{'title':'Song','artists':['Artist'],'duration':123}]}
    calls=[]
    monkeypatch.setattr(service,'_recognize_metadata',lambda query,*a,**k:(calls.append(query) or {'selected_candidate':item,'candidates':[item]}))
    monkeypatch.setattr(service,'_catalog_resolver',lambda *_:SimpleNamespace(resolve_candidate=lambda *_:{'candidate':release}))
    assert recover(service.workflows,job)
    saved=service.store.get('evidence','workflow:source-job')
    assert saved['targets'][0]['album']=='Actual Album' and saved['search_query'] is None
    assert saved['targets'][0]['source_metadata_original']['album']=='Wrong Album'
    assert saved['recognition_evidence']['tracklist_verified'] and not recover(service.workflows,saved)
    assert len(calls)==1 and calls[0]['untrusted_metadata']


def test_wrong_artist_lead_enters_metadata_queue_without_overwrite(service,monkeypatch):
    job=source_job(service);job['config']['catalog']['enabled']=True
    item=candidate(artists=['Different Artist'],release_id='release-1')
    monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**k:{'candidates':[item]})
    assert recover(service.workflows,job)
    saved=service.store.get('evidence','workflow:source-job')
    assert saved['review_kind']=='metadata' and saved['targets'][0]['artists']==['Artist']


def test_partial_album_mapping_cannot_correct_whole_group(service,monkeypatch):
    job=source_job(service);job['config']['catalog']['enabled']=True
    job['targets'].append({**job['targets'][0],'raw_title':'Other Song'})
    item=candidate(album='Actual Album',release_id='release-1')
    release={**item,'source_id':'release:release-1','tracklist_complete':True,'tracklist':[{'title':'Song','artists':['Artist'],'duration':123}]}
    monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**k:{'candidates':[item],'selected_candidate':item})
    monkeypatch.setattr(service,'_catalog_resolver',lambda *_:SimpleNamespace(resolve_candidate=lambda *_:{'candidate':release}))
    assert recover(service.workflows,job)
    assert all(r['album']=='Wrong Album' for r in job['targets']) and job['stage']=='needs_review'


def make_record(tmp_path):
    data,_=torrent();meta=parse_metainfo(data);store=StateStore(tmp_path/'state.sqlite')
    qbt=QbtClient(FakeTransport(), 'https://qbt.invalid',tmp_path/'source',store=store,tag_prefix='custom:')
    row=qbt_row(tmp_path,meta)
    qbt.transport=FakeTransport([response([]),Response(200),response([row])])
    assert qbt.add_stopped(data,'task-1',free(meta))['status']=='STOPPED'
    assert qbt.transport.requests[1]['data']['tags']=='MusicOps'
    return qbt,row


def test_fixed_tag_ignores_legacy_prefix_setting_and_keeps_path_ownership(tmp_path):
    qbt,row=make_record(tmp_path)
    row['save_path']=str(tmp_path/'source'/'another-task')
    qbt.transport=FakeTransport([response([row])])
    assert qbt.poll('task-1')['status']=='POLICY_BLOCKED'
    cfg=default_config(tmp_path);cfg['qbt']['tag_prefix']='old-prefix-'
    assert upgrade_config(cfg,tmp_path)['qbt']['tag_prefix']=='MusicOps'


@pytest.mark.parametrize('used_by_other',[False,True])
def test_legacy_tags_migrate_only_owned_torrent_and_delete_only_unused_label(tmp_path,used_by_other):
    qbt,row=make_record(tmp_path);record=qbt._get('task-1');record['task_tag']='musicops-task-1';qbt._put('task-1',record)
    row['tags']='musicops-task-1, MyManualTag'
    updated={**row,'tags':row['tags']+', MusicOps'}
    qbt.transport=FakeTransport([response([row]),Response(200),response([updated]),Response(200),response([{'hash':'other'}] if used_by_other else []),Response(200)])
    assert qbt.normalize_tags('task-1')['status']=='OK'
    writes=[r for r in qbt.transport.requests if r['method']=='POST']
    assert [r['url'].rsplit('/',1)[-1] for r in writes]==['addTags','removeTags']+([] if used_by_other else ['deleteTags'])
    assert writes[0]['data']=={'hashes':record['client_id'],'tags':'MusicOps'}
    assert writes[1]['data']['tags']=='musicops-task-1'
    assert qbt._get('task-1')['task_tag']=='MusicOps' and not qbt._get('task-1').get('legacy_task_tag')
    assert all('MyManualTag' not in r['data'].get('tags','') for r in writes)


def test_tag_cleanup_refuses_foreign_and_never_adds_or_starts_torrents(tmp_path):
    qbt,row=make_record(tmp_path);record=qbt._get('task-1');record['task_tag']='musicops-task-1';qbt._put('task-1',record)
    row['tags']='musicops-task-1';row['category']='Other'
    qbt.transport=FakeTransport([response([row])])
    assert qbt.normalize_tags('task-1')['status']=='POLICY_BLOCKED'
    assert all(r['method']=='GET' for r in qbt.transport.requests)


def test_tag_remove_timeout_retains_pending_evidence_for_safe_retry(tmp_path):
    qbt,row=make_record(tmp_path);record=qbt._get('task-1');record['task_tag']='musicops-task-1';qbt._put('task-1',record)
    row['tags']='musicops-task-1';updated={**row,'tags':'MusicOps, musicops-task-1'}
    qbt.transport=FakeTransport([response([row]),Response(200),response([updated]),TimeoutError('synthetic')])
    assert qbt.normalize_tags('task-1')['status']=='WAITING'
    saved=qbt._get('task-1');assert saved['task_tag']=='MusicOps' and saved['legacy_task_tag']=='musicops-task-1'
    qbt.transport=FakeTransport([response([updated]),Response(200),response([]),Response(200)])
    assert qbt.normalize_tags('task-1')['status']=='OK'
    assert not any(r['url'].endswith(('torrents/add','torrents/start','torrents/resume')) for r in qbt.transport.requests)


def test_streaming_search_relaxes_query_without_relaxing_identity(tmp_path):
    import httpx
    from musicops.providers.streaming import StreamingProvider
    from musicops.clients.http import HttpError
    cfg=default_config(tmp_path,offline=False)['streaming'];seen=[]
    def handler(request):
        q=request.url.params['q'];seen.append(q)
        rows=[] if q!='Song' else [{'id':'one','source':'qq','name':'Song','artist':'Artist','album':'Album','duration':123}]
        return httpx.Response(200,json={'songs':rows})
    with httpx.Client(transport=httpx.MockTransport(handler)) as client:
        provider=StreamingProvider(cfg,lambda *_:None,client)
        target={'raw_title':'Song','artists':['Artist'],'album':'Album','duration':123}
        assert provider.search('qqmusic',target)['id']=='one'
        assert seen==['Artist Song','Song']
        seen.clear()
        with pytest.raises(HttpError) as error:provider.search('qqmusic',{**target,'artists':['Wrong Artist']})
        assert error.value.error_class=='NO_MATCH' and seen==['Wrong Artist Song','Song']


def test_tag_cleanup_is_queued_and_request_idempotent(service):
    from musicops.worker.server import handle_request,QUEUED_COMMANDS
    assert 'normalize_qbt_tags' in QUEUED_COMMANDS
    req={'schema_version':1,'request_id':'tag-cleanup-id','profile_id':'default','command':'normalize_qbt_tags','payload':{}}
    first=handle_request(service,req,b'fixture-key');again=handle_request(service,req,b'fixture-key')
    assert first==again and first['result']['accepted']
    assert len([t for t in service.store.list('tasks') if t.get('command')=='normalize_qbt_tags'])==1


def test_manually_normalized_legacy_tag_keeps_record_ownership(tmp_path):
    qbt,row=make_record(tmp_path);record=qbt._get('task-1');record['task_tag']='musicops-task-1';qbt._put('task-1',record)
    row['tags']='MusicOps'
    qbt.transport=FakeTransport([response([row]),Response(200),response([row]),response([]),Response(200)])
    assert qbt.normalize_tags('task-1')['status']=='OK'
    assert qbt._get('task-1')['task_tag']=='MusicOps'
    assert not any(r['url'].endswith('removeTags') for r in qbt.transport.requests)


def test_settings_tag_cleanup_button_completes(app,tmp_path):
    from test_task_center import window_for,wait
    window,client=window_for(tmp_path);window.setup();wizard=window.wizard
    wizard.qbt_tag_button.click();assert not wizard.qbt_tag_button.isEnabled()
    wait(app,window)
    assert any(command=='normalize_qbt_tags' for command,payload in client.calls)
    assert wizard.qbt_tag_button.isEnabled() and '标签整理完成' in wizard.qbt_test_note.text()
    wizard.close();window.close()
