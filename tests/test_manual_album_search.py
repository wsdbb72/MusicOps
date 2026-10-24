import copy
import pytest
from musicops.app.service import AppService
from musicops.app.album_search import manual_album_search,exact_album,SITES
from musicops.clients.http import PolicyBlocked


@pytest.fixture
def service(tmp_path):
    s=AppService(tmp_path/'profile',offline=False);s.stop.set();s.thread.join(5)
    current=s.config.current();cfg=copy.deepcopy(current['values'])
    for site in SITES:cfg['providers'][site]['enabled']=True
    s.config.save(cfg,current['version']);return s


def candidate(site,title='Album',artists=None,rid='1'):
    return {'candidate_id':site+':'+rid,'source':site,'resource_id':rid,'title':title,'artists':artists or ['Artist'],'media':{'format':'FLAC'}}


def test_literal_search_sends_exact_input_to_all_four_sites_without_catalogue(service,monkeypatch):
    calls=[]
    class Provider:
        def __init__(self,site):self.site=site
        def search(self,text):calls.append((self.site,text));return {'status':'OK','search_complete':True,'candidates':[candidate(self.site)]}
    monkeypatch.setattr(service,'_real_pt',lambda site,cfg:Provider(site))
    monkeypatch.setattr(service,'_catalog_command',lambda *a:pytest.fail('literal mode must not identify'))
    text='  Artist - Album 24bit [2020] '
    result=manual_album_search(service,{'mode':'literal','query':text},service.config.current()['values'])
    assert calls==[(site,text) for site in SITES] and len(result['candidates'])==4
    assert result['music_writes']==0 and not service.store.list('token_reservations')


def test_precise_mode_requires_album_choice_before_any_site_search(service,monkeypatch):
    album={'candidate_id':'catalog:one','entity_kind':'release','title':'Album','artists':['Artist']}
    def identify(command,args,cfg):
        assert command=='identify_metadata' and args=={'album':'Album','artists':['Artist']}
        return {'candidates':[album,{'entity_kind':'recording','title':'Song'}]}
    monkeypatch.setattr(service,'_catalog_command',identify)
    monkeypatch.setattr(service,'_real_pt',lambda *a:pytest.fail('wait for user-confirmed album'))
    result=manual_album_search(service,{'mode':'precise','query':'Artist - Album'},service.config.current()['values'])
    assert result['status']=='NEEDS_CHOICE' and result['albums']==[album]


def test_precise_search_resolves_saved_album_and_filters_unrelated_results(service,monkeypatch):
    service.store.put('evidence','catalog:chosen',{'candidate':{'entity_kind':'release'}});calls=[]
    def resolve(command,args,cfg):
        assert command=='resolve_metadata' and args['candidate_id']=='chosen'
        return {'candidate':{'title':'Album','artists':['Artist'],'source':'musicbrainz'}}
    monkeypatch.setattr(service,'_catalog_command',resolve)
    class Provider:
        def __init__(self,site):self.site=site
        def search(self,text):
            calls.append(text)
            return {'status':'OK','candidates':[candidate(self.site),candidate(self.site,'Album Live',rid='2'),candidate(self.site,artists=['Different'],rid='3')]}
    monkeypatch.setattr(service,'_real_pt',lambda site,cfg:Provider(site))
    result=manual_album_search(service,{'mode':'precise','query':'raw ambiguous content','candidate_id':'chosen'},service.config.current()['values'])
    assert calls==['Artist Album']*4 and len(result['candidates'])==4 and result['excluded_count']==8
    assert all(c['precise_match'] for c in result['candidates'])


def test_precise_search_rejects_unconfirmed_candidate(service):
    with pytest.raises(PolicyBlocked,match='先识别'):
        manual_album_search(service,{'mode':'precise','query':'Album','candidate_id':'forged'},service.config.current()['values'])


def test_disabled_sites_are_not_contacted_and_partial_failures_are_visible(service,monkeypatch):
    cfg=service.config.current()['values']
    for site in SITES:cfg['providers'][site]['enabled']=False
    monkeypatch.setattr(service,'_real_pt',lambda *a:pytest.fail('disabled site contacted'))
    result=manual_album_search(service,{'query':'Album'},cfg)
    assert result['status']=='NOT_CONFIGURED'
    cfg['providers']['dicmusic']['enabled']=True
    class Provider:
        def search(self,_):return {'status':'AUTH_FAILED','candidates':[]}
    monkeypatch.setattr(service,'_real_pt',lambda *a:Provider())
    assert manual_album_search(service,{'query':'Album'},cfg)['source_outcomes']['dicmusic']['status']=='AUTH_FAILED'


def test_open_cd_exact_match_accepts_quality_suffix_but_not_other_album():
    assert exact_album(candidate('open.cd','Artist - Album [FLAC]'), 'Album',['Artist'])
    assert not exact_album(candidate('open.cd','Artist - Album Deluxe'), 'Album',['Artist'])


def test_query_limit_matches_provider_limit_before_contacting_sites(service,monkeypatch):
    monkeypatch.setattr(service,'_real_pt',lambda *a:pytest.fail('oversized query contacted a site'))
    with pytest.raises(ValueError,match='300'):
        manual_album_search(service,{'mode':'literal','query':'x'*301},service.config.current()['values'])


def test_unread_result_pages_are_a_scope_warning_not_a_failed_search(service,monkeypatch):
    service.store.put('evidence','catalog:chosen',{'candidate':{'entity_kind':'release'}})
    monkeypatch.setattr(service,'_catalog_command',lambda *a:{'candidate':{'title':'Album','artists':['Artist']}})
    class Provider:
        def __init__(self,site):self.site=site
        def search(self,_,page=1):return {'status':'OK','search_complete':False,'page':page,'pages':3,'candidates':[candidate(self.site,'Other Album')]}
    monkeypatch.setattr(service,'_real_pt',lambda site,cfg:Provider(site))
    result=manual_album_search(service,{'mode':'precise','query':'Album','candidate_id':'chosen'},service.config.current()['values'])
    assert result['status']=='NO_MATCH' and result['candidates']==[] and result['excluded_count']==4
    assert len(result['limited_sites'])==4 and result['unavailable_sites']==[]
    assert '不能用于断言' in result['message']


def test_long_resolved_credit_does_not_fall_through_to_invalid_site_search(service,monkeypatch):
    service.store.put('evidence','catalog:chosen',{'candidate':{'entity_kind':'release'}})
    monkeypatch.setattr(service,'_catalog_command',lambda *a:{'candidate':{'title':'Album','artists':['Artist'*60]}})
    monkeypatch.setattr(service,'_real_pt',lambda *a:pytest.fail('resolved query exceeds provider limit'))
    with pytest.raises(PolicyBlocked,match='300'):
        manual_album_search(service,{'mode':'precise','query':'Album','candidate_id':'chosen'},service.config.current()['values'])
