from __future__ import annotations
import base64,ctypes,json,os,re,time,tempfile,threading,contextlib
from ctypes import wintypes
from pathlib import Path

class Blob(ctypes.Structure):
    _fields_=[('cbData',wintypes.DWORD),('pbData',ctypes.POINTER(ctypes.c_byte))]
def protect(data:bytes, decrypt=False):
    if os.name!='nt': raise RuntimeError('凭据保护需要Windows当前账户DPAPI')
    buf=ctypes.create_string_buffer(data); inp=Blob(len(data),ctypes.cast(buf,ctypes.POINTER(ctypes.c_byte))); out=Blob()
    crypt=ctypes.WinDLL('crypt32',use_last_error=True)
    crypt.CryptProtectData.argtypes=[ctypes.POINTER(Blob),wintypes.LPCWSTR,ctypes.POINTER(Blob),ctypes.c_void_p,ctypes.c_void_p,wintypes.DWORD,ctypes.POINTER(Blob)]
    crypt.CryptUnprotectData.argtypes=[ctypes.POINTER(Blob),ctypes.c_void_p,ctypes.POINTER(Blob),ctypes.c_void_p,ctypes.c_void_p,wintypes.DWORD,ctypes.POINTER(Blob)]
    crypt.CryptProtectData.restype=crypt.CryptUnprotectData.restype=wintypes.BOOL
    if decrypt: ok=crypt.CryptUnprotectData(ctypes.byref(inp),None,None,None,None,1,ctypes.byref(out))
    else: ok=crypt.CryptProtectData(ctypes.byref(inp),'MusicOps',None,None,None,1,ctypes.byref(out))
    if not ok: raise ctypes.WinError(ctypes.get_last_error())
    try: return ctypes.string_at(out.pbData,out.cbData)
    finally:
        kernel=ctypes.WinDLL('kernel32');kernel.LocalFree.argtypes=[ctypes.c_void_p];kernel.LocalFree.restype=ctypes.c_void_p
        kernel.LocalFree(out.pbData)

class Vault:
    _locks={};_locks_guard=threading.Lock()
    def __init__(self,base): self.path=Path(base)/'vault'/'secrets.dpapi'; self.path.parent.mkdir(parents=True,exist_ok=True)
    @contextlib.contextmanager
    def _locked(self):
        key=os.path.normcase(str(self.path.resolve()))
        with self._locks_guard:lock=self._locks.setdefault(key,threading.RLock())
        with lock:
            import msvcrt
            with self.path.with_suffix('.lock').open('a+b') as guard:
                if guard.seek(0,2)==0:guard.write(b'0');guard.flush()
                deadline=time.monotonic()+5
                while True:
                    try:guard.seek(0);msvcrt.locking(guard.fileno(),msvcrt.LK_NBLCK,1);break
                    except OSError:
                        if time.monotonic()>=deadline:raise RuntimeError('凭据库正由其他进程更新，请稍后重试') from None
                        time.sleep(.05)
                try:yield
                finally:guard.seek(0);msvcrt.locking(guard.fileno(),msvcrt.LK_UNLCK,1)
    def _load(self): return json.loads(protect(self.path.read_bytes(),True)) if self.path.exists() else {}
    def set(self,ref,value,host):
        if not isinstance(ref,str) or not re.fullmatch(r'vault://[A-Za-z0-9_.\-/]+',ref) or not host: raise ValueError('密钥引用和绑定域名必填')
        if not isinstance(value,str) or not value or len(value.encode('utf-8'))>1_000_000:raise ValueError('凭据须为非空文本，最大1MB')
        hosts=host if isinstance(host,list) else str(host).split(',')
        hosts=sorted({str(h).strip().lower() for h in hosts if str(h).strip()})
        if not hosts or any(not re.fullmatch(r'[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?',h) or '..' in h for h in hosts): raise ValueError('绑定只接受明确域名/IP白名单，不含协议/端口/路径')
        with self._locked():
            data=self._load(); old=data.get(ref,{})
            data[ref]={'value':value,'hosts':hosts,'revision':old.get('revision',0)+1}
            handle,name=tempfile.mkstemp(prefix='secrets-',suffix='.new',dir=self.path.parent)
            tmp=Path(name)
            try:
                with os.fdopen(handle,'wb') as stream:stream.write(protect(json.dumps(data,ensure_ascii=False).encode()));stream.flush();os.fsync(stream.fileno())
                deadline=time.monotonic()+3
                while True:
                    try:os.replace(tmp,self.path);break
                    except PermissionError:
                        # Windows readers/antivirus may briefly hold the old file;
                        # retain the complete replacement until that handle closes.
                        if time.monotonic()>=deadline:raise
                        time.sleep(.05)
            finally:tmp.unlink(missing_ok=True)
        return {'ref':ref,'configured':True,'revision':data[ref]['revision']}
    def get(self,ref,host):
        record=self._load().get(ref)
        if not record: return None
        if host.lower() not in record.get('hosts',[record.get('host')]): raise RuntimeError('CREDENTIAL_HOST_MISMATCH')
        return record['value']
    def revision(self,ref): return self._load().get(ref,{}).get('revision',0)
    def list_refs(self): return [{'ref':r,'hosts':v.get('hosts',[v.get('host')]),'revision':v['revision'],'configured':True} for r,v in self._load().items()]

def import_cookies(text,host):
    host=host.lower()
    text=text.strip()
    # Cookie-Editor exports sometimes have a human-readable site label.
    if re.match(r'^[A-Za-z0-9_.-]+:\s*\n\s*\[',text): text=text.split(':',1)[1].strip()
    cookies=json.loads(text)
    if not isinstance(cookies,list): raise ValueError('Cookie-Editor JSON必须是数组')
    result=[]
    for c in cookies:
        if not isinstance(c,dict): raise ValueError('Cookie项必须为对象')
        domain=c.get('domain','').lstrip('.').lower()
        if not domain or not (host==domain or host.endswith('.'+domain)): continue
        if c.get('hostOnly') is True and host!=domain:continue
        if not isinstance(c.get('name'),str) or not re.fullmatch(r"[!#$%&'*+.^_`|~0-9A-Za-z-]+",c['name']) or not isinstance(c.get('value'),str): raise ValueError('Cookie字段不完整')
        if any(ch in c['value'] for ch in '\r\n;\x00'): raise ValueError('Cookie值包含不允许的字符')
        if not str(c.get('path') or '/').startswith('/'): raise ValueError('Cookie路径无效')
        for key in ('expirationDate','expires'):
            if c.get(key) is not None and (type(c[key]) not in (int,float) or not __import__('math').isfinite(c[key])): raise ValueError('Cookie过期时间无效')
        result.append({k:c.get(k) for k in ('name','value','domain','path','secure','expirationDate','expires','httpOnly','sameSite','hostOnly')})
    if not result: raise ValueError('没有属于指定域名的Cookie')
    return result

def cookie_header(text,host,path='/',now=None):
    """Select cookies for one HTTPS request, never export the stored jar."""
    cookies=import_cookies(text,host)
    now=time.time() if now is None else now
    selected=[]
    for c in cookies:
        expiry=c.get('expirationDate') or c.get('expires') or 0
        cookie_path=c.get('path') or '/'
        if expiry>0 and expiry<=now: continue
        if path!=cookie_path and not (path.startswith(cookie_path) and (cookie_path.endswith('/') or path[len(cookie_path):].startswith('/'))): continue
        selected.append((cookie_path,c['name']+'='+c['value']))
    selected.sort(key=lambda x:len(x[0]),reverse=True)
    if not selected: raise ValueError('没有可用Cookie；请重新导出登录态')
    return '; '.join(pair for _,pair in selected)

def read_lyrics_auth_file(path):
    """Read one explicit raw/JSON/compose LrcApi credential without logging it."""
    path=Path(path)
    if path.stat().st_size>65536:raise ValueError('鉴权文件超过64KB')
    text=path.read_text('utf-8-sig')
    if path.suffix.lower() in ('.yaml','.yml'):
        values=re.findall(r'^\s*(?:-\s*)?API_AUTH\s*[:=]\s*(.*?)\s*$',text,re.M)
        if len(values)!=1:raise ValueError('部署配置须有唯一API_AUTH值')
        value=values[0]
        if value.startswith('"'):
            try:value=json.loads(value)
            except ValueError:raise ValueError('API_AUTH引号或JSON转义无效') from None
        elif value.startswith("'"):
            if not value.endswith("'"):raise ValueError('API_AUTH引号不完整')
            value=value[1:-1].replace("''", "'")
        else:value=value.split(' #',1)[0].rstrip()
        if value.startswith('$'):raise ValueError('文件只引用环境变量，没有可导入的鉴权值')
    elif path.suffix.lower()=='.json':
        data=json.loads(text)
        if isinstance(data,str):value=data
        elif isinstance(data,dict):
            values=[v for k,v in data.items() if k.lower() in ('api_auth','authorization','authentication','auth')]
            if len(values)!=1:raise ValueError('JSON须有唯一明确的歌词鉴权字段')
            value=values[0]
        else:raise ValueError('歌词鉴权JSON结构不符')
    else:
        value=text.rstrip('\r\n')
        if value.startswith('Authorization:'):value=value[len('Authorization:'):].lstrip()
    if not isinstance(value,str) or not value or any(c in value for c in '\r\n\x00'):raise ValueError('歌词鉴权必须为非空单行原始值')
    return value
