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
