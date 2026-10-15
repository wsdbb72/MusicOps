"""Local platform detection and bounded, credential-free share-link resolution."""
import html,re
from urllib.parse import parse_qs,urljoin,urlsplit,urlencode
from musicops.clients.http import HttpError

PLATFORMS={'netease':'网易云音乐','qqmusic':'QQ 音乐','qishui':'汽水音乐','kugou':'酷狗音乐','kuwo':'酷我音乐','migu':'咪咕音乐','bilibili':'bilibili','applemusic':'Apple Music','spotify':'Spotify','youtube':'YouTube Music'}
INPUT_HOSTS={
    'netease':{'music.163.com','m.music.163.com','y.music.163.com','163cn.tv'},
    'qqmusic':{'y.qq.com','i.y.qq.com','c.y.qq.com','c6.y.qq.com','c7.y.qq.com','c8.y.qq.com','c9.y.qq.com'},
    'qishui':{'music.douyin.com','qishui.douyin.com','qishui.com','www.qishui.com','v.douyin.com'},
    'kugou':{'www.kugou.com','m.kugou.com','m3ws.kugou.com','t1.kugou.com','t2.kugou.com','kugou.com'},
    'kuwo':{'www.kuwo.cn','kuwo.cn','m.kuwo.cn','h5.kuwo.cn'},
    'migu':{'music.migu.cn','m.music.migu.cn','c.musicapp.migu.cn'},
    'bilibili':{'www.bilibili.com','bilibili.com','space.bilibili.com','b23.tv'},
    'applemusic':{'music.apple.com'},'spotify':{'open.spotify.com','spotify.link'},
    'youtube':{'music.youtube.com','www.youtube.com','youtube.com','m.youtube.com','youtu.be'},
}
SHARE_HOSTS={'163cn.tv','c6.y.qq.com','c7.y.qq.com','c8.y.qq.com','c9.y.qq.com','v.douyin.com','t1.kugou.com','t2.kugou.com','b23.tv','spotify.link'}


def _one(query,*keys):
    values=[str(value) for key in keys for value in query.get(key,[]) if value]
    if len(set(values))>1:raise ValueError('链接包含冲突的歌单 ID，请重新复制歌单分享链接')
    return values[0] if values else None


def _id(value):
    if not isinstance(value,str) or not re.fullmatch(r'[A-Za-z0-9._~=-]{1,200}',value) or value=='0':raise ValueError('歌单 ID 格式无效')
    return value


def canonical(platform,pid,extra=None):
    pid=_id(pid);extra=extra or {}
    urls={
        'netease':'https://music.163.com/playlist?id='+pid,
        'qqmusic':'https://y.qq.com/n/ryqq/playlist/'+pid,
        'qishui':'https://music.douyin.com/qishui/playlist?playlist_id='+pid,
        'kugou':'https://www.kugou.com/yy/special/single/'+pid+'.html',
        'kuwo':'https://www.kuwo.cn/playlist_detail/'+pid,
        'migu':'https://music.migu.cn/v3/music/playlist/'+pid,
        'bilibili':'https://www.bilibili.com/medialist/detail/ml'+pid,
        'applemusic':'https://music.apple.com/'+extra.get('storefront','cn')+'/playlist/'+pid,
        'spotify':'https://open.spotify.com/playlist/'+pid,
        'youtube':'https://music.youtube.com/playlist?list='+pid,
    }
    return {'platform':platform,'playlist_id':pid,'url':urls[platform],**extra}


def parse_playlist_locator(value,platform='auto'):
    if platform not in ('auto',*PLATFORMS):raise ValueError('不支持所选歌单平台')
    if not isinstance(value,str) or not value.strip() or len(value)>100000:raise ValueError('请粘贴歌单链接或分享文本')
    text=html.unescape(value.strip());urls=re.findall(r'https?://[^\s<>"“”]+',text)
    if not urls:
        chosen='netease' if platform=='auto' else platform
        if chosen=='netease':
            from musicops.providers.netease import parse_playlist_input
            parsed=parse_playlist_input(text)
            if parsed['status']!='OK':raise ValueError('纯数字 ID 默认属于网易云；其他平台请先选择平台或粘贴完整链接')
            return [canonical(chosen,pid) for pid in parsed['playlist_ids']]
        return [canonical(chosen,text)]
    if len(urls)>20:raise ValueError('一次最多解析 20 个歌单链接')
    locators=[]
    for raw in urls:
        url=raw.rstrip(')）]】。，,;；');parts=urlsplit(url)
        try:port=parts.port
        except ValueError:raise ValueError('歌单链接端口无效') from None
        if parts.username or parts.password or port not in (None,80,443):raise ValueError('歌单链接不可包含登录信息或自定义端口')
        host=(parts.hostname or '').lower();chosen=next((key for key,hosts in INPUT_HOSTS.items() if host in hosts),None)
        if not chosen or platform!='auto' and platform!=chosen:raise ValueError('歌单域名与所选平台不匹配或不受支持')
        url=parts._replace(scheme='https',netloc=host).geturl();path=parts.path;query=parse_qs(parts.query)
        if parts.fragment and chosen=='netease':
            fragment=urlsplit(parts.fragment)
            if fragment.path:path=fragment.path;query=parse_qs(fragment.query)
        pid=None;extra={}
        if chosen=='netease' and path.rstrip('/') in ('/playlist','/m/playlist'):pid=_one(query,'id')
        elif chosen=='qqmusic':
            match=re.search(r'/playlist/([0-9]+)',path);pid=match.group(1) if match else _one(query,'id','disstid','dissid','songlistid')
        elif chosen=='qishui':
            match=re.search(r'/playlist/([A-Za-z0-9_-]+)',path);pid=match.group(1) if match else _one(query,'playlist_id','playlistId','id')
        elif chosen=='kugou':
            match=re.search(r'/(?:special/single|songlist)/((?:collection|gcid)_[A-Za-z0-9_-]+|[0-9]+)',path);pid=match.group(1) if match else _one(query,'specialid','specialId','collectionid','id')
            if not pid and _one(query,'gcid','global_collection_id'):
                pid=_one(query,'gcid','global_collection_id');extra['kind']='gcid'
        elif chosen=='kuwo':
            match=re.search(r'/(?:playlist_detail|playlist|npl)/([0-9]+)',path);pid=match.group(1) if match else _one(query,'pid','id')
        elif chosen=='migu':
            match=re.search(r'/playlist/([A-Za-z0-9]+)',path);pid=match.group(1) if match else _one(query,'playlistId','playlistid','id')
        elif chosen=='bilibili':
            match=re.search(r'/medialist/detail/ml(\d+)',path);pid=match.group(1) if match else _one(query,'fid','media_id')
            if pid:extra['kind']='favorite'
            else:
                match=re.search(r'/([0-9]+)/lists/([0-9]+)',path)
                if match:pid=match.group(2);extra.update(kind='season' if query.get('type',['season'])[0]=='season' else 'series',uid=match.group(1))
                else:
                    match=re.search(r'/audio/am([0-9]+)',path)
                    if match:pid=match.group(1);extra['kind']='audio'
        elif chosen=='applemusic':
            match=re.search(r'/([a-z]{2})/playlist/(?:[^/]+/)?(pl\.[A-Za-z0-9-]+)',path,re.I)
            if match:pid=match.group(2);extra['storefront']=match.group(1).lower()
        elif chosen=='spotify':
            match=re.search(r'/playlist/([A-Za-z0-9]+)',path)
            if match:pid=match.group(1)
        elif chosen=='youtube':pid=_one(query,'list')
        if pid:
            result=canonical(chosen,pid,extra)
            # Bili collection kind/owner and GCID are carried separately; the
            # original public URL can contain routing fields needed by providers.
            if chosen=='bilibili' or extra.get('kind')=='gcid':result['url']=url
            elif chosen=='applemusic':result['url']=parts._replace(scheme='https',netloc=host,query='',fragment='').geturl()
        elif host in SHARE_HOSTS or chosen in ('qishui','kugou','migu'):
            result={'platform':chosen,'playlist_id':None,'url':url,'short_url':url}
        else:raise ValueError('这是平台链接，但没有找到歌单 ID；请复制歌单而非单曲的分享链接')
        if result not in locators:locators.append(result)
    return locators


def resolve_share(locator,transport,checkpoint=lambda *_:None):
    if locator.get('playlist_id'):return locator
    platform=locator['platform'];url=locator['url']
    for _ in range(5):
        checkpoint('playlist_share_link');parts=urlsplit(url)
        if parts.scheme!='https' or parts.hostname not in INPUT_HOSTS[platform] or parts.username or parts.password or parts.port not in (None,443):raise ValueError('分享链接跳转到了其他域名，未继续访问')
        reply=transport.request('GET',url,headers={'User-Agent':'Mozilla/5.0 MusicOps'},timeout=15,allow_redirect_response=True,max_response_bytes=2_000_000)
        if reply.status_code in (401,403):raise HttpError('AUTH_REQUIRED','分享链接需要登录，请使用完整歌单链接或导入导出文件')
        if reply.status_code==429:raise HttpError('RATE_LIMITED','分享链接暂时限流，请稍后重试')
        if 300<=reply.status_code<400:
            target=next((v for k,v in reply.headers.items() if k.lower()=='location'),None)
            if not target:break
            url=urljoin(url,target)
        elif reply.status_code==200:
            body=reply.text
            candidates=re.findall(r'(?:content\s*=\s*[\'"][^\'"]*url=|location(?:\.href)?\s*=\s*[\'"])(https?://[^\s\'"<>]+)',body,re.I)
            if not candidates:break
            url=html.unescape(candidates[0])
        else:raise HttpError('SOURCE_UNAVAILABLE','分享链接暂不可访问，请导入平台导出的歌单文件')
        parsed=parse_playlist_locator(url,platform)
        if len(parsed)==1 and parsed[0].get('playlist_id'):return parsed[0]
    raise ValueError('未能从短链取得完整歌单 ID，请粘贴完整歌单链接或导入歌单文件')
