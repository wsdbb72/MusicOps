"""Durable album workflows. Download source and watched library are separate.

One bounded stage runs per queued advance. Every network mutation has an intent;
process interruption becomes review instead of replaying an ambiguous request.
"""
from __future__ import annotations
import base64,collections,copy,hashlib,json,re,shutil,uuid
from pathlib import Path
from musicops.state.store import utcnow
from musicops.config.schema import digest
from musicops.clients.http import HttpError,PolicyBlocked,SafeHttpTransport
from musicops.domain.identity import strict_text
from musicops.domain.editions import compatible as album_compatible
from musicops.library.index import index_library,AUDIO_EXTENSIONS,sha256_file
from musicops.library.paths import reject_reparse_chain
from musicops.library.transactions import TransactionEngine,durable_copy
from musicops.media.tags import read_tag_fields,apply_tag_changes,embedded_covers,apply_cover_changes
from musicops.media.resources import validate_lrc,validate_cover
from musicops.app.search_status import SITE_NAMES,search_site,search_album,edition_conflict,summarize,detail as search_detail

TERMINAL={'completed','failed','needs_review','cancelled','skipped','restored'}
SCENES={'playlist':'歌单入库','complete':'资料补全','upgrade':'音质升级','fill':'补全整张专辑'}

def artist_key(artists):return tuple(sorted(strict_text(a) for a in artists if a))
def identity(row):return (strict_text(row.get('raw_title',row.get('title',''))),artist_key(row.get('artists',[])))
def segment(text):
    text=re.sub(r'[<>:"/\\|?*\x00-\x1f]','_',str(text)).strip(' .')[:100]
    if not text or re.fullmatch(r'(CON|PRN|AUX|NUL|COM[1-9]|LPT[1-9])(?:\..*)?',text,re.I):text='_'+(text or '未命名')
    return text
def album_root(row,library):
    path=Path(row['path']).parent
    if re.match(r'^(?:CD|Disc|Disk)\s*\d+(?:\b|\s|[-_])',path.name,re.I):path=path.parent
    return path if path!=Path(library) else Path(row['path']).parent
def quality(media):
    codec=str(media.get('codec_name') or media.get('codec','')).lower()
    lossless=codec in ('flac','alac','wave','wav','aiff','aif','ape','dsf','dff') or codec.startswith(('pcm_','dsd_'))
    return {'lossless':lossless,'codec':codec,'bits':int(media.get('bits_per_sample') or media.get('bits_per_raw_sample') or 0),'rate':int(media.get('sample_rate') or 0),'bitrate':int(media.get('bitrate') or media.get('bit_rate') or 0),'channels':int(media.get('channels') or 0)}
def improves(old,new,prefer24=True):
    a,b=quality(old),quality(new)
    if a['channels'] and b['channels'] and a['channels']!=b['channels']:return False
    if a['codec'].startswith('ds') or b['codec'].startswith('ds'):return False
    if a['lossless']!=b['lossless']:return b['lossless']
    if a['lossless']:
        return b['bits']>=a['bits'] and b['rate']>=a['rate'] and (b['rate']>a['rate'] or prefer24 and b['bits']>a['bits'])
    return a['codec']==b['codec'] and b['bitrate']>a['bitrate']*1.05

def no_downgrade(old,new):
    a,b=quality(old),quality(new)
    if a['channels'] and b['channels'] and a['channels']!=b['channels']:return False
    if a['lossless'] and not b['lossless']:return False
    if b['lossless'] and not a['lossless']:return True
    if a['lossless']:return b['bits']>=a['bits'] and b['rate']>=a['rate']
    return a['codec']==b['codec'] and b['bitrate']>=a['bitrate']*.95

class Workflows:
    def __init__(self,service):self.s=service;self.store=service.store
    def rows(self,include_batches=False):
        where=" AND coalesce(json_extract(payload,'$.hidden'),0)=0" + ('' if include_batches else " AND coalesce(json_extract(payload,'$.job_kind'),'album')!='batch'")
        with self.store.connection() as c:rows=c.execute("SELECT payload FROM evidence WHERE id LIKE 'workflow:%'"+where+" ORDER BY updated_at DESC,id").fetchall()
        return [json.loads(r[0]) for r in rows]
    def put(self,job,**changes):
        with self.s.mutex:
            previous=self.store.get('evidence','workflow:'+job['job_id'])
            if previous and previous.get('hidden'):job['hidden']=True
            if previous and previous.get('cancel_requested'):
                job['cancel_requested']=True
                if changes.get('stage')!='cancelled':changes.update(stage='cancelled',message='已在安全阶段取消；已经发布的内容和做种任务保留')
            if previous and 'paused' in previous and 'paused' not in changes:
                job['paused']=previous['paused']
            job.update(changes,updated_at=utcnow());self.store.put('evidence','workflow:'+job['job_id'],job)
            if job.get('stage') in ('needs_review','failed','skipped') and job.get('job_kind') not in ('batch','activity'):
                config=self.s.config.current()['values']
                if not config['offline'] and config['ai'].get('enabled'):
                    from musicops.app.review_queue import register
                    register(self.s,job)
            return job
    def list(self,offset=0,state='all',selection=False,view='steps',query=''):
        if type(offset) is not int or offset<0:raise ValueError('任务分页参数无效')
        if not isinstance(query,str) or len(query)>200:raise ValueError('任务关键词最多 200 字符')
        if view=='albums':
            from musicops.app.task_overview import listing
            return listing(self,offset,state,selection,query)
        if view!='steps':raise ValueError('任务视图无效')
        from musicops.app.task_filters import predicate
        clause,parameters=predicate(state)
        if query.strip():
            escaped=query.strip().replace('\\','\\\\').replace('%','\\%').replace('_','\\_')
            clause+=" AND coalesce(json_extract(payload,'$.label'),'') LIKE ? ESCAPE '\\'"
            parameters=(*parameters,'%'+escaped+'%')
        with self.store.connection() as c:
            if selection:
                selected=c.execute("SELECT json_extract(payload,'$.job_id') FROM evidence WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.hidden'),0)=0 AND coalesce(json_extract(payload,'$.job_kind'),'album') NOT IN ('batch','activity')"+clause+" ORDER BY coalesce(json_extract(payload,'$.created_at'),updated_at),id",parameters).fetchall()
                return {'job_ids':[r[0] for r in selected],'total':len(selected)}
            filtered=c.execute("SELECT COUNT(*) FROM evidence WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.hidden'),0)=0"+clause,parameters).fetchone()[0]
            counts=c.execute("SELECT json_extract(payload,'$.stage'),coalesce(json_extract(payload,'$.job_kind'),'album'),COUNT(*),coalesce(json_extract(payload,'$.paused'),0) FROM evidence WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.hidden'),0)=0 GROUP BY 1,2,4").fetchall()
            token_prompts=[r[0] for r in c.execute("SELECT json_extract(payload,'$.job_id') FROM evidence WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.hidden'),0)=0 AND json_extract(payload,'$.stage')='needs_review' AND json_extract(payload,'$.review_kind')='token' ORDER BY updated_at,id LIMIT 100").fetchall()]
            rows=c.execute("SELECT json_remove(payload,'$.config','$.targets','$.old_files','$.prepared','$.choices','$.outcomes','$.publication_plan','$.catalog_choices','$.lookup_links','$.engine_paths') FROM evidence WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.hidden'),0)=0"+clause+" ORDER BY coalesce(json_extract(payload,'$.created_at'),updated_at) DESC,id LIMIT 200 OFFSET ?",(*parameters,offset)).fetchall()
        public=[self.public_row(json.loads(r[0])) for r in rows]
        return {'jobs':public,'token_prompts':token_prompts,'total':filtered,'all_total':sum(r[2] for r in counts),'offset':offset,'active':sum(r[2] for r in counts if r[0] not in TERMINAL and not r[3]),'paused_count':sum(r[2] for r in counts if r[0] not in TERMINAL and r[3]),'total_active':sum(r[2] for r in counts if r[0] not in TERMINAL),'finished':sum(r[2] for r in counts if r[0]=='completed' and r[1]!='batch'),'preparing_count':sum(r[2] for r in counts if r[1]=='batch' and r[0] not in TERMINAL)}
    def public_row(self,j):
        row={k:j.get(k) for k in ('job_id','job_kind','task_type','scene','stage','label','progress','message','detail','review_kind','batch_id','scanned_count','cached_count','current_directory','album_count','selected_target_count','target_count','matched_count','identification_job_id','search_job_id','processing_job_id','progress_phase','ai_fallback','ai_assistance','retry_job_id','retry_count','ai_review_state')}
        if j.get('ai_review_state') in ('pending','running'):row['ai_review_message']='AI 辅助等待后台分批核对' if j['ai_review_state']=='pending' else 'AI 正在核对身份、搜索或恢复方案'
        if j.get('message')=='处理未完成：KeyError' and j.get('resume_stage')=='fallback':row['message']='历史回退任务配置不完整；可重试以更新配置后继续'
        if j.get('ai_assistance'):row['ai_assistance']={k:j['ai_assistance'].get(k) for k in ('status','kind','message')}
        pending=bool(j.get('paused') and j['stage']=='running')
        if j.get('paused') and j.get('job_kind')=='batch' and j['stage']=='preparing':
            task=self.store.get('tasks',j.get('start_task_id'))
            pending=bool(task and task['state']=='running')
        row.update(task_group=j.get('task_group') or ('identify' if j.get('job_kind')=='batch' or j['stage'] in ('identifying','snapshotting','metadata','enrich','planning') else 'search' if j['stage'] in ('searching','requesting') else 'process'),paused=bool(j.get('paused')),pause_pending=pending)
        return row
    def next_job(self):
        import time
        with self.store.connection() as c:
            row=c.execute("SELECT payload FROM evidence AS current WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.hidden'),0)=0 AND coalesce(json_extract(payload,'$.job_kind'),'album') NOT IN ('batch','activity') AND coalesce(json_extract(payload,'$.paused'),0)=0 AND json_extract(payload,'$.stage') NOT IN ('completed','failed','needs_review','cancelled','skipped','restored','running') AND (json_extract(payload,'$.identification_job_id') IS NULL OR EXISTS (SELECT 1 FROM evidence AS parent WHERE parent.id='workflow:'||json_extract(current.payload,'$.identification_job_id') AND json_extract(parent.payload,'$.stage')='completed')) AND (json_extract(payload,'$.search_job_id') IS NULL OR coalesce(json_extract(payload,'$.task_type'),'')!='album' OR EXISTS (SELECT 1 FROM evidence AS searched WHERE searched.id='workflow:'||json_extract(current.payload,'$.search_job_id') AND json_extract(searched.payload,'$.stage')='completed')) AND coalesce(json_extract(payload,'$.next_poll'),0)<=? ORDER BY CASE json_extract(payload,'$.stage') WHEN 'notifying' THEN 0 WHEN 'validating' THEN 0 WHEN 'metadata' THEN 1 WHEN 'enrich' THEN 1 WHEN 'downloading' THEN 2 WHEN 'requesting' THEN 3 WHEN 'fallback' THEN 3 WHEN 'searching' THEN 4 ELSE 5 END,updated_at,id LIMIT 1",(time.time(),)).fetchone()
        return json.loads(row[0]) if row else None
    def queue(self,args,cfg,task_id=None):
        """Expose the preparation before the serial worker begins any filesystem IO."""
        scene=args.get('scene')
        if scene not in SCENES:raise ValueError('请选择歌单入库、资料补全或音质升级')
        if cfg['offline']:raise PolicyBlocked('请先在连接向导完成设置')
        # Prevent two live batches from modifying the same album or issuing duplicate FL.
        with self.store.connection() as c:
            active=c.execute("SELECT 1 FROM evidence WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.hidden'),0)=0 AND coalesce(json_extract(payload,'$.job_kind'),'album')!='activity' AND json_extract(payload,'$.stage') NOT IN ('completed','failed','needs_review','cancelled','skipped','restored') LIMIT 1").fetchone()
        if active:raise PolicyBlocked('已有任务正在准备或运行，请等待完成或取消后再开始下一批')
        root=Path(cfg['paths']['library']);reject_reparse_chain(root)
        if not root.is_dir():raise ValueError('音乐库目录不存在，请在设置中选择已有目录')
        if scene=='playlist':
            tids=args.get('target_ids')
            if not isinstance(tids,list) or not tids or len(tids)>50000 or any(not isinstance(t,str) or not t for t in tids) or len(set(tids))!=len(tids):raise ValueError('请选择有效且不重复的歌单曲目')
        batch_id=str(uuid.uuid4());job_id='batch:'+batch_id
        job={'kind':'workflow','job_kind':'batch','task_group':'identify','task_type':'preparation','job_id':job_id,'batch_id':batch_id,'scene':scene,'stage':'preparing','progress':0,'created_at':utcnow(),'message':'已加入队列，等待扫描本地音乐库','detail':'先扫描本地音频，再对比歌单；这里的文件数不是歌单歌曲数。','label':SCENES[scene]+' · 本地库检查','start_task_id':task_id,'scanned_count':0,'cached_count':0,'album_count':0,'selected_target_count':len(args.get('target_ids',[])),'progress_phase':'library_scan'}
        self.put(job);return job
    def start(self,args,cfg):
        batch=self.store.get('evidence','workflow:'+args['_batch_job_id']) if args.get('_batch_job_id') else self.queue(args,cfg)
        if not batch or batch.get('job_kind')!='batch':raise ValueError('准备任务不存在，请重新开始')
        try:return self._start(args,cfg,batch)
        except Exception as exc:
            self.start_failed(batch['job_id'],getattr(exc,'error_class',type(exc).__name__),str(exc) if isinstance(exc,(PolicyBlocked,ValueError)) else '')
            raise
    def start_failed(self,job_id,category,detail=''):
        batch=self.store.get('evidence','workflow:'+job_id)
        if not batch or batch['stage'] in TERMINAL:return
        cancelled=category=='CANCELLED' or batch.get('cancel_requested')
        self.put(batch,stage='cancelled' if cancelled else 'failed',message='准备任务已取消' if cancelled else '创建专辑任务失败：'+category,detail=detail or '可检查音乐目录和读取权限后重新开始。',error_class=category)
        self._cancel_children(batch['batch_id'],'准备未完成，后续专辑任务已停止，请重新开始本批次')
    def _start(self,args,cfg,batch):
        import time
        scene=batch['scene'];root=Path(cfg['paths']['library']);last=[0.0]
        def scan_progress(info):
            if time.monotonic()-last[0]<.5 and info['phase']!='scanned':return
            self.checkpoint(batch,'metadata_index')
            last[0]=time.monotonic()
            self.put(batch,scanned_count=info['scanned_count'],cached_count=info['cached_count'],current_directory=info['current_directory'],message=f"正在扫描本地音乐库：已检查 {info['scanned_count']} 个音频文件（缓存复用 {info['cached_count']} 个）",detail=(f"本批歌单选择 {batch['selected_target_count']} 首；" if scene=='playlist' else '')+'当前目录：'+(info['current_directory'] or '音乐库根目录'))
        self.checkpoint(batch,'before_metadata_index');self.put(batch,message='正在扫描本地音乐库：已检查 0 个音频文件')
        self.s._index_real_library({'root':str(root),'lightweight':True},cfg,progress=scan_progress)
        self.checkpoint(batch,'grouping_albums');self.put(batch,progress=85,progress_phase='matching',message='本地音乐库扫描完成，正在对比并按专辑整理任务')
        files=[f for f in self.store.list('library_files') if f.get('location_state')=='library' and Path(f['path']).suffix.lower() in AUDIO_EXTENSIONS and Path(f['path']).is_relative_to(root)]
        groups=collections.defaultdict(list);batch_id=batch['batch_id']
        if scene=='playlist':
            tids=args.get('target_ids')
            # Read all requested targets in one snapshot; thousands of SQLite
            # connections made a large playlist needlessly slow to prepare.
            targets={t['target_id']:t for t in self.store.list('target_tracks')}
            from musicops.library.catalog import AlbumCatalog
            catalog=AlbumCatalog(self.store)
            catalog.sync(root,files,lambda phase:self.checkpoint(batch,phase))
            selected=[targets[tid] for tid in tids if tid in targets]
            present=catalog.matched_targets(root,selected,lambda phase:self.checkpoint(batch,phase))
            for i,tid in enumerate(tids):
                if i%50==0:self.checkpoint(batch,'grouping_targets')
                t=targets.get(tid)
                if not t:raise ValueError('歌单曲目已改变，请重新解析')
                if tid in present:continue
                key=(t.get('source') or 'netease','album',str(t['source_album_id'])) if t.get('source_album_id') else (strict_text(t.get('album')),artist_key(t.get('artists',[])))
                if not t.get('album') or not t.get('artists'):key=('unresolved',t['target_id'])
                groups[key].append(t)
        else:
            for f in files:
                groups[(str(album_root(f,root)),strict_text(f.get('album')),artist_key(f.get('albumartist') or f.get('artists',[])))].append(f)
        local_groups=collections.defaultdict(list)
        if scene=='playlist':
            for f in files:local_groups[(strict_text(f.get('album')),artist_key(f.get('albumartist') or f.get('artists',[])))].append(f)
        created=0
        for _,tracks in groups.items():
            self.checkpoint(batch,'creating_album_tasks')
            if scene=='complete' and not args.get('refresh_resources'):
                recent=True
                for row in tracks:
                    cached=self.store.get('evidence','resources-checked:'+row['file_id'])
                    recent=recent and bool(cached and cached.get('version')==[row['size'],row['mtime_ns']] and cached.get('settings')==digest(cfg['lyrics']) and __import__('time').time()-cached.get('checked_at',0)<cfg['lyrics']['negative_ttl'])
                if recent:continue
            job_id=str(uuid.uuid4());first=tracks[0];stage='metadata' if scene=='complete' else 'snapshotting' if scene in ('upgrade','fill') else 'identifying'
            paused=bool((self.store.get('evidence','workflow:'+batch['job_id']) or {}).get('paused'))
            job={'kind':'workflow','job_id':job_id,'batch_id':batch_id,'scene':scene,'config':copy.deepcopy(cfg),'targets':tracks if scene=='playlist' else [],'old_files':tracks if scene!='playlist' else [],'stage':stage,'task_group':'identify','task_type':'album' if scene=='complete' else 'identification','target_count':len(tracks),'paused':paused,'progress':0,'created_at':utcnow(),'message':'等待识别' if scene=='playlist' else '等待处理','detail':'','label':(first.get('album') or '待识别专辑')+' · '+' / '.join(first.get('albumartist') or first.get('artists',[])),'prefer_24bit':args.get('prefer_24bit',cfg['workflow']['prefer_24bit']),'refresh_resources':bool(args.get('refresh_resources'))}
            if scene=='playlist':
                local=local_groups.get((strict_text(first.get('album')),artist_key(first.get('albumartist') or first.get('artists',[]))),[])
                if len({str(album_root(f,root)) for f in local})==1:job['existing_album_files']=local
            self.put(job)
            created+=1
            if created%25==0:self.put(batch,progress=90,album_count=created,message=f'正在创建专辑任务：已创建 {created} / {len(groups)} 项')
        message=f'已创建 {created} 个专辑任务；已经存在的歌单曲目和近期检查过的专辑已跳过'
        self.put(batch,stage='completed',progress=100,album_count=created,message=message,detail='准备完成，专辑任务会在后台依次处理。' if created else '本批次没有需要处理的专辑。可检查曲目是否已在库中，或启用重新检查。')
        return {'status':'OK','batch_id':batch_id,'album_count':created,'message':message}
    def recover(self):
        # Version 0.3.1 queued starts were invisible while a first scan was
        # running. Do not replay several historical clicks after an upgrade.
        for task in self.store.list('tasks'):
            if task.get('command')!='start_workflow' or task.get('args',{}).get('_batch_job_id') or task['state'] not in ('queued','running','paused','recovery_required'):continue
            job_id='batch:'+task['task_id'];scene=task.get('args',{}).get('scene','complete')
            self.put({'kind':'workflow','job_kind':'batch','job_id':job_id,'batch_id':task['task_id'],'scene':scene,'stage':'needs_review','progress':0,'label':SCENES.get(scene,scene)+' · 上次准备任务','created_at':task.get('created_at',utcnow()),'message':'旧版准备任务已停止，请重新开始本批次','detail':'保留了原来的歌单选择和设置；旧版重复排队请求不会自动执行下载。','review_kind':'interrupted','start_task_id':task['task_id']})
            task.update(state='recovery_required',error='旧版准备任务已停止，请重新开始本批次',error_class='RECOVERY_REQUIRED');self.store.put('tasks',task['task_id'],task)
        jobs=self.rows(include_batches=True)
        batches={job['batch_id'] for job in jobs if job.get('job_kind')=='batch' and isinstance(job.get('batch_id'),str) and job['batch_id'] and job.get('job_id')=='batch:'+job['batch_id']}
        legacy=[]
        for job in jobs:
            if job.get('job_kind') not in ('batch','activity') and job.get('stage') not in TERMINAL and job.get('batch_id') not in batches:
                # A 0.3.1 scan could finish after its window appeared stuck,
                # leaving hundreds of invisible album requests. They have no
                # durable preparation batch and must never restart downloads
                # merely because the user installs the new application.
                original_stage=job.get('stage')
                job.update(legacy_stage=original_stage,resume_stage=job.get('resume_stage') or original_stage,
                           legacy_batch_requires_review=True,stage='needs_review',review_kind='interrupted',
                           message='旧版专辑队列已暂停；请核对后手动继续，或重新开始本批次',
                           detail='升级不会自动重放旧版下载请求。已有下载、做种任务、令牌记录和文件事务保持原状。',updated_at=utcnow())
                legacy.append(job)
        if legacy:
            # One commit, rather than a full synchronous filesystem flush for
            # every legacy album, keeps first startup responsive on large queues.
            with self.store.connection() as connection:
                connection.execute('BEGIN IMMEDIATE')
                connection.executemany('UPDATE evidence SET payload=?,updated_at=? WHERE id=?',
                    [(json.dumps(job,ensure_ascii=False),job['updated_at'],'workflow:'+job['job_id']) for job in legacy])
        for job in jobs:
            if job.get('job_kind')=='batch' and job.get('stage')=='preparing':
                task=self.store.get('tasks',job.get('start_task_id')) if job.get('start_task_id') else None
                if not task or task['state'] not in ('queued','paused'):
                    self.put(job,stage='needs_review',review_kind='interrupted',message='上次音乐库检查被中断，请重新开始本批次',detail='原音频保留；未完成的准备不会自动重复执行。')
                    self._cancel_children(job['batch_id'],'准备被中断，未开始的专辑任务已取消，请重新开始本批次')
            if job.get('stage')=='running':self.put(job,stage='needs_review',review_kind='interrupted',message='上次处理被中断；原版未自动删除，请核对下载或事务后继续')
    def _cancel_children(self,batch_id,message):
        with self.store.connection() as c:
            rows=c.execute("SELECT payload FROM evidence WHERE id LIKE 'workflow:%' AND coalesce(json_extract(payload,'$.job_kind'),'album')!='batch' AND json_extract(payload,'$.batch_id')=?",(batch_id,)).fetchall()
        for raw in rows:
            child=json.loads(raw[0])
            if child['stage'] not in TERMINAL:self.put(child,cancel_requested=True,stage='cancelled',message=message)
    def cancel(self,job_id):
        job=self.store.get('evidence','workflow:'+job_id)
        if not job:raise ValueError('任务不存在')
        if job['stage'] in ('completed','failed','cancelled','skipped','restored'):return {'status':'OK','job':self.list()}
        if job.get('job_kind') in ('batch','activity'):
            task=self.store.get('tasks',job.get('start_task_id')) if job.get('start_task_id') else None
            if task and task['state'] in ('queued','running','paused'):
                task['cancel_requested']=True
                if task['state'] in ('queued','paused'):task['state']='cancelled'
                self.store.put('tasks',task['task_id'],task)
            if job.get('job_kind')=='batch':self._cancel_children(job['batch_id'],'准备任务已取消，后续专辑处理已停止')
        self.put(job,cancel_requested=True,stage='cancelled',message='已取消后续处理；已发布文件和做种任务保留');return {'status':'OK'}
    def bulk(self,action,job_ids):
        if action in ('retry','delete'):
            from musicops.app.workflow_actions import bulk
            return bulk(self,action,job_ids)
        if action not in ('pause','resume','cancel'):raise ValueError('请选择暂停、继续或取消')
        if not isinstance(job_ids,list) or not 1<=len(job_ids)<=500 or any(not isinstance(j,str) or not j or len(j)>160 for j in job_ids):raise ValueError('一次请选择 1 至 500 个有效任务')
        results=[]
        for job_id in dict.fromkeys(job_ids):
            job=self.store.get('evidence','workflow:'+job_id)
            if not job:results.append({'job_id':job_id,'status':'NOT_FOUND','message':'任务不存在'});continue
            if action=='resume' and job.get('stage') in ('failed','needs_review') and job.get('job_kind') not in ('activity','batch') and job.get('review_kind') in ('error','interrupted','search','token') and (job.get('resume_stage') or job.get('task_type')=='search'):
                from musicops.app.workflow_actions import retry
                try:results.append({'job_id':job_id,'status':'OK',**retry(self,job,allow_legacy=True)})
                except (ValueError,PolicyBlocked) as exc:results.append({'job_id':job_id,'status':'SKIPPED','message':str(exc)})
                continue
            children=[]
            if job.get('job_kind')=='batch':
                with self.store.connection() as c:
                    rows=c.execute("SELECT payload FROM evidence WHERE id LIKE 'workflow:%' AND json_extract(payload,'$.batch_id')=? AND coalesce(json_extract(payload,'$.job_kind'),'album')!='batch'",(job['batch_id'],)).fetchall()
                children=[json.loads(r[0]) for r in rows]
            stopped=TERMINAL-{'needs_review'} if action=='cancel' else TERMINAL
            affected=[row for row in [job,*children] if row['stage'] not in stopped]
            # A completed preparation can still control its active album tasks.
            if not affected:
                guidance={'cover':'需要选择或跳过封面，请打开核对窗口','metadata':'需要补充专辑与曲目信息，请打开核对窗口','candidate':'需要选择发行版本，请打开核对窗口','resources':'需要确认歌词服务或跳过，请打开核对窗口','numbering':'需要确认曲序，请打开核对窗口'}
                results.append({'job_id':job_id,'status':'SKIPPED','message':guidance.get(job.get('review_kind'),'任务已结束或需要核对，未重新执行')});continue
            if action=='cancel':
                for row in affected:self.cancel(row['job_id'])
            else:
                for row in affected:self.put(row,paused=action=='pause')
                if job.get('job_kind')=='batch':self.put(job,paused=action=='pause')
                if job.get('job_kind') in ('batch','activity'):
                    task=self.store.get('tasks',job.get('start_task_id'))
                    if task and task['state'] in ('queued','paused'):
                        task['state']='paused' if action=='pause' else 'queued';self.store.put('tasks',task['task_id'],task)
            results.append({'job_id':job_id,'status':'OK','affected_count':len(affected),'message':'当前阶段完成后暂停' if action=='pause' else '已继续等待执行' if action=='resume' else '已取消后续处理'})
        counts=collections.Counter(r['status'] for r in results)
        self.store.event('workflow_bulk_'+action,{'selected_count':len(results),'succeeded':counts['OK'],'skipped':counts['SKIPPED'],'failed':counts['NOT_FOUND']})
        message=f"已处理 {counts['OK']} 项，跳过 {counts['SKIPPED']} 项，未找到 {counts['NOT_FOUND']} 项"
        if action=='resume' and self.s.paused:message+='；整个后台仍已暂停，请点击“继续整个后台”'
        return {'status':'OK','results':results,'succeeded':counts['OK'],'skipped':counts['SKIPPED'],'failed':counts['NOT_FOUND'],'message':message}
    def checkpoint(self,job,phase):
        self.s._checkpoint(phase);current=self.store.get('evidence','workflow:'+job['job_id'])
        if current and current.get('cancel_requested'):
            from musicops.app.service import TaskCancelled
            raise TaskCancelled('已取消专辑任务')
    def advance(self):
        with self.s.mutex:
            job=self.next_job()
            if not job:return {'status':'OK','idle':True}
            stage=job['stage'];self.put(job,stage='running',resume_stage=stage)
        try:
            # Snapshot defaults evolve independently of the current profile.
            from musicops.config.schema import upgrade_config
            if not isinstance(job.get('config'),dict):raise PolicyBlocked('历史任务缺少配置快照，请重新创建本批次')
            job['config']=upgrade_config(job['config'],self.s.base)
            self.checkpoint(job,stage)
            getattr(self,'_'+stage)(job)
        except Exception as exc:
            current=self.store.get('evidence','workflow:'+job['job_id'])
            # A failed stage may have partially mutated its in-memory dict
            # (including a cyclic provider payload). Recover the durable copy.
            job=current or job
            if job.get('cancel_requested'):self.put(job,stage='cancelled',message='已取消；已有下载和原音源保留')
            else:
                category=getattr(exc,'error_class',type(exc).__name__)
                if category=='TOKEN_APPROVAL_REQUIRED':exc=PolicyBlocked(SITE_NAMES.get((job.get('selected') or {}).get('source'),'资源站')+'：'+str(exc))
                stage='needs_review' if isinstance(exc,(PolicyBlocked,HttpError)) or category in ('RECOVERY_REQUIRED','CONFLICT') else 'failed'
                # Never store exception URLs, credentials, API payloads or remote HTML.
                import traceback
                frames=[{'file':Path(f.filename).name,'line':f.lineno,'function':f.name} for f in traceback.extract_tb(exc.__traceback__)[-8:]]
                self.store.event('workflow_error',{'job_id':job['job_id'],'error_class':category,'stage':job.get('resume_stage'),'frames':frames})
                message='等待令牌下载确认' if category=='TOKEN_APPROVAL_REQUIRED' else str(exc) if isinstance(exc,(PolicyBlocked,HttpError)) else {'KeyError':'历史任务资料不完整，请核对后继续','AttributeError':'旧任务检查点异常，可选择继续恢复','ValueError':'任务资料格式异常，请核对后重试'}.get(category,'处理未完成：'+category)
                self.put(job,stage=stage,error_class=category,review_kind='token' if category=='TOKEN_APPROVAL_REQUIRED' else 'error',message=message,detail=str(exc) if isinstance(exc,(PolicyBlocked,HttpError)) else '原音源保留；请检查本机工具、文件或磁盘空间。')
        return {'status':'OK','job_id':job['job_id']}
    def provider(self,site,cfg):
        from musicops.providers.pt_download import PTDownloadProvider
        from musicops.providers.pt_readonly import HOSTS
        return PTDownloadProvider(site,cfg['providers'][site],self.s._pt_transport(site,cfg),self.s.vault.get)
    def _snapshotting(self,job):
        # Hash only the current album after its task is visible, instead of
        # reading the entire library before any task can appear.
        tracks=job['old_files'];self.put(job,message=f'正在核对本专辑原音源：0 / {len(tracks)} 首')
        for i,row in enumerate(tracks):
            self.checkpoint(job,'upgrade_original_hash');path=Path(row['path']);reject_reparse_chain(path)
            before=path.stat()
            if (before.st_size,before.st_mtime_ns)!=(row['size'],row['mtime_ns']):raise PolicyBlocked('原文件在扫描后改变，请重新检查音乐库')
            row['original_hash']=sha256_file(path)
            after=path.stat()
            if (after.st_size,after.st_mtime_ns)!=(before.st_size,before.st_mtime_ns):raise PolicyBlocked('原文件在核对时改变，请重新检查音乐库')
            row['companion_hashes']={c['path']:sha256_file(c['path']) for c in row.get('companions',[]) if c['type']=='lyrics'}
            self.put(job,message=f'正在核对本专辑原音源：{i+1} / {len(tracks)} 首')
        self.checkpoint(job,'upgrade_original_hash_finished')
        if job['scene']=='fill':return self._identifying(job)
        if job.get('task_type')=='identification':self._queue_search(job,'原音源已核对，已创建独立搜索任务')
        else:self.put(job,stage='searching',message='原音源已核对，等待搜索更高音质发行')
    def _queue_search(self,job,message='识别完成，已创建独立搜索任务'):
        # Deterministic IDs prevent duplicate searches after a checkpoint/review.
        self.checkpoint(job,'queue_independent_search')
        with self.s.mutex:
            current=self.store.get('evidence','workflow:'+job['job_id']) or job
            if current.get('cancel_requested'):return
            job['paused']=bool(current.get('paused'))
            search_id=str(uuid.uuid5(uuid.NAMESPACE_URL,'musicops:search:'+job['job_id']))
            previous=self.store.get('evidence','workflow:'+search_id)
            if not previous:
                child={k:copy.deepcopy(v) for k,v in job.items() if k not in ('cancel_requested','resume_stage','created_at','updated_at','review_kind','search_job_id')}
                child.update(job_id=search_id,task_type='search',task_group='search',identification_job_id=job['job_id'],stage='searching',progress=0,message='身份已核对，等待四站检索',created_at=utcnow())
                self.put(child)
            self.put(job,stage='completed',progress=100,search_job_id=search_id,message=message)
    def _identifying(self,job):
        if job['scene']=='fill':
            from musicops.app.album_completion import manifest
            return manifest(self,job)
        tracks=job['targets'] or job['old_files'];first=tracks[0]
        incomplete=[r for r in tracks if not all((r.get('raw_title'),r.get('artists'),r.get('album'),r.get('duration')))]
        if incomplete:
            resolver=self.s._catalog_resolver(job['config']);row=incomplete[0]
            query={'title':row.get('raw_title',''),'artists':row.get('artists',[]),'album':row.get('album',''),'duration':row.get('duration'),'filename_hint':Path(row.get('path') or '').name}
            found=resolver.identify(query);verified=[];examined=set()
            def resolve(candidates):
                for candidate in candidates[:5]:
                    self.checkpoint(job,'identification_catalog')
                    if candidate.get('entity_kind')=='recording' and not candidate.get('release_id'):continue
                    source_id=('release:'+candidate['release_id']) if candidate.get('entity_kind')=='recording' and candidate.get('release_id') else candidate['source_id']
                    if (candidate['source'],source_id) in examined:continue
                    examined.add((candidate['source'],source_id));resolved=resolver.resolve_candidate(candidate['source'],source_id).get('candidate')
                    if not resolved or not resolved.get('tracklist_complete'):continue
                    mapping=[]
                    for target in tracks:
                        same=[t for t in resolved.get('tracklist',[]) if identity(t)==identity(target) and target.get('duration') and t.get('duration') and abs(target['duration']-t['duration'])<=2]
                        if len(same)!=1 or target.get('album') and strict_text(target['album'])!=strict_text(resolved.get('album') or resolved.get('title')):break
                        mapping.append(same[0])
                    if len(mapping)==len(tracks):verified.append((resolved,mapping))
            resolve(found.get('candidates',[]))
            if len(verified)!=1:
                found=self.s._recognize_metadata(query,job['config'],found=found,force=True)
                selected=found.get('selected_candidate');resolve(([selected] if selected else [])+found.get('candidates',[]))
            job['catalog_choices']=found.get('candidates',[]);job['lookup_links']=found.get('lookup_links',[]);job['ai_fallback']=found.get('ai_fallback',{})
            verified=list({(c['source'],c['source_id']):(c,m) for c,m in verified}.values())
            if len(verified)!=1:
                self.put(job,stage='needs_review',review_kind='metadata',message='已尝试目录与 AI 兜底，身份仍需核对' if job.get('ai_fallback',{}).get('status') not in (None,'NOT_CONFIGURED','DISABLED') else '专辑、艺人或完整时长缺失，请先核对资料',detail='识别与搜索已分开；确认身份后才会创建搜索任务。');return
            candidate,mapping=verified[0]
            for target,recording in zip(tracks,mapping):
                target.update(album=candidate.get('album') or candidate['title'],metadata_complete=True)
            job['recognition_evidence']={'source':candidate['source'],'source_id':candidate['source_id'],'tracklist_verified':True}
        self.put(job,matched_count=len(tracks),target_count=len(tracks))
        self._queue_search(job)
    def _queue_processing(self,job,stage,message,**changes):
        if job.get('task_type')!='search':return self.put(job,stage=stage,message=message,**changes)
        self.checkpoint(job,'queue_independent_processing')
        with self.s.mutex:
            current=self.store.get('evidence','workflow:'+job['job_id']) or job
            if current.get('cancel_requested'):return
            job['paused']=bool(current.get('paused'))
            child_id=str(uuid.uuid5(uuid.NAMESPACE_URL,'musicops:process:'+job['job_id']))
            if not self.store.get('evidence','workflow:'+child_id):
                child={k:copy.deepcopy(v) for k,v in job.items() if k not in ('cancel_requested','resume_stage','created_at','updated_at','review_kind','processing_job_id')}
                child.update(changes,job_id=child_id,task_type='album',task_group='process',search_job_id=job['job_id'],stage=stage,message=message,created_at=utcnow())
                self.put(child)
            self.put(job,stage='completed',progress=100,processing_job_id=child_id,message='检索完成，已创建独立下载与入库任务')
    def _searching(self,job):
        cfg=job['config'];tracks=job['targets'] or job['old_files'];first=tracks[0];album=first.get('album');artists=first.get('albumartist') or first.get('artists',[])
        if not album or not artists or job['scene']=='playlist' and first.get('source') and not first.get('duration'):
            from musicops.providers.catalog import lookup_links
            self.put(job,lookup_links=lookup_links({'title':first.get('raw_title',''),'artists':artists,'album':album}),stage='needs_review',review_kind='metadata',message='专辑、艺人或完整时长缺失，需要先核对资料');return
        outcomes={};candidates=[]
        for site in cfg['workflow']['source_order']:
            settings=cfg['providers'][site]
            if not settings['enabled'] or job.get('search_sites') is not None and site not in job['search_sites']:outcomes[site]={'status':'DISABLED'};continue
            self.checkpoint(job,'search_'+site);result=search_album(lambda:self.provider(site,cfg),job.get('search_query') or ' '.join(artists)+' '+album,album='' if job.get('search_query') else album,checkpoint=lambda:self.checkpoint(job,'search_'+site))
            outcomes[site]={k:v for k,v in result.items() if k not in ('candidates','reason')}
            for c in result.get('candidates',[]):
                exact=strict_text(c['title'])==strict_text(album) and artist_key(c['artists'])==artist_key(artists)
                if job['scene']=='playlist' and artists and strict_text(c['title'])==strict_text(album) and set(artist_key(artists)).issubset(set(artist_key(c['artists']))):exact=True
                if site=='open.cd' and len(artists)==1:
                    text=strict_text(c['title']);prefix=strict_text(artists[0])+' - '+strict_text(album)
                    tail=text[len(prefix):] if text.startswith(prefix) else None
                    if tail is not None and (not tail.strip() or re.match(r'^\s*(?:\(?\d{4}\)?|\[?(?:flac|alac|mp3|wav|24\s*bit|16\s*bit)\b)',tail)):
                        exact=True;c['artists']=artists;c['release']['album']=album
                from musicops.domain.editions import compatible
                if compatible(c['title'],album) and artist_key(c['artists'])==artist_key(artists):exact=True
                if edition_conflict(c['title'],' '.join(artists)+' '+album) and not compatible(c['title'],album):exact=False
                candidates.append({**c,'identity_exact':exact})
        candidates=list({(c['source'],c['candidate_id']):c for c in candidates}.values())
        summary=summarize(outcomes);job['outcomes']=outcomes;job['choices']=candidates;job['search_summary']=summary;job['detail']=search_detail(summary)
        if not summary['successful_sites'] and not summary['unavailable_sites']:
            if job['scene'] in ('playlist','fill') and cfg['workflow']['netease_fallback']:
                self._queue_processing(job,'fallback','未启用资源站，按设置使用网易云下载',progress=20)
            else:self.put(job,stage='needs_review',review_kind='search',message='尚未启用可检索站点',detail='请在连接与设置中启用资源站，或为歌单开启网易云回退。')
            return
        exact=[c for c in candidates if c.get('identity_exact') and (c['source']=='open.cd' or (c.get('seeders') or 0)>0)]
        if job['scene']=='upgrade':
            useful=[]
            for candidate in exact:
                media=candidate.get('media',{});fmt=str(media.get('format','')).lower();encoding=str(media.get('encoding','')).lower();text=encoding+' '+str(candidate['title']).lower()
                lossless=fmt in ('flac','alac','wav','aiff','ape');bits=24 if re.search(r'24\s*[- ]?bit',text) else 16 if lossless else 0
                rate=re.search(r'([0-9]+(?:\.[0-9]+)?)\s*khz',text);sample_rate=int(float(rate.group(1))*1000) if rate else 0
                bitrate=re.search(r'\b(128|192|256|320)\b',encoding);br=int(bitrate.group(1))*1000 if bitrate else 0
                for row in job['old_files']:
                    old=quality(row['media_info'])
                    if lossless and not old['lossless'] or lossless and old['lossless'] and (job['prefer_24bit'] and bits>old['bits'] or sample_rate>old['rate']) or not lossless and not old['lossless'] and fmt==old['codec'] and br>old['bitrate']*1.05:
                        useful.append(candidate);break
            if not useful:
                if exact:self.put(job,stage='skipped',message='检索到的发行未显示明确音质提升，当前音源保留');return
            exact=useful
        from musicops.domain.editions import rank as edition_rank
        def rank(c):return edition_rank(c,cfg['workflow'].get('edition_policy','quality'),cfg['workflow']['source_order'])
        if cfg['workflow'].get('edition_policy')=='complete':
            from musicops.domain.editions import audio_count
            for candidate in sorted(exact,key=rank,reverse=True)[:8]:
                if candidate['source']=='open.cd':continue
                self.checkpoint(job,'edition_manifest')
                try:
                    count=audio_count(self.provider(candidate['source'],cfg).detail(candidate).get('torrent',{}))
                    if count:candidate['track_count']=count
                except (HttpError,PolicyBlocked,OSError,TimeoutError):candidate['track_count_unknown']=True
        if exact:
            best=sorted(exact,key=rank,reverse=True)[0];self._queue_processing(job,'requesting','找到匹配专辑，准备下载',selected=best,progress=20);return
        if not job.get('ai_search_attempted') and cfg['ai'].get('enabled'):
            job['ai_search_attempted']=True
            found=self.s._recognize_metadata({'title':first.get('raw_title',''),'artists':artists,'album':album,'duration':first.get('duration')},cfg,force=True)
            job['ai_fallback']=found.get('ai_fallback',{});job['lookup_links']=found.get('lookup_links',[])
            candidate=found.get('selected_candidate')
            if candidate:
                canonical=candidate.get('album') or (candidate.get('title') if candidate.get('entity_kind') in ('release','master') else None)
                credits=candidate.get('artists',[]);query=' '.join(credits)+' '+str(canonical or '')
                if canonical and credits and len(query)<=300 and strict_text(query)!=strict_text(' '.join(artists)+' '+album):
                    self.put(job,stage='searching',search_query=query,message='AI 找到真实目录中的别名，使用规范关键词重新检索',detail='别名仅用于检索；不一致的艺人或发行仍需核对。');return
        if candidates:self.put(job,stage='needs_review',review_kind='candidate',message='找到了候选发行，需要核对艺人和版本');return
        if not summary['successful_sites']:
            self.put(job,stage='needs_review',review_kind='search',message='已启用站点当前均不可用' if summary['unavailable_sites'] else '尚未启用可检索站点',detail=search_detail(summary) or '请在连接与设置中启用资源站。');return
        if summary['unavailable_sites'] or summary['limited_sites']:
            self.put(job,stage='needs_review',review_kind='search',message='部分站点失败或结果尚不完整，暂停自动流媒体回退',detail=search_detail(summary)+'；可重试失败站点，或在核对页明确选择回退流媒体。');return
        if job['scene'] in ('playlist','fill') and cfg['workflow']['netease_fallback']:
            self._queue_processing(job,'fallback','可用站点未找到匹配发行，回退网易云下载',progress=20,detail=search_detail(summary));return
        self.put(job,stage='skipped',message='可用站点未找到匹配发行，当前音源保留',detail=search_detail(summary))
    def _requesting(self,job):
        cfg=job['config'];candidate=job.get('selected') or {}
        if not isinstance(candidate,dict) or candidate.get('source') not in cfg['providers'] or any(not isinstance(candidate.get(key),(str,int)) or isinstance(candidate.get(key),bool) or not str(candidate[key]).strip() for key in ('candidate_id','resource_id')):
            raise PolicyBlocked('所选资源缺少有效的站点或种子编号，请重新搜索并选择专辑资源')
        cache=Path(cfg['paths']['source'])/'torrents'/(hashlib.sha256(str(candidate['candidate_id']).encode()).hexdigest()+'.torrent')
        with self.store.connection() as connection:reservations=[json.loads(r[0]) for r in connection.execute('SELECT payload FROM token_reservations').fetchall()]
        used=sum(r['cost'] for r in reservations if r.get('budget_ref')==job['batch_id'] and r['site']!=candidate['source'] and r['state'] not in ('RELEASED','REJECTED'))
        budget=None if cfg['workflow'].get('fl_unlimited',False) else max(0,cfg['workflow']['fl_per_batch']-used)
        from musicops.app.download_policy import consent_for
        consent_id=consent_for(self.store,job) if candidate['source'] in ('dicmusic','orpheus') else None
        arguments={'consent_id':consent_id} if consent_id else {}
        from musicops.clients.qbt_live import live_client
        # Verify that the receiving client is reachable before spending FL.
        qbt=live_client(cfg['qbt'],cfg['paths']['source'],self.store,self.s.vault.get)
        try:
            content,evidence=self.provider(candidate['source'],cfg).fetch(candidate,self.store,job['batch_id'],budget,cache,**arguments)
            self.checkpoint(job,'before_qbt_add')
            qbt.transport.prepare(cfg['qbt'],cfg['qbt']['tag_prefix']+job['job_id']);result=qbt.add_stopped(content,job['job_id'],evidence)
            if result['status'] in ('START_REQUESTED','DOWNLOADING','COMPLETE'):
                self.put(job,stage='downloading',progress=30,message='已核对已有下载，继续等待完成');return
            if result['status']!='STOPPED':raise PolicyBlocked(result.get('reason','新增下载尚未确认，请在 qBittorrent 核对'))
            result=qbt.start_download(job['job_id'],evidence)
            if result['status']!='START_REQUESTED':raise PolicyBlocked(result.get('reason','下载启动状态未知，请核对后继续'))
        finally:qbt.transport.close()
        method=evidence.get('download_method');cost=evidence.get('token_cost',0)
        free_detail=('用户已授权普通下载：本次不使用 FL，可能计入下载量' if method=='explicit-nonfree' else
                     'OpenCD 按直接下载方式提交' if method=='direct' else
                     'FL 令牌请求已核验，记录用量 '+str(cost)+' 枚' if method=='fl-token' else
                     '已核验个人免费；本次未重复使用令牌' if method=='personal-free' else '已核验站点免费状态')
        if evidence.get('reconciled_previous_request'):free_detail+='；已恢复此前待对账请求'
        self.put(job,stage='downloading',progress=30,message='qBittorrent 正在下载，下载完成后自动整理',detail=free_detail)
    def _downloading(self,job):
        from musicops.clients.qbt_live import live_client
        cfg=job['config'];qbt=live_client(cfg['qbt'],cfg['paths']['source'],self.store,self.s.vault.get)
        try:result=qbt.poll(job['job_id'])
        finally:qbt.transport.close()
        if result.get('complete'):
            self.put(job,source_path=result['source_path'],stage='metadata',progress=55,message='下载完整，开始整理资料')
        elif result['status'] in ('DOWNLOADING','WAITING','PARTIAL_DOWNLOAD'):
            self.put(job,stage='downloading',next_poll=__import__('time').time()+cfg['qbt']['poll_seconds'],message='等待整张发行下载完成')
        else:raise PolicyBlocked('下载归属、文件清单或完成状态未通过，请在 qBittorrent 检查')
    def _fallback(self,job):
        from musicops.app.streaming_fallback import run
        return run(self,job)
    def work_paths(self,job):
        cfg=job['config'];library=Path(cfg['paths']['library']).resolve()
        if library==library.parent:raise PolicyBlocked('请选择音乐库子目录，不使用整个磁盘根目录')
        work=library.parent/('.MusicOps-work-'+hashlib.sha256(str(library).casefold().encode()).hexdigest()[:10])
        for watched in cfg['music_servers']['roon'].get('watched_folders',[]):
            if work.is_relative_to(Path(watched).resolve()):raise PolicyBlocked('整理工作区也被 Roon 监听，请将监听目录限定为音乐库')
        reject_reparse_chain(work);work.mkdir(parents=True,exist_ok=True)
        return {**cfg['paths'],'work':str(work)}
    def _catalog_fill(self,job,rows):
        """Missing tags may only use a unique complete release matching every track."""
        first=rows[0];query={'title':first.get('raw_title',''),'artists':first.get('artists',[]),'album':first.get('album',''),'duration':first.get('duration'),'filename_hint':Path(first.get('original_path') or first.get('path') or '').name}
        resolver=self.s._catalog_resolver(job['config']);found=resolver.identify(query);matches=[]
        examined=set()
        def resolve(candidates):
            for c in candidates[:5]:
                self.checkpoint(job,'catalog_identification')
                if c.get('entity_kind')=='recording' and not c.get('release_id'):continue
                source_id=('release:'+c['release_id']) if c.get('release_id') and c.get('entity_kind')=='recording' else c['source_id']
                if (c['source'],source_id) in examined:continue
                examined.add((c['source'],source_id));resolved=resolver.resolve_candidate(c['source'],source_id).get('candidate')
                if not resolved or not resolved.get('tracklist_complete'):continue
                tracks=resolved.get('tracklist',[])
                if len(tracks)!=len(rows):continue
                if collections.Counter(identity(t) for t in tracks)!=collections.Counter(identity(r) for r in rows):continue
                # Titles alone do not prove a live/remaster recording.
                if any(r.get('duration') and t.get('duration') and abs(r['duration']-t['duration'])>2 for r in rows for t in tracks if identity(r)==identity(t)):continue
                if first.get('album') and strict_text(first['album'])!=strict_text(resolved.get('album') or resolved.get('title')):continue
                matches.append(resolved)
        resolve(found.get('candidates',[]))
        if len(matches)!=1:
            found=self.s._recognize_metadata(query,job['config'],found=found,force=True)
            job['ai_fallback']=found.get('ai_fallback',{})
            selected=found.get('selected_candidate');resolve(([selected] if selected else [])+found.get('candidates',[]))
        matches=list({(r['source'],r['source_id']):r for r in matches}.values())
        job['catalog_choices']=matches;job['lookup_links']=found.get('lookup_links',[])
        # Different pressings with identical tracks are still different releases.
        return matches[0] if len(matches)==1 else None
    def _metadata(self,job):
        cfg=job['config'];paths=self.work_paths(job);work=Path(paths['work'])/'prepared'/job['job_id'];reject_reparse_chain(work)
        if job['scene']=='complete':
            complete=True;disc_totals=[];disc_numbers=[]
            for row in job['old_files']:
                tags=read_tag_fields(row['path']);number=(tags.get('discnumber') or [''])[0]
                if not all(tags.get(k) for k in ('title','artist','album','albumartist','tracknumber','discnumber')) or '/' not in number:complete=False
                if '/' in number:
                    n,total=number.split('/',1)
                    if not n.isdigit() or not total.isdigit():complete=False
                    else:disc_numbers.append(int(n));disc_totals.append(int(total))
                import mutagen
                audio=mutagen.File(row['path'])
                if audio and number and '/' in number:
                    for alias in ('totaldiscs','disctotal'):
                        value=(audio.tags or {}).get(alias)
                        if value and list(value)!=[number.split('/',1)[1]]:complete=False
                if not embedded_covers(row['path']) or not any(c['type']=='lyrics' and validate_lrc(Path(c['path']).read_text('utf-8-sig'),row.get('duration'))['valid'] for c in row.get('companions',[])):complete=False
            if complete and disc_numbers and set(disc_numbers)==set(range(1,max(disc_numbers)+1)) and set(disc_totals)=={max(disc_numbers)}:
                self.put(job,stage='skipped',message='专辑、艺人、封面和同步歌词已齐全');return
        if work.exists():raise PolicyBlocked('已存在未发布的整理副本，请核对后继续')
        work.mkdir(parents=True);job['prepared_root']=str(work);job['engine_paths']=paths
        sources=job['old_files'] if job['scene']=='complete' else index_library(job['source_path'],metadata_only=True,checkpoint=lambda:self.checkpoint(job,'download_index'))
        if not sources:raise PolicyBlocked('没有识别到音频文件')
        if len(sources)>2000:raise PolicyBlocked('单张发行超过 2000 个音频文件，需要核对资源范围')
        prepared=[]
        for i,row in enumerate(sources):
            self.checkpoint(job,'prepare_audio');source=Path(row['path']);reject_reparse_chain(source)
            original_hash=sha256_file(source);dest=work/f'{i:04d}{source.suffix.lower()}'
            if source.suffix.lower()=='.ncm':
                from musicops.media.ncm import decrypt_ncm
                result=decrypt_ncm(source,work/f'{i:04d}')
                if result['status']!='REQUIRES_VALIDATION':raise PolicyBlocked('NCM 解密未通过，原文件保留')
                dest=work/(f'{i:04d}.'+result['format']);Path(result['path']).rename(dest)
                meta=result.get('metadata',{});artist=[a[0] for a in meta.get('artist',[]) if isinstance(a,list) and a]
                tags={'title':meta.get('musicName'),'artist':artist,'album':meta.get('album') or meta.get('albumName')}
                apply_tag_changes(dest,{k:v for k,v in tags.items() if v})
                if result.get('artwork_base64'):apply_cover_changes(dest,base64.b64decode(result['artwork_base64']),min_side=cfg['lyrics']['min_cover_side'])
            else:durable_copy(source,dest)
            if sha256_file(source)!=original_hash:raise PolicyBlocked('原音源在复制时发生改变，请重新扫描')
            tags=read_tag_fields(dest);title=(tags.get('title') or [''])[0];artists=tags.get('artist',[]);album=(tags.get('album') or [''])[0]
            # NetEase response identity is an official song ID, not a guessed filename.
            target=next((t for t in job['targets'] if source.stem==str(t.get('download_file_id') or t.get('source_song_id'))),None)
            if target and not (title and artists and album):
                tags.update(title=[target['raw_title']],artist=target['artists'],album=[target['album']],tracknumber=[target.get('track') or str(i+1)],discnumber=[target.get('disc') or '1'])
                apply_tag_changes(dest,{k:v for k,v in tags.items() if k in ('title','artist','album','tracknumber','discnumber') and v})
            item={**row,'path':str(dest),'original_path':str(source),'original_hash':original_hash,'raw_title':(tags.get('title') or [''])[0],'artists':tags.get('artist',[]),'album':(tags.get('album') or [''])[0],'tags':tags,'sidecars':[],'old_sidecars':{}}
            for companion in row.get('companions',[]):
                cp=Path(companion['path']);reject_reparse_chain(cp)
                if companion['type']=='lyrics':
                    text=cp.read_text('utf-8-sig')
                    if validate_lrc(text,row.get('duration'))['valid']:
                        dst=dest.with_suffix('.lrc');durable_copy(cp,dst);item['sidecars'].append(str(dst))
                        item['old_sidecars'][str(cp)]=sha256_file(dst)
                elif companion['type']=='cover' and not embedded_covers(dest):
                    content=cp.read_bytes()
                    if validate_cover(content,cfg['lyrics']['min_cover_side'])['valid']:apply_cover_changes(dest,content,min_side=cfg['lyrics']['min_cover_side'])
            prepared.append(item)
        if any(not r['raw_title'] or not r['artists'] or not r['album'] for r in prepared):
            proposal=self._catalog_fill(job,prepared)
            if not proposal:
                self.put(job,prepared=prepared,stage='needs_review',review_kind='metadata',message='缺失资料没有唯一发行证据，需要人工识别');return
            album=proposal.get('album') or proposal['title']
            for row in prepared:
                changes={'album':album};apply_tag_changes(row['path'],changes);row['album']=album;row['tags']['album']=[album]
        self._enrich(job,prepared)
    def _enrich(self,job,prepared=None):
        prepared=prepared if prepared is not None else job['prepared']
        cfg=job['config'];album_names={strict_text(r['album']) for r in prepared}
        if len(album_names)!=1:raise PolicyBlocked('下载内容包含多个专辑，不能合并为一张发行')
        if any(not r['tags'].get('tracknumber') for r in prepared):
            proposal=self._catalog_fill(job,prepared)
            if proposal:
                for row in prepared:
                    matching=[t for t in proposal['tracklist'] if identity(t)==identity(row)]
                    if len(matching)==1 and matching[0].get('position') and not row['tags'].get('tracknumber'):
                        changes={'tracknumber':str(matching[0]['position'])}
                        if not row['tags'].get('discnumber'):changes['discnumber']=str(matching[0]['disc'])
                        apply_tag_changes(row['path'],changes);row['tags']=read_tag_fields(row['path'])
            if any(not r['tags'].get('tracknumber') for r in prepared):
                self.put(job,prepared=prepared,stage='needs_review',review_kind='numbering',message='缺少可靠曲序，需要核对每碟曲目编号后继续');return
        album=prepared[0]['album'];albumartists=prepared[0]['tags'].get('albumartist') or sorted(set(a for row in prepared for a in row['artists']))
        discs=[]
        for row in prepared:
            value=(row['tags'].get('discnumber') or ['1'])[0];match=re.match(r'^([1-9]\d*)',value)
            if not match:raise PolicyBlocked('碟号无效，需要先核对')
            discs.append(int(match.group(1)))
        total=max(discs)
        if set(discs)!=set(range(1,total+1)):raise PolicyBlocked('发行缺少部分光盘，暂不入库')
        from musicops.providers.cover_download import cover
        missing_cover=any(not embedded_covers(r['path']) for r in prepared);artwork=None
        if missing_cover:
            # A multi-disc release commonly keeps cover.jpg in its album root,
            # above CD1/CD2. This resource already belongs to the local release.
            local_covers={}
            parents={album_root({'path':r['original_path']},cfg['paths']['library']) for r in prepared}
            for parent in parents:
                reject_reparse_chain(parent)
                for candidate in parent.iterdir():
                    if candidate.is_file() and candidate.stem.casefold() in ('cover','folder','front') and candidate.suffix.casefold() in ('.jpg','.jpeg','.png','.webp') and candidate.stat().st_size<=10_000_000:
                        reject_reparse_chain(candidate);content=candidate.read_bytes()
                        if validate_cover(content,cfg['lyrics']['min_cover_side'])['valid']:local_covers[hashlib.sha256(content).hexdigest()]=content
            if len(local_covers)==1:artwork=next(iter(local_covers.values()))
            from musicops.providers.netease_match import netease_reference
            references=[netease_reference(t) for t in job['targets']]
            album_ids={str(t['source_album_id']) for t in references if t and t.get('source_album_id')}
            album_id=next(iter(album_ids)) if references and all(references) and len(album_ids)==1 else None
            release_id=None
            import mutagen
            audio=mutagen.File(prepared[0]['path']);release=(audio.tags or {}).get('musicbrainz_albumid') if audio else None
            if release:release_id=release[0]
            if not artwork and not album_id and not release_id:
                proposal=self._catalog_fill(job,prepared)
                if proposal:release_id=proposal.get('release_id') or (proposal.get('source_id') if proposal['source']=='musicbrainz' else None)
            try:
                if not artwork:artwork=cover(album,albumartists,album_id,release_id,cfg['lyrics']['min_cover_side'])
            except (HttpError,PolicyBlocked,__import__('httpx').HTTPError):artwork=None
        lyric_missing=0;cover_missing=0
        saved_locks=self.store.list('field_locks')
        for row,disc in zip(prepared,discs):
            self.checkpoint(job,'metadata_resources');changes={'albumartist':albumartists,'discnumber':f'{disc}/{total}'}
            locked=set(cfg['metadata']['locked_fields'])|{lock['field'] for lock in saved_locks if lock.get('identity')==row.get('file_id')}
            fields=read_tag_fields(row['path'])
            for field,value in changes.items():
                expected=[value] if isinstance(value,str) else value
                if field in locked and fields.get(field)!=expected:
                    self.put(job,prepared=prepared,stage='needs_review',review_kind='locks',message='资料字段被锁定，需要核对后解锁再继续',detail='受影响字段：'+field);return
            changes={k:v for k,v in changes.items() if k not in locked}
            apply_tag_changes(row['path'],changes);row['tags'].update({k:[v] if isinstance(v,str) else v for k,v in changes.items()})
            if artwork and not embedded_covers(row['path']):apply_cover_changes(row['path'],artwork,min_side=cfg['lyrics']['min_cover_side'])
            if not embedded_covers(row['path']):cover_missing+=1
            # Query only lyrics for automatic writes. Unrelated structurally valid
            # covers from a search service never become an album cover silently.
            resources=self.s._real_resources({'title':row['raw_title'],'artist':' / '.join(row['artists']),'album':album,'duration':row.get('duration'),'refresh':job.get('refresh_resources',False)},cfg) if not row['sidecars'] and not job.get('skip_lyrics_service') and '.example.invalid' not in cfg['lyrics']['primary']['base_url'] else {}
            state=resources.get('lyrics',{}).get('state')
            if state in ('AUTH_REQUIRED','AUTH_FAILED','ACCESS_BLOCKED','NETWORK_ERROR','TRANSIENT_ERROR','RATE_LIMITED'):
                self.put(job,prepared=prepared,stage='needs_review',review_kind='resources',message='歌词服务未通过：'+state,detail='这不代表歌曲没有歌词。请检查接口地址与鉴权，或明确跳过这次歌词服务。');return
            lyric=next((c for c in resources.get('candidates',[]) if c.get('kind')=='lyrics' and c.get('selected') and c.get('identity_state')=='matched'),None)
            if lyric and not row['sidecars']:
                path=Path(row['path']).with_suffix('.lrc');path.write_text(lyric['lyrics'],encoding='utf-8');row['sidecars'].append(str(path))
            if not row['sidecars']:
                from musicops.providers.netease_match import netease_reference
                target=next((ref for t in job['targets'] if identity(t)==identity(row) for ref in [netease_reference(t)] if ref),None)
                if target and cfg['providers']['netease']['enabled']:
                    from musicops.providers.netease_download import lyrics_for_id
                    text=lyrics_for_id(target['source_song_id'],row.get('duration'))
                    if text:
                        path=Path(row['path']).with_suffix('.lrc');path.write_text(text,encoding='utf-8');row['sidecars'].append(str(path))
            if not row['sidecars']:lyric_missing+=1
        self.put(job,prepared=prepared,album=album,albumartists=albumartists,total_discs=total,lyrics_missing=lyric_missing,cover_missing=cover_missing,stage='validating',progress=70,message='资料已整理，检查全部音频')
    def _validating(self,job):
        cfg=job['config'];validator=self.s._validator(cfg);prepared=job['prepared'];rows=[]
        for row in prepared:
            self.checkpoint(job,'full_decode');validation=validator.validate(row['path'])
            if validation['decode_state']!='passed' or not validation.get('roon_compatible'):raise PolicyBlocked('完整音频校验未通过，原音源保留')
            row['validation']=validation;row['media_info']=validation['media_info'];row['duration']=validation.get('duration');rows.append(row)
        if job['targets']:
            for target in job['targets']:
                matching=[r for r in rows if identity(r)==identity(target) and album_compatible(r.get('album'),target.get('album')) and target.get('duration') and r.get('duration') and abs(target['duration']-r['duration'])<=2]
                if len(matching)!=1:raise PolicyBlocked('下载内容与歌单录音或完整时长不一致，暂不入库')
        if job.get('cover_missing') and not job.get('cover_unavailable'):
            self.put(job,prepared=rows,stage='needs_review',review_kind='cover',message='音频通过检查，但缺少可靠封面；请选择已核对的专辑封面，或明确跳过');return
        if job['scene']=='upgrade':
            old=job['old_files'];by_id=collections.defaultdict(list)
            for row in rows:by_id[identity(row)].append(row)
            if len(old)!=len(rows) or any(len(by_id[identity(r)])!=1 for r in old):raise PolicyBlocked('新旧版完整曲目表不一致，原版保留')
            better=False;edition=True
            for r in old:
                n=by_id[identity(r)][0]
                existing_validation=validator.validate(r['path'])
                if existing_validation['decode_state']!='passed':raise PolicyBlocked('旧音源完整性或音质资料需要核对，暂不替换')
                r['media_info']=existing_validation['media_info']
                if not r.get('duration') or not n.get('duration') or abs(r['duration']-n['duration'])>2:raise PolicyBlocked('新旧版录音时长不一致，原版保留')
                if not no_downgrade(r['media_info'],n['media_info']):raise PolicyBlocked('新版包含音质下降的曲目，原版保留')
                better=better or improves(r['media_info'],n['media_info'],job['prefer_24bit'])
                prior=read_tag_fields(r['path']);new=read_tag_fields(n['path'])
                # A date alone is not evidence of the same mastering/edition.
                import mutagen
                a=mutagen.File(r['path']);b=mutagen.File(n['path'])
                a_id=(a.tags or {}).get('musicbrainz_albumid') if a else None;b_id=(b.tags or {}).get('musicbrainz_albumid') if b else None
                edition=edition and bool(a_id and b_id and list(a_id)==list(b_id))
            if not better:self.put(job,stage='skipped',message='新版没有实际音质提升，当前音源保留');return
            if not edition and not job.get('edition_approved'):
                self.put(job,prepared=rows,stage='needs_review',review_kind='edition',message='新版音质较高且曲目完整，但缺少同一发行的 ID，需要核对版本后替换');return
        self.put(job,prepared=rows,stage='publishing',progress=85,message='全部音频通过，准备入库')
    def _publishing(self,job):
        cfg=job['config'];root=Path(cfg['paths']['library']);operations=[];album=job['album'];artist=' & '.join(job['albumartists']);suffix=''
        if job['scene']=='upgrade':suffix=' [升级 '+job['job_id'][:8]+']'
        release=Path(segment(artist))/segment(album+suffix)
        for i,row in enumerate(job['prepared']):
            self.checkpoint(job,'build_publication');src=Path(row['path']);tags=read_tag_fields(src)
            if job['scene']=='fill' or job.get('existing_album_files'):
                preserved=job['old_files'] if job['scene']=='fill' else job['existing_album_files']
                original=next((r for r in preserved if identity(r)==identity(row) and r.get('duration') and row.get('duration') and abs(r['duration']-row['duration'])<=2),None)
                if original:
                    if original.get('original_hash') and sha256_file(original['path'])!=original['original_hash'] or (Path(original['path']).stat().st_size,Path(original['path']).stat().st_mtime_ns)!=(original['size'],original['mtime_ns']):raise PolicyBlocked('补全前原音源已改变，请重新检查；不会覆盖现有歌曲')
                    tags_before=read_tag_fields(original['path'])
                    if strict_text((tags_before.get('album') or [''])[0])!=strict_text(job['album']):raise PolicyBlocked('原音源与新版专辑标签不一致，请先核对发行或完成资料补全；不会混合不同版本')
                    continue
                release=album_root(preserved[0],root).relative_to(root)
            disc=int((tags.get('discnumber') or ['1'])[0].split('/')[0]);track=(tags.get('tracknumber') or [str(i+1)])[0].split('/')[0]
            folder=release/('CD'+str(disc)) if job['total_discs']>1 else release
            destination=Path(row['original_path']).relative_to(root) if job['scene']=='complete' else folder/(segment(track.zfill(2)+' '+row['raw_title'])+src.suffix)
            old=root/destination;before=row['original_hash'] if job['scene']=='complete' else None
            operations.append({'operation_id':job['job_id']+':audio:'+str(i),'kind':'replace' if before else 'publish','source':str(src),'source_hash':sha256_file(src),'destination':str(destination),'before_hash':before})
            for sidecar in row['sidecars']:
                target=destination.with_suffix('.lrc');existing=root/target;previous=row.get('old_sidecars',{}).get(str(existing)) if existing.is_file() else None
                if existing.is_file() and previous is None:raise PolicyBlocked('目标歌词已存在或改变，需要核对后再写入')
                operations.append({'operation_id':job['job_id']+':lrc:'+str(i),'kind':'replace' if previous else 'publish','source':sidecar,'source_hash':sha256_file(sidecar),'destination':str(target),'before_hash':previous})
        if not operations:
            self.put(job,stage='notifying',progress=95,message='现有歌曲已完整核验，无需重复发布');return
        plan={'schema_version':1,'plan_id':'workflow:'+job['job_id'],'config_hash':digest(cfg),'operations':operations}
        self.put(job,publication_plan=plan,stage='running',resume_stage='publishing',message='正在提交已经验证的文件')
        engine=TransactionEngine(self.store,job['engine_paths'],validator=self.s._validator(cfg));result=engine.execute(plan)
        if result['state']!='COMMITTED':raise PolicyBlocked('入库事务尚未提交，旧版保留')
        job['transaction_id']=result['tx_id'];self.put(job,stage='retiring' if job['scene']=='upgrade' else 'notifying',progress=95,message='新文件入库完成')
    def _retiring(self,job):
        # New publication must still verify before touching old files.
        cfg=job['config'];engine=TransactionEngine(self.store,job['engine_paths'],validator=self.s._validator(cfg));new=engine.execute(job['publication_plan'])
        if new['state']!='COMMITTED':raise PolicyBlocked('新版入库未确认，旧版保留')
        operations=[]
        companion_paths=set()
        for i,row in enumerate(job['old_files']):
            source=Path(row['path']);reject_reparse_chain(source)
            if source.stat().st_size!=row['size'] or source.stat().st_mtime_ns!=row['mtime_ns'] or sha256_file(source)!=row['original_hash']:raise PolicyBlocked('旧文件在扫描后发生改变，保留原版等待核对')
            operations.append({'operation_id':job['job_id']+':retire:'+str(i),'kind':'quarantine','source':str(source),'source_hash':sha256_file(source),'destination':str(Path(job['job_id'])/Path(row['relative_path'])),'before_hash':sha256_file(source),'quarantine_reason':'duplicate'})
            for path,expected in row.get('companion_hashes',{}).items():
                if path in companion_paths:continue
                companion_paths.add(path);companion=Path(path)
                if not companion.is_file() or sha256_file(companion)!=expected:raise PolicyBlocked('旧版伴随歌词在扫描后发生改变，暂不移出')
                relative=companion.relative_to(Path(cfg['paths']['library']))
                operations.append({'operation_id':job['job_id']+':retire-lrc:'+str(i),'kind':'quarantine','source':path,'source_hash':expected,'destination':str(Path(job['job_id'])/relative),'before_hash':expected,'quarantine_reason':'duplicate'})
        self.checkpoint(job,'before_retirement');result=engine.execute({'schema_version':1,'plan_id':'retire:'+job['job_id'],'config_hash':digest(cfg),'operations':operations})
        if result['state']!='COMMITTED':raise PolicyBlocked('旧版移出事务尚未提交，需要核对')
        self.put(job,retirement_transaction_id=result['tx_id'],stage='notifying',message='旧版已移出监听目录，恢复副本已保存')
    def _notifying(self,job):
        from musicops.clients.navidrome import scan
        library=Path(job['config']['paths']['library'])
        for op in job.get('publication_plan',{}).get('operations',[]):
            destination=library/op['destination']
            if destination.suffix.lower() in AUDIO_EXTENSIONS and destination.is_file():
                file_id=str(uuid.uuid5(uuid.NAMESPACE_URL,str(destination).casefold()));stat=destination.stat()
                self.store.put('evidence','resources-checked:'+file_id,{'kind':'resource_check','version':[stat.st_size,stat.st_mtime_ns],'checked_at':__import__('time').time(),'settings':digest(job['config']['lyrics'])})
        try:result=scan(job['config']['music_servers']['navidrome'],self.s.vault.get);message=result['message']
        except (HttpError,PolicyBlocked):message='音乐已入库；Navidrome 扫描未成功，请检查连接设置'
        self.put(job,stage='completed',progress=100,message='已完成 · '+message,detail=f"暂无匹配歌词：{job.get('lyrics_missing',0)} 首；未找到可靠封面：{job.get('cover_missing',0)} 首。恢复副本保存在音乐库外，不影响 Roon 分组。")
    def review(self,job_id,choice=None,metadata=None,cover_path=None,query=None,sites=None,expected_revision=None):
        job=self.store.get('evidence','workflow:'+job_id)
        if job and job.get('job_kind')=='batch':raise ValueError('请返回“'+SCENES.get(job['scene'],job['scene'])+'”页面重新开始；此准备记录不会自动重放')
        if expected_revision is not None:
            from musicops.app.assistance import review_revision
            if not job or review_revision(job)!=expected_revision:raise PolicyBlocked('任务资料已改变，请重新打开核对窗口')
        if choice in ('nonfree_once','nonfree_batch'):
            from musicops.app.download_policy import approve
            return approve(self,job or {},'once' if choice=='nonfree_once' else 'batch')
        if choice=='streaming_fallback' and job and job.get('stage')=='needs_review' and job.get('review_kind')=='search' and job['scene'] in ('playlist','fill'):
            if not job['config']['workflow']['netease_fallback']:raise PolicyBlocked('当前设置未启用流媒体回退')
            self._queue_processing(job,'fallback','用户确认在当前检索范围后回退流媒体',progress=20);return {'status':'OK'}
        if job and choice in ('retry','research') and (job.get('review_kind') in ('search','error','interrupted') or job['stage'] in ('failed','skipped') or choice=='research' and job.get('task_type')=='search'):
            from musicops.app.workflow_actions import retry
            return {'status':'OK',**retry(self,job,query=query,sites=sites,allow_review_search=choice=='research')}
        if not job or job['stage']!='needs_review' and not (choice=='edit_metadata' and job['stage'] in ('failed','skipped') and job.get('task_type') in ('identification','search')):raise ValueError('该任务不需要核对')
        if isinstance(choice,str) and choice.startswith('completion_release:') and job['scene']=='fill' and job.get('review_kind')=='metadata':
            candidate_id=choice[len('completion_release:'):];selected=next((c for c in job.get('catalog_choices',[]) if c.get('candidate_id')==candidate_id),None)
            if not selected:raise ValueError('请先选择目录中的实际发行')
            self.put(job,completion_choice=copy.deepcopy(selected),stage='identifying',review_kind=None,message='已选择完整发行，重新核对原音源与曲目表');return {'status':'OK'}
        if job.get('review_kind')=='numbering' and metadata:
            prepared=job['prepared']
            if len(metadata)!=len(prepared):raise ValueError('请逐曲核对曲序')
            for row,entry in zip(prepared,metadata):
                track=str(entry.get('track',''));disc=str(entry.get('disc',''))
                if not re.fullmatch(r'[1-9]\d{0,3}',track) or not re.fullmatch(r'[1-9]\d{0,2}',disc):raise ValueError('曲号和碟号需要为正整数')
                apply_tag_changes(row['path'],{'tracknumber':track,'discnumber':disc});row['tags']=read_tag_fields(row['path'])
            self.put(job,prepared=prepared,stage='enrich',message='用户核对曲序，继续补全与检查')
        elif job.get('review_kind')=='locks' and choice=='unlock':
            fields={'albumartist','discnumber'};current=self.s.config.current();values=copy.deepcopy(current['values']);values['metadata']['locked_fields']=[k for k in values['metadata']['locked_fields'] if k not in fields]
            saved=self.s.config.save(values,current['version'])
            identities={r.get('file_id') for r in job['prepared']}
            for lock in self.store.list('field_locks'):
                if lock.get('identity') in identities and lock.get('field') in fields:self.store.delete_lock(lock['id'])
            self.put(job,config=saved['values'],stage='enrich',message='用户解锁所需字段，继续整理副本')
        elif job.get('review_kind')=='resources':
            current=self.s.config.current()['values']
            if current['paths']['library']!=job['config']['paths']['library']:raise PolicyBlocked('音乐目录已改变，请建立新任务')
            self.put(job,config=copy.deepcopy(current),stage='enrich',refresh_resources=True,skip_lyrics_service=choice=='skip',message='重新检查歌词服务并继续处理')
        elif job.get('review_kind')=='cover':
            if choice=='skip':self.put(job,cover_unavailable=True,stage='validating',message='用户确认暂无可靠封面，继续核对音频')
            elif cover_path:
                path=Path(cover_path);reject_reparse_chain(path)
                if not path.is_file() or path.stat().st_size>10_000_000:raise ValueError('请选择不超过 10 MB 的专辑封面')
                content=path.read_bytes()
                for row in job['prepared']:
                    if not embedded_covers(row['path']):apply_cover_changes(row['path'],content,min_side=job['config']['lyrics']['min_cover_side'])
                self.put(job,cover_missing=0,stage='validating',message='用户确认封面已写入副本，重新检查完整音频')
            else:raise ValueError('请选择已确认的封面或跳过')
        elif (job.get('review_kind')=='metadata' or choice=='edit_metadata' and job.get('task_type') in ('identification','search')) and metadata:
            prepared=job.get('prepared',[])
            if not prepared:
                targets=job['targets'] or job['old_files']
                if len(metadata)!=len(targets):raise ValueError('请逐曲核对资料')
                for row,proposal in zip(targets,metadata):
                    if not all(isinstance(proposal.get(k),str) and proposal[k].strip() and len(proposal[k])<=500 for k in ('title','artist','album')):raise ValueError('请填写完整资料')
                    if 'duration' in proposal:
                        try:duration=float(proposal['duration']) if str(proposal['duration']).strip() else row.get('duration')
                        except (TypeError,ValueError):raise ValueError('完整时长请填写正数秒数') from None
                        if type(duration) not in (int,float) or not __import__('math').isfinite(duration) or not 0<duration<604800:raise ValueError('请核对并填写完整时长（秒）')
                        row['duration']=duration
                    row.update(raw_title=proposal['title'].strip(),artists=[a.strip() for a in proposal['artist'].split(' / ') if a.strip()],album=proposal['album'].strip(),metadata_complete=True)
                self.put(job,manual_metadata=True)
                if job.get('task_type')=='identification':self._queue_search(job,'采用用户核对的资料，已创建独立搜索任务')
                else:self.put(job,stage='searching',message='采用用户核对的资料，重新寻找发行')
                return {'status':'OK'}
            if not prepared or len(metadata)!=len(prepared):raise ValueError('请逐曲核对资料')
            for row,proposal in zip(prepared,metadata):
                if not all(isinstance(proposal.get(k),str) and proposal[k].strip() and len(proposal[k])<=500 for k in ('title','artist','album')):raise ValueError('请填写每首曲目的歌曲、艺人和专辑')
                path=Path(row['path']);reject_reparse_chain(path)
                if not path.resolve().is_relative_to(Path(job['prepared_root']).resolve()):raise PolicyBlocked('整理副本路径改变')
                artists=[a.strip() for a in proposal['artist'].split(' / ') if a.strip()]
                changes={'title':proposal['title'].strip(),'artist':artists,'album':proposal['album'].strip()}
                apply_tag_changes(path,changes);row.update(raw_title=changes['title'],artists=artists,album=changes['album'],tags=read_tag_fields(path))
            job['prepared']=prepared;job['manual_metadata']=True
            self.put(job,stage='enrich',message='已保存用户核对资料，继续补全与验证')
        elif job.get('review_kind')=='candidate':
            candidates=[c for c in job.get('choices',[]) if c['candidate_id']==choice]
            if len(candidates)!=1:raise ValueError('请选择有效的候选发行')
            self._queue_processing(job,'requesting','用户确认候选，准备下载',selected=candidates[0])
        elif job.get('review_kind')=='edition':self.put(job,edition_approved=True,stage='publishing',message='用户确认发行版本，准备替换')
        elif choice=='retry' and job.get('resume_stage') in ('identifying','searching','downloading','requesting','validating','notifying','retiring'):
            current=self.s.config.current()['values']
            if any(current['paths'][k]!=job['config']['paths'][k] for k in ('library','source','backup','quarantine')):raise PolicyBlocked('音乐目录已改变，请建立新的任务')
            self.put(job,config=copy.deepcopy(current),stage=job['resume_stage'],message='重新核对当前状态后继续处理')
        else:raise PolicyBlocked('需要先解决连接、文件或资料问题；不会重复消费 FL 或跳过音频校验')
        return {'status':'OK'}
    def restore(self,job_id):
        job=self.store.get('evidence','workflow:'+job_id)
        if not job or not job.get('engine_paths'):raise PolicyBlocked('该任务没有文件事务可恢复')
        if job['stage'] not in TERMINAL:raise PolicyBlocked('请先取消正在处理的任务，再恢复原版')
        engine=TransactionEngine(self.store,job['engine_paths'],validator=self.s._validator(job['config']))
        ids=[]
        for prefix in ('retire:','workflow:'):
            plan_id=prefix+job_id;tx_id=str(uuid.uuid5(uuid.NAMESPACE_URL,'musicops:transaction:'+plan_id))
            tx=self.store.get('transactions',tx_id)
            if tx:
                result=engine.rollback(tx_id)
                if result['state']!='ROLLED_BACK':raise PolicyBlocked('文件事务未完全恢复，请核对日志')
                ids.append(tx_id)
        if not ids:raise PolicyBlocked('该任务尚未修改音乐库，无需恢复')
        job.pop('cancel_requested',None)
        # Preserve cancel safety until this deliberate restore has completed.
        saved=self.store.get('evidence','workflow:'+job_id);saved.pop('cancel_requested',None);self.store.put('evidence','workflow:'+job_id,saved)
        self.put(job,stage='restored',progress=100,message='已恢复原版；本任务新发布的文件已移出，做种源保留')
        return {'status':'OK','restored':True,'transaction_ids':ids}
