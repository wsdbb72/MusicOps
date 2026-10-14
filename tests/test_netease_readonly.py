import copy,json
import pytest
from musicops.clients.http import FakeTransport,Response
from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
from musicops.clients.vault import read_lyrics_auth_file
from musicops.app.service import AppService
from musicops.config.schema import default_config

def response(data):return Response(200,json.dumps(data).encode())
def song(i):return {'id':i,'name':'Song '+str(i),'ar':[{'id':1,'name':'A'},{'id':2,'name':'B'}],'al':{'id':10,'name':'Album'},'dt':123456,'no':i,'cd':'1','alia':[]}
def manifest(ids,count=None):return response({'code':200,'playlist':{'id':900000001,'name':'Test','trackIds':[{'id':i} for i in ids],'trackCount':len(ids) if count is None else count,'trialMode':1,'trackUpdateTime':100}})

def test_full_tail_details_multivalue_and_metadata_download_boundary():
    ids=list(range(1,154));t=FakeTransport([manifest(ids),response({'code':200,'songs':[song(i) for i in ids[:150]]}),response({'code':200,'songs':[song(i) for i in ids[150:]]}),manifest(ids),manifest(ids)])
    out=NetEaseReadOnlyProvider(t).refresh('900000001');s=out['snapshot']
    assert s['complete'] and s['metadata_complete'] and len(s['songs'])==153
    assert s['songs'][-1]['artists']==['A','B'] and s['songs'][-1]['duration']==123.456
    assert s['songs'][-1]['download_eligibility']=='UNKNOWN'
    paths=[r['url'].split('music.163.com')[-1] for r in t.requests]
    assert paths==['/api/v3/playlist/detail','/api/v3/song/detail','/api/v3/song/detail','/api/v3/playlist/detail','/api/v6/playlist/detail']
    assert all(r['method']=='GET' and 'Cookie' not in r['headers'] for r in t.requests)

def test_cross_endpoint_counter_difference_does_not_claim_complete():
    ids=[1,2,3];t=FakeTransport([manifest(ids),response({'code':200,'songs':[song(i) for i in ids]}),manifest(ids),manifest(ids,4)])
    out=NetEaseReadOnlyProvider(t).refresh('900000001');s=out['snapshot']
    assert out['status']=='SNAPSHOT_INCOMPLETE' and not s['complete'] and s['visible_catalogue_complete']
    assert s['warnings'][0]['error_class']=='CROSS_ENDPOINT_COUNT_MISMATCH'

def test_missing_album_keeps_other_tracks_and_all_id_evidence():
    a=song(1);a['al']={'id':0,'name':''}
    t=FakeTransport([manifest([1,2]),response({'code':200,'songs':[a,song(2)]}),manifest([1,2]),manifest([1,2])])
    s=NetEaseReadOnlyProvider(t).refresh('900000001')['snapshot']
    assert len(s['songs'])==2 and not s['missing_ids'] and not s['complete']
    assert s['songs'][0]['source_album_id'] is None and s['songs'][0]['metadata_missing_fields']==['album']
    assert s['songs'][1]['metadata_complete'] and s['warnings'][0]['error_class']=='INCOMPLETE_SONG_METADATA'

@pytest.mark.parametrize('returned',[[1,2],[1,2,2,3],[1,2,3,99]])
def test_missing_duplicate_and_unexpected_ids_block_snapshot(returned):
    ids=[1,2,3];t=FakeTransport([manifest(ids),response({'code':200,'songs':[song(i) for i in returned]}),manifest(ids),manifest(ids)])
    assert not NetEaseReadOnlyProvider(t).refresh('900000001')['snapshot']['complete']

def test_manifest_changes_during_fetch_keeps_partial_evidence():
    t=FakeTransport([manifest([1]),response({'code':200,'songs':[song(1)]}),manifest([1,2]),manifest([1])])
    s=NetEaseReadOnlyProvider(t).refresh('900000001')['snapshot']
    assert not s['complete'] and not s['manifest_stable'] and s['warnings'][0]['error_class']=='SOURCE_CHANGED'

@pytest.mark.parametrize('r,status',[(Response(403),'AUTH_FAILED'),(Response(429),'RATE_LIMITED'),(Response(200,b'<html>login</html>'),'API_CONTRACT_ERROR'),(response({'code':200,'playlist':{}}),'PARSE_ERROR')])
def test_manifest_failures_never_report_empty_success(r,status):
    out=NetEaseReadOnlyProvider(FakeTransport([r])).refresh('900000001')
    assert out['status']==status and 'snapshot' not in out

@pytest.mark.parametrize('name,text',[('auth.txt','virtual-key\n'),('auth.json','{"API_AUTH":"virtual-key"}'),('compose.yml','services:\n  lrcapi:\n    environment:\n      API_AUTH: "virtual-key"\n'),('compose.yaml','- API_AUTH=virtual-key')])
def test_auth_file_import_supported_formats(tmp_path,name,text):
    p=tmp_path/name;p.write_text(text,'utf-8');assert read_lyrics_auth_file(p)=='virtual-key'

@pytest.mark.parametrize('name,text',[('auth.txt','bad\nheader'),('auth.json','{"API_AUTH":"a","Authorization":"b"}'),('compose.yml','API_AUTH: ${API_AUTH}'),('auth.json','{"API_AUTH":123}')])
def test_auth_import_rejects_ambiguous_or_unresolved_values(tmp_path,name,text):
    p=tmp_path/name;p.write_text(text,'utf-8')
    with pytest.raises(ValueError):read_lyrics_auth_file(p)

def test_incomplete_refresh_does_not_replace_saved_complete_target(tmp_path,monkeypatch):
    service=AppService(tmp_path);cfg=copy.deepcopy(service.config.current()['values']);cfg['offline']=False;cfg['playlist_ids']=['900000001']
    prior={'target_id':'netease:1','source_song_id':'1','raw_title':'Complete','snapshot_complete':True};service.store.put('target_tracks','netease:1',prior)
    snapshot={'snapshot_id':'netease:900000001:partial','complete':False,'songs':[NetEaseReadOnlyProvider.normalize_song(song(1))],'warnings':[{'error_class':'COUNT_MISMATCH'}]}
    monkeypatch.setattr(NetEaseReadOnlyProvider,'refresh',lambda *a,**k:{'status':'SNAPSHOT_INCOMPLETE','snapshot':snapshot})
    try:
        result=service._execute('refresh_playlist',{},cfg)
        assert not result['complete'] and result['targets'][0]['snapshot_complete'] is False
        assert service.store.get('target_tracks','netease:1')['raw_title']=='Complete'
        assert not service.store.list('playlist_snapshots')
        assert service.dispatch('get_playlist_targets')['network_used'] is False
    finally:service.stop.set();service.thread.join(2)
