"""Task record removal and deliberate retries; music/download evidence survives."""
import copy
from musicops.clients.http import PolicyBlocked
from musicops.state.store import utcnow

RETRY_STAGES = {'identifying', 'snapshotting', 'searching', 'requesting', 'downloading',
                'fallback', 'metadata', 'enrich', 'validating', 'notifying'}
PATH_KEYS = ('library', 'source', 'work', 'backup', 'quarantine')


def selection(job_ids, limit=500):
    if not isinstance(job_ids, list) or not 1 <= len(job_ids) <= limit or any(
            not isinstance(j, str) or not j or len(j) > 160 for j in job_ids):
        raise ValueError(f'一次请选择 1 至 {limit} 个有效任务')
    return list(dict.fromkeys(job_ids))


def current_config(workflows, job):
    cfg = workflows.s.config.current()['values']
    if any(cfg['paths'][key] != job['config']['paths'][key] for key in PATH_KEYS):
        raise PolicyBlocked('音乐目录已改变，请从对应入口重新创建任务')
    return copy.deepcopy(cfg)


def descendants(workflows, job):
    rows = workflows.rows(include_batches=True)
    found = {job['job_id']}
    if job.get('job_kind') == 'batch':
        found.update(r['job_id'] for r in rows if r.get('batch_id') == job.get('batch_id'))
    changed = True
    while changed:
        before = len(found)
        # identification.search_job_id points FORWARD to its child; the same
        # field on a processing record points BACK to the parent search.
        found.update(r['job_id'] for r in rows if r.get('identification_job_id') in found
                     or r.get('task_type') != 'identification' and r.get('search_job_id') in found)
        changed = len(found) != before
    return [r for r in rows if r['job_id'] in found]


def remove(workflows, job):
    """Tombstone, never erase dependencies, FL ledgers, transactions or files."""
    rows = descendants(workflows, job)
    for row in rows:
        workflows.cancel(row['job_id'])
        latest = workflows.store.get('evidence', 'workflow:' + row['job_id'])
        workflows.put(latest, hidden=True, removed_at=utcnow())
    return {'removed_job_ids': [r['job_id'] for r in rows], 'affected_count': len(rows), 'message': '已移除任务记录及后续任务；音乐文件、下载与做种保留'}


def retry(workflows, job, *, query=None, sites=None, allow_review_search=False,allow_legacy=False):
    if job.get('hidden'):
        raise PolicyBlocked('任务已删除')
    if job['stage'] not in {'failed', 'needs_review', 'skipped'}:
        raise PolicyBlocked('仅重试失败、待核对或未找到资源的任务')
    if job.get('retry_job_id'):
        prior = workflows.store.get('evidence', 'workflow:' + job['retry_job_id'])
        if prior and not prior.get('hidden'):
            raise PolicyBlocked('已创建重试任务，请操作该任务')
    if job.get('job_kind') in ('batch', 'activity'):
        task = workflows.store.get('tasks', job.get('start_task_id'))
        if not task or task['command'] not in {'start_workflow', 'manual_album_search', 'identify_metadata', 'resolve_metadata', 'assist_workflows', 'index_library'}:
            raise PolicyBlocked('请从对应入口重新发起此任务')
        if job.get('job_kind') == 'batch' and task['args'].get('_batch_job_id') != job['job_id']:
            raise PolicyBlocked('旧版准备任务请返回对应入口重新开始，不会自动重放')
        if task['state'] in {'queued', 'running', 'paused'}:
            raise PolicyBlocked('上一次请求尚未结束，请稍后重试')
        args = copy.deepcopy(task['args']); args.pop('_batch_job_id', None)
        if query is not None and task['command'] == 'manual_album_search':
            args = {'query': validate_query(query), 'mode': 'literal'}
            if sites is not None: args['sites'] = validate_sites(sites, workflows.s.config.current()['values'])
        accepted = workflows.s.enqueue(task['command'], args)
        new_id = accepted.get('job_id') or 'activity:' + accepted['task_id']
        workflows.put(job, retry_job_id=new_id, message='已创建重试任务，原记录保留')
        return {'retry_job_id': new_id, 'message': '已重新排队，使用当前连接设置'}
    children = descendants(workflows, job)
    if any(row['job_id'] != job['job_id'] and not row.get('hidden') for row in children):
        raise PolicyBlocked('已有后续任务，请重试失败的后续任务，避免重复下载')
    kind = job.get('review_kind')
    if kind in {'metadata', 'candidate', 'numbering', 'cover', 'edition', 'locks', 'resources'} and not (allow_review_search and job.get('task_type')=='search' and kind=='candidate'):
        raise PolicyBlocked('此任务需要选择或补充资料，请打开核对窗口')
    if job.get('legacy_batch_requires_review') and not allow_legacy:
        raise PolicyBlocked('旧版队列请从对应入口重新创建，避免重放历史下载')
    stage = 'searching' if job.get('task_type') == 'search' or kind == 'search' else job.get('resume_stage')
    if stage=='running' and allow_legacy:
        # Old versions overwrote the actual checkpoint with the UI-only state.
        # Only an explicit Continue action may recover such records.
        if job.get('selected'):
            stage='downloading' if workflows.store.get('download_tasks','qbt:'+job['job_id']) else 'requesting'
        elif job.get('prepared'):stage='validating'
        elif job.get('scene')=='playlist' and job.get('targets'):stage='identifying'
        elif job.get('scene') in ('upgrade','fill') and job.get('old_files'):stage='snapshotting'
        elif job.get('scene')=='complete' and job.get('old_files'):stage='metadata'
    if not stage and job.get('task_type') == 'identification':
        stage = 'identifying' if job['scene'] == 'playlist' else 'snapshotting'
    if stage not in RETRY_STAGES:
        raise PolicyBlocked('请先核对文件事务或原音源；此阶段不能直接重放')
    cfg = current_config(workflows, job)
    changes = dict(config=cfg, stage=stage, paused=False, next_poll=0,
                   review_kind=None, detail='', error_class=None, ai_assistance=None,
                   retry_count=int(job.get('retry_count', 0)) + 1, message='已重新排队，先核对既有状态后重试')
    if allow_legacy:changes.update(legacy_batch_requires_review=False)
    if stage == 'searching':
        changes.update(choices=[], outcomes={}, search_summary={}, ai_search_attempted=False,progress=0)
        if query is not None:
            changes['search_query'] = validate_query(query)
        if sites is not None:
            changes['search_sites'] = validate_sites(sites, cfg)
    workflows.put(job, **changes)
    return {'message': '已重新排队；沿用下载与 FL 记录，不重复创建任务'}


def validate_query(query):
    if not isinstance(query, str) or not query.strip() or len(query) > 300 or any(ord(c) < 32 for c in query):
        raise ValueError('搜索词须为 1 至 300 个字符')
    return query.strip()


def validate_sites(sites, cfg):
    if not isinstance(sites, list) or not sites or len(sites) > 4 or any(
            site not in cfg['workflow']['source_order'] or not cfg['providers'][site]['enabled'] for site in sites):
        raise ValueError('请至少选择一个已启用站点')
    return list(dict.fromkeys(sites))


def bulk(workflows, action, job_ids):
    ids = selection(job_ids); results = []
    with workflows.s.mutex:
        for job_id in ids:
            job = workflows.store.get('evidence', 'workflow:' + job_id)
            if not job or job.get('hidden'):
                results.append({'job_id': job_id, 'status': 'NOT_FOUND', 'message': '任务不存在或已删除'}); continue
            try:
                result = remove(workflows, job) if action == 'delete' else retry(workflows, job)
                results.append({'job_id': job_id, 'status': 'OK', **result})
            except (ValueError, PolicyBlocked) as exc:
                results.append({'job_id': job_id, 'status': 'SKIPPED', 'message': str(exc)})
        counts = {key: sum(row['status'] == key for row in results) for key in ('OK', 'SKIPPED', 'NOT_FOUND')}
        workflows.store.event('workflow_bulk_' + action, {'selected_count': len(ids), 'succeeded': counts['OK'], 'skipped': counts['SKIPPED'], 'failed': counts['NOT_FOUND']})
    return {'status': 'OK', 'results': results, 'succeeded': counts['OK'], 'skipped': counts['SKIPPED'],
            'failed': counts['NOT_FOUND'], 'message': f"成功 {counts['OK']} 项，跳过 {counts['SKIPPED']} 项，未找到 {counts['NOT_FOUND']} 项"}
