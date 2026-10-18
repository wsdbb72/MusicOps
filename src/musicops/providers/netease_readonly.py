"""Read only public playlist manifests and song metadata; no music download API."""
from __future__ import annotations
import json,re,time
from urllib.parse import urlsplit
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.clients.vault import cookie_header
from musicops.providers.netease import build_snapshot
from musicops.domain.identity import version_from_title

def identifier(value):
    if not re.fullmatch(r'[1-9][0-9]{0,18}',str(value)):raise ValueError('网易云ID无效')
    return str(value)

class NetEaseReadOnlyProvider:
    def __init__(self,transport,config=None,secret_getter=None,checkpoint=None):
        self.transport=transport;self.config=config or {};self.secret_getter=secret_getter;self.checkpoint=checkpoint or (lambda *_:None)
        u=urlsplit(self.config.get('base_url','https://music.163.com'))
        if u.scheme!='https' or u.hostname!='music.163.com' or u.port not in (None,443) or u.username or u.password or u.path not in ('','/') or u.query or u.fragment:raise PolicyBlocked('真实网易云只允许已验证origin')
    def _get(self,path,params):
        if path not in ('/api/v3/playlist/detail','/api/v6/playlist/detail','/api/v3/song/detail') and not re.fullmatch(r'/api/album/[1-9][0-9]{0,18}',path):raise PolicyBlocked('端点不在网易云只读白名单')
        headers={'Referer':'https://music.163.com/','Accept':'application/json'}
        ref=self.config.get('credential_ref')
        if ref:
            raw=self.secret_getter(ref,'music.163.com') if self.secret_getter else None
            if not raw:raise HttpError('AUTH_REQUIRED','网易云Cookie引用未配置')
            try:headers['Cookie']=cookie_header(raw,'music.163.com',path)
            except ValueError:raise HttpError('AUTH_FAILED','网易云Cookie已失效或格式不符') from None
        r=self.transport.request('GET','https://music.163.com'+path,params=params,headers=headers,timeout=self.config.get('timeout',20),allow_redirect_response=True,max_response_bytes=10_000_000)
        u=urlsplit(r.url or ('https://music.163.com'+path))
        if u.scheme!='https' or u.hostname!='music.163.com' or u.port not in (None,443) or u.path!=path:raise HttpError('ACCESS_BLOCKED','网易云响应来源改变')
        if r.status_code in (401,403):raise HttpError('AUTH_FAILED','网易云鉴权失败')
        if r.status_code==429:raise HttpError('RATE_LIMITED','网易云限流')
        if r.status_code in (400,413,414):return {'error_class':'PARAM_LIMIT','parameter_limit':True,'status_code':r.status_code}
        if 300<=r.status_code<400:raise HttpError('ACCESS_BLOCKED','网易云重定向未转发凭据')
        if r.status_code!=200:raise HttpError('NETWORK_ERROR','网易云请求失败')
        if len(r.body)>10_000_000:raise HttpError('PARSE_ERROR','网易云响应过大')
        try:data=r.json()
        except ValueError:raise HttpError('API_CONTRACT_ERROR','网易云未返回JSON') from None
        if not isinstance(data,dict):raise HttpError('API_CONTRACT_ERROR','网易云JSON结构改变')
        code=data.get('code')
        if code in (401,403,301):raise HttpError('AUTH_FAILED','网易云需要有效登录')
        if code in (429,509):raise HttpError('RATE_LIMITED','网易云限流')
        if code!=200:raise HttpError('API_CONTRACT_ERROR','网易云业务响应未通过')
        return data
    def album(self,album_id):
        aid=identifier(album_id);data=self._get('/api/album/'+aid,{})
        album=data.get('album') or {};songs=data.get('songs',album.get('songs'))
        count=album.get('size',album.get('trackCount'))
        if str(album.get('id'))!=aid or not isinstance(songs,list) or type(count) is not int or not 1<=count<=2000 or len(songs)!=count:
            raise HttpError('SNAPSHOT_INCOMPLETE','网易云未返回可对账的完整专辑曲目表')
        result=[]
        for i,row in enumerate(songs):
            item_album=row.get('al') or row.get('album') or {}
            normalized={**row,'ar':row.get('ar') or row.get('artists'),'al':{'id':album['id'],'name':album.get('name',''),**item_album},'dt':row.get('dt',row.get('duration'))}
            song=self.normalize_song(normalized)
            if song.get('source_album_id')!=aid or not song.get('duration'):raise HttpError('SNAPSHOT_INCOMPLETE','专辑曲目缺少身份或完整时长')
            song.update(track=str(row.get('no') or i+1),disc=str(row.get('cd') or 1));result.append(song)
        if len({r['source_song_id'] for r in result})!=count:raise HttpError('SNAPSHOT_INCOMPLETE','专辑曲目 ID 重复')
        return result
    def _manifest(self,pid,path='/api/v3/playlist/detail'):
        data=self._get(path,{'id':pid,'n':'10000','s':'0'})
        p=data.get('playlist')
        if not isinstance(p,dict) or identifier(p.get('id'))!=pid or not isinstance(p.get('trackIds'),list) or type(p.get('trackCount')) is not int or p['trackCount']<0:raise HttpError('API_CONTRACT_ERROR','歌单清单结构不符')
        ordered=[identifier(item['id']) for item in p['trackIds']]
        if len(ordered)>50000:raise PolicyBlocked('单歌单超过50000首，需单独确定处理范围')
        return {'ordered_ids':ordered,'declared_count':p['trackCount'],'name':p.get('name',''),'trial_mode':p.get('trialMode'),'track_update_time':p.get('trackUpdateTime'),'endpoint':path}
    @staticmethod
    def normalize_song(raw):
        if not isinstance(raw,dict):raise ValueError('曲目结构不符')
        sid=identifier(raw.get('id'));artists=raw.get('ar');album=raw.get('al');title=raw.get('name')
        missing=[]
        if not isinstance(title,str) or not title.strip():title='';missing.append('title')
        if not isinstance(artists,list) or not artists or not all(isinstance(a,dict) and isinstance(a.get('name'),str) and a['name'].strip() for a in artists):artists=[];missing.append('artists')
        album=album if isinstance(album,dict) else {}
        aid=str(album['id']) if re.fullmatch(r'[1-9][0-9]*',str(album.get('id',''))) else None
        album_name=album.get('name') if isinstance(album.get('name'),str) else ''
        if not aid and not album_name.strip():missing.append('album')
        duration=raw.get('dt')
        if type(duration) not in (int,float) or not 0<=duration<=1_000_000_000_000:duration=None;missing.append('duration')
        return {'id':sid,'target_id':'netease:'+sid,'source':'netease','source_song_id':sid,'raw_title':title,
                'artists':[a['name'] for a in artists],'source_album_id':aid,'album':album_name,
                'duration':duration/1000 if duration is not None else None,'version':version_from_title(title),'recording_version':version_from_title(title),
                'track':str(raw.get('no') or ''),'disc':str(raw.get('cd') or ''),'aliases':raw.get('alia') or [],
                'date_raw':raw.get('publishTime') or None,'date_source':'netease','date_semantics':'unconfirmed',
                'evidence_level':'live_readonly','download_eligibility':'UNKNOWN','metadata_complete':not missing,'metadata_missing_fields':missing}
    def _batch(self,ids):
        self.checkpoint('netease_batch')
        if not getattr(self.transport,'is_fake',False):time.sleep(.35)
        data=self._get('/api/v3/song/detail',{'c':json.dumps([{'id':int(s),'v':0} for s in ids],separators=(',',':'))})
        if data.get('error_class'):return data
        songs=data.get('songs')
        if not isinstance(songs,list):raise HttpError('PARSE_ERROR','曲目详情缺少songs数组')
        normalized=[];item_errors=[]
        for index,song in enumerate(songs):
            try:normalized.append(self.normalize_song(song))
            except (ValueError,KeyError,TypeError):item_errors.append({'item_index':index})
        return {'songs':normalized,'item_errors':item_errors}
    def refresh(self,playlist_id,batch_size=150):
        pid=identifier(playlist_id)
        try:
            self.checkpoint('netease_manifest');first=self._manifest(pid)
            snapshot=build_snapshot(pid,first['ordered_ids'],self._batch,batch_size=batch_size)
            details_complete=snapshot['complete'] and all(s.get('metadata_complete') for s in snapshot['songs']);warnings=[]
            incomplete_metadata=[{'song_id':s['id'],'fields':s['metadata_missing_fields']} for s in snapshot['songs'] if not s.get('metadata_complete')]
            if incomplete_metadata:warnings.append({'error_class':'INCOMPLETE_SONG_METADATA','songs':incomplete_metadata})
            halt=next((f['error_class'] for f in snapshot['failed_batches'] if f['error_class'] in ('AUTH_REQUIRED','AUTH_FAILED','RATE_LIMITED','ACCESS_BLOCKED','POLICY_BLOCKED')),None)
            if halt:
                warnings.append({'error_class':'MANIFEST_RECHECK_SKIPPED','reason':'鉴权、限流或访问策略失败，停止后续请求'})
                snapshot.update(complete=False,visible_catalogue_complete=False,declared_count=first['declared_count'],
                                observed_id_count=len(first['ordered_ids']),manifest_stable=False,metadata_complete=False,
                                trial_mode=first['trial_mode'],warnings=warnings,alternate_manifest={'status':'NOT_REQUESTED'},
                                name=first['name'],contract='netease-public-readonly.v1',evidence_level='live_readonly',read_only=True)
                return {'status':'SNAPSHOT_INCOMPLETE','snapshot':snapshot}
            self.checkpoint('netease_manifest_recheck');last=self._manifest(pid)
            stable=first==last
            if not stable:warnings.append({'error_class':'SOURCE_CHANGED','reason':'分批读取期间歌单清单发生变化'})
            manifest_complete=len(first['ordered_ids'])==first['declared_count'] and len(set(first['ordered_ids']))==len(first['ordered_ids'])
            if not manifest_complete:warnings.append({'error_class':'COUNT_MISMATCH','declared_count':first['declared_count'],'returned_id_count':len(first['ordered_ids'])})
            try:
                alternate=self._manifest(pid,'/api/v6/playlist/detail')
                if alternate['ordered_ids']!=first['ordered_ids'] or alternate['declared_count']!=first['declared_count']:
                    warnings.append({'error_class':'CROSS_ENDPOINT_COUNT_MISMATCH','v3_declared_count':first['declared_count'],'v6_declared_count':alternate['declared_count'],'v6_id_count':len(alternate['ordered_ids'])})
            except (HttpError,ValueError) as exc:
                alternate={'status':getattr(exc,'error_class','PARSE_ERROR')}
                warnings.append({'error_class':'ALTERNATE_MANIFEST_UNVERIFIED','reason':'另一版本清单未核实'})
            snapshot.update(complete=details_complete and manifest_complete and stable and not warnings,
                            visible_catalogue_complete=details_complete and manifest_complete and stable,
                            declared_count=first['declared_count'],observed_id_count=len(first['ordered_ids']),manifest_stable=stable,
                            metadata_complete=details_complete,trial_mode=first['trial_mode'],warnings=warnings,
                            alternate_manifest={k:v for k,v in alternate.items() if k!='ordered_ids'},name=first['name'],
                            contract='netease-public-readonly.v1',evidence_level='live_readonly',read_only=True)
            return {'status':'OK' if snapshot['complete'] else 'SNAPSHOT_INCOMPLETE','snapshot':snapshot}
        except (HttpError,ValueError,KeyError,TypeError) as exc:
            error=getattr(exc,'error_class','PARSE_ERROR');return {'status':error,'error_class':error,'playlist_id':pid,'reason':'歌单读取/解析未通过，保留上次完整快照'}
