"""Compare observed editions without equating live/remix recordings with studio."""
import re
import html
from musicops.domain.identity import strict_text
SPECIAL=r'deluxe|expanded|anniversary|remaster(?:ed)?|bonus|special edition|japan(?:ese)? edition|豪华|周年|重制|加曲|日本版|限定版'
RECORDING=r'\blive\b|\bremix(?:ed)?\b|\binstrumental\b|\bkaraoke\b|\bre[- ]?record(?:ed|ing)\b|现场|混音|重录|伴奏'

def album_base(title):
    text=strict_text(title)
    def strip(match):return '' if re.search(SPECIAL,match[0],re.I) else match[0]
    text=re.sub(r'\([^)]*\)|\[[^]]*\]|（[^）]*）',strip,text)
    text=re.sub(r'\s*[-–:]?\s*(?:\d{4}\s*)?(?:'+SPECIAL+r')(?:\s+(?:edition|version|\d{4}))*\s*$', '',text,flags=re.I)
    return ' '.join(text.split()).strip(' -–:')

def compatible(observed,expected):
    return album_base(observed)==album_base(expected) and not (re.search(RECORDING,observed,re.I) and not re.search(RECORDING,expected,re.I))

def rank(candidate,policy='quality',source_order=()):
    media=candidate.get('media') or {};encoding=str(media.get('encoding') or '');text=encoding+' '+candidate.get('title','')+' '+str((candidate.get('release') or {}).get('edition',''))
    lossless=str(media.get('format','')).lower() in ('flac','alac','wav','aiff','ape')
    bits=re.search(r'(16|24|32)\s*[- ]?bit',text,re.I);rate=re.search(r'(\d+(?:\.\d+)?)\s*khz',text,re.I)
    quality=(lossless,int(bits[1]) if bits else 16 if lossless else 0,float(rate[1]) if rate else 0)
    count=candidate.get('track_count') or (candidate.get('release') or {}).get('track_count') or (len(candidate.get('tracklist',[])) if candidate.get('tracklist_complete') else 0)
    count=count if type(count) is int and 0<=count<=2000 else 0
    special=bool(re.search(SPECIAL,text,re.I))
    priorities=(count,*quality,special) if policy=='complete' else (special,*quality,count) if policy=='special' else (*quality,count,special)
    return (*priorities,candidate.get('seeders') or 0,-source_order.index(candidate['source']) if candidate.get('source') in source_order else 0)

def audio_count(torrent):
    file_list=torrent.get('fileList')
    if not isinstance(file_list,str) or len(file_list)>2_000_000:return None
    files=file_list.split('|||');count=0
    for entry in files:
        match=re.fullmatch(r'(.+)\{\{\{\d+\}\}\}',html.unescape(entry).strip())
        if not match:return None
        if re.search(r'\.(?:flac|alac|m4a|mp3|wav|aiff?|ape|dsf|dff|ogg)$',match[1],re.I):count+=1
    return count if 0<count<=2000 else None
