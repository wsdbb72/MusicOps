import copy
from types import SimpleNamespace
import pytest
from musicops.app.service import AppService
from musicops.app.album_search import manual_album_search,SITES
from musicops.clients.http import HttpError


@pytest.fixture
def service(tmp_path):
    s=AppService(tmp_path/'profile',offline=False);s.stop.set();s.thread.join(3)
    current=s.config.current();cfg=copy.deepcopy(current['values'])
    for site in SITES:cfg['providers'][site]['enabled']=True
    cfg['ai']['enabled']=False;cfg['workflow']['netease_fallback']=True
    s.config.save(cfg,current['version']);return s


def make_job(service):
    return service.workflows.put({'kind':'workflow','job_id':'search-fixture','batch_id':'batch','task_group':'search','task_type':'search','scene':'playlist','config':service.config.current()['values'],'targets':[{'raw_title':'Song','album':'Album','artists':['Artist'],'duration':180}],'old_files':[],'stage':'searching','progress':0,'prefer_24bit':True,'label':'Album'})


@pytest.mark.parametrize('failure',['AUTH_FAILED','NETWORK_ERROR','RATE_LIMITED'])
def test_one_site_failure_waits_for_explicit_fallback_after_other_empty_searches(service,monkeypatch,failure):
    calls=[]
    def search(site):
        calls.append(site)
        return {'status':failure,'candidates':[]} if site=='open.cd' else {'status':'NO_RESOURCE','candidates':[]}
    monkeypatch.setattr(service.workflows,'provider',lambda site,cfg:SimpleNamespace(search=lambda _:search(site)))
    job=make_job(service);service.workflows._searching(job)
    assert job['stage']=='needs_review' and job['review_kind']=='search' and len(calls)==7 and calls.count('open.cd')==1
    assert not job.get('processing_job_id')
    service.workflows.review(job['job_id'],'streaming_fallback')
    job=service.store.get('evidence','workflow:'+job['job_id'])
    process=service.store.get('evidence','workflow:'+job['processing_job_id'])
    assert process['stage']=='fallback'
    assert job['search_summary']['unavailable_sites']==['open.cd']
    assert '四站均无' not in job['message']
    assert not service.store.list('download_tasks') and not service.store.list('token_reservations')


def test_provider_constructor_network_failure_is_isolated_for_manual_search(service,monkeypatch):
    calls=[]
    def provider(site,cfg):
        calls.append(site)
        if site=='dicmusic':raise HttpError('AUTH_FAILED','private content must not become a UI detail')
        return SimpleNamespace(search=lambda _: {'status':'NO_RESOURCE','candidates':[]})
    monkeypatch.setattr(service,'_real_pt',provider)
    result=manual_album_search(service,{'query':'Artist Album'},service.config.current()['values'])
    assert len(calls)==4 and result['status']=='NO_MATCH'
    assert result['unavailable_sites']==['dicmusic']
    assert 'private content' not in str(result) and 'Cookie' in result['source_outcomes']['dicmusic']['message']


def test_all_unavailable_waits_for_review_and_does_not_claim_no_match(service,monkeypatch):
    monkeypatch.setattr(service.workflows,'provider',lambda *a:SimpleNamespace(search=lambda _: {'status':'NETWORK_ERROR','candidates':[]}))
    job=make_job(service);service.workflows._searching(job)
    assert job['stage']=='needs_review' and job['review_kind']=='search'
    assert not job['search_summary']['successful_sites']
    monkeypatch.setattr(service,'_real_pt',lambda *a:SimpleNamespace(search=lambda _: {'status':'AUTH_FAILED','candidates':[]}))
    assert manual_album_search(service,{'query':'Album'},service.config.current()['values'])['status']=='SEARCH_UNAVAILABLE'


def test_available_exact_match_wins_even_when_another_site_fails(service,monkeypatch):
    candidate={'candidate_id':'dicmusic:1','source':'dicmusic','title':'Album','artists':['Artist'],'seeders':1,'media':{'format':'FLAC'},'release':{'album':'Album'}}
    def search(site):
        if site=='dicmusic':return {'status':'OK','candidates':[candidate],'search_complete':True}
        return {'status':'AUTH_FAILED' if site=='open.cd' else 'NO_RESOURCE','candidates':[]}
    monkeypatch.setattr(service.workflows,'provider',lambda site,cfg:SimpleNamespace(search=lambda _:search(site)))
    job=make_job(service);service.workflows._searching(job)
    process=service.store.get('evidence','workflow:'+job['processing_job_id'])
    assert job['stage']=='completed' and process['stage']=='requesting' and process['selected']['candidate_id']=='dicmusic:1'
    assert process['task_group']=='process' and process['search_job_id']==job['job_id']
    assert job['search_summary']['unavailable_sites']==['open.cd']


def test_no_resource_is_success_even_without_search_complete_field(service,monkeypatch):
    monkeypatch.setattr(service,'_real_pt',lambda *a:SimpleNamespace(search=lambda _: {'status':'NO_RESOURCE','candidates':[]}))
    result=manual_album_search(service,{'query':'Album'},service.config.current()['values'])
    assert result['status']=='NO_MATCH' and result['warnings']==[]
    assert len(result['successful_sites'])==4


@pytest.mark.parametrize('fallback',[True,False])
def test_skipping_all_pt_accounts_respects_explicit_netease_fallback_without_ai_or_pt_requests(service,monkeypatch,fallback):
    job=make_job(service);job['config']['workflow']['netease_fallback']=fallback
    job['config']['ai']['enabled']=True
    for site in SITES:job['config']['providers'][site]['enabled']=False
    monkeypatch.setattr(service.workflows,'provider',lambda *a:pytest.fail('disabled site contacted'))
    monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**k:pytest.fail('no PT query does not need AI'))
    service.workflows._searching(job)
    if fallback:
        process=service.store.get('evidence','workflow:'+job['processing_job_id'])
        assert job['stage']=='completed' and process['stage']=='fallback'
        assert process['task_group']=='process' and process['search_job_id']==job['job_id']
    else:assert job['stage']=='needs_review' and not job.get('processing_job_id')
    assert not service.store.list('download_tasks') and not service.store.list('token_reservations')
