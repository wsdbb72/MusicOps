"""Authenticated, origin-bound WebAPI session; no global preference mutations."""
import json
from urllib.parse import urlsplit
import httpx
from musicops.clients.http import Response, HttpError, PolicyBlocked, local_service_host
from musicops.clients.qbt import QbtClient

class QbtSession:
    authenticated_qbt = True
    is_fake = False
    def __init__(self, config, secret_getter):
        self.origin=config['base_url'].rstrip('/');u=urlsplit(self.origin)
        if u.scheme not in ('http','https') or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('','/'):
            raise PolicyBlocked('qBittorrent 地址需要为完整服务地址')
        if u.scheme=='http' and not local_service_host(u.hostname):
            raise PolicyBlocked('公网 qBittorrent 请使用 HTTPS；本机、局域网和 Docker 内网地址可使用 HTTP')
        raw=secret_getter(config['credential_ref'],u.hostname)
        if not raw:raise HttpError('AUTH_REQUIRED','请在连接向导填写 qBittorrent 登录信息')
        try:creds=json.loads(raw)
        except (ValueError,TypeError):raise HttpError('AUTH_FAILED','qBittorrent 登录资料格式无效，请在连接与设置中重新填写') from None
        if not isinstance(creds,dict) or any(not isinstance(creds.get(key),str) for key in ('username','password')):
            raise HttpError('AUTH_FAILED','qBittorrent 登录资料缺少用户名或密码，请在连接与设置中重新填写')
        self.client=httpx.Client(follow_redirects=False,verify=True,trust_env=False,timeout=15,headers={'Referer':self.origin+'/'})
        try:
            r=self.client.post(self.origin+'/api/v2/auth/login',data={'username':creds['username'],'password':creds['password']})
            if r.status_code not in (200,204) or r.status_code==200 and r.text.strip()!='Ok.':raise HttpError('AUTH_FAILED','qBittorrent 登录失败')
            self.version=self.request('GET',self.origin+'/api/v2/app/version').text.strip()
        except httpx.TimeoutException:
            self.client.close();raise HttpError('TRANSIENT_ERROR','连接 qBittorrent 超时，请检查 WebUI 地址与服务状态') from None
        except httpx.HTTPError:
            self.client.close();raise HttpError('NETWORK_ERROR','无法连接 qBittorrent，请确认程序正在运行并已启用 WebUI') from None
        except Exception:
            self.client.close();raise
    def request(self,method,url,**kwargs):
        if not url.startswith(self.origin+'/api/v2/'):raise PolicyBlocked('qBittorrent 请求来源改变')
        endpoint=url[len(self.origin+'/api/v2/'):]
        reads={'app/version','torrents/info','torrents/files','torrents/categories','torrents/tags','torrents/export'}
        writes={'torrents/add','torrents/start','torrents/resume','torrents/createCategory','torrents/createTags','torrents/addTags','torrents/removeTags','torrents/deleteTags','torrents/setLocation','torrents/setDownloadPath'}
        if (method=='GET' and endpoint not in reads) or (method=='POST' and endpoint not in writes) or method not in ('GET','POST'):raise PolicyBlocked('未开放该客户端操作')
        try:r=self.client.request(method,url,**kwargs)
        except httpx.HTTPError:raise HttpError('NETWORK_ERROR','qBittorrent 请求失败') from None
        if len(r.content)>20_000_000:raise HttpError('INVALID_PAYLOAD','qBittorrent 返回过大')
        return Response(r.status_code,r.content,dict(r.headers),str(r.url))
    def prepare(self,config,task_tag=None):
        base=self.origin+'/api/v2/'
        r=self.request('GET',base+'torrents/categories')
        if r.status_code!=200:raise HttpError('ACCESS_BLOCKED','无法读取下载分类')
        if config['category'] not in r.json():
            r=self.request('POST',base+'torrents/createCategory',data={'category':config['category'],'savePath':''})
            if r.status_code!=200:raise HttpError('ACCESS_BLOCKED','无法创建 MusicOps 下载分类')
        if task_tag:
            r=self.request('GET',base+'torrents/tags')
            if r.status_code!=200:raise HttpError('ACCESS_BLOCKED','无法读取下载任务标签')
            if task_tag not in r.json():
                r=self.request('POST',base+'torrents/createTags',data={'tags':task_tag})
                if r.status_code!=200:raise HttpError('ACCESS_BLOCKED','无法创建下载任务标签')
    def close(self):self.client.close()

def live_client(config,source_root,store,secret_getter):
    session=QbtSession(config,secret_getter)
    client=QbtClient(session,config['base_url'],source_root,version=session.version,store=store,category=config['category'],tag_prefix=config['tag_prefix'])
    if client.capabilities()['status']!='OK':
        session.close();raise PolicyBlocked('当前 qBittorrent 版本尚未适配，请使用 4.6–5.2.2')
    return client
