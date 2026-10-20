"""Human-facing setup form to typed configuration and domain-bound private vault."""
from __future__ import annotations
import copy,hashlib,json
from pathlib import Path
from urllib.parse import urlsplit
from musicops.clients.vault import import_cookies
from musicops.providers.pt_readonly import HOSTS

def save_setup(service,payload):
    current=service.config.current();values=copy.deepcopy(current['values']);base=service.base
    library=Path(payload.get('library') or values['paths']['library'])
    if not library.is_absolute():raise ValueError('请选择完整的音乐库目录')
    values['offline']=False;values['paths']['library']=str(library)
    from musicops.library.storage import storage_paths
    values['paths'].update(storage_paths(library))
    for key in ('ffmpeg','ffprobe'):
        if payload.get(key):values['paths']['tools'][key]=payload[key]
    sites=payload.get('sites',{})
    pending=[]
    for site in HOSTS:
        item=sites.get(site,{})
        values['providers'][site]['enabled']=bool(item.get('enabled',values['providers'][site]['enabled']))
        text=item.get('cookie_json','').strip()
        if text:
            cookies=import_cookies(text,HOSTS[site]);ref='vault://pt/'+site+'/cookies'
            pending.append((ref,json.dumps(cookies),HOSTS[site],('providers',site,'credential_ref')))
    if payload.get('netease_cookie_json','').strip():
        cookies=import_cookies(payload['netease_cookie_json'],'music.163.com');ref='vault://netease/user/cookies'
        pending.append((ref,json.dumps(cookies),'music.163.com',('netease','credential_ref')))
        pending.append((ref,json.dumps(cookies),'music.163.com',('providers','netease','credential_ref')))
    for platform,text in payload.get('playlist_credentials',{}).items():
        if platform not in values['playlist_sources']:raise ValueError('未知歌单平台')
        if not isinstance(text,str):raise ValueError('歌单平台登录信息须为文本')
        text=text.strip()
        if not text:continue
        from musicops.providers.playlist_access import credential_hosts,validate_credential
        hosts=credential_hosts(platform);text=validate_credential(platform,text)
        pending.append(('vault://playlist/'+platform,text,hosts,('playlist_sources',platform,'credential_ref')))
    def secret(kind,url,value,path):
        host=urlsplit(url).hostname
        if not host:raise ValueError('服务地址需要包含主机名')
        ref='vault://user/'+kind+'/'+hashlib.sha256(host.encode()).hexdigest()[:12]
        if value:pending.append((ref,value,host,path))
    if 'streaming' in payload:
        supplied=payload['streaming'];target=values['streaming'];previous_host=urlsplit(target['base_url']).hostname
        for key in ('managed','enabled','base_url','order','timeout'):
            if key in supplied:target[key]=supplied[key]
        if previous_host!=urlsplit(target['base_url']).hostname:target['credential_ref']=''
        if supplied.get('cookie_json'):
            host=urlsplit(target['base_url']).hostname;cookies=import_cookies(supplied['cookie_json'],host)
            secret('streaming',target['base_url'],json.dumps(cookies),('streaming','credential_ref'))
    qbt=payload.get('qbt',{})
    if qbt.get('url'):values['qbt']['base_url']=qbt['url'].rstrip('/')
    if qbt.get('password'):
        if not qbt.get('username'):raise ValueError('请填写 qBittorrent WebUI 用户名')
        secret('qbt',values['qbt']['base_url'],json.dumps({'username':qbt['username'],'password':qbt['password']}),('qbt','credential_ref'))
    for kind in ('ai','lyrics'):
        item=payload.get(kind,{})
        if not item:continue
        target=values['ai'] if kind=='ai' else values['lyrics']['primary']
        if item.get('url'):target['base_url']=item['url'].rstrip('/')
        if kind=='ai':
            target['enabled']=bool(item.get('enabled',target['enabled']))
            if item.get('model'):target['model']=item['model']
        secret(kind,target['base_url'],item.get('key' if kind=='ai' else 'authorization',''),('ai','auth_secret_ref') if kind=='ai' else ('lyrics','primary','auth_secret_ref'))
    if 'browser_solver' in payload:
        settings=payload['browser_solver']
        if not isinstance(settings,dict) or set(settings)-{'managed','enabled','base_url','timeout','executable','auto_start'}:raise ValueError('浏览器验证设置无效')
        values['browser_solver'].update(settings)
    if 'catalog_web' in payload:values['catalog']['web_enabled']=bool(payload['catalog_web'])
    if 'prefer_simplified' in payload:values['metadata']['prefer_simplified']=bool(payload['prefer_simplified'])
    if payload.get('discogs_token'):secret('discogs','https://api.discogs.com',payload['discogs_token'],('catalog','discogs','credential_ref'))
    server=payload.get('servers',{})
    roon=values['music_servers']['roon'];roon['enabled']=bool(server.get('roon',roon['enabled']))
    roon['library']=str(library);roon['watched_folders']=[str(library)] if roon['enabled'] else []
    nav=server.get('navidrome',{})
    if nav:
        target=values['music_servers']['navidrome'];target['enabled']=bool(nav.get('enabled',target['enabled']))
        target['library']=str(library)
        if nav.get('url'):target['url']=nav['url'].rstrip('/')
        if nav.get('username'):target['username']=nav['username']
        if nav.get('password'):secret('navidrome',target['url'],nav['password'],('music_servers','navidrome','password_ref'))
    workflow=payload.get('workflow',{})
    for key in ('edition_policy','netease_fallback','netease_api_url','netease_downloader','netease_download_args','netease_import_dir','prefer_24bit','fl_per_batch','fl_unlimited'):
        if key in workflow:values['workflow'][key]=workflow[key]
    if 'updates' in payload:
        if not isinstance(payload['updates'],dict) or type(payload['updates'].get('enabled')) is not bool:raise ValueError('自动更新设置无效')
        values['updates']['enabled']=payload['updates']['enabled']
    values['workflow']['setup_done']=True
    # Save URLs first: a changed domain intentionally unbinds its previous key.
    from musicops.config.schema import validate_config
    validate_config(values)
    from musicops.library.paths import reject_reparse_chain
    reject_reparse_chain(library);library.mkdir(parents=True,exist_ok=True)
    saved=service.config.save(values,current['version']);values=copy.deepcopy(saved['values'])
    for ref,value,host,path in pending:
        service.vault.set(ref,value,host);obj=values
        for key in path[:-1]:obj=obj[key]
        obj[path[-1]]=ref
    if pending:saved=service.config.save(values,saved['version'])
    if saved['values']['ai'].get('enabled'):
        from musicops.app.review_queue import submit
        submit(service,{'all':True})
    service.prepare_local_catalog()
    return {'config':saved,'saved':True,'credentials_saved':len(pending),'message':'设置已保存，可以开始使用。'}
