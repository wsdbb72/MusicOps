"""Field plans and format-aware edits on independent work copies."""
from __future__ import annotations
import hashlib,base64
from pathlib import Path
import mutagen
from mutagen.id3 import ID3, TIT2, TPE1, TALB, TPE2, TRCK, TPOS, TDRC, TCON, TCOM, USLT, SYLT
from mutagen.id3 import APIC
from mutagen.flac import FLAC,Picture
from mutagen.mp4 import MP4Cover
from mutagen.apev2 import APEv2,APETextValue
from musicops.media.resources import validate_cover,validate_lrc,STAMP

ID3_FIELDS = {'title':('TIT2',TIT2),'artist':('TPE1',TPE1),'album':('TALB',TALB),'albumartist':('TPE2',TPE2),'tracknumber':('TRCK',TRCK),'discnumber':('TPOS',TPOS),'date':('TDRC',TDRC),'genre':('TCON',TCON),'composer':('TCOM',TCOM)}
SUPPORTED = set(ID3_FIELDS) | {'lyrics'}
APE_FIELDS={'albumartist':'Album Artist','tracknumber':'Track','discnumber':'Disc','date':'Year'}

def _values(value):
    if value is None: return []
    if isinstance(value,APETextValue):return list(value)
    return [str(x) for x in value] if isinstance(value,(list,tuple)) else [str(value)]

def read_tag_fields(path) -> dict:
    obj = mutagen.File(path)
    if obj is None: raise ValueError('UNSUPPORTED_TAG_FORMAT')
    tags = obj.tags
    result = {}
    if isinstance(tags,ID3):
        for field,(key,_) in ID3_FIELDS.items():
            result[field] = [str(t) for frame in tags.getall(key) for t in frame.text]
        result['lyrics'] = [frame.text for frame in tags.getall('USLT')]
    elif obj.__class__.__name__ == 'MP4':
        mapping={'title':'\xa9nam','artist':'\xa9ART','album':'\xa9alb','albumartist':'aART','date':'\xa9day','genre':'\xa9gen','composer':'\xa9wrt','lyrics':'\xa9lyr'}
        result = {k:_values((tags or {}).get(v,[])) for k,v in mapping.items()}
        for field,key in [('tracknumber','trkn'),('discnumber','disk')]:
            result[field] = [str(x[0]) if not x[1] else f'{x[0]}/{x[1]}' for x in (tags or {}).get(key,[])]
    elif isinstance(tags,APEv2):
        result={field:_values(tags.get(APE_FIELDS.get(field,field),tags.get(field,[]))) for field in SUPPORTED}
    else:
        result = {field:_values((tags or {}).get(field,[])) for field in SUPPORTED}
    return result

def _sylt(text):
    rows=[]
    for line in text.splitlines():
        line=line.strip();stamps=[];position=0
        while match:=STAMP.match(line,position):stamps.append(match);position=match.end()
        lyric=line[position:].strip()
        for match in stamps if lyric else ():
            fraction=match.group(3) or '0'
            rows.append((lyric,(int(match.group(1))*60+int(match.group(2)))*1000+round(int(fraction)*10**(3-len(fraction)))))
    return sorted(rows,key=lambda row:row[1])

def plan_tag_changes(path, changes, locks=()) -> dict:
    path = Path(path)
    fields = read_tag_fields(path)
    rows = []
    for field, proposed in changes.items():
        if field not in SUPPORTED: raise ValueError(f'UNSUPPORTED_FIELD: {field}')
        spec = proposed if isinstance(proposed,dict) else {'value':proposed}
        new = _values(spec.get('value'))
        rows.append({'field':field,'before':fields.get(field,[]),'after':new,'source':spec.get('source','user'),'reason':spec.get('reason','用户字段选择'),'confidence':spec.get('confidence','manual'),'locked':field in locks,'selected':field not in locks and new != fields.get(field,[])})
    return {'schema_version':1,'path':str(path),'before_hash':hashlib.sha256(path.read_bytes()).hexdigest(),'fields':rows,'changes':{r['field']:r['after'] for r in rows if r['selected']},'requires_work_copy':True,'roon_state':'pending'}

def apply_tag_changes(copy_path, changes) -> dict:
    path = Path(copy_path)
    if path.stat().st_nlink != 1: raise ValueError('HARDLINK_EDIT_FORBIDDEN')
    obj = mutagen.File(path)
    if obj is None: raise ValueError('UNSUPPORTED_TAG_FORMAT')
    if obj.tags is None: obj.add_tags()
    for field,value in changes.items():
        if field not in SUPPORTED: raise ValueError(f'UNSUPPORTED_FIELD: {field}')
        values = _values(value.get('value') if isinstance(value,dict) else value)
        if isinstance(obj.tags,ID3):
            if field == 'lyrics':
                obj.tags.delall('USLT');obj.tags.delall('SYLT')
                for i,text in enumerate(values):
                    obj.tags.add(USLT(encoding=3,lang='eng',desc=str(i),text=text))
                    # Roon reads ID3 synchronized lyrics from SYLT, not USLT.
                    if validate_lrc(text)['valid']:obj.tags.add(SYLT(encoding=3,lang='eng',format=2,type=1,desc=str(i),text=_sylt(text)))
            else:
                key,cls = ID3_FIELDS[field]
                obj.tags.delall(key)
                if values: obj.tags.add(cls(encoding=3,text=values))
        elif obj.__class__.__name__ == 'MP4':
            mapping={'title':'\xa9nam','artist':'\xa9ART','album':'\xa9alb','albumartist':'aART','date':'\xa9day','genre':'\xa9gen','composer':'\xa9wrt','lyrics':'\xa9lyr','tracknumber':'trkn','discnumber':'disk'}
            key = mapping[field]
            if field in ('tracknumber','discnumber'):
                values = [tuple(int(x) for x in v.split('/')) if '/' in v else (int(v),0) for v in values]
            if values: obj.tags[key]=values
            elif key in obj.tags: del obj.tags[key]
        elif isinstance(obj.tags,APEv2):
            key=APE_FIELDS.get(field,field)
            if key.casefold()!=field.casefold() and field in obj.tags:del obj.tags[field]
            if values:obj.tags[key]=values
            elif key in obj.tags:del obj.tags[key]
        else:
            if values: obj.tags[field]=values
            elif field in obj.tags: del obj.tags[field]
            if field in ('discnumber','tracknumber') and values and '/' in values[0]:
                total=values[0].split('/',1)[1]
                # Roon also reads these legacy Vorbis total fields. Keep both
                # aliases consistent when normalizing a complete multi-disc set.
                if total.isdigit():
                    for alias in (('totaldiscs','disctotal') if field=='discnumber' else ('totaltracks','tracktotal')):obj.tags[alias]=[total]
    obj.save()
    readback = read_tag_fields(path)
    for field,value in changes.items():
        expected = _values(value.get('value') if isinstance(value,dict) else value)
        if readback.get(field,[]) != expected: raise ValueError(f'TAG_READBACK_MISMATCH: {field}')
    return {'schema_version':1,'path':str(path),'verified':True,'fields':readback,'sha256':hashlib.sha256(path.read_bytes()).hexdigest(),'roon_state':'pending'}

def embedded_covers(path) -> list[bytes]:
    obj=mutagen.File(path)
    if isinstance(obj,FLAC):return [picture.data for picture in obj.pictures]
    if isinstance(getattr(obj,'tags',None),ID3):return [picture.data for picture in obj.tags.getall('APIC')]
    if obj and obj.__class__.__name__=='MP4':return [bytes(data) for data in (obj.tags or {}).get('covr',[])]
    if isinstance(getattr(obj,'tags',None),APEv2):
        return [bytes(value).split(b'\0',1)[1] for key,value in obj.tags.items() if key.casefold().startswith('cover art (') and getattr(value,'kind',None)==1 and b'\0' in bytes(value)]
    result=[]
    if obj:
        for encoded in (obj.tags or {}).get('metadata_block_picture',[]):
            try:result.append(Picture(base64.b64decode(encoded)).data)
            except Exception:continue
    return result


def mark_local_compilation(copy_path):
    """Withdraw original release identity on an independent compilation copy.

    Recording identifiers, titles and track artists deliberately remain intact.
    """
    path=Path(copy_path)
    if path.stat().st_nlink!=1:raise ValueError('HARDLINK_EDIT_FORBIDDEN')
    obj=mutagen.File(path)
    if obj is None:raise ValueError('UNSUPPORTED_TAG_FORMAT')
    if obj.tags is None:obj.add_tags()
    names={'musicbrainz_albumid','musicbrainz_releasegroupid','musicbrainz_albumartistid','musicbrainz_albumtype','musicbrainz_albumstatus','musicbrainz_albumreleasecountry','releasecountry','releasestatus','releasetype','catalognumber','cataloguenumber','barcode','label','publisher','totaltracks','tracktotal','totaldiscs','disctotal'}
    if isinstance(obj.tags,ID3):
        from mutagen.id3 import TCMP
        for frame in list(obj.tags.getall('TXXX')):
            if frame.desc.casefold().replace(' ','_') in names:obj.tags.delall(frame.HashKey)
        obj.tags.delall('TPUB');obj.tags.delall('TCMP');obj.tags.add(TCMP(encoding=3,text=['1']))
    elif obj.__class__.__name__=='MP4':
        for key in list(obj.tags.keys()):
            if key.rsplit(':',1)[-1].casefold() in names:del obj.tags[key]
        obj.tags['cpil']=True
    else:
        for key in list(obj.tags.keys()):
            if key.casefold() in names:del obj.tags[key]
        obj.tags['compilation']=['1']
    obj.save()

IMAGE_MIMES={'JPEG':'image/jpeg','PNG':'image/png','WEBP':'image/webp','GIF':'image/gif','BMP':'image/bmp'}

def _image_facts(data):
    import io
    from PIL import Image
    try:
        with Image.open(io.BytesIO(data)) as img:
            if img.format not in IMAGE_MIMES:return None
            img.verify()
            return IMAGE_MIMES[img.format],img.size
    except Exception:return None

def repair_embedded_pictures(copy_path) -> int:
    """Fix picture blocks whose MIME type or size fields are missing.

    Some store downloads embed a valid JPEG with an empty MIME type and a 0x0
    size. Recent FFmpeg refuses to open such files and older builds report an
    error, so a playable album fails validation. The image bytes are kept; a
    block whose data is not an image is dropped. Only work copies are edited.
    Returns the number of repaired or dropped pictures.
    """
    path=Path(copy_path)
    if path.stat().st_nlink!=1:raise ValueError('HARDLINK_EDIT_FORBIDDEN')
    obj=mutagen.File(path)
    changed=0
    if isinstance(obj,FLAC):
        kept=[]
        for pic in obj.pictures:
            facts=_image_facts(pic.data)
            if facts is None:changed+=1;continue
            mime,(width,height)=facts
            if pic.mime!=mime or not pic.width or not pic.height:
                pic.mime=mime;pic.width=width;pic.height=height;pic.depth=pic.depth or 24;changed+=1
            kept.append(pic)
        if changed:
            obj.clear_pictures()
            for pic in kept:obj.add_picture(pic)
    elif obj is not None and isinstance(obj.tags,ID3):
        for frame in list(obj.tags.getall('APIC')):
            facts=_image_facts(frame.data)
            if facts is None:obj.tags.delall(frame.HashKey);changed+=1
            elif frame.mime!=facts[0]:frame.mime=facts[0];changed+=1
    if changed:obj.save()
    return changed

def apply_cover_changes(copy_path,content,min_side=300) -> dict:
    """Replace front cover only on a work copy; preserve other/unknown pictures."""
    path=Path(copy_path)
    if path.stat().st_nlink!=1:raise ValueError('HARDLINK_EDIT_FORBIDDEN')
    checked=validate_cover(content,min_side)
    if not checked['valid']:raise ValueError('INVALID_COVER')
    obj=mutagen.File(path)
    if obj is None:raise ValueError('UNSUPPORTED_TAG_FORMAT')
    if obj.tags is None:obj.add_tags()
    mime={'JPEG':'image/jpeg','PNG':'image/png','WEBP':'image/webp'}[checked['format']]
    picture=Picture();picture.type=3;picture.mime=mime;picture.desc='MusicOps approved front cover';picture.width=checked['width'];picture.height=checked['height'];picture.depth=24;picture.data=content
    if isinstance(obj,FLAC):
        retained=[old for old in obj.pictures if old.type!=3];obj.clear_pictures()
        for old in retained:obj.add_picture(old)
        obj.add_picture(picture)
    elif isinstance(obj.tags,ID3):
        retained=[old for old in obj.tags.getall('APIC') if old.type!=3];obj.tags.delall('APIC')
        for old in retained:obj.tags.add(old)
        obj.tags.add(APIC(encoding=3,mime=mime,type=3,desc=picture.desc,data=content))
    elif obj.__class__.__name__=='MP4':
        if checked['format'] not in ('JPEG','PNG'):raise ValueError('UNSUPPORTED_MP4_COVER_FORMAT')
        retained=list(obj.tags.get('covr',[]))[1:]
        obj.tags['covr']=[MP4Cover(content,imageformat=MP4Cover.FORMAT_JPEG if checked['format']=='JPEG' else MP4Cover.FORMAT_PNG),*retained]
    elif isinstance(obj.tags,APEv2):
        extension={'JPEG':'jpg','PNG':'png','WEBP':'webp'}[checked['format']]
        obj.tags['Cover Art (Front)']=('cover.'+extension).encode('ascii')+b'\0'+content
    elif obj.__class__.__name__ in ('OggVorbis','OggOpus'):
        retained=[]
        for old in obj.tags.get('metadata_block_picture',[]):
            try:
                if Picture(base64.b64decode(old)).type!=3:retained.append(old)
            except Exception:retained.append(old)
        obj.tags['metadata_block_picture']=retained+[base64.b64encode(picture.write()).decode()]
    else:raise ValueError('UNSUPPORTED_EMBEDDED_COVER_FORMAT')
    obj.save()
    if content not in embedded_covers(path):raise ValueError('COVER_READBACK_MISMATCH')
    return {'schema_version':1,'verified':True,'cover_sha256':checked['sha256'],'sha256':hashlib.sha256(path.read_bytes()).hexdigest(),'roon_state':'pending'}
