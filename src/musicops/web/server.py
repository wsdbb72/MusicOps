from __future__ import annotations
import argparse
import base64
import hmac
import io
import json
import mimetypes
import secrets
import threading
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit
from musicops.worker.client import WorkerClient
from musicops.clients.http import redact
from musicops.providers.playlist_files import MAX_BYTES as PLAYLIST_MAX_BYTES

UPLOAD_LIMITS={'playlist':PLAYLIST_MAX_BYTES,'cover':10_000_000}

COMMANDS = frozenset(('attach get_status get_task list_workflows get_workflow '
    'get_playlist_targets refresh_playlist start_workflow manual_album_search list_library_albums '
    'index_library list_library_files bulk_workflows review_workflow queue_ai_reviews '
    'pause_pipeline resume_pipeline cancel_workflow save_setup engine_control test_connection '
    'test_browser_solver test_qbt_connection scan_duplicates merge_duplicates restore_duplicates '
    'duplicate_history compilation_albums preview_compilation merge_compilation restore_workflow '
    'assist_workflows normalize_qbt_tags').split())


class Server(ThreadingHTTPServer):
    daemon_threads=True
    request_queue_size=32

    def __init__(self,address,base,password):
        self.client=WorkerClient(base,timeout=90)
        self.password=password
        self.csrf=secrets.token_urlsafe(32)
        self.slots=threading.BoundedSemaphore(16)
        self.upload_slots=threading.BoundedSemaphore(2)
        super().__init__(address,Handler)


class Handler(BaseHTTPRequestHandler):
    server_version='MusicOps'

    def log_message(self,*_):pass  # URLs and credential payloads never enter access logs.

    def setup(self):
        super().setup();self.connection.settimeout(100)

    def respond(self,status,data,kind='application/json; charset=utf-8'):
        if not isinstance(data,bytes):data=json.dumps(data,ensure_ascii=False,allow_nan=False).encode()
        self.send_response(status)
        self.send_header('Content-Type',kind)
        self.send_header('Content-Length',str(len(data)))
        self.send_header('Cache-Control','no-store')
        self.send_header('X-Content-Type-Options','nosniff')
        self.send_header('Referrer-Policy','no-referrer')
        self.send_header('Content-Security-Policy',"default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
        if status==401:self.send_header('WWW-Authenticate','Basic realm="MusicOps", charset="UTF-8"')
        self.end_headers();self.wfile.write(data)

    def authenticated(self):
        try:
            value=self.headers.get('Authorization','')
            if not value.startswith('Basic '):return False
            raw=base64.b64decode(value[6:],validate=True).decode('utf-8')
            return hmac.compare_digest(raw.encode(),('musicops:'+self.server.password).encode())
        except (ValueError,UnicodeError):return False

    def do_GET(self):
        path=urlsplit(self.path).path
        if path=='/healthz':
            return self.respond(200,{'http':'ready'})
        if not self.authenticated():return self.respond(401,{'error':'请使用 musicops 用户及 /config/web-password 中的密码登录'})
        if path=='/api/bootstrap':
            return self.respond(200,{'csrf':self.server.csrf,'commands':sorted(COMMANDS)})
        if path=='/api/startup':
            try:status=json.loads((self.server.client.base/'state'/'worker-startup.json').read_text('utf-8'))
            except (OSError,ValueError):status={'message':'后台正在准备，请稍候'}
            return self.respond(200,{'message':status.get('message'),'failed':bool(status.get('failed'))})
        if path=='/brand.png':return self.respond(200,(Path(__file__).parents[1]/'assets/musicops.png').read_bytes(),'image/png')
        if path=='/favicon.ico':return self.respond(200,(Path(__file__).parents[1]/'assets/musicops.ico').read_bytes(),'image/x-icon')
        assets={'/':'index.html','/app.js':'app.js','/style.css':'style.css'}
        if path in assets:
            p=Path(__file__).parent/'static'/assets[path]
            return self.respond(200,p.read_bytes(),mimetypes.guess_type(str(p))[0]+'; charset=utf-8')
        return self.respond(404,{'error':'页面不存在'})

    def do_POST(self):
        endpoint=urlsplit(self.path).path
        try:size=int(self.headers.get('Content-Length','0'))
        except ValueError:return self.respond(400,{'error':'长度无效'})
        limit=30_000_000 if endpoint=='/api/upload' else 1_000_000
        if not 0<size<=limit:return self.respond(413,{'error':'请求过大，请分批处理'})
        slots=self.server.upload_slots if endpoint=='/api/upload' else self.server.slots
        if not slots.acquire(blocking=False):return self.respond(429,{'error':'请求繁忙，请稍后重试'})
        try:
            # Consume the bounded body before rejecting a request. Closing a
            # socket with unread POST bytes can reset Windows connections and
            # hide the actual 401/403 response from the browser.
            raw=self.rfile.read(size)
            if not self.authenticated():return self.respond(401,{'error':'请登录'})
            origin=self.headers.get('Origin')
            if origin and urlsplit(origin).netloc!=self.headers.get('Host'):
                return self.respond(403,{'error':'请求来源不匹配'})
            if not hmac.compare_digest(self.headers.get('X-MusicOps-CSRF',''),self.server.csrf):
                return self.respond(403,{'error':'会话已更新，请刷新页面','error_class':'CSRF_EXPIRED'})
            if endpoint not in ('/api/command','/api/upload'):return self.respond(404,{'error':'接口不存在'})
            if self.headers.get('Content-Type','').split(';')[0]!='application/json':return self.respond(415,{'error':'需要 JSON 请求'})
            body=json.loads(raw)
            if not isinstance(body,dict):raise ValueError('请求须为 JSON 对象')
            if endpoint=='/api/upload':
                kind=body.get('kind');suffix=Path(str(body.get('filename',''))).suffix.lower()
                allowed={'playlist':{'.json','.csv','.tsv','.txt','.xlsx','.m3u','.m3u8'},'cover':{'.jpg','.jpeg','.png','.webp'}}
                if suffix not in allowed.get(kind,set()):raise ValueError('文件类型不支持')
                content=base64.b64decode(body.get('content',''),validate=True)
                if not 0<len(content)<=UPLOAD_LIMITS[kind]:raise ValueError('歌单文件不能超过 20MB' if kind=='playlist' else '封面不能超过 10MB')
                if kind=='cover':
                    from musicops.media.resources import validate_cover
                    if not validate_cover(content,300)['valid']:raise ValueError('封面须为至少 300 像素的有效图片')
                directory=self.server.client.base/'work'/'web-uploads'
                directory.mkdir(parents=True,exist_ok=True,mode=0o700)
                file=directory/(uuid.uuid4().hex+suffix)
                with file.open('xb') as stream:stream.write(content)
                return self.respond(200,{'ok':True,'file_path':str(file)})
            command=body.get('command');payload=body.get('payload',{})
            if command not in COMMANDS or not isinstance(payload,dict):raise ValueError('未知操作或参数无效')
            request_id=body.get('request_id') or str(uuid.uuid4())
            if not isinstance(request_id,str) or not 1<=len(request_id)<=128:raise ValueError('请求标识无效')
            result=self.server.client.call(command,payload,request_id=request_id)
            if command=='engine_control' and payload.get('action')=='create' and isinstance(result,dict) and not result.get('image'):
                url=result.get('url')
                if isinstance(url,str) and 0<len(url)<=8192:
                    import qrcode
                    output=io.BytesIO();qrcode.make(url).save(output,format='PNG')
                    result={**result,'image':'data:image/png;base64,'+base64.b64encode(output.getvalue()).decode('ascii')}
            self.respond(200,{'ok':True,'result':result})
        except Exception as exc:
            self.respond(400,{'ok':False,'error':redact(str(exc)),'error_class':getattr(exc,'error_class',type(exc).__name__)})
        finally:slots.release()


def main(argv=None):
    parser=argparse.ArgumentParser()
    parser.add_argument('--base',required=True);parser.add_argument('--host',default='0.0.0.0');parser.add_argument('--port',type=int,default=8080)
    parser.add_argument('--password-file',required=True);args=parser.parse_args(argv)
    password=Path(args.password_file).read_text('utf-8').rstrip('\r\n')
    if not 12<=len(password)<=256 or any(c in password for c in '\r\n\0'):raise ValueError('网页登录密码须为 12–256 个字符')
    server=Server((args.host,args.port),args.base,password)
    server.serve_forever(poll_interval=.5)


if __name__=='__main__':main()
