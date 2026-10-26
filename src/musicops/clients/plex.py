"""Plex music sections and authenticated, targeted library scan requests."""
import re
from urllib.parse import urlsplit
from xml.etree import ElementTree
import httpx
from musicops.clients.http import HttpError,local_service_host


def validate(config):
    if not config.get('enabled'):return
    u=urlsplit(config.get('url',''))
    if u.scheme not in ('http','https') or not u.hostname or u.username or u.password or u.query or u.fragment:
        raise ValueError('Plex 地址须为不含凭据的 HTTP(S) 服务地址')
    if u.scheme=='http' and not local_service_host(u.hostname):raise ValueError('公网 Plex 请使用 HTTPS；局域网支持 HTTP')
    if config.get('section_id') and not re.fullmatch(r'\d{1,12}',str(config['section_id'])):
        raise ValueError('Plex 音乐库 ID 须为数字，可先测试读取音乐库列表')


def request(config, secret_getter, path):
    validate({**config,'enabled':True})
    token=secret_getter(config.get('auth_secret_ref',''),urlsplit(config['url']).hostname)
    if not token:raise HttpError('AUTH_REQUIRED','请填写 Plex X-Plex-Token')
    try:
        with httpx.Client(timeout=15,follow_redirects=False,verify=True) as client:
            response=client.get(config['url'].rstrip('/')+path,headers={'X-Plex-Token':token,'Accept':'application/xml'})
        if response.status_code in (401,403):raise HttpError('AUTH_FAILED','Plex Token 无效或权限不足')
        if response.status_code not in (200,201,202,204):raise HttpError('ACCESS_BLOCKED','Plex 未接受请求，请检查音乐库 ID 和权限')
        return response
    except httpx.HTTPError:raise HttpError('NETWORK_ERROR','Plex 连接失败，请检查地址') from None


def sections(config, secret_getter):
    response=request(config,secret_getter,'/library/sections')
    if len(response.content)>2_000_000 or b'<!DOCTYPE' in response.content.upper():
        raise HttpError('INVALID_PAYLOAD','Plex 音乐库返回无效')
    try:
        root=ElementTree.fromstring(response.content)
        if root.tag!='MediaContainer':raise ValueError()
        rows=[{'id':r.get('key'),'name':r.get('title'),'paths':[v.get('path') for v in r.findall('Location')]} for r in root.findall('Directory') if r.get('type')=='artist' and re.fullmatch(r'\d{1,12}',r.get('key',''))]
        return {'status':'OK','sections':rows,'message':'Plex 连接成功，共 '+str(len(rows))+' 个音乐库；请选定音乐库 ID'}
    except (ElementTree.ParseError,ValueError):raise HttpError('INVALID_PAYLOAD','Plex 返回的音乐库格式无效') from None


def scan(config, secret_getter):
    if not config.get('enabled'):return {'state':'skipped','message':'Plex 未启用'}
    result=sections(config,secret_getter)
    section=str(config.get('section_id',''))
    if not any(row['id']==section for row in result['sections']):
        raise HttpError('INVALID_INPUT','所选 Plex ID 不是可访问的音乐库，请在设置中测试并选择')
    request(config,secret_getter,'/library/sections/'+section+'/refresh')
    return {'state':'requested','message':'已请求 Plex 扫描音乐库'}
