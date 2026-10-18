"""Live-shaped regression fixtures. No site requests or real token spending."""
import json,sqlite3
from concurrent.futures import ThreadPoolExecutor
import pytest
from musicops.clients.http import Response,PolicyBlocked
from musicops.providers.pt_download import TokenApprovalRequired
from musicops.providers.pt_readonly import token_balance
from musicops.state.store import StateStore,utcnow
from test_dicmusic_fl import CONTENT,HASH,DL,FL,response
from test_release044_features import pt
from musicops.clients.metainfo import parse_metainfo


def legacy(store,cache,site):
    cache.write_bytes(CONTENT)
    store.put('evidence','pt-request:'+site+':3',{'state':'confirmed','site':site,'resource_id':'3',
        'metainfo_sha256':parse_metainfo(CONTENT)['metainfo_sha256'],
        'free_evidence':{'confirmed':True,'infohash':HASH,'method':'free-status-after-download','source':site,'resource_id':'3'}})
    old=store.reserve_token(site,'3',1,10,10,utcnow(),budget_ref='old-batch')
    store.token_result(old['id'],'CONFIRMED',{'legacy':True})
    return old


@pytest.mark.parametrize('site',['orpheus','dicmusic'])
def test_legacy_global_free_cache_renews_using_live_token_link(monkeypatch,tmp_path,site):
    store=StateStore(tmp_path/'db');cache=tmp_path/'cache';legacy(store,cache,site)
    p=pt(monkeypatch,site,[Response(200,(DL+FL).encode()),response({'id':1,'userstats':{'tokens':252}}),response({}),Response(200,CONTENT)])
    details=iter([{'group':{'id':2},'torrent':{'freeTorrent':'Normal','infoHash':HASH}},
                  {'group':{'id':2},'torrent':{'freeTorrent':'personal','infoHash':HASH}}])
    monkeypatch.setattr(p,'detail',lambda _:next(details))
    content,proof=p.fetch({'candidate_id':site+':3','resource_id':'3'},store,'new-batch',None,cache)
    assert content==CONTENT and proof['download_method']=='fl-token' and proof['token_cost']==1
    downloads=[r for r in p.transport.requests if r['params'].get('action')=='download']
    assert len(downloads)==1 and downloads[0]['params']['usetoken']=='1'
    assert len(store.list('token_reservations'))==2 and all(r['state']=='CONFIRMED' for r in store.list('token_reservations'))
    assert len([r for r in store.list('evidence') if r['id'].startswith('pt-request-history:')])==1
    # Already personal: another retry must use the cache with fresh evidence.
    monkeypatch.setattr(p,'detail',lambda _: {'group':{'id':2},'torrent':{'freeTorrent':'personal','infoHash':HASH}})
    assert p.fetch({'candidate_id':site+':3','resource_id':'3'},store,'new-batch',None,cache)[1]['download_method']=='personal-free'
    assert len(downloads)==1 and len(p.transport.requests)==4


@pytest.mark.parametrize('site',['orpheus','dicmusic'])
def test_legacy_cache_does_not_bypass_current_balance_or_batch_budget(monkeypatch,tmp_path,site):
    store=StateStore(tmp_path/'db');cache=tmp_path/'cache';legacy(store,cache,site)
    p=pt(monkeypatch,site,[Response(200,(DL+FL).encode()),response({'id':1,'userstats':{'tokens':252}}),response({})])
    with pytest.raises(TokenApprovalRequired,match='预算'):
        p.fetch({'candidate_id':site+':3','resource_id':'3'},store,'new-batch',0,cache)
    assert not any(r['params'].get('action')=='download' for r in p.transport.requests)
    assert len(store.list('token_reservations'))==1


@pytest.mark.parametrize('site',['orpheus','dicmusic'])
def test_renewal_timeout_is_not_replayed(monkeypatch,tmp_path,site):
    store=StateStore(tmp_path/'db');cache=tmp_path/'cache';legacy(store,cache,site)
    p=pt(monkeypatch,site,[Response(200,(DL+FL).encode()),response({'id':1,'userstats':{'tokens':252}}),response({}),TimeoutError('fixture')])
    candidate={'candidate_id':site+':3','resource_id':'3'}
    with pytest.raises(TimeoutError):p.fetch(candidate,store,'batch',None,cache)
    p.transport.responses.append(Response(200,(DL+FL).encode()))
    with pytest.raises(PolicyBlocked,match='不会重复'):p.fetch(candidate,store,'batch',None,cache)
    assert sum(r['params'].get('action')=='download' for r in p.transport.requests)==1
    assert {r['state'] for r in store.list('token_reservations')}=={'CONFIRMED','REQUEST_UNKNOWN'}


@pytest.mark.parametrize('site',['orpheus','dicmusic'])
def test_unknown_request_reconciles_api_personal_without_spending_again(monkeypatch,tmp_path,site):
    store=StateStore(tmp_path/'db');candidate={'candidate_id':site+':3','resource_id':'3'}
    old=store.reserve_token(site,'3',1,10,10,utcnow());store.token_result(old['id'],'REQUEST_UNKNOWN')
    store.put('evidence','pt-request:'+site+':3',{'state':'unknown'})
    p=pt(monkeypatch,site,[Response(200,DL.encode()),Response(200,CONTENT)],{'freeTorrent':'personal','infoHash':HASH})
    _,proof=p.fetch(candidate,store,'batch',0,tmp_path/'cache')
    assert proof['reconciled_previous_request'] and len(store.list('token_reservations'))==1
    assert all('usetoken' not in r['params'] for r in p.transport.requests)


@pytest.mark.parametrize('value,expected',[('252',252),('1,252',1252),(True,None),(-1,None),('2.5',None)])
def test_balance_shape_differences(value,expected):
    assert token_balance({'userstats':{'tokens':value}},{'stats':{'tokens':None}})==expected


def test_dicmusic_live_shaped_header_and_split_inventory():
    assert token_balance({}, {},'<span>令牌 : 125</span>')==125
    assert token_balance({}, {},'<span>令牌 : 0+125</span>')==125
    assert token_balance({'userstats':{'tokens':0}}, {},'<span>令牌 : 125</span>')==0


def test_renewal_does_not_erase_history_and_remains_atomic(tmp_path):
    path=tmp_path/'db';store=StateStore(path)
    old=store.reserve_token('orpheus','3',1,10,10,utcnow());store.token_result(old['id'],'CONFIRMED')
    # Simulate the old installed index, then open with the upgraded app.
    with sqlite3.connect(path) as c:
        c.execute("CREATE UNIQUE INDEX token_active ON token_reservations(site,resource_id) WHERE state NOT IN ('RELEASED','REJECTED')")
    StateStore(path)
    with pytest.raises(RuntimeError,match='ALREADY_RESERVED'):store.reserve_token('orpheus','3',1,10,10,utcnow())
    def renew(_):
        try:return StateStore(path).reserve_token('orpheus','3',1,10,10,utcnow(),renew_confirmed=True)
        except RuntimeError:return None
    with ThreadPoolExecutor(max_workers=2) as pool:results=list(pool.map(renew,range(2)))
    assert sum(r is not None for r in results)==1
    assert store.get('token_reservations',old['id'])['state']=='CONFIRMED'


def test_legacy_camelcase_hint_is_sent_as_real_lowercase_parameter(monkeypatch):
    p=pt(monkeypatch,'orpheus',[])
    assert p._button(FL.replace('usetoken','useToken'),{'resource_id':'3'},False)[1]['usetoken']=='1'
