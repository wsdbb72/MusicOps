"""Observed Gazelle/NexusPHP read endpoints. Download eligibility stays unknown."""
from __future__ import annotations
import html,json,re,threading,time
from datetime import datetime,timezone
from html.parser import HTMLParser
from urllib.parse import urlsplit,parse_qs
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.clients.vault import cookie_header

HOSTS={'dicmusic':'dicmusic.com','orpheus':'orpheus.network','open.cd':'www.open.cd'}

def positive_id(value):
    if not re.fullmatch(r'[1-9][0-9]*',str(value)): raise ValueError('资源ID无效')
    return str(value)

class NexusRows(HTMLParser):
    """Extract detail-link identity and visible metadata, never download links."""
    def __init__(self):
        super().__init__();self.rows=[];self.frames=[];self.anchor=None
    def handle_starttag(self,tag,attrs):
        a=dict(attrs)
        if tag=='tr':
            if len(self.frames)>=64:raise ValueError('表格嵌套超过解析上限')
            self.frames.append({'titles':[],'parts':[]})
        if self.frames and tag=='a':
            u=urlsplit(a.get('href',''))
            ids=parse_qs(u.query).get('id',[])
            if not u.scheme and not u.netloc and u.path.lstrip('/') in ('details.php','plugin_details.php') and len(ids)==1 and re.fullmatch(r'[1-9][0-9]*',ids[0]):
                self.anchor={'id':ids[0],'title':a.get('title',''),'text':[]}
    def handle_data(self,data):
        for frame in self.frames:frame['parts'].append(data)
        if self.anchor is not None:self.anchor['text'].append(data)
    def handle_endtag(self,tag):
        if tag=='a' and self.anchor is not None:
            text=' '.join(' '.join(self.anchor['text']).split())
            if text or self.anchor['title']:
                for frame in self.frames:frame['titles'].append({'id':self.anchor['id'],'title':self.anchor['title'] or text})
            self.anchor=None
        if tag=='tr' and self.frames:
            frame=self.frames.pop();titles=frame['titles']
            if titles and len({x['id'] for x in titles})==1:self.rows.append({'identity':titles[0],'text':' '.join(' '.join(frame['parts']).split())})

class PTReadOnlyProvider:
    _guard=threading.Lock();_locks={};_last={};_blocked_until={}
    def __init__(self,site,config,transport,secret_getter):
        if site not in HOSTS:raise ValueError('未知站点')
        self.site,self.config,self.transport,self.secret_getter=site,config,transport,secret_getter
        if getattr(transport,'is_fake',False):self._blocked_until={}
        u=urlsplit(config['base_url'])
        if u.scheme!='https' or u.hostname!=HOSTS[site] or u.port not in (None,443) or u.path not in ('','/') or u.query or u.fragment or u.username or u.password:raise PolicyBlocked('真实PT仅允许已核验站点origin')
        self.host=u.hostname;self.base='https://'+self.host
        with self._guard:self._lock=self._locks.setdefault(site,threading.Lock())
    def capabilities(self):
        return {'status':'OK','source':self.site,'contract':'nexus-readonly.v1' if self.site=='open.cd' else 'gazelle-readonly.v1',
                'search':True,'check_eligibility':False,'fetch_asset':False,'reconcile':False,'reliable_free_confirmation':False,
                'token_required':self.site!='open.cd','reason':'只读登录/检索已接入；令牌成本、免费生效与保种规则仍须确认'}
    def _get(self,path,params=None):
        # Explicit endpoint and action allowlist: a GET alone does not prove safety.
        allowed=(path=='/torrents.php' and self.site=='open.cd' and set(params or {})<= {'search','search_area','search_mode'}) or (path=='/ajax.php' and self.site!='open.cd' and (params or {}).get('action') in ('index','browse') and set(params or {})<= {'action','searchstr','page'})
        if not allowed:raise PolicyBlocked('端点不在只读白名单')
        if self._blocked_until.get(self.site,0)>time.monotonic():raise HttpError('RATE_LIMITED','站点仍在Retry-After等待期')
        ref=self.config.get('credential_ref','')
        raw=self.secret_getter(ref,self.host) if ref else None
        if not raw:raise HttpError('AUTH_REQUIRED','请先导入该站Cookie')
        try:header=cookie_header(raw,self.host,path)
        except ValueError:raise HttpError('AUTH_FAILED','Cookie无效或已过期') from None
        with self._lock:
            if self._blocked_until.get(self.site,0)>time.monotonic():raise HttpError('RATE_LIMITED','站点仍在Retry-After等待期')
            if not getattr(self.transport,'is_fake',False):
                delay=2.1-(time.monotonic()-self._last.get(self.site,0))
                if delay>0:time.sleep(delay)
            try:r=self.transport.request('GET',self.base+path,params=params,headers={'Cookie':header,'Accept':'application/json' if path=='/ajax.php' else 'text/html'},timeout=self.config.get('timeout',15),allow_redirect_response=True,max_response_bytes=10_000_000)
            finally:self._last[self.site]=time.monotonic()
            if r.status_code==429:
                retry=next((v for k,v in r.headers.items() if k.lower()=='retry-after'),'60')
                try:delay=min(1800,max(1,float(retry)))
                except (ValueError,TypeError):delay=60
                self._blocked_until[self.site]=time.monotonic()+delay
                raise HttpError('RATE_LIMITED','站点限流；停止请求')
        if r.url:
            actual=urlsplit(r.url)
            if actual.scheme!='https' or actual.hostname!=self.host or actual.port not in (None,443) or actual.path!=path:raise HttpError('ACCESS_BLOCKED','响应来源或路径改变')
        if 300<=r.status_code<400:
            location=urlsplit(next((v for k,v in r.headers.items() if k.lower()=='location'),''))
            login=location.path=='/login.php' and location.hostname in (None,self.host)
            raise HttpError('AUTH_FAILED' if login else 'ACCESS_BLOCKED','重定向未转发Cookie；请确认登录态')
        if r.status_code in (401,403):raise HttpError('AUTH_FAILED','站点拒绝鉴权')
        if r.status_code!=200:raise HttpError('NETWORK_ERROR','站点请求失败')
        if len(r.body)>10_000_000:raise HttpError('PARSE_ERROR','站点响应超过大小限制')
        return r
    @staticmethod
    def _gazelle(r):
        try:data=r.json()
        except ValueError:raise HttpError('ACCESS_BLOCKED','未取得API JSON；可能为登录/风控页') from None
        if not isinstance(data,dict) or data.get('status')!='success' or not isinstance(data.get('response'),dict):raise HttpError('API_CONTRACT_ERROR','API失败或响应结构改变')
        return data['response']
    @staticmethod
    def _html(r):
        text=r.text
        lower=text.lower()
        if 'cf-chl-' in lower or 'just a moment' in lower:raise HttpError('ACCESS_BLOCKED','站点风控页面')
        if not re.search(r'<a\b[^>]*\bhref\s*=\s*[\"\'](?:https://www\.open\.cd/|/)?logout\.php(?:[?][^\"\']*)?[\"\']',text,re.I):raise HttpError('AUTH_FAILED','未找到已登录证据')
        return text
    def preflight(self):
        try:
            if self.site=='open.cd':
                self._html(self._get('/torrents.php'));balance=None
            else:
                payload=self._gazelle(self._get('/ajax.php',{'action':'index'}))
                if not payload.get('id') or not payload.get('username'):raise ValueError('缺少登录证据')
                stats=payload.get('userstats',{})
                if not isinstance(stats,dict):raise ValueError('账户统计结构改变')
                balance=stats.get('tokens')
                if type(balance) is not int or balance<0:balance=None
            return {'status':'OK','source':self.site,'authenticated':True,'token_balance':balance,'observed_at':datetime.now(timezone.utc).isoformat(),'download_status':'POLICY_BLOCKED','capabilities':self.capabilities()}
        except Exception as exc:return self._failure(exc)
    def search(self,query):
        if not isinstance(query,str) or not query.strip() or len(query)>300:return {'status':'INVALID_INPUT','source':self.site,'candidates':[]}
        try:
            if self.site=='open.cd':
                text=self._html(self._get('/torrents.php',{'search':query.strip(),'search_area':'0','search_mode':'0'}))
                parser=NexusRows();parser.feed(text)
                if not parser.rows and not re.search(r'(没有种子|没有找到|沒有種子|没有匹配|no torrents|no results|nothing found)',text,re.I):raise ValueError('无法确认空结果结构')
                candidates=[self._nexus(row) for row in parser.rows]
                unique={c['candidate_id']:c for c in candidates};candidates=list(unique.values())
                page=1;pages=None
            else:
                data=self._gazelle(self._get('/ajax.php',{'action':'browse','searchstr':query.strip(),'page':'1'}))
                if not isinstance(data.get('results'),list):raise ValueError('缺少results数组')
                candidates=[]
                for group in data['results']:candidates.extend(self._group(group))
                page=data.get('currentPage',1);pages=data.get('pages')
                if type(page) is not int or page!=1 or pages is not None and (type(pages) is not int or pages<1):raise ValueError('分页结构改变')
            return {'status':'OK' if candidates else 'NO_RESOURCE','source':self.site,'candidates':candidates,
                    'page':page,'pages':pages,'search_complete':pages==1,'read_only':True,'download_status':'POLICY_BLOCKED'}
        except Exception as exc:return {**self._failure(exc),'candidates':[]}
    def _base_candidate(self,id,title,artists):
        id=positive_id(id)
        if not isinstance(title,str) or not title.strip():raise ValueError('缺少候选名称')
        return {'schema_version':1,'candidate_id':self.site+':'+id,'source':self.site,'resource_id':id,'torrent_id':id,
                'title':html.unescape(title),'artists':artists,'eligibility':{'state':'UNKNOWN'},'estimated_cost':None,
                'evidence':[{'kind':'authenticated_search','observed_at':datetime.now(timezone.utc).isoformat()}],
                'evidence_level':'live_readonly','requires_review':True,'download_status':'POLICY_BLOCKED','file_list':[]}
    def _group(self,group):
        if not isinstance(group,dict):raise ValueError('分组结构改变')
        if 'torrents' not in group and group.get('torrentId') and isinstance(group.get('category'),str) and group['category'] and group['category'].lower()!='music':
            positive_id(group['torrentId']);positive_id(group.get('groupId'))
            return []  # Gazelle browse includes standalone non-music rows.
        if not isinstance(group.get('torrents'),list):raise ValueError('分组结构改变')
        group_id=positive_id(group.get('groupId'))
        artist=group.get('artist','')
        if not isinstance(artist,str):raise ValueError('艺人字段结构改变')
        out=[]
        for torrent in group['torrents']:
            if not isinstance(torrent,dict):raise ValueError('种子结构改变')
            credits=torrent.get('artists')
            if isinstance(credits,list) and credits:
                if not all(isinstance(a,dict) and isinstance(a.get('name'),str) and a['name'].strip() for a in credits):raise ValueError('多艺人字段结构改变')
                artists=list(dict.fromkeys(html.unescape(a['name']) for a in credits))
            else:artists=[html.unescape(artist)] if artist else []
            c=self._base_candidate(torrent.get('torrentId'),group.get('groupName'),artists)
            c.update(group_id=group_id,release={'album':c['title'],'year':group.get('groupYear'),'edition':html.unescape(str(torrent.get('remasterTitle') or 'unknown')),
                     'remaster_year':torrent.get('remasterYear'),'catalogue_number':torrent.get('remasterCatalogueNumber'),'complete_tracklist':False},
                     media={k:torrent.get(k) for k in ('format','encoding','media','fileCount','size')},seeders=torrent.get('seeders',0),
                     site_flags={k:torrent.get(k) for k in ('isFreeleech','isPersonalFreeleech','isNeutralLeech','canUseToken')})
            # Site flags are visible hints, not authorization or free-download proof.
            out.append(c)
        return out
    def _nexus(self,row):
        c=self._base_candidate(row['identity']['id'],row['identity']['title'],[])
        visible=row['text'];size=re.search(r'([0-9.]+)\s*(TiB|GiB|MiB|KiB|TB|GB|MB|KB)',visible,re.I)
        c.update(release={'complete_tracklist':False,'edition':'unknown'},media={'size_display':size.group(0) if size else None},seeders=None)
        return c
    def _failure(self,exc):
        error=getattr(exc,'error_class','PARSE_ERROR' if isinstance(exc,(ValueError,TypeError,KeyError)) else 'NETWORK_ERROR')
        return {'status':error,'error_class':error,'source':self.site,'reason':'只读预检/解析未通过；未判为无资源','download_status':'POLICY_BLOCKED'}
    def check_eligibility(self,*args,**kwargs):return {'status':'POLICY_BLOCKED','reason':'令牌成本/免费生效/保种规则尚未确认'}
    def fetch_asset(self,*args,**kwargs):return self.check_eligibility()
    def reconcile(self,*args,**kwargs):return self.check_eligibility()
