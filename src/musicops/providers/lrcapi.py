"""Private raw-auth LrcApi and isolated public fallback, with bounded caching."""
from __future__ import annotations
import copy
import hashlib
import json
import math
import re
import threading
import time
from urllib.parse import urlsplit
from musicops.media.resources import validate_cover, validate_lrc

CONTRACT_VERSION = 'lrcapi-legacy-3'
VERSION = re.compile(r'\b(live|remix|remaster(?:ed)?|instrumental|deluxe|acoustic)\b|现场|伴奏|重制',re.I)

def _normalized(value): return re.sub(r'\s+',' ',str(value).strip()).casefold()
def _versions(value): return set(_normalized(x.group()) for x in VERSION.finditer(str(value)))

class _IsolatedTransport:
    """A request client with a fixed host and header policy; redirects are rejected."""
    def __init__(self, transport, host, public=False,max_response_bytes=10_000_000): self.transport,self.host,self.public,self.max_response_bytes=transport,host,public,max_response_bytes
    def request(self,url,params,headers,timeout):
        parsed = urlsplit(url)
        if parsed.hostname != self.host: raise ValueError('HOST_POLICY_BLOCKED')
        if self.public:
            headers = {k:v for k,v in headers.items() if k.lower() not in ('authorization','cookie','sid','proxy-authorization')}
        response = self.transport.request('GET',url,params=params,headers=headers,timeout=timeout,follow_redirects=False,max_response_bytes=self.max_response_bytes)
        if 300 <= response.status_code < 400: raise ValueError('REDIRECT_POLICY_BLOCKED')
        final = urlsplit(getattr(response,'url',url) or url)
        if final.hostname != self.host or final.scheme!=parsed.scheme or final.port!=parsed.port or final.path!=parsed.path or final.username or final.password:
            raise ValueError('REDIRECT_POLICY_BLOCKED')
        return response

class LrcApiClient:
    def __init__(self, config, transport, secret_getter=None, clock=time.monotonic):
        self.config = copy.deepcopy(config.get('lyrics',config))
        self.secret_getter = secret_getter
        if isinstance(transport,dict):
            self.private_transport, self.public_transport = transport['private'],transport['public']
        else: self.private_transport,self.public_transport = transport,transport
        self.clock=clock
        self._lock=threading.RLock()
        self._cache={}
        self._cache_bytes=0
        self._inflight={}
        self._breakers={}
        self._source_locks={name:threading.Semaphore(max(1,int(self.config.get('concurrency',1)))) for name in ('private-group','public')}
        self._auth_revision=None
        self._auth_blocked=False

    def _revision(self,ref):
        if self.secret_getter is None: return 0
        revision = getattr(self.secret_getter,'revision',None)
        if revision is None: revision=getattr(getattr(self.secret_getter,'__self__',None),'revision',None)
        return revision(ref) if revision else self.config.get('credential_revision',0)

    def restore_private(self):
        with self._lock: self._auth_blocked=False; self._breakers.clear()

    def _get_secret(self,ref,host):
        if not self.secret_getter: raise ValueError('CREDENTIAL_REQUIRED')
        value=self.secret_getter(ref,host)
        if not isinstance(value,str) or not value: raise ValueError('CREDENTIAL_REQUIRED')
        if '\r' in value or '\n' in value or '\x00' in value: raise ValueError('INVALID_AUTH_HEADER')
        return value  # Complete raw value, intentionally no strip or Bearer prefix.

    def _sources(self,kind):
        primary=self.config.get('primary',{})
        primary={'base_url':'https://lyrics.example.invalid','auth_secret_ref':'vault://lyrics/default/authorization',**primary}
        key={'candidates':'candidates_path','lyrics':'lyrics_path','cover':'cover_path'}[kind]
        path={'candidates':'/jsonapi','lyrics':'/lyrics','cover':'/cover'}[kind]
        yield ('private-primary',primary['base_url'].rstrip('/')+primary.get(key,path),False,primary)
        backup=self.config.get('direct_backup',{})
        if backup.get('enabled',False):
            yield ('private-backup',backup.get('base_url','http://backup.example.invalid').rstrip('/')+backup.get(key,primary.get(key,path)),False,{**primary,**backup})
        public=self.config.get('public_fallback',{})
        if public.get('enabled',False):
            # Public has no confirmed /jsonapi. Candidates fallback is a single LRC.
            public_kind='lyrics' if kind=='candidates' else kind
            yield ('public',public.get(public_kind+'_url','https://api.lrc.cx/'+public_kind),True,public)

    def _classify(self,response):
        status=response.status_code
        if status in (401,403): return 'AUTH_FAILED'
        if status==429: return 'RATE_LIMITED'
        if status>=500: return 'TRANSIENT_ERROR'
        if status!=200: return 'API_CONTRACT_ERROR'
        return None

    def _identity(self,item,query):
        if not item.get('title') or not item.get('artist'): return 'structural_only'
        if _normalized(item['title']) != _normalized(query['title']): return 'mismatch'
        artists=item['artist'] if isinstance(item['artist'],list) else [item['artist']]
        expected=query['artist'] if isinstance(query['artist'],list) else [query['artist']]
        if {_normalized(a) for a in artists}!={_normalized(a) for a in expected}: return 'mismatch'
        actual_version=(item.get('version') or '')+' '+item.get('title','')
        expected_version=query.get('version','')+' '+query['title']
        if _versions(actual_version)!=_versions(expected_version): return 'mismatch'
        if item.get('album') and query.get('album') and _normalized(item['album'])!=_normalized(query['album']): return 'mismatch'
        return 'matched'

    def _parse(self,kind,response,query,provider,public):
        data=response.body
        if len(data)>min(20_000_000,int(self.config.get('max_response_bytes',10_000_000))): return [],'INVALID_PAYLOAD'
        if kind=='cover':
            validation=validate_cover(data,int(self.config.get('min_cover_side',300)))
            return ([{'schema_version':1,'resource_id':validation.get('sha256','invalid'),'kind':'cover','provider':provider,'content_hash':validation.get('sha256'),'content_bytes':data,'validation':validation,'identity_state':'structural_only','reason':'图片结构通过；发行身份需用户复核','selected':False}] if validation['valid'] else []),None if validation['valid'] else 'INVALID_PAYLOAD'
        try: text=data.decode('utf-8-sig')
        except UnicodeDecodeError: return [],'INVALID_PAYLOAD'
        if kind=='candidates' and not public:
            try: items=json.loads(text)
            except (ValueError,TypeError): return [],'INVALID_PAYLOAD'
            if not isinstance(items,list): return [],'API_CONTRACT_ERROR'
            if not items: return [],'NO_MATCH'
            if any(not isinstance(item,dict) or not {'id','title','artist','lyrics'}<=set(item)
                   or type(item['id']) not in (str,int) or not str(item['id']).strip()
                   or not isinstance(item['title'],str) or not isinstance(item['lyrics'],str)
                   or not (isinstance(item['artist'],str) or isinstance(item['artist'],list) and all(isinstance(a,str) for a in item['artist']))
                   or item.get('version') is not None and not isinstance(item['version'],str)
                   or item.get('album') is not None and not isinstance(item['album'],str) for item in items): return [],'API_CONTRACT_ERROR'
        else:
            if not text.strip(): return [],'INVALID_PAYLOAD'
            items=[{'id':hashlib.sha256(data).hexdigest(),'lyrics':text}]
        candidates=[]
        for item in items:
            validation=validate_lrc(item['lyrics'],query.get('duration'))
            identity=self._identity(item,query)
            candidates.append({'schema_version':1,'resource_id':str(item['id']),'kind':'lyrics','provider':provider,'title':item.get('title'),'artist':item.get('artist'),'album':item.get('album'),'version':item.get('version'),'lyrics':item['lyrics'],'content_hash':validation.get('sha256'),'validation':validation,'identity_state':identity,'selected':False,'reason':'匹配身份与版本' if identity=='matched' else '身份信息不足，需用户复核' if identity=='structural_only' else '标题、艺人、专辑或版本不符'})
        valid=[x for x in candidates if x['validation']['valid'] and x['identity_state']!='mismatch']
        if not valid: return candidates,'MISMATCH' if any(x['identity_state']=='mismatch' for x in candidates) else 'INVALID_PAYLOAD'
        matched=[x for x in valid if x['identity_state']=='matched']
        if len(matched)==1: matched[0]['selected']=True
        return candidates,None

    def query(self,kind,title,artist,album='',version='',duration=None,refresh=False):
        if kind not in ('candidates','lyrics','cover'): raise ValueError('UNKNOWN_RESOURCE_KIND')
        query={'title':title,'artist':artist,'album':album,'version':version,'duration':duration}
        if (not isinstance(title,str) or not title.strip()
                or not (isinstance(artist,str) and artist.strip() or isinstance(artist,list) and artist and all(isinstance(a,str) and a.strip() for a in artist))
                or not isinstance(album,str) or not isinstance(version,str)
                or duration is not None and (type(duration) not in (int,float) or not 0<=duration<=1_000_000_000 or not math.isfinite(duration))):
            return {'schema_version':1,'state':'API_CONTRACT_ERROR','candidates':[],'errors':[{'error_class':'INVALID_QUERY_IDENTITY'}]}
        ref=self.config.get('primary',{}).get('auth_secret_ref','vault://lyrics/private/authorization')
        revision=self._revision(ref)
        key=(CONTRACT_VERSION,kind,_normalized(title),json.dumps(artist,ensure_ascii=False,sort_keys=True),_normalized(album),_normalized(version),duration,revision,json.dumps(self.config,sort_keys=True))
        with self._lock:
            if revision != self._auth_revision: self._auth_revision=revision; self._auth_blocked=False; self._breakers.clear()
            for expired in [k for k,v in self._cache.items() if v[0]<=self.clock()]:
                self._cache_bytes-=self._cache.pop(expired)[2]
            cached=self._cache.get(key)
            if not refresh and cached and cached[0]>self.clock():
                result=copy.deepcopy(cached[1]);result['cache_hit']=True;return result
            flight=self._inflight.get(key)
            if flight is None: flight={'event':threading.Event(),'result':None};self._inflight[key]=flight;owner=True
            else: owner=False
        if not owner:
            if not flight['event'].wait(float(self.config.get('total_budget_seconds',self.config.get('total_budget',60)))+5): return {'schema_version':1,'state':'TRANSIENT_ERROR','candidates':[],'errors':[{'error_class':'SINGLEFLIGHT_TIMEOUT'}]}
            with self._lock:
                result=copy.deepcopy(flight['result']);result['singleflight_join']=True;return result
        try:
            try:result=self._query_uncached(kind,query)
            except Exception as exc:result={'schema_version':1,'state':'API_CONTRACT_ERROR','candidates':[],'errors':[{'error_class':'INVALID_CLIENT_CONFIGURATION','exception_type':type(exc).__name__}]}
            state=result['state']
            ttl=float(self.config.get('cache_ttl',604800)) if state=='VALID_PAYLOAD' else float(self.config.get('negative_ttl',86400)) if state in ('NO_MATCH','MISMATCH') else 1800 if state in ('INVALID_PAYLOAD','API_CONTRACT_ERROR') else 300
            size=sum(len(c.get('content_bytes',b''))+len(c.get('lyrics','').encode('utf-8',errors='replace'))+2048 for c in result['candidates'])+2048
            with self._lock:
                flight['result']=copy.deepcopy(result)
                prior=self._cache.pop(key,None)
                if prior:self._cache_bytes-=prior[2]
                max_entries=min(512,max(1,int(self.config.get('max_cache_entries',128))))
                max_bytes=min(128*1024*1024,max(1,int(self.config.get('max_cache_bytes',32*1024*1024))))
                if size<=max_bytes:
                    while self._cache and (len(self._cache)>=max_entries or self._cache_bytes+size>max_bytes):
                        self._cache_bytes-=self._cache.pop(next(iter(self._cache)))[2]
                    self._cache[key]=(self.clock()+max(0,ttl),copy.deepcopy(result),size);self._cache_bytes+=size
            return result
        finally:
            with self._lock:self._inflight.pop(key)['event'].set()

    def _query_uncached(self,kind,query):
        errors=[];all_candidates=[];started=self.clock();last='NO_MATCH';tls_failed=False
        params={'title':query['title'],'artist':' / '.join(query['artist']) if isinstance(query['artist'],list) else query['artist']}
        if query.get('album'):params['album']=query['album']
        for provider,url,public,source in self._sources(kind):
            if tls_failed and provider=='private-backup':
                errors.append({'provider':provider,'error_class':'TLS_DOWNGRADE_BLOCKED'});continue
            if self.clock()-started>float(self.config.get('total_budget_seconds',self.config.get('total_budget',60))):
                errors.append({'provider':provider,'error_class':'QUERY_BUDGET_EXHAUSTED'});last='TRANSIENT_ERROR';break
            parsed=urlsplit(url)
            if parsed.scheme not in ('http','https') or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment:
                errors.append({'provider':provider,'error_class':'API_CONTRACT_ERROR'});last='API_CONTRACT_ERROR';continue
            if public and parsed.scheme!='https':
                errors.append({'provider':provider,'error_class':'POLICY_BLOCKED'});last='POLICY_BLOCKED';continue
            if provider=='private-primary' and parsed.scheme!='https':
                errors.append({'provider':provider,'error_class':'POLICY_BLOCKED'});last='POLICY_BLOCKED';continue
            if not public and self._auth_blocked:
                errors.append({'provider':provider,'error_class':'AUTH_FAILED'});last='AUTH_FAILED';continue
            breaker=self._breakers.get((provider,kind))
            if breaker and breaker[0]>self.clock():
                last=breaker[1]
                errors.append({'provider':provider,'error_class':'CIRCUIT_OPEN','cause':last,'retry_after_seconds':round(breaker[0]-self.clock(),2)});continue
            headers={'Accept':'image/*' if kind=='cover' else 'application/json' if kind=='candidates' and not public else 'text/plain'}
            try:
                if not public:headers['Authorization']=self._get_secret(source.get('auth_secret_ref','vault://lyrics/private/authorization'),parsed.hostname)
            except Exception as exc:
                errors.append({'provider':provider,'error_class':str(exc) if str(exc) in ('CREDENTIAL_REQUIRED','INVALID_AUTH_HEADER') else 'CREDENTIAL_DOMAIN_MISMATCH'});last='AUTH_FAILED';self._auth_blocked=True
                if not self.config.get('fallback_on_auth_error',False):break
                continue
            client=_IsolatedTransport(self.public_transport if public else self.private_transport,parsed.hostname,public,min(20_000_000,int(self.config.get('max_response_bytes',10_000_000))))
            for attempt in range(min(2,max(1,int(self.config.get('max_attempts',2))))):
                response=None
                try:
                    remaining=float(self.config.get('total_budget_seconds',self.config.get('total_budget',60)))-(self.clock()-started)
                    if remaining<=0:raise TimeoutError('QUERY_BUDGET_EXHAUSTED')
                    source_lock=self._source_locks['public' if public else 'private-group']
                    if not source_lock.acquire(timeout=remaining):raise TimeoutError('QUERY_BUDGET_EXHAUSTED')
                    try:
                        remaining=float(self.config.get('total_budget_seconds',self.config.get('total_budget',60)))-(self.clock()-started)
                        if remaining<=0:raise TimeoutError('QUERY_BUDGET_EXHAUSTED')
                        response=client.request(url,params,headers,(min(remaining,float(self.config.get('connect_timeout',3))),min(remaining,float(self.config.get('read_timeout',12)))))
                    finally:source_lock.release()
                    error=self._classify(response)
                    if error is None:
                        candidates,error=self._parse(kind,response,query,provider,public);all_candidates.extend(candidates)
                        if error is None:return {'schema_version':1,'state':'VALID_PAYLOAD','candidates':all_candidates,'errors':errors,'provider':provider,'cache_hit':False}
                except Exception as exc:
                    name=type(exc).__name__.lower();message=(str(exc)+' '+str(getattr(exc,'__cause__',''))).lower();declared=getattr(exc,'error_class','')
                    error=declared if declared in ('TLS_ERROR','POLICY_BLOCKED','INVALID_PAYLOAD','API_CONTRACT_ERROR','AUTH_FAILED','RATE_LIMITED') else 'TLS_ERROR' if 'ssl' in name or 'tls' in name or 'certificate' in message else 'POLICY_BLOCKED' if 'policy_blocked' in message or 'policyblocked' in name else 'TRANSIENT_ERROR'
                    if error=='TLS_ERROR':tls_failed=True
                last=error
                errors.append({'provider':provider,'error_class':error,'attempt':attempt+1})
                if error=='AUTH_FAILED':
                    self._auth_blocked=True;break
                if error=='RATE_LIMITED':
                    retry=(getattr(response,'headers',{}) or {}).get('Retry-After','60')
                    try:delay=min(1800,max(1,float(retry)))
                    except (TypeError,ValueError):delay=60
                    self._breakers[(provider,kind)]=(self.clock()+delay,error);break
                if error in ('INVALID_PAYLOAD','API_CONTRACT_ERROR'):self._breakers[(provider,kind)]=(self.clock()+1800,error)
                if error!='TRANSIENT_ERROR':break
                if attempt+1>=min(2,max(1,int(self.config.get('max_attempts',2)))):self._breakers[(provider,kind)]=(self.clock()+300,error)
            if last=='AUTH_FAILED' and not self.config.get('fallback_on_auth_error',False):break
        return {'schema_version':1,'state':last,'candidates':all_candidates,'errors':errors,'cache_hit':False}

    def candidates(self,title,artist,album='',**kwargs):return self.query('candidates',title,artist,album,**kwargs)
    def lyrics(self,title,artist,album='',**kwargs):return self.query('lyrics',title,artist,album,**kwargs)
    def cover(self,title,artist,album='',**kwargs):return self.query('cover',title,artist,album,**kwargs)
