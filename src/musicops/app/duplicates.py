"""Fresh, reversible song deduplication and same-release album consolidation."""
import collections
import copy
import math
import os
import re
import shutil
import subprocess
import uuid
from pathlib import Path
from musicops.config.schema import digest
from musicops.domain.errors import Conflict,PolicyBlocked,PathBlocked
from musicops.domain.identity import identities_same,strict_text
from musicops.library.catalog import album_directory
from musicops.library.index import file_version,sha256_file
from musicops.library.paths import reject_reparse_chain,safe_destination,reparse
from musicops.media.validation import MediaValidator,_run
from musicops.media.resources import validate_cover,validate_lrc
from musicops.media.tags import read_tag_fields,apply_tag_changes,embedded_covers,apply_cover_changes
from musicops.performance import bounded_map


def _release_key(row):
    if row.get('release_id'):return str(row['release_id'])
    proposal=row.get('metadata_proposal') or {}
    if not isinstance(proposal,dict):return ''
    if proposal.get('reviewed_by')!='current_user' or not proposal.get('release_id'):return ''
    if strict_text(proposal.get('raw_title'))!=strict_text(row.get('raw_title')) or strict_text(proposal.get('album'))!=strict_text(row.get('album')) or {strict_text(a) for a in proposal.get('artists',[])}!={strict_text(a) for a in row.get('artists',[])}:return ''
    try:
        first=float(proposal.get('duration') or 0);second=float(row.get('duration') or 0)
        if not math.isfinite(first) or not math.isfinite(second) or min(first,second)<=0 or abs(first-second)>3:return ''
    except (ValueError,TypeError,OverflowError):return ''
    return ('' if proposal.get('source')=='musicbrainz' else str(proposal.get('source'))+':')+str(proposal['release_id'])

def _album(row):
    return (_release_key(row),strict_text(row.get('album')),tuple(sorted(strict_text(a) for a in (row.get('albumartist') or row.get('artists',[])))),strict_text(row.get('edition')),strict_text(row.get('catalogue')))

def _slot(row):
    disc=str(row.get('disc') or '1').split('/')[0];track=str(row.get('track') or '').split('/')[0]
    if disc.isdigit() and track.isdigit() and int(track)>0:return (str(int(disc)),str(int(track)))
    return ('title',strict_text(row.get('raw_title')),tuple(sorted(strict_text(a) for a in row.get('artists',[]))))

def _rank(row,preferences=None):
    info=row.get('media_info',{});codec=str(info.get('codec_name',info.get('codec','')))
    lossless=codec in ('flac','alac','wav','aiff') or codec.startswith('pcm_')
    def number(key):
        try:return max(0,int(info.get(key) or 0))
        except (ValueError,TypeError):return 0
    formats=(preferences or {}).get('format_order',['flac','alac','wav','aiff','mp3','aac'])
    format_name=codec if codec in formats else Path(row['path']).suffix.lstrip('.').casefold()
    preference=len(formats)-formats.index(format_name) if format_name in formats else 0
    return (lossless,(number('bits_per_raw_sample') or number('bits_per_sample')) if lossless else 0,number('sample_rate') if lossless else 0,(number('bit_rate') or number('bitrate')) if not lossless else 0,preference)


def _pcm_identity(row,cfg):
    streams=[s for s in row['validation'].get('streams',[]) if s.get('codec_type')=='audio']
    if len(streams)!=1 or str(streams[0].get('codec_name','')).startswith('dsd'):raise Conflict('多音轨或 DSD 音频需要确认发行后再合并')
    stream=streams[0];fmt=str(stream.get('sample_fmt',''))
    if fmt not in ('u8','u8p','s16','s16p','s32','s32p','flt','fltp','dbl','dblp'):raise Conflict('未知或超高精度采样格式，保留原文件核对')
    # Preserve integer samples up to 32 bits and float samples up to 64 bits.
    codec='pcm_f64le' if fmt.startswith(('flt','dbl')) else 'pcm_s32le'
    path=Path(row['path']);before=file_version(path);reject_reparse_chain(path)
    try:
        out=_run([cfg['paths']['tools']['ffmpeg'],'-nostdin','-v','error','-threads','1','-i',str(path),'-map','0:a:0','-c:a',codec,'-f','hash','-hash','sha256','-'],cfg['worker']['decode_timeout'])
    except (OSError,subprocess.TimeoutExpired):raise Conflict('音频内容校验未完成，原文件保留') from None
    match=re.fullmatch(rb'SHA256=([a-fA-F0-9]{64})\s*',out.stdout)
    if out.returncode or not match:raise Conflict('无法取得完整音频内容证据，原文件保留')
    if before!=file_version(path) or sha256_file(path)!=row['sha256']:raise Conflict('音频内容校验时文件发生变化')
    return (stream.get('sample_rate'),stream.get('channels'),codec,match[1].lower())

def _resources(directories):
    result={}
    for root in directories:
        reject_reparse_chain(root)
        for current,dirs,names in os.walk(root,followlinks=False):
            dirs[:]=[name for name in dirs if not reparse(Path(current)/name)]
            for name in names:
                path=Path(current)/name
                if path.suffix.casefold() not in ('.lrc','.cue','.jpg','.jpeg','.png','.webp'):continue
                if reparse(path):raise Conflict('伴随资源包含链接，需要核对原始文件')
                result[str(path)]=list(file_version(path))
    return result

def scan(service,cfg):
    root=Path(cfg['paths']['library']).resolve()
    # Use the incremental index, not a full-library decode/hash pass.
    service._index_real_library({'lightweight':True},cfg)
    groups=collections.defaultdict(list)
    locked={r.get('identity') for r in service.store.list('field_locks')}|{r.get('identity') for r in service.store.list('suppressions') if r.get('active')}
    for row in service.store.list('library_files'):
        path=Path(row.get('path',''))
        if row.get('location_state','library')!='library' or row.get('read_error') or not path.is_relative_to(root):continue
        key=_album(row)
        if not key[1] or not key[2] or not row.get('raw_title') or not row.get('duration'):continue
        groups[key].append(row)
    candidates=[];truncated=False
    for key,rows in groups.items():
        service._checkpoint('duplicate_candidates')
        directories={str(album_directory(r,root)) for r in rows}
        slots=collections.Counter(_slot(r) for r in rows)
        if not any(n>1 for n in slots.values()) and not (key[0] and len(directories)>1):continue
        if len(candidates)>=1000:truncated=True;break
        candidate={'group_id':digest([key,sorted(r['file_id'] for r in rows)]),'album':rows[0]['album'],'artists':rows[0].get('albumartist') or rows[0]['artists'],'file_count':len(rows),'directory_count':len(directories),'state':'review','reason':'','files':[{'file_id':r['file_id'],'path':r['path'],'title':r['raw_title']} for r in rows[:50]]}
        if len(rows)>500:candidate['reason']='单组超过500首，保留原文件并等待分组核对'
        elif any(r['file_id'] in locked or r['path'] in locked for r in rows):candidate['reason']='包含人工锁定或抑制记录'
        else:
            try:
                plan=_prepare(service,cfg,rows,bool(key[0]))
                if not plan:candidate['reason']='同名资料不足以确认同一发行，或没有可安全合并的重复文件'
                else:
                    candidate.update(state='ready',plan_id=plan['plan_id'],reason=plan['preview']['reason'],keep_directory=plan['preview']['keep_directory'],removed_tracks=plan['preview']['removed_tracks'])
                    service.store.put('change_plans',plan['plan_id'],plan)
            except (Conflict,PolicyBlocked,PathBlocked,OSError,ValueError) as exc:candidate['reason']=str(exc)
        candidates.append(candidate)
    scan_id=str(uuid.uuid4())
    report={'truncated':truncated,'scan_id':scan_id,'groups':candidates,'ready_count':sum(c['state']=='ready' for c in candidates),'review_count':sum(c['state']=='review' for c in candidates),'music_writes':0,'policy':'同名不等于重复；不同发行/母带/声道保留。执行后旧副本进入可恢复隔离区。'}
    service.store.put('evidence','duplicate-scan:'+scan_id,{'kind':'duplicate_scan','config_hash':digest(cfg),'report':report})
    return report


def _prepare(service,cfg,rows,confirmed_release):
    for tool in ('ffmpeg','ffprobe'):
        executable=cfg['paths']['tools'].get(tool)
        if not executable or not shutil.which(executable):raise PolicyBlocked('缺少可用 FFmpeg / FFprobe，请按操作说明安装后重新保存连接设置；原文件未改动')
    root=Path(cfg['paths']['library']).resolve();plan_id='merge-'+str(uuid.uuid4())
    # Fast local candidates are verified in bounded parallel I/O/decode jobs.
    def inspect(row):
        r=copy.deepcopy(row);path=Path(r['path']);reject_reparse_chain(path,root)
        before=file_version(path)
        if (before[0],before[1])!=(r.get('size'),r.get('mtime_ns')):raise Conflict('候选文件发生变化，请重新扫描')
        check=MediaValidator(cfg['paths']['tools']['ffmpeg'],cfg['paths']['tools']['ffprobe'],cfg['worker']['decode_timeout']).validate(path)
        if check.get('decode_state')!='passed' or check.get('roon_compatible') is not True:raise Conflict('候选音频未通过完整解码或兼容性检查')
        if file_version(path)!=before:raise Conflict('验证时文件发生变化')
        fields=read_tag_fields(path)
        if strict_text((fields.get('title') or [''])[0])!=strict_text(r['raw_title']) or {strict_text(a) for a in fields.get('artist',[])}!={strict_text(a) for a in r['artists']} or strict_text((fields.get('album') or [''])[0])!=strict_text(r['album']):raise Conflict('文件标签在索引后发生变化')
        if not check.get('duration') or abs(check['duration']-r['duration'])>3:raise Conflict('实际音频时长与曲目资料不符')
        r.update(sha256=check['file_hash'],validation=check,media_info=check.get('media_info') or r.get('media_info',{}),_fields=fields);return r
    checked=[]
    for row in bounded_map(inspect,rows,workers=4):
        service._checkpoint('duplicate_audio_verified');checked.append(row)
    directories=collections.defaultdict(list)
    for row in checked:directories[album_directory(row,root)].append(row)
    canonical=max(directories,key=lambda p:(len({_slot(r) for r in directories[p]}),sum(_rank(r)[0] for r in directories[p]),-len(str(p)),str(p)))
    resource_versions=_resources(directories)
    if any(Path(p).suffix.casefold()=='.cue' for p in resource_versions):raise Conflict('包含 CUE 引用，保留原目录并交由核对')
    slots=collections.defaultdict(list)
    for row in checked:slots[_slot(row)].append(row)
    operations=[];kept=[];removed=[];destinations=set();resources={}
    def add_quarantine(path,file_id=None):
        reject_reparse_chain(path,root)
        name=path.relative_to(root).as_posix()
        operations.append({'operation_id':plan_id+':q:'+str(len(operations)),'kind':'quarantine','source':str(path),'destination':plan_id+'/'+name,'before_hash':sha256_file(path),'quarantine_reason':'duplicate',**({'file_id':file_id} if file_id else {})})
    def publish(source,destination,replace_hash=None):
        relative=destination.relative_to(root).as_posix()
        if relative.casefold() in destinations:raise Conflict('合并后的文件名冲突，需要核对')
        destinations.add(relative.casefold())
        if destination.exists():
            current=sha256_file(destination)
            if sha256_file(source)==current:return
            if not replace_hash or current!=replace_hash:raise Conflict('目标已存在不同内容，未覆盖')
        elif replace_hash:raise Conflict('待替换版本已丢失')
        if source.stat().st_size>(2_000_000 if source.suffix.casefold()=='.lrc' else 20_000_000) and source.suffix.casefold() in ('.lrc','.jpg','.jpeg','.png','.webp'):raise Conflict('资源文件过大，需要核对')
        if source.suffix.casefold()=='.lrc' and not validate_lrc(source.read_text(encoding='utf-8-sig')).get('valid'):raise Conflict('歌词格式未通过校验')
        if source.suffix.casefold() in ('.jpg','.jpeg','.png','.webp') and not validate_cover(source.read_bytes(),min_side=300).get('valid'):raise Conflict('封面格式或尺寸未通过校验')
        stage=safe_destination(Path(cfg['paths']['work']),plan_id+'/prepared/'+str(len(operations))+source.suffix)
        stage.parent.mkdir(parents=True,exist_ok=True);shutil.copy2(source,stage)
        content_hash=sha256_file(source)
        if sha256_file(stage)!=content_hash:raise Conflict('暂存副本校验失败')
        operations.append({'operation_id':plan_id+':p:'+str(len(operations)),'kind':'replace' if replace_hash else 'publish','source':str(stage),'source_hash':content_hash,'destination':relative,'before_hash':replace_hash})
    for slot,versions in slots.items():
        first=versions[0]
        if any(not identities_same(first,r) for r in versions[1:]):raise Conflict('同曲号存在歌名、歌手、时长或版本冲突')
        if len({str(r.get('media_info',{}).get('channels')) for r in versions})>1:raise Conflict('不同声道版本分别保留')
        if len({str(r.get('media_info',{}).get('codec_name','')).startswith('dsd') for r in versions})>1:raise Conflict('DSD 与 PCM 版本分别保留')
        if not confirmed_release and len({r['sha256'] for r in versions})>1:
            identities=list(bounded_map(lambda row:_pcm_identity(row,cfg),versions,workers=4))
            if len(set(identities))!=1:raise Conflict('本地标签相同但解码音频不同，需要先确认发行身份')
        winner=max(versions,key=lambda r:(_rank(r,cfg.get('quality')),album_directory(r,root)==canonical,-len(r['path'])))
        selected=Path(winner['path']);destination=selected
        if confirmed_release and album_directory(winner,root)!=canonical:
            relative=selected.relative_to(album_directory(winner,root));destination=safe_destination(canonical,relative)
        destination_version=next((r for r in versions if Path(r['path'])==destination),None)
        before_hash=destination_version['sha256'] if destination_version else None
        additions={}
        for field in ('albumartist','date','genre','composer','lyrics'):
            if winner['_fields'].get(field):continue
            options={tuple(r['_fields'][field]) for r in versions if r['_fields'].get(field)}
            if len(options)==1:additions[field]=list(next(iter(options)))
        source=selected
        covers=[] if embedded_covers(selected) else next((embedded_covers(r['path']) for r in versions if embedded_covers(r['path'])),[])
        if additions or covers:
            source=safe_destination(Path(cfg['paths']['work']),plan_id+'/metadata/'+str(len(operations))+selected.suffix)
            source.parent.mkdir(parents=True,exist_ok=True);shutil.copy2(selected,source)
            if additions:apply_tag_changes(source,additions)
            if covers:apply_cover_changes(source,covers[0])
        if source!=destination:publish(source,destination,before_hash)
        kept.append({'path':str(destination),'sha256':sha256_file(source),'before_hash':before_hash})
        # Missing lyrics are copied; conflicting lyrics are never discarded.
        lyrics={}
        for version in versions:
            p=Path(version['path']).with_suffix('.lrc')
            if p.is_file():reject_reparse_chain(p,root);lyrics[sha256_file(p)]=p
        if len(lyrics)>1:raise Conflict('重复曲目的歌词内容不同，需要核对')
        if lyrics:publish(next(iter(lyrics.values())),destination.with_suffix('.lrc'))
        for version in versions:
            path=Path(version['path'])
            if path==destination:continue
            add_quarantine(path,version['file_id']);removed.append(version['file_id'])
            lrc=path.with_suffix('.lrc')
            if lrc.is_file():add_quarantine(lrc)
    if not removed:return None
    # Preserve album artwork: fill missing names, never overwrite different images.
    for directory in directories:
        for path in directory.iterdir():
            if path.suffix.casefold() not in ('.jpg','.jpeg','.png','.webp'):continue
            reject_reparse_chain(path,root)
            name=path.name.casefold();signature=sha256_file(path)
            if name in resources and resources[name][0]!=signature:raise Conflict('专辑封面存在冲突，需要核对')
            resources[name]=(signature,path)
    for _,path in resources.values():publish(path,canonical/path.name)
    return {'schema_version':1,'plan_id':plan_id,'reviewed':False,'config_hash':digest(cfg),'operations':operations,'resource_directories':[str(p) for p in directories],'resource_versions':resource_versions,'keep_files':kept,'preview':{'kind':'album_merge' if confirmed_release else 'song_dedup','keep_directory':str(canonical),'removed_tracks':len(removed),'reason':'合并同一发行并补齐缺曲，保留较好音质与歌词/封面' if confirmed_release else '文件内容完全一致，合并重复歌曲','recoverable':True}}


def merge(service,cfg,args):
    scan=service.store.get('evidence','duplicate-scan:'+str(args.get('scan_id')))
    if not scan or scan['config_hash']!=digest(cfg):raise Conflict('扫描结果已过期，请重新扫描')
    selected=args.get('group_ids')
    if not isinstance(selected,list) or not selected:raise ValueError('请选择可以自动合并的项目')
    if len(selected)!=len(set(selected)):raise ValueError('重复的合并项目')
    groups={g['group_id']:g for g in scan['report']['groups']}
    if any(g not in groups or groups[g]['state']!='ready' for g in selected):raise PolicyBlocked('存在需要人工核对的项目，未执行合并')
    transactions=[]
    for ident in selected:
        service._checkpoint('before_duplicate_merge')
        plan=service.store.get('change_plans',groups[ident]['plan_id'])
        if not plan or plan.get('config_hash')!=digest(cfg):raise Conflict('合并计划已过期')
        previous=service.store.get('transactions',str(uuid.uuid5(uuid.NAMESPACE_URL,'musicops:transaction:'+plan['plan_id'])))
        if previous and previous['state']=='COMMITTED':
            checked=copy.deepcopy(plan);checked['reviewed']=True
            tx=service._engine(cfg).execute(checked);transactions.append({'tx_id':tx['tx_id'],'state':tx['state'],'group_id':ident});continue
        if previous:raise Conflict('已有事务记录，请先恢复或重新扫描')
        if _resources([Path(p) for p in plan['resource_directories']])!=plan['resource_versions']:raise Conflict('歌词、封面或 CUE 在扫描后发生变化，请重新扫描')
        # Quarantining a duplicate requires its surviving peer to remain unchanged.
        for item in plan['keep_files']:
            path=Path(item['path'])
            if path.exists():
                reject_reparse_chain(path,Path(cfg['paths']['library']))
                if sha256_file(path)!=(item.get('before_hash') or item['sha256']):raise Conflict('保留版本发生变化，请重新扫描')
            elif not any(op['kind'] in ('publish','replace') and Path(cfg['paths']['library'])/op['destination']==path for op in plan['operations']):raise Conflict('保留版本已丢失，拒绝移出重复文件')
        plan=copy.deepcopy(plan);plan['reviewed']=True
        tx=service._engine(cfg).execute(plan);transactions.append({'tx_id':tx['tx_id'],'state':tx['state'],'group_id':ident})
        service.store.put('evidence','duplicate-latest',{'kind':'duplicate_merge_history','scan_id':args['scan_id'],'transactions':transactions})
    service._post_transaction(cfg)
    return {'status':'OK','transactions':transactions,'merged_groups':len(transactions),'message':'合并完成，旧副本已进入可恢复隔离区；下载和做种源未改动。'}

def restore(service,cfg,args):
    from musicops.library.transactions import digest as plan_digest
    tx=service.store.get('transactions',str(args.get('tx_id')))
    plan=service.store.get('change_plans',tx['plan_id']) if tx else None
    if not tx or not plan or not str(plan.get('plan_id','')).startswith('merge-') or plan.get('preview',{}).get('kind') not in ('album_merge','song_dedup'):
        raise PolicyBlocked('只能通过此入口恢复 MusicOps 去重合并事务')
    reviewed=copy.deepcopy(plan);reviewed['reviewed']=True
    if plan_digest(reviewed)!=tx.get('plan_hash'):raise Conflict('合并记录与事务不一致，未恢复文件')
    result=service._engine(cfg).rollback(tx['tx_id']);service._post_transaction(cfg);return result
