"""One current action per album; completed prerequisite stages remain history."""
import collections
import json
from musicops.app.task_filters import STATES, TERMINAL


def listing(workflows, offset, state, selection, query=''):
    if state not in ('all', 'active', *STATES):
        raise ValueError('任务分类无效')
    with workflows.store.connection() as connection:
        rows = connection.execute("SELECT * FROM workflow_overview WHERE job_kind!='activity' ORDER BY created_at DESC,id").fetchall()
        chains = collections.defaultdict(list)
        for row in rows:
            chains[row['root_id']].append(dict(row))
        priority = {'album': 2, 'search': 1, 'identification': 0}
        current = [max(chain, key=lambda r: (priority.get(r['task_type'], 0), r['created_at'], r['id'])) for chain in chains.values()]
        # Deleting a successor must not resurrect its completed prerequisite.
        # Prerequisites without successors are stage history, never an import.
        current = [r for r in current if not r['hidden'] and not (r['stage']=='completed' and (r['task_type'] in ('identification','search') or r['job_kind']=='batch'))]
        current.sort(key=lambda r: (r['created_at'], r['id']), reverse=True)
        order={'running':0,'downloading':0,'validating':0,'publishing':0,'retiring':0,'notifying':0,'requesting':1,'fallback':1,'searching':2,'identifying':3,'snapshotting':3,'metadata':4,'enrich':4}
        current.sort(key=lambda r: (bool(r['paused']),order.get(r['stage'],5)))
        if query.strip():current=[r for r in current if query.strip().casefold() in r['label'].casefold()]
        def matches(row):
            if state == 'all': return True
            if state == 'active': return row['stage'] not in TERMINAL
            return row['stage'] in STATES[state]
        filtered = [row for row in current if matches(row)]
        if selection:
            ids = [r['job_id'] for r in filtered if r['job_kind'] != 'batch']
            return {'job_ids': ids, 'total': len(ids)}
        page = filtered[offset:offset + 200]
        payloads = {}
        if page:
            placeholders = ','.join('?' for _ in page)
            for row in connection.execute("SELECT id,json_remove(payload,'$.config','$.targets','$.old_files','$.prepared','$.choices','$.outcomes','$.publication_plan','$.catalog_choices','$.lookup_links','$.engine_paths','$.selected') FROM evidence WHERE id IN (" + placeholders + ')', [r['id'] for r in page]):
                payloads[row[0]] = json.loads(row[1])
    jobs = []
    for row in page:
        job = workflows.public_row(payloads[row['id']])
        chain = sorted((r for r in chains[row['root_id']] if not r['hidden']), key=lambda r: priority.get(r['task_type'], 0))
        job['chain'] = [{k: r[k] for k in ('job_id','stage','task_type','paused')} for r in chain]
        job['overview'] = True
        jobs.append(job)
    counts = collections.Counter(row['stage'] for row in current)
    active = [r for r in current if r['stage'] not in TERMINAL]
    return {'jobs': jobs, 'view': 'albums', 'offset': offset, 'total': len(filtered),
            'all_total': len(current), 'active': sum(not r['paused'] for r in active),
            'paused_count': sum(bool(r['paused']) for r in active), 'total_active': len(active),
            'finished': counts['completed'], 'preparing_count': counts['preparing'],
            'scene_active_counts':dict(collections.Counter(row['scene'] for row in active)),
            'stage_counts': dict(counts), 'token_prompts': [r['job_id'] for r in current if r['stage']=='needs_review' and r['review_kind']=='token'][:100]}
