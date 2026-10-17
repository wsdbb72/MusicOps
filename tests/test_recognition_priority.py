"""Evidence order, public HTML and hosted-search provenance contracts."""
import copy
import json
import pytest
from musicops.app.recognition import RecognitionFallback
from musicops.clients.http import FakeTransport, HttpError, Response
from musicops.config.schema import default_config
from musicops.providers.ai import AIProvider
from musicops.providers.catalog import CatalogResolver
from musicops.providers.catalog_web import WebCatalog, detail_url

URL = 'https://www.discogs.com/release/123-Album'
QUERY = {'title': '', 'album': 'Album', 'artists': ['Artist'], 'duration': None}


def html(text, status=200): return Response(status, text.encode())


def album_page(**changes):
    node = {'@type': 'MusicAlbum', 'name': 'Album', 'byArtist': {'name': 'Artist'}, 'url': URL,
            'numTracks': 1, 'track': [{'@type': 'ListItem', 'position': 1, 'item': {'@type': 'MusicRecording', 'name': 'Song', 'duration': 'PT2M3S'}}], **changes}
    return html('<script type="application/ld+json">' + json.dumps(node) + '</script>')


def test_web_html_is_fetched_without_secrets_and_partial_tracks_never_confirmed():
    transport = FakeTransport([html('User-agent: *\nAllow: /'), album_page(numTracks=2)])
    web = WebCatalog({}, transport=transport)
    candidate = web.read(URL)[0]
    assert candidate['album'] == 'Album' and candidate['artists'] == ['Artist']
    assert candidate['tracklist'][0]['duration'] == 123
    assert not candidate['tracklist_complete'] and not candidate['identity_confirmed']
    assert candidate['identity_evidence'][0]['url'] == URL
    assert web.resolve('discogs', candidate['source_id'])['candidate'] == candidate
    assert len(transport.requests) == 2
    assert all('Authorization' not in r['headers'] and 'Cookie' not in r['headers'] for r in transport.requests)


@pytest.mark.parametrize('url', ['https://127.0.0.1/release/123', 'https://www.discogs.com.evil.invalid/release/123', 'https://www.discogs.com:443/release/123', 'http://www.discogs.com/release/123', 'https://x:www@www.discogs.com/release/123', 'https://www.discogs.com/release/123?token=secret'])
def test_web_sources_reject_private_or_unrelated_urls(url): assert detail_url(url) is None


def test_robots_and_permission_blocks_make_no_detail_request():
    transport = FakeTransport([html('User-agent: *\nDisallow: /release')])
    web = WebCatalog({}, transport=transport)
    with pytest.raises(Exception, match='访问规则'): web.read(URL)
    assert len(transport.requests) == 1
    with pytest.raises(Exception, match='许可'): web.read('https://www.albumoftheyear.org/album/123-test.php')
    assert len(transport.requests) == 1


def test_challenge_page_is_not_reported_as_no_match():
    web = WebCatalog({}, transport=FakeTransport([html('User-agent: *\nAllow: /'), html('<title>Verifying your browser</title>')]))
    with pytest.raises(HttpError) as error: web.read(URL)
    assert error.value.error_class == 'ACCESS_BLOCKED'


def ai_response(value, *, search=True, citations=True):
    rows = [{'type': 'web_search_call', 'status': 'completed', 'action': {'type': 'search'}}] if search else []
    rows.append({'type': 'message', 'content': [{'type': 'output_text', 'text': json.dumps(value),
        'annotations': [{'type': 'url_citation', 'url': URL}] if citations else []}]})
    return Response(200, json.dumps({'status': 'completed', 'output': rows}).encode())


@pytest.mark.parametrize('search,citations,code', [(False, True, 'WEB_SEARCH_UNAVAILABLE'), (True, False, 'WEB_SEARCH_UNGROUNDED')])
def test_ai_cannot_claim_search_without_completed_tool_and_sources(tmp_path, search, citations, code):
    cfg = default_config(tmp_path)['ai']; cfg['model'] = 'configured-model'
    ai = AIProvider(cfg, FakeTransport([ai_response({'queries': [dict(title='', album='Album', artists=['Artist'])], 'reason': 'found'}, search=search, citations=citations)]), lambda *_: 'fixture-key')
    with pytest.raises(HttpError) as error: ai.web_recognition_queries(QUERY, [])
    assert error.value.error_class == code


def test_ai_web_search_preserves_configured_model_and_demands_tool(tmp_path):
    cfg = default_config(tmp_path)['ai']; cfg['model'] = 'configured-model'
    transport = FakeTransport([ai_response({'queries': [dict(title='', album='Album', artists=['Artist'])], 'reason': 'found'})])
    result = AIProvider(cfg, transport, lambda *_: 'fixture-key').web_recognition_queries(QUERY, [])
    payload = json.loads(transport.requests[0]['content'])
    assert payload['model'] == 'configured-model' and payload['tool_choice'] == 'required'
    assert payload['tools'][0]['type'] == 'web_search' and result['sources'][0]['url'] == URL


class OrderedResolver:
    def __init__(self, events, api=(), web=()): self.events, self.api, self.web = events, list(api), list(web)
    def identify(self, query):
        self.events.append('api'); return {'status': 'OK' if self.api else 'NO_RESOURCE', 'candidates': copy.deepcopy(self.api), 'sources': [], 'lookup_links': []}
    def web_identify(self, query):
        self.events.append('websites'); return {'candidates': copy.deepcopy(self.web), 'sources': []}


class OrderedAI:
    def __init__(self, events): self.events = events
    def web_recognition_queries(self, query, candidates): self.events.append('ai_web'); return {'queries': [], 'sources': []}


@pytest.mark.parametrize('stage,expected', [('api', ['api']), ('websites', ['api', 'websites']), ('none', ['api', 'websites', 'ai_web'])])
def test_api_then_websites_then_ai_in_order(tmp_path, stage, expected):
    cfg = default_config(tmp_path, offline=False); cfg['ai'].update(enabled=True, model='configured')
    item = CatalogResolver._rank(CatalogResolver._candidate('discogs', 'release:123', 'release', 'Album', ['Artist'], 'Album', url=URL), QUERY)
    events = []; resolver = OrderedResolver(events, [item] if stage == 'api' else [], [item] if stage == 'websites' else [])
    result = RecognitionFallback(cfg, resolver, lambda *_: 'fixture-key', ai_provider=OrderedAI(events)).identify(QUERY)
    assert events == expected and result['read_only'] and result['requires_review']


def test_web_disabled_and_offline_make_no_http_request(tmp_path):
    transport = FakeTransport([])
    for cfg in ({'offline': True}, {'catalog': {'web_enabled': False}}):
        resolver = CatalogResolver(cfg, transport=transport)
        assert not resolver.web_identify(QUERY)['candidates']
    assert not transport.requests
