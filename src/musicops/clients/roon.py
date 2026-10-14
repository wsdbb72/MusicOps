"""Bounded Roon Core information probe using the official MOO/WebSocket API.

This client never registers an extension, writes tokens, plays music, or opens
Roon's internal database. Core connectivity and library visibility are distinct.
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
import re
import socket
import struct
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlsplit

from musicops.clients.http import HttpError, PolicyBlocked

MAX_RESPONSE = 262144
INFO_REQUEST = b'MOO/1 REQUEST com.roonlabs.registry:1/info\nRequest-Id: 0\n\n'
_GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11'


def _json(data):
    def invalid_constant(_):
        raise ValueError('非有限 JSON 数值')
    return json.loads(data, parse_constant=invalid_constant)


def parse_moo_info(data):
    if isinstance(data, str):
        data = data.encode('utf-8')
    if not isinstance(data, bytes) or len(data) > MAX_RESPONSE:
        raise HttpError('INVALID_PAYLOAD', 'Roon 响应超过大小上限')
    header, separator, body = data.partition(b'\n\n')
    if not separator or len(header) > 8192:
        raise HttpError('PARSE_ERROR', 'Roon MOO 消息头无效')
    try:
        lines = header.decode('ascii').splitlines()
        match = re.fullmatch(r'MOO/1 (COMPLETE|CONTINUE) ([A-Za-z][A-Za-z0-9_]*)', lines[0])
        if not match:
            raise ValueError()
        headers = {}
        for line in lines[1:]:
            name, sep, value = line.partition(':')
            if not sep or name in headers:
                raise ValueError()
            headers[name] = value.strip()
        if headers.get('Request-Id') != '0':
            raise ValueError()
        length = headers.get('Content-Length')
        if length is None:
            if body or 'Content-Type' in headers:
                raise ValueError()
        elif not length.isdigit() or int(length) != len(body) or headers.get('Content-Type') != 'application/json':
            raise ValueError()
        name = match[2]
        if name in ('Unauthorized', 'NotRegistered', 'AuthorizationRequired', 'NotAuthorized'):
            raise HttpError('AUTH_REQUIRED', 'Roon 尚未授权该浏览扩展')
        if name != 'Success' or match[1] != 'COMPLETE':
            raise HttpError('API_CONTRACT_ERROR', 'Roon 信息请求未成功')
        data = _json(body.decode('utf-8'))
        if not isinstance(data, dict):
            raise ValueError()
        result = {}
        for field in ('core_id', 'display_name', 'display_version'):
            value = data.get(field)
            if not isinstance(value, str) or not value.strip() or len(value) > 500 or any(ord(c) < 32 for c in value):
                raise ValueError()
            result[field] = value
        return result
    except (ValueError, UnicodeError, IndexError):
        raise HttpError('PARSE_ERROR', 'Roon 信息响应结构无效') from None


class LocalRoonWebSocket:
    """A single request, no reconnect, no compression, capped frames and deadline."""
    def __init__(self, checkpoint=None):
        self.checkpoint = checkpoint or (lambda *_: None)

    def exchange(self, url, payload, timeout=8, max_bytes=MAX_RESPONSE):
        parsed = urlsplit(url)
        if parsed.scheme != 'ws' or parsed.hostname not in ('127.0.0.1', 'localhost', '::1') or parsed.path != '/api' or parsed.query or parsed.fragment or parsed.username or parsed.password:
            raise PolicyBlocked('Roon 本机探测仅允许 localhost 的 /api')
        if payload != INFO_REQUEST or type(max_bytes) is not int or not 1 <= max_bytes <= MAX_RESPONSE:
            raise PolicyBlocked('Roon 仅允许官方只读 info 请求')
        if type(timeout) not in (int, float) or not 1 <= timeout <= 20:
            raise ValueError('Roon 超时无效')
        host = '127.0.0.1' if parsed.hostname == 'localhost' else parsed.hostname
        deadline = time.monotonic() + timeout
        self.checkpoint('roon_before_connect')
        sock = None
        buffered = bytearray()
        def receive(count):
            while len(buffered) < count:
                self.checkpoint('roon_receive')
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise HttpError('TRANSIENT_ERROR', 'Roon 信息请求超时')
                sock.settimeout(min(.25, remaining))
                try:
                    block = sock.recv(min(4096, count - len(buffered)))
                except socket.timeout:
                    continue
                if not block:
                    raise HttpError('NETWORK_ERROR', 'Roon 已关闭连接')
                buffered.extend(block)
            result = bytes(buffered[:count])
            del buffered[:count]
            return result
        def send_frame(opcode, body=b''):
            mask = os.urandom(4)
            length = len(body)
            header = bytes([0x80 | opcode, 0x80 | length]) if length < 126 else bytes([0x80 | opcode, 0xFE]) + struct.pack('!H', length)
            sock.sendall(header + mask + bytes(value ^ mask[index % 4] for index, value in enumerate(body)))
        try:
            sock = socket.create_connection((host, parsed.port or 9330), timeout=min(3, timeout))
            nonce = base64.b64encode(os.urandom(16)).decode()
            display_host = '[' + host + ']' if ':' in host else host
            request = ('GET /api HTTP/1.1\r\nHost: ' + display_host + ':' + str(parsed.port or 9330) + '\r\n'
                       'Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: ' + nonce + '\r\n'
                       'Sec-WebSocket-Version: 13\r\nUser-Agent: MusicOps-RoonReadOnly/0.2.0\r\n\r\n')
            sock.sendall(request.encode('ascii'))
            head = bytearray()
            while not head.endswith(b'\r\n\r\n'):
                if len(head) >= 8192:
                    raise HttpError('INVALID_PAYLOAD', 'Roon WebSocket 消息头过大')
                head.extend(receive(1))
            lines = bytes(head).decode('ascii').split('\r\n')
            first = re.fullmatch(r'HTTP/1\.[01] ([0-9]{3})(?: .*)?', lines[0])
            if not first:
                raise HttpError('PARSE_ERROR', 'Roon WebSocket 握手无效')
            status = int(first[1])
            if status in (401, 403):
                raise HttpError('AUTH_REQUIRED' if status == 401 else 'ACCESS_BLOCKED')
            if status != 101:
                raise HttpError('API_CONTRACT_ERROR', '该端口未提供 Roon WebSocket API')
            headers = {}
            for line in lines[1:]:
                if not line:
                    continue
                name, sep, value = line.partition(':')
                if not sep or name.lower() in headers:
                    raise HttpError('PARSE_ERROR', 'Roon WebSocket 头无效')
                headers[name.lower()] = value.strip()
            expected = base64.b64encode(hashlib.sha1((nonce + _GUID).encode()).digest()).decode()
            if headers.get('sec-websocket-accept') != expected or headers.get('upgrade', '').lower() != 'websocket' or 'upgrade' not in headers.get('connection', '').lower():
                raise HttpError('PARSE_ERROR', 'Roon WebSocket 握手校验失败')
            send_frame(2, payload)
            result = bytearray()
            fragmented = False
            for _ in range(16):
                first, second = receive(2)
                final, opcode, length = bool(first & 0x80), first & 0x0F, second & 0x7F
                if first & 0x70 or second & 0x80:
                    raise HttpError('PARSE_ERROR', 'Roon WebSocket 帧不受支持')
                if length == 126:
                    length = struct.unpack('!H', receive(2))[0]
                elif length == 127:
                    length = struct.unpack('!Q', receive(8))[0]
                if opcode >= 8 and (not final or length > 125):
                    raise HttpError('PARSE_ERROR', 'Roon WebSocket 控制帧无效')
                if length > max_bytes or len(result) + length > max_bytes:
                    raise HttpError('INVALID_PAYLOAD', 'Roon 响应超过大小上限')
                data = receive(length)
                if opcode == 8:
                    raise HttpError('NETWORK_ERROR', 'Roon 已关闭连接')
                if opcode == 9:
                    send_frame(10, data)
                    continue
                if opcode == 10:
                    continue
                if opcode not in (0, 1, 2) or opcode == 0 and not fragmented or opcode in (1, 2) and fragmented:
                    raise HttpError('PARSE_ERROR', 'Roon WebSocket 分片无效')
                result.extend(data)
                fragmented = not final
                if final:
                    return bytes(result)
            raise HttpError('INVALID_PAYLOAD', 'Roon 响应帧数量超过上限')
        except socket.timeout:
            raise HttpError('TRANSIENT_ERROR', 'Roon 连接超时') from None
        except OSError:
            raise HttpError('NETWORK_ERROR', '无法连接本机 Roon Core') from None
        except UnicodeError:
            raise HttpError('PARSE_ERROR', 'Roon 握手编码无效') from None
        finally:
            if sock:
                try:
                    sock.close()
                except OSError:
                    pass


class RoonReadOnlyClient:
    def __init__(self, config=None, transport=None, checkpoint=None):
        config = config or {}
        self.offline = config.get('offline', False) is True
        self.config = config.get('music_servers', {}).get('roon', config)
        self.checkpoint = checkpoint or (lambda *_: None)
        self.transport = transport or LocalRoonWebSocket(self.checkpoint)

    def preflight(self):
        result = {'read_only': True, 'music_writes': 0, 'registration_attempted': False,
                  'token_saved': False, 'playback_used': False, 'connected': False,
                  'library_visibility': 'UNVERIFIED', 'storage_configuration': 'UNVERIFIED'}
        try:
            host = self.config.get('host', '127.0.0.1')
            port = self.config.get('port', 9330)
            timeout = self.config.get('timeout', 8)
            if self.offline:
                raise PolicyBlocked('当前演示档案未启用联网 Roon 探测')
            if host not in ('127.0.0.1', 'localhost', '::1') or type(port) is not int or not 1 <= port <= 65535 or type(timeout) not in (int, float) or not 1 <= timeout <= 20:
                raise ValueError('本机 Roon host/port/timeout 配置无效')
            self.checkpoint('roon_preflight')
            authority = '[' + host + ']' if ':' in host else host
            data = self.transport.exchange('ws://' + authority + ':' + str(port) + '/api', INFO_REQUEST, timeout=timeout, max_bytes=MAX_RESPONSE)
            core = parse_moo_info(data)
            result.update(status='AUTH_REQUIRED', connected=True, endpoint='ws://' + authority + ':' + str(port) + '/api',
                          core_info={'display_name': core['display_name'], 'display_version': core['display_version'],
                                     'core_id_hash': hashlib.sha256(core['core_id'].encode()).hexdigest()},
                          library_visibility='AUTH_REQUIRED',
                          message='Roon Core 信息连接成功；本次未注册浏览扩展，尚不能读取曲目列表。曲目可见性验收需在 Roon 中授权一个实际连接中的浏览扩展或使用客户端界面。',
                          observed_at=datetime.now(timezone.utc).isoformat())
        except Exception as exc:
            if getattr(exc, 'error_class', None) == 'CANCELLED':
                raise
            state = getattr(exc, 'error_class', 'INVALID_INPUT' if isinstance(exc, (ValueError, TypeError)) else 'NETWORK_ERROR')
            result.update(status=state, message={'NETWORK_ERROR': '无法连接本机 Roon Core；请确认服务器运行及 API 端口。',
                                                'TRANSIENT_ERROR': '本机 Roon 信息请求超时。',
                                                'AUTH_REQUIRED': 'Roon 需要浏览扩展授权。',
                                                'POLICY_BLOCKED': '当前档案未允许联网 Roon 探测。',
                                                'INVALID_INPUT': 'Roon 探测仅支持本机地址及有效端口、超时。'}.get(state, 'Roon 返回的 API 响应无法通过校验。'))
        return result


def inspect_local_logs(log_dir=None, expected_root=None, max_files=4, max_bytes=32_000_000):
    """Extract historical storage/error evidence; never expose raw log lines."""
    if type(max_files) is not int or not 1 <= max_files <= 10 or type(max_bytes) is not int or not 1 <= max_bytes <= 64_000_000:
        raise ValueError('日志检查上限无效')
    path = Path(log_dir) if log_dir is not None else Path(os.environ.get('LOCALAPPDATA', '')) / 'RoonServer' / 'Logs'
    if not path.is_dir():
        return {'status': 'NO_RESOURCE', 'log_evidence_found': False, 'storage_configuration_verified': False,
                'library_visibility_verified': False, 'read_only': True}
    files = sorted((p for p in path.glob('RoonServer_log*.txt') if p.is_file()), key=lambda p: p.stat().st_mtime, reverse=True)[:max_files]
    roots, errors, identities = {}, {}, set()
    total = 0
    inspected = []
    expected = os.path.normcase(os.path.normpath(expected_root)) if expected_root else None
    root_expression = re.compile(r'directorystorage init, location\((.+?), dirwatcher is available: (True|False)\)')
    failure_expression = re.compile(r"Failed to extract audio format from '(.+?)': ([A-Za-z0-9_]+)")
    for file in files:
        remaining = max_bytes - total
        if remaining <= 0:
            break
        # Tail reading is bounded and retains the most recent observation.
        size = file.stat().st_size
        amount = min(size, remaining, 10_000_000)
        with file.open('rb') as handle:
            if size > amount:
                handle.seek(size - amount)
            data = handle.read(amount)
        total += len(data)
        inspected.append({'file': file.name, 'bytes_read': len(data), 'truncated': size > amount})
        for line in data.decode('utf-8', errors='replace').splitlines():
            root = root_expression.search(line)
            if root:
                value = os.path.normpath(root[1])
                roots[os.path.normcase(value)] = {'path': value, 'watcher_available': root[2] == 'True',
                                                'log_timestamp': line[:14], 'evidence_kind': 'existing_server_log'}
            failure = failure_expression.search(line)
            if failure:
                value = os.path.normpath(failure[1])
                if expected and not (os.path.normcase(value).startswith(expected.rstrip('\\/') + os.sep)):
                    continue
                category = failure[2]
                errors[category] = errors.get(category, 0) + 1
                identities.add(hashlib.sha256(os.path.normcase(value).encode()).hexdigest())
    return {'status': 'OK' if inspected else 'NO_RESOURCE', 'read_only': True,
            'log_evidence_found': bool(roots or errors), 'storage_roots': list(roots.values()),
            'expected_root_observed': expected in roots if expected else None,
            'storage_configuration_verified': False, 'library_visibility_verified': False,
            'import_error_occurrences': errors, 'distinct_import_error_files': len(identities),
            'import_error_file_hashes': sorted(identities)[:20], 'logs': inspected, 'bytes_read': total,
            'evidence_note': '既有日志证明历史监听/导入事件，不等同当前 Storage 界面或曲目可见性验收。',
            'observed_at': datetime.now(timezone.utc).isoformat()}
