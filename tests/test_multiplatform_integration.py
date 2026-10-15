import copy,json
from pathlib import Path
import pytest
from musicops.app.service import AppService
from musicops.app.playlists import _song,refresh_platform_playlists
from musicops.clients.http import FakeTransport,Response,PolicyBlocked
from musicops.providers.playlist_input import parse_playlist_locator,resolve_share
from musicops.providers.netease_match import netease_reference,resolve_netease_target
from musicops.providers.netease_download import download_track


@pytest.fixture
def service(tmp_path):
    instance=AppService(tmp_path/'profile',offline=False);instance.stop.set();instance.thread.join(5)
    current=instance.config.current();cfg=copy.deepcopy(current['values']);cfg['paths']['library']=str(tmp_path/'music');Path(cfg['paths']['library']).mkdir()
    instance.config.save(cfg,current['version'])
    return instance


@pytest.mark.parametrize('platform,url,pid',[
    ('netease','https://music.163.com/#/playlist?id=123','123'),
    ('qqmusic','https://y.qq.com/n/ryqq/playlist/123','123'),
    ('qishui','https://music.douyin.com/qishui/playlist?playlist_id=123','123'),
    ('kugou','https://www.kugou.com/yy/special/single/123.html','123'),
    ('kuwo','https://www.kuwo.cn/playlist_detail/123','123'),
    ('migu','https://music.migu.cn/v3/music/playlist/123','123'),
    ('bilibili','https://space.bilibili.com/1/favlist?fid=123','123'),
    ('applemusic','https://music.apple.com/us/playlist/fixture/pl.abc123','pl.abc123'),
    ('spotify','https://open.spotify.com/playlist/1234567890123456789012?si=public','1234567890123456789012'),
    ('youtube','https://music.youtube.com/playlist?list=PLabcdefghijk','PLabcdefghijk'),
])
def test_ten_platform_share_text_detects_without_network(platform,url,pid):
    result=parse_playlist_locator('分享一个歌单 '+url+'。')[0]
    assert result['platform']==platform and result['playlist_id']==pid
    if platform=='applemusic':assert '/fixture/' in result['url']


@pytest.mark.parametrize('url',['https://music.163.com.evil.invalid/playlist?id=1','https://user:pass@y.qq.com/playlist/1','https://music.youtube.com:8443/playlist?list=PLabcdefghijk','https://music.youtube.com/watch?v=abcdefghijk','https://music.163.com/playlist?id=1&id=2'])
def test_ambiguous_or_untrusted_playlist_urls_rejected(url):
    with pytest.raises(ValueError):parse_playlist_locator(url)


def test_short_link_cannot_redirect_to_another_site_or_forward_credentials():
    locator=parse_playlist_locator('https://163cn.tv/share')[0]
    transport=FakeTransport([Response(302,headers={'location':'https://evil.invalid/playlist?id=1'})])
    with pytest.raises(ValueError):resolve_share(locator,transport)
    assert len(transport.requests)==1 and 'Cookie' not in transport.requests[0]['headers']


def track(platform,sid='123',**changes):
    return {'id':sid,'target_id':platform+':'+sid,'source':platform,'source_song_id':sid,'raw_title':'Song','artists':['Artist'],'album':'Album','source_album_id':'789','duration':180,'metadata_complete':True,**changes}


def test_platform_ids_do_not_collide_in_persistence_or_album_grouping(service,monkeypatch):
    cfg=service.config.current()['values']
    class Reader:
        def __init__(self,platform):self.platform=platform
        def refresh(self,locator):return {'status':'OK','snapshot':{'source':self.platform,'songs':[track(self.platform)],'complete':True,'declared_count':1,'name':'Fixture'}}
    monkeypatch.setattr('musicops.app.playlists.reader',lambda s,p,c:Reader(p))
    for platform in ('qqmusic','migu'):
        result=refresh_platform_playlists(service,{'platform':platform,'playlist_input':'123'},cfg)
        assert result['targets'][0]['target_id']==platform+':123'
    assert {t['target_id'] for t in service.store.list('target_tracks')}=={'qqmusic:123','migu:123'}
    monkeypatch.setattr(service,'_index_real_library',lambda *a,**k:{'count':0})
    result=service.workflows.start({'scene':'playlist','target_ids':['qqmusic:123','migu:123']},cfg)
    assert result['album_count']==2


def test_file_without_source_id_keeps_generated_identity_and_no_fake_platform_id(service,tmp_path):
    file=tmp_path/'playlist.csv';file.write_text('title,artist,album,duration\nSong,Artist,Album,180\n','utf-8')
    result=refresh_platform_playlists(service,{'file_path':str(file),'platform':'auto'},service.config.current()['values'])
    row=result['targets'][0]
    assert row['source_song_id'] is None and row['target_id'].startswith('imported:')
    assert row['target_id'].count('imported:')==1 and netease_reference(row) is None


def test_failed_foreign_refresh_retains_other_platform_membership(service,monkeypatch):
    cfg=service.config.current()['values'];row=track('netease');row.update(playlist_ids=['123'],snapshot_ids=['old'],snapshot_id='old',snapshot_complete=True)
    service.store.put('target_tracks',row['target_id'],row)
    class Reader:
        def refresh(self,_):return {'status':'AUTH_REQUIRED','reason':'需要登录'}
    monkeypatch.setattr('musicops.app.playlists.reader',lambda *a:Reader())
    result=refresh_platform_playlists(service,{'platform':'qqmusic','playlist_input':'123'},cfg)
    assert not result['targets'] and '需要登录' in result['reason']
    assert service.store.get('target_tracks','netease:123')['playlist_ids']==['123']


def test_authenticated_reader_failure_with_empty_snapshot_keeps_reason_and_prior_complete(service,monkeypatch):
    cfg=service.config.current()['values'];membership='qqmusic:playlist:123'
    previous=track('qqmusic');previous.update(playlist_ids=[membership],snapshot_ids=['old-complete'],snapshot_id='old-complete',snapshot_complete=True)
    service.store.put('target_tracks',previous['target_id'],previous)
    service.store.put('playlist_snapshots','old-complete',{'complete':True})
    class Reader:
        def refresh(self,_):
            return {'status':'AUTH_REQUIRED','reason':'QQ 音乐 Cookie 已过期，请重新填写','snapshot':{'source':'qqmusic','songs':[],'complete':False,'declared_count':None,'warnings':[]}}
    monkeypatch.setattr('musicops.app.playlists.reader',lambda *a:Reader())
    result=refresh_platform_playlists(service,{'platform':'qqmusic','playlist_input':'123'},cfg)
    assert result['status']=='SNAPSHOT_INCOMPLETE' and 'Cookie 已过期' in result['reason']
    assert result['snapshots'][0]['status']=='AUTH_REQUIRED' and 'Cookie 已过期' in result['snapshots'][0]['reason']
    assert result['targets']==[] and service.store.get('target_tracks',previous['target_id'])['snapshot_complete']
    assert service.store.get('target_tracks',previous['target_id'])['playlist_ids']==[membership]


def test_error_status_never_reconciles_an_inconsistent_complete_snapshot(service,monkeypatch):
    cfg=service.config.current()['values']
    previous=track('qqmusic');previous.update(playlist_ids=['qqmusic:playlist:123'],snapshot_ids=['old'],snapshot_id='old',snapshot_complete=True)
    service.store.put('target_tracks',previous['target_id'],previous)
    class Reader:
        def refresh(self,_):return {'status':'AUTH_REQUIRED','reason':'需要登录','snapshot':{'source':'qqmusic','songs':[],'complete':True,'declared_count':0,'warnings':[]}}
    monkeypatch.setattr('musicops.app.playlists.reader',lambda *a:Reader())
    result=refresh_platform_playlists(service,{'platform':'qqmusic','playlist_input':'123'},cfg)
    assert result['complete'] is False and result['snapshots'][0]['complete'] is False
    assert service.store.get('target_tracks',previous['target_id'])['playlist_ids']==['qqmusic:playlist:123']


def test_foreign_numeric_id_never_becomes_a_netease_download_parameter(service,tmp_path):
    foreign=track('qqmusic')
    assert netease_reference(foreign) is None
    with pytest.raises(PolicyBlocked,match='其他平台'):
        download_track(foreign,service.config.current()['values'],tmp_path/'download',lambda *_:None)
    assert not (tmp_path/'download').exists()


def test_cross_platform_fallback_requires_unique_title_artist_album_duration_match(service):
    target=track('spotify');song={'id':456,'name':'Song','artists':[{'name':'Artist'}],'album':{'id':999,'name':'Album'},'duration':180000}
    transport=FakeTransport([Response(200,json.dumps({'code':200,'result':{'songs':[song]}}).encode())])
    match=resolve_netease_target(target,service.config.current()['values'],lambda *_:None,transport=transport)
    assert match['source']=='netease' and match['source_song_id']=='456'
    assert netease_reference({**target,'netease_match':match})['source_song_id']=='456'
    wrong={**song,'id':789,'duration':181000}
    transport=FakeTransport([Response(200,json.dumps({'code':200,'result':{'songs':[song,wrong]}}).encode())])
    with pytest.raises(PolicyBlocked,match='唯一'):resolve_netease_target(target,service.config.current()['values'],lambda *_:None,transport=transport)
    assert netease_reference({**target,'netease_match':{**match,'album':'Different'}}) is None


def test_missing_video_album_is_not_grouped_as_one_fake_release(service,monkeypatch):
    cfg=service.config.current()['values']
    for sid in ('1','2'):service.store.put('target_tracks','bilibili:'+sid,track('bilibili',sid,album='',artists=[]))
    monkeypatch.setattr(service,'_index_real_library',lambda *a,**k:{'count':0})
    result=service.workflows.start({'scene':'playlist','target_ids':['bilibili:1','bilibili:2']},cfg)
    assert result['album_count']==2
    service.workflows.advance();assert any(j['stage']=='needs_review' and j['review_kind']=='metadata' for j in service.workflows.rows())
