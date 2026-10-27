"""Download playable NetEase URLs; unavailable/trial tracks remain incomplete."""
import json,os
from pathlib import Path
from urllib.parse import urlsplit
import httpx
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.clients.vault import cookie_header
from musicops.library.paths import reject_reparse_chain

def download_track(target,config,destination,secret_getter,checkpoint=lambda _:None):
    from musicops.providers.netease_match import netease_reference
    reference=netease_reference(target)
    if reference is None:raise PolicyBlocked('下载需要已核实的网易云歌曲 ID，不能使用其他平台的 ID')
    target=reference
    destination=Path(destination);reject_reparse_chain(destination);destination.mkdir(parents=True,exist_ok=True)
    song_id=str(target['source_song_id']);api=config['workflow']['netease_api_url'].rstrip('/')
    origin=urlsplit(api) if api else urlsplit('https://music.163.com')
    if api and (origin.scheme not in ('http','https') or origin.scheme=='http' and origin.hostname not in ('localhost','127.0.0.1','::1')):raise PolicyBlocked('网易云 API 需要 HTTPS 或本机地址')
    headers={};ref=config['netease'].get('credential_ref')
    if not api and ref:
        raw=secret_getter(ref,'music.163.com')
        if raw:headers['Cookie']=cookie_header(raw,'music.163.com','/api/song/enhance/player/url')
    endpoint=api+'/song/url' if api else 'https://music.163.com/api/song/enhance/player/url'
    params={'id':song_id,'br':'999000'} if api else {'ids':json.dumps([int(song_id)]),'br':'999000'}
    try:
        with httpx.Client(verify=True,follow_redirects=False,timeout=20) as client:
            r=client.get(endpoint,params=params,headers=headers)
            if r.status_code!=200:raise HttpError('SOURCE_UNAVAILABLE','网易云下载接口暂不可用')
            data=r.json();rows=data.get('data',[])
            if not isinstance(rows,list) or len(rows)!=1 or str(rows[0].get('id'))!=song_id:raise HttpError('API_CONTRACT_ERROR','下载接口未返回当前曲目')
            item=rows[0];url=item.get('url');u=urlsplit(url or '')
            if item.get('freeTrialInfo') or item.get('trialInfo') or not url:raise HttpError('SOURCE_UNAVAILABLE','当前曲目不可完整下载；可在网易云客户端下载后导入')
            if u.scheme not in ('http','https') or u.username or u.password or not u.hostname or not (u.hostname.endswith('.music.126.net') or u.hostname.endswith('.music.163.com')):raise PolicyBlocked('网易云音源没有返回受支持的下载地址')
            if u.scheme=='http':url='https://'+url[7:]
            kind=str(item.get('type','')).lower()
            if kind not in ('flac','mp3','m4a','ncm'):raise HttpError('INVALID_PAYLOAD','网易云音源格式不受支持')
            file=destination/(song_id+'.'+kind);temporary=file.with_suffix(file.suffix+'.part');reject_reparse_chain(temporary)
            if file.exists() or temporary.exists():raise PolicyBlocked('下载文件已存在，需要核对后再继续')
            size=0
            with client.stream('GET',url,headers={},timeout=60) as audio:
                if audio.status_code!=200:raise HttpError('NETWORK_ERROR','音源下载失败')
                with temporary.open('xb') as out:
                    for block in audio.iter_bytes(1024*1024):
                        checkpoint('netease_download');size+=len(block)
                        if size>4*1024**3:raise HttpError('INVALID_PAYLOAD','单曲超过下载大小上限')
                        out.write(block)
                    out.flush();os.fsync(out.fileno())
            if size<1024 or type(item.get('size')) is int and item['size']>0 and size!=item['size']:raise HttpError('INVALID_PAYLOAD','音源大小与下载响应不一致')
            os.rename(temporary,file);return str(file)
    except httpx.HTTPError:raise HttpError('NETWORK_ERROR','网易云下载请求失败') from None

def import_match(target,directory):
    """Select an existing client download using its tags or NCM metadata."""
    from musicops.library.index import index_library
    from musicops.media.ncm import ncm_metadata
    from musicops.domain.identity import strict_text
    if not directory:return None
    root=Path(directory);reject_reparse_chain(root)
    if not root.is_dir():raise PolicyBlocked('网易云客户端下载目录不存在')
    rows=index_library(root,metadata_only=True);matches=[]
    if len(rows)>5000:raise PolicyBlocked('客户端下载目录超过 5000 个文件，请选择更具体的目录')
    for row in rows:
        if Path(row['path']).suffix.lower()=='.ncm':
            try:meta=ncm_metadata(row['path'])
            except ValueError:continue
            title=meta.get('musicName');artists=[a[0] for a in meta.get('artist',[]) if isinstance(a,list) and a];album=meta.get('album') or meta.get('albumName')
        else:title=row.get('raw_title');artists=row.get('artists',[]);album=row.get('album')
        if strict_text(title)==strict_text(target['raw_title']) and {strict_text(a) for a in artists}=={strict_text(a) for a in target['artists']} and strict_text(album)==strict_text(target['album']):matches.append(row['path'])
    if len(matches)>1:raise PolicyBlocked('客户端下载目录有多个相同曲目版本，需要先核对')
    return matches[0] if matches else None

def lyrics_for_id(song_id,duration=None):
    """An explicit NetEase song ID is identity evidence for its official LRC."""
    from musicops.media.resources import validate_lrc
    try:
        with httpx.Client(verify=True,follow_redirects=False,timeout=15) as client:
            r=client.get('https://music.163.com/api/song/lyric',params={'id':str(song_id),'lv':'-1','kv':'-1','tv':'-1'})
        if r.status_code!=200:return None
        payload=r.json();text=payload.get('lrc',{}).get('lyric')
        if payload.get('code')!=200 or not isinstance(text,str):return None
        # NetEase prefixes credits as JSON rows; drop them rather than the whole timeline.
        from musicops.media.lyrics import player_lrc
        text=player_lrc(text)
        return text if validate_lrc(text,duration)['valid'] else None
    except (httpx.HTTPError,ValueError,TypeError):return None
