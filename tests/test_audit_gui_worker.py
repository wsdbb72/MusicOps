import copy
import hashlib
import json
import os
import time
from concurrent.futures import ThreadPoolExecutor

os.environ.setdefault('QT_QPA_PLATFORM', 'offscreen')
import pytest
from PySide6.QtWidgets import QApplication, QPushButton

from musicops.config.schema import default_config
from musicops.gui.main import MainWindow
from musicops.state.store import StateStore
from musicops.worker.client import WorkerClient, WorkerRequestError, auth_key, pipe_address, MAX_REQUEST_BYTES
from musicops.worker.lock import WriterLock
from musicops.worker.server import handle_request, request_signature, encode_response


@pytest.fixture(scope='module')
def app():
    return QApplication.instance() or QApplication([])


def drain(app, predicate, timeout=5):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        app.processEvents()
        if predicate():
            return
        time.sleep(.005)
    assert predicate(), 'GUI异步结果等待超时'


def req(command='set_secret', payload=None, request_id='stable'):
    return {'schema_version': 1, 'request_id': request_id, 'profile_id': 'default', 'command': command, 'payload': payload or {}}


class ServiceFixture:
    def __init__(self, path):
        self.store = StateStore(path)
        self.calls = []
        self.fail = False

    def dispatch(self, command, payload, request_id=None):
        self.calls.append((command, payload))
        if self.fail:
            raise RuntimeError('CONFIG_CONFLICT：配置已更新')
        return {'saved': True}


def test_ipc_secret_fields_have_distinct_keyed_signatures_and_conflicts(tmp_path):
    service = ServiceFixture(tmp_path / 'state.sqlite')
    first = req(payload={'password': 'fixture-first'})
    changed = req(payload={'password': 'fixture-second'})
    assert request_signature(first['command'], first['payload'], b'a') != request_signature(changed['command'], changed['payload'], b'a')
    assert request_signature(first['command'], first['payload'], b'a') != request_signature(first['command'], first['payload'], b'b')
    handle_request(service, first, b'a')
    with pytest.raises(RuntimeError, match='REQUEST_ID_CONFLICT'):
        handle_request(service, changed, b'a')
    assert len(service.calls) == 1
    assert 'fixture-first' not in json.dumps(service.store.list('ipc_requests'))


def test_ipc_failed_response_is_durable_without_repeating_partial_mutation(tmp_path):
    service = ServiceFixture(tmp_path / 'state.sqlite')
    service.fail = True
    first = handle_request(service, req(), b'key')
    service.fail = False
    assert not first['ok']
    assert handle_request(service, req(), b'key') == first
    assert len(service.calls) == 1


def test_readonly_polling_does_not_store_response_copies(tmp_path):
    service = ServiceFixture(tmp_path / 'state.sqlite')
    for number in range(10):
        assert handle_request(service, req(command='get_status', request_id=str(number)), b'key')['ok']
    assert service.store.list('ipc_requests') == []
    assert len(service.calls) == 10


@pytest.mark.parametrize('invalid_value', [float('nan'), float('inf'), b'unsupported-bytes'])
def test_invalid_response_payload_has_controlled_contract_error(invalid_value):
    response = json.loads(encode_response({'ok': True, 'request_id': 'fixture', 'result': {'value': invalid_value}}))
    assert response == {'ok': False, 'request_id': 'fixture', 'error': 'IPC_RESPONSE_INVALID_JSON：后台返回的数据无法编码，请查看业务事件。', 'error_class': 'API_CONTRACT_ERROR'}


@pytest.mark.skipif(os.name != 'nt', reason='Windows named pipe')
def test_worker_survives_legacy_state_with_nonfinite_task_result(tmp_path):
    base = tmp_path / 'profile'
    store = StateStore(base / 'state' / 'musicops.sqlite')
    store.put('tasks', 'legacy', {'task_id': 'legacy', 'command': 'fixture', 'state': 'completed', 'result': {'value': float('nan')}})
    client = WorkerClient(base)
    try:
        client.ensure_running()
        with pytest.raises(WorkerRequestError) as error:
            client.call('get_task', {'task_id': 'legacy'})
        assert error.value.error_class == 'API_CONTRACT_ERROR'
        assert client.call('get_status', {'compact': True})['config']['version'] == 1
    finally:
        try:
            client.call('shutdown_worker')
        except Exception:
            pass


def test_ipc_uncertain_mutation_is_not_blindly_reissued(tmp_path):
    service = ServiceFixture(tmp_path / 'state.sqlite')
    request = req(command='add_playlists', payload={'text': '123'})
    service.store.put('ipc_requests', 'stable', {'signature': request_signature(request['command'], request['payload'], b'key'), 'signature_version': 2, 'response': None})
    with pytest.raises(RuntimeError, match='REQUEST_RESULT_UNKNOWN'):
        handle_request(service, request, b'key')
    assert service.calls == []


@pytest.mark.parametrize('wire_request', [[], req(payload={'a': float('nan')}), {**req(), 'request_id': ''}, {**req(), 'payload': []}])
def test_ipc_invalid_shapes_and_nonfinite_numbers_are_rejected(tmp_path, wire_request):
    service = ServiceFixture(tmp_path / 'state.sqlite')
    with pytest.raises(ValueError):
        handle_request(service, wire_request, b'key')
    assert service.calls == []


def test_legacy_read_replay_works_and_redacted_legacy_payload_is_uncertain(tmp_path):
    service = ServiceFixture(tmp_path / 'state.sqlite')
    request = req(command='get_status')
    signature = hashlib.sha256(json.dumps({'command': 'get_status', 'payload': {}}, sort_keys=True).encode()).hexdigest()
    service.store.put('ipc_requests', 'stable', {'signature': signature, 'response': {'ok': True, 'result': {'legacy': True}}})
    assert handle_request(service, request, b'key')['result']['legacy']
    service.store.put('ipc_requests', 'stable', {'signature': 'old-redacted', 'response': {'ok': True}})
    with pytest.raises(RuntimeError, match='REQUEST_RESULT_UNKNOWN'):
        handle_request(service, req(payload={'password': 'fixture-only'}), b'key')


def test_writer_lock_reacquire_guard_and_no_file_growth(tmp_path):
    lock = WriterLock(tmp_path)
    with lock:
        with pytest.raises(RuntimeError, match='ALREADY_ACQUIRED'):
            lock.acquire()
    for _ in range(10):
        with lock:
            pass
    assert lock.path.stat().st_size == 1


@pytest.mark.skipif(os.name != 'nt', reason='Windows DPAPI / named pipe')
def test_parallel_auth_initialization_has_one_key_and_atomic_file(tmp_path):
    with ThreadPoolExecutor(4) as pool:
        keys = list(pool.map(lambda _: auth_key(tmp_path), range(4)))
    assert len(set(keys)) == 1 and len(keys[0]) == 32
    assert not (tmp_path / 'vault' / 'ipc.tmp').exists()


def test_client_rejects_oversized_and_nonobject_payload_before_connecting(tmp_path, monkeypatch):
    import musicops.worker.client as module
    connections = []
    monkeypatch.setattr(module, 'Client', lambda *a, **k: connections.append(1))
    client = WorkerClient(tmp_path)
    with pytest.raises(ValueError, match='TOO_LARGE'):
        client.call('test', {'content': 'x' * MAX_REQUEST_BYTES})
    with pytest.raises(ValueError, match='OBJECT'):
        client.call('test', [])
    assert connections == []


class GuiClient:
    def __init__(self, base):
        self.calls = []
        self.config = {'version': 1, 'values': default_config(base)}
        self.tasks = []

    def ensure_running(self):
        pass

    def status(self):
        return {'config': copy.deepcopy(self.config), 'tasks': [{k: v for k, v in task.items() if k != 'result'} for task in self.tasks]}

    def call(self, command, payload):
        self.calls.append((command, copy.deepcopy(payload)))
        if command in ('attach', 'get_status'):
            return self.status()
        if command == 'get_task':
            return {'task': copy.deepcopy(next(t for t in self.tasks if t['task_id'] == payload['task_id']))}
        if command == 'same':
            time.sleep(payload['delay'])
            return {'name': payload['name']}
        if command == 'update_config':
            raise WorkerRequestError('CONFIG_CONFLICT：请重新载入配置', 'CONFIG_CONFLICT')
        if command == 'queued':
            self.tasks.append({'task_id': 'q1', 'command': 'queued', 'state': 'completed', 'result': {'files': [{'file_id': 'fixture', 'path': 'synthetic.flac'}]}})
            return {'accepted': True, 'task_id': 'q1'}
        return {}


def test_same_command_out_of_order_callbacks_belong_to_the_request(app, tmp_path):
    client = GuiClient(tmp_path)
    window = MainWindow(tmp_path, client, autostart=False)
    window.pool.setMaxThreadCount(2)
    received = []
    window.request('same', {'name': 'first', 'delay': .12}, lambda result: received.append(('first', result['name'])))
    window.request('same', {'name': 'second', 'delay': 0}, lambda result: received.append(('second', result['name'])))
    drain(app, lambda: window.pending == 0)
    assert received == [('second', 'second'), ('first', 'first')]
    assert not window.callbacks
    window.close()


def test_config_editor_keeps_its_loaded_version_and_rejections_keep_connection(app, tmp_path):
    client = GuiClient(tmp_path)
    window = MainWindow(tmp_path, client, autostart=False)
    window._status(client.status())
    client.config['version'] = 2
    window._status(client.status())
    assert window.config_version == 2 and window.editor.version_number == 1
    window.findChild(QPushButton, 'save_config').click()
    drain(app, lambda: window.pending == 0)
    assert next(payload for command, payload in client.calls if command == 'update_config')['expected_version'] == 1
    assert '后台断连' not in window.banner.text()
    assert 'CONFIG_CONFLICT' in window.statusBar().currentMessage()
    window.close()


def test_compact_status_fetches_only_pending_terminal_task_result(app, tmp_path):
    client = GuiClient(tmp_path)
    window = MainWindow(tmp_path, client, autostart=False)
    window.request('queued', {}, lambda result: window.result_to_page('音乐库与音质', result))
    drain(app, lambda: window.pending == 0)
    window._status(client.status())
    drain(app, lambda: window.pending == 0 and not window.task_callbacks)
    window._status(client.status())
    assert window.tables['音乐库与音质'].rowCount() == 1
    assert sum(command == 'get_task' for command, _ in client.calls) == 1
    window.refresh()
    drain(app, lambda: window.pending == 0)
    assert next(payload for command, payload in client.calls if command == 'get_status')['compact'] is True
    window.close()


def test_unchanged_rows_preserve_selection_and_avoid_duplicate_row_payloads(app, tmp_path):
    window = MainWindow(tmp_path, GuiClient(tmp_path), autostart=False)
    rows = [{'target_id': 'a', 'title': 'One'}, {'target_id': 'b', 'title': 'Two'}]
    window.show_rows('歌单与补齐', rows)
    table = window.tables['歌单与补齐']
    table.selectRow(1)
    first_item = table.item(0, 0)
    window.show_rows('歌单与补齐', copy.deepcopy(rows))
    assert table.item(0, 0) is first_item and window.selected('歌单与补齐')['target_id'] == 'b'
    assert table.item(0, 1).data(256) is None
    window.show_rows('歌单与补齐', list(reversed(rows)))
    assert window.selected('歌单与补齐')['target_id'] == 'b'
    window.close()


def test_resource_display_fields_do_not_change_candidate_submitted_to_service(app, tmp_path):
    client = GuiClient(tmp_path)
    window = MainWindow(tmp_path, client, autostart=False)
    candidate = {'candidate_id': 'fixture', 'title': 'Song', 'source': 'fixture',
                 'media': {'format': 'FLAC', 'encoding': 'Lossless', 'size_display': '1 MiB'},
                 'release': {'edition': 'Original'}, 'manifest': {'verified': True}}
    window.show_rows('资源搜索', [candidate])
    table = window.tables['资源搜索']
    table.selectRow(0)
    assert window.selected('资源搜索') == candidate
    headers = [table.horizontalHeaderItem(i).text() for i in range(table.columnCount())]
    assert table.item(0, headers.index('格式')).text() == 'FLAC'
    window._candidate_plan()
    drain(app, lambda: window.pending == 0)
    submitted = next(payload['candidate'] for command, payload in client.calls if command == 'prepare_fill')
    assert submitted == candidate and 'format' not in submitted and 'edition' not in submitted
    window.close()


def test_guided_secret_has_default_ref_after_domain_change_and_empty_rejected(app, tmp_path):
    client = GuiClient(tmp_path)
    client.config['values']['ai']['auth_secret_ref'] = ''
    window = MainWindow(tmp_path, client, autostart=False)
    window._status(client.status())
    window.secret_purpose.setCurrentIndex(1)
    assert window.secret_ref.text().startswith('vault://ai/')
    window._save_secret()
    assert window.pending == 0 and not client.calls
    window.secret_value.setText('fixture-only')
    window._save_secret()
    drain(app, lambda: window.pending == 0)
    assert window.editor.controls[('ai', 'auth_secret_ref')][0].text() == window.secret_ref.text()
    assert '保存全部配置' in window.statusBar().currentMessage()
    window.close()


def test_ai_context_is_bounded_and_declares_truncation(app, tmp_path):
    client = GuiClient(tmp_path)
    window = MainWindow(tmp_path, client, autostart=False)
    window.status_cache = {'files': [{'file_id': str(i), 'raw_tags': 'x' * 100_000} for i in range(60)], 'reviews': [{'target_id': str(i), 'large_evidence': 'x' * 100_000} for i in range(70)]}
    window._analyze_ai()
    drain(app, lambda: window.pending == 0)
    context = next(payload['context'] for command, payload in client.calls if command == 'ai_analyze')
    assert len(context['files']) == len(context['reviews']) == 50
    assert context['totals'] == {'files': 60, 'reviews': 70}
    assert context['truncated'] == {'files': True, 'reviews': True}
    assert len(json.dumps(context)) < 5000
    window.close()


def test_empty_selection_and_identity_mismatch_do_not_start_requests(app, tmp_path):
    client = GuiClient(tmp_path)
    window = MainWindow(tmp_path, client, autostart=False)
    window._search_selected()
    window._playlist_resources()
    window._do_search()
    window._prepare_resource()
    window.show_rows('歌词与封面', [{'resource_id': 'fixture', 'kind': 'lyrics', 'identity_state': 'mismatch'}])
    window.tables['歌词与封面'].selectRow(0)
    window._prepare_resource()
    assert window.pending == 0 and not client.calls
    assert '不符' in window.statusBar().currentMessage()
    window.close()


@pytest.mark.parametrize('arguments', [['--payload', '[]'], ['--wait-timeout', 'nan']])
def test_cli_invalid_options_do_not_start_worker(arguments, monkeypatch, capsys):
    import musicops.cli as cli
    starts = []
    monkeypatch.setattr(cli, 'WorkerClient', lambda base: starts.append(base))
    assert cli.main(arguments) == 7
    assert starts == [] and 'error' in capsys.readouterr().out


def test_cli_cancelled_task_has_failure_exit_status(monkeypatch, capsys):
    import musicops.cli as cli
    class Client:
        def ensure_running(self):
            pass
        def call(self, command, payload):
            if command == 'get_task':
                return {'task': {'task_id': 'fixture', 'state': 'cancelled'}}
            return {'task_id': 'fixture', 'accepted': True}
    monkeypatch.setattr(cli, 'WorkerClient', lambda base: Client())
    assert cli.main(['search', '--wait']) == 2
    assert 'cancelled' in capsys.readouterr().out


@pytest.mark.skipif(os.name != 'nt', reason='Windows named pipe')
def test_worker_survives_failed_authentication_and_oversized_frames(tmp_path):
    from multiprocessing import AuthenticationError
    from multiprocessing.connection import Client
    client = WorkerClient(tmp_path / 'profile')
    try:
        client.ensure_running()
        with pytest.raises(AuthenticationError):
            Client(pipe_address(client.base), family='AF_PIPE', authkey=b'wrong-fixture-key')
        assert client.call('get_status', {'compact': True})['config']['version'] == 1
        with Client(pipe_address(client.base), family='AF_PIPE', authkey=auth_key(client.base)) as conn:
            try:
                conn.send_bytes(b'x' * (MAX_REQUEST_BYTES + 1))
            except OSError:
                pass  # The receiver closes this oversized frame immediately.
        assert client.call('get_status', {'compact': True})['config']['version'] == 1
        with Client(pipe_address(client.base), family='AF_PIPE', authkey=auth_key(client.base)) as conn:
            conn.send_bytes(json.dumps({**req(), 'payload': []}).encode())
            response = json.loads(conn.recv_bytes())
        assert not response['ok'] and response['error'] == 'IPC_INVALID_COMMAND_OR_PAYLOAD'
        assert client.call('get_status', {'compact': True})['config']['version'] == 1
    finally:
        try:
            client.call('shutdown_worker')
        except Exception:
            pass
