import copy,json
from pathlib import Path
import pytest
from musicops.app.service import AppService
from musicops.clients.http import FakeTransport,Response
from musicops.clients.metainfo import bencode,parse_metainfo
from musicops.config.schema import default_config,upgrade_config,validate_config
from musicops.providers.pt_download import PTDownloadProvider
from musicops.state.store import StateStore,utcnow


def provider(monkeypatch,balance=2):
    content=bencode({b'info':{b'name':b'song.flac',b'piece length':16384,b'length':100,b'pieces':b'a'*20}})
    meta=parse_metainfo(content)
    page='<a href="torrents.php?action=download&amp;id=3&amp;usetoken=1">FL (2)</a>'
    responses=[Response(200,page.encode()),Response(200,json.dumps({'status':'success','response':{'id':1}}).encode()),
               Response(200,json.dumps({'status':'success','response':{'personal':{'freeleechTokens':balance}}}).encode()),Response(200,content)]
    transport=FakeTransport(responses)
    p=PTDownloadProvider('orpheus',{'base_url':'https://orpheus.network','credential_ref':'vault://test'},transport,
        lambda *_:json.dumps([{'domain':'orpheus.network','path':'/','name':'session','value':'fixture'}]))
    details=iter([{'group':{'id':2},'torrent':{'freeTorrent':False}},
                  {'group':{'id':2},'torrent':{'freeTorrent':'Personal','infoHash':meta['infohash_v1']}}])
    monkeypatch.setattr(p,'detail',lambda _:next(details))
    return p,content


def test_unlimited_fl_ignores_exhausted_batch_cap_but_confirms_free_status(tmp_path,monkeypatch):
    store=StateStore(tmp_path/'state.sqlite')
    old=store.reserve_token('orpheus','old',1,10,1,utcnow(),budget_ref='batch')
    store.token_result(old['id'],'CONFIRMED',{'confirmed':True})
    p,content=provider(monkeypatch)
    actual,proof=p.fetch({'candidate_id':'orpheus:3','resource_id':'3'},store,'batch',None,tmp_path/'private.torrent')
    assert actual==content and proof['confirmed']
    with store.connection() as c:
        rows=c.execute('SELECT cost,state FROM token_reservations').fetchall()
    assert sum(r['cost'] for r in rows)==3 and all(r['state']=='CONFIRMED' for r in rows)


@pytest.mark.parametrize('balance,cap',[(1,None),(2,0)])
def test_unlimited_fl_does_not_bypass_live_balance_or_regular_cap(tmp_path,monkeypatch,balance,cap):
    store=StateStore(tmp_path/'state.sqlite');p,_=provider(monkeypatch,balance)
    with pytest.raises(RuntimeError,match='余额|预算'):
        p.fetch({'candidate_id':'orpheus:3','resource_id':'3'},store,'batch',cap,tmp_path/'private.torrent')
    assert len(p.transport.requests)==3
    assert store.get('evidence','pt-request:orpheus:3') is None


def test_unlimited_fl_does_not_retry_unresolved_request(tmp_path,monkeypatch):
    store=StateStore(tmp_path/'state.sqlite')
    old=store.reserve_token('orpheus','old',1,10,1,utcnow(),budget_ref='batch')
    store.token_result(old['id'],'REQUEST_UNKNOWN',{'reason':'fixture timeout'})
    p,_=provider(monkeypatch)
    with pytest.raises(RuntimeError,match='对账'):
        p.fetch({'candidate_id':'orpheus:3','resource_id':'3'},store,'batch',None,tmp_path/'private.torrent')
    assert len(p.transport.requests)==3


def test_unlimited_flag_migrates_and_roundtrips_without_losing_numeric_limit(tmp_path):
    old=default_config(tmp_path,offline=False);old['workflow'].pop('fl_unlimited');old['workflow']['fl_per_batch']=7
    upgraded=upgrade_config(old,tmp_path)
    assert upgraded['workflow']['fl_unlimited'] is False and upgraded['workflow']['fl_per_batch']==7
    s=AppService(tmp_path/'profile',offline=False);s.stop.set();s.thread.join(5)
    library=tmp_path/'music'
    saved=s.dispatch('save_setup',{'library':str(library),'workflow':{'fl_unlimited':True,'fl_per_batch':7}})
    assert saved['config']['values']['workflow']['fl_unlimited'] is True
    assert saved['config']['values']['workflow']['fl_per_batch']==7
    invalid=copy.deepcopy(saved['config']['values']);invalid['workflow']['fl_unlimited']='true'
    with pytest.raises(ValueError,match='布尔'):
        validate_config(invalid)
