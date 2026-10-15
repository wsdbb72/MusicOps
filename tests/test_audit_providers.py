"""Fault-shaped provider regression tests; all credentials/network are virtual."""
import json
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import httpx
import pytest

from musicops.clients.http import FakeTransport, Response
from musicops.clients.qbt import QbtClient, readonly_preflight
from musicops.media.resources import shift_lrc, validate_lrc
from musicops.providers.lrcapi import LrcApiClient
from musicops.providers.netease import build_snapshot, parse_playlist_input
from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
from musicops.providers.pt import TokenCoordinator
from musicops.providers.pt_readonly import PTReadOnlyProvider, NexusRows
from musicops.state.store import StateStore


def response(data):
    return Response(200, json.dumps(data).encode())


def manifest(ids):
    return response({'code': 200, 'playlist': {'id': 1, 'trackCount': len(ids), 'trackIds': [{'id': i} for i in ids]}})


def song(i):
    return {'id': i, 'name': 'Fixture', 'ar': [{'name': 'Artist'}], 'al': {'id': 10, 'name': 'Album'}, 'dt': 123000}


def lrc_config(**extra):
    return {'primary': {'base_url': 'https://fixture.invalid', 'auth_secret_ref': 'vault://test'},
            'public_fallback': {'enabled': False}, **extra}


def lrc_response(**extra):
    return response([{'id': 'fixture', 'title': 'Fixture', 'artist': 'Artist', 'lyrics': '[00:00]fixture', **extra}])


def test_audit_playlist_share_query_is_canonicalized_and_long_id_rejected():
    result = parse_playlist_input('https://music.163.com/#/playlist?id=1&uct2=virtual-sharing-value')
    assert result['playlist_ids'] == ['1'] and 'uct2' not in json.dumps(result)
    assert result['links'][0]['canonical_url'] == 'https://music.163.com/playlist?id=1'
    assert parse_playlist_input('9'*5000)['status'] == 'INVALID_INPUT'
    assert parse_playlist_input('https://music.163.com/playlist?id='+'9'*20)['status'] == 'INVALID_INPUT'


@pytest.mark.parametrize('duration', [float('nan'), float('inf'), -1, True, 10**400])
def test_audit_nonfinite_song_duration_cannot_complete_metadata(duration):
    raw = song(1); raw['dt'] = duration
    normalized = NetEaseReadOnlyProvider.normalize_song(raw)
    assert not normalized['metadata_complete'] and normalized['duration'] is None
    assert 'duration' in normalized['metadata_missing_fields']


def test_audit_one_malformed_song_preserves_other_batch_evidence():
    transport = FakeTransport([manifest([1, 2]), response({'code': 200, 'songs': [song(1), {'id': 'bad'}]}), manifest([1, 2]), manifest([1, 2])])
    snapshot = NetEaseReadOnlyProvider(transport).refresh(1)['snapshot']
    assert not snapshot['complete'] and snapshot['returned_ids'] == ['1']
    assert snapshot['missing_ids'] == ['2'] and snapshot['failed_batches']


@pytest.mark.parametrize('status', [401, 429])
def test_audit_song_batch_auth_or_rate_stops_all_later_manifest_calls(status):
    transport = FakeTransport([manifest([1, 2]), Response(status)])
    snapshot = NetEaseReadOnlyProvider(transport).refresh(1, batch_size=1)['snapshot']
    assert not snapshot['complete'] and len(transport.requests) == 2
    assert snapshot['alternate_manifest']['status'] == 'NOT_REQUESTED'


def test_audit_playlist_cancel_is_not_swallowed_as_incomplete():
    class Cancelled(RuntimeError):
        error_class = 'CANCELLED'
    def cancel(_): raise Cancelled()
    with pytest.raises(Cancelled): build_snapshot(1, [1, 2, 3], cancel, batch_size=1)


@pytest.mark.parametrize('duration', [float('nan'), float('inf'), True, [], -1])
def test_audit_lrc_rejects_invalid_duration_without_network(duration):
    assert validate_lrc('[00:00]fixture', duration)['errors'] == ['INVALID_DURATION']
    transport = FakeTransport([])
    assert LrcApiClient(lrc_config(), transport, lambda *_: 'virtual').candidates('Fixture', 'Artist', duration=duration)['state'] == 'API_CONTRACT_ERROR'
    assert not transport.requests


def test_audit_lrc_prefix_only_and_repeated_chorus_timestamps():
    assert not validate_lrc('prose [00:00]fixture')['valid']
    result = validate_lrc('[00:00][01:00]one\n[00:01][01:01]two', duration=70)
    assert result['valid'] and result['timestamps'] == [0, 1, 60, 61]
    assert validate_lrc('[00:00]body [99:59]literal', duration=2)['valid']
    assert shift_lrc('[00:00]body [99:59]literal', 1000) == '[00:01.00]body [99:59]literal'
    assert shift_lrc('[00:59.999]rollover', 0) == '[01:00.00]rollover'


def test_audit_invalid_offset_and_unicode_are_controlled():
    assert 'INVALID_OFFSET' in validate_lrc('[offset:x]\n[00:00]fixture')['errors']
    assert 'INVALID_OFFSET' in validate_lrc('[offset:1]\n[offset:2]\n[00:00]fixture')['errors']
    assert validate_lrc('[00:00]\ud800')['errors'] == ['INVALID_TEXT']


@pytest.mark.parametrize('failed,state', [(Response(429, headers={'Retry-After': '10'}), 'RATE_LIMITED'),
                                       (Response(200, b'<html>bad</html>'), 'INVALID_PAYLOAD')])
def test_audit_open_circuit_retains_failure_instead_of_no_match(failed,state):
    transport = FakeTransport([failed])
    client = LrcApiClient(lrc_config(), transport, lambda *_: 'virtual')
    assert client.candidates('Fixture', 'Artist')['state'] == state
    second = client.candidates('Other', 'Artist')
    assert second['state'] == state and len(transport.requests) == 1
    assert second['errors'][0]['error_class'] == 'CIRCUIT_OPEN'


def test_audit_nullable_candidate_version_is_compatible_and_schema_checked():
    assert LrcApiClient(lrc_config(), FakeTransport([lrc_response(version=None)]), lambda *_: 'virtual').candidates('Fixture', 'Artist')['state'] == 'VALID_PAYLOAD'
    assert LrcApiClient(lrc_config(), FakeTransport([lrc_response(artist={'name': 'Artist'})]), lambda *_: 'virtual').candidates('Fixture', 'Artist')['state'] == 'API_CONTRACT_ERROR'


def test_audit_lrc_cache_bounded_and_join_does_not_depend_on_cached_entry():
    now = [0.0]
    transport = FakeTransport([lrc_response(), lrc_response(), lrc_response()])
    client = LrcApiClient(lrc_config(max_cache_entries=2, cache_ttl=10, negative_ttl=10), transport, lambda *_: 'virtual', clock=lambda: now[0])
    for title in ('One', 'Two', 'Three'): client.candidates(title, 'Artist')
    assert len(client._cache) == 2
    now[0] = 11
    client.query('cover', 'No transport', 'Artist')
    assert len(client._cache) <= 1
    started = threading.Event()
    def slow(_): started.set(); time.sleep(.06); return lrc_response()
    client = LrcApiClient(lrc_config(max_cache_bytes=1), FakeTransport([slow]), lambda *_: 'virtual')
    with ThreadPoolExecutor(max_workers=2) as pool:
        first = pool.submit(client.candidates, 'Fixture', 'Artist')
        assert started.wait(1)
        second = pool.submit(client.candidates, 'Fixture', 'Artist')
        assert first.result()['state'] == second.result()['state'] == 'VALID_PAYLOAD'
        assert second.result()['singleflight_join']
    assert not client._cache


@pytest.mark.parametrize('prefs,rows', [([], []), ({}, {}), ({}, [None]), ({}, [{'state': []}])])
def test_audit_qbt_bad_readonly_schema_returns_parse_error(monkeypatch,prefs,rows):
    class Client:
        def __init__(self,**kwargs): pass
        def __enter__(self): return self
        def __exit__(self,*args): pass
        def post(self,url,**kwargs): return httpx.Response(200,text='Ok.')
        def get(self,url):
            if url.endswith('app/version'): return httpx.Response(200,text='v5.2.2')
            if url.endswith('app/webapiVersion'): return httpx.Response(200,text='2.15.1')
            return httpx.Response(200,json=rows if url.endswith('torrents/info') else prefs)
    monkeypatch.setattr(httpx,'Client',Client)
    out = readonly_preflight({'base_url': 'http://127.0.0.1:8085', 'credential_ref': 'vault://test'}, lambda *_: json.dumps({'username':'virtual','password':'virtual'}))
    assert out['status'] == 'PARSE_ERROR'


def test_audit_qbt_unexpected_identity_does_not_confirm_absence(tmp_path):
    client = QbtClient(FakeTransport([response([{'hash': 'unrelated'}])]), 'https://fixture.invalid', tmp_path)
    with pytest.raises(ValueError,match='unexpected client identity'): client._query({'client_id':'expected'})


def pt_provider(responses):
    transport = FakeTransport(responses)
    provider = PTReadOnlyProvider('dicmusic', {'base_url':'https://dicmusic.com','credential_ref':'vault://test'},transport,
        lambda *_: json.dumps([{'domain':'dicmusic.com','path':'/','name':'session','value':'virtual'}]))
    return provider,transport


def test_audit_pt_retry_after_blocks_next_query_and_unknown_pages_are_partial():
    provider,transport = pt_provider([Response(429,headers={'retry-after':'60'})])
    assert provider.search('Fixture')['status'] == provider.search('Other')['status'] == 'RATE_LIMITED'
    assert len(transport.requests) == 1
    provider,_ = pt_provider([response({'status':'success','response':{'results':[]}})])
    result = provider.search('Fixture')
    assert result['status'] == 'NO_RESOURCE' and result['search_complete']
    provider,_ = pt_provider([response({'status':'success','response':{'results':[
        {'groupId':1,'groupName':'Fixture','artist':'Artist','torrents':[{'torrentId':2}]}
    ]}})])
    result = provider.search('Fixture')
    assert result['status'] == 'OK' and not result['search_complete']


def test_audit_pt_bad_torrent_shape_and_nested_html_are_bounded():
    provider,_ = pt_provider([response({'status':'success','response':{'results':[{'groupId':1,'groupName':'Fixture','torrents':[None]}]}})])
    assert provider.search('Fixture')['status'] == 'PARSE_ERROR'
    with pytest.raises(ValueError,match='嵌套'): NexusRows().feed('<tr>'*65)
    with pytest.raises(Exception): PTReadOnlyProvider._html(Response(200,b'<!-- logout.php --><form>login</form>'))


def test_audit_non_token_unknown_request_can_be_reconciled(tmp_path):
    class Provider:
        site = 'open.cd'
        def reconcile(self,candidate): return {'state':'NOT_CONSUMED','evidence':{'kind':'virtual'}}
    store = StateStore(tmp_path/'state.sqlite')
    store.put('download_tasks','asset:open.cd:1',{'state':'REQUEST_UNKNOWN','source':'open.cd'})
    result = TokenCoordinator(store,Provider()).reconcile({'candidate_id':'open.cd:1','resource_id':'1'})
    assert result['status'] == 'OK' and result['task']['state'] == 'RECONCILED_NOT_CONSUMED'
