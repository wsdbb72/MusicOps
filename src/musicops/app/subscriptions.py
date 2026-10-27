"""Subscribed playlists: re-read on a timer and import only newly added tracks.

A subscription records every track it has already seen. Each check re-reads
the playlist with the same reader as a manual import, and only targets that
were never seen go through the ordinary playlist workflow, which still skips
audio already in the library. Tracks that disappear from the playlist are
never removed from the library. A check is a queued task, so it survives
restarts and shares the worker's safe points and cancellation.
"""
from __future__ import annotations
import copy,hashlib,time,uuid
from musicops.state.store import utcnow
from musicops.clients.http import PolicyBlocked

PREFIX='playlist_subscription:'
MIN_HOURS,MAX_HOURS,DEFAULT_HOURS=1,168,6
# Another playlist batch, or no worker capacity: try again soon instead of waiting a full interval.
DEFER_SECONDS=600
MAX_SUBSCRIPTIONS=50
MAX_HISTORY=20


def _key(platform,text):
    return hashlib.sha256((platform+'\0'+text.strip()).encode('utf-8')).hexdigest()[:32]


def _public(row):
    return {k:row.get(k) for k in ('subscription_id','name','playlist_input','platform','enabled','interval_hours','prefer_full_album',
                                   'created_at','last_checked_at','next_check_at','last_status','last_reason','last_new_count',
                                   'last_batch_id','song_count','history')}|{'known_count':len(row.get('known_target_ids',[]))}


def listing(service):
    rows=[service.store.get('evidence',PREFIX+sid) for sid in _ids(service)]
    return {'subscriptions':[_public(r) for r in sorted((r for r in rows if r),key=lambda r:r.get('created_at',''))],'network_used':False}


def _ids(service):
    with service.store.connection() as c:
        return [row[0][len(PREFIX):] for row in c.execute("SELECT id FROM evidence WHERE id LIKE ?",(PREFIX+'%',))]


def save(service,payload):
    """Create or update one subscription; creating one schedules an immediate first check."""
    if not isinstance(payload,dict):raise ValueError('订阅参数无效')
    sid=payload.get('subscription_id')
    row=service.store.get('evidence',PREFIX+sid) if sid else None
    if sid and not row:raise ValueError('订阅不存在或已删除')
    if row is None:
        text=payload.get('playlist_input');platform=payload.get('platform','auto')
        if not isinstance(text,str) or not text.strip() or len(text)>2000:raise ValueError('请输入歌单链接或 ID')
        from musicops.providers.playlist_input import parse_playlist_locator
        parse_playlist_locator(text,platform)  # Reject unreadable links now, not at the first timed check.
        sid=_key(platform,text)
        if service.store.get('evidence',PREFIX+sid):raise ValueError('已订阅这个歌单')
        if len(_ids(service))>=MAX_SUBSCRIPTIONS:raise ValueError(f'最多订阅 {MAX_SUBSCRIPTIONS} 个歌单')
        import_existing=payload.get('import_existing',False)
        if type(import_existing) is not bool:raise ValueError('“现有曲目也入库”须为勾选状态')
        row={'kind':'playlist_subscription','subscription_id':sid,'playlist_input':text.strip(),'platform':platform,'name':'',
             'enabled':True,'interval_hours':DEFAULT_HOURS,'prefer_full_album':False,'created_at':utcnow(),
             # Without a baseline the first check only records what is already there.
             'baseline_pending':not import_existing,'known_target_ids':[],'next_check_at':0,'history':[]}
    for field,kind in (('enabled',bool),('prefer_full_album',bool)):
        if field in payload:
            if type(payload[field]) is not kind:raise ValueError('订阅开关须为勾选状态')
            row[field]=payload[field]
    if 'interval_hours' in payload:
        hours=payload['interval_hours']
        if type(hours) is not int or not MIN_HOURS<=hours<=MAX_HOURS:raise ValueError(f'检查间隔须为 {MIN_HOURS}–{MAX_HOURS} 小时')
        if hours!=row['interval_hours'] and row.get('last_checked_ts'):row['next_check_at']=row['last_checked_ts']+hours*3600
        row['interval_hours']=hours
    if 'name' in payload:
        if not isinstance(payload['name'],str) or len(payload['name'])>200:raise ValueError('订阅名称过长')
        row['name']=payload['name'].strip()
    service.store.put('evidence',PREFIX+sid,row)
    return {'subscription':_public(row),'network_used':False}


def delete(service,payload):
    sid=payload.get('subscription_id') if isinstance(payload,dict) else None
    if not isinstance(sid,str) or not service.store.get('evidence',PREFIX+sid):raise ValueError('订阅不存在或已删除')
    with service.store.connection() as c:c.execute('DELETE FROM evidence WHERE id=?',(PREFIX+sid,))
    return {'deleted':sid,'library_changes':0,'network_used':False}


def check_now(service,payload,request_id=None):
    sid=payload.get('subscription_id') if isinstance(payload,dict) else None
    row=service.store.get('evidence',PREFIX+sid) if isinstance(sid,str) else None
    if not row:raise ValueError('订阅不存在或已删除')
    return service.enqueue('check_subscription',{'subscription_id':sid,'manual':True},request_id=request_id)


def _in_flight(service):
    with service.store.connection() as c:
        return c.execute("SELECT 1 FROM tasks WHERE json_extract(payload,'$.command')='check_subscription' AND json_extract(payload,'$.state') IN ('queued','running','paused') LIMIT 1").fetchone() is not None


def schedule(service,now=None):
    """Queue at most one due check at a time; returns True when one was queued."""
    now=time.time() if now is None else now
    if now-getattr(service,'_last_subscription_scan',0)<30:return False
    service._last_subscription_scan=now
    if service.config.current()['values']['offline'] or _in_flight(service):return False
    due=[r for r in (service.store.get('evidence',PREFIX+sid) for sid in _ids(service)) if r and r.get('enabled') and r.get('next_check_at',0)<=now]
    if not due:return False
    row=min(due,key=lambda r:r.get('next_check_at',0))
    service.enqueue('check_subscription',{'subscription_id':row['subscription_id']})
    return True


def run(service,args,cfg):
    """Re-read one subscribed playlist and start an import for unseen tracks."""
    sid=args.get('subscription_id');row=service.store.get('evidence',PREFIX+sid) if isinstance(sid,str) else None
    if not row:return {'status':'OK','reason':'订阅已删除','new_count':0}
    if not row.get('enabled') and not args.get('manual'):return {'status':'OK','reason':'订阅已暂停','new_count':0}
    started=time.time()
    def finish(status,reason,new_count=0,batch=None,retry=False,**extra):
        current=service.store.get('evidence',PREFIX+sid)
        if not current:return {'status':'OK','reason':'订阅已删除','new_count':0}
        current.update(extra)
        current.update(last_checked_at=utcnow(),last_checked_ts=started,last_status=status,last_reason=reason,last_new_count=new_count,
                       next_check_at=started+(DEFER_SECONDS if retry else current['interval_hours']*3600))
        if batch:current['last_batch_id']=batch
        current['history']=([{'at':current['last_checked_at'],'status':status,'new_count':new_count,'batch_id':batch,'reason':reason}]+current.get('history',[]))[:MAX_HISTORY]
        service.store.put('evidence',PREFIX+sid,current)
        return {'status':status,'reason':reason,'new_count':new_count,'subscription_id':sid,**({'batch_id':batch} if batch else {})}
    if cfg['offline'] or not cfg['workflow'].get('setup_done'):return finish('BLOCKED','请先完成连接设置并选择音乐库')
    try:
        result=service._execute('refresh_playlist',{'playlist_input':row['playlist_input'],'platform':row['platform']},cfg)
    except PolicyBlocked as exc:return finish('BLOCKED',str(exc))
    except Exception as exc:return finish('FAILED','读取歌单失败：'+(str(exc) or type(exc).__name__),retry=True)
    targets=result.get('targets') or []
    names=[s.get('name') for s in result.get('snapshots',[]) if s.get('name')]
    extra={'song_count':len(targets)}
    if names and not row.get('name'):extra['name']=names[0]
    if not targets:return finish('FAILED',result.get('reason') or '歌单没有返回曲目',retry=True,**extra)
    known=set(row.get('known_target_ids',[]))
    ids=[t['target_id'] for t in targets]
    if row.get('baseline_pending'):
        # Only tracks returned by a complete read count as the baseline; a partial
        # read keeps the baseline open so missing tracks are not silently skipped.
        complete=result.get('complete') is not False
        return finish('OK','已记录现有 '+str(len(ids))+' 首曲目，之后只自动入库新增曲目' if complete else '歌单只返回了部分曲目，已记录这些曲目，下次检查继续核对',
                      known_target_ids=sorted(known|set(ids)),baseline_pending=not complete,**extra)
    new=[tid for tid in dict.fromkeys(ids) if tid not in known]
    if not new:return finish('OK','没有新增曲目',**extra)
    try:
        accepted=service.enqueue('start_workflow',{'scene':'playlist','target_ids':new,'prefer_24bit':cfg['workflow']['prefer_24bit'],
                                                   'prefer_full_album':bool(row.get('prefer_full_album'))},request_id='subscription:'+sid+':'+uuid.uuid4().hex)
    except PolicyBlocked as exc:
        # Leave the tracks unseen so the next check submits them again.
        return finish('DEFERRED',f'发现 {len(new)} 首新增曲目，但{exc}；稍后自动重试',len(new),retry=True,**extra)
    return finish('OK',f'发现 {len(new)} 首新增曲目，已开始入库',len(new),accepted.get('batch_id'),known_target_ids=sorted(known|set(new)),**extra)
