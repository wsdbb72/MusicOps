"""Bounded, durable archival of routine events; file/token audits stay in SQLite."""
import datetime as dt,gzip,json,os,uuid
from pathlib import Path

ROUTINE=('task_queued','task_completed','task_safe_paused')

def archive(store,days=30,limit=1000):
    if type(days) is not int or not 7<=days<=3650:raise ValueError('常规日志保留天数须为 7–3650')
    cutoff=(dt.datetime.now(dt.timezone.utc)-dt.timedelta(days=days)).isoformat()
    with store.connection() as c:
        rows=c.execute("SELECT * FROM events WHERE phase IN (?,?,?) AND at<? ORDER BY seq LIMIT ?",(*ROUTINE,cutoff,limit)).fetchall()
    if not rows:return {'status':'OK','archived':0}
    from musicops.library.paths import reject_reparse_chain
    directory=store.path.parent/'event-archives';reject_reparse_chain(directory);directory.mkdir(exist_ok=True)
    target=directory/(str(rows[0]['seq'])+'-'+str(rows[-1]['seq'])+'-'+uuid.uuid4().hex+'.jsonl.gz')
    with target.open('xb') as raw:
        with gzip.GzipFile(fileobj=raw,mode='wb') as compressed:
            for row in rows:compressed.write((json.dumps(dict(row),ensure_ascii=False)+'\n').encode('utf-8'))
        raw.flush();os.fsync(raw.fileno())
    # Never delete until the complete archive is durable. If interrupted here,
    # duplicate archive files are harmless; the source records remain intact.
    with store.connection() as c:c.executemany('DELETE FROM events WHERE seq=?',[(r['seq'],) for r in rows])
    store.event('events_archived',{'count':len(rows),'first_seq':rows[0]['seq'],'last_seq':rows[-1]['seq'],'archive':str(target)})
    return {'status':'OK','archived':len(rows),'archive':str(target),'more_possible':len(rows)==limit}
