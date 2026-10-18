"""Local FlareSolverr adapter with read-only challenge recovery and UA binding."""
from __future__ import annotations
import json
import os
from pathlib import Path
import re
import subprocess
import threading
import time
from html import unescape
from urllib.parse import parse_qs, urlencode, urlsplit, urlunsplit
from musicops.clients.http import HttpError, PolicyBlocked, Response, SafeHttpTransport


def validate_solver(settings):
    try:
        parsed = urlsplit(settings['base_url'])
        valid = parsed.scheme in ('http', 'https') and parsed.hostname in ('127.0.0.1', 'localhost', '::1') and parsed.port not in (0,) and parsed.path in ('', '/') and not (parsed.username or parsed.password or parsed.query or parsed.fragment)
    except (KeyError, ValueError, TypeError): valid = False
    if not valid: raise ValueError('FlareSolverr 地址须为本机回环服务，例如 http://127.0.0.1:8191')
    timeout = settings.get('timeout', 60)
    if type(timeout) is not int or not 10 <= timeout <= 120: raise ValueError('浏览器验证超时须为 10–120 秒')
    return parsed


def challenge(response):
    if response.status_code == 429: return False
    if any(k.lower() == 'cf-mitigated' and str(v).lower() == 'challenge' for k, v in response.headers.items()): return True
    return bool(re.search(rb'cf-chl-|/cdn-cgi/challenge-platform/|<title>\s*(?:Just a moment|Verifying your browser)|ddos-guard', response.body[:100_000], re.I))


class FlareSolverrClient:
    def __init__(self, settings, *, checkpoint=None, post=None):
        validate_solver(settings)
        self.settings, self.post = settings, post
        self.checkpoint = checkpoint or (lambda *_: None)
        self.lock = threading.Lock()
        self.process = None

    def _post(self, payload):
        self.checkpoint('browser_solver_request')
        if self.post: return self.post(payload)
        import httpx
        try:
            with httpx.Client(follow_redirects=False, trust_env=False, timeout=httpx.Timeout(self.settings.get('timeout', 60) + 5, connect=3)) as client:
                with client.stream('POST', self.settings['base_url'].rstrip('/') + '/v1', json=payload, headers={'X-MusicOps-Service':self.settings['_managed_token']} if self.settings.get('_managed_token') else {}) as response:
                    if response.status_code != 200: raise HttpError('BROWSER_SOLVER_FAILED', '本机 FlareSolverr 返回错误')
                    body = bytearray()
                    for block in response.iter_bytes():
                        self.checkpoint('browser_solver_response')
                        if len(body) + len(block) > 20_000_000: raise HttpError('INVALID_PAYLOAD', '浏览器验证响应过大')
                        body.extend(block)
            return json.loads(body)
        except httpx.ConnectError:
            raise HttpError('BROWSER_SOLVER_UNAVAILABLE', '本机 FlareSolverr 未运行，请在连接向导设置服务或可执行程序') from None
        except httpx.HTTPError:
            raise HttpError('BROWSER_SOLVER_FAILED', '浏览器验证连接失败或超时') from None

    def start_local(self):
        path = self.settings.get('executable', '')
        if not path: return False
        executable = Path(path)
        if not executable.is_absolute() or not executable.is_file() or executable.suffix.lower() != '.exe' or os.name != 'nt':
            raise ValueError('请选择 Windows FlareSolverr 原生可执行程序')
        parsed = validate_solver(self.settings)
        if self.process and self.process.poll() is None: return True
        environment = os.environ.copy()
        environment.update(HOST='127.0.0.1', PORT=str(parsed.port or (443 if parsed.scheme == 'https' else 80)), LOG_LEVEL='error', LOG_HTML='false')
        self.process = subprocess.Popen([str(executable)], cwd=executable.parent, env=environment,
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, creationflags=subprocess.CREATE_NO_WINDOW)
        return True

    def request(self, payload):
        with self.lock:
            try: return self._post(payload)
            except HttpError as exc:
                if exc.error_class != 'BROWSER_SOLVER_UNAVAILABLE' or not self.settings.get('auto_start') or not self.start_local(): raise
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                self.checkpoint('browser_solver_starting'); time.sleep(.25)
                try: return self._post(payload)
                except HttpError as exc:
                    if exc.error_class != 'BROWSER_SOLVER_UNAVAILABLE': raise
            raise HttpError('BROWSER_SOLVER_UNAVAILABLE', 'FlareSolverr 启动后仍未就绪')

    def check(self):
        result = self.request({'cmd': 'sessions.list'})
        if not isinstance(result, dict) or result.get('status') != 'ok' or not isinstance(result.get('sessions'), list):
            raise HttpError('BROWSER_SOLVER_FAILED', '本机服务不符合 FlareSolverr 契约')
        return {'status': 'OK', 'message': 'FlareSolverr 已连接；仅在浏览器挑战时介入', 'version': str(result.get('version', ''))[:80], 'read_only': True}


class BrowserChallengeTransport:
    is_fake = False
    def __init__(self, direct, settings, allowed_hosts, *, checkpoint=None, solver=None):
        self.direct, self.settings = direct, settings
        self.allowed_hosts = set(allowed_hosts)
        self.solver = solver or FlareSolverrClient(settings, checkpoint=checkpoint)
        self.clearance = {}; self.lock = threading.RLock()

    @staticmethod
    def _readonly(url, params):
        parsed = urlsplit(url)
        values = {**parse_qs(parsed.query), **(params or {})}
        path = parsed.path
        # Download/FL GETs can consume tokens. Never replay those via a browser.
        if any(key.lower() in ('usetoken', 'passkey', 'authkey', 'torrent_pass') for key in values): return False
        if path in ('/download.php',): return False
        if path == '/torrents.php': return values.get('action') in (None, 'browse', ['browse'])
        if path == '/ajax.php':
            action = values.get('action'); action = action[0] if isinstance(action, list) and len(action) == 1 else action
            return action in ('index', 'browse', 'torrent', 'user')
        return path in ('/robots.txt', '/details.php', '/plugin_details.php', '/search', '/search/', '/search.php') or bool(re.match(r'/(?:release|recording|master|album)/', path))

    def request(self, method, url, params=None, headers=None, **kwargs):
        parsed = urlsplit(url)
        if parsed.scheme != 'https' or parsed.hostname not in self.allowed_hosts or parsed.username or parsed.password or parsed.port not in (None, 443):
            raise PolicyBlocked('浏览器辅助目标不在允许的 HTTPS 站点范围')
        original = dict(headers or {}); outgoing = dict(original)
        with self.lock:
            stored = self.clearance.get(parsed.hostname)
            if stored and stored[0] > time.monotonic():
                outgoing['User-Agent'] = stored[1]
                cookie = '; '.join(pair.strip() for k, v in outgoing.items() if k.lower() == 'cookie'
                                   for pair in v.split(';') if pair.strip().partition('=')[0] != 'cf_clearance')
                outgoing = {k: v for k, v in outgoing.items() if k.lower() != 'cookie'}
                outgoing['Cookie'] = '; '.join(filter(None, (cookie, stored[2])))
        response = self.direct.request(method, url, params=params, headers=outgoing, **kwargs)
        if not self.settings.get('enabled') or method != 'GET' or not challenge(response) or not self._readonly(url, params): return response
        query = parsed.query
        if params: query = '&'.join(filter(None, (query, urlencode(params, doseq=True))))
        target = urlunsplit(parsed._replace(query=query))
        cookies = []
        for name, value in original.items():
            if name.lower() == 'cookie':
                for pair in value.split(';'):
                    key, sep, val = pair.strip().partition('=')
                    if sep: cookies.append({'name': key, 'value': val, 'domain': parsed.hostname, 'path': '/'})
        result = self.solver.request({'cmd': 'request.get', 'url': target, 'maxTimeout': self.settings.get('timeout', 60) * 1000, 'cookies': cookies})
        if not isinstance(result, dict) or result.get('status') != 'ok' or not isinstance(result.get('solution'), dict):
            raise HttpError('BROWSER_CHALLENGE_UNRESOLVED', '浏览器挑战未通过，请手动核对或稍后重试')
        solution = result['solution']; actual = urlsplit(solution.get('url', ''))
        if actual.scheme != 'https' or actual.hostname != parsed.hostname or actual.port not in (None, 443) or actual.username or actual.password or actual.path != parsed.path or parse_qs(actual.query) != parse_qs(query):
            raise HttpError('ACCESS_BLOCKED', '浏览器验证后地址改变，请检查登录态')
        text = solution.get('response')
        if not isinstance(text, str) or len(text.encode()) > kwargs.get('max_response_bytes', 10_000_000): raise HttpError('INVALID_PAYLOAD')
        # Chromium wraps JSON responses in a pre element. Decode only this
        # observed representation; never execute scripts or use HTML as JSON.
        if parsed.path == '/ajax.php':
            pre = re.search(r'<pre\b[^>]*>(.*?)</pre>', text, re.S | re.I)
            if pre: text = unescape(pre.group(1))
        response_headers = solution.get('headers') or {}
        if not isinstance(response_headers, dict): raise HttpError('INVALID_PAYLOAD')
        solved = Response(solution.get('status', 0), text.encode(), response_headers, target)
        if solved.status_code != 200 or challenge(solved): raise HttpError('BROWSER_CHALLENGE_UNRESOLVED', '浏览器验证仍未通过')
        clearance = []
        for cookie in solution.get('cookies', []) if isinstance(solution.get('cookies'), list) else []:
            if not isinstance(cookie, dict) or cookie.get('name') != 'cf_clearance': continue
            domain = str(cookie.get('domain', '')).lstrip('.')
            value = cookie.get('value')
            if domain == parsed.hostname and isinstance(value, str) and len(value) < 10000 and not any(c in value for c in '\r\n;\x00'):
                clearance.append('cf_clearance=' + value)
        ua = solution.get('userAgent')
        if clearance and isinstance(ua, str) and len(ua) < 2000 and not any(c in ua for c in '\r\n'):
            with self.lock: self.clearance[parsed.hostname] = (time.monotonic() + 900, ua, '; '.join(clearance))
        return solved


def browser_transport(settings, hosts, *, checkpoint=None, solver=None):
    direct = SafeHttpTransport(hosts, offline=False)
    return BrowserChallengeTransport(direct, settings, hosts, checkpoint=checkpoint, solver=solver) if settings.get('enabled') else direct
