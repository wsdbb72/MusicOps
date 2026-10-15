"""Bounded, read-only playlist metadata adapters; no media/download endpoints.

Official token APIs are preferred. Public pages are a best-effort fallback, and
their visible rows are never treated as a complete playlist without a count and
an exhausted, consistent page chain. All code here is an independent implementation.
"""
from __future__ import annotations

import html,json,math,re
from urllib.parse import urljoin,urlsplit,parse_qs
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.clients.vault import cookie_header
from musicops.domain.identity import version_from_title

HOSTS={
    'spotify':('api.spotify.com','open.spotify.com'),
    'applemusic':('api.music.apple.com','music.apple.com'),
    'youtube':('www.googleapis.com','www.youtube.com','music.youtube.com'),
    'bilibili':('api.bilibili.com',),
}
MAX_TRACKS=50_000
MAX_PAGES=1000
MAX_RESPONSE=20_000_000

def _count(value):
    return value if type(value) is int and 0<=value<=MAX_TRACKS else None

def _text(value):return value.strip() if isinstance(value,str) else ''

def _seconds(value,milliseconds=False):
    if type(value) in (int,float) and math.isfinite(value) and 0<value<=86400000:
        return value/1000 if milliseconds else value
    if isinstance(value,str):
        iso=re.fullmatch(r'PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?',value)
        if iso and any(iso.groups()):return float(iso[1] or 0)*3600+float(iso[2] or 0)*60+float(iso[3] or 0)
        if re.fullmatch(r'\d+(?::\d{1,2}){1,2}',value):
            total=0
            for part in value.split(':'):total=total*60+int(part)
            return total or None
    return None

def _walk(value):
    """Iterative traversal bounded by the response limit and a node budget."""
    stack=[value];seen=0
    while stack:
        current=stack.pop();seen+=1
        if seen>400_000:raise HttpError('API_CONTRACT_ERROR','网页结构超过解析上限')
        if isinstance(current,dict):yield current;stack.extend(reversed(list(current.values())))
        elif isinstance(current,list):stack.extend(reversed(current))

def _runs(value):
    if not isinstance(value,dict):return ''
    if isinstance(value.get('simpleText'),str):return value['simpleText']
    return ''.join(_text(r.get('text')) for r in value.get('runs',[]) if isinstance(r,dict))

def _json_scripts(text):
    result=[]
    for match in re.finditer(r'<script\b([^>]*)>(.*?)</script\s*>',text,re.I|re.S):
        attrs,body=match.groups()
        if 'json' not in attrs.lower():continue
        try:result.append((attrs,json.loads(body)))
        except (ValueError,RecursionError):continue
    return result

def _assignment(text,marker):
    decoder=json.JSONDecoder()
    for match in re.finditer(marker,text):
        try:return decoder.raw_decode(text[match.end():].lstrip())[0]
        except (ValueError,RecursionError):continue
    return None

class GlobalPlaylistProvider:
    def __init__(self,platform,transport,config=None,secret_getter=None,checkpoint=None):
        if platform not in HOSTS:raise ValueError('不支持的国际歌单平台')
        self.platform=platform;self.transport=transport;self.config=config or {};self.secret_getter=secret_getter;self.checkpoint=checkpoint or (lambda *_:None)
        timeout=self.config.get('timeout',15)
        self.timeout=timeout if type(timeout) in (int,float) and math.isfinite(timeout) and 1<=timeout<=120 else 15

    def _request(self,url,params=None,headers=None,payload=None,public_redirect=None,_redirects=0):
        parsed=urlsplit(url)
        if parsed.scheme!='https' or parsed.hostname not in HOSTS[self.platform] or parsed.port not in (None,443) or parsed.username or parsed.password or parsed.fragment:raise PolicyBlocked('歌单请求来源不在白名单')
        self.checkpoint(self.platform+'_playlist_page')
        options={'params':params or {},'headers':{'Accept':'application/json, text/html','User-Agent':'MusicOps/0.4 (playlist metadata; read only)',**(headers or {})},'timeout':self.timeout,'allow_redirect_response':True,'max_response_bytes':MAX_RESPONSE}
        if parsed.hostname in ('music.apple.com','open.spotify.com','www.youtube.com','music.youtube.com'):
            options['headers']['User-Agent']='Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36'
            options['headers']['Accept']='text/html,application/xhtml+xml'
        if payload is not None:
            if parsed.hostname not in ('www.youtube.com','music.youtube.com') or parsed.path!='/youtubei/v1/browse':raise PolicyBlocked('只允许公开歌单的只读续页请求')
            options['content']=json.dumps(payload,separators=(',',':')).encode();options['headers']['Content-Type']='application/json'
        response=self.transport.request('POST' if payload is not None else 'GET',url,**options)
        actual=urlsplit(response.url or url)
        if actual.scheme!='https' or actual.hostname!=parsed.hostname or actual.path!=parsed.path or actual.port not in (None,443) or actual.username or actual.password:raise HttpError('ACCESS_BLOCKED','歌单响应来源改变')
        if response.status_code in (401,403):raise HttpError('AUTH_FAILED','歌单接口未授权或歌单不公开')
        if response.status_code==404:raise HttpError('NOT_FOUND','歌单不存在或不可见')
        if response.status_code==429:raise HttpError('RATE_LIMITED','平台限流，请稍后重试')
        if 300<=response.status_code<400:
            # Apple canonical URLs without a human-readable slug redirect to
            # the same public playlist. Never apply this to authenticated APIs.
            destination=urljoin(url,response.headers.get('location',''));target=urlsplit(destination)
            if public_redirect and _redirects<2 and parsed.hostname=='music.apple.com' and not headers and payload is None and target.scheme=='https' and target.hostname=='music.apple.com' and target.port in (None,443) and not target.username and not target.password and not target.query and not target.fragment and re.fullmatch(public_redirect,target.path):
                return self._request(destination,public_redirect=public_redirect,_redirects=_redirects+1)
            raise HttpError('ACCESS_BLOCKED','未向重定向页面转发登录信息')
        if response.status_code!=200:raise HttpError('NETWORK_ERROR','歌单元数据请求未完成')
        if len(response.body)>MAX_RESPONSE:raise HttpError('INVALID_PAYLOAD','歌单响应超过解析上限')
        return response

    def _json(self,url,**kwargs):
        try:value=self._request(url,**kwargs).json()
        except (ValueError,UnicodeError,RecursionError):raise HttpError('API_CONTRACT_ERROR','歌单接口未返回有效 JSON') from None
        if not isinstance(value,dict):raise HttpError('API_CONTRACT_ERROR','歌单接口结构改变')
        return value

    def _credential(self,host):
        ref=self.config.get('credential_ref')
        if not ref:return None
        raw=self.secret_getter(ref,host) if self.secret_getter else None
        if not isinstance(raw,str) or not raw.strip():raise HttpError('AUTH_REQUIRED','请在设置中填写此平台登录信息')
        if '\r' in raw.strip(' \r\n') or '\n' in raw.strip(' \r\n'):
            # JSON pretty-print whitespace is allowed, raw header injection is not.
            if not raw.lstrip().startswith(('{','[')):raise HttpError('AUTH_FAILED','鉴权值格式无效')
        if raw.lstrip().startswith(('{','[')):
            try:return json.loads(raw)
            except ValueError:raise HttpError('AUTH_FAILED','鉴权 JSON 格式无效') from None
        return raw.strip()

    @staticmethod
    def _bearer(value,key='access_token'):
        token=value.get(key) if isinstance(value,dict) else value
        if not isinstance(token,str) or not token.strip() or any(c in token for c in '\r\n\x00'):raise HttpError('AUTH_FAILED','缺少有效的平台 Token')
        return token if token.lower().startswith('bearer ') else 'Bearer '+token

    def _track(self,sid,title,artists=(),album='',album_id=None,duration=None,**extra):
        sid=str(sid)
        if not re.fullmatch(r'[A-Za-z0-9_.:-]{1,200}',sid):raise ValueError('曲目标识无效')
        artists=list(dict.fromkeys(_text(a) for a in artists if _text(a)));title=_text(title);album=_text(album)
        missing=[name for name,value in (('title',title),('artists',artists),('album',album),('duration',duration)) if not value]
        target=self.platform+':'+sid
        return {'id':target,'target_id':target,'source':self.platform,'source_song_id':sid,'raw_title':title,'artists':artists,'album':album,'source_album_id':str(album_id) if album_id else None,'duration':duration,'metadata_complete':not missing,'metadata_missing_fields':missing,'evidence_level':'live_readonly','download_eligibility':'UNKNOWN','version':version_from_title(title),'recording_version':version_from_title(title),**extra}

    def _result(self,pid,name,songs,declared,exhausted,warnings=None,observed=None,contract='api'):
        warnings=list(warnings or []);observed=len(songs) if observed is None else observed
        unique={}
        for song in songs:unique.setdefault(song['target_id'],song)
        duplicate_count=len(songs)-len(unique)
        if duplicate_count:warnings.append({'error_class':'DUPLICATE_OCCURRENCES','count':duplicate_count,'reason':'相同来源的重复曲目仅保留一个入库目标；源歌单出现次数另行对账'})
        complete=bool(exhausted and declared is not None and observed==declared and len(songs)==observed)
        if declared is None:warnings.append({'error_class':'COUNT_UNVERIFIED','reason':'来源未提供可核实总数，仅保留可见曲目'})
        elif observed!=declared:warnings.append({'error_class':'COUNT_MISMATCH','declared_count':declared,'observed_count':observed})
        if not exhausted:warnings.append({'error_class':'PAGINATION_INCOMPLETE','reason':'仍有未读取的分页，或来源未确认清单终点'})
        snapshot={'playlist_id':pid,'source':self.platform,'name':_text(name) or self.platform+' 歌单','songs':list(unique.values()),'complete':complete,'declared_count':declared,'observed_count':observed,'duplicate_occurrences':duplicate_count,'warnings':warnings,'contract':self.platform+'-playlist-'+contract+'.v1','evidence_level':'live_readonly','read_only':True,'metadata_complete':all(s['metadata_complete'] for s in songs)}
        return {'status':'OK' if complete else 'SNAPSHOT_INCOMPLETE','snapshot':snapshot}

    def refresh(self,locator):
        pid=str(locator.get('playlist_id','')) if isinstance(locator,dict) else ''
        try:
            if not isinstance(locator,dict) or locator.get('platform',self.platform)!=self.platform:raise ValueError('歌单平台不一致')
            patterns={'spotify':r'[A-Za-z0-9]{22}','applemusic':r'pl\.[A-Za-z0-9.-]{1,160}','youtube':r'[A-Za-z0-9_-]{10,160}','bilibili':r'[1-9][0-9]{0,19}'}
            if not re.fullmatch(patterns[self.platform],pid):raise ValueError('歌单 ID 格式无效')
            return getattr(self,'_'+self.platform)(pid,locator)
        except (HttpError,PolicyBlocked,ValueError,KeyError,TypeError,AttributeError,UnicodeError,RecursionError) as exc:
            error=getattr(exc,'error_class','API_CONTRACT_ERROR')
            return {'status':error,'error_class':error,'playlist_id':pid,'source':self.platform,'reason':'未能确认歌单元数据，请检查链接、公开范围和平台鉴权；保留已有完整快照'}

    def _spotify_track(self,raw):
        if not isinstance(raw,dict) or raw.get('type','track')!='track' or raw.get('is_local'):raise ValueError('不可用或非音乐曲目')
        sid=raw.get('id') or str(raw.get('uri','')).removeprefix('spotify:track:')
        if not re.fullmatch(r'[A-Za-z0-9]{22}',str(sid)):raise ValueError('Spotify 曲目标识无效')
        album=raw.get('album') or {};artists=[a.get('name','') for a in raw.get('artists',[]) if isinstance(a,dict)]
        return self._track(sid,raw.get('name') or raw.get('title'),artists,album.get('name',''),album.get('id'),_seconds(raw.get('duration_ms',raw.get('duration')),True),source_url='https://open.spotify.com/track/'+sid)

    def _spotify(self,pid,locator):
        credential=self._credential('api.spotify.com')
        if credential:
            try:return self._spotify_api(pid,credential)
            except HttpError as exc:
                if exc.error_class not in ('AUTH_FAILED','NOT_FOUND','API_CONTRACT_ERROR'):raise
                result=self._spotify_public(pid);result['snapshot']['warnings'].append({'error_class':'API_UNAVAILABLE','reason':'Spotify API 不允许当前账号读取此歌单，改读公开可见内容'})
                return result
        return self._spotify_public(pid)

    def _spotify_api(self,pid,credential):
        headers={'Authorization':self._bearer(credential)};market=credential.get('market','') if isinstance(credential,dict) else ''
        if market and not re.fullmatch('[A-Za-z]{2}',market):raise ValueError('Spotify market 无效')
        metadata=self._json('https://api.spotify.com/v1/playlists/'+pid,headers=headers,params={'market':market} if market else {})
        if metadata.get('id')!=pid:raise HttpError('API_CONTRACT_ERROR','Spotify 歌单身份未通过')
        songs=[];warnings=[];observed=0;total=None;offset=0;seen=set();exhausted=False
        for _ in range(MAX_PAGES):
            try:page=self._json('https://api.spotify.com/v1/playlists/'+pid+'/items',headers=headers,params={'limit':50,'offset':offset,**({'market':market} if market else {})})
            except HttpError as exc:
                if not observed:raise
                warnings.append({'error_class':exc.error_class,'reason':'读取后续分页失败，保留已确认曲目'});break
            items=page.get('items');count=_count(page.get('total'))
            if not isinstance(items,list) or count is None or page.get('offset',offset)!=offset:raise HttpError('API_CONTRACT_ERROR','Spotify 分页字段改变')
            if total is None:total=count
            elif total!=count:warnings.append({'error_class':'SOURCE_CHANGED'});break
            if observed+len(items)>MAX_TRACKS:break
            observed+=len(items)
            for item in items:
                try:songs.append(self._spotify_track(item.get('item') if isinstance(item,dict) and 'item' in item else item.get('track') if isinstance(item,dict) else None))
                except (ValueError,TypeError,KeyError):warnings.append({'error_class':'UNAVAILABLE_ITEM','position':len(songs)})
            next_url=page.get('next')
            if not next_url:exhausted=True;break
            next_parts=urlsplit(str(next_url));expected='/v1/playlists/'+pid+'/items'
            if next_parts.scheme!='https' or next_parts.hostname!='api.spotify.com' or next_parts.path!=expected or next_parts.port not in (None,443) or next_parts.username or next_parts.password:raise HttpError('ACCESS_BLOCKED','Spotify 分页地址改变')
            query=parse_qs(next_parts.query);new_offset=query.get('offset',[''])[0]
            if not re.fullmatch(r'\d+',new_offset) or int(new_offset)<=offset or next_url in seen:raise HttpError('API_CONTRACT_ERROR','Spotify 分页未前进')
            seen.add(next_url);offset=int(new_offset)
        if metadata.get('snapshot_id'):
            try:
                check=self._json('https://api.spotify.com/v1/playlists/'+pid,headers=headers,params={'fields':'id,snapshot_id'})
                if check.get('id')!=pid or check.get('snapshot_id')!=metadata['snapshot_id']:exhausted=False;warnings.append({'error_class':'SOURCE_CHANGED'})
            except HttpError as exc:exhausted=False;warnings.append({'error_class':exc.error_class,'reason':'未完成读取后的歌单版本核对'})
        return self._result(pid,metadata.get('name'),songs,total,exhausted,warnings,observed)

    def _spotify_public(self,pid):
        response=self._request('https://open.spotify.com/embed/playlist/'+pid)
        state=next((value for attrs,value in _json_scripts(response.text) if '__NEXT_DATA__' in attrs),None)
        try:entity=state['props']['pageProps']['state']['data']['entity']
        except (TypeError,KeyError):raise HttpError('API_CONTRACT_ERROR','Spotify 公开页面未提供可读清单') from None
        if entity.get('uri') not in (None,'spotify:playlist:'+pid):raise HttpError('API_CONTRACT_ERROR','Spotify 页面歌单身份不符')
        rows=entity.get('trackList')
        if not isinstance(rows,list):raise HttpError('API_CONTRACT_ERROR','Spotify 公开清单结构改变')
        total=next((_count(entity.get(key)) for key in ('totalTracks','trackCount','total') if _count(entity.get(key)) is not None),None);songs=[]
        for row in rows[:MAX_TRACKS]:
            raw=dict(row);raw['name']=row.get('title') or row.get('name');raw['artists']=row.get('artists') or ([{'name':row['subtitle']}] if _text(row.get('subtitle')) else [])
            try:songs.append(self._spotify_track(raw))
            except (ValueError,TypeError,KeyError):continue
        # Embed is a preview product, not a supported complete-manifest endpoint.
        return self._result(pid,entity.get('name') or entity.get('title'),songs,total,False,[{'error_class':'PUBLIC_PREVIEW','reason':'Spotify 公开嵌入清单不提供可信完整分页；可填写授权 Token 或导入导出清单'}],len(rows),'public')

    def _applemusic(self,pid,locator):
        store=locator.get('storefront','cn')
        if not isinstance(store,str) or not re.fullmatch(r'[a-z]{2}',store):raise ValueError('Apple Music 地区代码无效')
        credential=self._credential('api.music.apple.com')
        if credential:
            try:return self._apple_api(pid,store,credential)
            except HttpError as exc:
                if exc.error_class not in ('AUTH_FAILED','NOT_FOUND'):raise
                result=self._apple_public(pid,store,locator.get('url',''));result['snapshot']['warnings'].append({'error_class':'API_UNAVAILABLE','reason':'Apple Music Developer Token 未通过，改读公开可见内容'});return result
        return self._apple_public(pid,store,locator.get('url',''))

    def _apple_api(self,pid,store,credential):
        headers={'Authorization':self._bearer(credential,'developer_token')};base='https://api.music.apple.com';prefix='/v1/catalog/'+store+'/playlists/'+pid
        first=self._json(base+prefix,headers=headers);records=first.get('data')
        if not isinstance(records,list) or len(records)!=1 or records[0].get('id')!=pid:raise HttpError('API_CONTRACT_ERROR','Apple Music 歌单身份未通过')
        playlist=records[0];page=playlist.get('relationships',{}).get('tracks',{});songs=[];warnings=[];observed=0;seen=set();exhausted=False;total=_count(page.get('meta',{}).get('total'))
        if total is None:total=_count(playlist.get('attributes',{}).get('trackCount'))
        for _ in range(MAX_PAGES):
            rows=page.get('data')
            if not isinstance(rows,list):raise HttpError('API_CONTRACT_ERROR','Apple Music 清单结构改变')
            if observed+len(rows)>MAX_TRACKS:break
            observed+=len(rows)
            for row in rows:
                attrs=row.get('attributes') or {}
                if row.get('type')!='songs' or not re.fullmatch(r'[1-9]\d{0,19}',str(row.get('id',''))):warnings.append({'error_class':'UNAVAILABLE_ITEM'});continue
                albums=row.get('relationships',{}).get('albums',{}).get('data') or []
                songs.append(self._track(row['id'],attrs.get('name'),[attrs.get('artistName')],attrs.get('albumName'),albums[0].get('id') if albums else None,_seconds(attrs.get('durationInMillis'),True),source_url='https://music.apple.com/'+store+'/song/'+row['id']))
            next_path=page.get('next')
            if not next_path:exhausted=True;break
            next_url=urljoin(base,str(next_path));parts=urlsplit(next_url)
            if parts.scheme!='https' or parts.hostname!='api.music.apple.com' or parts.path not in (prefix+'/tracks',prefix+'/relationships/tracks') or parts.port not in (None,443) or parts.username or parts.password or next_url in seen:raise HttpError('ACCESS_BLOCKED','Apple Music 分页地址改变')
            seen.add(next_url)
            try:page=self._json(next_url,headers=headers)
            except HttpError as exc:warnings.append({'error_class':exc.error_class,'reason':'读取后续分页失败，保留已确认曲目'});break
            new_total=_count(page.get('meta',{}).get('total'))
            if total is None:total=new_total
            elif new_total is not None and new_total!=total:exhausted=False;warnings.append({'error_class':'SOURCE_CHANGED'});break
        # Apple catalog relationships may omit totals. Exhausting next is a
        # supported completeness signal, unlike an HTML preview's first page.
        if total is None and exhausted:total=observed
        return self._result(pid,playlist.get('attributes',{}).get('name'),songs,total,exhausted,warnings,observed)

    def _apple_public(self,pid,store,public_url=''):
        pattern='/'+store+r'/playlist/(?:[^/?#]+/)?'+re.escape(pid)+r'/?';parts=urlsplit(public_url)
        public_url=public_url if parts.scheme=='https' and parts.hostname=='music.apple.com' and parts.port in (None,443) and not parts.username and not parts.password and not parts.query and not parts.fragment and re.fullmatch(pattern,parts.path) else 'https://music.apple.com/'+store+'/playlist/'+pid
        response=self._request(public_url,public_redirect=pattern);scripts=_json_scripts(response.text);schema=None;sections=[]
        for attrs,value in scripts:
            if 'ld+json' in attrs:
                for node in _walk(value):
                    if node.get('@type')=='MusicPlaylist' and pid in str(node.get('url','')):schema=node;break
            if 'serialized-server-data' in attrs:
                for node in _walk(value):
                    if node.get('id')=='track-list - '+pid and isinstance(node.get('items'),list):sections.append(node)
        if schema is None:raise HttpError('API_CONTRACT_ERROR','Apple Music 页面没有可核对的歌单清单')
        total=_count(schema.get('numTracks'));songs=[];observed=0
        if sections:
            rows=sections[0]['items'];observed=len(rows)
            for row in rows[:MAX_TRACKS]:
                descriptor=row.get('contentDescriptor') or {};sid=descriptor.get('identifiers',{}).get('storeAdamID')
                if descriptor.get('kind')!='song' or not re.fullmatch(r'[1-9]\d{0,19}',str(sid)):continue
                artists=[];album='';aid=None
                for link in row.get('subtitleLinks') or []:
                    content=link.get('segue',{}).get('destination',{}).get('contentDescriptor',{})
                    if content.get('kind')=='artist':artists.append(link.get('title'))
                for link in row.get('tertiaryLinks') or []:
                    content=link.get('segue',{}).get('destination',{}).get('contentDescriptor',{})
                    if content.get('kind')=='album':album=link.get('title');aid=content.get('identifiers',{}).get('storeAdamID');break
                songs.append(self._track(sid,row.get('title'),artists or [row.get('artistName')],album,aid,_seconds(row.get('duration'),True),source_url='https://music.apple.com/'+store+'/song/'+str(sid)))
        else:
            rows=schema.get('track') or [];rows=rows.get('itemListElement',[]) if isinstance(rows,dict) else rows
            if not isinstance(rows,list):raise HttpError('API_CONTRACT_ERROR','Apple Music 页面曲目结构改变')
            observed=len(rows)
            for row in rows[:MAX_TRACKS]:
                row=row.get('item',row);url=row.get('url','');match=re.search(r'/song/(?:[^/?]+/)?(\d+)',url)
                if not match:continue
                artist=row.get('byArtist') or [];artist=artist if isinstance(artist,list) else [artist];album=row.get('inAlbum') or {}
                songs.append(self._track(match[1],row.get('name'),[a.get('name') for a in artist if isinstance(a,dict)],album.get('name'),None,_seconds(row.get('duration')),source_url='https://music.apple.com/'+store+'/song/'+match[1]))
        return self._result(pid,schema.get('name'),songs,total,total is not None and observed==total,[],observed,'public')

    def _youtube(self,pid,locator):
        credential=self._credential('www.googleapis.com')
        return self._youtube_api(pid,credential) if credential else self._youtube_public(pid,locator)

    def _youtube_auth(self,credential):
        if isinstance(credential,dict):
            if credential.get('access_token'):return {'Authorization':self._bearer(credential)}
            key=credential.get('api_key')
        elif str(credential).lower().startswith('bearer '):return {'Authorization':self._bearer(credential)}
        else:key=credential
        if not isinstance(key,str) or not re.fullmatch(r'[A-Za-z0-9_-]{10,250}',key):raise HttpError('AUTH_FAILED','YouTube API key 格式无效')
        return {'X-Goog-Api-Key':key}

    def _youtube_api(self,pid,credential):
        headers=self._youtube_auth(credential);base='https://www.googleapis.com/youtube/v3/'
        first=self._json(base+'playlists',headers=headers,params={'part':'snippet,contentDetails','id':pid,'maxResults':1});playlists=first.get('items')
        if not isinstance(playlists,list) or len(playlists)!=1 or playlists[0].get('id')!=pid:raise HttpError('NOT_FOUND','YouTube 歌单未公开或不存在')
        playlist=playlists[0];total=_count(playlist.get('contentDetails',{}).get('itemCount'));songs=[];warnings=[];observed=0;token='';seen=set();exhausted=False
        for _ in range(MAX_PAGES):
            try:page=self._json(base+'playlistItems',headers=headers,params={'part':'snippet,contentDetails','playlistId':pid,'maxResults':50,**({'pageToken':token} if token else {})})
            except HttpError as exc:
                if not observed:raise
                warnings.append({'error_class':exc.error_class,'reason':'读取后续分页失败，保留已确认曲目'});break
            items=page.get('items')
            if not isinstance(items,list):raise HttpError('API_CONTRACT_ERROR','YouTube 分页结构改变')
            if observed+len(items)>MAX_TRACKS:break
            page_total=_count(page.get('pageInfo',{}).get('totalResults'))
            if total is None:total=page_total
            elif page_total is not None and page_total!=total:warnings.append({'error_class':'SOURCE_CHANGED'});break
            observed+=len(items);details={};ids=[]
            for item in items:
                sid=item.get('contentDetails',{}).get('videoId') or item.get('snippet',{}).get('resourceId',{}).get('videoId')
                if re.fullmatch(r'[A-Za-z0-9_-]{11}',str(sid)):ids.append(sid)
            if ids:
                videos=self._json(base+'videos',headers=headers,params={'part':'snippet,contentDetails','id':','.join(dict.fromkeys(ids))})
                if not isinstance(videos.get('items'),list):raise HttpError('API_CONTRACT_ERROR','YouTube 视频资料结构改变')
                details={v.get('id'):v for v in videos['items']}
            for item in items:
                snippet=item.get('snippet') or {};sid=item.get('contentDetails',{}).get('videoId') or snippet.get('resourceId',{}).get('videoId');video=details.get(sid)
                if not video:warnings.append({'error_class':'UNAVAILABLE_ITEM','reason':'私有、已删除或不可见的视频未转为音乐目标'});continue
                attrs=video.get('snippet') or {};songs.append(self._track(sid,attrs.get('title') or snippet.get('title'),(),'',None,_seconds(video.get('contentDetails',{}).get('duration')),source_uploader=attrs.get('channelTitle',''),source_url='https://www.youtube.com/watch?v='+sid))
            token=page.get('nextPageToken')
            if not token:exhausted=True;break
            if not isinstance(token,str) or len(token)>4000 or token in seen:raise HttpError('API_CONTRACT_ERROR','YouTube 分页未前进')
            seen.add(token)
        return self._result(pid,playlist.get('snippet',{}).get('title'),songs,total,exhausted,warnings,observed)

    def _youtube_public(self,pid,locator):
        host='music.youtube.com' if urlsplit(locator.get('url','')).hostname=='music.youtube.com' else 'www.youtube.com'
        response=self._request('https://'+host+'/playlist',params={'list':pid,'hl':'en'});text=response.text
        data=_assignment(text,r'(?:var\s+)?ytInitialData\s*=\s*') or _assignment(text,r'window\["ytInitialData"\]\s*=\s*')
        cfg={}
        for match in re.finditer(r'ytcfg\.set\(\s*',text):
            try:value=json.JSONDecoder().raw_decode(text[match.end():])[0]
            except (ValueError,RecursionError):continue
            if isinstance(value,dict):cfg.update(value)
        if data is None:
            initial=cfg.get('YTMUSIC_INITIAL_DATA')
            if isinstance(initial,list):data=next((entry.get('data') for entry in initial if isinstance(entry,dict) and isinstance(entry.get('data'),dict)),None)
        if not isinstance(data,dict):raise HttpError('API_CONTRACT_ERROR','YouTube 公开页面未提供可读歌单')
        name='';total=None
        for node in _walk(data):
            for key in ('playlistHeaderRenderer','playlistSidebarPrimaryInfoRenderer','musicDetailHeaderRenderer','musicResponsiveHeaderRenderer'):
                header=node.get(key)
                if not isinstance(header,dict):continue
                name=name or _runs(header.get('title',{}));count_text=_runs(header.get('numVideosText',{}))
                for value in header.get('stats',[]):
                    candidate=_runs(value)
                    if re.search(r'\b(?:videos?|songs?)\b',candidate,re.I):count_text=candidate;break
                count_text=count_text or _runs(header.get('secondSubtitle',{}));match=re.search(r'([0-9][0-9,]*)\s*(?:videos?|songs?|首)',count_text,re.I)
                if match:total=_count(int(match[1].replace(',','')))
        songs=[];observed=0;seen=set();exhausted=False;warnings=[]
        for _ in range(MAX_PAGES):
            rows,continuation=self._youtube_rows(data)
            if observed+len(rows)>MAX_TRACKS:break
            observed+=len(rows)
            for row in rows:
                try:songs.append(self._youtube_row(row))
                except (ValueError,TypeError,KeyError):warnings.append({'error_class':'UNAVAILABLE_ITEM'})
            if not continuation:exhausted=True;break
            key=cfg.get('INNERTUBE_API_KEY');context=cfg.get('INNERTUBE_CONTEXT')
            if continuation in seen or not isinstance(key,str) or not re.fullmatch(r'[A-Za-z0-9_-]{10,250}',key) or not isinstance(context,dict):break
            seen.add(continuation)
            try:data=self._json('https://'+host+'/youtubei/v1/browse',params={'key':key},payload={'context':context,'continuation':continuation})
            except HttpError as exc:warnings.append({'error_class':exc.error_class,'reason':'公开页面续页不可用，保留可见曲目'});break
        if not songs and total!=0:raise HttpError('API_CONTRACT_ERROR','YouTube 页面未包含有效曲目')
        return self._result(pid,name,songs,total,exhausted,warnings,observed,'public')

    def _youtube_rows(self,data):
        scopes=[]
        for node in _walk(data):
            for key in ('playlistVideoListRenderer','musicPlaylistShelfRenderer','musicPlaylistShelfContinuation'):
                if isinstance(node.get(key),dict):scopes.append(node[key])
            for key in ('appendContinuationItemsAction','reloadContinuationItemsCommand'):
                if isinstance(node.get(key),dict):scopes.append(node[key])
        rows=[];continuation=None
        for scope in scopes:
            for entry in scope.get('contents',scope.get('continuationItems',[])):
                if not isinstance(entry,dict):continue
                if 'playlistVideoRenderer' in entry:rows.append(entry['playlistVideoRenderer'])
                elif 'musicResponsiveListItemRenderer' in entry:rows.append(entry['musicResponsiveListItemRenderer'])
                elif 'continuationItemRenderer' in entry:
                    for node in _walk(entry):
                        if isinstance(node.get('continuationCommand'),dict):continuation=node['continuationCommand'].get('token')
            for entry in scope.get('continuations',[]):
                if isinstance(entry,dict):continuation=entry.get('nextContinuationData',{}).get('continuation') or continuation
        if continuation is not None and (not isinstance(continuation,str) or len(continuation)>10000):raise HttpError('API_CONTRACT_ERROR','YouTube 续页标识无效')
        return rows,continuation

    def _youtube_row(self,row):
        sid=row.get('videoId') or row.get('playlistItemData',{}).get('videoId');title=_runs(row.get('title',{}));artists=[];album='';aid=None;duration=_seconds(row.get('lengthSeconds')) or _seconds(_runs(row.get('lengthText',{})))
        columns=row.get('flexColumns') or []
        if columns:
            for index,column in enumerate(columns):
                text=column.get('musicResponsiveListItemFlexColumnRenderer',{}).get('text',{})
                if index==0:title=_runs(text)
                for run in text.get('runs',[]):
                    nav=run.get('navigationEndpoint',{});watch=nav.get('watchEndpoint',{});sid=sid or watch.get('videoId');browse=nav.get('browseEndpoint',{});kind=browse.get('browseEndpointContextSupportedConfigs',{}).get('browseEndpointContextMusicConfig',{}).get('pageType')
                    if kind=='MUSIC_PAGE_TYPE_ARTIST':artists.append(run.get('text'))
                    elif kind=='MUSIC_PAGE_TYPE_ALBUM' or str(browse.get('browseId','')).startswith('MPRE'):album=run.get('text');aid=browse.get('browseId')
            for column in row.get('fixedColumns') or []:duration=duration or _seconds(_runs(column.get('musicResponsiveListItemFixedColumnRenderer',{}).get('text',{})))
        if not re.fullmatch(r'[A-Za-z0-9_-]{11}',str(sid)):raise ValueError('YouTube 视频标识无效')
        return self._track(sid,title,artists,album,aid,duration,source_uploader=_runs(row.get('shortBylineText',{})),source_url='https://www.youtube.com/watch?v='+sid)

    def _bilibili(self,pid,locator):
        if locator.get('kind','favorite')!='favorite':raise HttpError('UNSUPPORTED_PLAYLIST','当前支持 bilibili 收藏夹直链；合集和音频菜单请导出歌单文件后导入')
        credential=self._credential('api.bilibili.com');headers={'Referer':'https://www.bilibili.com/'}
        if credential:
            cookies=credential.get('cookies') if isinstance(credential,dict) else credential
            if not isinstance(cookies,list):raise HttpError('AUTH_FAILED','Bilibili 登录信息需要 Cookie-Editor JSON 数组')
            try:headers['Cookie']=cookie_header(json.dumps(cookies),'api.bilibili.com','/x/v3/fav/resource/list')
            except ValueError:raise HttpError('AUTH_FAILED','Bilibili Cookie 与接口域名不匹配') from None
        songs=[];observed=0;total=None;name='';exhausted=False;warnings=[];seen=set()
        for page_number in range(1,MAX_PAGES+1):
            try:page=self._json('https://api.bilibili.com/x/v3/fav/resource/list',headers=headers,params={'media_id':pid,'pn':page_number,'ps':20,'platform':'web','order':'mtime','type':0})
            except HttpError as exc:
                if not observed:raise
                warnings.append({'error_class':exc.error_class,'reason':'读取后续分页失败，保留已确认曲目'});break
            if page.get('code') in (-101,-403,-404):raise HttpError('AUTH_FAILED','Bilibili 收藏夹不公开或登录已失效')
            if page.get('code') in (-412,-509):raise HttpError('RATE_LIMITED','Bilibili 访问受限，请稍后重试')
            if page.get('code')!=0 or not isinstance(page.get('data'),dict):raise HttpError('API_CONTRACT_ERROR','Bilibili 收藏夹接口未通过')
            data=page['data'];info=data.get('info') or {};rows=data.get('medias')
            if rows is None and info.get('media_count')==0:rows=[]
            if str(info.get('id'))!=pid or not isinstance(rows,list):raise HttpError('API_CONTRACT_ERROR','Bilibili 收藏夹身份或分页结构改变')
            count=_count(info.get('media_count'))
            if total is None:total=count;name=info.get('title','')
            elif count!=total:warnings.append({'error_class':'SOURCE_CHANGED'});break
            fingerprint=tuple(str(row.get('bvid') or row.get('id')) for row in rows)
            if rows and fingerprint in seen:warnings.append({'error_class':'PAGINATION_REPEATED'});break
            seen.add(fingerprint)
            if observed+len(rows)>MAX_TRACKS:break
            observed+=len(rows)
            for row in rows:
                sid=row.get('bvid') or row.get('id');title=_text(row.get('title'))
                if row.get('type',2)!=2 or not title or title in ('已失效视频','视频已失效') or not re.fullmatch(r'(?:BV[A-Za-z0-9]{10}|[1-9]\d{0,19})',str(sid)):warnings.append({'error_class':'UNAVAILABLE_ITEM'});continue
                songs.append(self._track(sid,title,(),'',None,_seconds(row.get('duration')),source_uploader=(row.get('upper') or {}).get('name',''),source_url='https://www.bilibili.com/video/'+(str(sid) if str(sid).startswith('BV') else 'av'+str(sid))))
            if data.get('has_more') is False:exhausted=True;break
            if not rows or data.get('has_more') is not True:break
        return self._result(pid,name,songs,total,exhausted,warnings,observed)
