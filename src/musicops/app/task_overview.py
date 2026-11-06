"""One current action per album; completed prerequisite stages remain history."""
import collections
import json
from musicops.app.task_filters import STATES, TERMINAL


def listing(workflows, offset, state, selection, query=''):
    if state not in ('all', 'active', *STATES):
        raise ValueError('任务分类无效')
    where="hidden=0 AND NOT (coalesce(stage,'')='completed' AND (coalesce(task_type,'') IN ('identification','search') OR job_kind='batch'))"
    args=()
    if query.strip():where+=' AND instr(musicops_casefold(label),?)>0';args=(query.strip().casefold(),)
    filtered=where;filtered_args=args
    if state!='all':
        stages=tuple(TERMINAL if state=='active' else STATES[state])
        filtered+=' AND coalesce(stage,\'\') '+('NOT IN' if state=='active' else 'IN')+' ('+','.join('?' for _ in stages)+')'
        filtered_args=(*args,*stages)
    order="paused,CASE stage WHEN 'running' THEN 0 WHEN 'downloading' THEN 0 WHEN 'validating' THEN 0 WHEN 'publishing' THEN 0 WHEN 'retiring' THEN 0 WHEN 'notifying' THEN 0 WHEN 'requesting' THEN 1 WHEN 'fallback' THEN 1 WHEN 'searching' THEN 2 WHEN 'identifying' THEN 3 WHEN 'snapshotting' THEN 3 WHEN 'metadata' THEN 4 WHEN 'enrich' THEN 4 ELSE 5 END,created_at DESC,workflow_id DESC"
    with workflows.store.connection() as connection:
        if selection:
            rows=connection.execute("SELECT job_id FROM album_workflow_heads WHERE "+filtered+" AND job_kind!='batch' ORDER BY "+order,filtered_args).fetchall()
            ids=[r[0] for r in rows];return {'job_ids':ids,'total':len(ids)}
        total=connection.execute('SELECT COUNT(*) FROM album_workflow_heads WHERE '+filtered,filtered_args).fetchone()[0]
        page=[dict(row) for row in connection.execute('SELECT * FROM album_workflow_heads WHERE '+filtered+' ORDER BY '+order+' LIMIT 200 OFFSET ?',(*filtered_args,offset))]
        counts=collections.Counter();scene_counts=collections.Counter();active_count=paused_count=0
        for row in connection.execute('SELECT stage,scene,paused,COUNT(*) FROM album_workflow_heads WHERE '+where+' GROUP BY stage,scene,paused',args):
            counts[row[0]]+=row[3]
            if row[0] not in TERMINAL:
                scene_counts[row[1]]+=row[3]
                if row[2]:paused_count+=row[3]
                else:active_count+=row[3]
        token_prompts=[r[0] for r in connection.execute("SELECT job_id FROM album_workflow_heads WHERE "+where+" AND stage='needs_review' AND review_kind='token' ORDER BY "+order+' LIMIT 100',args)]
        payloads={};chains=collections.defaultdict(list)
        if page:
            placeholders=','.join('?' for _ in page)
            payloads={r[0]:json.loads(r[1]) for r in connection.execute('SELECT id,summary FROM workflow_overview WHERE id IN ('+placeholders+')',[r['workflow_id'] for r in page])}
            roots=[r['root_id'] for r in page if r['root_id'] is not None]
            root_clause='root_id IN ('+','.join('?' for _ in roots)+')' if roots else '0'
            if any(r['root_id'] is None for r in page):root_clause='('+root_clause+' OR root_id IS NULL)'
            for row in connection.execute("SELECT root_id,job_id,stage,task_type,paused FROM workflow_overview WHERE hidden=0 AND job_kind!='activity' AND "+root_clause+' ORDER BY task_priority,created_at DESC,id DESC',roots):chains[row['root_id']].append(dict(row))
    jobs = []
    for row in page:
        job = workflows.public_row(payloads[row['workflow_id']])
        chain = chains[row['root_id']]
        job['chain'] = [{k: r[k] for k in ('job_id','stage','task_type','paused')} for r in chain]
        job['overview'] = True
        jobs.append(job)
    return {'jobs': jobs, 'view': 'albums', 'offset': offset, 'total': total,
            'all_total': sum(counts.values()), 'active': active_count,
            'paused_count': paused_count, 'total_active': active_count+paused_count,
            'finished': counts['completed'], 'preparing_count': counts['preparing'],
            'scene_active_counts':dict(scene_counts),
            'stage_counts': dict(counts), 'token_prompts': token_prompts}
