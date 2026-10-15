"""Synthetic contracts plus fail-closed completeness and credential boundaries."""
import json
import pytest

from musicops.clients.http import FakeTransport,HttpError,Response
from musicops.providers.playlist_domestic import DomesticPlaylistProvider,HOSTS,DOMESTIC_HOSTS


def response(data,status=200,url=''):
    return Response(status,json.dumps(data,ensure_ascii=False).encode(),url=url)


def song(platform,sid='1',**changes):
    values={
        'qqmusic':{'mid':sid,'name':'Song','singer':[{'name':'Artist'}],'album':{'name':'Album','mid':'album'},'interval':120},
        'qishui':{'entity':{'track_wrapper':{'track':{'id':sid,'name':'Song','artists':[{'name':'Artist'}],'album':{'name':'Album','id':'album'},'duration':120000}}}},
        'kuwo':{'rid':sid,'name':'Song','artist':'Artist','album':'Album','albumid':'album','duration':120},
        'migu':{'contentId':sid,'songName':'Song','singerList':[{'name':'Artist'}],'albumList':[{'id':'album','name':'Album'}],'duration':'02:00'},
        'kugou':{'hash':sid,'filename':'Artist - Song','album_name':'Album','album_id':'album','timelen':120000},
    }[platform]
    values.update(changes);return values


def page(platform,items,total,name='Playlist',**changes):
    if platform=='qqmusic':data={'code':0,'cdlist':[{'disstid':'123','dissname':name,'songnum':total,'songlist':items,**changes}]}
    elif platform=='qishui':data={'status_code':0,'playlist':{'id':'123','name':name,'count_tracks':total},'media_resources':items,**changes}
    elif platform=='kuwo':data={'code':200,'data':{'id':'123','name':name,'total':total,'musicList':items,**changes}}
    elif platform=='migu':data={'code':'000000','data':{'playlistName':name,'totalCount':total,'songList':items,**changes}}
    else:data={'status':1,'data':{'specialname':name,'total':total,'info':items,**changes}}
    return response(data)


def provider(platform,responses,**kwargs):
    transport=FakeTransport(responses);return DomesticPlaylistProvider(platform,transport,**kwargs),transport


def refresh(p,pid='123'):
    return p.refresh({'platform':p.platform,'playlist_id':pid,'url':'https://untrusted.invalid/not-used'})


@pytest.mark.parametrize('platform',list(HOSTS))
def test_domestic_normalization_preserves_real_platform_and_duration(platform):
    p,_=provider(platform,[page(platform,[song(platform)],1)])
    result=refresh(p);snapshot=result['snapshot'];target=snapshot['songs'][0]
    assert result['status']=='OK' and snapshot['complete'] and snapshot['declared_count']==1
    assert target['target_id']==platform+':1' and target['source']==platform and target['source_song_id']=='1'
    assert target['duration']==120 and target['album']=='Album' and target['artists']==['Artist'] and target['metadata_complete']
    assert target['evidence_level']=='live_readonly' and target['download_eligibility']=='UNKNOWN'


@pytest.mark.parametrize('platform',['qqmusic','kuwo','migu','kugou'])
def test_declared_counts_drive_full_pagination_even_small_pages(platform):
    p,t=provider(platform,[page(platform,[song(platform,'1')],2),page(platform,[song(platform,'2')],2)])
    result=refresh(p)
    assert result['status']=='OK' and len(result['snapshot']['songs'])==2 and len(t.requests)==2
    if platform=='qqmusic':assert t.requests[1]['params']['song_begin']==1
    elif platform=='migu':assert t.requests[1]['params']['pageNo']==2
    else:assert t.requests[1]['params'].get('pn',t.requests[1]['params'].get('page'))==2


def test_qishui_uses_next_cursor_and_reconciles_all_resources():
    p,t=provider('qishui',[page('qishui',[song('qishui','1')],2,has_more=True,next_cursor='next'),page('qishui',[song('qishui','2')],2,has_more=False,next_cursor='')])
    result=refresh(p)
    assert result['snapshot']['complete'] and len(result['snapshot']['songs'])==2
    assert json.loads(t.requests[1]['content'])['cursor']=='next'


@pytest.mark.parametrize('platform',list(HOSTS))
def test_truncation_never_claims_complete(platform):
    extra={'has_more':False} if platform=='qishui' else {}
    p,_=provider(platform,[page(platform,[song(platform)],3,**extra),page(platform,[],3,**extra)])
    result=refresh(p)
    assert result['status']=='SNAPSHOT_INCOMPLETE' and not result['snapshot']['complete']
    assert result['snapshot']['songs'][0]['source']==platform


def test_replayed_page_and_count_change_are_not_completed():
    p,t=provider('qqmusic',[page('qqmusic',[song('qqmusic')],2),page('qqmusic',[song('qqmusic')],2)])
    result=refresh(p)
    assert result['status']=='SNAPSHOT_INCOMPLETE' and len(t.requests)==2
    assert any(w['error_class']=='PAGINATION_STALLED' for w in result['snapshot']['warnings'])
    p,_=provider('migu',[page('migu',[song('migu','1')],2),page('migu',[song('migu','2')],3),page('migu',[song('migu','3')],3)])
    assert refresh(p)['snapshot']['complete'] is False


def test_unknown_count_and_broken_cursor_are_partial():
    p,_=provider('qishui',[page('qishui',[song('qishui')],None,has_more=True,next_cursor='0')])
    result=refresh(p)
    assert result['status']=='SNAPSHOT_INCOMPLETE'
    assert {w['error_class'] for w in result['snapshot']['warnings']}=={'COUNT_UNKNOWN','PAGINATION_STALLED'}


def test_missing_album_and_artist_are_preserved_without_inventing_metadata():
    p,_=provider('kuwo',[page('kuwo',[song('kuwo',album='',artist='')],1)])
    result=refresh(p);target=result['snapshot']['songs'][0]
    assert target['album']=='' and target['artists']==[] and not target['metadata_complete']
    assert target['metadata_missing_fields']==['artists','album']
    assert any(w['error_class']=='METADATA_MISSING' for w in result['snapshot']['warnings'])


def test_unknown_track_ids_are_counted_as_missing_not_synthetic_ids():
    p,_=provider('kuwo',[page('kuwo',[song('kuwo',rid='')],1)])
    result=refresh(p)
    assert result['status']=='SNAPSHOT_INCOMPLETE' and result['snapshot']['songs']==[]


def test_qq_jsonp_and_direct_official_fallback():
    data={'code':0,'cdlist':[{'songnum':1,'songlist':[song('qqmusic')]}]}
    p,_=provider('qqmusic',[Response(200,('callback('+json.dumps(data)+');').encode())])
    assert refresh(p)['status']=='OK'
    p,t=provider('qqmusic',[HttpError('NETWORK_ERROR'),response({'playlist':{'code':0,'data':{'dirinfo':{'songnum':1},'songlist':[song('qqmusic')]}}})])
    assert refresh(p)['status']=='OK' and t.requests[1]['url']=='https://u.y.qq.com/cgi-bin/musicu.fcg'


def test_kugou_h5_preview_never_fakes_full_ten_song_playlist():
    data={'info':{'listinfo':{'name':'Public preview','count':30},'songs':[song('kugou',str(i)) for i in range(1,11)]}}
    p,t=provider('kugou',[Response(200,('<script>window.$output = '+json.dumps(data)+';</script>').encode())])
    result=refresh(p,'gcid_example')
    assert result['status']=='SNAPSHOT_INCOMPLETE' and len(result['snapshot']['songs'])==10
    assert result['snapshot']['declared_count']==30 and len(t.requests)==1


def test_kugou_desktop_preview_parses_json_without_executing_script():
    text='<title>Collection</title><script>var data='+json.dumps([song('kugou')])+';throw dangerous()</script>'
    p,_=provider('kugou',[HttpError('NETWORK_ERROR'),Response(200,text.encode())])
    result=refresh(p,'collection_3_1_2_0')
    assert result['status']=='SNAPSHOT_INCOMPLETE' and result['snapshot']['declared_count'] is None
    assert result['snapshot']['songs'][0]['raw_title']=='Song'


def test_kugou_anonymous_signed_collection_paginates_without_user_credentials():
    p,t=provider('kugou',[
        response({'status':1,'data':{'count':2,'info':[song('kugou','1')]}}),
        response({'status':1,'data':{'specialname':'Public collection'}}),
        response({'status':1,'data':{'count':2,'info':[song('kugou','2')]}}),
    ])
    result=refresh(p,'collection_3_1_2_0')
    assert result['status']=='OK' and len(result['snapshot']['songs'])==2
    assert result['snapshot']['contract']=='kugou-public-collection-v2'
    assert t.requests[0]['params']['uid']=='0' and t.requests[0]['params']['token']==''
    assert len(t.requests[0]['params']['signature'])==32 and t.requests[2]['params']['page']=='2'
    assert all('Cookie' not in request['headers'] for request in t.requests)


def test_kugou_gcid_can_resolve_public_collection_and_keep_original_identity():
    data={'encode_gic':'collection_3_1_2_0','info':{'listinfo':{'name':'Preview','count':2},'songs':[song('kugou','1')]}}
    p,_=provider('kugou',[
        Response(200,('window.$output='+json.dumps(data)+';').encode()),
        response({'status':1,'data':{'count':2,'info':[song('kugou','1'),song('kugou','2')]}}),
        response({'status':1,'data':{'specialname':'Full collection'}}),
    ])
    result=refresh(p,'gcid_example')
    assert result['status']=='OK' and result['snapshot']['playlist_id']=='gcid_example'
    assert result['snapshot']['name']=='Full collection'


def test_cookies_are_domain_and_path_scoped_with_no_external_host_requests():
    jar=json.dumps([{'domain':'.qq.com','name':'allowed','value':'fixture','path':'/'},{'domain':'third-party.invalid','name':'private','value':'never','path':'/'}])
    reads=[]
    def getter(ref,host):reads.append((ref,host));return jar
    p,t=provider('qqmusic',[page('qqmusic',[song('qqmusic')],1)],config={'credential_ref':'vault://fixture/qq'},secret_getter=getter)
    assert refresh(p)['status']=='OK' and reads==[('vault://fixture/qq','c.y.qq.com')]
    assert t.requests[0]['headers']['Cookie']=='allowed=fixture'
    assert 'untrusted.invalid' not in t.requests[0]['url'] and t.requests[0]['max_response_bytes']<=20_000_000
    assert set(HOSTS['qqmusic']).issubset(DOMESTIC_HOSTS)


@pytest.mark.parametrize('status',[401,403])
def test_login_required_and_network_failure_remain_explicit(status):
    p,_=provider('migu',[response({},status)])
    result=refresh(p)
    assert result['status']=='AUTH_REQUIRED' and not result['snapshot']['complete']


def test_failure_after_first_page_keeps_only_verified_visible_tracks():
    p,_=provider('kuwo',[page('kuwo',[song('kuwo')],2),HttpError('RATE_LIMITED')])
    result=refresh(p)
    assert result['status']=='SNAPSHOT_INCOMPLETE' and result['error_class']=='RATE_LIMITED'
    assert len(result['snapshot']['songs'])==1 and not result['snapshot']['complete']


def test_checkpoint_cancellation_propagates_and_never_becomes_partial():
    class Cancelled(RuntimeError):pass
    def checkpoint(_):raise Cancelled()
    p,t=provider('migu',[],checkpoint=checkpoint)
    with pytest.raises(Cancelled):refresh(p)
    assert t.requests==[]


def test_redirects_and_response_origin_changes_are_rejected_without_forwarding():
    p,t=provider('migu',[Response(302,b'',{'location':'https://third-party.invalid/'})])
    assert refresh(p)['status']=='ACCESS_BLOCKED' and len(t.requests)==1
    p,t=provider('migu',[response({},url='https://third-party.invalid/')])
    assert refresh(p)['status']=='ACCESS_BLOCKED' and len(t.requests)==1


def test_empty_shell_is_not_a_verified_empty_playlist():
    p,_=provider('kuwo',[page('kuwo',[],0,name='')])
    assert refresh(p)['status']=='NOT_FOUND'
    p,_=provider('kuwo',[page('kuwo',[],0,name='An explicitly named empty playlist')])
    assert refresh(p)['status']=='OK'
