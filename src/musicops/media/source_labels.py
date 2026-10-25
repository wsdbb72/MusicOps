"""Provider/territory credits are provenance, not album-artist identity."""
import re
import unicodedata

_SOURCE=re.compile(r'^(Qobuz|Tidal|Deezer|Spotify|Apple\s+Music|Amazon\s+Music)\s*(?:[-·|:/]\s*)?'
                   r'(UK|US|GB|DE|FR|JP|CA|AU|NZ|IT|ES|NL|SE|NO|CH|AT|BE|DK|FI|IE|PT|PL|BR|MX|AR|CL|HK|TW|CN|KR|IN)$',re.I)
_SEPARATOR=re.compile(r'\s+[·|—-]\s+')


def _values(values):
    if values is None:return []
    return [str(value) for value in values if value is not None] if isinstance(values,(list,tuple,set)) else [str(values)]


def split_source_credit(value):
    original=str(value);text=' '.join(unicodedata.normalize('NFKC',original).split())
    def parse(label):
        if len(label)>128:return None
        match=_SOURCE.fullmatch(label.strip('[]()（） '))
        return {'field':'albumartist','value':original,'provider':re.sub(r'\s+','',match[1]).casefold(),'territory':match[2].upper()} if match else None
    marker=parse(text)
    if marker:return '',marker
    separators=list(_SEPARATOR.finditer(text))
    if separators:
        last=separators[-1];marker=parse(text[last.end():])
        if marker:return text[:last.start()].strip(),marker
    return original,None


def album_artist_credits(values):
    return list(dict.fromkeys(credit for value in _values(values) for credit,_ in [split_source_credit(value)] if credit.strip()))


def has_source_credit(values):
    return any(marker for value in _values(values) for _,marker in [split_source_credit(value)])


def effective_album_artists(row):
    tags=row.get('tags') if isinstance(row.get('tags'),dict) else {}
    values=row.get('albumartist',tags.get('albumartist',[]))
    return album_artist_credits(values) or _values(row.get('artists',[]))


def album_label(row):
    artists=effective_album_artists(row)
    return str(row.get('album') or '待识别专辑')+(' · '+' / '.join(artists) if artists else '')


def normalize_album_artist(row,values=None):
    values=_values(row.get('albumartist',[]) if values is None else values)
    markers=[marker for value in values for _,marker in [split_source_credit(value)] if marker]
    if not markers:return False
    row.setdefault('raw_albumartist',values)
    row['albumartist']=album_artist_credits(values)
    previous=row.get('source_labels',[])
    if not isinstance(previous,list):previous=[]
    row['source_labels']=previous+[marker for marker in markers if marker not in previous]
    return True
