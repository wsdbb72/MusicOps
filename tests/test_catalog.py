import copy
import json

import pytest

from musicops.clients.http import FakeTransport, HttpError, Response
from musicops.providers.catalog import CACHE_MAX_BYTES, CatalogResolver, MAX_BYTES, lookup_links


REC = '11111111-1111-4111-8111-111111111111'
REL = '22222222-2222-4222-8222-222222222222'
GROUP = '33333333-3333-4333-8333-333333333333'
QUERY = {'title': 'Song', 'artists': ['A', 'B'], 'album': 'Album', 'duration': 123.0, 'target_id': 'netease:1'}


def response(data, status=200):
    return Response(status, json.dumps(data).encode())


def recording():
    return {'id': REC, 'title': 'Song', 'length': 123000, 'score': 100,
            'artist-credit': [{'name': 'A'}, ' & ', {'artist': {'name': 'B'}}],
            'releases': [{'id': REL, 'title': 'Album'}]}


def release():
    return {'id': REL, 'title': 'Album', 'artist-credit': [{'name': 'A'}, {'name': 'B'}],
            'media': [{'position': 1, 'track-count': 1, 'tracks': [{'position': 1,
                       'recording': {'id': REC, 'title': 'Song', 'length': 123000}}]}],
            'relations': [{'target-type': 'url', 'type': 'discogs', 'url': {'resource': 'https://www.discogs.com/release/123'}},
                          {'target-type': 'url', 'type': 'other databases', 'url': {'resource': 'https://www.allmusic.com/album/example-mw00000000'}}],
            'release-group': {'id': GROUP, 'relations': [{'target-type': 'url', 'type': 'other databases',
                             'url': {'resource': 'https://rateyourmusic.com/release/album/a/album/'}}]}}


def resolver(responses=(), **kwargs):
    return CatalogResolver({'discogs': {'enabled': False}}, transport=FakeTransport(responses), **kwargs)


def test_identify_musicbrainz_multiartist_rank_and_six_sources():
    r = resolver([response({'recordings': [recording()]}), response({'releases': [release()]})])
    out = r.identify(QUERY)
    assert out['status'] == 'OK' and len(out['lookup_links']) == 6 and len(out['sources']) == 6
    candidate = out['candidates'][0]
    assert candidate['confidence'] == 1 and candidate['artists'] == ['A', 'B']
    assert candidate['source_id'] == 'release:' + REL and candidate['recording_id'] == REC
    assert candidate['requires_review'] and not candidate['identity_confirmed'] and out['read_only']
    assert not candidate['tracklist_complete']
    requests = r.transports['musicbrainz'].requests
    assert len(requests) == 2 and all(q['method'] == 'GET' for q in requests)
    assert all(q['max_response_bytes'] == MAX_BYTES for q in requests)
    assert all(q['headers']['User-Agent'].startswith('MusicOps/') for q in requests)
    assert all('Authorization' not in q['headers'] for q in requests)
    assert not any(item['identity_evidence'] for item in out['lookup_links'])
    assert all(item['status'] == 'MANUAL_LOOKUP' for item in out['sources'][2:])


def test_discogs_missing_token_is_explicit_partial_with_official_search_link():
    transport = FakeTransport([response({'recordings': []})])
    out = CatalogResolver({}, transport=transport).identify({'title': 'Song'})
    assert out['status'] == 'NO_RESOURCE'
    assert out['sources'][1]['status'] == 'AUTH_REQUIRED'
    assert len(transport.requests) == 1
    assert next(x for x in out['lookup_links'] if x['source'] == 'discogs')['url'].startswith('https://www.discogs.com/search/')


def test_discogs_uses_only_independent_token_host_and_not_query_parameter():
    mb = FakeTransport([response({'releases': []})])
    dc = FakeTransport([response({'results': [{'id': 123, 'type': 'release', 'title': 'A - Album'}]})])
    asked = []
    def get_secret(ref, host):
        asked.append((ref, host))
        return 'virtual-token-123'
    r = CatalogResolver({'discogs': {'credential_ref': 'vault://catalog/discogs/token'}},
                        transports={'musicbrainz': mb, 'discogs': dc}, secret_getter=get_secret)
    out = r.identify({'album': 'Album', 'artists': ['A']})
    assert out['status'] == 'OK' and out['candidates'][0]['source'] == 'discogs'
    assert asked == [('vault://catalog/discogs/token', 'api.discogs.com')]
    assert dc.requests[0]['headers']['Authorization'] == 'Discogs token=virtual-token-123'
    assert 'token' not in dc.requests[0]['params'] and 'virtual-token' not in json.dumps(out)
    assert all('Authorization' not in x['headers'] for x in mb.requests)


def test_release_detail_tracklist_and_relationship_links_are_reviewable_evidence():
    r = resolver([response(release())])
    out = r.resolve_candidate('musicbrainz', 'release:' + REL)
    assert out['status'] == 'OK'
    c = out['candidate']
    assert c['tracklist_complete'] and c['tracklist'][0]['duration'] == 123
    assert c['tracklist'][0]['artists'] == ['A', 'B']
    assert {x['source'] for x in out['lookup_links']} == {'discogs', 'allmusic', 'rateyourmusic'}
    assert all(x['identity_evidence'] and not x['external_page_fetched'] for x in out['lookup_links'])
    assert c['requires_review'] and not c['identity_confirmed']
    assert '+url-rels' in r.transports['musicbrainz'].requests[0]['params']['inc']


def test_recording_detail_does_not_claim_related_release_list_complete():
    data = recording()
    data['relations'] = []
    out = resolver([response(data)]).resolve_candidate('musicbrainz', 'recording:' + REC)
    assert out['status'] == 'OK' and not out['candidate']['related_releases_complete']
    assert out['candidate']['releases'][0]['source_id'] == 'release:' + REL


@pytest.mark.parametrize('mutation', ['truncated', 'duplicate_position', 'duplicate_disc'])
def test_incomplete_release_tracklist_never_claims_complete(mutation):
    data = release()
    if mutation == 'truncated':
        data['media'][0]['track-count'] = 2
    elif mutation == 'duplicate_position':
        data['media'][0]['track-count'] = 2
        data['media'][0]['tracks'] *= 2
    else:
        data['media'] *= 2
    out = resolver([response(data)]).resolve_candidate('musicbrainz', 'release:' + REL)
    assert out['status'] == 'OK' and not out['candidate']['tracklist_complete']


def test_discogs_detail_handles_heading_and_artist_credits_but_index_requires_review():
    data = {'id': 123, 'title': 'Album', 'artists': [{'name': 'A'}, {'name': 'B'}],
            'tracklist': [{'type_': 'heading', 'title': 'Disc One'},
                          {'type_': 'track', 'position': '1', 'title': 'Song', 'duration': '2:03'},
                          {'type_': 'index', 'title': 'Suite', 'sub_tracks': [{'title': 'Part'}]}]}
    r = CatalogResolver({'musicbrainz': {'enabled': False}, 'discogs': {'credential_ref': 'vault://dc'}},
                        transport=FakeTransport([response(data)]), secret_getter=lambda *_: 'virtual-token')
    out = r.resolve_candidate('discogs', 'release:123')
    c = out['candidate']
    assert out['status'] == 'OK' and c['tracklist'][0]['duration'] == 123
    assert c['tracklist'][0]['artists'] == ['A', 'B'] and not c['tracklist_complete']


@pytest.mark.parametrize('reply, expected', [
    (Response(401), 'AUTH_REQUIRED'), (Response(403), 'ACCESS_BLOCKED'),
    (Response(302), 'ACCESS_BLOCKED'), (Response(429), 'RATE_LIMITED'),
    (Response(500), 'TRANSIENT_ERROR'), (Response(404), 'NO_RESOURCE'),
    (Response(200, b'<html>blocked</html>'), 'INVALID_PAYLOAD'),
    (response({'bad': []}), 'PARSE_ERROR'), (response([]), 'PARSE_ERROR'),
    (Response(200, b'x' * (MAX_BYTES + 1)), 'INVALID_PAYLOAD'),
    (Response(200, b'{}', url='https://unrelated.example/ws/2/recording'), 'ACCESS_BLOCKED'),
])
def test_failures_distinct_from_empty_match(reply, expected):
    out = resolver([reply]).identify({'title': 'Song'})
    assert out['status'] == expected and not out['candidates']
    assert out['sources'][0]['status'] == expected


def test_rate_limit_cooldown_no_second_network_request():
    r = resolver([Response(429, headers={'Retry-After': '120'}), response({'recordings': []})])
    assert r.identify({'title': 'Song'})['status'] == 'RATE_LIMITED'
    assert r.identify({'title': 'Other'})['status'] == 'RATE_LIMITED'
    assert len(r.transports['musicbrainz'].requests) == 1


def test_cache_is_bounded_and_returns_copies():
    r = CatalogResolver({'cache_entries': 1, 'discogs': {'enabled': False}},
                        transport=FakeTransport([response({'recordings': [recording()]}), response({'recordings': []}), response({'recordings': []})]))
    first = r.identify({'title': 'Song'})
    first['candidates'][0]['title'] = 'Changed locally'
    assert r.identify({'title': 'Song'})['candidates'][0]['title'] == 'Song'
    assert len(r.transports['musicbrainz'].requests) == 1
    r.identify({'title': 'Different'})
    r.identify({'title': 'Song'})
    assert len(r.cache) == 1 and len(r.transports['musicbrainz'].requests) == 3


@pytest.mark.parametrize('query', [{'title': ''}, {'title': 'a\nInjected'}, {'title': 'x', 'duration': float('nan')},
                                   {'album': 'x', 'artists': [123]}, {'title': 'x', 'duration': True}, {'title': 'x' * 301}])
def test_invalid_queries_have_no_network_requests(query):
    r = resolver()
    assert r.identify(query)['status'] == 'INVALID_INPUT'
    assert not r.transports['musicbrainz'].requests


@pytest.mark.parametrize('source, ident', [('musicbrainz', 'release:../../foo'), ('discogs', 'release:123?token=x'),
                                          ('vgmdb', '123'), ('musicbrainz', 'artist:' + REC)])
def test_untrusted_details_do_not_construct_network_endpoints(source, ident):
    r = resolver()
    assert r.resolve_candidate(source, ident)['status'] in ('INVALID_INPUT', 'PARSE_ERROR')
    assert not r.transports['musicbrainz'].requests


def test_id_mismatch_is_parse_failure():
    data = release()
    data['id'] = REC
    assert resolver([response(data)]).resolve_candidate('musicbrainz', 'release:' + REL)['status'] == 'PARSE_ERROR'


def test_lucene_query_values_escaped_and_plain_browser_links_encoded():
    r = resolver([response({'recordings': []})])
    out = r.identify({'title': 'A" OR *:*', 'artists': ['C/D']})
    query = r.transports['musicbrainz'].requests[0]['params']['query']
    assert 'A\\" OR \\*\\:\\*' in query and 'C\\/D' in query
    assert all('A"' not in x['url'] for x in out['lookup_links'])


def test_offline_block_is_enforced_even_with_fake_transport():
    r = CatalogResolver({'offline': True, 'catalog': {}}, transport=FakeTransport())
    assert r.identify({'title': 'Song'})['status'] == 'POLICY_BLOCKED'
    assert r.resolve_candidate('musicbrainz', 'release:' + REL)['status'] == 'POLICY_BLOCKED'
    assert not r.transports['musicbrainz'].requests


def test_cancellation_propagates_to_durable_task_owner():
    class Cancelled(Exception):
        error_class = 'CANCELLED'
    def checkpoint(_):
        raise Cancelled('cancel')
    r = resolver(checkpoint=checkpoint)
    with pytest.raises(Cancelled):
        r.identify({'title': 'Song'})
    with pytest.raises(Cancelled):
        r.resolve_candidate('musicbrainz', 'release:' + REL)


def test_url_relationships_ignore_unknown_domains_credentials_and_non_web_schemes():
    data = release()
    data['relations'].extend({'target-type': 'url', 'url': {'resource': url}} for url in
                            ('file:///C:/secret', 'https://user:password@www.allmusic.com/album/x',
                             'https://allmusic.com.evil.example/x', 'https://www.allmusic.com:123/album/x'))
    out = resolver([response(data)]).resolve_candidate('musicbrainz', 'release:' + REL)
    assert out['status'] == 'OK' and len(out['lookup_links']) == 3


def test_retrieved_metadata_never_modifies_input_query():
    original = copy.deepcopy(QUERY)
    resolver([response({'recordings': [recording()]}), response({'releases': []})]).identify(QUERY)
    assert QUERY == original


def test_one_invalid_recording_does_not_discard_valid_candidates():
    bad = recording()
    bad['id'] = 'not-an-id'
    out = resolver([response({'recordings': [bad, recording(), None]})]).identify({'title': 'Song', 'artists': ['A', 'B']})
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1
    assert out['candidates'][0]['recording_id'] == REC
    assert out['sources'][0]['status'] == 'PARTIAL'
    assert {w['row_index'] for w in out['warnings']} == {0, 2}
    assert all(w['scope'] == 'candidate' and w['error_class'] == 'PARSE_ERROR' for w in out['warnings'])


def test_nullable_optional_annotation_and_no_related_album_do_not_fabricate_metadata():
    row = recording()
    row['disambiguation'] = None
    row['releases'] = None
    out = resolver([response({'recordings': [row]})]).identify({'title': 'Song'})
    assert out['status'] == 'OK'
    candidate = out['candidates'][0]
    assert candidate['disambiguation'] == '' and candidate['album'] == ''
    assert candidate['source_id'] == 'recording:' + REC and candidate['metadata_missing_fields'] == ['album']
    assert candidate['requires_review'] and not candidate['identity_confirmed']


def test_bad_associated_release_only_discards_that_release():
    row = recording()
    row['releases'] = [None, {'id': REL, 'title': None}, {'id': REL, 'title': 'Album'}]
    out = resolver([response({'recordings': [row]})]).identify({'title': 'Song'})
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1
    assert out['candidates'][0]['album'] == 'Album'
    assert {w['release_index'] for w in out['warnings']} == {0, 1}


def test_bad_release_keeps_recording_evidence_when_no_good_release_remains():
    row = recording()
    row['releases'] = [None]
    out = resolver([response({'recordings': [row]})]).identify({'title': 'Song'})
    candidate = out['candidates'][0]
    assert out['status'] == 'PARTIAL' and candidate['album'] == '' and candidate['release_id'] is None
    assert candidate['recording_id'] == REC and candidate['source_id'] == 'recording:' + REC


def test_large_related_release_list_is_bounded_and_partial_instead_of_all_source_failure():
    row = recording()
    row['releases'] = [{'id': f'22222222-2222-4222-8222-{i:012x}', 'title': 'Album'} for i in range(150)]
    out = resolver([response({'recordings': [row]})]).identify({'title': 'Song'})
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 5
    warning = out['warnings'][0]
    assert warning['error_class'] == 'RESULT_LIMIT' and warning['returned_count'] == 150 and warning['retained_limit'] == 5


def test_missing_artist_row_not_filled_from_query_and_other_row_survives():
    row = recording()
    row['artist-credit'] = None
    out = resolver([response({'recordings': [row, recording()]})]).identify({'title': 'Song', 'artists': ['A', 'B']})
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1
    assert out['warnings'][0]['row_index'] == 0 and out['warnings'][0]['scope'] == 'candidate'


def test_album_request_failure_preserves_prior_recording_candidates():
    out = resolver([response({'recordings': [recording()]}), HttpError('NETWORK_ERROR')]).identify(QUERY)
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1
    assert out['warnings'][0]['stage'] == 'release_search' and out['warnings'][0]['scope'] == 'request'
    assert out['warnings'][0]['error_class'] == 'NETWORK_ERROR'


def test_rate_failure_after_recording_keeps_evidence_and_does_not_retry():
    r = resolver([response({'recordings': [recording()]}), Response(429, headers={'Retry-After': '60'})])
    out = r.identify(QUERY)
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1
    assert out['warnings'][0]['error_class'] == 'RATE_LIMITED'
    assert len(r.transports['musicbrainz'].requests) == 2


def test_invalid_release_row_does_not_discard_valid_release():
    out = resolver([response({'releases': [None, release()]})]).identify({'album': 'Album', 'artists': ['A', 'B']})
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1
    assert out['candidates'][0]['release_id'] == REL


def test_discogs_bad_row_does_not_discard_valid_release():
    dc = FakeTransport([response({'results': [None, {'id': 123, 'type': 'release', 'title': 'A - Album'}]})])
    r = CatalogResolver({'musicbrainz': {'enabled': False}, 'discogs': {'credential_ref': 'vault://catalog/discogs/token'}},
                        transports={'discogs': dc}, secret_getter=lambda *_: 'virtual-token-123')
    out = r.identify({'album': 'Album', 'artists': ['A']})
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1 and out['candidates'][0]['source_id'] == 'release:123'
    assert out['warnings'][0]['source'] == 'discogs' and out['warnings'][0]['scope'] == 'candidate'


def test_only_bad_records_is_parse_failure_and_never_empty_success():
    out = resolver([response({'recordings': [None, {'title': 'Song'}]})]).identify({'title': 'Song'})
    assert out['status'] == 'PARSE_ERROR' and not out['candidates'] and len(out['warnings']) == 2


@pytest.mark.parametrize('field', ['length', 'disambiguation'])
def test_invalid_optional_field_preserves_known_identity_and_warns(field):
    row = recording()
    row[field] = {'wrong': 'shape'}
    out = resolver([response({'recordings': [row]})]).identify({'title': 'Song'})
    assert out['status'] == 'PARTIAL' and len(out['candidates']) == 1
    assert out['warnings'][0]['field'] == field
    if field == 'length':
        assert out['candidates'][0]['duration'] is None


def test_resolver_accepts_exact_configuration_schema_upper_bounds():
    r = CatalogResolver({'timeout': 120, 'cache_ttl': 604800, 'cache_entries': 512}, transport=FakeTransport())
    assert r.timeout == 120 and r.cache_ttl == 604800 and r.cache_limit == 512


def test_total_cache_budget_evicts_serialized_payloads_even_below_entry_limit():
    body = json.dumps({'large_metadata': 'x' * 1_200_000}).encode()
    transport = FakeTransport([Response(200, body) for _ in range(36)])
    r = CatalogResolver({'cache_entries': 512, 'cache_ttl': 604800}, transport=transport)
    for i in range(36):
        r._get('musicbrainz', '/ws/2/recording', {'query': str(i), 'fmt': 'json'})
    assert 0 < r.cache_bytes <= CACHE_MAX_BYTES and len(r.cache) < 36
    assert r.cache_bytes == sum(value[2] for value in r.cache.values())
    assert all(isinstance(value[1], bytes) for value in r.cache.values())
    assert len(r.cache) < r.cache_limit


def test_zero_cache_ttl_disables_retention_and_requests_again():
    transport = FakeTransport([response({'recordings': []}), response({'recordings': []})])
    r = CatalogResolver({'cache_ttl': 0, 'discogs': {'enabled': False}}, transport=transport)
    r.identify({'title': 'Song'})
    r.identify({'title': 'Song'})
    assert not r.cache and r.cache_bytes == 0 and len(transport.requests) == 2
