"""Read-only connection checks, independent from the album execution queue."""
import copy
import time
from urllib.parse import urlsplit
from musicops.clients.http import SafeHttpTransport


def run(service,args,cfg):
    kind=args.get('kind');settings=copy.deepcopy(args.get('settings') or {})
    started=time.monotonic()
    if kind=='plex':
        from musicops.clients.plex import sections
        original=cfg['music_servers']['plex'];current={**original,**{k:v for k,v in settings.items() if k in ('url','section_id')}}
        token=settings.get('token')
        getter=lambda ref,host:token or service.vault.get(ref,host)
        if not token and current['url']!=original['url']:raise ValueError('地址已改变，请填写该服务器的 Plex Token')
        result=sections(current,getter)
    elif kind=='ai':
        from musicops.providers.ai import AIProvider
        current={**cfg['ai'],**{k:v for k,v in settings.items() if k in ('base_url','model','protocol')}}
        key=settings.get('key');host=urlsplit(current['base_url']).hostname
        if not key and current['base_url']!=cfg['ai']['base_url']:raise ValueError('地址已改变，请填写对应 AI Key')
        getter=lambda ref,bound_host:key or service.vault.get(ref,bound_host)
        transport=SafeHttpTransport([host],offline=False)
        try:
            provider=AIProvider(current,transport,getter)
            provider.analyze({'connection_test':True},'这是连接测试。仅返回连接成功，不搜索，不调用工具。')
        finally:transport.close()
        result={'status':'OK','message':'AI 连接、鉴权及当前模型响应成功'}
    elif kind=='lyrics':
        from musicops.providers.lrcapi import LrcApiClient
        current=copy.deepcopy(cfg['lyrics']);original=current['primary'];current['primary']={**original,**{k:v for k,v in settings.items() if k in ('base_url',)}}
        current['public_fallback']['enabled']=False;current['direct_backup']['enabled']=False
        key=settings.get('authorization');host=urlsplit(current['primary']['base_url']).hostname
        if not key and current['primary']['base_url']!=original['base_url']:raise ValueError('地址已改变，请填写对应歌词鉴权')
        if not key and not service.vault.get(current['primary'].get('auth_secret_ref'),host):
            return {'status':'NOT_CONFIGURED','message':'尚未填写此歌词服务的鉴权值；请在设置的歌词接口中填写完整 Authorization 内容并保存后再测试','valid_candidates':0,'seconds':round(time.monotonic()-started,2),'read_only':True}
        transport=SafeHttpTransport([host],offline=False)
        try:
            client=LrcApiClient(current,transport,lambda ref,bound_host:key or service.vault.get(ref,bound_host))
            response=client.candidates(args.get('title') or '童年',args.get('artist') or '罗大佑',args.get('album') or '之乎者也',refresh=True)
            valid=sum(row.get('validation',{}).get('valid',False) for row in response['candidates'])
            result={'status':'OK' if response['state']=='VALID_PAYLOAD' else response['state'],'valid_candidates':valid,
                    'message':'歌词接口连接成功，返回 '+str(valid)+' 项有效同步歌词' if valid else '歌词测试未通过：'+response['state'],'errors':response.get('errors',[])}
        finally:transport.close()
    else:raise ValueError('请选择 AI、歌词或 Plex 连接测试')
    return {**result,'seconds':round(time.monotonic()-started,2),'read_only':True}
