"""Synthetic regressions for native paths, numbering and resource recovery."""
import copy,json
from pathlib import Path
import pytest
from mutagen.flac import FLAC
from musicops.media.numbering import number_tag,directory_disc
from musicops.media.identity_fields import credits_equal,title_keys,cover_pairs,release_pairs
from musicops.media.tags import read_tag_fields
from musicops.clients.http import FakeTransport,Response,PolicyBlocked
from musicops.config.schema import default_config
from test_workflows import service,flac,job,image
from test_task_controls import controls,album,control,read_job


@pytest.fixture(autouse=True)
def isolated_synthetic_environment(monkeypatch,tmp_path):
    # A mounted production container must not redirect synthetic fixtures to
    # its /media defaults or share the production vault master-key location.
    monkeypatch.delenv('MUSICOPS_CONTAINER',raising=False)
    monkeypatch.setenv('MUSICOPS_VAULT_KEY_FILE',str(tmp_path/'synthetic-vault.key'))


@pytest.mark.parametrize('value,expected',[('01',(1,None)),('02/03',(2,3)),('0',None),('2/1',None),('CNHiFi.COM',None),('1x',None),('1000',None)])
def test_disc_tag_accepts_zero_padding_but_rejects_garbage(value,expected):
    assert number_tag(value,999)==expected


def test_numbering_directory_hint_is_explicit_and_not_a_playlist_position():
    assert directory_disc({'original_path':'/music/Album/CD02 - Bonus/song.flac'})==2
    assert directory_disc({'original_path':'/music/Grammy 2024/62 Song.flac'}) is None
    assert directory_disc({'original_path':'/music/CD000/song.flac'}) is None


def test_joined_credits_and_known_numeric_prefix_do_not_invent_artists_or_titles():
    assert credits_equal(['A / B'],['A','B'])
    assert credits_equal(['AC/DC'],['AC/DC'])
    assert not credits_equal(['AC/DC'],['AC','DC','Other'])
    assert 'song' in title_keys({'raw_title':'01. Song','tags':{'tracknumber':['01/12']}})
    assert 'song' not in title_keys({'raw_title':'01. Song','tags':{'tracknumber':['62']}})
    assert 'song' not in title_keys({'raw_title':'01. Song'})


def test_artwork_article_alias_needs_every_track_and_duration():
    local=[{'raw_title':'Song','artists':['Band'],'duration':123}]
    tracks=[{'title':'Song','artists':['The Band'],'duration':123}]
    assert release_pairs(local,tracks) is None
    assert cover_pairs(local,tracks)
    assert cover_pairs(local,[{**tracks[0],'duration':None}]) is None
    assert cover_pairs(local,[{**tracks[0],'duration':128}]) is None
    assert cover_pairs(local,tracks*2) is None
    guest=[{'title':'Song (featuring Guest)','artists':['The Band','Guest'],'duration':123}]
    assert cover_pairs(local,guest)
    assert cover_pairs(local,[{**guest[0],'title':'Song (Live)'}]) is None
    assert cover_pairs(local,[{**guest[0],'artists':['Other Band']}]) is None


def prepared(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'numbering';flac(source/'song.flac')
    row=job(service,sources=source);service.workflows._metadata(row)
    return service.store.get('evidence','workflow:'+row['job_id'])


@pytest.mark.parametrize('disc',['01/02','01'])
def test_partial_library_repair_preserves_declared_disc_total(service,disc):
    row=prepared(service);p=Path(row['prepared'][0]['path']);tags=FLAC(p);tags['discnumber']=[disc];tags['totaldiscs']=['2'];tags.save()
    row['prepared'][0]['tags']=read_tag_fields(p);row['scene']='complete'
    service.workflows._enrich(row)
    saved=service.store.get('evidence','workflow:'+row['job_id'])
    assert saved['stage']=='validating' and saved['total_discs']==2 and saved['missing_discs']==[2]
    assert read_tag_fields(p)['discnumber']==['1/2']
    row['scene']='playlist'
    with pytest.raises(PolicyBlocked,match='缺少部分光盘'):service.workflows._enrich(row)


def test_missing_track_is_not_fabricated_for_a_one_file_album(service,monkeypatch):
    row=prepared(service);p=Path(row['prepared'][0]['path']);tags=FLAC(p);del tags['tracknumber'];tags.save()
    row['prepared'][0]['tags']=read_tag_fields(p);monkeypatch.setattr(service.workflows,'_catalog_fill',lambda *args,**kwargs:None)
    service.workflows._enrich(row)
    saved=service.store.get('evidence','workflow:'+row['job_id'])
    assert saved['stage']=='needs_review' and saved['review_kind']=='numbering'
    assert not read_tag_fields(p).get('tracknumber')


def test_invalid_disc_routes_to_actionable_numbering_review(service,monkeypatch):
    row=prepared(service);p=Path(row['prepared'][0]['path']);tags=FLAC(p);tags['discnumber']=['invalid'];tags.save()
    row['prepared'][0]['tags']=read_tag_fields(p);monkeypatch.setattr(service.workflows,'_catalog_fill',lambda *args,**kwargs:None)
    service.workflows._enrich(row)
    assert service.store.get('evidence','workflow:'+row['job_id'])['review_kind']=='numbering'


def test_transient_lyrics_retry_is_delayed_bounded_and_refreshes_failed_rows(service,monkeypatch):
    row=prepared(service);row['config']['lyrics']['primary']['base_url']='https://lyrics.example.test';calls=[]
    def resources(query,cfg):
        calls.append(query);return {'lyrics':{'state':'TRANSIENT_ERROR','errors':[{'retry_after_seconds':80}]},'candidates':[]}
    monkeypatch.setattr(service,'_real_resources',resources)
    for attempt in (1,2):
        service.workflows._enrich(row);row=service.store.get('evidence','workflow:'+row['job_id'])
        assert row['stage']=='enrich' and row['resource_retry_count']==attempt and row['next_poll']>0
    service.workflows._enrich(row);row=service.store.get('evidence','workflow:'+row['job_id'])
    assert row['review_kind']=='resources' and len(calls)==3
    assert [query['refresh'] for query in calls]==[False,True,True]


def test_auth_error_is_not_blindly_retried(service,monkeypatch):
    row=prepared(service);row['config']['lyrics']['primary']['base_url']='https://lyrics.example.test'
    monkeypatch.setattr(service,'_real_resources',lambda *args:{'lyrics':{'state':'AUTH_FAILED'},'candidates':[]})
    service.workflows._enrich(row);saved=service.store.get('evidence','workflow:'+row['job_id'])
    assert saved['review_kind']=='resources' and not saved.get('resource_retry_count')


@pytest.mark.parametrize('kind',['numbering','cover','resources'])
def test_bulk_retry_prepared_resources_uses_current_settings(controls,kind):
    row=album(controls,stage='needs_review',review_kind=kind,prepared=[{'path':'synthetic.flac'}])
    current=controls.config.current();cfg=copy.deepcopy(current['values']);cfg['ai']['model']='new-model';controls.config.save(cfg,current['version'])
    assert control(controls,'retry',[row['job_id']])['succeeded']==1
    saved=read_job(controls,row['job_id'])
    assert saved['stage']=='enrich' and saved['config']['ai']['model']=='new-model' and saved['resource_retry_count']==0


def test_resource_retry_never_mutates_prepared_audio_owned_by_a_transaction(controls):
    row=album(controls,stage='needs_review',review_kind='cover',prepared=[{'path':'synthetic.flac'}])
    controls.store.put('transactions','existing',{'plan_id':'workflow:'+row['job_id'],'state':'PREPARED'})
    result=control(controls,'retry',[row['job_id']])
    assert result['skipped']==1 and '事务' in result['results'][0]['message']


def test_album_only_cover_search_verifies_tracks_and_scopes_shared_cookie(monkeypatch,tmp_path):
    from musicops.providers.cover_download import evidence_cover
    cfg=default_config(tmp_path);cfg['offline']=False;cfg['providers']['netease']['enabled']=True;cfg['netease']['credential_ref']='private-ref'
    local=[{'raw_title':'Song','artists':['Band'],'duration':123}];seen=[]
    albums=[{'id':i,'name':'Album','artist':{'name':'Other'}} for i in range(100,110)]+[{'id':42,'name':'Album','artist':{'name':'The Band'}}]
    body={'code':200,'album':{'id':42,'name':'Album','artist':{'name':'The Band'},'size':1,'picUrl':'https://p1.music.126.net/front.jpg'},'songs':[{'id':99,'name':'Song','artists':[{'name':'The Band'}],'album':{'id':42,'name':'Album'},'duration':123000,'no':1,'cd':'01'}]}
    class Transport:
        def request(self,method,url,**kwargs):
            seen.append((url,kwargs));return Response(200,json.dumps({'code':200,'result':{'albums':albums}} if 'search' in url else body).encode())
        def close(self):pass
    transport=Transport()
    monkeypatch.setattr('musicops.clients.http.SafeHttpTransport',lambda *args,**kwargs:transport)
    monkeypatch.setattr('musicops.providers.cover_download._image',lambda *args:image())
    secrets=[]
    def getter(ref,host):secrets.append((ref,host));return json.dumps([{'name':'MUSIC_U','value':'synthetic-cookie','domain':'music.163.com','path':'/'}])
    art=evidence_cover('Album',['Band'],local,cfg,getter)
    assert art and art.provenance['album_id']=='42' and art.provenance['scope']=='all_tracks_and_durations'
    assert all(host=='music.163.com' for _,host in secrets)
    assert all(url.startswith('https://music.163.com/') for url,_ in seen)
    body['songs'][0]['duration']=150000
    assert evidence_cover('Album',['Band'],local,cfg,getter) is None


def test_verified_cover_group_does_not_select_a_pressing_or_spend_ai_tokens():
    from types import SimpleNamespace
    from musicops.app.pipeline import Workflows
    track={'title':'Song','artists':['The Band'],'duration':123,'position':1,'disc':1}
    candidates=[{'source':'musicbrainz','source_id':str(i),'album':'Album'} for i in range(2)]
    details={str(i):{**candidate,'artists':['The Band'],'tracklist':[track],'tracklist_complete':True,'release_group_id':'verified-group'} for i,candidate in enumerate(candidates)}
    resolver=SimpleNamespace(identify=lambda query:{'candidates':candidates},resolve_candidate=lambda source,key:{'candidate':details[key]})
    ai=[];flow=object.__new__(Workflows);flow.store=SimpleNamespace(get=lambda *args:None)
    flow.s=SimpleNamespace(_checkpoint=lambda *args:None,_catalog_resolver=lambda cfg:resolver,_recognize_metadata=lambda *args,**kwargs:ai.append(True) or kwargs['found'])
    local=[{'raw_title':'Song','album':'Album','artists':['Band'],'duration':123}]
    proposal=flow._catalog_fill({'scene':'complete','config':{},'job_id':'fixture'},local,for_cover=True)
    assert proposal=={'source':'musicbrainz','cover_release_group_id':'verified-group'} and not ai
    details['0']['artists']=details['1']['artists']=['Other Band']
    assert flow._catalog_fill({'scene':'complete','config':{},'job_id':'fixture'},local,for_cover=True) is None
