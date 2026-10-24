"""Unknown/encoding-placeholder tags are missing identity, not artist names."""
import re
from musicops.domain.identity import strict_text


def credits_equal(first,second):
    """Match actual credited names, including one joined multi-artist tag.

    Never split a band such as AC/DC into invented individual artists.
    """
    from musicops.media.chinese import alias_key
    left=[alias_key(a) for a in first if a];right=[alias_key(a) for a in second if a]
    if set(left)==set(right):return bool(left)
    separators=('/', ' / ', ';', '; ', ' & ', ', ', '、')
    def joined(single,multiple):
        return len(single)==1 and len(multiple)>1 and any(single[0]==separator.join(order) for separator in separators for order in (multiple,sorted(multiple)))
    return joined(left,right) or joined(right,left)


def title_keys(row):
    from musicops.media.chinese import alias_key
    text=row.get('raw_title') or row.get('title') or '';keys={alias_key(text)}
    value=(row.get('tags',{}).get('tracknumber') or [row.get('track') or ''])[0]
    position=re.fullmatch(r'\s*0*([1-9]\d*)(?:\s*/\s*\d+)?\s*',str(value))
    prefix=re.match(r'^\s*0*([1-9]\d*)\s*[-.、_]\s+(.+)$',text)
    if position and prefix and int(position[1])==int(prefix[1]):keys.add(alias_key(prefix[2]))
    return keys


def usable(value):
    text=str(value or '').strip()
    return bool(text and '\ufffd' not in text and not re.fullmatch(r'[?？\s]+',text) and text.casefold() not in {'unknown','unknown artist','unknown album','未知','未知歌手','未知艺术家','未知专辑','<unknown>'})


def release_pairs(rows,tracks):
    if len(rows)!=len(tracks):return None
    pairs=[];used=set()
    for row in rows:
        title=row.get('raw_title') or row.get('title');artists=[a for a in row.get('artists',[]) if usable(a)]
        candidates=[]
        for i,track in enumerate(tracks):
            if i in used or not usable(title) or not title_keys(row)&title_keys(track):continue
            known=[a for a in track.get('artists',[]) if usable(a)]
            if artists and not credits_equal(artists,known):continue
            a,b=row.get('duration'),track.get('duration')
            if a and b and abs(a-b)>2:continue
            if not artists and (not known or not a or not b):continue
            candidates.append((i,track))
        if len(candidates)!=1:return None
        i,track=candidates[0];used.add(i);pairs.append((row,track))
    return pairs


def cover_artist_names(values):
    from musicops.media.chinese import alias_key
    return [re.sub(r'^the\s+','',alias_key(value)) for value in values]


def cover_pairs(rows,tracks):
    """Artwork-only aliases need every local track and measured durations.

    An omitted English article does not establish an exact pressing identity.
    """
    if len(rows)!=len(tracks):return None
    def titles(row):
        # Platforms move guest credits between title and artist fields. Keep
        # live/remix/edition qualifiers: only a final explicit feat credit moves.
        return {re.sub(r'\s*[（(\[]\s*(?:feat\.?|ft\.?|featuring)\s+[^）)\]]+[）)\]]\s*$','',key,flags=re.I) for key in title_keys(row)}
    used=set();pairs=[]
    for row in rows:
        names=set(cover_artist_names(row.get('artists',[])));matches=[]
        for index,track in enumerate(tracks):
            if index in used or not titles(row)&titles(track):continue
            known=set(cover_artist_names(track.get('artists',[])))
            if not names or not known or not (names<=known or known<=names or credits_equal(list(names),list(known))):continue
            first,second=row.get('duration'),track.get('duration')
            if not first or not second or abs(first-second)>2:continue
            matches.append((index,track))
        if len(matches)!=1:return None
        index,track=matches[0];used.add(index);pairs.append((row,track))
    return pairs


def filename_identity(row):
    """Search hints only: filenames are never authoritative recording tags."""
    from pathlib import Path
    path=Path(row.get('original_path') or row.get('path') or '')
    match=re.fullmatch(r'\d{1,4}\s*[.、_-]\s*(.+?)\s+-\s+(.+)',path.stem)
    if not match:return dict(row)
    artist,title=match.groups()
    # Remove descriptive film credits, but never live/remix/version evidence.
    title=re.sub(r'\s*[（(](?:电影|電影|电视剧|電視劇)[^）)]*[）)]','',title).strip()
    return {**row,'raw_title':row.get('raw_title') or title,'artists':row.get('artists') or [artist]}


def metadata_subset_pairs(rows,tracks):
    """Unique title/artist/duration matches may fill a partial local album.

    This does not certify release completeness or exact recording identity.
    """
    from musicops.media.chinese import alias_key
    if len(rows)>len(tracks) or len(rows)<2:return None
    pairs=[];used=set()
    for row in rows:
        hint=filename_identity(row);title=hint.get('raw_title') or hint.get('title');artists=hint.get('artists',[])
        candidates=[]
        for i,track in enumerate(tracks):
            if i in used or not usable(title) or alias_key(title)!=alias_key(track.get('title')):continue
            if not artists or {alias_key(a) for a in artists}!={alias_key(a) for a in track.get('artists',[])}:continue
            a,b=row.get('duration'),track.get('duration')
            if not a or not b or abs(a-b)>2:continue
            candidates.append((i,track))
        if len(candidates)==1:
            i,track=candidates[0];used.add(i);pairs.append((row,track))
    return pairs if len(pairs)>=max(2,len(rows)*.8) else None
