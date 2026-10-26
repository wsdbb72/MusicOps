"""Cover identity comes from an album ID, never an unrelated search thumbnail."""
import re
from urllib.parse import urlsplit,urljoin
import httpx
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.media.resources import validate_cover
from musicops.media.chinese import alias_key
from musicops import __version__

class Artwork(bytes):
    def __new__(cls,data,**provenance):
        item=super().__new__(cls,data);item.provenance=provenance;return item

def _image(url,min_side,allowed):
    with httpx.Client(verify=True,follow_redirects=False,timeout=15) as client:
        for _ in range(5):
            u=urlsplit(url)
            if u.scheme!='https' or u.username or u.password or not u.hostname or not allowed(u.hostname):raise PolicyBlocked('封面来源或重定向不受支持')
            with client.stream('GET',url,headers={'User-Agent':'MusicOps/'+__version__+' (personal music library)'}) as r:
                if r.status_code in (301,302,303,307,308):url=urljoin(url,r.headers.get('location',''));continue
                if r.status_code==404:return None
                if r.status_code!=200:raise HttpError('NETWORK_ERROR','专辑封面暂不可用')
                data=bytearray()
                for block in r.iter_bytes():
                    if len(data)+len(block)>10_000_000:raise HttpError('INVALID_PAYLOAD','封面过大')
                    data.extend(block)
            if validate_cover(bytes(data),min_side)['valid']:return bytes(data)
            return None
    raise PolicyBlocked('封面重定向次数过多')

def cover(album,artists,album_id=None,release_id=None,min_side=300,verified_album_id=False,*,release_group_id=None):
    # Local-library repairs have no playlist ID. Search the album API, requiring
    # one exact album + album-artist identity before accepting its artwork URL.
    if not album_id and album and artists:
        try:
            with httpx.Client(verify=True,follow_redirects=False,timeout=15) as client:
                response=client.get('https://music.163.com/api/search/get',params={'s':album+' '+' '.join(artists),'type':10,'limit':30,'offset':0},headers={'Referer':'https://music.163.com/','User-Agent':'MusicOps (personal music library)'})
            if response.status_code==200:
                data=response.json();matches={}
                for item in data.get('result',{}).get('albums',[]):
                    names=item.get('artists') or [item.get('artist',{})]
                    if (alias_key(item.get('name'))==alias_key(album) and {alias_key(a) for a in artists}=={alias_key(a.get('name')) for a in names}
                        and re.fullmatch(r'[1-9]\d{0,18}',str(item.get('id','')))):matches[str(item['id'])]=item
                if len(matches)==1:album_id=next(iter(matches))
        except (httpx.HTTPError,ValueError,TypeError,AttributeError):pass
    if album_id and re.fullmatch(r'[1-9]\d{0,18}',str(album_id)):
        try:
            with httpx.Client(verify=True,follow_redirects=False,timeout=15) as client:
                r=client.get('https://music.163.com/api/album/'+str(album_id))
            if r.status_code==200:
                data=r.json().get('album',{});names=[a.get('name') for a in data.get('artists',[])] or [data.get('artist',{}).get('name')]
                # Official song->album IDs remain valid when a guest vocalist
                # differs from the release artist (soundtracks/compilations).
                if alias_key(data.get('name'))==alias_key(album) and (verified_album_id or {alias_key(a) for a in artists}=={alias_key(a) for a in names}):
                    url=data.get('picUrl','')
                    if url.startswith('http://'):url='https://'+url[7:]
                    artwork=_image(url,min_side,lambda h:h.endswith('.music.126.net') or h.endswith('.music.163.com'))
                    if artwork:return Artwork(artwork,provider='netease',album_id=str(album_id),album=data.get('name'),artists=names,url=url)
        except (httpx.HTTPError,HttpError,PolicyBlocked,ValueError,TypeError,AttributeError):pass
    if release_id and re.fullmatch(r'[0-9a-fA-F-]{36}',str(release_id)):
        url='https://coverartarchive.org/release/'+str(release_id)+'/front'
        data=_image(url,min_side,lambda h:h in ('coverartarchive.org','archive.org') or h.endswith('.archive.org'))
        return Artwork(data,provider='coverartarchive',release_id=str(release_id),url=url) if data else None
    if release_group_id and re.fullmatch(r'[0-9a-fA-F-]{36}',str(release_group_id)):
        url='https://coverartarchive.org/release-group/'+str(release_group_id)+'/front'
        data=_image(url,min_side,lambda h:h in ('coverartarchive.org','archive.org') or h.endswith('.archive.org'))
        return Artwork(data,provider='coverartarchive',release_group_id=str(release_group_id),scope='verified_release_group',url=url) if data else None
    return None


def evidence_cover(album,artists,rows,cfg,secret_getter,checkpoint=lambda *_:None):
    """Bounded album-only search; artwork requires a complete track comparison.

    Search thumbnails and relaxed artist spelling alone are never sufficient.
    This does not assign an exact release ID or change the local album identity.
    """
    if cfg['offline'] or not cfg['providers']['netease']['enabled'] or not rows:return None
    from musicops.clients.http import SafeHttpTransport
    from musicops.clients.vault import cookie_header
    from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
    from musicops.media.identity_fields import cover_pairs,cover_artist_names,credits_equal
    settings={**cfg['providers']['netease'],**cfg['netease']}
    settings['credential_ref']=cfg['netease'].get('credential_ref') or cfg['providers']['netease'].get('credential_ref','')
    transport=SafeHttpTransport(['music.163.com'],offline=False)
    def album_key(value):
        # Advisory labels vary between platform album titles. Track titles,
        # performers and every duration must still match below.
        return re.sub(r'\s*[（(]\s*explicit\s*[）)]\s*$','',alias_key(value))
    try:
        headers={'Referer':'https://music.163.com/','Accept':'application/json'}
        if settings['credential_ref']:
            raw=secret_getter(settings['credential_ref'],'music.163.com')
            if raw:headers['Cookie']=cookie_header(raw,'music.163.com','/api/search/get')
        checkpoint('album_cover_search')
        response=transport.request('GET','https://music.163.com/api/search/get',params={'s':album,'type':10,'limit':30,'offset':0},headers=headers,timeout=20,max_response_bytes=5_000_000)
        if response.status_code!=200:return None
        data=response.json()
        if not isinstance(data,dict) or data.get('code')!=200:return None
        candidates=data.get('result',{}).get('albums',[])
        if not isinstance(candidates,list):return None
        candidates={str(item['id']):item for item in candidates[:30] if isinstance(item,dict) and re.fullmatch(r'[1-9]\d{0,18}',str(item.get('id',''))) and album_key(item.get('name'))==album_key(album)}
        candidates={album_id:item for album_id,item in candidates.items() if credits_equal(cover_artist_names(artists),cover_artist_names([value.get('name') for value in item.get('artists',[]) or [item.get('artist',{})]]))}
        if len(candidates)>5:return None
        provider=NetEaseReadOnlyProvider(transport,settings,secret_getter,checkpoint);matches=[]
        for album_id in candidates:
            checkpoint('album_cover_tracklist')
            try:
                songs=provider.album(album_id)
                if cover_pairs(rows,songs):matches.append(album_id)
            except (HttpError,PolicyBlocked,ValueError,TypeError,KeyError):continue
        if len(matches)!=1:return None
        # Fetch the same verified album endpoint, with shared login restricted
        # to music.163.com. Image requests never receive those credentials.
        album_id=matches[0];data=provider._get('/api/album/'+album_id,{}).get('album',{})
        if str(data.get('id'))!=album_id or album_key(data.get('name'))!=album_key(album):return None
        names=[value.get('name') for value in data.get('artists',[]) or [data.get('artist',{})]]
        if not credits_equal(cover_artist_names(artists),cover_artist_names(names)):return None
        url=data.get('picUrl','')
        if url.startswith('http://'):url='https://'+url[7:]
        artwork=_image(url,cfg['lyrics']['min_cover_side'],lambda h:h.endswith('.music.126.net') or h.endswith('.music.163.com'))
        if artwork:return Artwork(artwork,provider='netease',album_id=album_id,scope='all_tracks_and_durations',album=data.get('name'),url=url)
    except (HttpError,PolicyBlocked,OSError,ValueError,TypeError,KeyError,AttributeError):return None
    finally:transport.close()
