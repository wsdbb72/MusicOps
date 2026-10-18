from __future__ import annotations
import hashlib,json,os,subprocess,sys,time,uuid
from pathlib import Path
from multiprocessing.connection import Client
from musicops.clients.vault import protect
from musicops.worker.lock import WriterLock
from musicops.platforms import ipc_address,ipc_family,is_windows,is_macos,private_directory

MAX_REQUEST_BYTES=1_000_000
MAX_RESPONSE_BYTES=64_000_000

class WorkerRequestError(RuntimeError):
    """The worker replied; an application rejection is not a lost connection."""
    def __init__(self,message,error_class='IPC_ERROR',request_id=None):
        self.error_class=error_class;self.request_id=request_id;super().__init__(message)

class WorkerConnectionError(RuntimeError):
    error_class='IPC_CONNECTION_ERROR'

def pipe_address(base):return ipc_address(base)
def auth_key(base):
    path=Path(base)/'vault'/('ipc.keychain' if is_macos() else 'ipc.dpapi');private_directory(path.parent)
    if path.exists(): return protect(path.read_bytes(),True)
    deadline=time.monotonic()+5;lock=WriterLock(Path(base)/'ipc-init')
    while True:
        try:lock.acquire();break
        except RuntimeError:
            if path.exists():return protect(path.read_bytes(),True)
            if time.monotonic()>=deadline:raise WorkerConnectionError('IPC鉴权初始化正在进行，请重新附着后台')
            time.sleep(.05)
    try:
        if path.exists(): return protect(path.read_bytes(),True)
        key=os.urandom(32); temporary=path.with_suffix('.tmp');temporary.write_bytes(protect(key));os.replace(temporary,path);return key
    finally:lock.release()

class WorkerClient:
    def __init__(self,base,timeout=30): self.base=Path(base).resolve(); self.config_version=0;self.timeout=timeout
    def call(self,command,payload=None,request_id=None):
        if payload is not None and not isinstance(payload,dict):raise ValueError('IPC_PAYLOAD_MUST_BE_OBJECT')
        req=dict(schema_version=1,request_id=request_id or str(uuid.uuid4()),profile_id='default',config_version=self.config_version,command=command,payload=payload or {})
        encoded=json.dumps(req,ensure_ascii=False,allow_nan=False).encode('utf-8')
        if len(encoded)>MAX_REQUEST_BYTES:raise ValueError('IPC_REQUEST_TOO_LARGE：请减少请求内容后重试')
        try:conn=Client(pipe_address(self.base),family=ipc_family(),authkey=auth_key(self.base))
        except OSError as exc:
            if getattr(exc,'winerror',None) in (121,231):raise WorkerConnectionError('IPC_CONNECTION_ERROR：后台连接繁忙，请稍后重连；已有任务仍保留') from None
            raise
        with conn:
            conn.send_bytes(encoded)
            if not conn.poll(self.timeout):raise WorkerConnectionError('后台响应超时；请先读取任务/配置状态，勿盲重发执行请求')
            data=json.loads(conn.recv_bytes(MAX_RESPONSE_BYTES))
        if not isinstance(data,dict) or 'ok' not in data:raise WorkerConnectionError('IPC_INVALID_RESPONSE')
        if not data['ok']: raise WorkerRequestError(data.get('error','IPC_ERROR'),data.get('error_class','IPC_ERROR'),data.get('request_id'))
        result=data['result']
        if isinstance(result,dict) and 'config' in result: self.config_version=result['config']['version']
        return result
    def ensure_running(self):
        try: return self.call('attach',{'compact':True})
        except (OSError,EOFError): pass
        if getattr(sys,'frozen',False):
            executable=Path(sys.executable).parent/('MusicOps-worker.exe' if is_windows() else 'MusicOps-worker'); cmd=[str(executable),'--base',str(self.base)]
        else: cmd=[sys.executable,'-m','musicops.worker.server','--base',str(self.base)]
        private_directory(self.base)
        env=os.environ.copy(); env['PYTHONPATH']=str(Path(__file__).resolve().parents[2])
        subprocess.Popen(cmd,env=env,creationflags=0x08000000 if is_windows() else 0,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,cwd=self.base)
        for _ in range(100):
            time.sleep(.1)
            try: return self.call('attach',{'compact':True})
            except (OSError,EOFError): pass
        raise RuntimeError('后台worker无法启动；检查状态/版本/账户权限')
