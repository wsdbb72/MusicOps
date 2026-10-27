import os
os.environ.setdefault('QT_QPA_PLATFORM','offscreen')
import pytest
from PySide6.QtWidgets import QApplication
from musicops import __version__
from musicops.gui.workbench import Workbench
from musicops.worker.client import WorkerClient,WorkerConnectionError,WorkerUnavailableError
from test_release0417_web_performance import web  # noqa: F401  (fixture)
from test_workbench import Client,wait


@pytest.fixture(scope='module')
def app():return QApplication.instance() or QApplication([])


def test_missing_worker_pipe_is_a_connection_error_not_a_raw_file_error(tmp_path):
    # No worker is listening: Windows reports [WinError 2], POSIX ENOENT.
    with pytest.raises(WorkerConnectionError) as error:WorkerClient(tmp_path/'profile',timeout=2).call('get_status',{})
    assert 'IPC_CONNECTION_ERROR' in str(error.value) and 'WinError' not in str(error.value)
    # Startup/shutdown loops in the smoke scripts and container entrypoint wait on OSError.
    assert isinstance(error.value,WorkerUnavailableError) and isinstance(error.value,OSError)


def test_lost_worker_during_background_refresh_reconnects_instead_of_showing_winerror(app,tmp_path):
    class StoppedWorker(Client):
        running=False;attached=0
        def call(self,command,payload):
            if not self.running:raise FileNotFoundError(2,'系统找不到指定的文件。')
            return super().call(command,payload)
        def ensure_running(self):
            self.attached+=1;self.running=True;return {'attached':True}
    client=StoppedWorker(tmp_path);window=Workbench(tmp_path,client,autostart=False)
    window.load_subscriptions();wait(app,lambda:client.attached and not window.requests)
    assert '需要处理' not in window.notice.text() and 'WinError' not in window.notice.text() and '系统找不到' not in window.notice.text()
    assert client.attached==1 and any(c=='get_status' for c,_ in client.calls)
    window.close()


def test_user_action_during_outage_asks_to_retry_after_reconnect(app,tmp_path):
    class StoppedWorker(Client):
        def call(self,command,payload):raise FileNotFoundError(2,'系统找不到指定的文件。')
        def ensure_running(self):raise OSError('still starting')
    window=Workbench(tmp_path,StoppedWorker(tmp_path),autostart=False)
    window.call('save_subscription',{});wait(app,lambda:not window.requests)
    assert '重新连接' in window.notice.text() and '重试' in window.notice.text() and '需要处理' not in window.notice.text()
    assert window._reattaching is False
    window.close()


def test_web_ui_shows_running_version(web):
    client,_=web;auth=('musicops','synthetic-web-password')
    assert client.get('/api/bootstrap',auth=auth).json()['version']==__version__
    assert 'id="app-version"' in client.get('/',auth=auth).text
    assert 'boot.version' in client.get('/app.js',auth=auth).text
