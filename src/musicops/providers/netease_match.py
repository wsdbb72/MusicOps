"""A foreign platform ID is never a NetEase identity or download parameter."""
import json,re
from musicops.clients.http import SafeHttpTransport,HttpError,PolicyBlocked
from musicops.clients.vault import cookie_header
from musicops.domain.identity import strict_text


def netease_reference(target):
    source=target.get('source')
    if source in (None,'netease') and re.fullmatch(r'[1-9][0-9]{0,18}',str(target.get('source_song_id') or '')):return target
    match=target.get('netease_match')
    if not isinstance(match,dict) or match.get('source')!='netease':return None
    if not re.fullmatch(r'[1-9][0-9]{0,18}',str(match.get('source_song_id') or '')):return None
    if not same_recording(target,match):return None
    return match


def same_recording(target,candidate):
    artists=lambda row:{strict_text(a) for a in row.get('artists',[]) if a}
    if not target.get('raw_title') or not artists(target) or not target.get('album'):return False
    if strict_text(target['raw_title'])!=strict_text(candidate.get('raw_title')) or artists(target)!=artists(candidate):return False
    if strict_text(target['album'])!=strict_text(candidate.get('album')):return False
    a,b=target.get('duration'),candidate.get('duration')
    return type(a) in (int,float) and type(b) in (int,float) and a>0 and b>0 and abs(a-b)<=2


def resolve_netease_target(target,config,secret_getter,checkpoint=lambda *_:None,transport=None):
    reference=netease_reference(target)
    if reference:return reference
    if not target.get('raw_title') or not target.get('artists') or not target.get('album') or not target.get('duration'):
        raise PolicyBlocked('跨平台回退需要确认歌曲、艺人、专辑及完整时长；可先核对资料或从客户端下载目录导入')
    checkpoint('netease_cross_platform_match');transport=transport or SafeHttpTransport(['music.163.com'],offline=False)
    headers={'Referer':'https://music.163.com/','Accept':'application/json'}
    ref=config['netease'].get('credential_ref')
    if ref:
        raw=secret_getter(ref,'music.163.com')
        if raw:headers['Cookie']=cookie_header(raw,'music.163.com','/api/search/get')
    response=transport.request('GET','https://music.163.com/api/search/get',params={'s':target['raw_title']+' '+' '.join(target['artists']),'type':1,'limit':30,'offset':0},headers=headers,timeout=20,max_response_bytes=5_000_000)
    if response.status_code in (401,403):raise HttpError('AUTH_REQUIRED','网易云跨平台匹配需要有效登录态')
    if response.status_code==429:raise HttpError('RATE_LIMITED','网易云跨平台匹配暂时限流')
    if response.status_code!=200:raise HttpError('SOURCE_UNAVAILABLE','网易云匹配服务暂不可用')
    try:data=response.json()
    except ValueError:raise HttpError('API_CONTRACT_ERROR','网易云匹配返回结构无效') from None
    if not isinstance(data,dict) or data.get('code')!=200:raise HttpError('SOURCE_UNAVAILABLE','网易云未返回可核对的匹配结果')
    songs=data.get('result',{}).get('songs',[])
    if not isinstance(songs,list) or len(songs)>100:raise HttpError('API_CONTRACT_ERROR','网易云匹配曲目结构改变')
    from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
    matches={}
    for raw in songs:
        if not isinstance(raw,dict):continue
        normalized={'id':raw.get('id'),'name':raw.get('name'),'ar':raw.get('ar') or raw.get('artists'),'al':raw.get('al') or raw.get('album'),'dt':raw.get('dt',raw.get('duration'))}
        try:candidate=NetEaseReadOnlyProvider.normalize_song(normalized)
        except (ValueError,TypeError,KeyError):continue
        if same_recording(target,candidate):matches[candidate['source_song_id']]=candidate
    if len(matches)!=1:raise PolicyBlocked('没有唯一且歌曲、艺人、专辑、时长均匹配的网易云音源；请核对资料或使用客户端下载目录，原平台 ID 不会用于网易云下载')
    return next(iter(matches.values()))
