"""go-music-dl web adapter. Keep credentials local and reject previews by decoding."""
import contextlib,json,os,re,uuid
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urlsplit
import httpx
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.clients.vault import cookie_header
from musicops.domain.identity import strict_text
from musicops.library.paths import reject_reparse_chain
from musicops.media.tags import read_tag_fields
from musicops.library.index import sha256_file

SOURCES={'qqmusic':'qq','kuwo':'kuwo','kugou':'kugou','migu':'migu','qishui':'soda',
         'bilibili':'bilibili','applemusic':'apple','spotify':'spotify','youtube':'youtube'}
NAMES={'netease':'网易云','qqmusic':'QQ 音乐','kuwo':'酷我','kugou':'酷狗','migu':'咪咕','qishui':'汽水','bilibili':'bilibili','applemusic':'Apple Music','spotify':'Spotify','youtube':'YouTube Music','tidal':'TIDAL','qobuz':'Qobuz','deezer':'Deezer','soundcloud':'SoundCloud','joox':'JOOX','audius':'Audius','qianqian':'千千音乐','streetvoice':'街声'}

def validate(settings):
    try:u=urlsplit(settings['base_url'])
    except ValueError:raise ValueError('流媒体服务地址无效') from None
    if u.scheme not in ('http','https') or not u.hostname or u.username or u.password or u.query or u.fragment:
        raise ValueError('流媒体服务需要明确的 HTTP(S) 地址，不含凭据、查询或片段')
    if u.scheme=='http' and u.hostname not in ('localhost','127.0.0.1','::1'):raise ValueError('远程流媒体服务必须使用 HTTPS')
    if not settings['order'] or len(set(settings['order']))!=len(settings['order']) or any(s not in NAMES for s in settings['order']):raise ValueError('流媒体回退顺序包含重复或不支持的平台')
    external=settings.get('musicdl',{})
    if not isinstance(external,dict) or type(external.get('enabled',False)) is not bool or not isinstance(external.get('python_path',''),str) or len(external.get('python_path',''))>4096:raise ValueError('外部 musicdl 设置无效')
    if not 3<=settings['timeout']<=120:raise ValueError('流媒体服务超时须为 3 至 120 秒')
    return settings

class SongCards(HTMLParser):
    def __init__(self):super().__init__();self.rows=[]
    def handle_starttag(self,tag,attrs):
        a=dict(attrs)
        if tag!='li' or 'song-card' not in a.get('class','').split():return
        try:extra=json.loads(a.get('data-extra') or '{}') or {}
        except ValueError:extra={}
        self.rows.append({'id':a.get('data-id'),'source':a.get('data-source'),'name':a.get('data-name'),
            'artist':a.get('data-artist'),'album':a.get('data-album'),'duration':a.get('data-duration'),'extra':extra})

def matches(target,song):
    try:duration=float(song.get('duration') or 0);expected=float(target.get('duration') or 0)
    except (ValueError,TypeError):return False
    title=strict_text(song.get('name'));artist=strict_text(song.get('artist'));album=strict_text(song.get('album'))
    names=[target,*target.get('verified_aliases',[])]
    for name in names:
        artists=name.get('artists') or [];credit={strict_text(a) for a in re.split(r'\s+/\s+|\s+&\s+|;|、',song.get('artist') or '') if a.strip()}
        if title==strict_text(name.get('raw_title') or name.get('title')) and (credit=={strict_text(a) for a in artists} or artist==strict_text(' / '.join(artists))) and album==strict_text(name.get('album') or target.get('album')) and expected>0 and abs(duration-expected)<=2:return True
    return False

class StreamingProvider:
    def __init__(self,settings,secret_getter,client=None):
        self.settings=validate(settings);self.base=settings['base_url'].rstrip('/');u=urlsplit(self.base)
        self.loopback=u.hostname in ('localhost','127.0.0.1','::1')
        raw=secret_getter(settings['credential_ref'],u.hostname) if settings.get('credential_ref') else None
        self.headers={'Cookie':cookie_header(raw,u.hostname,u.path or '/')} if raw else {}
        if settings.get('_managed_token'):self.headers['X-MusicOps-Service']=settings['_managed_token']
        self.client=client
    def _session(self):return contextlib.nullcontext(self.client) if self.client is not None else httpx.Client(verify=True,follow_redirects=False,trust_env=not self.loopback,timeout=self.settings['timeout'])
    @staticmethod
    def _ok(response):
        if response.status_code in (401,403) or 300<=response.status_code<400:raise HttpError('AUTH_REQUIRED','流媒体服务需要登录；请在向导填写服务 Cookie')
        if response.status_code!=200:raise HttpError('SOURCE_UNAVAILABLE','流媒体服务暂不可用')
    def search(self,platform,target):
        terms=list(dict.fromkeys([(' '.join(target.get('artists',[]))+' '+target.get('raw_title','')).strip(),target.get('raw_title','').strip()]))
        for term in terms:
            if not term:continue
            try:return self._search_query(platform,target,term)
            except HttpError as exc:
                if exc.error_class!='NO_MATCH':raise
        raise HttpError('NO_MATCH','未找到经艺人、专辑和完整时长核实的唯一音源；仅歌名检索不能直接确认身份')
    def _search_query(self,platform,target,term):
        if platform not in SOURCES:raise ValueError('未知流媒体来源')
        with self._session() as client:
            with client.stream('GET',self.base+'/search',params={'q':term,'sources':SOURCES[platform],'type':'song'},headers=self.headers) as response:
                self._ok(response);data=bytearray()
                for block in response.iter_bytes():
                    data.extend(block)
                    if len(data)>5_000_000:raise HttpError('INVALID_PAYLOAD','流媒体检索结果过大')
        text=data.decode('utf-8-sig')
        if text.lstrip().startswith('{'):
            payload=json.loads(text);rows=payload.get('songs')
            if not isinstance(rows,list):raise HttpError('API_CONTRACT_ERROR','流媒体检索结构改变')
        else:parser=SongCards();parser.feed(text);rows=parser.rows
        if len(rows)>10000 or any(not isinstance(r,dict) for r in rows):raise HttpError('API_CONTRACT_ERROR','流媒体歌曲清单结构改变')
        selected=[r for r in rows if r.get('source')==SOURCES[platform] and matches(target,r) and isinstance(r.get('id'),str) and 0<len(r['id'])<=200]
        unique={r['id']:r for r in selected}
        if len(unique)!=1:raise HttpError('NO_MATCH' if not unique else 'AMBIGUOUS','未找到唯一同名、同艺人、同专辑且完整时长一致的音源')
        return next(iter(unique.values()))
    def download(self,platform,target,directory,validator,checkpoint=lambda _:None):
        song=self.search(platform,target);destination=Path(directory);reject_reparse_chain(destination);destination.mkdir(parents=True,exist_ok=True)
        params={'id':song['id'],'source':SOURCES[platform],'name':song['name'],'artist':song['artist'],'album':song['album'],'extra':json.dumps(song.get('extra') or {},ensure_ascii=False)}
        if len(params['extra'])>20_000:raise HttpError('INVALID_PAYLOAD','流媒体音源附加资料过大')
        partial=destination/(uuid.uuid4().hex+'.part');size=0
        try:
            with self._session() as client:
                with client.stream('GET',self.base+'/download',params=params,headers=self.headers,timeout=120) as response:
                    self._ok(response)
                    with partial.open('xb') as out:
                        for block in response.iter_bytes(1024*1024):
                            checkpoint('streaming_download');size+=len(block)
                            if size>4*1024**3:raise HttpError('INVALID_PAYLOAD','单曲音源超过大小上限')
                            out.write(block)
                        out.flush();os.fsync(out.fileno())
            validation=validator.validate(partial)
            if size<1024 or validation.get('decode_state')!='passed' or not validation.get('duration') or abs(validation['duration']-target['duration'])>2:
                raise HttpError('INCOMPLETE_AUDIO','音源是试听、受保护内容或未通过完整音频校验，继续其他平台')
            tags=read_tag_fields(partial)
            observed={'name':(tags.get('title') or [song['name']])[0],'artist':' / '.join(tags['artist']) if tags.get('artist') else song['artist'],
                      'album':(tags.get('album') or [song['album']])[0],'duration':validation['duration']}
            if not matches(target,observed):raise HttpError('IDENTITY_CONFLICT','下载音源标签与检索身份不一致')
            codec=str(validation.get('media_info',{}).get('codec_name') or '').lower()
            extension={'flac':'.flac','alac':'.m4a','aac':'.aac' if validation.get('format',{}).get('format_name')=='aac' else '.m4a','mp3':'.mp3','vorbis':'.ogg'}.get(codec)
            if codec.startswith('pcm_'):extension='.wav'
            if not extension:raise HttpError('INVALID_PAYLOAD','流媒体音频编码尚不支持导入')
            output=partial.with_suffix(extension);partial.rename(output)
            checked=validator.validate(output)
            if checked.get('decode_state')!='passed' or not checked.get('roon_compatible'):
                output.unlink(missing_ok=True);raise HttpError('INCOMPLETE_AUDIO','音频容器或编码未通过 Roon 兼容检查')
            return output,{'source':platform,'source_song_id':song['id'],'duration':validation['duration'],'sha256':sha256_file(output)}
        finally:partial.unlink(missing_ok=True)
