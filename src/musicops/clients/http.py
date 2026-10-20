from __future__ import annotations
import json,re
from dataclasses import dataclass,field
from urllib.parse import urlparse

class PolicyBlocked(RuntimeError):
    error_class='POLICY_BLOCKED'
class HttpError(RuntimeError):
    def __init__(self,error_class,message=''): self.error_class=error_class; super().__init__(message or error_class)

@dataclass
class Response:
    status_code:int
    body:bytes=b''
    headers:dict=field(default_factory=dict)
    url:str=''
    @property
    def text(self): return self.body.decode('utf-8-sig',errors='strict')
    def json(self):
        def invalid_constant(value):raise ValueError('JSON包含无效非有限数值')
        return json.loads(self.text,parse_constant=invalid_constant)

def redact(data):
    if isinstance(data,dict):
        return {k:('[REDACTED]' if is_secret_key(k) else redact(v)) for k,v in data.items()}
    if isinstance(data,list): return [redact(v) for v in data]
    if isinstance(data,str):
        data=re.sub(r'https?://[^\s<>\"]+(?:passkey|authkey|token)[^\s<>\"]*','[SENSITIVE_URL]',data,flags=re.I)
        return data
    return data

def is_secret_key(key):
    name=re.sub(r'[^a-z0-9]','',str(key).lower())
    return (any(x in name for x in ('authorization','cookie','passkey','authkey','password','secretvalue'))
            or name in ('sid','apikey','apiauth','accesstoken','refreshtoken','clientsecret','secret','credential'))

class FakeTransport:
    is_fake=True
    def __init__(self,responses=()): self.responses=list(responses); self.requests=[]
    def request(self,method,url,params=None,headers=None,content=None,timeout=None,**kwargs):
        req=dict(method=method,url=url,params=params or {},headers=headers or {},content=content,**kwargs); self.requests.append(req)
        if not self.responses: raise HttpError('NETWORK_ERROR','fixture响应用尽')
        response=self.responses.pop(0)
        if isinstance(response,BaseException): raise response
        if callable(response): response=response(req)
        if isinstance(response,dict):
            response=Response(response.get('status_code',200),json.dumps(response.get('json',{})).encode(),response.get('headers',{}),url)
        if not response.url: response.url=url
        return response

class SafeHttpTransport:
    is_fake=False
    def __init__(self,allowed_hosts,offline=True):
        self.allowed_hosts={h.lower() for h in allowed_hosts if isinstance(h,str)}; self.offline=offline
        self._client=None; self._client_cls=None
    def _shared_client(self,httpx):
        if self._client is None or self._client_cls is not httpx.Client:
            if self._client is not None and hasattr(self._client,'close'):
                try:self._client.close()
                except Exception:pass
            self._client_cls=httpx.Client
            self._client=httpx.Client(verify=True,follow_redirects=False,timeout=15)
        return self._client
    def close(self):
        if self._client is not None and hasattr(self._client,'close'):
            try:self._client.close()
            except Exception:pass
        self._client=None
    def request(self,method,url,params=None,headers=None,content=None,timeout=None,**kwargs):
        if self.offline: raise PolicyBlocked('离线阶段禁止真实服务')
        parts=urlparse(url)
        if parts.hostname not in self.allowed_hosts or parts.username or parts.password: raise PolicyBlocked('请求域名不在白名单')
        if parts.scheme!='https': raise PolicyBlocked('明文请求需独立显式授权客户端')
        import httpx
        allow_redirect_response=kwargs.pop('allow_redirect_response',False)
        limit=kwargs.pop('max_response_bytes',20_000_000)
        if type(limit) is not int or not 1<=limit<=20_000_000:raise ValueError('响应大小上限无效')
        if isinstance(timeout,tuple) and len(timeout)==2: timeout=httpx.Timeout(timeout[1],connect=timeout[0])
        def _read_stream(client):
            with client.stream(method,url,params=params,headers=headers,content=content,timeout=timeout or 15,**kwargs) as r:
                if 300<=r.status_code<400 and not allow_redirect_response: raise PolicyBlocked('重定向需单独核对，不转发凭据')
                body=bytearray()
                for block in r.iter_bytes():
                    if len(body)+len(block)>limit:raise HttpError('INVALID_PAYLOAD','服务响应超过本机大小上限')
                    body.extend(block)
                return Response(r.status_code,bytes(body),dict(r.headers),str(r.url))
        try:
            if getattr(httpx.Client,'__module__','').startswith('httpx'):
                response=_read_stream(self._shared_client(httpx))
            else:
                with httpx.Client(verify=True,follow_redirects=False,timeout=timeout or 15) as client:
                    response=_read_stream(client)
        except httpx.TimeoutException as e: raise HttpError('TRANSIENT_ERROR','请求超时') from e
        except httpx.HTTPError as e: raise HttpError('NETWORK_ERROR','网络访问失败') from e
        return response
