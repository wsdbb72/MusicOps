import copy
from types import SimpleNamespace
from pathlib import Path
from test_search_resilience import service,make_job


def test_ai_search_words_keep_album_only_fallback_before_streaming(service,monkeypatch):
    calls=[]
    candidate={'candidate_id':'dicmusic:1','source':'dicmusic','title':'Album','artists':['Artist'],'seeders':3,'media':{'format':'FLAC'},'release':{'album':'Album'}}
    def provider(site,cfg):
        def search(query):
            calls.append((site,query));found=site=='dicmusic' and query=='Album'
            return {'status':'OK' if found else 'NO_RESOURCE','candidates':[candidate] if found else [],'search_complete':True}
        return SimpleNamespace(search=search)
    monkeypatch.setattr(service.workflows,'provider',provider)
    job=make_job(service);job['search_query']='Artist Guest Long Translation Album';service.workflows._searching(job)
    assert ('dicmusic','Album') in calls and ('dicmusic',job['search_query']) in calls
    child=service.store.get('evidence','workflow:'+job['processing_job_id'])
    assert child['stage']=='requesting' and child['selected']['source']=='dicmusic'
    assert child['pt_search_contract']==2 and not service.store.list('download_tasks')


def test_recovered_fallback_rechecks_current_sites_and_keeps_cached_audio(service):
    job=make_job(service);job.update(task_type='album',stage='fallback',fallback_serialization_recovered=True)
    job['config']=copy.deepcopy(job['config']);job['config']['providers']['dicmusic']['enabled']=False
    cached=Path(job['config']['paths']['source'])/'existing-cache.flac';cached.parent.mkdir(parents=True,exist_ok=True);cached.write_bytes(b'existing audio cache')
    job['source_path']=str(cached.parent);job['native_receipt']={'status':'cached','path':str(cached)}
    service.workflows._fallback(job)
    assert job['stage']=='searching' and job['config']['providers']['dicmusic']['enabled']
    assert cached.read_bytes()==b'existing audio cache' and job['native_receipt']['path']==str(cached)
    assert not service.store.list('download_tasks') and not service.store.list('token_reservations')


def test_rechecked_fallback_does_not_repeat_search_forever(service,monkeypatch):
    from musicops.app import streaming_fallback
    called=[];monkeypatch.setattr(streaming_fallback,'run',lambda flows,row:called.append(row['job_id']))
    job=make_job(service);job.update(stage='fallback',fallback_serialization_recovered=True,pt_search_contract=2)
    service.workflows._fallback(job)
    assert called==[job['job_id']] and job['stage']=='fallback'


def test_recovered_fallback_obeys_disabled_current_fallback(service):
    current=service.config.current();cfg=current['values']
    job=make_job(service);job.update(stage='fallback',fallback_serialization_recovered=True)
    for provider in cfg['providers'].values():provider['enabled']=False
    cfg['workflow']['netease_fallback']=False;cfg['catalog']['enabled']=False
    service.config.save(cfg,current['version'])
    service.workflows._fallback(job);service.workflows._searching(job)
    assert job['stage']=='needs_review' and job['pt_search_contract']==2
    assert not service.store.list('download_tasks')


def test_title_only_retry_does_not_treat_auth_failure_as_no_results(service,monkeypatch):
    calls=[]
    def provider(site,cfg):
        def search(query):
            calls.append((site,query))
            return {'status':'AUTH_FAILED','candidates':[]}
        return SimpleNamespace(search=search)
    monkeypatch.setattr(service.workflows,'provider',provider)
    job=make_job(service);job['search_query']='Artist Guest Album'
    service.workflows._searching(job)
    assert job['stage']=='needs_review' and job['review_kind']=='search'
    assert len(calls)==4 and all(query==job['search_query'] for _,query in calls)


def test_pt_search_gets_turn_without_blocking_verified_publication(service):
    flows=service.workflows
    def add(id,stage):
        return flows.put({'job_id':id,'batch_id':'batch','scene':'playlist','config':service.config.current()['values'],'stage':stage,'targets':[],'old_files':[]})
    add('fallback','fallback');add('search','searching');add('identify','identifying')
    assert flows.next_job()['job_id']=='fallback'
    flows._fallback_turns=3
    assert flows.next_job()['job_id'] in ('search','identify')
    add('publish','publishing')
    assert flows.next_job()['job_id']=='publish'
