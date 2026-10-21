"""Private, owned loopback engines. Credentials are kept in the MusicOps vault."""
import atexit
from contextlib import ExitStack
import base64
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import threading
import time
from urllib.parse import urlsplit

import httpx
from musicops.clients.http import HttpError
from musicops.platforms import private_directory

SOURCES = {'netease', 'qq', 'qq_wx', 'kuwo', 'kugou', 'migu', 'soda', 'bilibili', 'apple', 'spotify', 'youtube'}
COOKIE_HOSTS={'netease':'music.163.com','qq':'y.qq.com','kuwo':'www.kuwo.cn','kugou':'www.kugou.com',
    'migu':'music.migu.cn','soda':'api.qishui.com','bilibili':'www.bilibili.com','apple':'music.apple.com',
    'spotify':'open.spotify.com','youtube':'music.youtube.com'}


def engine_root():
    if os.environ.get('MUSICOPS_ENGINE_ROOT'):return Path(os.environ['MUSICOPS_ENGINE_ROOT'])
    if getattr(sys, 'frozen', False):
        root = Path(sys.executable).resolve().parent
        if sys.platform == 'darwin':
            root = root.parent / 'Resources'
        return root / 'tools' / 'services'
    return Path(__file__).resolve().parents[3] / 'work' / 'engines'


class ManagedServices:
    def __init__(self, profile, vault, checkpoint=lambda _: None, root=None, diagnostic_output=None):
        self.profile = Path(profile)
        self.vault = vault
        self.checkpoint = checkpoint
        self.root = Path(root) if root else engine_root()
        self.diagnostic_output = diagnostic_output
        self.start_locks = {name:threading.RLock() for name in ('go-music-dl','flaresolverr')}
        self.running = {}
        self.owners = {}
        self.sessions = {}
        self.default_tools = lambda: {}
        self.default_storage = lambda: ''
        self.tool_config = {}
        atexit.register(self.close)

    def close(self):
        # Same-engine starts are serialized; unrelated engines can start in
        # parallel. Closing takes both locks in a fixed order.
        with ExitStack() as locks:
            for lock in self.start_locks.values():locks.enter_context(lock)
            for owner in self.owners.values(): owner.close()
            self.owners.clear()
            self.running.clear()

    def ensure(self, name, tools=None, *, restart_if_changed=False):
        if name not in ('go-music-dl', 'flaresolverr'):
            raise ValueError('未知内置服务')
        with self.start_locks[name]:
            result=self._ensure_locked(name,tools,restart_if_changed=restart_if_changed)
        # Pausing a music task must never retain the service startup lock.
        # Wait here before the caller can begin a download or other request.
        self.checkpoint('managed_service_ready')
        return result

    def _ensure_locked(self,name,tools,*,restart_if_changed):
        tools=dict(tools if tools is not None else self.default_tools())
        previous = self.running.get(name)
        if previous and previous[0].poll() is None:
            if name!='go-music-dl' or not restart_if_changed or self.tool_config.get(name)==tools:
                return previous[1], previous[2]
            # The serial workflow calls this between requests. Login UI
            # calls never restart an engine underneath an active download.
            self.owners.pop(name).close()
            self.running.pop(name,None)
        binary = self.root / name / (name + ('.exe' if os.name == 'nt' else ''))
        if not binary.is_file():
            raise HttpError('NOT_CONFIGURED', '此程序包缺少内置服务，请安装完整版本：' + name)
        directory = self.profile / 'services' / name
        private_directory(directory)
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        token = secrets.token_hex(32)
        environment = os.environ.copy()
        for key in ('PROXY_URL', 'PROXY_USERNAME', 'PROXY_PASSWORD', 'LOG_FILE'):
            environment.pop(key, None)
        environment.update(MUSICOPS_SERVICE_TOKEN=token, HOST='127.0.0.1', PORT=str(port),
            LOG_LEVEL='debug' if self.diagnostic_output else 'error', LOG_HTML='false', HEADLESS='true', MUSICOPS_ENGINE_DATA=str(directory),
            MUSIC_DL_CONFIG_DB=str(directory / 'settings.db'), MUSIC_DL_COOKIE_FILE=str(directory / 'unused.json'))
        storage=self.default_storage()
        temporary=(Path(storage)/'engine-temp'/name) if storage else directory/'temp'
        from musicops.library.paths import reject_reparse_chain
        reject_reparse_chain(temporary);private_directory(temporary)
        # go-music-dl invokes FFmpeg with temporary input/output audio. Its
        # os.CreateTemp must inherit the selected library volume as well.
        environment.update(TEMP=str(temporary),TMP=str(temporary),TMPDIR=str(temporary))
        for key, value in (tools or {}).items():
            if key in ('ffmpeg', 'ffprobe') and value:
                environment['MUSIC_DL_' + key.upper()] = str(value)
        old_owner=self.owners.pop(name,None)
        if old_owner:old_owner.close()
        command = [str(binary)]
        if name == 'go-music-dl':
            command += ['web', '--desktop', '--no-browser', '--port', str(port)]
        process = subprocess.Popen(command, cwd=directory, env=environment, stdin=subprocess.DEVNULL,
            stdout=self.diagnostic_output if self.diagnostic_output else subprocess.DEVNULL,
            stderr=subprocess.STDOUT if self.diagnostic_output else subprocess.DEVNULL,
            creationflags=subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0, start_new_session=os.name!='nt')
        from musicops.performance import configure_child
        configure_child(process)
        from musicops.clients.process_owner import ProcessOwner
        self.owners[name]=ProcessOwner(process)
        base = f'http://127.0.0.1:{port}' + ('/music' if name == 'go-music-dl' else '')
        self.running[name] = (process, base, token)
        self.tool_config[name]=tools
        try:
            with httpx.Client(trust_env=False, follow_redirects=False, timeout=2) as client:
                # A first Chromium launch can exceed a minute on efficiency
                # cores while Windows verifies the freshly installed binaries.
                deadline = time.monotonic() + (120 if name=='flaresolverr' else 60)
                while time.monotonic() < deadline:
                    self.checkpoint('managed_service_start')
                    if process.poll() is not None:
                        raise HttpError('SERVICE_START_FAILED', '内置服务启动失败：' + name)
                    try:
                        response = client.get(base + ('/healthz' if name == 'go-music-dl' else '/health'), headers={'X-MusicOps-Service': token})
                        if response.status_code == 200:
                            if name == 'go-music-dl':
                                cookies = self._cookies()
                                if cookies:
                                    response = client.post(base + '/cookies', json=cookies, headers={'X-MusicOps-Service': token})
                                    response.raise_for_status()
                            return base, token
                    except httpx.HTTPError: pass
                    time.sleep(.2)
            raise HttpError('SERVICE_START_FAILED', '内置服务启动超时：' + name)
        except BaseException:
            self.owners.pop(name).close()
            self.running.pop(name, None)
            raise

    def _cookies(self):
        result = {}
        for source in SOURCES - {'qq_wx'}:
            try:
                value = self.vault.get('vault://engine/' + source, '127.0.0.1')
                if value: result[source] = value
            except KeyError: pass
        return result

    def request(self, method, path, **kwargs):
        base, token = self.ensure('go-music-dl')
        with httpx.Client(trust_env=False, follow_redirects=False, timeout=45) as client:
            response = client.request(method, base + path, headers={'X-MusicOps-Service': token}, **kwargs)
            if response.status_code != 200 or len(response.content) > 2_000_000:
                raise HttpError('SERVICE_REQUEST_FAILED', '平台服务未完成操作，请重试或改用 Cookie 登录')
            return response.json()

    def save_cookie(self, source, value):
        if source not in SOURCES or source == 'qq_wx' or not isinstance(value, str) or not value.strip() or len(value) > 1_000_000:
            raise ValueError('平台 Cookie 无效')
        value = value.strip()
        if value.startswith('['):
            from musicops.clients.vault import import_cookies
            rows = import_cookies(value,COOKIE_HOSTS[source])
            value = '; '.join(str(row['name']) + '=' + str(row['value']) for row in rows)
        if '\r' in value or '\n' in value: raise ValueError('Cookie 不应包含换行')
        self.request('POST', '/cookies', json={source: value})
        self.vault.set('vault://engine/' + source, value, '127.0.0.1')
        return {'saved': True, 'message': '平台 Cookie 已加密保存'}

    def login(self, action, source, session_id=None):
        if source not in SOURCES: raise ValueError('未知登录平台')
        if action == 'create':
            session = self.request('POST', '/qr_login/' + source)
            key = session.get('key')
            if not isinstance(key, str) or not key or len(key) > 10000: raise HttpError('INVALID_PAYLOAD', '扫码服务未返回有效会话')
            identifier = secrets.token_hex(16)
            self.sessions[identifier] = (source, key, time.monotonic())
            url = session.get('url') or session.get('URL')
            image = session.get('image_url') or session.get('imageURL')
            if isinstance(image,str) and image.startswith('https://open.weixin.qq.com/connect/qrcode/'):
                parsed=urlsplit(image)
                if parsed.hostname!='open.weixin.qq.com' or parsed.username or parsed.password:raise ValueError('二维码地址无效')
                with httpx.Client(trust_env=False,follow_redirects=False,timeout=15) as client:
                    with client.stream('GET',image) as response:
                        if response.status_code!=200:raise HttpError('SERVICE_REQUEST_FAILED','微信二维码读取失败')
                        data=bytearray()
                        for block in response.iter_bytes():
                            if len(data)+len(block)>2_000_000:raise HttpError('INVALID_PAYLOAD','二维码图像过大')
                            data.extend(block)
                image='data:image/png;base64,'+base64.b64encode(data).decode('ascii')
            if not url and not (isinstance(image, str) and image.startswith('data:image/')):
                raise HttpError('INVALID_PAYLOAD', '此平台未提供可内嵌的二维码，请使用 Cookie 登录')
            return {'session_id': identifier, 'url': url, 'image': image if isinstance(image, str) and image.startswith('data:image/') else None}
        stored = self.sessions.get(session_id)
        if not stored or stored[0] != source or time.monotonic() - stored[2] > 300:
            raise ValueError('二维码已过期，请刷新')
        result = self.request('GET', '/qr_login/' + source, params={'key': stored[1]})
        cookie = result.get('cookie')
        if not cookie and isinstance(result.get('cookies'), dict):
            cookie = '; '.join(k + '=' + v for k, v in result['cookies'].items())
        if cookie and str(result.get('status')).lower() in ('success', 'authorized'):
            self.save_cookie('qq' if source == 'qq_wx' else source, cookie)
            self.sessions.pop(session_id, None)
            return {'saved': True, 'status': 'success', 'message': '登录成功，Cookie 已加密保存'}
        return {'saved': False, 'status': str(result.get('status', 'waiting'))[:80], 'message': '等待扫码确认；若平台要求额外验证，可使用 Cookie 登录'}


class ManagedSolver:
    def __init__(self, manager, settings):
        self.manager, self.settings = manager, settings

    def request(self, payload):
        from musicops.clients.browser_solver import FlareSolverrClient
        base, token = self.manager.ensure('flaresolverr')
        return FlareSolverrClient({**self.settings, 'base_url': base, '_managed_token': token}, checkpoint=self.manager.checkpoint).request(payload)
