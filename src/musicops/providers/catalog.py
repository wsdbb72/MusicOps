"""Read-only catalogue evidence; web search links are never identity evidence.

MusicBrainz supplies recording/release IDs. Discogs search requires its own user
token. Other sites remain explicit browser lookups until an authorised API is
configured. A candidate, including an exact textual match, always needs review.
"""
from __future__ import annotations

import copy
import hashlib
import json
import math
import re
import sys
import threading
import time
import unicodedata
from collections import OrderedDict
from datetime import datetime, timezone
from urllib.parse import quote, urlencode, urlsplit

from musicops.clients.http import HttpError, PolicyBlocked, SafeHttpTransport


SOURCES = {
    'musicbrainz': {'name': 'MusicBrainz', 'host': 'musicbrainz.org', 'mode': 'official_api'},
    'discogs': {'name': 'Discogs', 'host': 'api.discogs.com', 'mode': 'official_api'},
    'allmusic': {'name': 'AllMusic', 'host': 'www.allmusic.com', 'mode': 'manual_lookup'},
    'rateyourmusic': {'name': 'Rate Your Music', 'host': 'rateyourmusic.com', 'mode': 'manual_lookup'},
    'albumoftheyear': {'name': 'Album of the Year', 'host': 'www.albumoftheyear.org', 'mode': 'manual_lookup'},
    'vgmdb': {'name': 'VGMdb', 'host': 'vgmdb.net', 'mode': 'manual_lookup'},
}
USER_AGENT = 'MusicOps/0.4.2 (Windows desktop; personal catalogue lookup)'
MAX_BYTES = 2_000_000
CACHE_MAX_BYTES = 32 * 1024 * 1024
_MBID = re.compile(r'[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\Z')


def _text(value, limit=500, required=False):
    if not isinstance(value, str) or len(value) > limit or any(ord(c) < 32 for c in value):
        raise ValueError('目录文本字段无效')
    value = value.strip()
    if required and not value:
        raise ValueError('目录文本字段为空')
    return value


def _mbid(value):
    if not isinstance(value, str) or not _MBID.fullmatch(value.lower()):
        raise ValueError('MusicBrainz ID 无效')
    return value.lower()


def _integer_id(value):
    if type(value) is int and value > 0 or isinstance(value, str) and re.fullmatch(r'[1-9][0-9]{0,11}', value):
        return str(value)
    raise ValueError('Discogs ID 无效')


def _norm(value):
    return ''.join(c for c in unicodedata.normalize('NFKC', value).casefold() if c.isalnum())


def _lucene(value):
    return '"' + re.sub(r'([+\-!(){}\[\]^"~*?:\\/]|&&|\|\|)', r'\\\1', value) + '"'


def _duration(value, milliseconds=False):
    if value is None:
        return None
    if type(value) not in (int, float) or not math.isfinite(value) or not 0 <= value <= (604800000 if milliseconds else 604800):
        raise ValueError('时长字段无效')
    return value / 1000 if milliseconds else value


def _credits(value, fallback=None):
    if value is None:
        return list(fallback or [])
    if not isinstance(value, list) or len(value) > 100:
        raise ValueError('艺人字段无效')
    names = []
    for item in value:
        if isinstance(item, str):  # MusicBrainz join phrases are not artists.
            continue
        if not isinstance(item, dict):
            raise ValueError('艺人字段无效')
        artist = item.get('artist')
        name = item.get('name') or (artist.get('name') if isinstance(artist, dict) else None)
        names.append(_text(name, required=True))
    return list(dict.fromkeys(names))


def lookup_links(query):
    """Return navigable searches, distinctly labelled as unevaluated links."""
    album = query.get('album', '')
    title = query.get('title', '')
    artists = query.get('artists', [])
    term = ' '.join(x for x in [*artists, album or title] if x)
    urls = {
        'musicbrainz': 'https://musicbrainz.org/search?' + urlencode({'query': term, 'type': 'release' if album else 'recording', 'method': 'indexed'}),
        'discogs': 'https://www.discogs.com/search/?' + urlencode({'q': term, 'type': 'release'}),
        'allmusic': 'https://www.allmusic.com/search/all/' + quote(term, safe=''),
        'rateyourmusic': 'https://rateyourmusic.com/search?' + urlencode({'searchterm': term, 'searchtype': 'l' if album else 's'}),
        'albumoftheyear': 'https://www.albumoftheyear.org/search/?' + urlencode({'q': term}),
        'vgmdb': 'https://vgmdb.net/search.php?' + urlencode({'do': 'results', 'query': album or title or term}),
    }
    return [{'source': source, 'name': SOURCES[source]['name'], 'url': url,
             'kind': 'search_link', 'identity_evidence': False, 'requires_review': True}
            for source, url in urls.items()]


class CatalogResolver:
    _guard = threading.Lock()
    _locks = {}
    _last = {}
    _blocked = {}

    def __init__(self, config=None, transport=None, transports=None, secret_getter=None, checkpoint=None):
        config = config or {}
        self.offline = config.get('offline', False) is True
        self.config = config.get('catalog', config)
        if not isinstance(self.config, dict):
            raise ValueError('目录识别配置无效')
        self.secret_getter = secret_getter or (lambda *_: None)
        self.checkpoint = checkpoint or (lambda *_: None)
        self.transports = dict(transports or {})
        for source in ('musicbrainz', 'discogs'):
            self.transports.setdefault(source, transport or SafeHttpTransport([SOURCES[source]['host']], offline=self.offline))
        self.limit = self._setting('candidate_limit', 20, 1, 50)
        self.cache_limit = self._setting('cache_entries', 64, 1, 512)
        self.cache_ttl = self._setting('cache_ttl', 900, 0, 604800)
        self.timeout = self._setting('timeout', 15, 1, 120)
        self.cache = OrderedDict()
        self.cache_bytes = 0
        self.cache_lock = threading.Lock()
        self._fake_blocked = {}
        with self._guard:
            for source in ('musicbrainz', 'discogs'):
                self._locks.setdefault(source, threading.Lock())

    def _setting(self, name, default, low, high):
        value = self.config.get(name, default)
        if type(value) is not int or not low <= value <= high:
            raise ValueError('目录识别配置 ' + name + ' 无效')
        return value

    def _source_cfg(self, source):
        value = self.config.get(source, {})
        if not isinstance(value, dict):
            raise ValueError('来源配置无效')
        return value

    def _enabled(self, source):
        return self.config.get('enabled', True) is True and self._source_cfg(source).get('enabled', True) is True

    @staticmethod
    def _query(query):
        if not isinstance(query, dict):
            raise ValueError('识别查询须为对象')
        title = _text(query.get('title', ''), 300)
        album = _text(query.get('album', ''), 300)
        artists = query.get('artists', [])
        if isinstance(artists, str):
            artists = [artists]
        if not isinstance(artists, list) or len(artists) > 20:
            raise ValueError('艺人查询无效')
        artists = list(dict.fromkeys(_text(name, 200, True) for name in artists))
        if not title and not album:
            raise ValueError('请提供歌曲或专辑名称')
        target_id = query.get('target_id')
        if target_id is not None:
            target_id = _text(target_id, 200)
        return {'title': title, 'artists': artists, 'album': album,
                'duration': _duration(query.get('duration')), 'target_id': target_id}

    @staticmethod
    def _error(exc):
        state = getattr(exc, 'error_class', 'PARSE_ERROR' if isinstance(exc, (ValueError, TypeError, KeyError)) else 'NETWORK_ERROR')
        messages = {'AUTH_REQUIRED': '来源需要独立凭据；Discogs 搜索请在设置中导入用户 Token。',
                    'AUTH_FAILED': '来源拒绝鉴权，请更新该来源凭据。',
                    'ACCESS_BLOCKED': '来源拒绝访问或返回跳转，未尝试绕过。',
                    'RATE_LIMITED': '来源限流，已暂停请求；请稍后重试。',
                    'PARSE_ERROR': '来源返回的目录数据无法完整解析。',
                    'INVALID_PAYLOAD': '来源响应超过大小上限或格式无效。',
                    'POLICY_BLOCKED': '当前运行模式或配置未允许在线目录识别。',
                    'NETWORK_ERROR': '目录来源网络访问失败。', 'TRANSIENT_ERROR': '目录来源请求超时。',
                    'NO_RESOURCE': '来源未找到该目录记录。'}
        return {'status': state, 'message': messages.get(state, '目录来源响应无效。')}

    def _get(self, source, path, params=None):
        if self.offline or not self._enabled(source):
            raise PolicyBlocked('在线目录识别未启用')
        if source == 'musicbrainz':
            allowed = path in ('/ws/2/recording', '/ws/2/release') or re.fullmatch(r'/ws/2/(recording|release)/[0-9a-f-]{36}', path)
        else:
            allowed = path == '/database/search' or re.fullmatch(r'/(releases|masters)/[1-9][0-9]{0,11}', path)
        if not allowed:
            raise PolicyBlocked('目录端点不在只读白名单')
        headers = {'User-Agent': USER_AGENT, 'Accept': 'application/json'}
        token = None
        if source == 'discogs':
            ref = self._source_cfg(source).get('credential_ref', '')
            token = self.secret_getter(ref, 'api.discogs.com') if ref else None
            if not token:
                raise HttpError('AUTH_REQUIRED')
            if not isinstance(token, str) or not re.fullmatch(r'[A-Za-z0-9_-]{8,256}', token):
                raise HttpError('AUTH_FAILED')
            headers['Authorization'] = 'Discogs token=' + token
        url = 'https://' + SOURCES[source]['host'] + path
        auth_digest = hashlib.sha256((token or '').encode()).hexdigest()
        cache_key = (source, path, json.dumps(params or {}, sort_keys=True), auth_digest)
        with self.cache_lock:
            entry = self.cache.get(cache_key)
            if entry and entry[0] > time.monotonic():
                self.cache.move_to_end(cache_key)
                return json.loads(entry[1])
            removed = self.cache.pop(cache_key, None)
            if removed:
                self.cache_bytes -= removed[2]
        transport = self.transports[source]
        fake = getattr(transport, 'is_fake', False)
        blocked = self._fake_blocked if fake else self._blocked
        lock = self._locks[source]
        self.checkpoint('catalog_before_request')
        while not lock.acquire(timeout=.1):
            self.checkpoint('catalog_rate_wait')
        try:
            if blocked.get(source, 0) > time.monotonic():
                raise HttpError('RATE_LIMITED')
            if not fake:
                delay = 1.05 - (time.monotonic() - self._last.get(source, 0))
                deadline = time.monotonic() + max(0, delay)
                while time.monotonic() < deadline:
                    self.checkpoint('catalog_rate_wait')
                    time.sleep(min(.1, deadline - time.monotonic()))
            self.checkpoint('catalog_request')
            try:
                response = transport.request('GET', url, params=params, headers=headers, timeout=self.timeout,
                                             allow_redirect_response=True, max_response_bytes=MAX_BYTES)
            finally:
                if not fake:
                    self._last[source] = time.monotonic()
            self.checkpoint('catalog_after_request')
            if response.url:
                actual = urlsplit(response.url)
                if actual.scheme != 'https' or actual.hostname != SOURCES[source]['host'] or actual.port not in (None, 443) or actual.path != path or actual.username or actual.password:
                    raise HttpError('ACCESS_BLOCKED')
            if response.status_code == 429 or response.status_code == 503 and any(k.lower() == 'retry-after' for k in response.headers):
                retry = next((v for k, v in response.headers.items() if k.lower() == 'retry-after'), '60')
                try:
                    seconds = float(retry)
                    if not math.isfinite(seconds):
                        raise ValueError()
                    seconds = max(1, min(3600, seconds))
                except (ValueError, TypeError):
                    seconds = 60
                blocked[source] = time.monotonic() + seconds
                raise HttpError('RATE_LIMITED')
        finally:
            lock.release()
        if response.status_code == 401:
            raise HttpError('AUTH_FAILED' if source == 'discogs' else 'AUTH_REQUIRED')
        if response.status_code == 403 or 300 <= response.status_code < 400:
            raise HttpError('ACCESS_BLOCKED')
        if response.status_code == 404:
            raise HttpError('NO_RESOURCE')
        if response.status_code != 200:
            raise HttpError('TRANSIENT_ERROR' if response.status_code >= 500 else 'NETWORK_ERROR')
        if len(response.body) > MAX_BYTES:
            raise HttpError('INVALID_PAYLOAD')
        try:
            payload = response.json()
        except (ValueError, UnicodeError):
            raise HttpError('INVALID_PAYLOAD') from None
        if not isinstance(payload, dict):
            raise HttpError('PARSE_ERROR')
        # Retain serialized bytes rather than a much larger Python object tree.
        # The fixed memory budget includes stored payload/key/entry overhead.
        encoded = json.dumps(payload, ensure_ascii=False, allow_nan=False, separators=(',', ':')).encode('utf-8')
        expires = time.monotonic() + self.cache_ttl
        cost = (sys.getsizeof(encoded) + sys.getsizeof(cache_key) + sum(sys.getsizeof(x) for x in cache_key)
                + sys.getsizeof((expires, encoded, 0)) + sys.getsizeof(expires) + sys.getsizeof(0) + 128)
        with self.cache_lock:
            for key in [k for k, entry in self.cache.items() if entry[0] <= time.monotonic()]:
                self.cache_bytes -= self.cache.pop(key)[2]
            removed = self.cache.pop(cache_key, None)
            if removed:
                self.cache_bytes -= removed[2]
            if self.cache_ttl and cost <= CACHE_MAX_BYTES:
                self.cache[cache_key] = (expires, encoded, cost)
                self.cache_bytes += cost
                while len(self.cache) > self.cache_limit or self.cache_bytes > CACHE_MAX_BYTES:
                    self.cache_bytes -= self.cache.popitem(last=False)[1][2]
        return payload

    @staticmethod
    def _candidate(source, source_id, kind, title, artists, album='', **extra):
        return {'schema_version': 1, 'candidate_id': source + ':' + source_id,
                'source': source, 'source_id': source_id, 'entity_kind': kind,
                'title': title, 'artists': artists, 'album': album,
                'recording_id': None, 'release_id': None, 'duration': None,
                'tracklist_complete': False, 'requires_review': True,
                'identity_confirmed': False, 'identity_evidence': [], **extra}

    @staticmethod
    def _rank(candidate, query):
        song = candidate['entity_kind'] == 'recording'
        title_match = bool(query['title'] and song and _norm(query['title']) == _norm(candidate['title']))
        album_match = bool(query['album'] and candidate['album'] and _norm(query['album']) == _norm(candidate['album']))
        artists_match = bool(query['artists']) and {_norm(a) for a in query['artists']} == {_norm(a) for a in candidate['artists']}
        duration_delta = None
        if query['duration'] is not None and candidate['duration'] is not None:
            duration_delta = round(abs(query['duration'] - candidate['duration']), 3)
        points = (50 if title_match else 0) + (30 if artists_match else 0) + (15 if album_match else 0) + (5 if duration_delta is not None and duration_delta <= 2 else 0)
        if not song:
            points = (60 if album_match else 0) + (30 if artists_match else 0)
        candidate.update(confidence=points / 100, confidence_kind='ranking_only',
                         text_match={'title': title_match, 'artists': artists_match, 'album': album_match},
                         duration_difference=duration_delta)
        return candidate

    @staticmethod
    def _warning(stage, exc=None, **details):
        error = CatalogResolver._error(exc) if exc is not None else {}
        return {'stage': stage, 'error_class': error.get('status', 'INCOMPLETE_METADATA'),
                'message': error.get('message', '部分来源元数据缺失或已按上限截断；保留其他候选。'), **details}

    @staticmethod
    def _search_result(candidates, warnings):
        status = 'PARTIAL' if candidates and warnings else ('OK' if candidates else 'NO_RESOURCE')
        if not candidates and warnings:
            status = warnings[0]['error_class']
            if status in ('INCOMPLETE_METADATA', 'RESULT_LIMIT'):
                status = 'PARSE_ERROR'
        return {'status': status, 'candidates': candidates, 'warnings': warnings}

    def _mb_recording(self, row, query, row_index, warnings):
        stage = 'recording_search'
        if not isinstance(row, dict):
            raise ValueError('录音候选结构无效')
        rid = _mbid(row.get('id'))
        title = _text(row.get('title'), required=True)
        artists = _credits(row.get('artist-credit'))
        if not artists:
            raise ValueError('录音来源缺少艺人信息')
        try:
            duration = _duration(row.get('length'), True)
        except (ValueError, TypeError) as exc:
            warnings.append(self._warning(stage, exc, row_index=row_index, field='length'))
            duration = None
        disambiguation = row.get('disambiguation')
        # Search responses may explicitly use null for optional annotation fields.
        try:
            disambiguation = '' if disambiguation is None else _text(disambiguation)
        except (ValueError, TypeError) as exc:
            warnings.append(self._warning(stage, exc, row_index=row_index, field='disambiguation'))
            disambiguation = ''
        releases = row.get('releases')
        if releases is None:
            releases = []
        elif not isinstance(releases, list):
            warnings.append(self._warning(stage, ValueError(), row_index=row_index, field='releases'))
            releases = []
        if len(releases) > 5:
            warnings.append(self._warning(stage, row_index=row_index, field='releases',
                                          error_class='RESULT_LIMIT', returned_count=len(releases), retained_limit=5))
        parsed_releases = []
        for release_index, release in enumerate(releases[:5]):
            try:
                if not isinstance(release, dict):
                    raise ValueError('关联发行候选结构无效')
                parsed_releases.append((_mbid(release.get('id')), _text(release.get('title'), required=True)))
            except (ValueError, TypeError) as exc:
                warnings.append(self._warning(stage, exc, row_index=row_index, release_index=release_index, field='release_identity'))
        candidates = []
        for release_id, album in parsed_releases or [(None, '')]:
            source_id = 'release:' + release_id if release_id else 'recording:' + rid
            candidate = self._candidate('musicbrainz', source_id, 'recording', title, artists, album,
                                        recording_id=rid, release_id=release_id,
                                        candidate_id='musicbrainz:recording:' + rid + (':release:' + release_id if release_id else ''),
                                        duration=duration, url='https://musicbrainz.org/' + source_id.replace(':', '/'),
                                        search_score=row.get('score'), disambiguation=disambiguation,
                                        identity_evidence=[{'kind': 'official_api_search', 'source': 'musicbrainz', 'entity_id': rid}],
                                        metadata_missing_fields=['album'] if not album else [])
            candidates.append(self._rank(candidate, query))
        return candidates

    def _mb_search(self, query):
        candidates, warnings = [], []
        for kind, field in (('recording', 'title'), ('release', 'album')):
            if not query[field]:
                continue
            stage = kind + '_search'
            search = kind + ':' + _lucene(query[field])
            for artist in query['artists']:
                search += ' AND artist:' + _lucene(artist)
            try:
                data = self._get('musicbrainz', '/ws/2/' + kind, {'query': search, 'fmt': 'json', 'limit': self.limit})
                rows = data.get(kind + 's')
                if not isinstance(rows, list):
                    raise ValueError('MusicBrainz 候选数组无效')
            except Exception as exc:
                if getattr(exc, 'error_class', None) == 'CANCELLED':
                    raise
                warnings.append(self._warning(stage, exc, scope='request'))
                # Do not make additional requests after source access/rate failures.
                if getattr(exc, 'error_class', None) in ('RATE_LIMITED', 'AUTH_REQUIRED', 'AUTH_FAILED', 'ACCESS_BLOCKED', 'POLICY_BLOCKED'):
                    break
                continue
            if len(rows) > self.limit:
                warnings.append(self._warning(stage, error_class='RESULT_LIMIT', returned_count=len(rows), retained_limit=self.limit))
            for row_index, row in enumerate(rows[:self.limit]):
                try:
                    if kind == 'recording':
                        candidates.extend(self._mb_recording(row, query, row_index, warnings))
                    else:
                        if not isinstance(row, dict):
                            raise ValueError('发行候选结构无效')
                        rid = _mbid(row.get('id'))
                        album = _text(row.get('title'), required=True)
                        artists = _credits(row.get('artist-credit'))
                        if not artists:
                            raise ValueError('发行来源缺少艺人信息')
                        candidate = self._candidate('musicbrainz', 'release:' + rid, 'release', album, artists, album,
                                                    release_id=rid, url='https://musicbrainz.org/release/' + rid,
                                                    search_score=row.get('score'), date=row.get('date'), country=row.get('country'),
                                                    identity_evidence=[{'kind': 'official_api_search', 'source': 'musicbrainz', 'entity_id': rid}])
                        candidates.append(self._rank(candidate, query))
                except (ValueError, TypeError, KeyError) as exc:
                    warnings.append(self._warning(stage, exc, row_index=row_index, scope='candidate'))
        return self._search_result(candidates, warnings)

    def _discogs_search(self, query):
        params = {'type': 'release', 'per_page': self.limit, 'page': 1}
        if query['artists']:
            params['artist'] = ' '.join(query['artists'])
        if query['album']:
            params['release_title'] = query['album']
        if query['title']:
            params['track'] = query['title']
        data = self._get('discogs', '/database/search', params)
        rows = data.get('results')
        if not isinstance(rows, list) or len(rows) > 100:
            raise ValueError('Discogs results 数组无效')
        candidates, warnings = [], []
        for row_index, row in enumerate(rows[:self.limit]):
            try:
                if not isinstance(row, dict) or row.get('type') != 'release':
                    raise ValueError('Discogs 候选类型无效')
                rid = _integer_id(row.get('id'))
                label = _text(row.get('title'), required=True)
                artist, sep, album = label.partition(' - ')
                artists = [artist] if sep else []
                album = album if sep else label
                if not artists:
                    warnings.append(self._warning('discogs_search', row_index=row_index, field='artists'))
                candidate = self._candidate('discogs', 'release:' + rid, 'release', album, artists, album,
                                            release_id=rid, url='https://www.discogs.com/release/' + rid,
                                            year=row.get('year'), country=row.get('country'),
                                            metadata_missing_fields=['artists'] if not artists else [],
                                            identity_evidence=[{'kind': 'official_api_search', 'source': 'discogs', 'entity_id': rid,
                                                                'note': '检索标题待发行详情核对；尚未确认曲目属于此发行。'}])
                candidates.append(self._rank(candidate, query))
            except (ValueError, TypeError, KeyError) as exc:
                warnings.append(self._warning('discogs_search', exc, row_index=row_index, scope='candidate'))
        return self._search_result(candidates, warnings)

    def identify(self, query):
        try:
            query = self._query(query)
        except (ValueError, TypeError):
            return {'status': 'INVALID_INPUT', 'message': '请提供有效歌曲/专辑、艺人及有限时长。',
                    'candidates': [], 'sources': [], 'lookup_links': [], 'read_only': True}
        candidates, sources = [], []
        links = lookup_links(query)
        if self.offline or not self.config.get('enabled', True):
            return {'status': 'POLICY_BLOCKED', 'query': query, 'candidates': [], 'sources': [],
                    'lookup_links': links, 'read_only': True, 'requires_review': True}
        for source, search in (('musicbrainz', self._mb_search), ('discogs', self._discogs_search)):
            self.checkpoint('catalog_source')
            if not self._enabled(source):
                sources.append({'source': source, 'mode': 'official_api', 'status': 'DISABLED'})
                continue
            try:
                outcome = search(query)
                found = outcome['candidates']
                candidates.extend(found)
                sources.append({'source': source, 'mode': 'official_api', 'status': outcome['status'],
                                'candidate_count': len(found), 'warnings': outcome['warnings'], 'search_complete': False})
            except Exception as exc:
                if getattr(exc, 'error_class', None) == 'CANCELLED':
                    raise
                sources.append({'source': source, 'mode': 'official_api', **self._error(exc)})
        for source in ('allmusic', 'rateyourmusic', 'albumoftheyear', 'vgmdb'):
            reason = '未接入已核验授权的公开 API；打开官网人工核对。'
            if source == 'albumoftheyear':
                reason = '官网条款要求自动访问获得书面许可；提供人工搜索。'
            sources.append({'source': source, 'mode': 'manual_lookup', 'status': 'MANUAL_LOOKUP', 'message': reason})
        unique = {c['candidate_id']: c for c in candidates}
        ranked = sorted(unique.values(), key=lambda c: (-c['confidence'], c['candidate_id']))[:self.limit]
        successful = any(s['status'] in ('OK', 'NO_RESOURCE') for s in sources if s['mode'] == 'official_api')
        failed = [s for s in sources if s['mode'] == 'official_api' and s['status'] not in ('OK', 'NO_RESOURCE', 'DISABLED')]
        status = ('PARTIAL' if failed else 'OK') if ranked else ('NO_RESOURCE' if successful else (failed[0]['status'] if failed else 'POLICY_BLOCKED'))
        return {'status': status, 'query': query, 'candidates': ranked, 'sources': sources,
                'lookup_links': links, 'read_only': True, 'requires_review': True,
                'warnings': [{'source': s['source'], **warning} for s in sources for warning in s.get('warnings', [])],
                'search_complete': False, 'observed_at': datetime.now(timezone.utc).isoformat()}

    @staticmethod
    def _relations(data):
        relations = data.get('relations', [])
        if not isinstance(relations, list) or len(relations) > 1000:
            raise ValueError('MusicBrainz 外链关系无效')
        result = []
        for relation in relations:
            if not isinstance(relation, dict) or relation.get('target-type') != 'url':
                continue
            url = relation.get('url', {})
            url = url.get('resource') if isinstance(url, dict) else None
            if not isinstance(url, str) or len(url) > 2000:
                continue
            try:
                parsed = urlsplit(url)
                if parsed.scheme not in ('http', 'https') or parsed.username or parsed.password or parsed.port not in (None, 80, 443):
                    continue
            except ValueError:
                continue
            host = (parsed.hostname or '').lower().removeprefix('www.')
            for source, meta in SOURCES.items():
                if host == meta['host'].removeprefix('www.') or source == 'discogs' and host == 'discogs.com':
                    canonical = parsed._replace(scheme='https', netloc=parsed.hostname).geturl()
                    result.append({'source': source, 'url': canonical, 'kind': 'musicbrainz_url_relationship',
                                   'relationship_type': relation.get('type'), 'identity_evidence': True,
                                   'evidence_source': 'musicbrainz', 'external_page_fetched': False, 'requires_review': True})
                    break
        return list({item['url']: item for item in result}.values())

    def _mb_detail(self, kind, ident):
        inc = 'artist-credits+url-rels+releases' if kind == 'recording' else 'artist-credits+recordings+release-groups+url-rels+release-group-level-rels'
        data = self._get('musicbrainz', '/ws/2/' + kind + '/' + ident, {'fmt': 'json', 'inc': inc})
        if _mbid(data.get('id')) != ident:
            raise ValueError('详情 ID 与所选候选不符')
        title = _text(data.get('title'), required=True)
        artists = _credits(data.get('artist-credit'))
        candidate = self._candidate('musicbrainz', kind + ':' + ident, kind, title, artists, title if kind == 'release' else '',
                                    url='https://musicbrainz.org/' + kind + '/' + ident,
                                    release_id=ident if kind == 'release' else None,
                                    recording_id=ident if kind == 'recording' else None,
                                    duration=_duration(data.get('length'), True),
                                    identity_evidence=[{'kind': 'official_api_detail', 'source': 'musicbrainz', 'entity_id': ident}])
        relations = self._relations(data)
        group = data.get('release-group')
        if isinstance(group, dict):
            relations.extend(self._relations(group))
            candidate['release_group_id'] = _mbid(group['id'])
        if kind == 'recording':
            releases = data.get('releases', [])
            if not isinstance(releases, list) or len(releases) > 100:
                raise ValueError('录音关联发行无效')
            candidate['releases'] = [{'release_id': _mbid(r.get('id')), 'album': _text(r.get('title'), required=True),
                                      'source_id': 'release:' + _mbid(r.get('id')),
                                      'url': 'https://musicbrainz.org/release/' + _mbid(r.get('id'))} for r in releases]
            candidate['related_releases_complete'] = False  # MusicBrainz includes are capped.
        else:
            media = data.get('media')
            if not isinstance(media, list) or not 1 <= len(media) <= 100:
                raise ValueError('发行介质字段无效')
            tracks, complete = [], True
            for medium in media:
                if not isinstance(medium, dict):
                    raise ValueError('发行介质结构无效')
                rows = medium.get('tracks')
                count = medium.get('track-count')
                disc = medium.get('position')
                if not isinstance(rows, list) or type(count) is not int or not 0 <= count <= 2000 or type(disc) is not int or disc < 1:
                    raise ValueError('发行曲目结构无效')
                complete = complete and len(rows) == count
                for track in rows:
                    if len(tracks) >= 2000 or not isinstance(track, dict) or not isinstance(track.get('recording'), dict):
                        raise ValueError('发行曲目超过上限或结构无效')
                    recording = track['recording']
                    position = track.get('position')
                    if type(position) is not int or position < 1:
                        raise ValueError('曲序无效')
                    tracks.append({'disc': disc, 'position': position, 'title': _text(track.get('title') or recording.get('title'), required=True),
                                   'artists': _credits(track.get('artist-credit', recording.get('artist-credit')), artists),
                                   'recording_id': _mbid(recording.get('id')), 'duration': _duration(track.get('length', recording.get('length')), True)})
                complete = complete and len({t['position'] for t in tracks if t['disc'] == disc}) == count
            complete = complete and len({m['position'] for m in media}) == len(media) and bool(tracks)
            candidate.update(tracklist=tracks, tracklist_complete=complete, date=data.get('date'), country=data.get('country'),
                             barcode=data.get('barcode'), status=data.get('status'))
        candidate['lookup_links'] = list({r['url']: r for r in relations}.values())
        return candidate

    def _discogs_detail(self, kind, ident):
        data = self._get('discogs', ('/releases/' if kind == 'release' else '/masters/') + ident)
        if _integer_id(data.get('id')) != ident:
            raise ValueError('详情 ID 与所选候选不符')
        album = _text(data.get('title'), required=True)
        artists = _credits(data.get('artists'))
        candidate = self._candidate('discogs', kind + ':' + ident, kind, album, artists, album,
                                    release_id=ident if kind == 'release' else None,
                                    url='https://www.discogs.com/' + kind + '/' + ident,
                                    identity_evidence=[{'kind': 'official_api_detail', 'source': 'discogs', 'entity_id': ident}])
        rows = data.get('tracklist')
        if not isinstance(rows, list) or len(rows) > 2000:
            raise ValueError('Discogs 曲目结构无效')
        tracks = []
        complete = True
        for row in rows:
            if not isinstance(row, dict):
                raise ValueError('Discogs 曲目结构无效')
            row_type = row.get('type_', 'track')
            if row_type == 'heading':
                continue
            if row_type != 'track' or row.get('sub_tracks'):
                complete = False
                continue  # Index suites need human interpretation, never fabricate tracks.
            duration = row.get('duration', '')
            seconds = None
            if duration:
                if not isinstance(duration, str) or not re.fullmatch(r'\d{1,4}:[0-5]\d', duration):
                    raise ValueError('Discogs 曲目时长无效')
                minutes, sec = duration.split(':')
                seconds = int(minutes) * 60 + int(sec)
            tracks.append({'position': _text(row.get('position', ''), 40), 'title': _text(row.get('title'), required=True),
                           'artists': _credits(row.get('artists'), artists), 'duration': _duration(seconds), 'recording_id': None})
        complete = complete and bool(tracks) and all(t['position'] for t in tracks) and len({t['position'] for t in tracks}) == len(tracks)
        candidate.update(tracklist=tracks, tracklist_complete=complete, year=data.get('year'),
                         country=data.get('country'), lookup_links=[])
        return candidate

    def resolve_candidate(self, source, source_id):
        try:
            if source not in ('musicbrainz', 'discogs') or not isinstance(source_id, str):
                raise ValueError('只能读取已接入目录来源的候选详情')
            kind, sep, ident = source_id.partition(':')
            if not sep or kind not in (('recording', 'release') if source == 'musicbrainz' else ('release', 'master')):
                raise ValueError('候选类型无效')
            ident = _mbid(ident) if source == 'musicbrainz' else _integer_id(ident)
        except (ValueError, TypeError):
            return {'status': 'INVALID_INPUT', 'message': '来源、候选类型或 ID 无效。',
                    'read_only': True, 'requires_review': True}
        try:
            self.checkpoint('catalog_detail')
            candidate = self._mb_detail(kind, ident) if source == 'musicbrainz' else self._discogs_detail(kind, ident)
            return {'status': 'OK', 'candidate': candidate, 'lookup_links': candidate['lookup_links'],
                    'read_only': True, 'requires_review': True, 'observed_at': datetime.now(timezone.utc).isoformat()}
        except Exception as exc:
            if getattr(exc, 'error_class', None) == 'CANCELLED':
                raise
            failure = self._error(exc)
            return {**failure, 'read_only': True, 'requires_review': True, 'source': source}
