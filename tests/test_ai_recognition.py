import copy
import json

import pytest

from musicops.app.recognition import MAX_AI_CANDIDATES, RecognitionFallback
from musicops.clients.http import FakeTransport, HttpError, Response
from musicops.config.schema import default_config
from musicops.providers.ai import AIProvider
from musicops.providers.catalog import lookup_links


QUERY = {'title': 'Song', 'artists': ['Artist'], 'album': 'Album', 'duration': 123.0}


def candidate(number=1, **changes):
    return {'candidate_id': 'musicbrainz:recording:' + str(number), 'source': 'musicbrainz',
            'source_id': 'recording:' + str(number), 'entity_kind': 'recording', 'title': 'Song',
            'artists': ['Artist'], 'album': 'Album', 'duration': 123.0,
            'url': 'https://musicbrainz.org/recording/' + str(number),
            'requires_review': True, 'identity_confirmed': False, **changes}


def found(candidates=(), status=None):
    return {'status': status or ('OK' if candidates else 'NO_RESOURCE'),
            'candidates': list(candidates), 'sources': [{'source': 'musicbrainz', 'status': 'OK'}],
            'lookup_links': lookup_links(QUERY), 'read_only': True}


class Resolver:
    def __init__(self, *outcomes):
        self.outcomes = list(outcomes)
        self.requests = []

    def identify(self, query):
        self.requests.append(copy.deepcopy(query))
        return copy.deepcopy(self.outcomes.pop(0))


class Model:
    def __init__(self, queries=(), choice=None, error=None):
        self.queries = list(queries)
        self.choice = choice or {'candidate_id': None, 'confidence': 0.3, 'ambiguous': True, 'reason': '无法唯一确定'}
        self.error = error
        self.requests = []

    def recognition_queries(self, query, candidates):
        self.requests.append(('queries', copy.deepcopy(query), copy.deepcopy(candidates)))
        if self.error:
            raise self.error
        return {'queries': self.queries, 'reason': '尝试发行别名'}

    web_recognition_queries = recognition_queries

    def recognize_candidate(self, query, candidates):
        self.requests.append(('select', copy.deepcopy(query), copy.deepcopy(candidates)))
        return copy.deepcopy(self.choice)


def configured(tmp_path):
    cfg = default_config(tmp_path)
    cfg['offline'] = False
    cfg['ai'].update(enabled=True, model='fixture-model', base_url='https://ai.example.invalid/v1')
    return cfg


def recognizer(tmp_path, resolver, model, **changes):
    cfg = configured(tmp_path)
    cfg.update(changes)
    return RecognitionFallback(cfg, resolver, lambda *_: 'fixture-key', ai_provider=model)


def recommend(ident):
    return {'candidate_id': ident, 'confidence': 0.98, 'ambiguous': False, 'reason': '真实目录中的对应录音与时长一致'}


def test_regular_unique_match_does_not_call_ai(tmp_path):
    item = candidate()
    model = Model()
    out = recognizer(tmp_path, Resolver(found([item])), model).identify(QUERY)
    assert out['ai_fallback']['status'] == 'NOT_NEEDED'
    assert not model.requests and out['selected_candidate'] is None


@pytest.mark.parametrize('missing', ['disabled', 'offline', 'key', 'model'])
def test_unconfigured_fallback_preserves_normal_results(tmp_path, missing):
    cfg = configured(tmp_path)
    if missing == 'disabled':
        cfg['ai']['enabled'] = False
    elif missing == 'offline':
        cfg['offline'] = True
    elif missing == 'model':
        cfg['ai']['model'] = ''
    model = Model()
    original = found([], 'NETWORK_ERROR')
    out = RecognitionFallback(cfg, Resolver(), lambda *_: None if missing == 'key' else 'fixture', ai_provider=model).identify(QUERY, found=original)
    assert out['status'] == original['status'] and out['sources'] == original['sources']
    assert out['ai_fallback']['calls'] == 0 and not model.requests


def test_ai_aliases_are_retrieved_before_recommendation_and_never_confirm_identity(tmp_path):
    alias = {'title': 'Song', 'artists': ['Artist'], 'album': 'Original Album'}
    item = candidate(album='Original Album')
    model = Model([alias], recommend(item['candidate_id']))
    resolver = Resolver(found([]), found([item]))
    out = recognizer(tmp_path, resolver, model).identify(QUERY)
    assert resolver.requests == [QUERY, {**alias, 'duration': 123.0}]
    assert out['ai_fallback']['calls'] == 2 and out['ai_fallback']['status'] == 'CANDIDATE_RECOMMENDED'
    selected = out['selected_candidate']
    assert selected['candidate_id'] == item['candidate_id'] and selected['ai_recommended']
    assert selected['requires_review'] and not selected['identity_confirmed']
    assert out['status'] == 'OK' and out['ai_lookup_attempts'][0]['sources'] == found()['sources']
    assert out['sources'] == found()['sources'] and out['read_only']


def test_filename_only_hint_sends_basename_and_never_paths_secrets_or_other_library_rows(tmp_path):
    item = candidate()
    model = Model([{'title': 'Song', 'artists': ['Artist'], 'album': 'Album'}], recommend(item['candidate_id']))
    resolver = Resolver(found([item]))
    query = {'filename_hint': r'E:\Private User\Music\Artist - Song.flac', 'path': r'E:\Private User',
             'cookie': 'private-cookie', 'other_tracks': [{'title': 'private-other'}]}
    out = recognizer(tmp_path, resolver, model).identify(query)
    assert out['selected_candidate']['candidate_id'] == item['candidate_id']
    text = json.dumps(model.requests)
    assert 'Artist - Song.flac' in text
    assert all(value not in text for value in ('Private User', 'private-cookie', 'private-other', 'E:'))
    assert len(resolver.requests) == 1 and resolver.requests[0] == {**QUERY, 'duration': None}


def test_ai_context_whitelist_drops_credential_urls_and_raw_provider_fields(tmp_path):
    item = candidate(album='Alias', url='https://musicbrainz.org/recording/1?token=private-token',
                     authorization='private-key', raw_payload={'cookie': 'private-cookie'}, path='E:/private')
    model = Model(choice=recommend(item['candidate_id']))
    out = recognizer(tmp_path, Resolver(), model).identify(QUERY, found=found([item]), force=True)
    text = json.dumps(model.requests)
    assert all(value not in text for value in ('private-token', 'private-key', 'private-cookie', 'E:/private'))
    assert out['selected_candidate']['ai_recommended']


@pytest.mark.parametrize('choice,status', [
    ({'candidate_id': None, 'confidence': 0.5, 'ambiguous': True, 'reason': '多发行'}, 'NEEDS_REVIEW'),
    ({'candidate_id': 'musicbrainz:recording:1', 'confidence': 0.89, 'ambiguous': False, 'reason': '不够确信'}, 'NEEDS_REVIEW'),
    ({'candidate_id': 'invented', 'confidence': 1, 'ambiguous': False, 'reason': 'fabricated'}, 'NEEDS_REVIEW'),
])
def test_ambiguity_low_confidence_or_unknown_id_never_selects(tmp_path, choice, status):
    item = candidate(album='Alias')
    model = Model(choice=choice)
    out = recognizer(tmp_path, Resolver(), model).identify(QUERY, found=found([item]), force=True)
    assert out['selected_candidate'] is None and out['ai_fallback']['status'] == status


def test_known_duration_conflict_rejects_even_high_confidence(tmp_path):
    item = candidate(duration=140.0)
    out = recognizer(tmp_path, Resolver(), Model(choice=recommend(item['candidate_id']))).identify(QUERY, found=found([item]), force=True)
    assert out['selected_candidate'] is None and out['ai_fallback']['status'] == 'DURATION_CONFLICT'


def test_ai_failure_never_leaks_private_endpoint_or_discards_regular_candidates(tmp_path):
    item = candidate(album='Alias')
    model = Model(error=HttpError('TRANSIENT_ERROR', 'https://private.example/api?token=SECRET'))
    out = recognizer(tmp_path, Resolver(), model).identify(QUERY, found=found([item]), force=True)
    assert out['candidates'] == [item] and out['status'] == 'OK'
    assert out['ai_fallback']['status'] == 'TRANSIENT_ERROR'
    assert 'private.example' not in json.dumps(out) and 'SECRET' not in json.dumps(out)


def test_cancellation_is_propagated_instead_of_being_misreported(tmp_path):
    class Cancelled(RuntimeError):
        error_class = 'CANCELLED'
    model = Model(error=Cancelled())
    with pytest.raises(Cancelled):
        recognizer(tmp_path, Resolver(), model).identify(QUERY, found=found([]))


def test_duplicate_queries_are_skipped_and_no_selection_call_without_evidence(tmp_path):
    alias = {'title': 'Alias', 'artists': ['Artist'], 'album': 'Album'}
    model = Model([alias, alias])
    resolver = Resolver(found([]))
    out = recognizer(tmp_path, resolver, model).identify(QUERY, found=found([]))
    assert len(resolver.requests) == 1 and out['ai_fallback']['calls'] == 1
    assert out['ai_fallback']['status'] == 'NO_RESOURCE'


def test_new_alias_candidates_keep_context_space_after_full_initial_result(tmp_path):
    originals = [candidate(i, album='Other') for i in range(20)]
    new = candidate(50, album='Original Album')
    model = Model([{'title': 'Song', 'artists': ['Artist'], 'album': 'Original Album'}], recommend(new['candidate_id']))
    out = recognizer(tmp_path, Resolver(found([new])), model).identify(QUERY, found=found(originals), force=True)
    selected_context = model.requests[-1][2]
    assert len(selected_context) == MAX_AI_CANDIDATES
    assert new['candidate_id'] in {c['candidate_id'] for c in selected_context}
    assert out['selected_candidate']['candidate_id'] == new['candidate_id'] and len(out['candidates']) == 21


def ai_response(value):
    return Response(200, json.dumps({'status': 'completed', 'output': [{'content': [
        {'type': 'output_text', 'text': json.dumps(value)}]}]}).encode())


def test_provider_recognition_schema_and_unknown_ids_are_strict(tmp_path):
    transport = FakeTransport([ai_response({'candidate_id': 'invented', 'confidence': 0.99, 'ambiguous': False, 'reason': 'bad'})])
    ai = AIProvider(configured(tmp_path)['ai'], transport, lambda *_: 'fixture-key')
    with pytest.raises(ValueError, match='AI_UNKNOWN_CANDIDATE'):
        ai.recognize_candidate(QUERY, [candidate()])
    payload = json.loads(transport.requests[0]['content'])
    assert payload['text']['format']['strict'] and payload['text']['format']['name'] == 'musicops_recognition_choice'
    assert 'tools' not in payload


@pytest.mark.parametrize('value', [
    {'queries': [{'title': 'Alias', 'artists': ['A'], 'album': 'B', 'url': 'https://evil.invalid'}], 'reason': ''},
    {'queries': [{'title': '', 'artists': ['A'], 'album': ''}], 'reason': ''},
    {'queries': [{'title': 'A', 'artists': ['A'], 'album': 'B'}] * 3, 'reason': ''},
])
def test_provider_rejects_fabricated_urls_empty_queries_and_budget_overflow(tmp_path, value):
    ai = AIProvider(configured(tmp_path)['ai'], FakeTransport([ai_response(value)]), lambda *_: 'fixture-key')
    with pytest.raises(ValueError, match='AI_INVALID_RECOGNITION'):
        ai.recognition_queries(QUERY, [])


@pytest.mark.parametrize('value', [True, float('nan'), 1.5, -0.1])
def test_provider_rejects_non_finite_boolean_or_out_of_range_confidence(tmp_path, value):
    choice = {'candidate_id': None, 'confidence': value, 'ambiguous': False, 'reason': ''}
    ai = AIProvider(configured(tmp_path)['ai'], FakeTransport([ai_response(choice)]), lambda *_: 'fixture-key')
    with pytest.raises(ValueError, match='AI_INVALID_RECOGNITION'):
        ai.recognize_candidate(QUERY, [candidate()])
