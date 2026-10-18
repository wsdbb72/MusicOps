"""Cookie-authenticated torrent retrieval, persisted FL intent, exact hash binding.

Gazelle variants share a parser. Unrecognized buttons or missing free confirmation
require review; never silently fall through to a charged download.
"""
import html,json,re
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urlsplit,parse_qs
from musicops.providers.pt_readonly import PTReadOnlyProvider,positive_id
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.clients.metainfo import parse_metainfo
from musicops.state.store import utcnow

class Links(HTMLParser):
    def __init__(self):super().__init__();self.links=[];self.current=None
    def handle_starttag(self,tag,attrs):
        if tag=='a':self.current={'attrs':dict(attrs),'text':''}
    def handle_data(self,data):
        if self.current:self.current['text']+=data
    def handle_endtag(self,tag):
        if tag=='a' and self.current:self.links.append(self.current);self.current=None

class TorrentFreeStatus(HTMLParser):
    """Read a free label only inside the exact torrent row, never its siblings."""
    def __init__(self,rid):
        super().__init__();self.rid=str(rid);self.active=False;self.rows=0;self.label=None;self.labels=[]
    def handle_starttag(self,tag,attrs):
        attrs=dict(attrs)
        if tag=='tr':
            self.active=attrs.get('id')=='torrent'+self.rid
            if self.active:self.rows+=1
        if self.active and tag=='strong' and 'tl_free' in attrs.get('class','').split():
            self.label={'text':'','title':attrs.get('title','')}
    def handle_data(self,data):
        if self.active and self.label is not None:self.label['text']+=data
    def handle_endtag(self,tag):
        if tag=='strong' and self.label is not None:self.labels.append(self.label);self.label=None
        if tag=='tr':self.active=False;self.label=None
    def status(self):
        if self.rows!=1:return None
        for label in self.labels:
            values={re.sub(r'[\s!！]+','',value).casefold() for value in label.values()}
            if values & {'个人免费','personalfreeleech','personalfree'}:return 'personal'
            if values & {'免费','freeleech','free'}:return 'global'
        return None

class PTDownloadProvider(PTReadOnlyProvider):
    _download_contract=True
    def _contract_allows(self,path,params):
        if self.site=='open.cd':return path in ('/details.php','/plugin_details.php','/download.php') and set(params)<={'id','type','https','passkey'} and bool(params.get('id'))
        return (path=='/ajax.php' and params.get('action') in ('torrent','user') and set(params)<={'action','id'}) or (path=='/torrents.php' and params.get('action') in ('download',None) and set(params)<={'action','id','torrentid','authkey','torrent_pass','usetoken','useToken','type'})
    def detail(self,candidate):
        rid=positive_id(candidate['resource_id'])
        if self.site=='open.cd':return {'page':self._html(self._get('/details.php',{'id':rid})),'torrent':{}}
        data=self._gazelle(self._get('/ajax.php',{'action':'torrent','id':rid}))
        group=data.get('group');torrent=data.get('torrent')
        if not isinstance(group,dict) or not isinstance(torrent,dict) or str(torrent.get('id'))!=rid:raise HttpError('API_CONTRACT_ERROR','种子详情身份不一致')
        return {'group':group,'torrent':torrent}
    def _button(self,page,candidate,free):
        parser=Links();parser.feed(page);rid=str(candidate['resource_id']);found=[]
        for link in parser.links:
            u=urlsplit(html.unescape(link['attrs'].get('href','')))
            if u.scheme and u.scheme!='https' or u.hostname not in (None,self.host):continue
            params=parse_qs(u.query,keep_blank_values=True)
            if any(len(v)!=1 for v in params.values()):continue
            params={k:v[0] for k,v in params.items()};path='/'+u.path.lstrip('/')
            if str(params.get('id',''))!=rid or not self._contract_allows(path,params):continue
            if path=='/download.php' and self.site=='open.cd':found.append((path,params,0));continue
            if path!='/torrents.php' or params.get('action')!='download':continue
            token_keys=set(params)&{'usetoken','useToken'}
            if token_keys and (len(token_keys)!=1 or params[next(iter(token_keys))]!='1'):continue
            token=bool(token_keys)
            if free and not token:found.append((path,params,0))
            elif not free and token:
                text=html.unescape(link['text']+' '+link['attrs'].get('title',''))
                # Gazelle renders FL, FL (2), or an explicit cost in title.
                match=re.search(r'\bFL\s*(?:\((\d+)\)|\[(\d+)\])?',text,re.I)
                if match:
                    cost=int(match.group(1) or match.group(2) or '1')
                    if 1<=cost<=100:found.append((path,params,cost))
        unique={(p,tuple(sorted(v.items())),c) for p,v,c in found}
        if self.site=='open.cd' and len(unique)>1:
            # NexusPHP offers both a session-authenticated button and a passkey
            # share URL for the same torrent. Prefer the actual cookie button.
            plain={entry for entry in unique if set(dict(entry[1]))=={'id'}}
            if len(plain)==1:unique=plain
        if len(unique)!=1:raise PolicyBlocked('下载或 FL 按钮无法唯一确认，请在任务中心核对')
        path,params,cost=next(iter(unique));return path,dict(params),cost
    @staticmethod
    def is_free(torrent):
        value=torrent.get('freeTorrent')
        return value is True or str(value).casefold() in ('1','free','freeleech','personal','personal freeleech') or torrent.get('isFreeleech') is True or torrent.get('isPersonalFreeleech') is True
    def page_free(self,page,candidate):
        # DicMusic's torrent API omits personal FL; its authenticated row does not.
        if self.site!='dicmusic':return None
        parser=TorrentFreeStatus(positive_id(candidate['resource_id']));parser.feed(page)
        return parser.status()
    def _page(self,details,candidate):
        if self.site=='open.cd':return details['page']
        gid=positive_id(details['group'].get('id') or candidate.get('group_id'))
        return self._get('/torrents.php',{'id':gid,'torrentid':str(candidate['resource_id'])}).text
    def fetch(self,candidate,store,batch_id,budget,cache):
        cache=Path(cache);key='pt-request:'+candidate['candidate_id'];previous=store.get('evidence',key)
        reconciliation=None
        if previous:
            if previous.get('state')=='confirmed' and cache.is_file():
                content=cache.read_bytes();meta=parse_metainfo(content)
                if meta['metainfo_sha256']==previous['metainfo_sha256']:return content,previous['free_evidence']
            if self.site!='dicmusic' or previous.get('state')!='unknown':
                raise PolicyBlocked('该资源已有下载请求记录；需要对账，不能再次消耗 FL')
            with store.connection() as connection:
                rows=connection.execute("SELECT payload FROM token_reservations WHERE site=? AND resource_id=? AND state IN ('REQUEST_UNKNOWN','UNKNOWN','REQUESTING')",(self.site,str(candidate['resource_id']))).fetchall()
            if len(rows)!=1:raise PolicyBlocked('该资源请求记录尚不能对账，不能再次消耗 FL')
            reconciliation=json.loads(rows[0][0])
        details=self.detail(candidate);torrent=details.get('torrent',{});page=self._page(details,candidate)
        page_status=self.page_free(page,candidate)
        if reconciliation and page_status!='personal':raise PolicyBlocked('该资源尚未确认个人免费，保留待对账记录；不会重复使用 FL')
        free=self.site=='open.cd' or self.is_free(torrent) or bool(page_status)
        path,params,cost=self._button(page,candidate,free);reservation=reconciliation
        if reconciliation and cost:raise PolicyBlocked('待对账资源不能再次使用 FL')
        if cost:
            account=self._gazelle(self._get('/ajax.php',{'action':'index'}))
            profile=self._gazelle(self._get('/ajax.php',{'action':'user','id':positive_id(account['id'])}))
            balance=profile.get('personal',{}).get('freeleechTokens')
            if type(balance) is not int:balance=profile.get('stats',{}).get('tokens',account.get('userstats',{}).get('tokens'))
            if type(balance) is not int:
                visible=' '.join(html.unescape(re.sub('<[^>]+>',' ',page)).split())
                balances={sum(int(n.replace(',','')) for n in re.split(r'\s*\+\s*',m)) for m in re.findall(r'(?:FL\s*令牌|令牌|Tokens?)\s*[:：]\s*([\d,]+(?:\s*\+\s*[\d,]+)*)',visible,re.I)}
                if len(balances)==1:balance=next(iter(balances))
            if type(balance) is not int:raise PolicyBlocked('无法确认可用 FL 数量，请刷新登录信息或稍后核对')
            if budget is None:
                # Unlimited means no batch cap; the live balance and unresolved
                # reservation guards still apply inside reserve_token.
                with store.connection() as connection:
                    spent=sum(r['cost'] for r in connection.execute(
                        "SELECT cost,payload FROM token_reservations WHERE site=? AND state NOT IN ('RELEASED','REJECTED')",(self.site,)
                    ) if json.loads(r['payload']).get('budget_ref','default')==batch_id)
                budget=spent+balance
            try:reservation=store.reserve_token(self.site,candidate['resource_id'],cost,balance,budget,utcnow(),budget_ref=batch_id)
            except RuntimeError as exc:
                # These are local policy messages, never remote exception text.
                if str(exc).startswith('POLICY_BLOCKED:'):raise PolicyBlocked(str(exc).split(':',1)[1]) from None
                raise
        intent={'kind':'pt_request','state':'requesting','site':self.site,'resource_id':candidate['resource_id'],'batch_id':batch_id,'requested_at':utcnow(),
                'download_method':'direct' if self.site=='open.cd' else ('fl-token' if cost else ('personal-free' if page_status=='personal' else 'free-download')),
                'token_cost':reservation['cost'] if reservation else 0,'reconciled_previous_request':bool(reconciliation)}
        store.put('evidence',key,intent)
        if reservation and not reconciliation:store.token_result(reservation['id'],'REQUESTING')
        try:
            response=self._get(path,params);content=response.body;meta=parse_metainfo(content)
            cache.parent.mkdir(parents=True,exist_ok=True);cache.write_bytes(content)
            if self.site!='open.cd':
                fresh_details=self.detail(candidate);fresh=fresh_details['torrent']
                if not self.is_free(fresh):
                    page_status=self.page_free(self._page(fresh_details,candidate),candidate) if self.site=='dicmusic' else None
                    if not page_status:raise PolicyBlocked('站点未确认 FL 免费生效；暂不启动下载')
                advertised=fresh.get('infoHash') or torrent.get('infoHash')
                if advertised and str(advertised).lower() not in (meta['infohash_v1'],meta['infohash_v2']):raise PolicyBlocked('下载种子与详情哈希不一致')
            evidence={'confirmed':True,'infohash':meta['infohash_v1'] or meta['infohash_v2'],'source':self.site,'resource_id':candidate['resource_id'],'observed_at':utcnow(),
                      'method':'direct' if self.site=='open.cd' else ('torrent-row-'+page_status if page_status else 'free-status-after-download'),
                      'download_method':intent['download_method'],'token_cost':intent['token_cost'],'reconciled_previous_request':bool(reconciliation)}
            cache.parent.mkdir(parents=True,exist_ok=True);cache.write_bytes(content)
            intent.update(state='confirmed',metainfo_sha256=meta['metainfo_sha256'],free_evidence=evidence);store.put('evidence',key,intent)
            if reservation:store.token_result(reservation['id'],'CONFIRMED',evidence)
            return content,evidence
        except Exception:
            intent['state']='unknown';store.put('evidence',key,intent)
            if reservation:store.token_result(reservation['id'],'REQUEST_UNKNOWN',{'reason':'未完成免费与种子对账'})
            raise
