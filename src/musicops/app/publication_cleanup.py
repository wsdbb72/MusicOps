"""Remove only journaled, verified temporary copies after committed publication."""
from pathlib import Path
import re
from musicops.clients.http import PolicyBlocked
from musicops.library.index import sha256_file
from musicops.library.paths import reject_reparse_chain


def finish(workflows,job):
    cfg=job['config'];store=workflows.store
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,120}',job['job_id']):raise PolicyBlocked('临时清理任务 ID 无效')
    work=Path(job.get('engine_paths',cfg['paths'])['work'])/'prepared'/job['job_id']
    reject_reparse_chain(work);work=work.resolve()
    if job.get('prepared_root') and Path(job['prepared_root']).resolve()!=work:raise PolicyBlocked('整理副本根目录不属于本任务')
    record=store.get('download_tasks','qbt:'+job['job_id'])
    if record:
        from musicops.clients.qbt_live import live_client
        client=live_client(cfg['qbt'],cfg['paths']['source'],store,workflows.s.vault.get)
        try:result=client.archive_completed(job['job_id'],cfg['paths']['library'])
        finally:client.transport.close()
        if result['status']=='WAITING':
            workflows.put(job,stage='notifying',next_poll=__import__('time').time()+5,message=result['reason']);return False
        if result['status']!='OK':raise PolicyBlocked(result.get('reason','做种目录迁移未通过，临时文件保留'))
        job['source_path']=result['source_path'];job['seeding_path']=result['source_path']
    # Publication targets must still match their committed hashes before cleanup.
    for op in job.get('publication_plan',{}).get('operations',[]):
        target=Path(cfg['paths']['library'])/op['destination'];reject_reparse_chain(target)
        if not target.is_file() or sha256_file(target)!=op['source_hash']:raise PolicyBlocked('入库文件已改变，暂不清理原始下载')
    manifest=job.get('temporary_cleanup')
    if manifest is None:
        manifest={}
        for row in job.get('prepared',[]):
            path=Path(row['path']).resolve()
            if not path.is_relative_to(work):raise PolicyBlocked('整理副本不在本任务目录，拒绝清理')
            for item in [path,*map(Path,row.get('sidecars',[]))]:
                item=item.resolve()
                if not item.is_relative_to(work):raise PolicyBlocked('歌词副本越过本任务目录')
                if item.is_file():manifest[str(item)]=sha256_file(item)
            if not record and job['scene']!='complete':
                original=Path(row['original_path']).resolve();source=(Path(cfg['paths']['source'])/job['job_id']).resolve()
                # Legacy/client imports may live outside the owned UUID folder.
                # They can be imported but are never deleted by this cleanup.
                if original.is_relative_to(source):manifest[str(original)]=row['original_hash']
        if not record and job['scene']!='complete':
            # Download adapters keep an independent validation copy. Both owned
            # job trees become disposable only after the library commit verifies.
            for root in [Path(cfg['paths']['work'])/'streaming'/job['job_id'],Path(cfg['paths']['source'])/job['job_id']]:
                reject_reparse_chain(root)
                if root.is_dir():
                    for item in root.rglob('*'):
                        reject_reparse_chain(item)
                        if item.is_file():manifest.setdefault(str(item.resolve()),sha256_file(item))
        workflows.put(job,temporary_cleanup=manifest)
    for name,expected in manifest.items():
        path=Path(name);reject_reparse_chain(path)
        roots=[work]
        if not record and job['scene']!='complete':roots.extend([Path(cfg['paths']['work'])/'streaming'/job['job_id'],Path(cfg['paths']['source'])/job['job_id']])
        if not any(path.resolve().is_relative_to(root.resolve()) for root in roots):raise PolicyBlocked('清理记录越过本任务临时目录')
        if path.is_file():
            if sha256_file(path)!=expected:raise PolicyBlocked('临时文件已改变，拒绝删除')
            path.unlink()
    workflows.put(job,temporary_cleanup_done=True)
    return True
