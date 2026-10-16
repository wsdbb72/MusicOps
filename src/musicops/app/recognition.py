"""Bounded, read-only AI fallback grounded in fetched catalogue candidates.

AI may normalize a name and recommend an existing candidate. It cannot supply
new identity IDs, fetch arbitrary URLs, confirm a release or authorize writes.
"""
from __future__ import annotations

import copy
import math
from urllib.parse import urlsplit

from musicops.clients.http import SafeHttpTransport
from musicops.domain.identity import strict_text
from musicops.providers.ai import AIProvider
from musicops.providers.catalog import lookup_links


MAX_AI_CANDIDATES = 20
MAX_QUERY_VARIANTS = 2


class RecognitionFallback:
    def __init__(self, config, resolver, secret_getter, checkpoint=None, ai_provider=None):
        self.config = config
        self.resolver = resolver
        self.secret_getter = secret_getter
        self.checkpoint = checkpoint or (lambda *_: None)
        self.ai_provider = ai_provider

    @staticmethod
    def _query(query):
        if not isinstance(query, dict):
            raise ValueError('识别查询须为对象')
        clean = {}
        for field in ('title', 'album'):
            clean[field] = AIProvider._recognition_text(query.get(field) or '', 300)
        artists = query.get('artists') or []
        if isinstance(artists, str):
            artists = [artists]
        if not isinstance(artists, list) or len(artists) > 20:
            raise ValueError('艺人查询无效')
        clean['artists'] = list(dict.fromkeys(AIProvider._recognition_text(a, 200, False) for a in artists))
        duration = query.get('duration')
        if duration is not None and (type(duration) not in (int, float) or not math.isfinite(duration) or not 0 <= duration <= 604800):
            raise ValueError('时长字段无效')
        clean['duration'] = duration
        # Only a basename can be used as a hint; never send a library directory.
        filename = query.get('filename_hint') or ''
        if not isinstance(filename, str):
            raise ValueError('文件名提示无效')
        filename = filename.replace('\\', '/').rsplit('/', 1)[-1]
        if filename:
            clean['filename_hint'] = AIProvider._recognition_text(filename, 300)
        if not clean['title'] and not clean['album'] and not filename:
            raise ValueError('请提供歌曲、专辑或文件名提示')
        return clean

    @staticmethod
    def _catalog_query(query):
        return {k: query[k] for k in ('title', 'artists', 'album', 'duration')}

    @staticmethod
    def _has_unique_exact(query, candidates):
        if not query['album'] or not query['artists']:
            return False
        exact = []
        for candidate in candidates:
            if strict_text(candidate.get('album') or '') != strict_text(query['album']):
                continue
            if {strict_text(a) for a in candidate.get('artists', [])} != {strict_text(a) for a in query['artists']}:
                continue
            if query['title'] and (candidate.get('entity_kind') != 'recording' or strict_text(candidate.get('title') or '') != strict_text(query['title'])):
                continue
            if query['duration'] is not None and candidate.get('duration') is not None and abs(query['duration'] - candidate['duration']) > 2:
                continue
            exact.append(candidate)
        return len(exact) == 1

    @staticmethod
    def _context_candidate(candidate):
        # Whitelist observed identity facts. In particular, never forward file
        # paths, provider payloads, cookies, passkeys or an entire source record.
        context = {k: candidate[k] for k in ('candidate_id', 'source', 'source_id', 'entity_kind', 'title', 'album',
                    'artists', 'duration', 'recording_id', 'release_id', 'date', 'year', 'country', 'disambiguation') if k in candidate}
        for field in ('title', 'album', 'disambiguation'):
            if field in context:
                context[field] = str(context[field] or '')[:500]
        context['artists'] = [str(a)[:200] for a in (context.get('artists') or [])[:20]]
        source = candidate.get('source')
        url = candidate.get('url')
        if isinstance(url, str):
            try:
                parsed = urlsplit(url)
                hosts = {'musicbrainz': {'musicbrainz.org'}, 'discogs': {'discogs.com', 'www.discogs.com'}}
                if parsed.scheme == 'https' and parsed.hostname in hosts.get(source, set()) and not (parsed.username or parsed.password or parsed.query or parsed.fragment):
                    context['url'] = url[:2000]
            except ValueError:
                pass
        return context

    def _provider(self):
        ai = self.config.get('ai') or {}
        if self.config.get('offline') or not ai.get('enabled'):
            return None, 'DISABLED'
        if not isinstance(ai.get('model'), str) or not ai['model'].strip():
            return None, 'NOT_CONFIGURED'
        try:
            base = urlsplit(ai.get('base_url', ''))
            valid = base.scheme == 'https' and base.hostname and not (base.username or base.password or base.query or base.fragment)
            if not valid:
                return None, 'NOT_CONFIGURED'
            key = self.secret_getter(ai.get('auth_secret_ref', ''), base.hostname)
        except Exception as exc:
            if getattr(exc, 'error_class', None) == 'CANCELLED':
                raise
            return None, 'AUTH_REQUIRED'
        if not isinstance(key, str) or not key.strip():
            return None, 'AUTH_REQUIRED'
        return self.ai_provider or AIProvider(ai, SafeHttpTransport([base.hostname], offline=False), self.secret_getter), None

    @staticmethod
    def _failure(exc):
        # No exception text: third-party errors can contain a private endpoint.
        state = getattr(exc, 'error_class', None)
        if state == 'CANCELLED':
            raise exc
        return state if state in ('AUTH_REQUIRED', 'AUTH_FAILED', 'RATE_LIMITED', 'TRANSIENT_ERROR', 'NETWORK_ERROR', 'INPUT_TOO_LARGE', 'POLICY_BLOCKED') else 'API_CONTRACT_ERROR'

    def identify(self, query, *, found=None, force=False):
        try:
            query = self._query(query)
        except (ValueError, TypeError):
            return {'status': 'INVALID_INPUT', 'message': '请提供有效歌曲、专辑或文件名提示。', 'candidates': [],
                    'sources': [], 'lookup_links': [], 'read_only': True, 'requires_review': True,
                    'selected_candidate': None, 'ai_fallback': {'status': 'NOT_ATTEMPTED', 'calls': 0, 'queries': []}}
        catalog_query = self._catalog_query(query)
        if found is None:
            found = self.resolver.identify(catalog_query) if query['title'] or query['album'] else {
                'status': 'NO_RESOURCE', 'candidates': [], 'sources': [], 'lookup_links': []}
        result = copy.deepcopy(found)
        result.update(query=catalog_query, read_only=True, requires_review=True, selected_candidate=None)
        result.setdefault('candidates', [])
        result.setdefault('sources', [])
        result.setdefault('lookup_links', lookup_links(catalog_query))
        report = {'status': 'NOT_NEEDED', 'calls': 0, 'queries': [], 'reason': ''}
        result['ai_fallback'] = report
        if not force and self._has_unique_exact(query, result['candidates']):
            return result
        provider, unavailable = self._provider()
        if unavailable:
            report['status'] = unavailable
            return result
        candidates = {c['candidate_id']: c for c in result['candidates'] if isinstance(c, dict) and isinstance(c.get('candidate_id'), str)}
        initial_ids = list(candidates)
        contexts = [self._context_candidate(c) for c in list(candidates.values())[:MAX_AI_CANDIDATES]]
        self.checkpoint('ai_before_normalize')
        try:
            report['calls'] += 1
            alternate = provider.recognition_queries(query, contexts)
        except Exception as exc:
            report.update(status=self._failure(exc), reason='AI 检索词规范化未完成，保留常规识别结果。')
            return result
        seen = {(catalog_query['title'], tuple(catalog_query['artists']), catalog_query['album'])}
        attempts = []
        alternate_ids = []
        links = {row.get('url'): row for row in result['lookup_links'] if isinstance(row, dict) and isinstance(row.get('url'), str)}
        for variant in alternate['queries'][:MAX_QUERY_VARIANTS]:
            self.checkpoint('ai_catalog_lookup')
            normalized = {**variant, 'duration': query['duration']}
            key = (normalized['title'], tuple(normalized['artists']), normalized['album'])
            if key in seen:
                continue
            seen.add(key)
            report['queries'].append(copy.deepcopy(variant))
            outcome = self.resolver.identify(normalized)
            attempts.append({'query': copy.deepcopy(variant), 'status': outcome.get('status'), 'sources': copy.deepcopy(outcome.get('sources', []))})
            for link in outcome.get('lookup_links', []):
                if isinstance(link, dict) and isinstance(link.get('url'), str):
                    links.setdefault(link['url'], link)
            for candidate in outcome.get('candidates', []):
                if isinstance(candidate, dict) and isinstance(candidate.get('candidate_id'), str):
                    candidates.setdefault(candidate['candidate_id'], candidate)
                    alternate_ids.append(candidate['candidate_id'])
        result['ai_lookup_attempts'] = attempts
        result['lookup_links'] = list(links.values())[:18]
        # Reserve context space for newly found aliases even when the first
        # regular lookup filled its entire candidate limit.
        prioritized = list(dict.fromkeys(initial_ids[:MAX_AI_CANDIDATES // 2] + alternate_ids + initial_ids))
        result['candidates'] = [candidates[key] for key in prioritized[:50]]
        if result['candidates'] and not found.get('candidates'):
            result['status'] = 'OK' if found.get('status') in ('OK', 'NO_RESOURCE', 'INVALID_INPUT') else 'PARTIAL'
        # The selection prompt has the same fixed upper bound regardless of a
        # playlist/library size. Query results remain separately inspectable.
        contexts = [self._context_candidate(c) for c in result['candidates'][:MAX_AI_CANDIDATES]]
        if not contexts:
            report.update(status='NO_RESOURCE', reason='AI 建议的检索词已查询目录，仍没有可核对的真实候选。')
            return result
        self.checkpoint('ai_before_selection')
        try:
            report['calls'] += 1
            choice = provider.recognize_candidate(query, contexts)
        except Exception as exc:
            report.update(status=self._failure(exc), reason='AI 身份核对未完成，保留真实目录候选供核对。')
            return result
        chosen = candidates.get(choice['candidate_id'])
        report.update(confidence=choice['confidence'], ambiguous=choice['ambiguous'], reason=choice['reason'])
        if not chosen or choice['ambiguous'] or choice['confidence'] < 0.9:
            report['status'] = 'NEEDS_REVIEW'
            return result
        if query['duration'] is not None and chosen.get('duration') is not None and abs(query['duration'] - chosen['duration']) > 2:
            report.update(status='DURATION_CONFLICT', reason='候选完整时长与目标不符，AI 建议未采用。')
            return result
        selected = copy.deepcopy(chosen)
        selected.update(ai_recommended=True, identity_confirmed=False, requires_review=True)
        selected['ai_recommendation'] = {'confidence': choice['confidence'], 'reason': choice['reason'], 'grounded': True}
        result['selected_candidate'] = selected
        result['candidates'] = [selected] + [c for c in result['candidates'] if c['candidate_id'] != selected['candidate_id']]
        report.update(status='CANDIDATE_RECOMMENDED', selected_candidate_id=selected['candidate_id'])
        # AI-assisted metadata remains unconfirmed, but an original empty search
        # no longer incorrectly labels a successful alternate lookup as empty.
        if result.get('status') in ('NO_RESOURCE', 'INVALID_INPUT', 'POLICY_BLOCKED'):
            result['status'] = 'OK'
        return result
