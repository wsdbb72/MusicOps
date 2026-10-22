"""Synthetic discography, complete-album expansion and async client contracts."""
import copy
from types import SimpleNamespace
import pytest
from musicops.app.artist_search import classify,groups,search,assist_grouping
from musicops.app.single_upgrade import full_album,identify,validate
from musicops.clients.http import FakeTransport,Response,PolicyBlocked
from musicops.clients.qbt import QbtClient
from musicops.clients.metainfo import parse_metainfo
from musicops.state.store import StateStore
from test_providers import torrent,free,qbt_row,response
from test_pt_readonly import provider,group,response as pt_response
from test_search_resilience import service,make_job
from test_task_center import app,window_for,wait


def resource(site='dicmusic',ident='1',title='Album',year=2000):
    return {'candidate_id':site+':'+ident,'resource_id':ident,'group_id':'10','source':site,'title':title,'artists':['Artist'],'release':{'album':title,'year':year},'media':{'format':'FLAC'},'seeders':3}


def song(title='Song',**kw):return {'title':title,'raw_title':title,'artists':['Artist'],'duration':180,'album':'Single',**kw}


def release(**kw):return {'candidate_id':'mb-release','source':'musicbrainz','source_id':'release:fixture','title':'Album','album':'Album','artists':['Artist'],'release_type':'Album','tracklist_complete':True,'tracklist':[song(),song('Second')],**kw}


@pytest.mark.parametrize('typ',['Single','EP','',None])
def test_single_or_unknown_is_never_full_album(typ):
    assert not full_album(release(release_type=typ),[song()])


def test_full_release_requires_same_recording_and_complete_durations():
    assert full_album(release(),[song()])
    assert not full_album(release(album='Album Live'),[song()])
    assert not full_album(release(tracklist_complete=False),[song()])
    assert not full_album(release(tracklist=[song(duration=None)]),[song()])
    assert not full_album(release(tracklist=[song(artists=['Other'])]),[song()])


def expand_job(service):
    return service.workflows.put({**make_job(service),'job_id':'expand-fixture','scene':'expand','task_type':'identification','stage':'identifying','old_files':[song(path='synthetic.wav')],'retire_single_requested':False})


def test_expansion_needs_explicit_release_choice_then_separate_search(service,monkeypatch):
    job=expand_job(service);candidate=release();resolver=SimpleNamespace(resolve_candidate=lambda *a:{'candidate':candidate})
    monkeypatch.setattr(service,'_catalog_resolver',lambda cfg:resolver)
    monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**k:{'candidates':[candidate]})
    identify(service.workflows,job)
    assert job['stage']=='needs_review' and job['catalog_choices']==[candidate] and not job.get('search_job_id')
    job['completion_choice']=candidate;identify(service.workflows,job)
    child=service.store.get('evidence','workflow:'+job['search_job_id'])
    assert child['scene']=='expand' and child['stage']=='searching' and len(child['targets'])==2
    assert not child['retire_single']


def test_unmatched_b_side_is_preserved_even_when_retirement_requested(service,monkeypatch):
    job=expand_job(service);candidate=release();job.update(old_files=[song(),song('B side')],catalog_choices=[candidate],completion_choice=candidate,retire_single_requested=True)
    monkeypatch.setattr(service,'_catalog_resolver',lambda cfg:SimpleNamespace(resolve_candidate=lambda *a:{'candidate':candidate}))
    identify(service.workflows,job)
    assert job['single_unmatched_count']==1 and not job['retire_single']


def test_expansion_rechecks_choice_and_cannot_use_unapproved_release(service,monkeypatch):
    job=expand_job(service);job['completion_choice']=release()
    monkeypatch.setattr(service,'_catalog_resolver',lambda cfg:SimpleNamespace(resolve_candidate=lambda *a:{'candidate':release()}))
    with pytest.raises(PolicyBlocked):identify(service.workflows,job)


def test_expansion_same_quality_allowed_but_missing_or_downscaled_song_blocks():
    media={'codec':'flac','sample_rate':44100,'bits_per_sample':16,'channels':2}
    job={'album':'Album','expected_album_tracks':[song(),song('Second')],'old_files':[song(path='synthetic.wav')],'single_release':release()}
    rows=[song(media_info=media),song('Second',media_info=media)]
    validator=SimpleNamespace(validate=lambda p:{'decode_state':'passed','media_info':media})
    validate(job,rows,validator)
    with pytest.raises(PolicyBlocked):validate(job,rows[:1],validator)
    with pytest.raises(PolicyBlocked):validate(job,[song(media_info={**media,'codec':'mp3'}),rows[1]],validator)


def test_gazelle_artist_filter_paginates_without_generic_search():
    p,t=provider([pt_response({'status':'success','response':{'currentPage':2,'pages':3,'results':[group()]}})])
    result=p.search_artist('Artist',page=2)
    assert result['status']=='OK' and not result['search_complete']
    assert t.requests[0]['params']=={'action':'browse','searchstr':'','artistname':'Artist','page':'2'}
    assert len(t.requests)==1 and t.requests[0]['method']=='GET'


def test_opencd_unknown_pagination_never_claims_all():
    html=b'<a href="logout.php">logout</a><table><tr><td><a href="details.php?id=1" title="Artist - Album">Album</a></td></tr></table>'
    p,_=provider([Response(200,html)],'open.cd')
    result=p.search_artist('Artist');assert result['status']=='OK' and result['pages'] is None and not result['search_complete']


def test_opencd_query_only_next_link_and_observed_title_format():
    html=b'<a href="logout.php">logout</a><a href="?search=Artist&amp;page=1">next</a><table><tr><td><a href="details.php?id=1" title="Artist - Album 1997 - FLAC tracks">Album</a></td></tr></table>'
    p,t=provider([Response(200,html)],'open.cd');result=p.search_artist('Artist')
    assert result['pages']==2 and not result['search_complete']
    row=classify(result['candidates'][0],'Artist')
    assert row['album_label']=='Album' and row['release']['year']==1997 and row['artist_match']


def test_cross_site_album_and_edition_grouping_preserves_live_and_reissues():
    a=classify(resource(), 'Artist')
    b=classify(resource('open.cd',title='Artist - 2000 - Album [FLAC]'),'Artist')
    result=groups([a,b],'Artist')
    assert len(result)==1 and len(result[0]['versions'])==1 and len(result[0]['versions'][0]['candidates'])==2
    live=classify(resource('orpheus',title='Album (Live)'),'Artist')
    reissue=classify(resource('redacted',year=2020),'Artist')
    result=groups([a,live,reissue],'Artist');assert len(result)==1 and len(result[0]['versions'])==3
    descriptions=[v['label'] for r in result for v in r['versions']];assert any('现场' in t for t in descriptions)
    assert all(not r['identity_confirmed'] for r in result)


def test_artist_all_pages_retained_failed_site_can_retry_without_losing_results(service,monkeypatch):
    calls=[];failure=[True]
    def page(site,query,page):
        calls.append((site,page))
        if site=='orpheus' and failure[0]:return {'status':'NETWORK_ERROR','candidates':[]}
        return {'status':'OK','candidates':[resource(site,str(page))],'page':page,'pages':2,'search_complete':page==2}
    monkeypatch.setattr(service,'_real_pt',lambda site,cfg:SimpleNamespace(search_artist=lambda query,page:globals_page(site,query,page)))
    globals_page=page
    cfg=service.config.current()['values'];first=search(service,{'query':'Artist'},cfg)
    assert len(first['candidates'])==3 and first['has_more'] and not first['complete']
    args={'query':'Artist','artist_result_id':first['artist_result_id']}
    second=search(service,args,cfg);assert len(second['candidates'])==6 and not second['has_more']
    failure[0]=False;third=search(service,{**args,'retry_sites':True},cfg)
    fourth=search(service,args,cfg)
    assert len(fourth['candidates'])==8 and fourth['complete'] and len(calls)==9
    assert not service.store.list('download_tasks') and not service.store.list('token_reservations')


def test_repeat_page_stops_and_manual_correction_does_not_search_again(service,monkeypatch):
    calls=[]
    def page(*a,**kw):calls.append(kw['page']);return {'status':'OK','candidates':[resource()],'pages':3,'page':kw['page'],'search_complete':False}
    cfg=service.config.current()['values']
    for site in cfg['providers']:cfg['providers'][site]['enabled']=site=='dicmusic'
    monkeypatch.setattr(service,'_real_pt',lambda *a:SimpleNamespace(search_artist=page))
    result=search(service,{'query':'Artist'},cfg);args={'query':'Artist','artist_result_id':result['artist_result_id']}
    result=search(service,args,cfg);assert not result['has_more'] and result['source_outcomes']['dicmusic']['status']=='PARSE_ERROR'
    result=search(service,{**args,'group_override':{'candidate_id':'dicmusic:1','album':'Corrected'}},cfg)
    assert calls==[1,2] and result['albums'][0]['title']=='corrected'


def test_ai_groups_all_batches_but_rejects_invented_album_and_bad_payload(service,monkeypatch):
    from musicops.providers.ai import AIProvider
    cfg=service.config.current()['values'];cfg['ai']['enabled']=True;seen=[]
    rows=[classify(resource('open.cd',str(i),'Ambiguous '+str(i)),'Artist') for i in range(49)]
    def request(self,kind,schema,prompt,context):
        seen.append(context)
        return {'groups':[{'candidate_id':r['candidate_id'],'album':'Invented Album','artist_matches':True} for r in context['candidates']]}
    monkeypatch.setattr(AIProvider,'_recognition_request',request)
    assist_grouping(service,cfg,'Artist',rows)
    assert [len(r['candidates']) for r in seen]==[24,24,1]
    assert all(r['ai_grouping_attempted'] and not r.get('ai_grouping') for r in rows)
    assist_grouping(service,cfg,'Artist',rows);assert len(seen)==3
    rows[0]['ai_grouping_attempted']=False;monkeypatch.setattr(AIProvider,'_recognition_request',lambda *a:None)
    assist_grouping(service,cfg,'Artist',rows)


@pytest.mark.parametrize('version,endpoint',[('5.2.2','start'),('4.6.7','resume')])
def test_async_add_auto_start_requires_observed_running_and_reuses_receipt(tmp_path,version,endpoint):
    data,_=torrent();meta=parse_metainfo(data);row=qbt_row(tmp_path,meta,version=version)
    transport=FakeTransport([response([]),Response(202),response([]),response([row]),response([row]),Response(200),response([{**row,'state':'downloading'}])])
    client=QbtClient(transport,'https://qbt.invalid',tmp_path/'source',version=version,store=StateStore(tmp_path/'state.sqlite'))
    assert client.add_stopped(data,'task-1',free(meta))['status']=='ADD_UNKNOWN'
    assert client.reconcile_add('task-1')['status']=='STOPPED'
    assert client.start_download('task-1',free(meta))['status']=='START_REQUESTED'
    assert client.reconcile_add('task-1')['status']=='DOWNLOADING'
    assert sum(r['url'].endswith('/torrents/add') for r in transport.requests)==1
    assert sum(r['url'].endswith('/torrents/'+endpoint) for r in transport.requests)==1


def test_stopped_client_bounded_retries_and_checking_waits(tmp_path,monkeypatch):
    data,_=torrent();meta=parse_metainfo(data);row=qbt_row(tmp_path,meta)
    transport=FakeTransport([response([]),Response(200),response([row])]+[r for _ in range(3) for r in (response([row]),Response(200),response([row]))]+[response([row])])
    store=StateStore(tmp_path/'state.sqlite');client=QbtClient(transport,'https://qbt.invalid',tmp_path/'source',store=store)
    assert client.add_stopped(data,'task-1',free(meta))['status']=='STOPPED'
    tick=[100];monkeypatch.setattr('musicops.clients.qbt.time.time',lambda:tick[0])
    for _ in range(3):
        assert client.start_download('task-1',free(meta))['status']=='START_REQUESTED'
        assert client.reconcile_add('task-1')['status']=='STOPPED';tick[0]+=11
    assert client.start_download('task-1',free(meta))['status']=='START_FAILED'
    assert sum(r['url'].endswith('/start') for r in transport.requests)==3


def test_requesting_waits_for_async_add_then_starts_without_another_fl(service,monkeypatch):
    job=make_job(service);job.update(job_id='async-job',stage='requesting',selected=resource())
    evidence={'source':'dicmusic','resource_id':'1','confirmed':True};fetches=[];starts=[];polls=iter(['ADD_UNKNOWN','STOPPED','DOWNLOADING'])
    def add(*args):
        service.store.put('download_tasks','qbt:async-job',{'free_evidence':evidence})
        return {'status':next(polls)}
    qbt=SimpleNamespace(transport=SimpleNamespace(prepare=lambda *a:None,close=lambda:None),add_stopped=add,
        reconcile_add=lambda *a:{'status':next(polls)},start_download=lambda *a:(starts.append(a) or {'status':'START_REQUESTED'}))
    def fetch(*args,**kw):fetches.append(1);return b'synthetic',evidence
    monkeypatch.setattr(service.workflows,'provider',lambda *a:SimpleNamespace(fetch=fetch))
    monkeypatch.setattr('musicops.clients.qbt_live.live_client',lambda *a:qbt)
    service.workflows._requesting(job);assert job['stage']=='requesting' and not starts
    service.workflows._requesting(job);assert job['stage']=='requesting' and len(starts)==1
    service.workflows._requesting(job);assert job['stage']=='downloading' and fetches==[1]


def test_gui_prevents_overlapping_artist_requests_and_ignores_stale_reply(app,tmp_path,monkeypatch):
    window,client=window_for(tmp_path);pending=[]
    monkeypatch.setattr(window,'call',lambda cmd,payload,done=None,failed=None:pending.append(done))
    window.artist_fetch({'mode':'artist','query':'Artist'});window.artist_fetch({'mode':'artist','query':'Artist'})
    assert len(pending)==1 and window.artist_busy
    window.artist_generation+=1;pending[0]({'error':'stale'})
    assert window.artist_result is None
    window.artist_busy=False;window.artist_fetch({'mode':'artist','query':'Other'})
    pending[1]({'error':'failed'});assert not window.artist_busy and 'failed' in window.album_note.text()
    window.close()


def test_artist_gui_groups_and_restores_expansion(app,tmp_path):
    window,client=window_for(tmp_path);rows=[classify(resource(),'Artist')]
    result={'query':'Artist','albums':groups(rows,'Artist'),'candidates':rows,'source_outcomes':{},'message':'Synthetic','has_more':False}
    window.show_artist_results(result);root=window.artist_tree.topLevelItem(0);album=root.child(0);album.setExpanded(True);album.child(0).setExpanded(True)
    window.show_artist_results(result);album=window.artist_tree.topLevelItem(0).child(0)
    assert album.isExpanded() and album.child(0).isExpanded() and album.child(0).child(0).text(2)=='DicMusic'
    window.close();wait(app,window)
