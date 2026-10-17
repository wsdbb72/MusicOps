import json
import pytest
from musicops.clients.browser_solver import BrowserChallengeTransport, FlareSolverrClient, validate_solver
from musicops.clients.http import FakeTransport, HttpError, Response

SETTINGS = {'enabled': True, 'base_url': 'http://127.0.0.1:8191', 'timeout': 60, 'auto_start': False, 'executable': ''}
URL = 'https://orpheus.network/ajax.php'
CHALLENGE = Response(403, b'<title>Just a moment</title><script src="/cdn-cgi/challenge-platform/x"></script>')


def solved(url=URL+'?action=browse&searchstr=Artist+Album', **changes):
    return {'status': 'ok', 'solution': {'url': url, 'status': 200, 'response': '<html><pre>{&quot;status&quot;:&quot;success&quot;,&quot;response&quot;:{}}</pre></html>',
        'headers': {'content-type': 'text/html'}, 'userAgent': 'Browser-UA',
        'cookies': [{'name': 'cf_clearance', 'value': 'clearance-fixture', 'domain': '.orpheus.network'},
                    {'name': 'session', 'value': 'ignored-session', 'domain': 'evil.invalid'}], **changes}}


def test_solver_recovers_api_json_and_reuses_clearance_with_exact_user_agent():
    requests = []; direct = FakeTransport([CHALLENGE, Response(200, b'normal')])
    solver = FlareSolverrClient(SETTINGS, post=lambda payload: requests.append(payload) or solved())
    transport = BrowserChallengeTransport(direct, SETTINGS, ['orpheus.network'], solver=solver)
    response = transport.request('GET', URL, params={'action': 'browse', 'searchstr': 'Artist Album'}, headers={'Cookie': 'session=fixture-login'}, max_response_bytes=1_000_000)
    assert response.json() == {'status': 'success', 'response': {}}
    assert requests[0]['cookies'] == [{'name': 'session', 'value': 'fixture-login', 'domain': 'orpheus.network', 'path': '/'}]
    transport.request('GET', URL, params={'action': 'torrent', 'id': 1}, headers={'Cookie': 'session=fixture-login'})
    assert direct.requests[1]['headers']['User-Agent'] == 'Browser-UA'
    assert direct.requests[1]['headers']['Cookie'] == 'session=fixture-login; cf_clearance=clearance-fixture'
    assert 'ignored-session' not in str(direct.requests)


@pytest.mark.parametrize('url,params,status', [
    ('https://orpheus.network/torrents.php', {'action': 'download', 'id': 1, 'usetoken': 1}, 403),
    ('https://www.open.cd/download.php', {'id': 1}, 403),
    (URL, {'action': 'browse'}, 429),
    (URL, {'action': 'browse'}, 401),
])
def test_download_fl_rate_limit_and_login_error_are_not_replayed(url, params, status):
    body = b'unauthorized' if status == 401 else CHALLENGE.body
    response = Response(status, body)
    solver = FlareSolverrClient(SETTINGS, post=lambda _: pytest.fail('not a recoverable read-only challenge'))
    transport = BrowserChallengeTransport(FakeTransport([response]), SETTINGS, ['orpheus.network', 'www.open.cd'], solver=solver)
    assert transport.request('GET', url, params=params) is response


@pytest.mark.parametrize('url', ['http://192.168.1.1:8191', 'https://solver.example.invalid', 'http://127.0.0.1:8191/v1', 'http://user:secret@localhost:8191', 'http://localhost:8191?token=x'])
def test_solver_cookie_destination_is_loopback_only(url):
    with pytest.raises(ValueError): validate_solver({**SETTINGS, 'base_url': url})


@pytest.mark.parametrize('result', [solved(url='https://evil.invalid/ajax.php?action=browse&searchstr=Artist+Album'), solved(url='https://orpheus.network/login.php'), solved(response='<title>Just a moment</title>'), {'status': 'error', 'message': 'private response'}])
def test_bad_solver_result_is_rejected_without_replaying_direct_request(result):
    direct = FakeTransport([CHALLENGE])
    transport = BrowserChallengeTransport(direct, SETTINGS, ['orpheus.network'], solver=FlareSolverrClient(SETTINGS, post=lambda _: result))
    with pytest.raises(HttpError): transport.request('GET', URL, params={'action': 'browse', 'searchstr': 'Artist Album'})
    assert len(direct.requests) == 1


def test_disabled_solver_does_not_receive_cookie_or_make_request():
    transport = BrowserChallengeTransport(FakeTransport([CHALLENGE]), {**SETTINGS, 'enabled': False}, ['orpheus.network'], solver=FlareSolverrClient(SETTINGS, post=lambda _: pytest.fail('disabled')))
    assert transport.request('GET', URL, params={'action': 'browse'}, headers={'Cookie': 'fixture'}) is CHALLENGE


def test_real_loopback_client_contract_and_proxy_environment(monkeypatch):
    import threading
    from http.server import BaseHTTPRequestHandler, HTTPServer
    received = []
    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            received.append((self.path, json.loads(self.rfile.read(int(self.headers['Content-Length'])))))
            data = json.dumps({'status': 'ok', 'sessions': [], 'version': 'fixture'}).encode()
            self.send_response(200); self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
        def log_message(self, *args): pass
    server = HTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
    monkeypatch.setenv('HTTP_PROXY', 'http://127.0.0.1:1')
    try:
        result = FlareSolverrClient({**SETTINGS, 'base_url': 'http://127.0.0.1:'+str(server.server_port)}).check()
        assert result['status'] == 'OK' and received == [('/v1', {'cmd': 'sessions.list'})]
    finally:
        server.shutdown(); server.server_close(); thread.join(3)
