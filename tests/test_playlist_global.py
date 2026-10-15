import json
import pytest
from musicops.clients.http import FakeTransport,Response,HttpError
from musicops.providers.playlist_global import GlobalPlaylistProvider,HOSTS

SP='3cEYpjA9oz9GiPac4AsH4n'
AP='pl.f4d106fed2bd41149aaacabb233eb5eb'
YT='PLBCF2DAC6FFB574DE'

def response(data,status=200):return Response(status,json.dumps(data).encode())
def page_html(scripts):return Response(200,''.join('<script '+attrs+'>'+json.dumps(value)+'</script>' for attrs,value in scripts).encode())
def provider(platform,responses,credential=None,checkpoint=None):
    transport=FakeTransport(responses);hosts=[]
    def secret(ref,host):hosts.append(host);return credential
    return GlobalPlaylistProvider(platform,transport,{'credential_ref':'vault://synthetic/platform' if credential is not None else ''},secret,checkpoint),transport,hosts
def spotify_track(index):return {'id':str(index).zfill(22),'type':'track','name':'Song '+str(index),'artists':[{'name':'Artist'}],'album':{'id':'album'+str(index),'name':'Album'},'duration_ms':180001}
def apple_song(index):return {'id':str(index),'type':'songs','attributes':{'name':'Song','artistName':'Artist','albumName':'Album','durationInMillis':180500}}

def test_spotify_current_items_contract_pages_past_100_and_rechecks_snapshot():
    pages=[response({'id':SP,'name':'Public fixture','snapshot_id':'a'})]
    for offset,size in ((0,50),(50,50),(100,1)):
        pages.append(response({'items':[{'item':spotify_track(i+1)} for i in range(offset,offset+size)],'offset':offset,'total':101,'next':f'https://api.spotify.com/v1/playlists/{SP}/items?offset={offset+50}&limit=50' if offset<100 else None}))
    pages.append(response({'id':SP,'snapshot_id':'a'}));p,t,hosts=provider('spotify',pages,'synthetic-token')
    result=p.refresh({'platform':'spotify','playlist_id':SP})
    assert result['status']=='OK' and len(result['snapshot']['songs'])==101
    assert all(song['source']=='spotify' and song['id']==song['target_id']=='spotify:'+song['source_song_id'] for song in result['snapshot']['songs'])
    assert result['snapshot']['songs'][0]['duration']==180.001 and hosts==['api.spotify.com']
    assert [r['params'].get('offset') for r in t.requests if r['url'].endswith('/items')]==[0,50,100]
    assert all(r['max_response_bytes']==20_000_000 for r in t.requests)

def test_spotify_repeated_or_cross_host_page_never_leaks_authorization():
    p,t,_=provider('spotify',[response({'id':SP}),response({'items':[{'item':spotify_track(1)}],'offset':0,'total':2,'next':'https://evil.invalid/steal?offset=1'})],'synthetic-token')
    assert p.refresh({'playlist_id':SP})['status']=='ACCESS_BLOCKED'
    assert len(t.requests)==2 and all('api.spotify.com' in r['url'] for r in t.requests)

def test_spotify_public_embed_is_explicit_preview_even_if_count_matches():
    entity={'uri':'spotify:playlist:'+SP,'title':'Visible only','totalTracks':1,'trackList':[{'uri':'spotify:track:'+str(1).zfill(22),'title':'Song','subtitle':'Artist','duration':180000}]}
    p,t,hosts=provider('spotify',[page_html([('id="__NEXT_DATA__" type="application/json"',{'props':{'pageProps':{'state':{'data':{'entity':entity}}}}})])])
    result=p.refresh({'playlist_id':SP})
    assert result['status']=='SNAPSHOT_INCOMPLETE' and len(result['snapshot']['songs'])==1
    assert not result['snapshot']['songs'][0]['metadata_complete'] and not hosts
    assert 'Authorization' not in t.requests[0]['headers']

def test_spotify_missing_tracks_are_partial_and_token_errors_not_exposed():
    p,_,_=provider('spotify',[response({'id':SP}),response({'items':[{'item':spotify_track(1)},{'item':None}],'offset':0,'total':2,'next':None})],'synthetic-token')
    result=p.refresh({'playlist_id':SP});assert result['status']=='SNAPSHOT_INCOMPLETE' and len(result['snapshot']['songs'])==1
    assert 'synthetic-token' not in json.dumps(result)

def test_spotify_partial_survives_later_network_failure():
    p,_,_=provider('spotify',[response({'id':SP}),response({'items':[{'track':spotify_track(1)}],'offset':0,'total':2,'next':f'https://api.spotify.com/v1/playlists/{SP}/items?offset=1'}),HttpError('NETWORK_ERROR')],'synthetic-token')
    result=p.refresh({'playlist_id':SP});assert result['status']=='SNAPSHOT_INCOMPLETE' and len(result['snapshot']['songs'])==1

def test_apple_api_relationship_pagination_is_exhausted_without_fake_100_limit():
    p,t,hosts=provider('applemusic',[response({'data':[{'id':AP,'attributes':{'name':'Album fixture'},'relationships':{'tracks':{'data':[apple_song(1)],'next':f'/v1/catalog/us/playlists/{AP}/tracks?offset=1'}}}]}),response({'data':[apple_song(2)]})],'developer-jwt')
    result=p.refresh({'platform':'applemusic','playlist_id':AP,'storefront':'us'})
    assert result['status']=='OK' and result['snapshot']['declared_count']==2
    assert hosts==['api.music.apple.com'] and t.requests[0]['headers']['Authorization']=='Bearer developer-jwt'

def apple_public_rows(count=2):
    return [{'title':'Song '+str(i),'duration':180200,'contentDescriptor':{'kind':'song','identifiers':{'storeAdamID':str(i+1)}},'subtitleLinks':[{'title':'Artist','segue':{'destination':{'contentDescriptor':{'kind':'artist'}}}}],'tertiaryLinks':[{'title':'Album','segue':{'destination':{'contentDescriptor':{'kind':'album','identifiers':{'storeAdamID':'99'}}}}}]} for i in range(count)]

def apple_public_response(declared=2,rows=2):
    return page_html([('type="application/ld+json"',{'@type':'MusicPlaylist','url':'https://music.apple.com/us/playlist/'+AP,'name':'Fixture','numTracks':declared}),('type="application/json" id="serialized-server-data"',{'data':[{'data':{'sections':[{'id':'track-list - '+AP,'items':apple_public_rows(rows)}]}}]})])

def test_apple_public_exact_playlist_section_uses_structured_album_and_artist():
    p,_,_=provider('applemusic',[apple_public_response()]);result=p.refresh({'playlist_id':AP,'storefront':'us'})
    assert result['status']=='OK' and len(result['snapshot']['songs'])==2
    song=result['snapshot']['songs'][0]
    assert song['artists']==['Artist'] and song['album']=='Album' and song['source_album_id']=='99' and song['duration']==180.2 and song['metadata_complete']

def test_apple_public_count_mismatch_is_partial():
    p,_,_=provider('applemusic',[apple_public_response(200,2)]);result=p.refresh({'playlist_id':AP,'storefront':'us'})
    assert result['status']=='SNAPSHOT_INCOMPLETE' and result['snapshot']['declared_count']==200

def test_apple_private_library_and_invalid_storefront_are_not_requested():
    p,t,_=provider('applemusic',[])
    assert p.refresh({'playlist_id':'p.private-library'})['status']=='API_CONTRACT_ERROR'
    assert p.refresh({'playlist_id':AP,'storefront':'../x'})['status']=='API_CONTRACT_ERROR' and not t.requests

def youtube_item(sid):return {'contentDetails':{'videoId':sid},'snippet':{'title':'Video title','resourceId':{'videoId':sid}}}
def youtube_video(sid):return {'id':sid,'snippet':{'title':'Video title','channelTitle':'Uploader, not singer'},'contentDetails':{'duration':'PT3M2S'}}

def test_youtube_api_follows_tokens_and_video_fields_do_not_invent_album_artist():
    ids=['abcdefghijk','ABCDEFGHIJK'];p,t,hosts=provider('youtube',[
        response({'items':[{'id':YT,'snippet':{'title':'Fixture'},'contentDetails':{'itemCount':2}}]}),
        response({'items':[youtube_item(ids[0])],'pageInfo':{'totalResults':2},'nextPageToken':'next'}),response({'items':[youtube_video(ids[0])]}),
        response({'items':[youtube_item(ids[1])],'pageInfo':{'totalResults':2}}),response({'items':[youtube_video(ids[1])]})
    ],'synthetic-api-key')
    result=p.refresh({'playlist_id':YT});assert result['status']=='OK' and len(result['snapshot']['songs'])==2
    assert all(s['artists']==[] and s['album']=='' and s['source_album_id'] is None and not s['metadata_complete'] and s['duration']==182 for s in result['snapshot']['songs'])
    assert t.requests[3]['params']['pageToken']=='next' and hosts==['www.googleapis.com']
    assert all(r['headers']['X-Goog-Api-Key']=='synthetic-api-key' and 'key' not in r['params'] for r in t.requests)

def test_youtube_private_or_deleted_item_makes_snapshot_partial():
    p,_,_=provider('youtube',[response({'items':[{'id':YT,'contentDetails':{'itemCount':1}}]}),response({'items':[youtube_item('abcdefghijk')],'pageInfo':{'totalResults':1}}),response({'items':[]})],'synthetic-api-key')
    result=p.refresh({'playlist_id':YT});assert result['status']=='SNAPSHOT_INCOMPLETE' and not result['snapshot']['songs']

def youtube_initial(continuation=True,total=2):
    entries=[{'playlistVideoRenderer':{'videoId':'abcdefghijk','title':{'runs':[{'text':'Video'}]},'lengthText':{'simpleText':'3:02'},'shortBylineText':{'runs':[{'text':'Uploader'}]}}}]
    if continuation:entries.append({'continuationItemRenderer':{'continuationEndpoint':{'continuationCommand':{'token':'continue'}}}})
    return {'header':{'playlistHeaderRenderer':{'title':{'simpleText':'Fixture'},'numVideosText':{'simpleText':str(total)+' videos'}}},'contents':{'playlistVideoListRenderer':{'contents':entries}}}

def test_youtube_public_readonly_continuation_excludes_recommendations():
    initial=youtube_initial();initial['recommendations']={'videoRenderer':{'videoId':'BADBADBADBA','title':{'simpleText':'Do not import'}}}
    cfg={'INNERTUBE_API_KEY':'public-api-key','INNERTUBE_CONTEXT':{'client':{'clientName':'WEB','clientVersion':'2.0'}}}
    text='<script>var ytInitialData = '+json.dumps(initial)+';</script><script>ytcfg.set('+json.dumps(cfg)+');</script>'
    second={'onResponseReceivedActions':[{'appendContinuationItemsAction':{'continuationItems':[{'playlistVideoRenderer':{'videoId':'ABCDEFGHIJK','title':{'simpleText':'Video 2'},'lengthText':{'simpleText':'1:00'}}}]}}]}
    p,t,hosts=provider('youtube',[Response(200,text.encode()),response(second)]);result=p.refresh({'playlist_id':YT})
    assert result['status']=='OK' and len(result['snapshot']['songs'])==2 and not hosts
    assert [r['method'] for r in t.requests]==['GET','POST'] and t.requests[1]['url']=='https://www.youtube.com/youtubei/v1/browse'
    assert json.loads(t.requests[1]['content'])['continuation']=='continue'

def test_youtube_public_missing_continuation_context_is_partial():
    text='<script>var ytInitialData = '+json.dumps(youtube_initial())+';</script>'
    p,t,_=provider('youtube',[Response(200,text.encode())]);result=p.refresh({'playlist_id':YT})
    assert result['status']=='SNAPSHOT_INCOMPLETE' and len(result['snapshot']['songs'])==1 and len(t.requests)==1

def test_youtube_music_uses_artist_album_browse_links_only():
    def column(text):return {'musicResponsiveListItemFlexColumnRenderer':{'text':{'runs':text}}}
    music={'playlistItemData':{'videoId':'abcdefghijk'},'flexColumns':[column([{'text':'Song'}]),column([{'text':'Artist','navigationEndpoint':{'browseEndpoint':{'browseId':'UCauthor','browseEndpointContextSupportedConfigs':{'browseEndpointContextMusicConfig':{'pageType':'MUSIC_PAGE_TYPE_ARTIST'}}}}},{'text':' • 10 million views'}]),column([{'text':'Album','navigationEndpoint':{'browseEndpoint':{'browseId':'MPREalbum'}}}])],'fixedColumns':[{'musicResponsiveListItemFixedColumnRenderer':{'text':{'simpleText':'3:00'}}}]}
    p,_,_=provider('youtube',[]);song=p._youtube_row(music)
    assert song['artists']==['Artist'] and song['album']=='Album' and song['metadata_complete']

def test_youtube_music_channel_link_does_not_invent_artist_credit():
    music={'playlistItemData':{'videoId':'abcdefghijk'},'flexColumns':[{'musicResponsiveListItemFlexColumnRenderer':{'text':{'runs':[{'text':'Song'}]}}},{'musicResponsiveListItemFlexColumnRenderer':{'text':{'runs':[{'text':'Uploader','navigationEndpoint':{'browseEndpoint':{'browseId':'UCchannel'}}}]}}}]}
    p,_,_=provider('youtube',[]);song=p._youtube_row(music)
    assert song['artists']==[] and not song['metadata_complete']

def bili_page(number,more,total=2):return response({'code':0,'data':{'info':{'id':12345,'title':'Fixture favourites','media_count':total},'medias':[{'id':number,'type':2,'bvid':'BV'+str(number).zfill(10),'title':'Music video','duration':180,'upper':{'name':'Uploader'}}],'has_more':more}})

def test_bilibili_pages_are_complete_but_video_metadata_requires_review():
    p,t,_=provider('bilibili',[bili_page(1,True),bili_page(2,False)]);result=p.refresh({'playlist_id':'12345'})
    assert result['status']=='OK' and len(result['snapshot']['songs'])==2
    assert all(song['artists']==[] and song['album']=='' and not song['metadata_complete'] and song['source_uploader']=='Uploader' for song in result['snapshot']['songs'])
    assert [r['params']['pn'] for r in t.requests]==[1,2]

def test_bilibili_repeated_page_cannot_satisfy_count_by_duplicate_rows():
    p,t,_=provider('bilibili',[bili_page(1,True),bili_page(1,False)]);result=p.refresh({'playlist_id':'12345'})
    assert result['status']=='SNAPSHOT_INCOMPLETE' and len(result['snapshot']['songs'])==1 and len(t.requests)==2

def test_bilibili_cookie_is_bound_to_api_host_and_private_errors_do_not_expose_it():
    cookie=json.dumps([{'name':'SESSDATA','value':'synthetic-session','domain':'.bilibili.com','path':'/'}]);p,t,hosts=provider('bilibili',[response({'code':-403})],cookie)
    result=p.refresh({'playlist_id':'12345'});assert result['status']=='AUTH_FAILED' and hosts==['api.bilibili.com']
    assert t.requests[0]['headers']['Cookie']=='SESSDATA=synthetic-session' and 'synthetic-session' not in json.dumps(result)

@pytest.mark.parametrize('platform,pid',[('spotify',SP),('applemusic',AP),('youtube',YT),('bilibili','12345')])
def test_redirect_and_oversized_response_are_not_accepted(platform,pid):
    p,_,_=provider(platform,[Response(302,b'',{'location':'https://evil.invalid/'})])
    assert p.refresh({'playlist_id':pid})['status']=='ACCESS_BLOCKED'
    p,_,_=provider(platform,[Response(200,b'x'*20_000_001)])
    assert p.refresh({'playlist_id':pid})['status']=='INVALID_PAYLOAD'

def test_checkpoints_abort_without_eating_cancellation():
    class Cancelled(RuntimeError):pass
    def checkpoint(_):raise Cancelled()
    p,_,_=provider('spotify',[],checkpoint=checkpoint)
    with pytest.raises(Cancelled):p.refresh({'playlist_id':SP})

def test_foreign_numeric_ids_keep_source_namespace():
    p,_,_=provider('bilibili',[]);song=p._track('12345','Video',duration=120)
    assert song['id']==song['target_id']=='bilibili:12345' and song['source_song_id']=='12345'
    assert song['album']=='' and song['source_album_id'] is None and song['source']!='netease'
