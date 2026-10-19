"""Keep synchronized LRC sidecars and Roon-readable embedded plain lyrics."""
import re
from pathlib import Path
from musicops.media.resources import validate_lrc
from musicops.media.tags import read_tag_fields,apply_tag_changes


def plain_lyrics(text):
    lines=[]
    for line in text.splitlines():
        if re.match(r'^\s*\[(?:ar|al|ti|by|offset|re|ve|length):',line,re.I):continue
        line=re.sub(r'\[\d+:\d+(?:[.:]\d+)?\]|<\d+:\d+(?:[.:]\d+)?>','',line).strip()
        if line:lines.append(line)
    return '\n'.join(lines)


def embed_sidecar(audio,sidecar,duration=None):
    text=Path(sidecar).read_text('utf-8-sig')
    if not validate_lrc(text,duration)['valid']:raise ValueError('INVALID_LRC')
    plain=plain_lyrics(text)
    if not plain:raise ValueError('EMPTY_LYRICS')
    existing=read_tag_fields(audio).get('lyrics',[])
    if not any(v.strip() for v in existing):apply_tag_changes(audio,{'lyrics':plain})
    return bool(read_tag_fields(audio).get('lyrics'))
