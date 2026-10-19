"""Cover identity comes from an album ID, never an unrelated search thumbnail."""
import re
from urllib.parse import urlsplit,urljoin
import httpx
from musicops.clients.http import HttpError,PolicyBlocked
from musicops.domain.identity import strict_text
from musicops.media.resources import validate_cover

def _image(url,min_side,allowed):
    with httpx.Client(verify=True,follow_redirects=False,timeout=15) as client:
        for _ in range(5):
            u=urlsplit(url)
            if u.scheme!='https' or u.username or u.password or not u.hostname or not allowed(u.hostname):raise PolicyBlocked('封面来源或重定向不受支持')
            with client.stream('GET',url,headers={'User-Agent':'MusicOps/0.3.0 (personal music library)'}) as r:
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

def cover(album,artists,album_id=None,release_id=None,min_side=300,verified_album_id=False):
    # Local-library repairs have no playlist ID. Search the album API, requiring
    # one exact album + album-artist identity before accepting its artwork URL.
    if not album_id and album and artists:
        try:
            with httpx.Client(verify=True,follow_redirects=False,timeout=15) as client:
                response=client.get('https://music.163.com/api/search/get',params={'s':album+' '+' '.join(artists),'type':10,'limit':30,'offset':0})
            if response.status_code==200:
                data=response.json();matches={}
                for item in data.get('result',{}).get('albums',[]):
                    names=item.get('artists') or [item.get('artist',{})]
                    if (strict_text(item.get('name'))==strict_text(album) and {strict_text(a) for a in artists}=={strict_text(a.get('name')) for a in names}
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
                if strict_text(data.get('name'))==strict_text(album) and (verified_album_id or {strict_text(a) for a in artists}=={strict_text(a) for a in names}):
                    url=data.get('picUrl','')
                    if url.startswith('http://'):url='https://'+url[7:]
                    artwork=_image(url,min_side,lambda h:h.endswith('.music.126.net') or h.endswith('.music.163.com'))
                    if artwork:return artwork
        except (httpx.HTTPError,HttpError,PolicyBlocked,ValueError,TypeError,AttributeError):pass
    if release_id and re.fullmatch(r'[0-9a-fA-F-]{36}',str(release_id)):
        return _image('https://coverartarchive.org/release/'+str(release_id)+'/front',min_side,lambda h:h in ('coverartarchive.org','archive.org') or h.endswith('.archive.org'))
    return None
