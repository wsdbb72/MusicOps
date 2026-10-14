import base64
import hashlib
import json
import struct

import pytest

from musicops.clients.http import HttpError, PolicyBlocked
from musicops.clients.roon import INFO_REQUEST, MAX_RESPONSE, LocalRoonWebSocket, RoonReadOnlyClient, inspect_local_logs, parse_moo_info


def moo(data=None, name='Success', request_id='0'):
    if data is None:
        data = {'core_id': 'private-core-id', 'display_name': 'Local Core', 'display_version': '2.70 production'}
    body = json.dumps(data).encode()
    return (f'MOO/1 COMPLETE {name}\nRequest-Id: {request_id}\nContent-Type: application/json\nContent-Length: {len(body)}\n\n'.encode() + body)


class FakeTransport:
    def __init__(self, result=None):
        self.result = result if result is not None else moo()
        self.requests = []
    def exchange(self, *args, **kwargs):
        self.requests.append((args, kwargs))
        if isinstance(self.result, Exception):
            raise self.result
        return self.result


def test_preflight_distinguishes_core_connected_from_library_acceptance():
    transport = FakeTransport()
    out = RoonReadOnlyClient({}, transport=transport).preflight()
    assert out['connected'] and out['status'] == 'AUTH_REQUIRED'
    assert out['core_info']['display_name'] == 'Local Core'
    assert out['core_info']['core_id_hash'] == hashlib.sha256(b'private-core-id').hexdigest()
    assert 'private-core-id' not in json.dumps(out)
    assert out['library_visibility'] == 'AUTH_REQUIRED' and out['storage_configuration'] == 'UNVERIFIED'
    assert out['read_only'] and not out['registration_attempted'] and not out['token_saved'] and not out['playback_used']
    args, kwargs = transport.requests[0]
    assert args == ('ws://127.0.0.1:9330/api', INFO_REQUEST) and kwargs['max_bytes'] == MAX_RESPONSE


@pytest.mark.parametrize('config', [{'offline': True, 'music_servers': {'roon': {}}},
                                   {'host': 'example.com'}, {'host': '127.0.0.1', 'port': 0},
                                   {'port': True}, {'timeout': float('nan')}, {'timeout': 999}])
def test_invalid_or_offline_config_never_connects(config):
    transport = FakeTransport()
    out = RoonReadOnlyClient(config, transport=transport).preflight()
    assert out['status'] in ('POLICY_BLOCKED', 'INVALID_INPUT') and not out['connected']
    assert not transport.requests


@pytest.mark.parametrize('message', [moo(request_id='1'), moo(data={'core_id': 'a'}), moo(data=[]),
                                    moo().replace(b'Content-Length: ', b'Content-Length: 999'),
                                    moo().replace(b'Request-Id: 0', b'Request-Id: 0\nRequest-Id: 0'),
                                    moo().replace(b'application/json', b'text/plain'), b'x' * (MAX_RESPONSE + 1),
                                    b'MOO/1 COMPLETE Success\nRequest-Id: 0\n\n{}'],
                         ids=['request_id', 'missing_fields', 'json_array', 'length_mismatch', 'duplicate_header',
                              'content_type', 'over_limit', 'unexpected_body'])
def test_invalid_moo_messages_are_rejected(message):
    with pytest.raises(HttpError) as exc:
        parse_moo_info(message)
    assert exc.value.error_class in ('PARSE_ERROR', 'INVALID_PAYLOAD')


def test_unauthorized_api_reply_is_explicit():
    transport = FakeTransport(moo(name='NotRegistered'))
    out = RoonReadOnlyClient({}, transport=transport).preflight()
    assert out['status'] == 'AUTH_REQUIRED' and not out['connected']


def test_cancellation_propagates_to_worker():
    class Cancelled(Exception):
        error_class = 'CANCELLED'
    def checkpoint(_):
        raise Cancelled()
    with pytest.raises(Cancelled):
        RoonReadOnlyClient({}, transport=FakeTransport(), checkpoint=checkpoint).preflight()


def frame(opcode, body, final=True):
    first = bytes([(0x80 if final else 0) | opcode])
    length = len(body)
    return first + (bytes([length]) if length < 126 else b'\x7e' + struct.pack('!H', length)) + body


class FakeSocket:
    def __init__(self, frames=None, bad_accept=False):
        self.buffer = bytearray()
        self.frames = frames if frames is not None else frame(2, moo())
        self.bad_accept = bad_accept
        self.sent = []
        self.closed = False
    def settimeout(self, _):
        pass
    def sendall(self, data):
        self.sent.append(data)
        if data.startswith(b'GET '):
            key = next(line.partition(':')[2].strip() for line in data.decode().splitlines() if line.startswith('Sec-WebSocket-Key:'))
            accept = base64.b64encode(hashlib.sha1((key + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest()).decode()
            if self.bad_accept:
                accept = 'bad'
            self.buffer.extend(('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ' + accept + '\r\n\r\n').encode() + self.frames)
    def recv(self, count):
        result = bytes(self.buffer[:count])
        del self.buffer[:count]
        return result
    def close(self):
        self.closed = True


def test_stdlib_ws_handshake_masks_request_and_accepts_fragment_with_ping(monkeypatch):
    message = moo()
    sock = FakeSocket(frame(9, b'ping') + frame(2, message[:60], final=False) + frame(0, message[60:]))
    monkeypatch.setattr('musicops.clients.roon.socket.create_connection', lambda *_a, **_k: sock)
    raw = LocalRoonWebSocket().exchange('ws://localhost:9330/api', INFO_REQUEST)
    assert parse_moo_info(raw)['display_name'] == 'Local Core' and sock.closed
    request = sock.sent[1]
    assert request[0] == 0x82 and request[1] & 0x80
    mask = request[2:6]
    assert bytes(value ^ mask[i % 4] for i, value in enumerate(request[6:])) == INFO_REQUEST
    assert sock.sent[2][0] == 0x8A  # Pong only; no registry/register or playback.


@pytest.mark.parametrize('frames, expected', [(b'\x82\x7f' + struct.pack('!Q', MAX_RESPONSE + 1), 'INVALID_PAYLOAD'),
                                             (frame(0, b'bad'), 'PARSE_ERROR'), (b'\x82\x80', 'PARSE_ERROR'),
                                             (frame(8, b''), 'NETWORK_ERROR')])
def test_ws_size_and_protocol_errors_close_connection(monkeypatch, frames, expected):
    sock = FakeSocket(frames)
    monkeypatch.setattr('musicops.clients.roon.socket.create_connection', lambda *_a, **_k: sock)
    with pytest.raises(HttpError) as exc:
        LocalRoonWebSocket().exchange('ws://127.0.0.1:9330/api', INFO_REQUEST)
    assert exc.value.error_class == expected and sock.closed


def test_ws_invalid_handshake_is_rejected(monkeypatch):
    sock = FakeSocket(bad_accept=True)
    monkeypatch.setattr('musicops.clients.roon.socket.create_connection', lambda *_a, **_k: sock)
    with pytest.raises(HttpError) as exc:
        LocalRoonWebSocket().exchange('ws://127.0.0.1:9330/api', INFO_REQUEST)
    assert exc.value.error_class == 'PARSE_ERROR' and sock.closed


@pytest.mark.parametrize('url,payload', [('ws://evil.example:9330/api', INFO_REQUEST),
                                       ('ws://127.0.0.1:9330/api?token=x', INFO_REQUEST),
                                       ('ws://127.0.0.1:9330/api', b'MOO/1 REQUEST registry/register'),
                                       ('ws://127.0.0.1:9330/internal', INFO_REQUEST)])
def test_ws_allowlist_blocks_arbitrary_requests(url, payload):
    with pytest.raises(PolicyBlocked):
        LocalRoonWebSocket().exchange(url, payload)


def test_existing_log_evidence_does_not_claim_current_storage_or_visibility(tmp_path):
    log = tmp_path / 'RoonServer_log.txt'
    music = str(tmp_path / 'music')
    file = str(tmp_path / 'music' / 'private-song.flac')
    log.write_text(f'10/13 18:18:38 Trace: [storage] [directory] directorystorage init, location({music}, dirwatcher is available: True)\n'
                   f"10/13 18:18:39 Warn: [storage] [directory] Failed to extract audio format from '{file}': CorruptFile\n"
                   f"10/13 18:18:40 Warn: [storage] [directory] Failed to extract audio format from '{file}': CorruptFile\n"
                   'secret_token=do-not-export\n', encoding='utf-8')
    out = inspect_local_logs(tmp_path, expected_root=music)
    assert out['expected_root_observed'] and out['storage_roots'][0]['watcher_available']
    assert out['import_error_occurrences'] == {'CorruptFile': 2} and out['distinct_import_error_files'] == 1
    assert not out['storage_configuration_verified'] and not out['library_visibility_verified']
    assert 'private-song' not in json.dumps(out) and 'do-not-export' not in json.dumps(out)


def test_log_read_is_bounded_and_missing_directory_not_success(tmp_path):
    (tmp_path / 'RoonServer_log.txt').write_bytes(b'0' * 1000)
    out = inspect_local_logs(tmp_path, max_bytes=20)
    assert out['bytes_read'] == 20 and out['logs'][0]['truncated']
    assert inspect_local_logs(tmp_path / 'absent')['status'] == 'NO_RESOURCE'
