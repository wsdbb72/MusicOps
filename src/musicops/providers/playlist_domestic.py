"""Read public playlist metadata directly from five domestic platforms.

Endpoint/field references are recorded in docs/references/domestic-playlists.md.
No audio endpoint, account mutation, third-party proxy, or executable page code.
"""
from __future__ import annotations

import html
import hashlib
import json
import math
import re
import time
from urllib.parse import urlsplit

from musicops.clients.http import HttpError, PolicyBlocked
from musicops.clients.vault import cookie_header
from musicops.domain.identity import version_from_title

HOSTS = {
    'qqmusic': {'c.y.qq.com', 'u.y.qq.com'},
    'qishui': {'beta-luna.douyin.com'},
    'kugou': {'mobilecdn.kugou.com', 'm.kugou.com', 'www.kugou.com', 'pubsongscdn.kugou.com', 'mobiles.kugou.com'},
    'kuwo': {'m.kuwo.cn'},
    'migu': {'app.c.nf.migu.cn'},
}
DOMESTIC_HOSTS = set().union(*HOSTS.values())
NAMES = {'qqmusic':'QQ 音乐','qishui':'汽水音乐','kugou':'酷狗音乐','kuwo':'酷我音乐','migu':'咪咕音乐'}
PAGE_SIZE = {'qqmusic':1000,'qishui':200,'kugou':300,'kuwo':100,'migu':50}
MAX_SONGS = 50000
MAX_PAGES = 1000
USER_AGENT = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36'
# Public H5 client protocol material; not a user token or an account credential.
# Protocol reference: Playlist Out, Copyright (c) 2026 LengxiQwQ, MIT.
# Full permission notice: docs/references/PlaylistOut-LICENSE.txt.
KUGOU_PUBLIC_H5_SIGNING_MATERIAL = 'NVPh5oo715z5DIWAeQlhMDsWXXQV4hwt'


def _kugou_public_signature(params):
    ordered=''.join(str(key)+'='+str(params[key]) for key in sorted(params))
    return hashlib.md5((KUGOU_PUBLIC_H5_SIGNING_MATERIAL+ordered+KUGOU_PUBLIC_H5_SIGNING_MATERIAL).encode()).hexdigest()


def _text(value):
    return html.unescape(value).strip()[:2000] if isinstance(value,str) else ''


def _object(value):return value if isinstance(value,dict) else {}


def _id(value):
    if type(value) is int:value=str(value)
    if not isinstance(value,str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,160}',value) or value in ('0','None'):return None
    return value


def _count(value):
    if type(value) is int and value>=0:return value
    if isinstance(value,str) and re.fullmatch(r'\d{1,9}',value):return int(value)
    return None


def _first_count(data,keys):
    for key in keys:
        value=_count(data.get(key))
        if value is not None:return value
    return None


def _duration(value,scale=1):
    try:
        if isinstance(value,bool):return None
        if isinstance(value,str) and re.fullmatch(r'\d{1,3}:\d{2}(?::\d{2})?',value):
            result=0
            for part in value.split(':'):result=result*60+int(part)
            return result
        result=float(value)/scale
        return result if math.isfinite(result) and 0<result<=86400 else None
    except (TypeError,ValueError,OverflowError):return None


def _artists(value):
    if isinstance(value,list):
        result=[]
        for item in value:
            name=_text(item.get('name') or item.get('artist_name') or item.get('author_name') or item.get('singerName') or item.get('simple_display_name')) if isinstance(item,dict) else _text(item)
            if name and name not in result:result.append(name)
        return result[:64]
    return [_text(a) for a in re.split(r'\s*(?:&|、|;|\|)\s*',value) if _text(a)][:64] if isinstance(value,str) else []


class DomesticPlaylistProvider:
    def __init__(self,platform,transport,config=None,secret_getter=None,checkpoint=None):
        if platform not in HOSTS:raise ValueError('不支持的国内歌单平台')
        self.platform=platform;self.transport=transport;self.config=config or {};self.secret_getter=secret_getter
        self.checkpoint=checkpoint or (lambda *_:None)

    def _request(self,url,params=None,body=None,raw=False):
        parts=urlsplit(url)
        if parts.scheme!='https' or parts.hostname not in HOSTS[self.platform] or parts.port not in (None,443) or parts.username or parts.password:raise PolicyBlocked('歌单请求地址不在平台白名单')
        self.checkpoint(self.platform+'_playlist_page')
        headers={'User-Agent':'Luna/19.1.0 Android' if self.platform=='qishui' else USER_AGENT,'Accept':'application/json','Referer':{'qqmusic':'https://y.qq.com/','qishui':'https://music.douyin.com/','kugou':'https://m.kugou.com/','kuwo':'https://m.kuwo.cn/','migu':'https://music.migu.cn/'}[self.platform]}
        ref=self.config.get('credential_ref')
        if ref:
            try:
                value=self.secret_getter(ref,parts.hostname) if self.secret_getter else None
                if not value:raise ValueError()
                headers['Cookie']=cookie_header(value,parts.hostname,parts.path)
            except (ValueError,RuntimeError):raise HttpError('AUTH_REQUIRED',NAMES[self.platform]+' Cookie 未配置、已过期或不适用于此接口域名') from None
        if body is not None:headers['Content-Type']='application/json; charset=utf-8'
        timeout=self.config.get('timeout',15)
        if type(timeout) not in (int,float) or not math.isfinite(timeout) or not 1<=timeout<=60:timeout=15
        response=self.transport.request('POST' if body is not None else 'GET',url,params=params,headers=headers,content=json.dumps(body,separators=(',',':')).encode() if body is not None else None,timeout=timeout,allow_redirect_response=True,max_response_bytes=20_000_000)
        actual=urlsplit(response.url or url)
        if (actual.scheme,actual.hostname,actual.port,actual.path)!=(parts.scheme,parts.hostname,parts.port,parts.path):raise HttpError('ACCESS_BLOCKED','平台响应地址发生改变；未转发凭据')
        if response.status_code in (401,403):raise HttpError('AUTH_REQUIRED',NAMES[self.platform]+' 要求有效登录或限制此网络访问')
        if response.status_code==404:raise HttpError('NOT_FOUND','歌单不存在、已删除或未公开')
        if response.status_code==429:raise HttpError('RATE_LIMITED',NAMES[self.platform]+' 请求过于频繁，请稍后重试')
        if 300<=response.status_code<400:raise HttpError('ACCESS_BLOCKED','平台重定向需要重新核对链接')
        if response.status_code!=200:raise HttpError('NETWORK_ERROR',NAMES[self.platform]+' 暂时无法读取歌单')
        if len(response.body)>20_000_000:raise HttpError('INVALID_PAYLOAD','歌单响应超过大小上限')
        if raw:
            try:return response.text
            except UnicodeError:raise HttpError('API_CONTRACT_ERROR','平台分享页不是有效 UTF-8 文本') from None
        try:
            text=response.text.strip()
            match=re.fullmatch(r'[A-Za-z_$][\w.$]*\s*\((.*)\)\s*;?',text,re.S)
            data=json.loads(match.group(1) if match else text,parse_constant=lambda _:(_ for _ in ()).throw(ValueError()))
        except (ValueError,UnicodeError):raise HttpError('API_CONTRACT_ERROR',NAMES[self.platform]+' 未返回有效歌单 JSON') from None
        if not isinstance(data,dict):raise HttpError('API_CONTRACT_ERROR','歌单 JSON 结构改变')
        return data

    def normalize(self,raw):
        if not isinstance(raw,dict):raise ValueError('曲目结构无效')
        p=self.platform;album_obj=raw.get('album') if isinstance(raw.get('album'),dict) else {}
        if p=='qqmusic':
            sid=_id(raw.get('mid') or raw.get('songmid') or raw.get('id') or raw.get('songid'))
            title=_text(raw.get('name') or raw.get('songname'));artists=_artists(raw.get('singer'))
            album=_text(album_obj.get('name') or raw.get('albumname'));aid=_id(album_obj.get('mid') or album_obj.get('id') or raw.get('albummid') or raw.get('albumid'));duration=_duration(raw.get('interval'))
        elif p=='qishui':
            entity=raw.get('entity') or {};wrapper=entity.get('track_wrapper') or {}
            raw=wrapper.get('track') or entity.get('track') or raw.get('track') or raw
            if not isinstance(raw,dict):raise ValueError('汽水曲目结构无效')
            sid=_id(raw.get('id'));title=_text(raw.get('name') or raw.get('title'));artists=_artists(raw.get('artists'))
            album_obj=raw.get('album') or {};album=_text(album_obj.get('name'));aid=_id(album_obj.get('id'));duration=_duration(raw.get('duration') or raw.get('duration_ms'),1000)
        elif p=='kugou':
            sid=_id(raw.get('hash') or raw.get('FileHash') or raw.get('filehash') or raw.get('audio_id'))
            title=_text(raw.get('songname') or raw.get('song_name') or raw.get('SongName'));artists=_artists(raw.get('singername') or raw.get('singer_name') or raw.get('singerinfo') or raw.get('authors'))
            filename=_text(raw.get('filename') or raw.get('name'))
            if not title and ' - ' in filename:
                singer,title=filename.split(' - ',1)
                if not artists:artists=_artists(singer)
            title=title or filename
            album=_text(raw.get('album_name') or raw.get('albumname') or (raw.get('albuminfo') or {}).get('name'));aid=_id(raw.get('album_id') or raw.get('albumid'))
            duration=_duration(raw.get('timelen'),1000) or _duration(raw.get('duration'))
        elif p=='kuwo':
            sid=_id(raw.get('rid') or raw.get('id'));title=_text(raw.get('name'));artists=_artists(raw.get('artist'))
            album=_text(raw.get('album'));aid=_id(raw.get('albumid'));duration=_duration(raw.get('duration') or raw.get('songTimeMinutes'))
        else:
            sid=_id(raw.get('contentId') or raw.get('copyrightId') or raw.get('id'));title=_text(raw.get('name') or raw.get('songName'));artists=_artists(raw.get('singerList') or raw.get('singers') or raw.get('singer'))
            albums=raw.get('albumList') or raw.get('albums') or []
            album_obj=albums[0] if isinstance(albums,list) and albums and isinstance(albums[0],dict) else album_obj
            album=_text(raw.get('albumName') or album_obj.get('name') or raw.get('album'));aid=_id(raw.get('albumId') or album_obj.get('id'));duration=_duration(raw.get('duration') or raw.get('length'))
        if not sid:raise ValueError('曲目缺少有效的平台 ID')
        missing=[key for key,value in (('title',title),('artists',artists),('album',album),('duration',duration)) if not value]
        return {'id':sid,'target_id':p+':'+sid,'source':p,'source_song_id':sid,'raw_title':title,'artists':artists,'album':album,'source_album_id':aid,'duration':duration,'metadata_complete':not missing,'metadata_missing_fields':missing,'version':version_from_title(title),'recording_version':version_from_title(title),'evidence_level':'live_readonly','download_eligibility':'UNKNOWN'}

    def _page(self,pid,page,offset,cursor,qq_fallback=False):
        p=self.platform;n=PAGE_SIZE[p]
        if p=='qqmusic':
            if qq_fallback:
                data=self._request('https://u.y.qq.com/cgi-bin/musicu.fcg',body={'comm':{'ct':24,'cv':0},'playlist':{'module':'music.srfDissInfo.DissInfo','method':'CgiGetDiss','param':{'disstid':int(pid),'onlysonglist':int(page>1),'song_begin':offset,'song_num':n}}})
                part=_object(data.get('playlist'))
                if part.get('code')!=0:raise HttpError('AUTH_REQUIRED' if part.get('code') in (10,-1,10004) else 'API_CONTRACT_ERROR','QQ 音乐歌单未公开、需要登录或接口发生变化')
                data=_object(part.get('data'));meta=_object(data.get('dirinfo')) or data
            else:
                data=self._request('https://c.y.qq.com/qzone/fcg-bin/fcg_ucc_getcdinfo_byids_cp.fcg',params={'disstid':pid,'type':1,'json':1,'utf8':1,'onlysong':0,'new_format':1,'format':'json','song_begin':offset,'song_num':n})
                if data.get('code')!=0:raise HttpError('AUTH_REQUIRED' if data.get('code') in (10,-1,10004) else 'API_CONTRACT_ERROR','QQ 音乐歌单未公开、需要登录或接口发生变化')
                cds=data.get('cdlist')
                if not isinstance(cds,list) or not cds:raise HttpError('NOT_FOUND','QQ 音乐歌单不存在或未公开')
                meta=data=cds[0]
            if not isinstance(meta,dict):raise HttpError('API_CONTRACT_ERROR','QQ 音乐歌单元数据结构改变')
            if meta.get('disstid') is not None and str(meta['disstid'])!=pid:raise HttpError('API_CONTRACT_ERROR','QQ 音乐返回了其他歌单')
            return {'items':data.get('songlist'),'total':_first_count(meta,('songnum','total_song_num','song_num','songCount','songcount')),'name':meta.get('dissname') or meta.get('name'),'reported_offset':_count(data.get('song_begin')),'contract':'qqmusic-musicu-v1' if qq_fallback else 'qqmusic-cdinfo-v1'}
        if p=='qishui':
            data=self._request('https://beta-luna.douyin.com/luna/playlist/detail',body={'playlist_id':pid,'cursor':cursor,'count':n})
            if data.get('status_code') not in (None,0):raise HttpError('AUTH_REQUIRED' if data.get('status_code') in (8,1001) else 'API_CONTRACT_ERROR','汽水歌单未公开、需要登录或接口发生变化')
            meta=_object(data.get('playlist'))
            if meta.get('id') is not None and str(meta['id'])!=pid:raise HttpError('API_CONTRACT_ERROR','汽水音乐返回了其他歌单')
            return {'items':data.get('media_resources'),'total':_first_count(meta,('count_tracks',)),'name':meta.get('title') or meta.get('name'),'more':data.get('has_more'),'cursor':str(data.get('next_cursor') or ''),'contract':'qishui-luna-detail-v1'}
        if p=='kuwo':
            data=self._request('https://m.kuwo.cn/newh5app/wapi/api/www/playlist/playListInfo',params={'pid':pid,'pn':page,'rn':n})
            if data.get('code')!=200:raise HttpError('AUTH_REQUIRED' if data.get('code') in (401,403) else 'API_CONTRACT_ERROR','酷我歌单未公开、需要登录或接口发生变化')
            meta=_object(data.get('data'))
            if meta.get('id') is not None and str(meta['id'])!=pid:raise HttpError('API_CONTRACT_ERROR','酷我返回了其他歌单')
            return {'items':meta.get('musicList'),'total':_first_count(meta,('total','musicNum')),'name':meta.get('name'),'contract':'kuwo-h5-playlist-v1'}
        if p=='migu':
            data=self._request('https://app.c.nf.migu.cn/MIGUM3.0/resource/playlist/song/v2.0',params={'playlistId':pid,'pageNo':page,'pageSize':n})
            if str(data.get('code','000000'))!='000000':raise HttpError('AUTH_REQUIRED' if str(data.get('code')) in ('401','403','100001','000001') else 'API_CONTRACT_ERROR','咪咕歌单未公开、需要登录或接口发生变化')
            meta=_object(data.get('data'))
            return {'items':meta.get('songList'),'total':_first_count(meta,('totalCount',)),'name':meta.get('name') or meta.get('playlistName'),'contract':'migu-migum3-playlist-v2'}
        if qq_fallback:
            data=self._request('https://m.kugou.com/plist/list/'+pid+'/',params={'json':'true','page':page,'pagesize':n})
            outer=_object(data.get('list'));meta=_object(outer.get('list')) or outer
            return {'items':meta.get('info'),'total':_first_count(meta,('total','songcount','count')),'name':meta.get('specialname'),'contract':'kugou-mobile-plist-v1'}
        data=self._request('https://mobilecdn.kugou.com/api/v3/special/song',params={'specialid':pid,'page':page,'pagesize':n,'version':9108,'area_code':1})
        if data.get('status') not in (None,1):raise HttpError('AUTH_REQUIRED','酷狗歌单未公开或需要登录')
        meta=_object(data.get('data'))
        return {'items':meta.get('info'),'total':_first_count(meta,('total','songcount','count')),'name':meta.get('specialname'),'contract':'kugou-special-song-v3'}

    def _kugou_share(self,pid):
        url='https://m.kugou.com/songlist/'+pid+'/' if pid.startswith('gcid_') else 'https://www.kugou.com/yy/special/single/'+pid+'.html'
        text=self._request(url,raw=True);match=re.search(r'window\.\$output\s*=\s*',text)
        if not match:
            # The public desktop page embeds a JSON array; parse data only,
            # without evaluating the surrounding JavaScript.
            match=re.search(r'\bvar\s+data\s*=\s*(?=\[)',text)
            if not match:raise HttpError('AUTH_REQUIRED','酷狗网页未公开完整歌单，请在酷狗导出歌单 JSON；本机不会伪装为已解析全量')
            try:items,_=json.JSONDecoder().raw_decode(text[match.end():].lstrip())
            except ValueError:raise HttpError('API_CONTRACT_ERROR','酷狗网页歌单结构发生变化') from None
            title=re.search(r'<title[^>]*>(.*?)</title>',text,re.S|re.I)
            return {'items':items,'total':None,'name':_text(title.group(1)) if title else '', 'contract':'kugou-desktop-public-preview-v1','more':False}
        try:data,_=json.JSONDecoder().raw_decode(text[match.end():].lstrip())
        except ValueError:raise HttpError('API_CONTRACT_ERROR','酷狗分享页歌单数据发生变化') from None
        if not isinstance(data,dict):raise HttpError('API_CONTRACT_ERROR','酷狗分享页歌单数据结构发生变化')
        info=_object(data.get('info'));meta=_object(info.get('listinfo'))
        collection=_id(data.get('encode_gic'))
        return {'items':info.get('songs'),'total':_first_count(meta,('count','songcount')),'name':meta.get('name'),'contract':'kugou-public-share-preview-v1','more':False,'collection_id':collection if collection and collection.startswith('collection_') else None}

    def _kugou_collection(self,pid,page):
        stamp=str(time.time_ns()//1_000_000)
        params={'srcappid':'2919','clientver':'20000','clienttime':stamp,'mid':stamp,'uuid':stamp,'dfid':'-','uid':'0','appid':'1058','token':'','type':'0','module':'playlist','page':str(page),'pagesize':'100','global_collection_id':pid}
        params['signature']=_kugou_public_signature(params)
        data=self._request('https://pubsongscdn.kugou.com/v2/get_other_list_file',params=params)
        if data.get('status')!=1:raise HttpError('AUTH_REQUIRED','酷狗未公开此合集，或需要登录后导出歌单')
        meta=_object(data.get('data'));name=meta.get('specialname') or meta.get('name')
        if page==1:
            details={'srcappid':'2919','clientver':'20000','clienttime':stamp,'mid':stamp,'uuid':stamp,'dfid':'-','specialid':'0','global_specialid':pid,'sign':'h5'}
            details['signature']=_kugou_public_signature(details)
            try:
                info=self._request('https://mobiles.kugou.com/v5/special/info_v2',params=details)
                name=_object(info.get('data')).get('specialname') or name
            except (HttpError,PolicyBlocked):pass
        return {'items':meta.get('info'),'total':_first_count(meta,('count','total')),'name':name,'contract':'kugou-public-collection-v2'}

    def refresh(self,locator):
        if not isinstance(locator,dict) or locator.get('platform')!=self.platform:raise ValueError('歌单平台不匹配')
        pid=_id(locator.get('playlist_id'))
        if not pid or (self.platform!='kugou' and not pid.isdigit()):raise ValueError('歌单 ID 无效')
        if self.platform=='kugou' and not (pid.isdigit() or re.fullmatch(r'(?:gcid|collection)_[A-Za-z0-9_-]+',pid)):raise ValueError('酷狗歌单 ID 无效')
        snapshot={'playlist_id':pid,'source':self.platform,'name':NAMES[self.platform]+'歌单','songs':[],'complete':False,'declared_count':None,'warnings':[],'contract':'domestic-playlist-v1','evidence_level':'live_readonly','read_only':True}
        cursor='0';offset=0;seen_pages=set();fallback=False;invalid=0;total_changed=False;active_pid=pid
        try:
            for page in range(1,MAX_PAGES+1):
                try:
                    if self.platform=='kugou' and active_pid.startswith('collection_'):
                        try:part=self._kugou_collection(active_pid,page)
                        except (HttpError,PolicyBlocked):
                            if page!=1:raise
                            part=self._kugou_share(pid)
                    elif self.platform=='kugou' and active_pid.startswith('gcid_'):
                        part=self._kugou_share(active_pid)
                        if part.get('collection_id'):
                            try:full=self._kugou_collection(part['collection_id'],page)
                            except (HttpError,PolicyBlocked):pass
                            else:active_pid=part['collection_id'];part=full
                    else:part=self._page(active_pid,page,offset,cursor,fallback)
                except HttpError as exc:
                    if self.platform in ('qqmusic','kugou') and page==1 and pid.isdigit() and exc.error_class in ('NETWORK_ERROR','API_CONTRACT_ERROR','ACCESS_BLOCKED'):
                        fallback=True;part=self._page(pid,page,offset,cursor,True)
                    else:raise
                items=part['items']
                if not isinstance(items,list):raise HttpError('API_CONTRACT_ERROR','平台未返回曲目数组，无法核对歌单')
                if page==1 and not items and part.get('total')==0 and not part.get('name'):raise HttpError('NOT_FOUND','平台返回空壳歌单，无法确认该歌单存在或可见')
                if len(items)>MAX_SONGS or offset+len(items)>MAX_SONGS:raise HttpError('INPUT_TOO_LARGE','单歌单超过 50000 首，已停止分页')
                if part.get('name'):snapshot['name']=_text(part['name'])
                snapshot['contract']=part['contract'];reported=part.get('total')
                if reported is not None:
                    if snapshot['declared_count'] is not None and reported!=snapshot['declared_count']:total_changed=True
                    snapshot['declared_count']=reported
                if page>1 and part.get('reported_offset') not in (None,offset):
                    snapshot['warnings'].append({'error_class':'PAGINATION_STALLED','message':'平台忽略了请求的曲目偏移，不能确认全量'});break
                fingerprint=json.dumps(items,sort_keys=True,ensure_ascii=False,separators=(',',':'))
                if page>1 and fingerprint in seen_pages:
                    snapshot['warnings'].append({'error_class':'PAGINATION_STALLED','message':'平台重复返回先前页面，不能确认全量'});break
                seen_pages.add(fingerprint)
                for item in items:
                    try:snapshot['songs'].append(self.normalize(item))
                    except (ValueError,TypeError,KeyError,AttributeError):invalid+=1
                offset+=len(items);snapshot['page_count']=page;snapshot['returned_count']=offset
                total=snapshot['declared_count']
                if total is not None and offset>=total:
                    if part.get('more') is True:snapshot['warnings'].append({'error_class':'COUNT_CONFLICT','message':'平台声明仍有下一页，但曲目数量已达到总数，需重新解析核对'})
                    break
                if not items or part.get('more') is False:break
                if self.platform=='qishui':
                    nxt=part.get('cursor')
                    if not part.get('more') or not nxt or nxt==cursor:
                        snapshot['warnings'].append({'error_class':'PAGINATION_STALLED','message':'汽水分页游标缺失或未前进'});break
                    cursor=nxt
                elif total is None and len(items)<PAGE_SIZE[self.platform]:break
            if invalid:snapshot['warnings'].append({'error_class':'INCOMPLETE_SONG_METADATA','invalid_items':invalid,'message':'部分曲目缺少有效平台 ID 或结构无效'})
            if total_changed:snapshot['warnings'].append({'error_class':'PLAYLIST_CHANGED','message':'歌单曲目总数在读取期间发生变化，请重新解析'})
            total=snapshot['declared_count']
            snapshot['complete']=total is not None and offset==total and not invalid and not total_changed and not snapshot['warnings']
            if total is None:snapshot['warnings'].append({'error_class':'COUNT_UNKNOWN','message':'平台未声明总曲目数，无法保证已读取完整歌单'})
            elif offset!=total:snapshot['warnings'].append({'error_class':'COUNT_MISMATCH','declared_count':total,'returned_count':offset,'message':'可见曲目数量与平台声明不一致；未伪装为完整歌单'})
            missing=sum(not s['metadata_complete'] for s in snapshot['songs'])
            if missing:snapshot['warnings'].append({'error_class':'METADATA_MISSING','song_count':missing,'message':'已保留可见曲目，缺少的专辑、艺人或时长需后续核对'})
            result={'status':'OK' if snapshot['complete'] else 'SNAPSHOT_INCOMPLETE','snapshot':snapshot,'read_only':True}
            if self.platform=='kugou' and not pid.isdigit() and not snapshot['complete']:result['reason']='酷狗分享页可能只公开前 10 首；完整云端歌单需要平台登录或导入导出文件'
            return result
        except (HttpError,PolicyBlocked) as exc:
            category=getattr(exc,'error_class','POLICY_BLOCKED')
            snapshot['warnings'].append({'error_class':category,'message':str(exc)})
            return {'status':'SNAPSHOT_INCOMPLETE' if snapshot['songs'] else category,'error_class':category,'reason':str(exc),'snapshot':snapshot,'read_only':True}
