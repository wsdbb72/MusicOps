"""Exercise the service/workflow boundary with synthetic read-only providers."""
import copy
import json

import pytest

from musicops.app.service import AppService, TaskCancelled
from musicops.clients.http import FakeTransport, PolicyBlocked, Response
from musicops.providers.ai import AIProvider


QUERY = {'title': 'Song', 'artists': ['Artist'], 'album': 'Album', 'duration': 123.0}


@pytest.fixture
def service(tmp_path):
    service = AppService(tmp_path / 'profile', offline=False)
    # Call the production command and workflow methods synchronously. No queued
    # workflow may run downloads as a side effect of these boundary tests.
    service.stop.set()
    service.thread.join(3)
    yield service


def configured(service):
    cfg = copy.deepcopy(service.config.current()['values'])
    cfg['ai'].update(enabled=True, model='fixture-model', base_url='https://ai.example.invalid/v1')
    return cfg


def candidate(**changes):
    return {'candidate_id': 'musicbrainz:recording:observed', 'source': 'musicbrainz',
            'source_id': 'release:observed', 'entity_kind': 'recording', 'title': 'Song',
            'artists': ['Artist'], 'album': 'Album', 'duration': 123.0,
            'recording_id': 'observed-recording', 'release_id': 'observed-release',
            'url': 'https://musicbrainz.org/release/observed-release',
            'identity_confirmed': False, 'requires_review': True, **changes}


def outcome(candidates=()):
    return {'status': 'OK' if candidates else 'NO_RESOURCE', 'candidates': list(candidates),
            'sources': [{'source': 'musicbrainz', 'status': 'OK'}], 'lookup_links': []}


class Catalog:
    def __init__(self, *outcomes, detail=None):
        self.outcomes = list(outcomes)
        self.queries = []
        self.details = []
        self.detail = detail

    def identify(self, query):
        self.queries.append(copy.deepcopy(query))
        return copy.deepcopy(self.outcomes.pop(0))

    def resolve_candidate(self, source, source_id):
        self.details.append((source, source_id))
        return {'status': 'OK', 'candidate': copy.deepcopy(self.detail)} if self.detail else {'status': 'NO_RESOURCE'}


def ai_response(value):
    return Response(200, json.dumps({'status': 'completed', 'output': [{'content': [
        {'type': 'output_text', 'text': json.dumps(value)}]}]}).encode())


def install_providers(service, monkeypatch, catalog, responses):
    transport = FakeTransport(responses)
    monkeypatch.setattr(service, '_catalog_resolver', lambda _: catalog)
    monkeypatch.setattr(service.vault, 'get', lambda ref, host: 'fixture-key')
    monkeypatch.setattr('musicops.app.recognition.SafeHttpTransport', lambda *args, **kwargs: transport)
    return transport


def choice(ident):
    return {'candidate_id': ident, 'confidence': 0.99, 'ambiguous': False, 'reason': '已获取目录中的别名录音'}


def album_detail(title='Song'):
    return {'source': 'musicbrainz', 'source_id': 'release:observed-release', 'entity_kind': 'release',
            'title': 'Album', 'album': 'Album', 'artists': ['Artist'], 'tracklist_complete': True,
            'tracklist': [{'title': title, 'artists': ['Artist'], 'duration': 123.0,
                           'recording_id': 'observed-recording', 'disc': 1, 'position': 1}]}


def workflow(service, cfg, **changes):
    target = {'target_id': 'target', 'raw_title': 'Song', 'artists': ['Artist'], 'album': 'Album',
              'duration': 123.0, 'source': 'netease', 'source_song_id': 'fixture-song'}
    target.update(changes)
    job = {'job_id': 'identification-fixture', 'batch_id': 'batch-fixture', 'job_kind': 'album',
           'task_type': 'identification', 'task_group': 'identify', 'scene': 'playlist', 'stage': 'identifying',
           'config': cfg, 'targets': [target], 'old_files': [], 'label': 'Album · Artist'}
    service.workflows.put(job)
    return job


def test_identify_metadata_automatically_uses_configured_api_and_saves_only_observed_evidence(service, monkeypatch):
    item = candidate(album='Canonical Album')
    catalog = Catalog(outcome(), outcome([item]))
    transport = install_providers(service, monkeypatch, catalog, [
        ai_response({'queries': [{'title': 'Song', 'artists': ['Artist'], 'album': 'Canonical Album'}], 'reason': '目录别名'}),
        ai_response(choice(item['candidate_id'])),
    ])
    original = {'target_id': 'target', 'raw_title': 'Song', 'artists': ['Artist'], 'album': 'Album', 'duration': 123.0}
    service.store.put('target_tracks', 'target', original)
    original = service.store.get('target_tracks', 'target')
    result = service._execute('identify_metadata', {**QUERY, 'target_id': 'target'}, configured(service))
    assert len(transport.requests) == 2 and len(catalog.queries) == 2
    assert result['ai_fallback']['status'] == 'CANDIDATE_RECOMMENDED'
    assert result['selected_candidate']['candidate_id'] == item['candidate_id']
    evidence = service.store.get('evidence', 'catalog:' + item['candidate_id'])
    assert evidence['query']['target_id'] == 'target'
    assert not evidence['candidate']['identity_confirmed']
    assert service.store.get('target_tracks', 'target') == original
    assert service.store.list('change_plans') == [] and service.store.list('token_reservations') == []


def test_complete_unique_catalog_match_skips_ai_even_when_api_is_configured(service, monkeypatch):
    catalog = Catalog(outcome([candidate()]))
    transport = install_providers(service, monkeypatch, catalog, [])
    result = service._execute('identify_metadata', QUERY, configured(service))
    assert result['ai_fallback']['status'] == 'NOT_NEEDED'
    assert len(catalog.queries) == 1 and transport.requests == []


def test_ai_cancel_propagates_through_default_service_boundary_without_saved_identity(service, monkeypatch):
    catalog = Catalog(outcome())
    transport = install_providers(service, monkeypatch, catalog, [TaskCancelled('fixture cancel')])
    with pytest.raises(TaskCancelled):
        service._execute('identify_metadata', QUERY, configured(service))
    assert len(transport.requests) == 1
    assert service.store.list('target_tracks') == [] and service.store.list('change_plans') == []
    assert not [row for row in service.store.list('evidence') if row.get('kind') == 'catalog_candidate']


def test_unknown_model_candidate_never_creates_accepted_metadata_or_download_plan(service, monkeypatch):
    item = candidate(album='Uncertain Album')
    catalog = Catalog(outcome([item]))
    install_providers(service, monkeypatch, catalog, [
        ai_response({'queries': [], 'reason': '候选中核对'}), ai_response(choice('fabricated-release')),
    ])
    result = service._execute('identify_metadata', QUERY, configured(service))
    assert result['ai_fallback']['status'] == 'API_CONTRACT_ERROR'
    assert result['selected_candidate'] is None
    assert service.store.get('evidence', 'catalog:fabricated-release') is None
    with pytest.raises(PolicyBlocked, match='没有已保存'):
        service._execute('accept_metadata', {'target_id': 'target', 'candidate_id': 'fabricated-release'}, configured(service))
    assert service.store.list('change_plans') == [] and service.workflows.rows() == []


def test_known_workflow_identity_creates_separate_search_without_catalog_or_ai(service, monkeypatch):
    cfg = configured(service)
    monkeypatch.setattr(service, '_catalog_resolver', lambda _: pytest.fail('complete known identity must not query catalog'))
    monkeypatch.setattr(AIProvider, '_request', lambda *_: pytest.fail('complete known identity must not call AI'))
    job = workflow(service, cfg)
    service.workflows._identifying(job)
    saved = service.store.get('evidence', 'workflow:' + job['job_id'])
    assert saved['stage'] == 'completed'
    search = service.store.get('evidence', 'workflow:' + saved['search_job_id'])
    assert search['task_type'] == 'search' and search['stage'] == 'searching'
    assert search['identification_job_id'] == job['job_id']
    assert service.store.list('change_plans') == [] and service.store.list('token_reservations') == []


def test_unknown_ai_choice_in_identification_never_bypasses_track_verification(service, monkeypatch):
    item = candidate(title='Different Recording')
    catalog = Catalog(outcome([item]), detail=album_detail('Different Recording'))
    install_providers(service, monkeypatch, catalog, [
        ai_response({'queries': [], 'reason': '核对现有候选'}), ai_response(choice('fabricated-release')),
    ])
    job = workflow(service, configured(service), album='')
    service.workflows._identifying(job)
    saved = service.store.get('evidence', 'workflow:' + job['job_id'])
    assert saved['stage'] == 'needs_review' and not saved.get('search_job_id')
    assert saved['targets'][0]['album'] == ''
    assert len(service.workflows.rows()) == 1
    assert service.store.list('change_plans') == [] and service.store.list('token_reservations') == []


def test_ai_recommendation_cannot_invent_missing_target_duration_to_start_download(service, monkeypatch):
    item = candidate()
    catalog = Catalog(outcome([item]), detail=album_detail())
    install_providers(service, monkeypatch, catalog, [
        ai_response({'queries': [], 'reason': '核对现有候选'}), ai_response(choice(item['candidate_id'])),
    ])
    job = workflow(service, configured(service), duration=None)
    service.workflows._identifying(job)
    saved = service.store.get('evidence', 'workflow:' + job['job_id'])
    assert saved['stage'] == 'needs_review' and not saved.get('search_job_id')
    assert saved['targets'][0]['duration'] is None
    assert service.store.list('change_plans') == [] and service.store.list('token_reservations') == []


def test_missing_album_with_unique_complete_catalog_evidence_does_not_spend_ai_call(service, monkeypatch):
    catalog = Catalog(outcome([candidate()]), detail=album_detail())
    transport = install_providers(service, monkeypatch, catalog, [])
    job = workflow(service, configured(service), album='')
    service.workflows._identifying(job)
    saved = service.store.get('evidence', 'workflow:' + job['job_id'])
    assert saved['stage'] == 'completed' and saved['targets'][0]['album'] == 'Album'
    assert saved['recognition_evidence']['tracklist_verified']
    assert len(catalog.queries) == 1 and len(catalog.details) == 1 and transport.requests == []


def test_catalog_fill_uses_unique_complete_release_before_ai_fallback(service, monkeypatch):
    catalog = Catalog(outcome([candidate()]), detail=album_detail())
    transport = install_providers(service, monkeypatch, catalog, [])
    job = workflow(service, configured(service), album='')
    proposal = service.workflows._catalog_fill(job, job['targets'])
    assert proposal['source_id'] == 'release:observed-release'
    assert len(catalog.queries) == 1 and len(catalog.details) == 1 and transport.requests == []


def test_ai_alias_retries_real_site_but_does_not_promote_different_identity_to_download(service, monkeypatch):
    cfg = configured(service)
    for site in cfg['workflow']['source_order']:
        cfg['providers'][site]['enabled'] = site == 'dicmusic'
    alias = candidate(artists=['Canonical Artist'], album='Canonical Album')
    catalog = Catalog(outcome([alias]))
    transport = install_providers(service, monkeypatch, catalog, [
        ai_response({'queries': [], 'reason': '目录已有别名候选'}), ai_response(choice(alias['candidate_id'])),
    ])
    queries = []
    class Site:
        def search(self, query):
            queries.append(query)
            if len(queries) == 1:
                return {'status': 'NO_RESOURCE', 'candidates': [], 'search_complete': True}
            return {'status': 'OK', 'search_complete': True, 'candidates': [{
                'candidate_id': 'dicmusic:observed', 'source': 'dicmusic', 'title': 'Canonical Album',
                'artists': ['Canonical Artist'], 'seeders': 5, 'release': {'album': 'Canonical Album'},
                'media': {'format': 'flac', 'encoding': '24bit'},
            }]}
        def fetch(self, *args):
            pytest.fail('AI aliases must not directly authorize torrent/FL actions')
    monkeypatch.setattr(service.workflows, 'provider', lambda *_: Site())
    job = workflow(service, cfg)
    job.update(task_type='search', task_group='search', stage='searching', prefer_24bit=True)
    service.workflows.put(job)
    service.workflows._searching(job)
    assert job['stage'] == 'searching' and job['search_query'] == 'Canonical Artist Canonical Album'
    service.workflows._searching(job)
    saved = service.store.get('evidence', 'workflow:' + job['job_id'])
    assert queries == ['Artist Album', 'Canonical Artist Canonical Album']
    assert saved['stage'] == 'needs_review' and saved['review_kind'] == 'candidate'
    assert not saved['choices'][0]['identity_exact']
    assert saved['targets'][0]['artists'] == ['Artist'] and saved['targets'][0]['album'] == 'Album'
    assert len(transport.requests) == 2 and service.store.list('change_plans') == []
    assert service.store.list('token_reservations') == []
