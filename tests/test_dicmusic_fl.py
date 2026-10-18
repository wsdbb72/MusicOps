"""Synthetic DicMusic row/API mismatch regression; no live downloads."""
import json
import pytest
from musicops.clients.http import FakeTransport,Response,PolicyBlocked
from musicops.clients.metainfo import bencode,parse_metainfo
from musicops.providers.pt_download import PTDownloadProvider,TorrentFreeStatus
from musicops.state.store import StateStore,utcnow

CANDIDATE={'candidate_id':'dicmusic:3','resource_id':'3','group_id':'2'}
CONTENT=bencode({b'info':{b'name':b'song.flac',b'piece length':16384,b'length':100,b'pieces':b'a'*20}})
HASH=parse_metainfo(CONTENT)['infohash_v1']
DL='<a href="torrents.php?action=download&amp;id=3">DL</a>'
FL='<a href="torrents.php?action=download&amp;id=3&amp;usetoken=1">FL</a>'
PERSONAL='<tr id="torrent3"><td>'+DL+'<strong class="torrent_label tooltip tl_free" title="&#20010;&#20154;&#20813;&#36153;&#65281;">个人免费！</strong></td></tr>'

def response(value):return Response(200,json.dumps({'status':'success','response':value}).encode())
def provider(monkeypatch,responses):
 p=PTDownloadProvider('dicmusic',{'base_url':'https://dicmusic.com','credential_ref':'vault://fixture'},FakeTransport(responses),lambda *_:json.dumps([{'domain':'dicmusic.com','path':'/','name':'session','value':'fixture'}]))
 monkeypatch.setattr(p,'detail',lambda _: {'group':{'id':2},'torrent':{'freeTorrent':False,'infoHash':HASH}})
 return p

def test_token_download_confirms_exact_row_despite_false_api(monkeypatch,tmp_path):
 p=provider(monkeypatch,[Response(200,(DL+FL+'<span>令牌: 0+1</span>').encode()),response({'id':1}),response({'personal':{}}),Response(200,CONTENT),Response(200,PERSONAL.encode())])
 store=StateStore(tmp_path/'db');content,proof=p.fetch(CANDIDATE,store,'batch',1,tmp_path/'cached.torrent')
 assert content==CONTENT and proof['method']=='torrent-row-personal'
 assert proof['download_method']=='fl-token' and proof['token_cost']==1
 assert p.transport.requests[3]['params']['usetoken']=='1'
 with store.connection() as c:
  row=c.execute('SELECT state,payload FROM token_reservations').fetchone()
 assert row['state']=='CONFIRMED' and json.loads(row['payload'])['cost']==1

@pytest.mark.parametrize('page',[
 '<tr id="torrent4"><strong class="tl_free">个人免费！</strong></tr>'+DL,
 '<tr id="torrent3"><strong class="tl_free">50% Free</strong></tr>'+DL,
 '<tr id="torrent3"><td>个人免费！</td></tr>'+DL,
 PERSONAL+PERSONAL,
])
def test_other_rows_and_partial_free_do_not_authorize_download(monkeypatch,tmp_path,page):
 p=provider(monkeypatch,[Response(200,page.encode())])
 with pytest.raises(PolicyBlocked):p.fetch(CANDIDATE,StateStore(tmp_path/'db'),'batch',1,tmp_path/'cache')
 assert len(p.transport.requests)==1

@pytest.mark.parametrize('token',['','0','false','1&amp;useToken=1','1&amp;usetoken=1'])
def test_invalid_or_duplicate_token_flags_never_fall_back_to_dl(monkeypatch,token):
 p=provider(monkeypatch,[])
 with pytest.raises(PolicyBlocked):p._button(DL+FL.replace('usetoken=1','usetoken='+token),CANDIDATE,False)

def unresolved(store):
 reservation=store.reserve_token('dicmusic','3',1,5,1,utcnow(),budget_ref='batch')
 store.token_result(reservation['id'],'REQUEST_UNKNOWN')
 store.put('evidence','pt-request:dicmusic:3',{'state':'unknown','site':'dicmusic','resource_id':'3'})
 return reservation

def test_old_unknown_reconciles_without_second_token(monkeypatch,tmp_path):
 store=StateStore(tmp_path/'db');reservation=unresolved(store)
 p=provider(monkeypatch,[Response(200,PERSONAL.encode()),Response(200,CONTENT),Response(200,PERSONAL.encode())])
 content,proof=p.fetch(CANDIDATE,store,'batch',0,tmp_path/'cache')
 assert content==CONTENT and proof['reconciled_previous_request'] and proof['token_cost']==1
 assert proof['download_method']=='personal-free'
 assert all('usetoken' not in r['params'] for r in p.transport.requests)
 assert store.get('token_reservations',reservation['id'])['state']=='CONFIRMED'

def test_unknown_without_personal_free_is_never_replayed(monkeypatch,tmp_path):
 store=StateStore(tmp_path/'db');reservation=unresolved(store)
 p=provider(monkeypatch,[Response(200,(DL+FL).encode())])
 with pytest.raises(PolicyBlocked,match='不会重复'):p.fetch(CANDIDATE,store,'batch',2,tmp_path/'cache')
 assert len(p.transport.requests)==1 and store.get('token_reservations',reservation['id'])['state']=='REQUEST_UNKNOWN'

def test_unconfirmed_response_stays_quarantined_and_cannot_start_qbt(monkeypatch,tmp_path):
 p=provider(monkeypatch,[Response(200,(DL+FL).encode()),response({'id':1}),response({'personal':{'freeleechTokens':2}}),Response(200,CONTENT),Response(200,DL.encode())])
 store=StateStore(tmp_path/'db');cache=tmp_path/'cache'
 with pytest.raises(PolicyBlocked,match='未确认'):p.fetch(CANDIDATE,store,'batch',1,cache)
 assert cache.read_bytes()==CONTENT
 assert store.get('evidence','pt-request:dicmusic:3')['state']=='unknown'
 assert not store.list('download_tasks')
