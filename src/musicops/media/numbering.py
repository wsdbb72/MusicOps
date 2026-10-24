"""Positions are metadata evidence, never an arbitrary local file sort."""
import re
from pathlib import Path
from musicops.clients.http import HttpError,PolicyBlocked,SafeHttpTransport
from musicops.media.identity_fields import credits_equal,title_keys


def number_tag(value,maximum=9999):
    match=re.fullmatch(r'\s*(\d{1,5})(?:\s*/\s*(\d{1,5}))?\s*',str(value or ''))
    if not match:return None
    number=int(match[1]);total=int(match[2]) if match[2] else None
    if not 1<=number<=maximum or total is not None and not number<=total<=maximum:return None
    return number,total


def directory_disc(row):
    name=Path(row.get('original_path') or row.get('path') or '').parent.name
    match=re.match(r'^(?:CD|Disc|Disk)[ _-]*0*([1-9]\d*)(?:$|[ ._(-])',name,re.I)
    return int(match[1]) if match and int(match[1])<=999 else None


def declared_disc_total(path):
    import mutagen
    audio=mutagen.File(path);totals=[]
    for key in ('totaldiscs','disctotal','Total Discs'):
        values=(audio.tags or {}).get(key,[]) if audio else []
        if isinstance(values,str):values=[values]
        try:
            for value in values:
                parsed=number_tag(value,999)
                if parsed:totals.append(parsed[0])
        except TypeError:continue
    return max(totals,default=0)


def netease_pairs(rows,cfg,secret_getter,checkpoint=lambda *_:None):
    if cfg['offline'] or not cfg['providers']['netease']['enabled'] or not rows:return []
    from musicops.providers.netease_match import resolve_netease_target
    from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
    settings={**cfg['providers']['netease'],**cfg['netease']}
    settings['credential_ref']=cfg['netease'].get('credential_ref') or cfg['providers']['netease'].get('credential_ref','')
    transport=SafeHttpTransport(['music.163.com'],offline=False)
    try:
        first=rows[0];title=first.get('raw_title','')
        # A redundant numeric prefix can be used as a lookup alias only when
        # its number agrees with the existing tag. It is not blindly deleted.
        aliases=title_keys(first)
        if len(aliases)>1:title=min(aliases,key=len)
        target={**first,'source':'local','raw_title':title}
        reference=resolve_netease_target(target,{'netease':settings},secret_getter,checkpoint,transport)
        songs=NetEaseReadOnlyProvider(transport,settings,secret_getter,checkpoint).album(reference['source_album_id'])
        pairs=[];used=set()
        for row in rows:
            matches=[song for song in songs if song['source_song_id'] not in used
                     and title_keys(row)&title_keys(song) and credits_equal(row.get('artists',[]),song.get('artists',[]))
                     and row.get('duration') and song.get('duration') and abs(row['duration']-song['duration'])<=2]
            if len(matches)!=1:return []
            song=matches[0];track=number_tag(song.get('track'));disc=number_tag(song.get('disc'),999)
            if not track or not disc:return []
            used.add(song['source_song_id'])
            pairs.append((row,{'title':song['raw_title'],'artists':song['artists'],'position':track[0],'disc':disc[0],
                'source_song_id':song['source_song_id'],'source_album_id':song['source_album_id'],'source':'netease'}))
        return pairs
    except (HttpError,PolicyBlocked,OSError,ValueError,KeyError,TypeError):return []
    finally:transport.close()
