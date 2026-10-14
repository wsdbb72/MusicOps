"""Validated resources and explicit companion relationships; never publish directly."""
from __future__ import annotations
import hashlib
import io
import math
import re
from pathlib import Path
from PIL import Image, ImageStat

STAMP = re.compile(r"\[(\d{1,3}):([0-5]\d)(?:[.:](\d{1,3}))?\]")
META = re.compile(r"^\[(?:ar|ti|al|by|re|ve|length|offset|id):.*\]$", re.I)

def validate_lrc(text: str, duration: float | None = None) -> dict:
    result = {"schema_version": 1, "valid": False, "kind": "synchronized", "errors": [], "timestamps": [], "line_count": 0}
    if not isinstance(text, str) or not text.strip():
        result['errors'].append('EMPTY_OR_TOO_LARGE'); return result
    try:encoded=text.encode('utf-8')
    except UnicodeError:result['errors'].append('INVALID_TEXT');return result
    if len(encoded)>2_000_000:result['errors'].append('EMPTY_OR_TOO_LARGE');return result
    if duration is not None and (type(duration) not in (int,float) or not 0<=duration<=1_000_000_000 or not math.isfinite(duration)):
        result['errors'].append('INVALID_DURATION');return result
    if re.search(r"<!doctype|<html|<body|<script|<head", text, re.I) or '\x00' in text or '\ufffd' in text:
        result['errors'].append('INVALID_TEXT'); return result
    offsets=re.findall(r"(?im)^\s*\[offset:([^\]]*)\]\s*$",text)
    if any(not re.fullmatch(r'[+-]?\d{1,12}',v) for v in offsets) or len(set(offsets))>1:
        result['errors'].append('INVALID_OFFSET');return result
    offset = int(offsets[0])/1000 if offsets else 0
    previous = -1.0
    lyrics = 0
    for line in text.splitlines():
        line = line.strip()
        if not line or META.match(line): continue
        matches=[];position=0
        while match:=STAMP.match(line,position):
            matches.append(match);position=match.end()
        if not matches:
            result['errors'].append('UNSYNCHRONIZED_LINE'); continue
        if not line[position:].strip(): continue
        lyrics += 1
        first_stamp=None;previous_in_line=-1.0
        for match in matches:
            frac = float('0.' + match.group(3)) if match.group(3) else 0
            stamp = int(match.group(1))*60 + int(match.group(2)) + frac + offset
            if stamp < 0: result['errors'].append('NEGATIVE_TIMESTAMP')
            if first_stamp is None:
                first_stamp=stamp
                if stamp<previous:result['errors'].append('UNORDERED_TIMESTAMP')
            if stamp<previous_in_line:result['errors'].append('UNORDERED_TIMESTAMP')
            if duration is not None and stamp > float(duration) + 3: result['errors'].append('TIMESTAMP_OUT_OF_RANGE')
            previous_in_line=stamp
            result['timestamps'].append(round(stamp, 3))
        previous=first_stamp
    if not lyrics: result['errors'].append('NO_LYRIC_LINES'); result['kind'] = 'plain'
    result['errors'] = sorted(set(result['errors']))
    result['line_count'] = lyrics
    result['valid'] = not result['errors']
    result['timestamps'].sort()
    result['sha256'] = hashlib.sha256(encoded).hexdigest()
    return result

def shift_lrc(text: str, offset_ms: int) -> str:
    """Produce a preview; flatten old offset into the new timestamps once."""
    if not isinstance(text,str) or type(offset_ms) is not int:raise ValueError('歌词与偏移格式无效')
    offsets=re.findall(r"(?im)^\s*\[offset:([^\]]*)\]\s*$",text)
    if any(not re.fullmatch(r'[+-]?\d{1,12}',v) for v in offsets) or len(set(offsets))>1:raise ValueError('歌词offset元数据无效或冲突')
    total = offset_ms + (int(offsets[0]) if offsets else 0)
    text = re.sub(r"(?im)^\s*\[offset:[+-]?\d+\][ \t]*(?:\r?\n|$)", '', text)
    def replace(match):
        frac = float('0.'+match.group(3)) if match.group(3) else 0
        ms = round((int(match.group(1))*60+int(match.group(2))+frac)*1000)+total
        if ms < 0: raise ValueError('偏移会产生负时间戳')
        centiseconds=round(ms/10)
        minutes, remainder = divmod(centiseconds, 6000)
        if minutes>999:raise ValueError('偏移会超出支持的时间戳范围')
        return f'[{minutes:02d}:{remainder/100:05.2f}]'
    def line_replace(line):
        prefix=line[:len(line)-len(line.lstrip())];matches=[];position=len(prefix)
        while match:=STAMP.match(line,position):matches.append(match);position=match.end()
        return prefix+''.join(replace(match) for match in matches)+line[position:]
    return ''.join(line_replace(line) for line in text.splitlines(keepends=True))

def validate_cover(data: bytes, min_side: int = 300) -> dict:
    result = {"schema_version": 1, "valid": False, "errors": [], "size_bytes": len(data)}
    if not data or len(data) > 20_000_000:
        result['errors'] = ['EMPTY_OR_TOO_LARGE']; return result
    try:
        with Image.open(io.BytesIO(data)) as img:
            image_format, dimensions = img.format, img.size
            if dimensions[0]*dimensions[1] > 40_000_000: raise ValueError('IMAGE_PIXEL_LIMIT')
            img.verify()
        with Image.open(io.BytesIO(data)) as img:
            img.load()
            extrema = ImageStat.Stat(img.convert('RGB').resize((32,32))).stddev
        result.update(format=image_format, width=dimensions[0], height=dimensions[1], sha256=hashlib.sha256(data).hexdigest())
        if image_format not in ('JPEG','PNG','WEBP'): result['errors'].append('UNSUPPORTED_IMAGE')
        if min(dimensions) < min_side: result['errors'].append('TOO_SMALL')
        if max(extrema) < 1: result['errors'].append('PLACEHOLDER_IMAGE')
    except Exception as exc:
        result['errors'].append('INVALID_IMAGE'); result['detail'] = type(exc).__name__
    result['valid'] = not result['errors']
    return result

def resource_change_allowed(existing: dict | None, candidate: dict, locked: bool = False, approved: bool = False) -> dict:
    if locked: return {'allowed':False,'reason':'USER_LOCKED'}
    if not candidate.get('validation',{}).get('valid'): return {'allowed':False,'reason':'INVALID_RESOURCE'}
    if candidate.get('identity_state') == 'mismatch': return {'allowed':False,'reason':'VERSION_MISMATCH'}
    if existing and existing.get('validation',{}).get('valid') and not approved:
        return {'allowed':False,'reason':'EXISTING_VALID_REQUIRES_REVIEW'}
    if candidate.get('identity_state','structural_only') != 'matched' and not approved:
        return {'allowed':False,'reason':'IDENTITY_REQUIRES_REVIEW'}
    return {'allowed':True,'reason':'USER_APPROVED' if approved else 'MATCHED_FILL_EMPTY'}

def companion_relationships(audio: str | Path) -> list[dict]:
    """LRC is track scoped; shared cover and CUE are release scoped."""
    audio = Path(audio)
    companions = []
    lrc = audio.with_suffix('.lrc')
    if lrc.is_file(): companions.append({'path':str(lrc),'kind':'lyrics','scope':'track','audio':str(audio)})
    for cover in audio.parent.iterdir():
        if cover.is_file() and cover.suffix.lower() in ('.jpg','.jpeg','.png','.webp') and cover.stem.lower() in ('cover','folder',audio.stem.lower()):
            companions.append({'path':str(cover),'kind':'cover','scope':'release','move_with_track':False})
        if cover.is_file() and cover.suffix.lower() == '.cue':
            text = cover.read_text(encoding='utf-8-sig')
            refs = re.findall(r'^\s*FILE\s+"([^"]+)"\s+\w+', text, re.M | re.I)
            if audio.name.casefold() in [x.casefold() for x in refs]:
                companions.append({'path':str(cover),'kind':'cue','scope':'release','references':refs,'move_with_track':False})
    return companions

def rewrite_cue(text: str, renames: dict[str,str]) -> str:
    folded = {old.casefold(): new for old,new in renames.items()}
    for new in folded.values():
        if Path(new).name != new or '"' in new or '\n' in new or '\r' in new: raise ValueError('CUE名称必须为安全文件名')
    return re.sub(r'(?im)^(\s*FILE\s+")([^"]+)("\s+\w+)', lambda m:m.group(1)+folded.get(m.group(2).casefold(),m.group(2))+m.group(3), text)
