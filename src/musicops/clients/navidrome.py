"""Optional authenticated scan after publication."""
import hashlib,secrets
from urllib.parse import urlsplit
import httpx
from musicops.clients.http import PolicyBlocked,HttpError

def scan(config,secret_getter):
    if not config['enabled']:return {'state':'skipped','message':'未启用 Navidrome；Roon 由监听目录自动扫描'}
    u=urlsplit(config['url'])
    if u.scheme=='http' and u.hostname not in ('127.0.0.1','localhost','::1'):raise PolicyBlocked('远程 Navidrome 请使用 HTTPS')
    password=secret_getter(config['password_ref'],u.hostname)
    if not password:raise HttpError('AUTH_REQUIRED','请在设置中填写 Navidrome 密码')
    salt=secrets.token_hex(16);token=hashlib.md5((password+salt).encode()).hexdigest()
    try:
        with httpx.Client(verify=True,follow_redirects=False,timeout=15) as client:
            r=client.get(config['url'].rstrip('/')+'/rest/startScan',params={'u':config['username'],'t':token,'s':salt,'v':'1.16.1','c':'MusicOps','f':'json'})
        response=r.json().get('subsonic-response',{})
        if r.status_code!=200 or response.get('status')!='ok':raise HttpError('ACCESS_BLOCKED','Navidrome 扫描未通过，请确认账号具有扫描权限')
        return {'state':'requested','message':'已请求 Navidrome 扫描；Roon 由监听目录自动扫描'}
    except httpx.HTTPError:raise HttpError('NETWORK_ERROR','Navidrome 扫描请求失败') from None
