"""Durable, revision-keyed AI work. A GUI callback never owns the backlog."""
import collections
import json
from musicops.app.assistance import review_revision
from musicops.config.schema import digest
from musicops.state.store import utcnow


def register(service, job, apply_verified=True, retry=False):
    if job.get('hidden') or job.get('job_kind') in ('batch', 'activity') or job.get('stage') not in ('needs_review', 'failed', 'skipped'):
        return False
    # Spending without a personal FL token always requires the human's choice.
    if job.get('review_kind') in ('token','album_scope') or job.get('version_choice'):
        return False
    if not retry:
        with service.store.connection() as connection:
            rounds=connection.execute("SELECT COUNT(*) FROM evidence WHERE id LIKE 'ai-review:%' AND json_extract(payload,'$.job_id')=?",(job['job_id'],)).fetchone()[0]
        if rounds>=3:return False
    revision = review_revision(job)
    key = 'ai-review:' + digest({'job': job['job_id'], 'revision': revision})
    previous = service.store.get('evidence', key)
    if previous and (not retry or previous['state'] in ('pending', 'running')):
        return False
    service.store.put('evidence', key, dict(kind='ai_review_queue', job_id=job['job_id'],
        revision=revision, state='pending', apply_verified=bool(apply_verified), created_at=utcnow()))
    job['ai_review_state']='pending'
    service.store.put('evidence','workflow:'+job['job_id'],job)
    return True


def submit(service, args):
    if args.get('all') is True:
        jobs = service.workflows.rows()
    else:
        from musicops.app.workflow_actions import selection
        ids = selection(args.get('job_ids'), 10000)
        jobs = [service.store.get('evidence', 'workflow:' + key) for key in ids]
    count=0;retry=args.get('retry',False);now=utcnow()
    # Thousands of registrations form one durable transaction, rather than
    # opening/fsyncing the SQLite database twice for every album over IPC.
    with service.store.connection() as connection:
        previous={row['id']:json.loads(row['payload']) for row in connection.execute("SELECT id,payload FROM evidence WHERE id LIKE 'ai-review:%'")}
        rounds=collections.Counter(row['job_id'] for row in previous.values())
        connection.execute('BEGIN IMMEDIATE')
        for job in jobs:
            if not job or job.get('hidden') or job.get('job_kind') in ('batch','activity') or job.get('stage') not in ('needs_review','failed','skipped') or job.get('review_kind') in ('token','album_scope') or job.get('version_choice'):continue
            revision=review_revision(job);key='ai-review:'+digest({'job':job['job_id'],'revision':revision})
            existing=previous.get(key)
            retry_entry=retry or bool(existing and (existing['state'] in ('failed','blocked','recovery_required') or (job.get('ai_assistance') or {}).get('status')=='FAILED'))
            if existing and (not retry_entry or existing['state'] in ('pending','running')) or not retry_entry and rounds[job['job_id']]>=3:continue
            entry=dict(kind='ai_review_queue',job_id=job['job_id'],revision=revision,state='pending',
                apply_verified=bool(args.get('apply_verified',True)),created_at=now)
            job['ai_review_state']='pending'
            for identifier,value in ((key,entry),('workflow:'+job['job_id'],job)):
                value.setdefault('schema_version',1);value.setdefault('id',identifier)
                connection.execute('INSERT INTO evidence VALUES (?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at',
                    (identifier,json.dumps(value,ensure_ascii=False),now))
            previous[key]=entry;rounds[job['job_id']]+=not bool(existing);count+=1
    return {'status': 'OK', 'registered': count, 'message': f'已登记 {count} 项 AI 辅助核对；后台自动分批处理，关闭窗口后继续，已有结果不会重复调用'}


def schedule(service):
    config=service.config.current()['values']
    if config['offline'] or not config['ai'].get('enabled'):
        return False
    with service.store.connection() as connection:
        rows = connection.execute("SELECT e.id,e.payload FROM evidence AS e LEFT JOIN workflow_overview AS w ON w.id='workflow:'||json_extract(e.payload,'$.job_id') WHERE e.id LIKE 'ai-review:%' AND json_extract(e.payload,'$.state')='pending' AND coalesce(w.paused,0)=0 ORDER BY e.updated_at,e.id LIMIT 32").fetchall()
    for row in rows:
        entry=json.loads(row['payload'])
        job=service.store.get('evidence','workflow:'+entry['job_id'])
        if not job or job.get('hidden') or job.get('stage') not in ('needs_review','failed','skipped') or review_revision(job)!=entry['revision']:
            service.store.put('evidence',row['id'],{**entry,'state':'stale'})
            continue
        break
    else:return False
    result = service.enqueue('assist_workflows', {'job_ids': [entry['job_id']],
        'apply_verified': entry['apply_verified'], '_review_queue_id': row['id']})
    service.store.put('evidence', row['id'], {**entry, 'state': 'running', 'task_id': result['task_id']})
    service.store.put('evidence','workflow:'+job['job_id'],{**job,'ai_review_state':'running'})
    return True


def recover(service):
    """Give each interrupted review exactly one owner after a restart.

    A queued task survives restarts, so re-pending its entry as well would
    let the scheduler enqueue a second AI call for the same revision. A task
    that already finished keeps its result instead of being replayed.
    """
    with service.store.connection() as connection:
        rows = connection.execute("SELECT id,payload FROM evidence WHERE id LIKE 'ai-review:%' AND json_extract(payload,'$.state')='running'").fetchall()
    for row in rows:
        entry=json.loads(row['payload'])
        task=service.store.get('tasks',entry['task_id']) if entry.get('task_id') else None
        state=task.get('state') if task else None
        if state in ('completed','failed','blocked','cancelled'):
            finish(service,task);continue
        if state in ('queued','paused') and not task.get('started_at'):
            # The scheduler re-checks the revision before it runs anything;
            # the superseded task must not call AI for a possibly stale job.
            task.update(state='cancelled',cancel_requested=True,result={'message':'AI 核对已重新排队，避免重启后重复调用'})
            service.store.put('tasks',task['task_id'],task)
        service.store.put('evidence', row['id'], {**entry, 'state': 'pending'})
        job=service.store.get('evidence','workflow:'+entry['job_id'])
        if job and job.get('ai_review_state')=='running':service.store.put('evidence','workflow:'+entry['job_id'],{**job,'ai_review_state':'pending'})


def finish(service, task):
    # A late AI result must not overwrite a simultaneous delete/cancel/pause.
    with service.mutex:
        key = task.get('args', {}).get('_review_queue_id')
        entry = service.store.get('evidence', key) if key else None
        if entry:
            job=service.store.get('evidence','workflow:'+entry['job_id'])
            if job:service.store.put('evidence','workflow:'+entry['job_id'],{**job,'ai_review_state':task['state']})
            service.store.put('evidence', key, {**entry, 'state': task['state'], 'finished_at': utcnow()})
