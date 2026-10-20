"""Compare observed editions without equating live/remix recordings with studio."""
import re
import html
from musicops.domain.identity import strict_text
from musicops.media.chinese import alias_key
from musicops.config.schema import digest
SPECIAL=r'deluxe|expanded|anniversary|remaster(?:ed)?|bonus|special edition|japan(?:ese)? edition|豪华|周年|重制|加曲|日本版|限定版'
RECORDING=r'\blive\b|\bremix(?:ed)?\b|\binstrumental\b|\bkaraoke\b|\bre[- ]?record(?:ed|ing)\b|现场|混音|重录|伴奏'

def album_base(title):
    text=strict_text(title)
    def strip(match):return '' if re.search(SPECIAL,match[0],re.I) else match[0]
    text=re.sub(r'\([^)]*\)|\[[^]]*\]|（[^）]*）',strip,text)
    text=re.sub(r'\s*[-–:]?\s*(?:\d{4}\s*)?(?:'+SPECIAL+r')(?:\s+(?:edition|version|\d{4}))*\s*$', '',text,flags=re.I)
    return ' '.join(text.split()).strip(' -–:')

def compatible(observed,expected):
    return alias_key(album_base(observed))==alias_key(album_base(expected)) and recording_kinds(observed)==recording_kinds(expected)


def recording_kinds(text):
    patterns={'live':r'\blive\b|现场|現場|演唱会|演唱會','remix':r'\bremix(?:ed)?\b|混音','acoustic':r'\bacoustic\b|\bunplugged\b|不插电|不插電','instrumental':r'\binstrumental\b|伴奏|纯音乐|純音樂','karaoke':r'\bkaraoke\b|卡拉','rerecording':r'\bre[- ]?record(?:ed|ing)\b|重录|重錄','demo':r'\bdemo\b|小样|小樣'}
    return tuple(k for k,p in patterns.items() if re.search(p,str(text),re.I))


def family_base(title):
    text=album_base(title)
    text=re.sub(r'\([^)]*\)|\[[^]]*\]|（[^）]*）',lambda m:'' if recording_kinds(m[0]) or re.search(r'\bsingle\b|单曲|單曲',m[0],re.I) else m[0],text)
    text=re.sub(r'\s+[-–:]\s*(?:live|remix|acoustic|unplugged|instrumental|demo|single)(?:\s+version)?\s*$','',text,flags=re.I)
    return alias_key(text.strip())


def describe(candidate):
    release=candidate.get('release') or {};title=candidate.get('title','');edition=str(release.get('edition') or '')
    typ=str(release.get('release_type_name') or release.get('release_type') or '')
    text=' '.join((title,edition,typ));kinds=recording_kinds(text)
    single=bool(re.search(r'\bsingle\b|单曲|單曲',typ,re.I) or re.search(r'(?:[-–:]\s*|[（(\[])\s*(?:single|单曲|單曲)\s*[）)\]]?\s*$',title,re.I))
    labels={'live':'现场','remix':'混音','acoustic':'不插电','instrumental':'纯音乐 / 伴奏','karaoke':'卡拉OK','rerecording':'重录','demo':'小样'}
    special=[]
    for key,pattern in [('重制',r'remaster|重制|重製'),('豪华 / 加曲',r'deluxe|expanded|bonus|豪华|豪華|加曲'),('周年',r'anniversary|周年'),('再发行',r'reissue|再发行|再發行')]:
        if re.search(pattern,text,re.I):special.append(key)
    if release.get('remastered') is True and not special:special.append('再发行 / 重制标记')
    label=' · '.join(([labels[k] for k in kinds]+special+(['单曲（最低优先）'] if single else [])) or ['普通版 / 版本待确认'])
    year=release.get('remaster_year') or release.get('year');catalogue=release.get('catalogue_number') or '';medium=(candidate.get('media') or {}).get('media') or ''
    # This is a grouping hint, never a claim of confirmed release identity.
    key=[family_base(title),sorted(alias_key(a) for a in candidate.get('artists',[])),kinds,special,alias_key(edition),str(year or ''),alias_key(catalogue),alias_key(release.get('record_label')),alias_key(medium),candidate.get('track_count') or release.get('track_count')]
    if not year and not catalogue:key.append([candidate.get('source'),candidate.get('group_id') or candidate.get('candidate_id')])
    return {'group_id':digest(key),'label':label,'year':year,'catalogue':catalogue,'medium':medium,'single':single,'special':bool(kinds or special),'recording_kinds':kinds,'identity_confirmed':False}


def needs_version_choice(candidates):
    descriptions=[describe(c) for c in candidates]
    return len({d['group_id'] for d in descriptions})>1 and any(d['special'] for d in descriptions)

def rank(candidate,policy='quality',source_order=()):
    media=candidate.get('media') or {};encoding=str(media.get('encoding') or '');text=encoding+' '+candidate.get('title','')+' '+str((candidate.get('release') or {}).get('edition',''))
    lossless=str(media.get('format','')).lower() in ('flac','alac','wav','aiff','ape')
    bits=re.search(r'(16|24|32)\s*[- ]?bit',text,re.I);rate=re.search(r'(\d+(?:\.\d+)?)\s*khz',text,re.I)
    quality=(lossless,int(bits[1]) if bits else 16 if lossless else 0,float(rate[1]) if rate else 0)
    count=candidate.get('track_count') or (candidate.get('release') or {}).get('track_count') or (len(candidate.get('tracklist',[])) if candidate.get('tracklist_complete') else 0)
    count=count if type(count) is int and 0<=count<=2000 else 0
    special=bool(re.search(SPECIAL,text,re.I))
    priorities=(count,*quality,special) if policy=='complete' else (special,*quality,count) if policy=='special' else (*quality,count,special)
    return (not describe(candidate)['single'],*priorities,candidate.get('seeders') or 0,-source_order.index(candidate['source']) if candidate.get('source') in source_order else 0)

def audio_count(torrent):
    file_list=torrent.get('fileList')
    if not isinstance(file_list,str) or len(file_list)>2_000_000:return None
    files=file_list.split('|||');count=0
    for entry in files:
        match=re.fullmatch(r'(.+)\{\{\{\d+\}\}\}',html.unescape(entry).strip())
        if not match:return None
        if re.search(r'\.(?:flac|alac|m4a|mp3|wav|aiff?|ape|dsf|dff|ogg)$',match[1],re.I):count+=1
    return count if 0<count<=2000 else None
