"""Bounded public-page evidence, independent of catalogue API credentials."""
from __future__ import annotations

import copy
import hashlib
import json
import re
import threading
import time
from collections import OrderedDict
from html.parser import HTMLParser
from urllib.parse import urljoin, urlsplit
from urllib.robotparser import RobotFileParser

from musicops.clients.http import HttpError, PolicyBlocked, SafeHttpTransport
from musicops.providers.catalog import CatalogResolver, MAX_BYTES, USER_AGENT, lookup_links


HOSTS = {'musicbrainz': 'musicbrainz.org', 'discogs': 'www.discogs.com',
         'allmusic': 'www.allmusic.com', 'rateyourmusic': 'rateyourmusic.com',
         'albumoftheyear': 'www.albumoftheyear.org', 'vgmdb': 'vgmdb.net'}
PATHS = {'musicbrainz': r'/(?:release|recording)/[0-9a-f-]{36}',
         'discogs': r'/(?:[a-z]{2}/)?(?:release|master)/[1-9][0-9]*(?:-[^/?#]*)?',
         'allmusic': r'/album/[^/?#]+', 'rateyourmusic': r'/release/[^?#]+',
         'albumoftheyear': r'/album/[^/?#]+', 'vgmdb': r'/album/[1-9][0-9]*'}


def detail_url(value):
    """Only public catalogue detail URLs, with no query, credentials or ports."""
    if not isinstance(value, str) or len(value) > 2000:
        return None
    try:
        parsed = urlsplit(value)
        if parsed.scheme != 'https' or parsed.username or parsed.password or parsed.port or parsed.query:
            return None
        host = (parsed.hostname or '').removeprefix('www.')
        for source, allowed in HOSTS.items():
            if host == allowed.removeprefix('www.') and re.fullmatch(PATHS[source], parsed.path, re.I):
                return source, 'https://' + allowed + parsed.path
    except ValueError:
        pass
    return None


class Page(HTMLParser):
    def __init__(self, text):
        super().__init__(convert_charrefs=True)
        self.links, self.documents, self.parts = [], [], []
        self.script = False
        self.feed(text)

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == 'a' and len(self.links) < 5000 and attrs.get('href'):
            self.links.append(attrs['href'])
        if tag == 'script':
            self.script = attrs.get('type', '').lower().split(';')[0] == 'application/ld+json'
            self.parts = []

    def handle_data(self, text):
        if self.script:
            self.parts.append(text)

    def handle_endtag(self, tag):
        if tag == 'script' and self.script:
            self.script = False
            if len(self.documents) < 30:
                try:
                    self.documents.append(json.loads(''.join(self.parts)))
                except (ValueError, RecursionError):
                    pass


def _nodes(documents):
    pending = [(doc, 0) for doc in documents]
    count = 0
    while pending and count < 5000:
        value, depth = pending.pop()
        count += 1
        if depth > 12:
            continue
        if isinstance(value, dict):
            yield value
            pending.extend((v, depth + 1) for v in value.values() if isinstance(v, (dict, list)))
        elif isinstance(value, list):
            pending.extend((v, depth + 1) for v in value[:2000])


def _text(value):
    return value.strip() if isinstance(value, str) and len(value) <= 500 and not any(ord(c) < 32 for c in value) else ''


def _artists(value):
    values = value if isinstance(value, list) else [value]
    return list(dict.fromkeys(name for item in values[:20] if (name := _text(item.get('name') if isinstance(item, dict) else item))))


def _duration(value):
    match = re.fullmatch(r'PT(?:(\d{1,4})H)?(?:(\d{1,5})M)?(?:(\d{1,6}(?:\.\d+)?)S)?', str(value or ''))
    if not match or not any(match.groups()):
        return None
    result = sum(float(n or 0) * unit for n, unit in zip(match.groups(), (3600, 60, 1)))
    return result if 0 < result <= 604800 else None


class WebCatalog:
    def __init__(self, config, *, transport=None, checkpoint=None):
        self.config = config
        self.checkpoint = checkpoint or (lambda *_: None)
        self.transport = transport or SafeHttpTransport(HOSTS.values(), offline=False)
        self.cache, self.robots, self.observed = OrderedDict(), {}, OrderedDict()
        self.lock = threading.RLock()
        self.last, self.blocked = {}, {}

    def _request(self, url, limit=MAX_BYTES):
        self.checkpoint('catalog_website_request')
        host = urlsplit(url).hostname
        with self.lock:
            cached = self.cache.get(url)
            if cached and cached[0] > time.monotonic():
                return cached[1]
            # Cache and rate limit apply to failed/empty pages as well as matches.
            wait = max(0, 1.05 - (time.monotonic() - self.last.get(host, 0)))
            if not getattr(self.transport, 'is_fake', False):
                deadline = time.monotonic() + wait
                while time.monotonic() < deadline:
                    self.checkpoint('catalog_website_wait')
                    time.sleep(min(.1, max(0, deadline - time.monotonic())))
            self.last[host] = time.monotonic()
            response = self.transport.request('GET', url, headers={'User-Agent': USER_AGENT, 'Accept': 'text/html,text/plain'},
                                              timeout=min(self.config.get('timeout', 15), 8), max_response_bytes=limit)
            self.cache[url] = (time.monotonic() + min(self.config.get('cache_ttl', 86400), 86400), response)
            while len(self.cache) > 24:
                self.cache.popitem(last=False)
        return response

    def _page(self, source, url):
        if self.config.get('enabled', True) is not True or self.config.get('web_enabled', True) is not True:
            raise PolicyBlocked('网站识别已关闭')
        if source == 'albumoftheyear':
            raise PolicyBlocked('Album of the Year 自动访问需要网站许可；保留人工入口')
        if source not in HOSTS or urlsplit(url).hostname != HOSTS[source]:
            raise PolicyBlocked('网站地址不在允许范围')
        with self.lock:
            blocked = self.blocked.get(source)
            if blocked and blocked[0] > time.monotonic():
                raise HttpError(blocked[1])
        root = 'https://' + HOSTS[source]
        with self.lock:
            if source not in self.robots:
                response = self._request(root + '/robots.txt', 200_000)
                robot = RobotFileParser(root + '/robots.txt')
                if response.status_code == 404:
                    robot.parse([])
                elif response.status_code == 200:
                    robot.parse(response.text.splitlines())
                else:
                    raise PolicyBlocked('网站未允许读取访问规则')
                self.robots[source] = robot
            if not self.robots[source].can_fetch('MusicOps', url):
                raise PolicyBlocked('网站访问规则不允许抓取此页面')
        response = self._request(url)
        if response.status_code in (401, 403, 429):
            raise HttpError('RATE_LIMITED' if response.status_code == 429 else 'ACCESS_BLOCKED')
        if response.status_code == 404:
            return Page('')
        if response.status_code != 200:
            raise HttpError('NETWORK_ERROR')
        if re.search(r'<title>\s*(?:Verifying your browser|Just a moment|Access Denied)', response.text, re.I):
            raise HttpError('ACCESS_BLOCKED', '网站要求浏览器验证，请打开来源核对')
        return Page(response.text)

    def read(self, url):
        checked = detail_url(url)
        if not checked:
            return []
        source, url = checked
        page = self._page(source, url)
        candidates = []
        for node in _nodes(page.documents):
            types = node.get('@type', [])
            types = [types] if isinstance(types, str) else types
            if not isinstance(types, list) or not set(types) & {'MusicAlbum', 'MusicRecording'}:
                continue
            title = _text(node.get('name')); artists = _artists(node.get('byArtist'))
            if not title or not artists:
                continue
            if node.get('url') and (not isinstance(node['url'], str) or detail_url(urljoin(url, node['url'])) != checked):
                continue
            album = 'MusicAlbum' in types
            source_id = 'web:' + hashlib.sha256((url + '\n' + title).encode()).hexdigest()[:32]
            candidate = CatalogResolver._candidate(source, source_id, 'release' if album else 'recording', title, artists,
                title if album else _text((node.get('inAlbum') or {}).get('name')) if isinstance(node.get('inAlbum'), dict) else '',
                url=url, duration=_duration(node.get('duration')), lookup_links=[], external_page_fetched=True,
                identity_evidence=[{'kind': 'public_page_jsonld', 'source': source, 'url': url}])
            track = node.get('track') or []
            track = track.get('itemListElement', []) if isinstance(track, dict) else track
            tracks = []
            if album and isinstance(track, list) and len(track) <= 2000:
                for item in track:
                    if not isinstance(item, dict): continue
                    row = item.get('item', item)
                    if not isinstance(row, dict): continue
                    name = _text(row.get('name')); position = item.get('position', row.get('position'))
                    if not name or type(position) is not int or position < 1: continue
                    tracks.append({'title': name, 'artists': _artists(row.get('byArtist')) or artists,
                                   'position': position, 'disc': None, 'recording_id': None, 'duration': _duration(row.get('duration'))})
                count = node.get('numTracks', node.get('numberOfTracks'))
                complete = bool(tracks) and type(count) is int and count == len(tracks) and {t['position'] for t in tracks} == set(range(1, count + 1))
                candidate.update(tracklist=tracks, tracklist_complete=complete)
            with self.lock:
                self.observed[(source, source_id)] = copy.deepcopy(candidate)
                while len(self.observed) > 256: self.observed.popitem(last=False)
            candidates.append(candidate)
        return candidates[:10]

    def identify(self, query):
        candidates, sources = [], []
        detail_budget = 6
        for link in lookup_links(query):
            source = link['source']; self.checkpoint('catalog_website_source')
            try:
                page = self._page(source, link['url'])
                urls = list(dict.fromkeys(checked[1] for href in page.links
                    if (checked := detail_url(urljoin(link['url'], href))) and checked[0] == source))
                found = []
                for url in urls[:min(1, detail_budget)]:
                    detail_budget -= 1
                    found.extend(self.read(url))
                candidates.extend(found)
                sources.append({'source': source, 'mode': 'public_website', 'status': 'OK' if found else 'UNSUPPORTED_PAGE' if urls else 'NO_RESOURCE', 'candidate_count': len(found)})
                if RecognitionMatch(query, found): break
            except Exception as exc:
                if getattr(exc, 'error_class', None) == 'CANCELLED': raise
                state = getattr(exc, 'error_class', None)
                if state in ('NETWORK_ERROR', 'TRANSIENT_ERROR', 'RATE_LIMITED', 'ACCESS_BLOCKED', 'POLICY_BLOCKED'):
                    with self.lock: self.blocked[source] = (time.monotonic() + 300, state)
                sources.append({'source': source, 'mode': 'public_website', **CatalogResolver._error(exc)})
        return {'candidates': [CatalogResolver._rank(c, query) for c in candidates], 'sources': sources}

    def resolve(self, source, source_id):
        with self.lock:
            prior = copy.deepcopy(self.observed.get((source, source_id)))
        if not prior: return {'status': 'NO_RESOURCE', 'read_only': True, 'requires_review': True}
        for candidate in self.read(prior['url']):
            if candidate['source_id'] == source_id:
                return {'status': 'OK', 'candidate': candidate, 'lookup_links': [], 'read_only': True, 'requires_review': True}
        return {'status': 'NO_RESOURCE', 'read_only': True, 'requires_review': True}


def RecognitionMatch(query, candidates):
    from musicops.app.recognition import RecognitionFallback
    return RecognitionFallback._has_unique_exact(query, candidates)
