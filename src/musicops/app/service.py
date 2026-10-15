from __future__ import annotations
import collections,copy,datetime as dt,hashlib,json,os,shutil,threading,time,uuid,wave
from pathlib import Path
from musicops.state.store import StateStore,utcnow
from musicops.config.schema import ConfigManager,digest
from musicops.clients.vault import Vault
from musicops.clients.http import PolicyBlocked,HttpError,redact

def sha(path):
    h=hashlib.sha256()
    with open(path,'rb') as f:
        for block in iter(lambda:f.read(1024*1024),b''): h.update(block)
    return h.hexdigest()

class TaskCancelled(RuntimeError):
    error_class='CANCELLED'

class AppService:
    """Single worker owns state. Every long command is a durable queued task."""
    def __init__(self,base,offline=True):
        self.base=Path(base).resolve(); self.base.mkdir(parents=True,exist_ok=True)
        self.offline=offline; self.store=StateStore(self.base/'state'/'musicops.sqlite')
        self.config=ConfigManager(self.store,self.base,initial_offline=offline); self.vault=Vault(self.base)
        self.paused=False; self.stop=threading.Event(); self.mutex=threading.RLock()
        self.active=None; self.thread=threading.Thread(target=self._loop,daemon=True,name='MusicOpsQueue')
        for task in self.store.list('tasks'):
            if task['state'] in ('running','paused'): task.update(state='recovery_required',error='上次进程中断；先核对事务/请求，不能盲重试'); self.store.put('tasks',task['task_id'],task)
        from musicops.app.pipeline import Workflows
        self.workflows=Workflows(self);self.workflows.recover();self._last_workflow_tick=0
        self.thread.start()
    def _safe_path(self,value):
        raw=Path(value).absolute()
        for parent in [raw,*raw.parents]:
            if parent.exists() and (parent.is_symlink() or getattr(parent,'is_junction',lambda:False)()):raise PolicyBlocked('拒绝重解析路径')
        p=raw.resolve()
        cfg=self.config.current()['values']
        roots=[self.base] if cfg['offline'] else [self.base,*[Path(cfg['paths'][key]).resolve() for key in ('library','source','work','backup','quarantine','reports')]]
        if not any(p.is_relative_to(root) for root in roots): raise PolicyBlocked('文件不属于设置中的音乐库或工作目录')
        for parent in [p,*p.parents]:
            if parent==self.base.parent: break
            if parent.exists() and (parent.is_symlink() or getattr(parent,'is_junction',lambda:False)()): raise PolicyBlocked('拒绝重解析路径')
        return p
    def dispatch(self,command,payload=None,request_id=None):
        with self.mutex:return self._dispatch(command,payload,request_id)
    def _dispatch(self,command,payload=None,request_id=None):
        payload=payload or {}
        if command=='save_setup':
            from musicops.app.setup import save_setup
            return save_setup(self,payload)
        if command=='list_workflows':return self.workflows.list(payload.get('offset',0))
        if command=='get_workflow':
            job=self.store.get('evidence','workflow:'+payload['job_id'])
            if not job:raise ValueError('任务不存在')
            public={k:job.get(k) for k in ('job_id','job_kind','scene','stage','message','detail','review_kind','choices','lookup_links','catalog_choices')}
            public['prepared']=[{**{k:r.get(k) for k in ('raw_title','artists','album','duration')},'track':(r.get('tags',{}).get('tracknumber') or [r.get('track','')])[0],'disc':(r.get('tags',{}).get('discnumber') or [r.get('disc','1')])[0]} for r in job.get('prepared',[]) or job.get('targets',[]) or job.get('old_files',[])]
            return {'job':public}
        if command=='cancel_workflow':return self.workflows.cancel(payload['job_id'])
        if command=='review_workflow':return self.workflows.review(payload['job_id'],payload.get('choice'),payload.get('metadata'),payload.get('cover_path'))
        if command in ('enable_online','enable_demo'):
            current=self.config.current();values=copy.deepcopy(current['values'])
            if command=='enable_demo' and not values['offline'] and self.base.name!='demo-profile':raise PolicyBlocked('演示使用独立demo-profile档案，不切换现有正式档案')
            desired=command=='enable_demo'
            if values['offline']!=desired:
                values['offline']=desired
                if desired:
                    from musicops.config.schema import default_config
                    values['paths']=default_config(self.base,offline=True)['paths']
                current=self.config.save(values,current['version'])
            return {'config':current,'mode':'demo' if desired else 'online','network_used':False}
        if command in ('attach','get_status'):
            if payload.get('compact'):
                with self.store.connection() as c:rows=c.execute("SELECT json_remove(payload,'$.args','$.config_snapshot','$.result') FROM tasks ORDER BY updated_at DESC,id LIMIT 200").fetchall()
                tasks=[json.loads(r[0]) for r in rows]
            else:tasks=self.store.list('tasks')
            with self.store.connection() as c:counts={r[0]:r[1] for r in c.execute("SELECT json_extract(payload,'$.state'),COUNT(*) FROM tasks GROUP BY json_extract(payload,'$.state')")}
            config=self.config.current();offline=config['values']['offline']
            with self.store.connection() as c:
                file_count=c.execute('SELECT COUNT(*) FROM library_files').fetchone()[0]
                file_rows=c.execute('SELECT payload FROM library_files ORDER BY id LIMIT ?',(500 if payload.get('compact') else 2000,)).fetchall()
            return dict(schema_version=1,base=str(self.base),offline=offline,paused=self.paused,active=self.active,config=config,tasks=tasks,counts=counts,provider_status={p:('演示数据' if offline else '已启用联网' if config['values']['providers'][p]['enabled'] else '来源已停用') for p in config['values']['providers']},roon=self.store.get('evidence','roon-preflight') or 'pending',next_schedule=self.next_schedule(),secret_refs=self.vault.list_refs(),files=[json.loads(r[0]) for r in file_rows],library_file_count=file_count,library_files_truncated=file_count>len(file_rows),candidates=self.store.list('candidates')[-200:],transactions=self.store.list('transactions'),reviews=self.store.list('match_decisions'))
        if command=='list_library_files':
            offset=payload.get('offset',0);limit=payload.get('limit',200);query=payload.get('query','')
            if type(offset) is not int or offset<0 or type(limit) is not int or not 1<=limit<=500 or not isinstance(query,str) or len(query)>300:raise ValueError('索引分页参数无效')
            pattern='%'+query.replace('\\','\\\\').replace('%','\\%').replace('_','\\_')+'%'
            where=" WHERE json_extract(payload,'$.raw_title') LIKE ? ESCAPE '\\' OR json_extract(payload,'$.artists') LIKE ? ESCAPE '\\' OR json_extract(payload,'$.album') LIKE ? ESCAPE '\\' OR json_extract(payload,'$.path') LIKE ? ESCAPE '\\'" if query else ''
            params=(pattern,)*4 if query else ()
            with self.store.connection() as c:
                total=c.execute('SELECT COUNT(*) FROM library_files'+where,params).fetchone()[0]
                rows=c.execute('SELECT payload FROM library_files'+where+' ORDER BY id LIMIT ? OFFSET ?',(*params,limit,offset)).fetchall()
            return {'files':[json.loads(r[0]) for r in rows],'total':total,'offset':offset,'limit':limit,'network_used':False}
        if command=='list_tasks': return {'tasks':self.store.list('tasks')}
        if command=='get_task':
            task=self.store.get('tasks',payload['task_id'])
            if task is None:raise KeyError('TASK_NOT_FOUND')
            return {'task':task}
        if command=='get_playlist_targets':return {'targets':self.store.list('target_tracks'),'network_used':False}
        if command=='events': return {'events':self.store.events(payload.get('after',0))}
        if command=='update_config': return {'config':self.config.save(payload['values'],payload['expected_version'])}
        if command=='export_config':
            data=self.config.export()
            if payload.get('path'):
                p=Path(payload['path']); p.parent.mkdir(parents=True,exist_ok=True); p.write_text(json.dumps(data,ensure_ascii=False,indent=2),encoding='utf-8')
            return {'config':data,'path':payload.get('path')}
        if command=='import_config':
            data=json.loads(Path(payload['path']).read_text(encoding='utf-8-sig')) if payload.get('path') else payload['data']
            return {'config':self.config.import_config(data,payload['expected_version'])}
        if command=='set_secret': return self.vault.set(payload['ref'],payload['value'],payload['host'])
        if command=='import_pt_cookies':
            from musicops.clients.vault import import_cookies
            from musicops.providers.pt_readonly import HOSTS
            site=payload['site'];host=HOSTS[site];ref='vault://pt/'+site+'/cookies'
            if __import__('urllib.parse',fromlist=['urlsplit']).urlsplit(self.config.current()['values']['providers'][site]['base_url']).hostname!=host:raise PolicyBlocked('导入站点域名与配置不一致')
            cookies=import_cookies(Path(payload['path']).read_text('utf-8-sig'),host)
            saved=self.vault.set(ref,json.dumps(cookies),host)
            current=self.config.current();values=copy.deepcopy(current['values']);values['providers'][site]['credential_ref']=ref
            config=self.config.save(values,current['version'])
            return {'config':config,'credential':saved,'imported_count':len(cookies),'network_used':False}
        if command=='import_lyrics_auth':
            from musicops.clients.vault import read_lyrics_auth_file
            from urllib.parse import urlsplit
            current=self.config.current();values=copy.deepcopy(current['values']);primary=values['lyrics']['primary']
            ref=primary.get('auth_secret_ref') or 'vault://lyrics/private/authorization'
            saved=self.vault.set(ref,read_lyrics_auth_file(payload['path']),urlsplit(primary['base_url']).hostname)
            primary['auth_secret_ref']=ref
            return {'config':self.config.save(values,current['version']),'credential':saved,'network_used':False}
        if command=='parse_playlist':
            from musicops.providers.netease import parse_playlist_input
            return parse_playlist_input(payload['text'])
        if command=='add_playlists':
            from musicops.providers.netease import parse_playlist_input
            result=parse_playlist_input(payload['text'])
            if result['status']!='OK': return result
            current=self.config.current(); values=copy.deepcopy(current['values']); values['playlist_ids']=list(dict.fromkeys(values['playlist_ids']+result['playlist_ids']))
            return {'config':self.config.save(values,current['version']),'playlist_ids':values['playlist_ids'],'network_used':False}
        if command=='pause_pipeline': self.paused=True; self.store.event('pipeline_paused',{'seeding_unchanged':True}); return {'paused':True}
        if command=='resume_pipeline': self.paused=False; self.store.event('pipeline_resumed',{}); return {'paused':False}
        if command=='cancel_task':
            task=self.store.get('tasks',payload['task_id'])
            if not task: raise KeyError('TASK_NOT_FOUND')
            if task['state'] in ('queued','paused'): task['state']='cancelled'
            elif task['state']=='running': task['cancel_requested']=True
            else: return {'task':task,'reason':'终态任务不再取消；已发布内容用回滚'}
            self.store.put('tasks',task['task_id'],task); return {'task':task,'seeding_unchanged':True}
        if command=='review_decision':
            identity=payload['identity']; decision={'schema_version':1,'identity':identity,'choice':payload['choice'],'reason':payload.get('reason',''),'reviewer':'current_user','at':utcnow()}
            if payload['choice']=='unlock':
                for row in self.store.list('field_locks'):
                    if row.get('identity')==identity and (not payload.get('fields') or row.get('field') in payload['fields']): self.store.delete_lock(row['id'])
                return {'decision':decision,'unlocked':True}
            self.store.put('evidence','manual:'+str(uuid.uuid4()),{'schema_version':1,'kind':'manual_review','observed_at':utcnow(),'scope':identity,'confidence':'user','decision':decision})
            old=self.store.get('match_decisions',identity)
            if old: old.update(reviewer='current_user',manual_choice=decision['choice'],manual_reason=decision['reason']); self.store.put('match_decisions',identity,old)
            for field in payload.get('fields',[]): self.store.put('field_locks',identity+':'+field,dict(identity=identity,field=field,reason=decision['reason']))
            return {'decision':decision}
        if command=='shutdown_worker':
            if self.active: raise RuntimeError('当前任务尚未到安全退出点，请先暂停流水线')
            self.stop.set(); return {'stopped':True,'seeding_unchanged':True}
        if command in ('manual_album_search','restore_workflow','start_workflow','advance_workflows','demo','index_library','compare','search','resources','prepare_fill','prepare_resource','prepare_repair','prepare_dedup','submit_plan','request_rollback','export_report','refresh_playlist','submit_download','recover','pt_preflight','qbt_preflight','ai_analyze','ai_plan','ai_test','lyrics_preflight','identify_metadata','resolve_metadata','accept_metadata','validate_selected_file','roon_preflight'):
            return self.enqueue(command,payload,request_id=request_id)
        raise ValueError('UNKNOWN_COMMAND:'+command)
    def enqueue(self,command,args,request_id=None):
        task_id=str(uuid.uuid5(uuid.NAMESPACE_URL,'ipc:'+request_id)) if request_id else str(uuid.uuid4()); cfg=copy.deepcopy(self.config.current())
        if self.store.get('tasks',task_id): return {'accepted':True,'task_id':task_id,'state':self.store.get('tasks',task_id)['state'],'idempotent':True}
        args=copy.deepcopy(args);batch=None
        if command=='start_workflow':
            batch=self.workflows.queue(args,cfg['values'],task_id);args['_batch_job_id']=batch['job_id']
        task=dict(schema_version=1,task_id=task_id,command=command,args=copy.deepcopy(args),state='queued',created_at=utcnow(),config_version=cfg['version'],config_snapshot=cfg,progress=0,result=None)
        self.store.put('tasks',task_id,task); self.store.event('task_queued',{'command':command,'config_version':cfg['version']},task_id)
        return {'accepted':True,'task_id':task_id,'state':'queued',**({'job_id':batch['job_id'],'batch_id':batch['batch_id']} if batch else {})}
    def _loop(self):
        while not self.stop.wait(.1):
            with self.mutex:
                if self.paused: continue
                with self.store.connection() as c:row=c.execute("SELECT payload FROM tasks WHERE json_extract(payload,'$.state')='queued' ORDER BY updated_at,id LIMIT 1").fetchone()
                if row is None:
                    if time.monotonic()-self._last_workflow_tick<3:continue
                    self._last_workflow_tick=time.monotonic()
                    if self.workflows.next_job():self.enqueue('advance_workflows',{})
                    continue
                task=json.loads(row[0]); self.active=task['task_id']; task.update(state='running',started_at=utcnow()); self.store.put('tasks',self.active,task)
            try:
                self._checkpoint('before_execute')
                result=self._execute(task['command'],task['args'],task['config_snapshot']['values'])
                state=result.get('state',result.get('status','')) if isinstance(result,dict) else ''
                mapped=self._task_state(state)
                task.update(state=mapped,progress=100 if mapped=='completed' else 0,result=redact(result),finished_at=utcnow())
                if mapped!='completed': task.update(error=result.get('reason',result.get('error',state)),error_class=result.get('error_class',state))
            except Exception as e:
                error_class=getattr(e,'error_class',type(e).__name__)
                state=self._task_state(error_class)
                if state=='completed':state='failed'
                task.update(state=state,error=redact(str(e)),error_class=error_class,finished_at=utcnow())
            saved=self.store.get('tasks',task['task_id'])
            if saved and saved.get('cancel_requested'): task['cancel_requested']=True; task['cancel_result']='任务已到安全点，未自动回滚或停种'
            if task['command']=='start_workflow' and task['state']!='completed' and task.get('args',{}).get('_batch_job_id'):
                self.workflows.start_failed(task['args']['_batch_job_id'],task.get('error_class',task['state']),task.get('error',''))
            self.store.put('tasks',task['task_id'],task); self.store.event('task_'+task['state'],{'command':task['command'],'error':task.get('error'),'result':task.get('result')},task['task_id']); self.active=None
    @staticmethod
    def _task_state(state):
        if state=='CANCELLED':return 'cancelled'
        if state in ('POLICY_BLOCKED','AUTH_REQUIRED','AUTH_FAILED','RATE_LIMITED','ACCESS_BLOCKED','INVALID_INPUT','INPUT_TOO_LARGE','NEEDS_NETWORK','API_CONTRACT_ERROR','SNAPSHOT_INCOMPLETE'):return 'blocked'
        if state in ('CONFLICT','RECOVERY_REQUIRED','REQUEST_UNKNOWN','ADD_UNKNOWN','START_UNKNOWN'):return 'recovery_required'
        if state in ('NETWORK_ERROR','TRANSIENT_ERROR','PARSE_ERROR','INVALID_PAYLOAD','VALIDATION_FAILED','FAILED','corrupt','unsupported','timeout','tool_error','changed_during_check'):return 'failed'
        return 'completed'
    def _checkpoint(self,phase):
        if not self.active or getattr(self,'_safe_finalizing',False):return
        marked=False
        while True:
            current=self.store.get('tasks',self.active)
            if not current or current.get('cancel_requested') or current['state']=='cancelled' or self.stop.is_set(): raise TaskCancelled('已在安全阶段取消后续步骤；已消费令牌与保种记录保留')
            if not self.paused:
                if marked: current['state']='running';self.store.put('tasks',self.active,current)
                return
            if not marked:
                current.update(state='paused',safe_phase=phase);self.store.put('tasks',self.active,current);self.store.event('task_safe_paused',{'phase':phase,'seeding_unchanged':True},self.active);marked=True
            self.stop.wait(.1)
    def _validator(self,cfg):
        from musicops.media.validation import MediaValidator
        return MediaValidator(cfg['paths']['tools']['ffmpeg'],cfg['paths']['tools']['ffprobe'],timeout=cfg['worker']['decode_timeout'])
    def _engine(self,cfg):
        from musicops.library.transactions import TransactionEngine
        for key in ('library','source','work','backup','quarantine'): self._safe_path(cfg['paths'][key])
        return TransactionEngine(self.store,cfg['paths'],validator=self._validator(cfg))
    def _execute(self,command,args,cfg):
        if command=='manual_album_search':
            from musicops.app.album_search import manual_album_search
            return manual_album_search(self,args,cfg)
        if command=='start_workflow':return self.workflows.start(args,cfg)
        if command=='advance_workflows':return self.workflows.advance()
        if command=='restore_workflow':return self.workflows.restore(args['job_id'])
        if command in ('identify_metadata','resolve_metadata','accept_metadata'):
            if cfg['offline']:raise PolicyBlocked('请启用正式联网模式后进行在线识别')
            return self._catalog_command(command,args,cfg)
        if command=='roon_preflight':
            if cfg['offline']:raise PolicyBlocked('请启用正式联网模式后检查Roon')
            from musicops.clients.roon import RoonReadOnlyClient,inspect_local_logs
            result=RoonReadOnlyClient(cfg['music_servers']['roon'],checkpoint=self._checkpoint).preflight()
            result['storage_diagnostics']=inspect_local_logs(expected_root=cfg['music_servers']['roon']['library'] or cfg['paths']['library'])
            self.store.put('evidence','roon-preflight',result)
            return result
        if command=='index_library' and not cfg['offline']:return self._index_real_library(args,cfg)
        if command=='compare' and not cfg['offline']:return self._compare_targets(args)
        if command=='validate_selected_file' and not cfg['offline']:
            record=self.store.get('library_files',args['file_id'])
            if not record or record.get('location_state','library')!='library':raise PolicyBlocked('请先索引并选定主库中的文件')
            from musicops.library.paths import reject_reparse_chain
            path=Path(record['path']);reject_reparse_chain(path)
            if not path.resolve().is_relative_to(Path(record.get('library_root',cfg['paths']['library'])).resolve()):raise PolicyBlocked('文件不属于已索引主库')
            stat=path.stat()
            if (record.get('size'),record.get('mtime_ns'))!=(stat.st_size,stat.st_mtime_ns):raise PolicyBlocked('文件已改变，请重新建立元数据索引')
            result=self._validator(cfg).validate(path);record['validation']=result;record['validation_id']=result.get('validation_id')
            self.store.put('library_files',record['file_id'],record)
            return {'file':record,'validation':result,'music_writes':0}
        if command=='qbt_preflight':
            if cfg['offline']:raise PolicyBlocked('离线模式不访问真实qBittorrent')
            from musicops.clients.qbt import readonly_preflight
            return readonly_preflight(cfg['qbt'],self.vault.get)
        if command in ('ai_analyze','ai_plan','ai_test'):
            from musicops.providers.ai import AIProvider
            from musicops.clients.http import SafeHttpTransport
            from urllib.parse import urlparse
            if cfg['offline']:raise PolicyBlocked('离线模式不访问AI；保存真实只读模式后重试')
            if not cfg['ai'].get('enabled'):raise PolicyBlocked('AI未启用')
            provider=AIProvider(cfg['ai'],SafeHttpTransport([urlparse(cfg['ai']['base_url']).hostname],offline=False),self.vault.get)
            if command=='ai_test':return {'status':'OK','result':provider.analyze({'connection_test':True},'仅回答连接成功。')}
            if command=='ai_analyze':return provider.analyze(args.get('context',{}),args.get('instructions',''))
            if not cfg['ai'].get('allow_plan_only'):raise PolicyBlocked('AI计划未启用')
            targets=self.store.list('target_tracks');decisions=self.store.list('match_decisions')
            state={'target_count':len(targets),'targets':targets[:50],'targets_truncated':len(targets)>50,
                   'decision_count':len(decisions),'decisions':decisions[:50],'decisions_truncated':len(decisions)>50,
                   'tasks':[{'command':t['command'],'state':t['state']} for t in self.store.list('tasks')[-20:]]}
            return provider.plan(args.get('message',''),state)
        if command=='pt_preflight':
            if cfg['offline']:raise PolicyBlocked('离线模式不访问真实PT')
            provider=self._real_pt(args['site'],cfg);outcome=provider.preflight()
            self.store.put('evidence','pt-preflight:'+args['site'],outcome)
            return outcome
        if command=='search' and not cfg['offline']:return self._search(args,cfg)
        if command=='refresh_playlist' and not cfg['offline']:return self._refresh_real_playlists(args,cfg)
        if command in ('resources','lyrics_preflight') and not cfg['offline']:return self._real_resources(args,cfg,private_only=command=='lyrics_preflight')
        if not cfg['offline']:raise PolicyBlocked('正式联网版提供在线识别、只读索引与比对、歌单/PT/歌词封面查询及AI；此操作尚未开放生产文件写入或下载')
        paths=cfg['paths']
        # Configuration can describe future production paths, but offline execution
        # validates every IO root before any glob, mkdir, media read or copy.
        for key in ('library','source','work','backup','quarantine','reports'): self._safe_path(paths[key])
        self._checkpoint('before_'+command)
        if command=='demo': return self._demo(cfg)
        if command=='index_library':
            from musicops.library.index import index_library
            files=index_library(self._safe_path(args.get('root',paths['library'])),self._validator(cfg),exclusions=[])
            for f in files: self.store.put('library_files',f['file_id'],f)
            live={f['file_id'] for f in files}
            for old in self.store.list('library_files'):
                if old['file_id'] not in live and old.get('location_state','library')=='library' and not Path(old['path']).exists():
                    old.update(location_state='externally_missing',read_error='文件已移出主库'); self.store.put('library_files',old['file_id'],old)
            return {'files':files,'count':len(files),'format_counts':dict(collections.Counter(f.get('media_info',{}).get('codec','unknown') for f in files))}
        if command=='compare':
            from musicops.domain.identity import match_track
            targets=args.get('targets') or self.store.list('target_tracks'); files=self.store.list('library_files'); results=[]
            for target in targets:
                decision=match_track(target,files); self.store.put('match_decisions',target['target_id'],decision); results.append(decision)
            return {'decisions':results,'total':len(results),'counts':dict(collections.Counter(r['main_state'] for r in results)),'roon_state':'pending'}
        if command=='refresh_playlist':
            from musicops.providers.netease import build_snapshot
            tracks=self.store.list('target_tracks'); ids=[t['source_song_id'] for t in tracks]
            snapshots=[]; playlist_ids=([args['playlist_id']] if args.get('playlist_id') else args.get('playlist_ids') or cfg['playlist_ids'] or ['10001'])
            if any(str(pid) not in ('10001','10002') for pid in playlist_ids): raise PolicyBlocked('离线仅提供显式演示歌单10001/10002映射；真实歌单ID只本地解析，尚未联网刷新')
            for pid in playlist_ids:
                snapshot=build_snapshot(str(pid),ids,lambda batch:[dict(id=t['source_song_id'],raw_title=t['raw_title'],artists=t['artists'],source_album_id=t['source_album_id'],album=t['album'],version=t['version'],duration=t['duration'],date_raw=t.get('date_raw')) for t in tracks if t['source_song_id'] in batch],batch_size=cfg['netease']['batch_size']); snapshot.update(evidence_level='synthetic_only',source='netease')
                snapshot_id='netease:'+str(pid)+':'+snapshot['hash']; snapshot['snapshot_id']=snapshot_id
                if snapshot['complete']: self.store.put('playlist_snapshots',snapshot_id,snapshot)
                else: self.store.put('evidence','incomplete:'+str(uuid.uuid4()),dict(kind='incomplete_snapshot',snapshot=snapshot))
                snapshots.append(snapshot)
            for target in tracks:
                target.update(snapshot_id=snapshots[0]['snapshot_id'],snapshot_ids=[s['snapshot_id'] for s in snapshots],snapshot_complete=all(s['complete'] for s in snapshots),evidence_level='synthetic_only')
                self.store.put('target_tracks',target['target_id'],target)
            return {'snapshots':snapshots,'complete':all(s['complete'] for s in snapshots),'target_ids_unique':ids,'evidence':'synthetic_only'}
        if command=='search': return self._search(args,cfg)
        if command=='resources':
            from musicops.providers.lrcapi import LrcApiClient
            from musicops.clients.http import FakeTransport,Response
            title=args.get('title','晨光'); artist=args.get('artist','示例艺人'); album=args.get('album','离线演示专辑')
            private=FakeTransport([Response(200,json.dumps([dict(id='demo',title=title,artist=artist,album=album,lyrics='[00:00.00]晨光\n[00:00.50]合成测试歌词')],ensure_ascii=False).encode()),Response(200,Path(paths['source'],'cover.jpg').read_bytes(),{'content-type':'image/jpeg'})])
            client=LrcApiClient(cfg['lyrics'],{'private':private,'public':FakeTransport([])},secret_getter=lambda ref,host:'offline-virtual-authorization')
            lyrics=client.candidates(title,artist,album,duration=args.get('duration',2))
            cover=client.cover(title,artist,album)
            # bytes are stored as local previews; IPC remains JSON.
            def serial(value):
                if isinstance(value,bytes):
                    import base64
                    return base64.b64encode(value).decode()
                if isinstance(value,dict): return {('content_base64' if k=='content_bytes' else k):serial(v) for k,v in value.items()}
                if isinstance(value,list): return [serial(v) for v in value]
                return value
            candidates=serial(lyrics.get('candidates',[])+cover.get('candidates',[]))
            for cand in candidates: self.store.put('resource_candidates',cand['resource_id'],cand)
            return {'lyrics':serial(lyrics),'cover':serial(cover),'candidates':candidates,'cover_preview':str(Path(paths['source'],'cover.jpg')),'lrc_text':'[00:00.00]晨光\n[00:00.50]合成测试歌词','evidence':'offline_fixture'}
        if command=='prepare_repair':
            from musicops.media.tags import apply_tag_changes,plan_tag_changes
            path=self._safe_path(args['path']); changes=args['changes']; identity=str(uuid.uuid5(uuid.NAMESPACE_URL,str(path).casefold()))
            before_hash=sha(path)
            locks=list(set(args.get('locks',[])+cfg['metadata'].get('locked_fields',[])+[r['field'] for r in self.store.list('field_locks') if r.get('identity') in (identity,str(path))]))
            for field in args.get('locks',[]): self.store.put('field_locks',identity+':'+field,dict(identity=identity,field=field,reason='用户在字段修复中锁定',created_at=utcnow()))
            preview=plan_tag_changes(path,changes,locks=locks)
            staged=Path(paths['work'])/('repair-'+str(uuid.uuid4()))/path.name; staged.parent.mkdir(parents=True); shutil.copy2(path,staged)
            if sha(staged)!=before_hash or sha(path)!=before_hash:raise HttpError('CONFLICT','生成修复副本期间原件发生变化，请重新生成预览')
            apply_tag_changes(staged,preview['changes'])
            check=self._validator(cfg).validate(staged)
            self._checkpoint('repair_validated')
            if check['decode_state']!='passed': raise RuntimeError('修复副本校验失败')
            plan=dict(schema_version=1,plan_id=str(uuid.uuid4()),config_hash=digest(cfg),reviewed=False,operations=[dict(operation_id=str(uuid.uuid4()),kind='replace',source=str(staged),source_hash=sha(staged),destination=str(path.relative_to(Path(paths['library']))),before_hash=before_hash,field_locks=locks,tag_changes=preview['changes'])],preview=preview)
            self.store.put('change_plans',plan['plan_id'],plan); return {'plan':plan,'preview':preview}
        if command=='prepare_resource':
            from musicops.media.resources import validate_lrc,validate_cover,resource_change_allowed
            args=copy.deepcopy(args)
            candidate=copy.deepcopy(args.get('candidate',{}))
            if candidate.get('resource_id'):
                saved=self.store.get('resource_candidates',candidate['resource_id'])
                if not saved:raise PolicyBlocked('资源候选没有已保存检索证据，请重新查询')
                if any(candidate.get(k)!=saved.get(k) for k in ('kind','provider','content_hash','identity_state','title','artist','album','version','query_context','path','content_base64')):raise PolicyBlocked('资源候选与已保存身份/内容证据不一致，请重新查询')
                args['candidate']=saved
            else:
                candidate['identity_state']='mismatch' if candidate.get('identity_state')=='mismatch' else 'structural_only'
                args['candidate']=candidate
            path=self._safe_path(args['path']); kind=args['kind']; staged_root=Path(paths['work'])/('resource-'+str(uuid.uuid4())); staged_root.mkdir(parents=True)
            from musicops.library.index import AUDIO_EXTENSIONS
            if not path.is_file() or not path.is_relative_to(Path(paths['library']).resolve()) or path.suffix.lower() not in AUDIO_EXTENSIONS:raise PolicyBlocked('资源计划须选定主库内现有音频文件')
            audio_before=sha(path) if cfg['lyrics']['write_mode']=='embedded' else None
            identity=str(uuid.uuid5(uuid.NAMESPACE_URL,str(path).casefold()))
            if args.get('locked'):
                self.store.put('field_locks',identity+':'+kind,dict(identity=identity,field=kind,reason='用户资源锁定',created_at=utcnow()))
                raise PolicyBlocked('资源已被用户锁定')
            if any(r.get('identity') in (identity,str(path)) and r.get('field')==kind for r in self.store.list('field_locks')): raise PolicyBlocked('资源已有持久锁定，先明确解除锁定')
            mode=cfg['lyrics']['write_mode']
            if kind=='lyrics':
                text=args.get('lyrics') or args.get('candidate',{}).get('lyrics',''); check=validate_lrc(text,duration=args.get('duration'))
                if not check['valid']:raise RuntimeError('歌词内容校验失败；未写资源内容')
                dest=path.with_suffix('.lrc'); staged=staged_root/dest.name; staged.write_text(text,encoding='utf-8')
            elif kind=='cover':
                import base64
                cand=args.get('candidate',{}); content=base64.b64decode(cand['content_base64']) if cand.get('content_base64') else self._safe_path(cand.get('path',str(Path(paths['source'])/'cover.jpg'))).read_bytes()
                check=validate_cover(content,min_side=cfg['lyrics']['min_cover_side'])
                if not check['valid']:raise RuntimeError('封面实际内容校验失败；未写资源内容')
                extension={'JPEG':'.jpg','PNG':'.png','WEBP':'.webp'}[check['format']]
                dest=path.parent/('cover'+extension); staged=staged_root/dest.name; staged.write_bytes(content)
            else: raise ValueError('未知资源类型')
            if check.get('valid') is False or check.get('state') in ('INVALID_PAYLOAD','MISMATCH'): raise RuntimeError('资源校验失败')
            candidate=copy.deepcopy(args.get('candidate',{}))
            if candidate.get('content_hash') and candidate['content_hash']!=check.get('sha256') and candidate.get('identity_state')!='mismatch':candidate['identity_state']='structural_only'
            permission=resource_change_allowed(None,{**candidate,'validation':check},approved=args.get('approved') is True)
            if not permission['allowed']:raise PolicyBlocked('资源身份或审核未通过：'+permission['reason'])
            if mode=='embedded':
                from musicops.media.tags import apply_tag_changes,apply_cover_changes
                dest=path;staged=staged_root/path.name;shutil.copy2(path,staged)
                if sha(staged)!=audio_before or sha(path)!=audio_before:raise HttpError('CONFLICT','生成嵌入资源副本期间原件发生变化，请重新生成预览')
                if kind=='lyrics':readback=apply_tag_changes(staged,{'lyrics':[text]})
                else:readback=apply_cover_changes(staged,content)
                media_check=self._validator(cfg).validate(staged)
                if media_check['decode_state']!='passed': raise RuntimeError('嵌入资源后完整媒体校验失败')
                self._checkpoint('embedded_resource_validated')
                check={'resource':check,'readback':readback,'media':media_check,'valid':True}
            elif mode!='sidecar':raise PolicyBlocked('未支持的资源写入模式，未发布任何内容')
            if dest.exists() and not args.get('approved'): raise PolicyBlocked('已有资源替换须先审查旧/新内容')
            plan=dict(schema_version=1,plan_id=str(uuid.uuid4()),reviewed=False,config_hash=digest(cfg),operations=[dict(operation_id=str(uuid.uuid4()),source=str(staged),source_hash=sha(staged),destination=str(dest.relative_to(Path(paths['library']))),before_hash=audio_before if mode=='embedded' else sha(dest) if dest.exists() else None,kind='replace' if dest.exists() else 'publish')],preview={'kind':kind,'mode':mode,'path':str(dest),'identity_state':candidate.get('identity_state','structural_only'),'validation':check})
            self.store.put('change_plans',plan['plan_id'],plan); return {'plan':plan,'preview':plan['preview']}
        if command=='prepare_dedup':
            from musicops.domain.quality import propose_duplicates
            files=args.get('files') or self.store.list('library_files')
            for file in files: file['field_locks']=[r['field'] for r in self.store.list('field_locks') if r.get('identity') in (file['file_id'],file['path'])]
            suggestions=propose_duplicates(files)
            selected=args.get('selected_file_id')
            if not selected: return {'suggestions':suggestions,'policy':'只隔离；保留恢复记录；不动做种源'}
            suggestion=next((s for s in suggestions if s.get('quarantine_file_id')==selected and s['action']=='quarantine_candidate'),None)
            if not suggestion: raise PolicyBlocked('选择项没有同录音/同发行及完整性确证，须复核')
            file=next(f for f in files if f['file_id']==selected); source=self._safe_path(file['path']); plan_id=str(uuid.uuid4())
            operations=[dict(operation_id=plan_id+':audio',kind='quarantine',source=str(source),destination=plan_id+'/'+source.name,before_hash=sha(source),quarantine_reason='duplicate')]
            lrc=source.with_suffix('.lrc')
            if lrc.exists(): operations.append(dict(operation_id=plan_id+':lrc',kind='quarantine',source=str(lrc),destination=plan_id+'/'+lrc.name,before_hash=sha(lrc),quarantine_reason='duplicate'))
            plan=dict(schema_version=1,plan_id=plan_id,reviewed=False,operations=operations,config_hash=digest(cfg),preview=suggestion)
            self.store.put('change_plans',plan_id,plan); return {'suggestions':suggestions,'plan':plan,'preview':suggestion}
        if command=='submit_download':
            prepared=self._execute('prepare_fill',{'candidate':args['candidate']},cfg); prepared['plan']['reviewed']=args.get('reviewed',False)
            return self._execute('submit_plan',{'plan':prepared['plan']},cfg)
        if command=='prepare_fill':
            candidate=args['candidate']; target,snapshot_hash=self._fill_preflight(candidate)
            source=self._safe_path(candidate['source_path']); check=self._validator(cfg).validate(source)
            if check['decode_state']!='passed': raise RuntimeError('来源样本未通过完整验证')
            plan=dict(schema_version=1,plan_id='fill:'+candidate['candidate_id'],config_hash=digest(cfg),snapshot_hash=snapshot_hash,target_id=target['target_id'],reviewed=False,operations=[dict(operation_id='publish:'+candidate['candidate_id'],source=str(source),source_hash=sha(source),destination=candidate.get('destination','示例艺人/离线演示专辑/02 星河.flac'),before_hash=None,kind='publish')],budget={candidate['source']:candidate.get('estimated_cost',0)},evidence=candidate.get('evidence'),download_candidate=candidate)
            self.store.put('change_plans',plan['plan_id'],plan); return {'plan':plan,'preview':plan}
        if command=='submit_plan':
            plan=copy.deepcopy(args['plan'])
            if plan.get('reviewed') is not True: raise PolicyBlocked('先在界面审查并确认具体计划')
            prepared=self.store.get('change_plans',plan.get('plan_id'))
            clean=lambda p:{k:v for k,v in p.items() if k not in ('reviewed','id')}
            if not prepared or clean(prepared)!=clean(plan):raise PolicyBlocked('执行计划与已保存预览不一致，请重新生成并审查')
            if plan.get('config_hash') and plan['config_hash']!=digest(cfg): raise PolicyBlocked('计划配置已改变，请重新预览')
            if plan.get('download_candidate'):
                from musicops.providers.workflow import run_offline_download
                tx_id=str(uuid.uuid5(uuid.NAMESPACE_URL,'musicops:transaction:'+str(plan['plan_id']))); previous=self.store.get('transactions',tx_id)
                if not previous or previous.get('state')!='COMMITTED':
                    target,snapshot_hash=self._fill_preflight(plan['download_candidate'])
                    if snapshot_hash!=plan.get('snapshot_hash') or target['target_id']!=plan.get('target_id'): raise PolicyBlocked('快照或目标身份已改变，请重新比对和预览')
                self._checkpoint('before_download')
                download=run_offline_download(self.store,cfg,plan['download_candidate'],checkpoint=self._checkpoint)
                self._checkpoint('download_finished')
                if download.get('status')!='REQUIRES_VALIDATION': raise PolicyBlocked(download.get('reason',download.get('status','下载流程阻断')))
                source=self._safe_path(download['source_path']); check=self._validator(cfg).validate(source)
                self._checkpoint('download_media_validated')
                if check['decode_state']!='passed': raise RuntimeError('下载来源完整解码未通过')
                plan=copy.deepcopy(plan); plan['operations'][0].update(source=str(source),source_hash=download['source_sha256'])
            for op in plan['operations']:
                if op.get('source'): self._safe_path(op['source'])
            self.store.put('change_plans',plan['plan_id'],{**prepared,'reviewed':True})
            self._checkpoint('before_file_transaction')
            tx=self._engine(cfg).execute(plan); self._post_transaction(cfg); return tx
        if command=='request_rollback':
            tx=self._engine(cfg).rollback(args['tx_id']); self._post_transaction(cfg); return tx
        if command=='recover': return {'transactions':self._engine(cfg).recover(),'unknown_requests':self.store.list('token_reservations')}
        if command=='export_report':
            dest=self._safe_path(args.get('path',str(Path(paths['reports'])/('report-'+dt.datetime.now().strftime('%Y%m%d-%H%M%S')+'.json')))); dest.parent.mkdir(parents=True,exist_ok=True)
            decisions=self.store.list('match_decisions'); counts=dict(collections.Counter(r['main_state'] for r in decisions if r.get('main_state')))
            targets=self.store.list('target_tracks'); transactions=self.store.list('transactions'); ops=[op for t in transactions if t['state']=='COMMITTED' for op in t['operations']]
            target_releases={(t.get('album'),t.get('edition','')) for t in targets}; albums={f.get('album') for f in self.store.list('library_files') if f.get('location_state')=='library'}
            confirmed_ids={d['target_id'] for d in decisions if d.get('main_state')=='verified_present'}
            release_confirmed=sum(all(t['target_id'] in confirmed_ids for t in targets if (t.get('album'),t.get('edition',''))==r) for r in target_releases)
            report=dict(schema_version=1,generated_at=utcnow(),timezone='Asia/Shanghai',evidence_level='offline_fixture',config_version=self.config.current()['version'],input_config_hash=digest(cfg),snapshot_hashes=[s.get('hash') for s in self.store.list('playlist_snapshots')],target_total=len(targets),match_counts=counts,
                coverage=dict(track={'numerator':len(confirmed_ids),'denominator':len(targets)},album_appearance={'numerator':sum(r[0] in albums for r in target_releases),'denominator':len(target_releases)},release_identity={'numerator':release_confirmed,'denominator':len(target_releases)},roon={'numerator':0,'denominator':len(targets)}),
                changes={'scope':'当前仍提交的事务文件数（包含伴随资源）','new':sum(o['kind']=='publish' for o in ops),'replace':sum(o['kind']=='replace' for o in ops),'quarantine':sum(o['kind']=='quarantine' for o in ops),'untouched_library_files':sum(f.get('location_state')=='library' and f['path'] not in {o['destination'] for o in ops} for f in self.store.list('library_files'))},
                transactions=transactions,token_budget=cfg['token_budget'],token_observations=self.store.list('token_reservations'),roon_state='pending',
                unresolved=[{'target_id':d['target_id'],'state':d['main_state'],'reason':d.get('reason'),'next_action':'复核身份或按现场计划确认服务/资源'} for d in decisions if d.get('main_state')!='verified_present']+[{'state':'现场未验收','reason':'真实服务、真实文件与Roon均未验收','next_action':'按现场预检与单张试点计划推进'}],events=self.store.events())
            dest.write_text(json.dumps(redact(report),ensure_ascii=False,indent=2),encoding='utf-8'); return {'path':str(dest),'report':report}
        raise ValueError(command)
    def _search(self,args,cfg):
        if not cfg['offline']:
            outcomes={};candidates=[]
            for site in ('dicmusic','orpheus','open.cd','redacted'):
                if not cfg['providers'][site]['enabled']:outcomes[site]={'status':'DISABLED'};continue
                self._checkpoint('search_'+site)
                outcome=self._real_pt(site,cfg).search(args.get('query',''));outcomes[site]={k:v for k,v in outcome.items() if k!='candidates'};outcomes[site]['candidate_count']=len(outcome.get('candidates',[]))
                for candidate in outcome.get('candidates',[]):self.store.put('candidates',candidate['candidate_id'],candidate);candidates.append(candidate)
            outcomes['netease']={'status':'POLICY_BLOCKED','reason':'真实网易云歌单/检索契约未验证；不混入模拟结果'}
            status='OK' if candidates else 'NO_RESOURCE' if all(o['status'] in ('NO_RESOURCE','DISABLED') for s,o in outcomes.items() if s!='netease') and any(o['status']=='NO_RESOURCE' for o in outcomes.values()) else 'ACCESS_BLOCKED'
            return {'status':status,'candidates':candidates,'source_outcomes':outcomes,'read_only':True,'search_complete':False,'download_status':'POLICY_BLOCKED'}
        from musicops.providers.workflow import offline_search
        return offline_search(self.store,cfg,args.get('query',''))
    def _catalog_resolver(self,cfg):
        from musicops.providers.catalog import CatalogResolver
        key=digest(cfg['catalog'])
        if not hasattr(self,'_catalog_clients'):self._catalog_clients={}
        if key not in self._catalog_clients:
            if len(self._catalog_clients)>=4:self._catalog_clients.pop(next(iter(self._catalog_clients)))
            self._catalog_clients[key]=CatalogResolver(cfg['catalog'],secret_getter=self.vault.get,checkpoint=self._checkpoint)
        return self._catalog_clients[key]
    def _catalog_command(self,command,args,cfg):
        resolver=self._catalog_resolver(cfg)
        if command=='identify_metadata':
            query={k:args.get(k) for k in ('title','artists','album','duration','target_id') if k in args}
            if 'artists' not in query:query['artists']=args.get('artist',[])
            result=resolver.identify(query)
            target=self.store.get('target_tracks',str(query.get('target_id',''))) or self.store.get('library_files',str(query.get('target_id','')))
            for candidate in result.get('candidates',[]):
                self.store.put('evidence','catalog:'+candidate['candidate_id'],{'kind':'catalog_candidate','candidate':candidate,'query':query,'target_hash':digest(target) if target else None,'catalog_config_hash':digest(cfg['catalog']),'observed_at':utcnow()})
            return result
        candidate_id=args.get('candidate_id');evidence=self.store.get('evidence','catalog:'+str(candidate_id))
        if not evidence:raise PolicyBlocked('候选没有已保存的在线检索证据，请先查询')
        if evidence['catalog_config_hash']!=digest(cfg['catalog']):raise PolicyBlocked('识别配置已改变，请重新查询')
        original=evidence['candidate']
        if command=='resolve_metadata':
            result=resolver.resolve_candidate(original['source'],original['source_id'])
            if result.get('candidate'):
                detail=result['candidate'];detail['candidate_id']=candidate_id
                detail['query_context']=evidence['query'];detail['catalog_resolved']=True
                if original.get('recording_id'):detail['selected_recording_id']=original['recording_id']
                evidence.update(candidate=detail,search_candidate=original,resolved_at=utcnow())
                self.store.put('evidence','catalog:'+candidate_id,evidence);result['candidate']=detail
            return result
        candidate=evidence['candidate'];target_id=args.get('target_id')
        if not candidate.get('catalog_resolved'):raise PolicyBlocked('请先读取所选候选的完整发行证据')
        if not target_id or evidence['query'].get('target_id') not in (None,'',target_id):raise PolicyBlocked('确认目标与查询目标不一致')
        target=self.store.get('target_tracks',target_id);file=self.store.get('library_files',target_id) if target is None else None
        current=target or file
        if not current:raise PolicyBlocked('目标不存在；请选中歌单曲目或索引文件')
        if evidence.get('target_hash') and digest(current)!=evidence['target_hash']:raise PolicyBlocked('目标资料在查询后发生变化，请重新查询')
        from musicops.domain.identity import search_text
        title=current.get('raw_title') or current.get('title') or evidence['query'].get('title','')
        tracks=candidate.get('tracklist',[])
        selected_id=candidate.get('selected_recording_id') or candidate.get('recording_id')
        matching=[t for t in tracks if selected_id and t.get('recording_id')==selected_id] or [t for t in tracks if search_text(t.get('title',''))==search_text(title)]
        if tracks:
            if not candidate.get('tracklist_complete') or len(matching)!=1:raise PolicyBlocked('曲目表不完整或无法选出唯一录音，请复核后重新查询')
            recording=matching[0]
        elif candidate.get('entity_kind')=='recording':recording=candidate
        else:raise PolicyBlocked('仅专辑名称不能确认逐曲身份，请读取完整曲目表')
        if search_text(recording.get('title',''))!=search_text(title):raise PolicyBlocked('候选标题与目标录音不一致')
        expected=current.get('artists') or evidence['query'].get('artists',[])
        if {search_text(a) for a in expected}!={search_text(a) for a in recording.get('artists',[])}:raise PolicyBlocked('候选艺人与目标不一致')
        album=candidate.get('album') or candidate.get('title') if tracks else candidate.get('album')
        if not album:raise PolicyBlocked('候选缺发行信息，不能确认专辑')
        proposal={'raw_title':recording['title'],'artists':recording['artists'],'album':album,'duration':recording.get('duration') or current.get('duration'),'recording_id':recording.get('recording_id') or selected_id,'release_id':candidate.get('release_id'),'source':candidate['source'],'source_id':candidate['source_id'],'url':candidate.get('url'),'candidate_id':candidate_id,'reviewed_by':'current_user','reviewed_at':utcnow()}
        if file:
            file['metadata_proposal']=proposal;self.store.put('library_files',target_id,file)
            result={'file':file,'proposal':proposal,'metadata_only':True,'music_writes':0,'reason':'发行证据已保存为人工确认提案，原始音频标签未修改'}
        else:
            target.setdefault('source_metadata',{k:target.get(k) for k in ('raw_title','artists','album','duration','source_album_id','metadata_complete')})
            target.update(album=album,metadata_complete=True,metadata_resolution=proposal,metadata_resolution_state='user_accepted')
            self.store.put('target_tracks',target_id,target)
            result={'target':target,'proposal':proposal,'music_writes':0,'source_snapshot_complete':target.get('snapshot_complete',False),'reason':'目标专辑采用人工确认外部证据；原始来源快照完整性保持独立'}
        self.store.event('catalog_manual_accept',{'target_id':target_id,'candidate_id':candidate_id,'source':candidate['source'],'music_writes':0})
        return result
    def _index_real_library(self,args,cfg,progress=None):
        from musicops.library.index import index_library
        from musicops.library.paths import reject_reparse_chain
        root=Path(args.get('root') or cfg['paths']['library']).absolute();reject_reparse_chain(root)
        if not args.get('lightweight',True):raise PolicyBlocked('请选择轻量索引；完整解码仅针对明确选中的文件')
        exclusions=[cfg['paths'][key] for key in ('source','work','backup','quarantine') if Path(cfg['paths'][key]).resolve().is_relative_to(root.resolve())]
        self._checkpoint('before_metadata_index')
        with self.store.connection() as c:previous=[json.loads(r[0]) for r in c.execute('SELECT payload FROM library_files')]
        root_resolved=root.resolve();cached={f['file_id']:f for f in previous if f.get('file_id') and Path(f.get('path','')).is_relative_to(root)}
        files=index_library(root,metadata_only=True,exclusions=exclusions,checkpoint=lambda:self._checkpoint('metadata_index'),cached=cached,progress=progress)
        self._checkpoint('metadata_index_read_finished')
        with self.store.connection() as c:
            c.execute('BEGIN IMMEDIATE')
            for item in files:
                item['library_root']=str(root)
                old=cached.get(item['file_id'])
                if old and old.get('size')==item.get('size') and old.get('mtime_ns')==item.get('mtime_ns') and old.get('validation',{}).get('decode_state')=='passed':
                    item['validation']=old['validation'];item['validation_id']=old.get('validation_id');item['sha256']=old.get('sha256')
                item.setdefault('id',item['file_id']);c.execute('INSERT INTO library_files VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at',(item['file_id'],json.dumps(item,ensure_ascii=False),utcnow()))
            live={item['file_id'] for item in files}
            for row in c.execute('SELECT id,payload FROM library_files').fetchall():
                old=json.loads(row['payload'])
                if row['id'] not in live and Path(old.get('path','')).resolve().is_relative_to(root_resolved) and not Path(old['path']).exists():
                    old.update(location_state='externally_missing',read_error='文件已移出索引目录');c.execute('UPDATE library_files SET payload=?,updated_at=? WHERE id=?',(json.dumps(old,ensure_ascii=False),utcnow(),row['id']))
        return {'files':files[:200],'count':len(files),'truncated':len(files)>200,'root':str(root),'metadata_only':True,'format_counts':dict(collections.Counter(f.get('media_info',{}).get('codec','unknown') for f in files)),'music_writes':0}
    def _compare_targets(self,args):
        from musicops.domain.identity import match_track,search_text
        import re
        targets=args.get('targets') or self.store.list('target_tracks');files=self.store.list('library_files');results=[]
        by_title=collections.defaultdict(list);by_source=collections.defaultdict(list)
        for file in files:
            title=file.get('raw_title') or file.get('title','')
            for key in {search_text(title),search_text(re.sub(r'\([^)]*\)|\[[^]]*\]','',title))}:
                if key:by_title[key].append(file)
            if file.get('source_song_id'):by_source[(file.get('source','netease'),str(file['source_song_id']))].append(file)
        for target in targets:
            self._checkpoint('compare_metadata')
            title=target.get('raw_title') or target.get('title','');subset={}
            for key in {search_text(title),search_text(re.sub(r'\([^)]*\)|\[[^]]*\]','',title))}:
                for file in by_title.get(key,[]):subset[file['file_id']]=file
            for file in by_source.get((target.get('source','netease'),str(target.get('source_song_id'))),[]):subset[file['file_id']]=file
            decision=match_track(target,list(subset.values()));self.store.put('match_decisions',target['target_id'],decision);results.append(decision)
        return {'decisions':results,'total':len(results),'counts':dict(collections.Counter(r['main_state'] for r in results)),'roon_state':'pending','metadata_only':True,'music_writes':0}
    def _real_pt(self,site,cfg):
        if not cfg['providers'][site]['enabled']:raise PolicyBlocked('该PT来源已停用；启用并保存配置后再验证')
        from musicops.providers.pt_readonly import PTReadOnlyProvider,HOSTS
        from musicops.clients.http import SafeHttpTransport
        return PTReadOnlyProvider(site,cfg['providers'][site],SafeHttpTransport([HOSTS[site]],offline=False),self.vault.get)
    def _refresh_real_playlists(self,args,cfg):
        from musicops.app.playlists import use_platform_reader,refresh_platform_playlists
        if use_platform_reader(args):return refresh_platform_playlists(self,args,cfg)
        if not cfg['providers']['netease']['enabled']:raise PolicyBlocked('网易云来源已停用；启用并保存配置后再刷新')
        from musicops.providers.netease_readonly import NetEaseReadOnlyProvider
        from musicops.clients.http import SafeHttpTransport
        ids=([args['playlist_id']] if args.get('playlist_id') else args.get('playlist_ids') or cfg['playlist_ids'])
        if args.get('playlist_input'):
            from musicops.providers.netease import parse_playlist_input
            parsed=parse_playlist_input(args['playlist_input'])
            if parsed['status']!='OK':raise ValueError('请输入有效的网易云歌单 ID 或链接')
            ids=parsed['playlist_ids']
        if not ids:raise PolicyBlocked('请先保存真实歌单ID或链接')
        provider=NetEaseReadOnlyProvider(SafeHttpTransport(['music.163.com'],offline=False),cfg['providers']['netease'],self.vault.get,self._checkpoint)
        summaries=[];targets={};all_complete=True;complete_ids=set()
        for pid in ids:
            outcome=provider.refresh(pid,batch_size=cfg['netease']['batch_size']);snapshot=outcome.get('snapshot')
            if not snapshot:
                summaries.append(outcome);all_complete=False;continue
            complete=snapshot['complete'];all_complete=all_complete and complete;sid=snapshot['snapshot_id']
            if complete:complete_ids.add(str(pid))
            if complete:self.store.put('playlist_snapshots',sid,snapshot)
            else:self.store.put('evidence','incomplete:'+sid,{'kind':'incomplete_live_snapshot','snapshot':snapshot})
            summaries.append({k:v for k,v in snapshot.items() if k not in ('songs','batches','ordered_ids','unique_ids','requested_ids','returned_ids')})
            summaries[-1].update(status=outcome['status'],song_count=len(snapshot['songs']))
            for song in snapshot['songs']:
                target=copy.deepcopy(song);target.update(snapshot_id=sid,snapshot_ids=[sid],snapshot_complete=complete,playlist_ids=[str(pid)])
                if target['target_id'] in targets:
                    old=targets[target['target_id']];target['snapshot_ids']=old['snapshot_ids']+[sid];target['playlist_ids']=old['playlist_ids']+[str(pid)];target['snapshot_complete']=complete and old['snapshot_complete']
                targets[target['target_id']]=target
        membership=self._persist_playlist_targets(targets,complete_ids)
        return {'status':'OK' if all_complete else 'SNAPSHOT_INCOMPLETE','complete':all_complete,'snapshots':summaries,'targets':list(targets.values()),**membership,'read_only':True,'reason':'' if all_complete else '可见曲目已逐曲取详情；清单计数或详情存在未对账项，不能驱动下载'}
    def _persist_playlist_targets(self,targets,complete_ids):
        # Reconcile only fully refreshed playlists. Unrefreshed memberships and
        # previous complete evidence survive an incomplete/network-failed refresh.
        existing={t['target_id']:t for t in self.store.list('target_tracks')}
        reconciled={};removed=set()
        for tid,prior in existing.items():
            current=copy.deepcopy(prior);pids=prior.get('playlist_ids',[]);sids=prior.get('snapshot_ids',[])
            if set(pids)&complete_ids:
                pairs=[(pid,sids[i] if i<len(sids) else prior.get('snapshot_id')) for i,pid in enumerate(pids) if pid not in complete_ids]
                current['playlist_ids']=[p for p,s in pairs];current['snapshot_ids']=[s for p,s in pairs if s]
                current['snapshot_id']=next(iter(current['snapshot_ids']),None)
                if not current['playlist_ids']:removed.add(tid);continue
            reconciled[tid]=current
        retained=0
        for tid,target in targets.items():
            prior=existing.get(tid)
            preserve=bool(prior and prior.get('snapshot_complete') and not target['snapshot_complete'])
            if preserve:retained+=1
            fresh=copy.deepcopy(prior if preserve else target);remaining=reconciled.get(tid)
            def pairs(row):
                return {pid:row.get('snapshot_ids',[])[i] if i<len(row.get('snapshot_ids',[])) else row.get('snapshot_id') for i,pid in enumerate(row.get('playlist_ids',[]))}
            members=pairs(remaining) if remaining else {};previous=pairs(prior) if prior else {}
            for pid,sid in pairs(target).items():
                members[pid]=previous[pid] if preserve and pid in previous and pid not in complete_ids else sid
            fresh['playlist_ids']=list(members);fresh['snapshot_ids']=list(members.values());fresh['snapshot_id']=next(iter(members.values()),None)
            reconciled[tid]=fresh;removed.discard(tid)
        with self.store.connection() as c:complete_by_sid={r[0]:bool(r[1]) for r in c.execute("SELECT id,json_extract(payload,'$.complete') FROM playlist_snapshots")}
        fallback={}
        for row in list(existing.values())+list(targets.values()):
            for sid in row.get('snapshot_ids',[]):fallback[sid]=fallback.get(sid,False) or row.get('snapshot_complete',False)
        for row in reconciled.values():
            if row.get('playlist_ids'):row['snapshot_complete']=all(complete_by_sid.get(sid,fallback.get(sid,False)) for sid in row.get('snapshot_ids',[]))
            prior=existing.get(row['target_id'])
            if prior and prior.get('metadata_resolution_state')=='user_accepted' and prior.get('metadata_resolution'):
                origin=prior.get('source_metadata',{})
                if all(row.get(k)==origin.get(k) for k in ('raw_title','artists','source_album_id')):
                    row.update(source_metadata=copy.deepcopy(origin),metadata_resolution=copy.deepcopy(prior['metadata_resolution']),metadata_resolution_state='user_accepted',album=prior['metadata_resolution']['album'],metadata_complete=True)
                else:row.update(metadata_resolution=copy.deepcopy(prior['metadata_resolution']),metadata_resolution_state='needs_review_source_changed')
        with self.store.connection() as c:
            c.execute('BEGIN IMMEDIATE')
            for tid in removed:
                c.execute('DELETE FROM target_tracks WHERE id=?',(tid,));c.execute('DELETE FROM match_decisions WHERE id=?',(tid,))
            for tid,target in reconciled.items():
                if target==existing.get(tid):continue
                target.setdefault('schema_version',1);target.setdefault('id',tid)
                c.execute('INSERT INTO target_tracks VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at',(tid,json.dumps(target,ensure_ascii=False),utcnow()))
        return {'retained_complete_targets':retained,'removed_targets':len(removed),'stored_target_count':len(reconciled)}
    def _real_resources(self,args,cfg,private_only=False):
        from musicops.providers.lrcapi import LrcApiClient
        from musicops.clients.http import SafeHttpTransport
        from urllib.parse import urlsplit
        settings=copy.deepcopy(cfg['lyrics']);settings['public_fallback'].setdefault('enabled',False)
        if private_only:settings['public_fallback']['enabled']=False;settings['direct_backup']['enabled']=False
        if settings['direct_backup'].get('enabled'):raise PolicyBlocked('真实只读资源阶段只验证HTTPS主服务，直连备用仍待独立确认')
        host=urlsplit(settings['primary']['base_url']).hostname
        public_hosts={urlsplit(settings['public_fallback'][k]).hostname for k in ('lyrics_url','cover_url')}
        if not hasattr(self,'_lrc_clients'):self._lrc_clients={}
        key=digest(settings)
        if key not in self._lrc_clients:
            if len(self._lrc_clients)>=4:self._lrc_clients.pop(next(iter(self._lrc_clients)))
            self._lrc_clients[key]=LrcApiClient(settings,{'private':SafeHttpTransport([host],offline=False),'public':SafeHttpTransport(public_hosts,offline=not settings['public_fallback']['enabled'])},self.vault.get)
        client=self._lrc_clients[key];title=args.get('title','');artist=args.get('artist','');album=args.get('album','');query=dict(duration=args.get('duration'),refresh=args.get('refresh',False))
        self._checkpoint('before_real_lyrics');lyrics=client.candidates(title,artist,album,**query)
        self._checkpoint('before_real_cover');cover=client.cover(title,artist,album,**query)
        candidates=[];cover_preview=None
        for candidate in lyrics.get('candidates',[])+cover.get('candidates',[]):
            candidate=copy.deepcopy(candidate);content=candidate.pop('content_bytes',None)
            if content is not None:
                path=self._safe_path(self.base/'previews'/(hashlib.sha256(content).hexdigest()+'.jpg'));path.parent.mkdir(parents=True,exist_ok=True)
                if not path.exists():path.write_bytes(content)
                candidate['path']=str(path);cover_preview=str(path)
            candidate['evidence_level']='live_readonly';candidate['query_context']={'title':title,'artist':artist,'album':album,'duration':args.get('duration')};self.store.put('resource_candidates',candidate['resource_id'],candidate);candidates.append(candidate)
        errors=lyrics.get('errors',[])+cover.get('errors',[])
        states=[lyrics['state'],cover['state']]
        state='OK' if all(s=='VALID_PAYLOAD' for s in states) else 'PARTIAL' if 'VALID_PAYLOAD' in states else states[0]
        return {'status':state,'lyrics':{k:v for k,v in lyrics.items() if k!='candidates'},'cover':{k:v for k,v in cover.items() if k!='candidates'},'candidates':candidates,'cover_preview':cover_preview,
                'lrc_text':next((c.get('lyrics','') for c in candidates if c.get('kind')=='lyrics' and c.get('selected')),''),'errors':errors,'read_only':True,'music_writes':0}
    def _post_transaction(self,cfg):
        self._safe_finalizing=True
        try:self._execute('index_library',{},cfg); self._execute('compare',{},cfg)
        finally:self._safe_finalizing=False
    def _fill_preflight(self,candidate):
        from musicops.domain.identity import match_track,recording_identity
        authoritative=self.store.get('candidates',candidate['candidate_id'])
        clean=lambda obj:{k:v for k,v in obj.items() if k!='id'}
        if not authoritative or clean(candidate)!=clean(authoritative): raise PolicyBlocked('候选与检索证据不一致，请重新搜索并选定唯一候选')
        target=self.store.get('target_tracks','netease:'+str(candidate['resource_id']))
        if not target or target.get('evidence_level')!='synthetic_only' or target.get('snapshot_complete') is not True: raise PolicyBlocked('目标快照不完整或不属于合成样本；不创建下载请求')
        release=candidate.get('release',{}); candidate_identity={**candidate,**release}
        recording=recording_identity(target); declaration=recording_identity(candidate_identity)
        if any(recording[k]!=declaration[k] for k in ('title','artist_identity','recording_version')) or target.get('release_id')!=release.get('release_id') or target.get('album')!=release.get('album') or target.get('edition')!=release.get('edition'): raise PolicyBlocked('下载候选录音/发行身份不符合目标，不自动替代')
        snapshots=[self.store.get('playlist_snapshots',sid) for sid in target.get('snapshot_ids',[target.get('snapshot_id')])]
        for snapshot in snapshots:
            if not snapshot or not snapshot.get('complete') or snapshot.get('evidence_level')!='synthetic_only': raise PolicyBlocked('缺完整来源快照证据')
            observed=dt.datetime.fromisoformat(snapshot['fetched_at']); age=(dt.datetime.now(dt.timezone.utc)-observed).total_seconds()
            if age<0 or age>86400: raise PolicyBlocked('快照过期，请刷新后重新比对')
            if str(candidate['resource_id']) not in snapshot['unique_ids']: raise PolicyBlocked('目标不在完整快照ID集合')
        if match_track(target,self.store.list('library_files'))['main_state']!='missing': raise PolicyBlocked('仅明确缺失目标进入补齐；冲突、模糊或已存在均须复核')
        return target,digest([s['hash'] for s in snapshots])
    def _demo(self,cfg):
        from PIL import Image,ImageDraw
        from mutagen.flac import FLAC
        import subprocess,math,struct
        for key in ('library','source','work','backup','quarantine','reports'): self._safe_path(cfg['paths'][key]).mkdir(parents=True,exist_ok=True)
        source=Path(cfg['paths']['source']); library=Path(cfg['paths']['library'])/'示例艺人'/'离线演示专辑'; library.mkdir(parents=True,exist_ok=True)
        for number,title in [(1,'晨光'),(2,'星河')]:
            out=source/f'{number:02} {title}.flac'
            if not out.exists():
                wav=source/f'{number:02}.wav'
                with wave.open(str(wav),'wb') as f:
                    f.setparams((1,2,44100,0,'NONE','not compressed'))
                    f.writeframes(b''.join(struct.pack('<h',int(5000*math.sin(2*math.pi*(300+number*100)*n/44100))) for n in range(88200)))
                proc=subprocess.run([cfg['paths']['tools']['ffmpeg'],'-v','error','-i',str(wav),'-c:a','flac',str(out)],capture_output=True,creationflags=0x08000000 if os.name=='nt' else 0)
                if proc.returncode: raise RuntimeError('合成媒体生成失败')
                tags=FLAC(out); tags['title']=[title]; tags['artist']=['示例艺人','合作艺人']; tags['albumartist']=['示例艺人']; tags['album']=['离线演示专辑']; tags['tracknumber']=[str(number)]; tags['discnumber']=['1']; tags['recording_version']=['original']; tags['edition']=['original']; tags['release_id']=['demo-album']; tags['musicops_private']=['保留未知标签']; tags.save()
            target=dict(schema_version=1,target_id='netease:'+str(number),source_song_id=str(number),raw_title=title,artists=['示例艺人','合作艺人'],source_album_id='demo-album',album='离线演示专辑',version='original',duration=2,recording_version='original',release_id='demo-album',edition='original',date_raw='2026',snapshot_id='demo',evidence_level='synthetic_only')
            self.store.put('target_tracks',target['target_id'],target)
        original=source/'01 晨光.flac'; dest=library/original.name
        if not dest.exists(): shutil.copy2(original,dest)
        duplicate_dir=library/'重复副本'; duplicate_dir.mkdir(exist_ok=True)
        dup=duplicate_dir/original.name
        if not dup.exists(): shutil.copy2(original,dup)
        cover=source/'cover.jpg'
        if not cover.exists():
            img=Image.new('RGB',(600,600),'#19334A'); ImageDraw.Draw(img).text((70,280),'MusicOps Offline Demo',fill='white'); img.save(cover,'JPEG')
        lrc=source/'01 晨光.lrc'; lrc.write_text('[00:00.00]晨光\n[00:00.50]离线合成歌词\n',encoding='utf-8')
        shutil.copy2(cover,library/'cover.jpg'); shutil.copy2(lrc,library/lrc.name)
        shutil.copy2(cover,duplicate_dir/'cover.jpg'); shutil.copy2(lrc,duplicate_dir/lrc.name)
        self.store.event('demo_generated',{'evidence':'synthetic','source':str(source)})
        self._execute('refresh_playlist',{'playlist_ids':[pid for pid in cfg['playlist_ids'] if pid in ('10001','10002')] or ['10001']},cfg)
        files_result=self._execute('index_library',{},cfg)
        # Add source release evidence to synthetic files, preserving indexed raw tags.
        for f in files_result['files']:
            f.update(release_id='demo-album',edition='original',version='original'); self.store.put('library_files',f['file_id'],f)
        compared=self._execute('compare',{},cfg)
        return {'paths':cfg['paths'],'files':self.store.list('library_files'),'targets':self.store.list('target_tracks'),'comparison':compared,'evidence':'synthetic_only','next':'搜索候选→选唯一候选；修复字段→预览→事务；重复件→隔离计划'}
    def next_schedule(self):
        s=self.config.current()['values']['scheduler']
        if not s['enabled']: return {'enabled':False,'registered':False,'timezone':'Asia/Shanghai'}
        zone=dt.timezone(dt.timedelta(hours=8)); now=dt.datetime.now(zone); hour,minute=map(int,s['playlist_time'].split(':')); nxt=now.replace(hour=hour,minute=minute,second=0,microsecond=0)
        if nxt<=now: nxt+=dt.timedelta(days=1)
        return {'enabled':True,'registered':False,'next_report':nxt.isoformat(),'mode':'report','reason':'生成调度计划；当前阶段不注册Windows计划任务'}
