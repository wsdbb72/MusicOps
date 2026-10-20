"""Chinese display/lookup aliases; never replace recording evidence with spelling."""
from functools import lru_cache
import re
import unicodedata

@lru_cache(maxsize=1)
def _converter():
    from opencc import OpenCC
    return OpenCC('t2s')

def simplified(text):
    return _converter().convert(str(text or ''))

def alias_key(text):
    return ' '.join(unicodedata.normalize('NFKC',simplified(text)).casefold().split())

def display_text(text,*,language=None,prefer_simplified=True):
    # Kana/Japanese identity stays intact; unknown languages keep source spelling.
    text=str(text or '')
    if not prefer_simplified or str(language or '').casefold() not in ('zh','zho','chi','zh-cn','zh-hans','cmn') or re.search(r'[\u3040-\u30ff]',text):return text
    return simplified(text)


def declared_language(path):
    import mutagen
    from mutagen.id3 import ID3
    obj=mutagen.File(path);tags=obj.tags if obj else None
    if isinstance(tags,ID3):
        values=[str(v) for frame in tags.getall('TLAN') for v in frame.text]
    else:values=(tags or {}).get('language',[])
    if isinstance(values,str):values=[values]
    return str(values[0]).casefold() if values else None
