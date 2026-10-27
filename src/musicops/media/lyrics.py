"""Player-ready synchronized lyrics for Roon, Navidrome and Plex.

Plex only reads a same-named UTF-8 ``.lrc`` sidecar, Roon only reads lyrics
embedded in file tags (LRC text in LYRICS/©lyr, SYLT for ID3) and Navidrome
reads both. Every writer therefore stores the same normalized timeline in the
sidecar and in the embedded tag, so the active line follows playback.
"""
import re
from pathlib import Path
from musicops.media.resources import STAMP,validate_lrc
from musicops.media.tags import read_tag_fields,apply_tag_changes

KEEP_META=re.compile(r'^\[(ar|al|ti|by|re|ve|length|au):[^\]]*\]$',re.I)
WORD_STAMP=re.compile(r'<\d{1,3}:[0-5]\d(?:[.:]\d{1,3})?>')


def plain_lyrics(text):
    lines=[]
    for line in text.splitlines():
        if re.match(r'^\s*\[(?:ar|al|ti|by|offset|re|ve|length|au|id):',line,re.I):continue
        line=re.sub(r'\[\d+:\d+(?:[.:]\d+)?\]|<\d+:\d+(?:[.:]\d+)?>','',line).strip()
        if line:lines.append(line)
    return '\n'.join(lines)


def _stamp(ms):
    centiseconds=(ms+5)//10
    minutes,remainder=divmod(centiseconds,6000)
    return f'[{minutes:02d}:{remainder//100:02d}.{remainder%100:02d}]'


def player_lrc(text):
    """Return one ``[mm:ss.xx]`` line per lyric, in playback order.

    The ``[offset:]`` value is folded into the timestamps (players disagree on
    its sign or ignore it), repeated-stamp lines are expanded, word-level
    ``<mm:ss.xx>`` stamps and NetEase JSON credit rows are removed, and the
    result keeps translation lines that share a timestamp in source order.
    """
    if not isinstance(text,str):raise ValueError('INVALID_LRC')
    text=text.replace('﻿','').replace('\r\n','\n').replace('\r','\n')
    offsets=re.findall(r'(?im)^\s*\[offset:\s*([+-]?\d{1,12})\s*\]\s*$',text)
    if len(set(offsets))>1:raise ValueError('INVALID_OFFSET')
    offset=int(offsets[0]) if offsets else 0
    meta=[];timed=[]
    for order,raw in enumerate(text.split('\n')):
        line=raw.strip()
        if not line or re.fullmatch(r'\{.*\}',line) or re.fullmatch(r'\[(?:offset|hash|sign|qq|total|id|kana):[^\]]*\]',line,re.I):continue
        if KEEP_META.match(line):
            if not any(m.split(':',1)[0].casefold()==line.split(':',1)[0].casefold() for m in meta):meta.append(line)
            continue
        stamps=[];position=0
        while match:=STAMP.match(line,position):stamps.append(match);position=match.end()
        lyric=WORD_STAMP.sub('',line[position:]).strip()
        if not stamps or not lyric:continue
        for match in stamps:
            fraction=match.group(3) or '0'
            ms=(int(match.group(1))*60+int(match.group(2)))*1000+round(int(fraction)*10**(3-len(fraction)))+offset
            timed.append((max(0,ms),order,lyric))
    timed.sort()
    return '\n'.join(meta+[_stamp(ms)+lyric for ms,_,lyric in timed])


def is_synchronized(text,duration=None):
    return isinstance(text,str) and validate_lrc(text,duration)['valid']


def write_sidecar(path,text,duration=None):
    """Write a Plex/Navidrome sidecar: same stem, UTF-8 without BOM, LF."""
    normalized=player_lrc(text)
    if not is_synchronized(normalized,duration):raise ValueError('INVALID_LRC')
    Path(path).write_text(normalized+'\n',encoding='utf-8',newline='\n')
    return str(path)


def embed_sidecar(audio,sidecar,duration=None,upgrade=True):
    """Embed the sidecar timeline so Roon (and Navidrome) can follow playback.

    Empty tags are filled. Earlier MusicOps releases embedded the same lyrics
    without timestamps; such a plain copy, or an older timed copy of the same
    text, is upgraded unless the lyrics field is locked. Different user lyrics
    are left untouched.
    """
    text=player_lrc(Path(sidecar).read_text('utf-8-sig'))
    if not is_synchronized(text,duration):raise ValueError('INVALID_LRC')
    plain=plain_lyrics(text)
    if not plain:raise ValueError('EMPTY_LYRICS')
    existing=[v for v in read_tag_fields(audio).get('lyrics',[]) if v.strip()]
    def same(value):return re.sub(r'\s+','',plain_lyrics(value))==re.sub(r'\s+','',plain)
    if not existing or (upgrade and existing!=[text] and all(same(v) for v in existing)):apply_tag_changes(audio,{'lyrics':text})
    return bool(read_tag_fields(audio).get('lyrics'))


def embedded_synchronized(audio,duration=None):
    return any(is_synchronized(v,duration) for v in read_tag_fields(audio).get('lyrics',[]))


def player_ready_sidecar(path,duration=None):
    """A sidecar counts as complete only once it is already in the player dialect."""
    try:text=Path(path).read_text('utf-8-sig')
    except (OSError,UnicodeError):return False
    try:return is_synchronized(text,duration) and text.strip()==player_lrc(text)
    except ValueError:return False
