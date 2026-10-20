"""Explicit local compilation assembly; original track artists are immutable."""
import copy
import json
import re
import uuid
from pathlib import Path
from musicops.config.schema import digest
from musicops.domain.errors import Conflict,PolicyBlocked
from musicops.library.catalog import root_key
from musicops.library.index import AUDIO_EXTENSIONS,file_version,sha256_file
from musicops.library.paths import reject_reparse_chain,safe_destination
from musicops.library.transactions import durable_copy
from musicops.media.tags import read_tag_fields,apply_tag_changes,mark_local_compilation
from musicops.media.resources import validate_lrc
from musicops.media.validation import MediaValidator


def albums(service,cfg,args):
    query=str(args.get('query','')).strip()[:200]
    pattern='%'+query.replace('\\','\\\\').replace('%','\\%').replace('_','\\_')+'%'
    with service.store.connection() as db:
        rows=db.execute("SELECT payload FROM library_albums WHERE root_key=? AND (json_extract(payload,'$.album') LIKE ? ESCAPE '\\' OR json_extract(payload,'$.artists') LIKE ? ESCAPE '\\') ORDER BY json_extract(payload,'$.album'),album_id LIMIT 201",(root_key(cfg['paths']['library']),pattern,pattern)).fetchall()
    return {'albums':[json.loads(r[0]) for r in rows[:200]],'truncated':len(rows)>200,'music_writes':0}


def _seed_guard(service,cfg,paths):
    from musicops.app.library_safety import protect_seeding
    protect_seeding(service,cfg,paths)


def _name(value):
    value=str(value).strip()
    if not value or len(value)>100 or value in ('.','..') or re.search(r'[<>:"/\\|?*\x00-\x1f]',value) or value.endswith(('.',' ')):
        raise ValueError('专辑名称或专辑艺人为空、过长或包含路径保留字符')
    return value


def preview(service,cfg,args):
    album=_name(args.get('album',''));albumartist=_name(args.get('albumartist') or '群星')
    ids=args.get('file_ids')
    if not isinstance(ids,list) or not ids or len(ids)>500 or len(set(ids))!=len(ids):raise ValueError('请选择1至500首互不重复的歌曲')
    root=Path(cfg['paths']['library']).absolute();rows=[]
    locks={r.get('identity') for r in service.store.list('field_locks')}|{r.get('identity') for r in service.store.list('suppressions') if r.get('active')}
    for fid in ids:
        row=service.store.get('library_files',fid)
        if not row or row.get('location_state','library')!='library' or row.get('read_error'):raise Conflict('所选歌曲已变化或资料未读取，请先更新音乐库索引')
        path=Path(row['path']);reject_reparse_chain(path,root)
        if not path.is_relative_to(root) or path.suffix.casefold() not in AUDIO_EXTENSIONS:raise PolicyBlocked('只能合并音乐库中的音频')
        if fid in locks or str(path) in locks:raise PolicyBlocked('所选歌曲包含人工锁定或抑制记录')
        if not row.get('artists') or not row.get('raw_title'):raise PolicyBlocked('部分歌曲缺少原艺人或歌名，请先补齐资料')
        if any(p.suffix.casefold()=='.cue' for p in path.parent.iterdir()):raise PolicyBlocked('原目录包含 CUE 引用，请先核对整碟引用关系；原文件保留')
        rows.append(row)
    _seed_guard(service,cfg,[Path(r['path']) for r in rows])
    plan_id='merge-compilation-'+str(uuid.uuid4());target=safe_destination(root,albumartist+' - '+album)
    if target.exists():raise Conflict('目标合辑目录已存在，请更换名称或先将已有合辑加入选择并改用新目录')
    validator=MediaValidator(cfg['paths']['tools']['ffmpeg'],cfg['paths']['tools']['ffprobe'],cfg['worker']['decode_timeout'])
    operations=[];originals=[];resources={}
    def order(row):
        def number(value):
            match=re.search(r'\d+',str(value or ''));return int(match[0]) if match else 0
        return (row.get('album',''),number(row.get('disc')),number(row.get('track')),row['path'])
    for number,row in enumerate(sorted(rows,key=order),1):
        service._checkpoint('compilation_prepare')
        path=Path(row['path']);before=file_version(path)
        if (before[0],before[1])!=(row.get('size'),row.get('mtime_ns')):raise Conflict('歌曲在索引后变化，请重新扫描')
        tags=read_tag_fields(path)
        if tags.get('artist',[])!=row.get('artists') or (tags.get('title') or [''])[0]!=row.get('raw_title'):raise Conflict('原歌名或艺人已变化，请重新扫描')
        validation=validator.validate(path)
        if validation.get('decode_state')!='passed' or not validation.get('roon_compatible'):raise PolicyBlocked('原歌曲未通过完整解码和兼容性校验')
        prepared=safe_destination(Path(cfg['paths']['work']),plan_id+'/'+str(number)+path.suffix)
        prepared.parent.mkdir(parents=True,exist_ok=True);durable_copy(path,prepared)
        mark_local_compilation(prepared)
        apply_tag_changes(prepared,{'album':[album],'albumartist':[albumartist],'tracknumber':[f'{number}/{len(rows)}'],'discnumber':['1/1'],'date':[]})
        if read_tag_fields(prepared).get('artist')!=tags.get('artist'):raise Conflict('合辑副本原艺人未保持，停止执行')
        if validator.pcm_hash(path)!=validator.pcm_hash(prepared):raise Conflict('合辑副本音频内容校验不一致')
        if file_version(path)!=before or sha256_file(path)!=validation['file_hash']:raise Conflict('准备期间原文件变化')
        # A numeric name avoids path limits and cross-platform title collisions.
        destination=target/(f'{number:03d}'+path.suffix.lower())
        operations.append({'operation_id':plan_id+':p:'+str(number),'kind':'publish','source':str(prepared),'source_hash':sha256_file(prepared),'destination':destination.relative_to(root).as_posix(),'before_hash':None})
        originals.append({'path':str(path),'sha256':validation['file_hash'],'artist':tags['artist'],'file_id':row['file_id']})
        lrc=path.with_suffix('.lrc')
        if lrc.is_file():
            reject_reparse_chain(lrc,root)
            if lrc.stat().st_size>2_000_000:raise Conflict('歌词过大，请先核对')
            try:valid=validate_lrc(lrc.read_text('utf-8-sig'),validation.get('duration'))['valid']
            except UnicodeError:valid=False
            if not valid:raise Conflict('已有歌词格式异常，请先核对；原件保留')
            staged=prepared.with_suffix('.lrc');durable_copy(lrc,staged);signature=sha256_file(lrc);resources[str(lrc)]=signature
            operations.append({'operation_id':plan_id+':l:'+str(number),'kind':'publish','source':str(staged),'source_hash':signature,'destination':destination.with_suffix('.lrc').relative_to(root).as_posix(),'before_hash':None})
    # Publish the complete new set before moving any originals out of the watch root.
    for row in originals:
        path=Path(row['path'])
        operations.append({'operation_id':plan_id+':q:'+row['file_id'],'kind':'quarantine','source':str(path),'destination':plan_id+'/'+path.relative_to(root).as_posix(),'before_hash':row['sha256'],'file_id':row['file_id'],'quarantine_reason':'compilation_merge'})
        lrc=path.with_suffix('.lrc')
        if str(lrc) in resources:operations.append({'operation_id':plan_id+':ql:'+row['file_id'],'kind':'quarantine','source':str(lrc),'destination':plan_id+'/'+lrc.relative_to(root).as_posix(),'before_hash':resources[str(lrc)],'quarantine_reason':'compilation_merge'})
    plan={'schema_version':1,'plan_id':plan_id,'reviewed':False,'config_hash':digest(cfg),'operations':operations,'originals':originals,'resources':resources,'preview':{'kind':'manual_compilation','album':album,'albumartist':albumartist,'tracks':len(rows),'keep_directory':str(target),'preserve_track_artists':True,'recoverable':True,'reason':'仅统一专辑分组，每首歌曲原艺人和音频内容保留；原件进入可恢复隔离区'}}
    service.store.put('change_plans',plan_id,plan)
    return {'plan_id':plan_id,**plan['preview'],'music_writes':0,'work_copies':len(rows)}


def execute(service,cfg,args):
    plan=service.store.get('change_plans',str(args.get('plan_id','')))
    if not plan or plan.get('preview',{}).get('kind')!='manual_compilation' or plan['config_hash']!=digest(cfg):raise Conflict('合辑预览已过期，请重新预览')
    reviewed=copy.deepcopy(plan);reviewed['reviewed']=True
    previous=service.store.get('transactions',str(uuid.uuid5(uuid.NAMESPACE_URL,'musicops:transaction:'+plan['plan_id'])))
    if not previous:
        _seed_guard(service,cfg,[Path(r['path']) for r in plan['originals']])
        for row in plan['originals']:
            path=Path(row['path']);reject_reparse_chain(path,Path(cfg['paths']['library']))
            if not path.is_file() or sha256_file(path)!=row['sha256'] or read_tag_fields(path).get('artist')!=row['artist']:raise Conflict('原歌曲发生变化，请重新预览')
        for name,signature in plan['resources'].items():
            reject_reparse_chain(name,Path(cfg['paths']['library']))
            if sha256_file(name)!=signature:raise Conflict('原歌词发生变化，请重新预览')
    elif previous['state']!='COMMITTED':raise Conflict('已有未完成或已恢复的合辑事务，请先恢复或重新预览')
    tx=service._engine(cfg).execute(reviewed)
    result={'tx_id':tx['tx_id'],'state':tx['state']}
    service.store.put('evidence','duplicate-latest',{'kind':'duplicate_merge_history','transactions':[result]})
    service._post_transaction(cfg)
    if tx['state']=='COMMITTED':
        work=Path(cfg['paths']['work'])/plan['plan_id']
        for op in plan['operations']:
            if op['kind']!='publish':continue
            path=Path(op['source']);reject_reparse_chain(path,work)
            if path.is_file() and sha256_file(path)==op['source_hash']:path.unlink()
        if work.is_dir() and not any(work.iterdir()):work.rmdir()
    return {'transactions':[result],'message':f"已合并为《{plan['preview']['album']}》共{plan['preview']['tracks']}首，逐曲原艺人保留，原件可恢复。",'state':tx['state']}
