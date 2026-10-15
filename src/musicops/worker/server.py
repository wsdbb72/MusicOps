from __future__ import annotations

import argparse
import hashlib
import hmac
import json
from pathlib import Path
from multiprocessing import AuthenticationError
from multiprocessing.connection import Listener

from musicops.worker.client import auth_key, pipe_address, MAX_REQUEST_BYTES
from musicops.worker.lock import WriterLock
from musicops.app.service import AppService
from musicops.clients.http import redact
from musicops.platforms import ipc_family,prepare_ipc_socket,secure_ipc_socket,private_directory

READ_COMMANDS = {'attach', 'get_status', 'get_task', 'list_tasks', 'get_playlist_targets', 'list_library_files', 'events', 'parse_playlist','list_workflows','get_workflow'}
QUEUED_COMMANDS = {'demo', 'index_library', 'compare', 'search', 'resources', 'prepare_fill', 'prepare_resource', 'prepare_repair', 'prepare_dedup', 'submit_plan', 'request_rollback', 'export_report', 'refresh_playlist', 'submit_download', 'recover', 'pt_preflight', 'qbt_preflight', 'ai_analyze', 'ai_plan', 'ai_test', 'lyrics_preflight','identify_metadata','resolve_metadata','accept_metadata','validate_selected_file','roon_preflight'}
QUEUED_COMMANDS.update({'start_workflow','advance_workflows','restore_workflow','manual_album_search'})


def request_signature(command, payload, key):
    # Redacting before hashing collapses distinct credential/config requests.
    # A keyed digest includes the actual payload without storing secret hashes.
    body = json.dumps({'command': command, 'payload': payload}, sort_keys=True, ensure_ascii=False, separators=(',', ':'), allow_nan=False).encode('utf-8')
    return hmac.new(key, body, hashlib.sha256).hexdigest()


def error_response(exc, request_id=None):
    category = getattr(exc, 'error_class', type(exc).__name__)
    message = str(exc) if isinstance(exc, (RuntimeError, ValueError, KeyError)) else '后台请求失败，请查看本机业务事件。'
    return {'ok': False, 'request_id': request_id, 'error': redact(message), 'error_class': category}


def encode_response(response):
    try:
        return json.dumps(response, ensure_ascii=False, allow_nan=False).encode('utf-8')
    except (TypeError, ValueError, OverflowError):
        request_id = response.get('request_id') if isinstance(response, dict) else None
        fallback = {'ok': False, 'request_id': request_id if isinstance(request_id, str) else None,
                    'error': 'IPC_RESPONSE_INVALID_JSON：后台返回的数据无法编码，请查看业务事件。',
                    'error_class': 'API_CONTRACT_ERROR'}
        return json.dumps(fallback, ensure_ascii=False, allow_nan=False).encode('utf-8')


def handle_request(service, req, key):
    if not isinstance(req, dict):
        raise ValueError('IPC_INVALID_REQUEST')
    request_id, command, payload = req.get('request_id'), req.get('command'), req.get('payload', {})
    if req.get('schema_version') != 1 or req.get('profile_id') != 'default':
        raise ValueError('IPC_SCHEMA_OR_PROFILE_MISMATCH')
    if not isinstance(request_id, str) or not request_id.strip() or len(request_id) > 128:
        raise ValueError('IPC_INVALID_REQUEST_ID')
    if not isinstance(command, str) or not command or len(command) > 80 or not isinstance(payload, dict):
        raise ValueError('IPC_INVALID_COMMAND_OR_PAYLOAD')
    signature = request_signature(command, payload, key)
    previous = service.store.get('ipc_requests', request_id)
    if previous:
        if previous.get('signature_version') == 2:
            matches = hmac.compare_digest(previous.get('signature', ''), signature)
        else:
            raw = {'command': command, 'payload': payload}
            safe = redact(raw)
            if raw != safe:
                raise RuntimeError('REQUEST_RESULT_UNKNOWN：旧请求签名含脱敏字段，请先核对当前状态')
            legacy = hashlib.sha256(json.dumps(safe, sort_keys=True).encode()).hexdigest()
            matches = hmac.compare_digest(previous.get('signature', ''), legacy)
        if not matches:
            raise RuntimeError('REQUEST_ID_CONFLICT')
        if previous.get('response') is not None:
            return previous['response']
        if command not in READ_COMMANDS | QUEUED_COMMANDS:
            raise RuntimeError('REQUEST_RESULT_UNKNOWN：先读取当前状态，不盲重发')
    if command in READ_COMMANDS:
        # Polling is read-only and must return current state. Persisting every
        # full status response would grow the database continuously while idle.
        try:
            return {'ok': True, 'request_id': request_id, 'result': service.dispatch(command, payload, request_id=request_id)}
        except Exception as exc:
            return error_response(exc, request_id)
    record = {'signature': signature, 'signature_version': 2, 'command': command, 'state': 'INTENT', 'response': None}
    service.store.put('ipc_requests', request_id, record)
    try:
        result = service.dispatch(command, payload, request_id=request_id)
        response = {'ok': True, 'request_id': request_id, 'result': result}
    except Exception as exc:
        # Failures may follow a partial mutation. Preserve their response to
        # prevent a retry from executing that mutation again.
        response = error_response(exc, request_id)
    service.store.put('ipc_requests', request_id, {**record, 'state': 'DONE', 'response': response})
    return response


def main(argv=None):
    parser = argparse.ArgumentParser()
    parser.add_argument('--base', required=True)
    args = parser.parse_args(argv)
    base = Path(args.base).resolve()
    private_directory(base)
    try:
        lock = WriterLock(base).acquire()
    except RuntimeError:
        return 0
    service = None
    try:
        service = AppService(base,offline=False)
        key = auth_key(base)
        prepare_ipc_socket(base)
        address=pipe_address(base)
        with Listener(address, family=ipc_family(), authkey=key) as listener:
            secure_ipc_socket(address)
            while not service.stop.is_set():
                try:
                    with listener.accept() as conn:
                        request_id = None
                        try:
                            if not conn.poll(15):
                                continue
                            req = json.loads(conn.recv_bytes(MAX_REQUEST_BYTES))
                            request_id = req.get('request_id') if isinstance(req, dict) else None
                            response = handle_request(service, req, key)
                        except Exception as exc:
                            response = error_response(exc, request_id)
                        conn.send_bytes(encode_response(response))
                except (EOFError, OSError, AuthenticationError):
                    # Dropped clients, malformed frames and failed authentication
                    # must not terminate the shared worker.
                    continue
    finally:
        if service is not None:
            service.stop.set()
            service.thread.join()
        lock.release()
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
