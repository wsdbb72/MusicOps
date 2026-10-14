import copy
import os
import time
from pathlib import Path

os.environ.setdefault('QT_QPA_PLATFORM', 'offscreen')
import pytest
from PySide6.QtWidgets import QApplication, QPushButton

from musicops import __version__
from musicops.config.schema import default_config
from musicops.gui.main import MainWindow, PAGE_NAMES


@pytest.fixture(scope='module')
def app():
    return QApplication.instance() or QApplication([])


def drain(app, window):
    end = time.monotonic() + 5
    while time.monotonic() < end:
        app.processEvents()
        if not window.pending:
            return
        time.sleep(.005)
    assert not window.pending


class OnlineClient:
    def __init__(self, base):
        self.calls = []
        values = default_config(base)
        values['catalog'] = {'musicbrainz': {'enabled': True}, 'discogs': {'enabled': True, 'credential_ref': ''}}
        self.config = {'version': 1, 'values': values}
        self.candidate = {'candidate_id': 'musicbrainz:release:fixture', 'source': 'musicbrainz', 'title': 'Song', 'artists': ['Artist'], 'album': 'Album', 'duration': 200, 'requires_manual_review': True}
        self.library_files = [{'file_id': 'file-'+str(index), 'raw_title': 'Track '+str(index), 'artists': ['Artist'], 'album': 'Album', 'path': str(base / ('file-'+str(index)+'.flac'))} for index in range(421)]

    def ensure_running(self):
        pass

    def call(self, command, payload):
        self.calls.append((command, copy.deepcopy(payload)))
        if command in ('attach', 'get_status'):
            return {'config': copy.deepcopy(self.config), 'tasks': []}
        if command in ('enable_online', 'enable_demo'):
            self.config['values']['offline'] = command == 'enable_demo'
            self.config['version'] += 1
            return {'config': copy.deepcopy(self.config)}
        if command == 'identify_metadata':
            return {'candidates': [copy.deepcopy(self.candidate)], 'sources': [{'source': 'musicbrainz', 'status': 'OK'}, {'source': 'discogs', 'status': 'AUTH_REQUIRED'}], 'lookup_links': [{'source': 'MusicBrainz', 'url': 'https://musicbrainz.org/search?query=Song&type=recording'}, {'source': 'Discogs', 'url': 'https://www.discogs.com/search/?q=Song&type=all'}, {'source': 'AllMusic', 'url': 'https://attacker.invalid/redirect'}]}
        if command == 'resolve_metadata':
            return {'candidate': {**self.candidate, 'resolved': True, 'release_id': 'fixture-release'}}
        if command == 'accept_metadata':
            if payload['target_id'].startswith('file-'):
                row = next(row for row in self.library_files if row['file_id'] == payload['target_id'])
                return {'status': 'OK', 'file': row, 'metadata_only': True, 'proposal': {'album': 'Album'}}
            return {'status': 'OK', 'target': {'target_id': payload['target_id'], 'album': 'Album'}}
        if command == 'get_playlist_targets':
            return {'targets': [{'target_id': 'netease:fixture', 'raw_title': 'Song', 'artists': ['Artist'], 'album': 'Album'}]}
        if command == 'list_library_files':
            query = payload['query'].casefold()
            rows = [row for row in self.library_files if not query or query in str(row).casefold()]
            return {'files': rows[payload['offset']:payload['offset']+payload['limit']], 'total': len(rows), 'offset': payload['offset'], 'limit': payload['limit']}
        if command == 'roon_preflight':
            return {'status': 'OK', 'connected': True, 'core_info': {'display_name': 'Fixture Core'}, 'storage_diagnostics': {'status': 'NOT_CONFIRMED', 'reason': '用户需核对Storage'}}
        if command == 'validate_selected_file':
            row = copy.deepcopy(next(row for row in self.library_files if row['file_id'] == payload['file_id']))
            row['validation'] = {'state': 'VALID_PAYLOAD', 'decode_complete': True}
            return {'file': row, 'validation': row['validation'], 'music_writes': 0}
        return {}


def window_for(app, base):
    client = OnlineClient(base)
    window = MainWindow(base, client, autostart=False)
    window._status(client.call('get_status', {}))
    return window, client


def test_formal_startup_enables_legacy_offline_profile_and_shows_path_version(app, tmp_path):
    client = OnlineClient(tmp_path)
    assert client.config['values']['offline']
    window = MainWindow(tmp_path, client, autostart=True, online_mode=True)
    drain(app, window)
    end = time.monotonic() + 5
    while not window.config_values and time.monotonic() < end:
        app.processEvents();time.sleep(.005)
    drain(app, window)
    assert not client.config['values']['offline']
    assert '正式联网模式' in window.banner.text()
    assert __version__ in window.banner.text()
    assert window.profile_label.text().endswith(str(tmp_path.resolve()))
    assert all(command in ('attach', 'get_status', 'enable_online') for command, _ in client.calls)
    assert not window.enable_online_button.isEnabled()
    window.close()


def test_selected_playlist_query_keeps_target_identity_and_requires_detail_before_acceptance(app, tmp_path):
    window, client = window_for(app, tmp_path)
    target = {'target_id': 'netease:fixture', 'raw_title': 'Song', 'artists': ['Artist', 'Guest'], 'album': '', 'duration': 200}
    window.show_rows('歌单与补齐', [target]);window.tables['歌单与补齐'].selectRow(0)
    window.findChild(QPushButton, 'playlist_identify').click();drain(app, window)
    assert window.nav.currentItem().text() == '在线识别'
    query = next(payload for command, payload in client.calls if command == 'identify_metadata')
    assert query == {'title': 'Song', 'artists': ['Artist', 'Guest'], 'album': '', 'duration': 200.0, 'target_id': 'netease:fixture'}
    window.tables['在线识别'].selectRow(0)
    window.findChild(QPushButton, 'accept_metadata').click()
    assert not any(command == 'accept_metadata' for command, _ in client.calls)
    window.findChild(QPushButton, 'resolve_metadata').click();drain(app, window)
    assert 'musicbrainz:release:fixture' in window.catalog_resolved
    window.findChild(QPushButton, 'accept_metadata').click();drain(app, window)
    assert next(payload for command, payload in client.calls if command == 'accept_metadata') == {'candidate_id': 'musicbrainz:release:fixture', 'target_id': 'netease:fixture'}
    assert any(command == 'get_playlist_targets' for command, _ in client.calls)
    assert '音乐文件保持原样' in window.result_summary['在线识别'].text()
    window.close()


def test_catalog_links_allow_only_official_https_sources_and_do_not_open_automatically(app, tmp_path, monkeypatch):
    window, client = window_for(app, tmp_path)
    import musicops.gui.main as gui
    opened = []
    monkeypatch.setattr(gui.QDesktopServices, 'openUrl', lambda url: opened.append(url.toString()))
    window.catalog_title.setText('Song');window.findChild(QPushButton, 'identify_metadata').click();drain(app, window)
    assert not opened
    assert window.catalog_link_buttons['musicbrainz'].isEnabled()
    assert window.catalog_link_buttons['discogs'].isEnabled()
    assert not window.catalog_link_buttons['allmusic'].isEnabled()
    window.catalog_link_buttons['musicbrainz'].click()
    assert opened == ['https://musicbrainz.org/search?query=Song&type=recording']
    for url in ['http://musicbrainz.org/', 'file:///C:/private.txt', 'https://musicbrainz.org.evil.invalid/', 'https://musicbrainz.org@evil.invalid/', 'https://musicbrainz.org:8443/']:
        assert not window._catalog_url_allowed(url)
    window.close()


def test_independent_query_cannot_change_target_playlist_and_demo_uses_separate_profile(app, tmp_path, monkeypatch):
    window, client = window_for(app, tmp_path)
    client.config['values']['offline'] = False;window._status(client.call('get_status', {}))
    import musicops.gui.main as gui
    processes = []
    monkeypatch.setattr(gui.subprocess, 'Popen', lambda command, **kwargs: processes.append((command, kwargs)))
    window.findChild(QPushButton, 'demo').click()
    command, kwargs = processes[0]
    assert command[-3:] == ['--base', str(tmp_path / 'demo-profile'), '--demo']
    assert not any(name in ('demo', 'enable_demo', 'update_config') for name, _ in client.calls)
    assert not client.config['values']['offline']
    window.catalog_title.setText('Song');window.findChild(QPushButton, 'identify_metadata').click();drain(app, window)
    window.tables['在线识别'].selectRow(0);window.findChild(QPushButton, 'resolve_metadata').click();drain(app, window)
    window.findChild(QPushButton, 'accept_metadata').click()
    assert not any(name == 'accept_metadata' for name, _ in client.calls)
    window.close()


def test_discogs_token_entry_masks_clears_and_keeps_token_out_of_plain_config(app, tmp_path):
    window, client = window_for(app, tmp_path)
    index = window.secret_purpose.findData('discogs');window.secret_purpose.setCurrentIndex(index)
    assert window.secret_ref.text() == 'vault://catalog/discogs/token'
    assert window.secret_host.text() == 'api.discogs.com'
    window.secret_value.setText('virtual-test-only-token');window.findChild(QPushButton, 'set_secret').click();drain(app, window)
    payload = next(payload for command, payload in client.calls if command == 'set_secret')
    assert payload['value'] == 'virtual-test-only-token' and payload['host'] == 'api.discogs.com'
    assert not window.secret_value.text()
    assert window.editor.controls[('catalog', 'discogs', 'credential_ref')][0].text() == 'vault://catalog/discogs/token'
    assert 'virtual-test-only-token' not in str(window.editor.collect())
    window.close()


def test_library_starts_from_configured_root_and_indexing_is_lightweight(app, tmp_path):
    window, client = window_for(app, tmp_path)
    assert window.library_root.text() == client.config['values']['paths']['library']
    window.findChild(QPushButton, 'index_library').click();drain(app, window)
    assert next(payload for command, payload in client.calls if command == 'index_library')['lightweight'] is True
    assert PAGE_NAMES.index('在线识别') == window.pages.indexOf(window.pages.widget(PAGE_NAMES.index('在线识别')))
    window.close()


def test_demo_startup_enables_only_separate_demo_and_never_online(app, tmp_path):
    base = tmp_path / 'demo-profile'
    client = OnlineClient(base)
    client.config['values']['offline'] = False
    window = MainWindow(base, client, autostart=False, demo_profile=True, online_mode=False)
    window._attached({});drain(app, window)
    assert [command for command, _ in client.calls] == ['enable_demo', 'demo', 'get_status']
    assert client.config['values']['offline']
    assert '演示档案' in window.banner.text()
    window.close()


def test_old_catalog_response_cannot_replace_new_target_query(app, tmp_path):
    window, client = window_for(app, tmp_path)
    window._load_catalog_query({'target_id': 'old-target', 'title': 'Old', 'artists': ['Artist']})
    old_serial = window.catalog_query_serial
    window._load_catalog_query({'target_id': 'new-target', 'title': 'New', 'artists': ['Artist']})
    window._catalog_search_received(old_serial, {'candidates': [client.candidate]})
    assert window.tables['在线识别'].rowCount() == 0
    assert window.catalog_target_id == 'new-target'
    assert not window.catalog_resolved
    window.close()


def test_invalid_endpoint_during_secret_choice_has_no_ui_exception(app, tmp_path):
    window, client = window_for(app, tmp_path)
    window.editor.controls[('ai', 'base_url')][0].setText('https://[invalid')
    window.secret_purpose.setCurrentIndex(window.secret_purpose.findData('ai'))
    assert window.secret_host.text() == ''
    assert window.secret_value.text() == ''
    window.close()


def test_library_paging_search_can_reach_any_saved_file_and_status_does_not_replace_page(app, tmp_path):
    window, client = window_for(app, tmp_path)
    window.findChild(QPushButton, 'list_library_files').click();drain(app, window)
    assert window.tables['音乐库与音质'].rowCount() == 200
    assert window.library_total == 421 and not window.library_previous.isEnabled()
    window.library_next.click();drain(app, window)
    assert window.library_offset == 200
    window.library_next.click();drain(app, window)
    assert window.library_offset == 400 and window.tables['音乐库与音质'].rowCount() == 21
    assert not window.library_next.isEnabled() and window.library_previous.isEnabled()
    window.tables['音乐库与音质'].selectRow(20)
    assert window.selected('音乐库与音质')['file_id'] == 'file-420'
    window._status({'config': client.config, 'tasks': [], 'files': client.library_files[:200], 'library_file_count': 421})
    assert window.selected('音乐库与音质')['file_id'] == 'file-420'
    assert window.tables['音乐库与音质'].rowCount() == 21
    window.library_query.setText('Track 420');window.findChild(QPushButton, 'list_library_files').click();drain(app, window)
    assert window.library_offset == 0 and window.library_total == 1
    assert window.tables['音乐库与音质'].rowCount() == 1
    assert not window.library_next.isEnabled()
    assert '421' in window.library_count_label.text()
    window.close()


def test_roon_check_exposes_connection_and_keeps_storage_diagnostics_separate(app, tmp_path):
    window, client = window_for(app, tmp_path)
    window.findChild(QPushButton, 'roon_preflight').click();drain(app, window)
    assert ('roon_preflight', {}) in client.calls
    assert 'Core已连接' in window.roon_connection.text()
    assert 'Fixture Core' in window.roon_connection.text()
    assert '尚未确证' in window.roon_connection.text()
    assert 'storage_diagnostics' in window.details['日志与报告'].toPlainText()
    window.close()


def test_roon_log_status_never_claims_current_storage_configuration_verified(app, tmp_path):
    window, client = window_for(app, tmp_path)
    window._roon_checked({'status': 'AUTH_REQUIRED', 'connected': True, 'core_info': {'display_name': 'Fixture Core'}, 'storage_diagnostics': {'status': 'OK', 'storage_configuration_verified': False}})
    assert '等待Roon授权 / 配对' in window.roon_connection.text()
    assert '当前监听尚未确证' in window.roon_connection.text()
    window.close()


def test_library_identification_binds_file_id_and_confirmation_is_only_a_proposal(app, tmp_path):
    window, client = window_for(app, tmp_path)
    window.findChild(QPushButton, 'list_library_files').click();drain(app, window)
    window.tables['音乐库与音质'].selectRow(0);window.findChild(QPushButton, 'library_identify').click();drain(app, window)
    assert window.catalog_target_id == 'file-0'
    query = next(payload for command, payload in client.calls if command == 'identify_metadata')
    assert query['target_id'] == 'file-0'
    window.tables['在线识别'].selectRow(0);window.findChild(QPushButton, 'resolve_metadata').click();drain(app, window)
    window.findChild(QPushButton, 'accept_metadata').click();drain(app, window)
    assert '元数据提案' in window.result_summary['在线识别'].text()
    assert '原始标签保持原样' in window.result_summary['在线识别'].text()
    assert not any(command == 'get_playlist_targets' for command, _ in client.calls)
    window.close()


def test_full_validation_is_explicit_and_only_selected_file_is_requested(app, tmp_path):
    window, client = window_for(app, tmp_path)
    window.findChild(QPushButton, 'validate_selected_file').click()
    assert not any(command == 'validate_selected_file' for command, _ in client.calls)
    window.findChild(QPushButton, 'list_library_files').click();drain(app, window)
    window.tables['音乐库与音质'].selectRow(199)
    window.findChild(QPushButton, 'validate_selected_file').click();drain(app, window)
    assert next(payload for command, payload in client.calls if command == 'validate_selected_file') == {'file_id': 'file-199'}
    assert window.selected('音乐库与音质')['validation']['decode_complete']
    assert '音乐文件保持原样' in window.result_summary['音乐库与音质'].text()
    window.close()
