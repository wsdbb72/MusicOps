"""Unknown/encoding-placeholder tags are missing identity, not artist names."""
import re
from musicops.domain.identity import strict_text


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
            if i in used or not usable(title) or strict_text(title)!=strict_text(track.get('raw_title') or track.get('title')):continue
            known=[a for a in track.get('artists',[]) if usable(a)]
            if artists and {strict_text(a) for a in artists}!={strict_text(a) for a in known}:continue
            a,b=row.get('duration'),track.get('duration')
            if a and b and abs(a-b)>2:continue
            if not artists and (not known or not a or not b):continue
            candidates.append((i,track))
        if len(candidates)!=1:return None
        i,track=candidates[0];used.add(i);pairs.append((row,track))
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
