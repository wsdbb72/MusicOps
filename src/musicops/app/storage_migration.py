"""Resume older job snapshots without creating fresh audio in profile storage."""
import re
from pathlib import Path
from musicops.library.storage import storage_paths,seed_root
from musicops.library.paths import reject_reparse_chain
from musicops.library.transactions import durable_copy
from musicops.library.index import sha256_file
from musicops.clients.http import PolicyBlocked


def prepare(workflows,job):
    cfg=job['config']
    if cfg['offline'] or not cfg['workflow'].get('setup_done'):return True
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,120}',job['job_id']):return True
    desired=storage_paths(cfg['paths']['library']);record=workflows.store.get('download_tasks','qbt:'+job['job_id'])
    mappings={}
    if record:
        old=Path(record['source_path']).resolve()
        for intent in (record.get('relocation'),record.get('active_relocation')):
            if intent and intent.get('state')=='VERIFIED':mappings[intent['from']]=record['source_path']
        allowed=[Path(desired['source'])/job['job_id'],seed_root(cfg['paths']['library'])/job['job_id']]
        if old not in [p.resolve() for p in allowed]:
            from musicops.clients.qbt_live import live_client
            client=live_client(cfg['qbt'],desired['source'],workflows.store,workflows.s.vault.get)
            try:
                if record.get('relocation'):result=client.archive_completed(job['job_id'],cfg['paths']['library'])
                elif record.get('active_relocation') and record['active_relocation'].get('state')!='VERIFIED':result=client.relocate_active(job['job_id'],cfg['paths']['library'])
                else:
                    polled=client.poll(job['job_id'])
                    result=client.archive_completed(job['job_id'],cfg['paths']['library']) if polled.get('complete') else client.relocate_active(job['job_id'],cfg['paths']['library'])
            finally:client.transport.close()
            if result['status']=='WAITING':
                workflows.put(job,stage=job.get('resume_stage',job['stage']),next_poll=__import__('time').time()+5,message='正在将旧下载迁移到音乐库同级目录');return False
            if result['status']!='OK':raise PolicyBlocked(result.get('reason','旧下载目录未通过迁移核对'))
            mappings[str(old)]=result['source_path']
    old_source=Path(cfg['paths']['source'])/job['job_id'];new_source=Path(desired['source'])/job['job_id']
    old_stream=Path(cfg['paths']['work'])/'streaming'/job['job_id'];new_stream=Path(desired['work'])/'streaming'/job['job_id']
    pairs=[] if record else [(old_source,new_source)]
    pairs.append((old_stream,new_stream))
    for source,target in pairs:
        if source.resolve()==target.resolve():continue
        reject_reparse_chain(source);reject_reparse_chain(target)
        if source.is_dir():
            for path in source.rglob('*'):
                reject_reparse_chain(path)
                if not path.is_file():continue
                workflows.checkpoint(job,'relocate_cached_download')
                dest=target/path.relative_to(source);expected=sha256_file(path)
                if dest.exists():
                    if sha256_file(dest)!=expected:raise PolicyBlocked('旧下载迁移目标发生冲突，原文件保留')
                else:durable_copy(path,dest)
                if sha256_file(dest)!=expected or sha256_file(path)!=expected:raise PolicyBlocked('旧下载迁移校验失败，原文件保留')
                path.unlink()
        mappings[str(source)]=str(target)
    def rebase(value):
        if isinstance(value,str):
            for old,new in mappings.items():
                if value==old or value.startswith(old+'\\') or value.startswith(old+'/'):return new+value[len(old):]
        if isinstance(value,list):return [rebase(v) for v in value]
        if isinstance(value,dict):return {k:rebase(v) for k,v in value.items()}
        return value
    migrated=rebase(job);migrated['config']['paths'].update(desired)
    if migrated!=job:job.clear();job.update(migrated);workflows.put(job)
    return True


def recover_serialization_failures(workflows):
    """Recover only the known native-reference failure, preserving user pauses."""
    import json
    with workflows.store.connection() as connection:
        events=connection.execute("SELECT payload FROM events WHERE phase='workflow_error' AND json_extract(payload,'$.error_class')='ValueError'").fetchall()
    affected=set()
    for row in events:
        event=json.loads(row[0]);frames=event.get('frames',[])
        if any(f.get('file')=='streaming_fallback.py' for f in frames) and any(f.get('file')=='encoder.py' and f.get('function')=='iterencode' for f in frames):affected.add(event.get('job_id'))
    changes=[]
    for job_id in affected:
        job=workflows.store.get('evidence','workflow:'+str(job_id))
        if (not job or job.get('hidden') or job.get('cancel_requested') or job.get('fallback_serialization_recovered') or job.get('stage')!='failed'
            or job.get('resume_stage')!='fallback' or job.get('error_class')!='ValueError'):continue
        job.update(stage='fallback',error_class=None,next_poll=0,fallback_serialization_recovered=True,message='已修复网易云任务保存异常；从已验证缓存恢复，未完成曲目继续处理')
        changes.append(('workflow:'+job_id,job))
    if changes:
        workflows.store.put_many('evidence',changes)
        workflows.store.event('fallback_serialization_recovered',{'count':len(changes),'cached_audio_preserved':True,'torrent_operations':0})
    return len(changes)
