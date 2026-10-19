"""Real macOS runner acceptance of the frozen .app and worker.

Creates a new profile and an empty synthetic library. No site requests, audio
downloads, FL use or writes to an existing library are part of this check.
"""
from __future__ import annotations
import argparse,hashlib,json,os,platform,plistlib,subprocess,sys,time,uuid
from pathlib import Path

REQUIRED_CHECKS=frozenset({
    'native_architecture','bundle_identity_version_icon','gui_started_frozen_worker',
    'concurrent_gui_polling_stays_connected','keychain_encrypted_vault_write',
    'keychain_cross_process_roundtrip','vault_has_no_plaintext_secret','vault_host_binding',
    'complete_immediately_visible','complete_preparation_completed','upgrade_immediately_visible',
    'upgrade_preparation_completed','playlist_immediately_visible','playlist_preparation_completed',
    'three_preparation_records','identification_and_search_are_separate','native_bulk_cancel_review_tasks',
    'native_bulk_retry_search','native_review_snapshot','native_local_album_database',
    'native_ai_assistance_guard','native_delete_records_only','native_update_setting','native_browser_solver_guard',
    'native_managed_engine_cookie','native_managed_solver','native_durable_ai_registration',
    'native_task_status_filter','native_full_queue_selection','native_streaming_and_edition_settings','native_full_album_preparation',
    'native_multiplatform_file_import','native_manual_search_readonly','native_search_activity_visible',
    'configuration_roundtrip','native_cocoa_window_exit_zero','native_window_rendered',
    'frozen_worker_restart_keeps_credentials','configuration_persisted_after_restart',
    'offscreen_gui_rendered','worker_clean_shutdown','app_bundle_unchanged_after_use','no_profile_inside_app',
})

def valid_acceptance(result):
    """Require named runtime evidence, allowing additional successful checks."""
    checks=result.get('checks',{})
    return (result.get('passed') is True and result.get('external_requests')==0
            and isinstance(checks,dict) and REQUIRED_CHECKS.issubset(checks)
            and all(value is True for value in checks.values()))

def wait_for(action,timeout=25):
    deadline=time.monotonic()+timeout
    while True:
        try:
            result=action()
            if result:return result
        except (OSError,EOFError,ConnectionError):pass
        if time.monotonic()>=deadline:raise TimeoutError('packaged macOS operation did not finish')
        time.sleep(.15)

def digest_tree(root):
    result={}
    for path in root.rglob('*'):
        if path.is_file() and not path.is_symlink():
            with path.open('rb') as stream:result[str(path.relative_to(root))]=hashlib.file_digest(stream,'sha256').hexdigest()
    return result

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--app',required=True);parser.add_argument('--work',required=True);parser.add_argument('--report',required=True);parser.add_argument('--expected-arch',choices=('arm64',),required=True);args=parser.parse_args()
    if sys.platform!='darwin':raise SystemExit('Run this acceptance test on a real macOS runner')
    from musicops.config.schema import ConfigManager
    from musicops.state.store import StateStore
    from musicops.clients.vault import Vault
    from musicops.worker.client import WorkerClient
    from musicops.worker.lock import WriterLock
    from musicops import __version__
    app=Path(args.app).resolve();work=Path(args.work).resolve();work.mkdir(parents=True,exist_ok=True)
    base=work/('new profile '+uuid.uuid4().hex[:8]);base.mkdir()
    report_path=Path(args.report).resolve();report_path.parent.mkdir(parents=True,exist_ok=True)
    gui=app/'Contents/MacOS/MusicOps';worker=app/'Contents/MacOS/MusicOps-worker';before=digest_tree(app)
    report={'version':__version__,'platform':'macOS','architecture':platform.machine(),'runner_macos':platform.mac_ver()[0],'evidence':'native_packaged_app_worker_keychain_three_entries_file_import_manual_search','external_requests':0,'developer_id_signed':False,'notarized':False,'checks':{}}
    checks=report['checks'];client=WorkerClient(base,timeout=20);processes=[];handles=[];sensitive_values=[]
    env=os.environ.copy()
    for key in ('PYTHONPATH','PYTHONHOME','VIRTUAL_ENV','QT_PLUGIN_PATH','QT_QPA_PLATFORM_PLUGIN_PATH'):env.pop(key,None)
    env['QT_QPA_PLATFORM']='cocoa';env['PYTHONIOENCODING']='utf-8'
    def launch(executable,*arguments,platform_name='cocoa'):
        child_env=env.copy();child_env['QT_QPA_PLATFORM']=platform_name
        handle=(work/(executable.name+'-'+str(len(processes))+'.log')).open('wb');handles.append(handle)
        process=subprocess.Popen([str(executable),*map(str,arguments)],cwd=work,env=child_env,stdin=subprocess.DEVNULL,stdout=handle,stderr=subprocess.STDOUT);processes.append(process);return process
    phase='profile_setup'
    try:
        checks['native_architecture']=platform.machine()==args.expected_arch
        info=plistlib.loads((app/'Contents/Info.plist').read_bytes());checks['bundle_identity_version_icon']=info.get('CFBundleIdentifier')=='com.wsdbb72.MusicOps' and info.get('CFBundleShortVersionString')==__version__ and bool(info.get('CFBundleIconFile'))
        store=StateStore(base/'state/musicops.sqlite');manager=ConfigManager(store,base,initial_offline=False);current=manager.current();values=current['values']
        library=Path(values['paths']['library']);library.mkdir(parents=True,exist_ok=True);values['workflow'].update(setup_done=True,netease_fallback=False)
        values['catalog']['enabled']=False;values['ai']['enabled']=False
        for provider in values['providers'].values():provider['enabled']=False
        values['music_servers']['roon']['enabled']=False;values['music_servers']['navidrome']['enabled']=False;manager.save(values,current['version'])
        store.put('target_tracks','macos:fixture',{'target_id':'macos:fixture','source':'local','raw_title':'Synthetic Song','artists':['Synthetic Artist'],'album':'Synthetic Album','duration':120,'metadata_complete':True})
        phase='native_cocoa_gui';screenshot=work/'native-window.png';process=launch(gui,'--base',base,'--screenshot',screenshot,'--quit-after','14000')
        status=wait_for(lambda:client.call('get_status',{'compact':True}));checks['gui_started_frozen_worker']=Path(status['base'])==base and status['offline'] is False
        phase='concurrent_gui_polling'
        from concurrent.futures import ThreadPoolExecutor
        from multiprocessing.connection import Client
        from musicops.worker.client import pipe_address,auth_key
        from musicops.platforms import ipc_family
        with Client(pipe_address(base),family=ipc_family(),authkey=auth_key(base)) as idle, ThreadPoolExecutor(max_workers=8) as pool:
            replies=list(pool.map(lambda _:WorkerClient(base,timeout=5).call('get_status',{'compact':True}),range(16)))
        checks['concurrent_gui_polling_stays_connected']=len(replies)==16 and all(Path(reply['base'])==base for reply in replies)
        # The worker receives a new token over authenticated local IPC and writes
        # encrypted data. No token value is printed or added to the report.
        phase='keychain_vault_roundtrip';secret='synthetic-macos-'+uuid.uuid4().hex;ref='vault://smoke/macos/token';host='smoke.example.invalid';sensitive_values.append(secret)
        saved=client.call('set_secret',{'ref':ref,'value':secret,'host':host});checks['keychain_encrypted_vault_write']=saved.get('configured') is True
        vault=Vault(base);checks['keychain_cross_process_roundtrip']=vault.get(ref,host)==secret
        checks['vault_has_no_plaintext_secret']=secret.encode() not in vault.path.read_bytes()
        try:vault.get(ref,'different.example.invalid');checks['vault_host_binding']=False
        except RuntimeError:checks['vault_host_binding']=True
        for scene in ('complete','upgrade','playlist'):
            phase='workflow_'+scene
            started=time.monotonic();accepted=client.call('start_workflow',{'scene':scene,'target_ids':['macos:fixture'] if scene=='playlist' else []})
            visible=client.call('list_workflows')['jobs'];checks[scene+'_immediately_visible']=time.monotonic()-started<5 and any(job['job_id']==accepted['job_id'] and job.get('job_kind')=='batch' for job in visible)
            def finished():
                task=client.call('get_task',{'task_id':accepted['task_id']})['task'];return task if task['state'] not in ('queued','running','paused') else None
            task=wait_for(finished,20);checks[scene+'_preparation_completed']=task['state']=='completed'
            wait_for(lambda:client.call('list_workflows')['active']==0,20)
        checks['three_preparation_records']=len([job for job in client.call('list_workflows')['jobs'] if job.get('job_kind')=='batch'])==3
        workflow_rows=client.call('list_workflows')['jobs']
        checks['identification_and_search_are_separate']=any(job.get('task_type')=='identification' and job['stage']=='completed' and job.get('search_job_id') for job in workflow_rows) and any(job.get('task_type')=='search' and job.get('identification_job_id') for job in workflow_rows)
        review_ids=[job['job_id'] for job in workflow_rows if job.get('task_type')=='search' and job['stage']=='needs_review']
        retried=client.call('bulk_workflows',{'action':'retry','job_ids':review_ids})
        checks['native_bulk_retry_search']=bool(review_ids) and retried['succeeded']==len(review_ids) and all(client.call('get_workflow',{'job_id':jid})['job']['job_id']==jid for jid in review_ids)
        snapshot=client.call('get_workflow',{'job_id':review_ids[0]})['job']
        checks['native_review_snapshot']=bool(snapshot.get('review_revision')) and bool(snapshot.get('prepared'))
        cancelled=client.call('bulk_workflows',{'action':'cancel','job_ids':review_ids})
        checks['native_bulk_cancel_review_tasks']=bool(review_ids) and cancelled['succeeded']==len(review_ids) and cancelled['failed']==0
        def completed(command,payload):
            accepted=client.call(command,payload)
            def finished_task():
                task=client.call('get_task',{'task_id':accepted['task_id']})['task']
                return task if task['state'] not in ('queued','running','paused') else None
            return wait_for(finished_task,70 if command=='browser_solver_preflight' and payload.get('settings',{}).get('managed') else 12)
        phase='multiplatform_file_import';fixture=base/'playlist.csv'
        fixture.write_text('title,artist,album,duration,source,id\nSong,Artist,Album,180,spotify,synthetic-track\n','utf-8')
        imported=completed('refresh_playlist',{'file_path':str(fixture),'platform':'auto'})
        targets=imported.get('result',{}).get('targets',[])
        checks['native_multiplatform_file_import']=imported['state']=='completed' and len(targets)==1 and targets[0].get('source')=='spotify' and targets[0].get('target_id')=='spotify:synthetic-track'
        phase='manual_album_search';searched=completed('manual_album_search',{'mode':'literal','query':'Synthetic Album'})
        result=searched.get('result',{})
        checks['native_manual_search_readonly']=searched['state']=='blocked' and result.get('status')=='NOT_CONFIGURED' and result.get('read_only') is True and result.get('music_writes')==0 and not any(library.rglob('*'))
        checks['native_search_activity_visible']=any(job.get('job_kind')=='activity' and job.get('task_group')=='search' for job in client.call('list_workflows')['jobs'])
        album_db=client.call('list_library_albums',{})
        checks['native_local_album_database']=album_db['total']==0 and album_db['summary']['updated_at'] is not None
        ai_guard=completed('assist_workflows',{'job_ids':review_ids})
        checks['native_ai_assistance_guard']=ai_guard['state']=='blocked' and ai_guard.get('error_class')=='POLICY_BLOCKED'
        activity_id='activity:'+searched['task_id']
        removed=client.call('bulk_workflows',{'action':'delete','job_ids':[activity_id]})
        checks['native_delete_records_only']=removed['succeeded']==1 and not any(j['job_id']==activity_id for j in client.call('list_workflows')['jobs']) and not any(library.rglob('*'))
        update_setting=client.call('save_setup',{'updates':{'enabled':False}})
        checks['native_update_setting']=update_setting['config']['values']['updates']['enabled'] is False
        checks['native_fixed_qbt_tag']=client.call('get_status',{'compact':True})['config']['values']['qbt']['tag_prefix']=='MusicOps'
        solver_guard=completed('browser_solver_preflight',{'settings':{'base_url':'http://192.0.2.1:8191'}})
        checks['native_browser_solver_guard']=solver_guard['state']=='failed' and solver_guard.get('error_class')=='ValueError'
        filtered=client.call('list_workflows',{'state':'completed'})
        checks['native_task_status_filter']=bool(filtered['jobs']) and all(j['stage'] in ('completed','restored','cancelled','skipped') for j in filtered['jobs'])
        selected=client.call('list_workflows',{'state':'completed','selection':True})['job_ids']
        checks['native_full_queue_selection']=bool(selected) and all(client.call('get_workflow',{'job_id':jid})['job'].get('job_kind') not in ('batch','activity') for jid in selected)
        streaming=client.call('save_setup',{'streaming':{'enabled':False,'order':['netease','qqmusic']},'workflow':{'edition_policy':'complete'}})['config']['values']
        checks['native_streaming_and_edition_settings']=streaming['streaming']['order']==['netease','qqmusic'] and streaming['streaming']['enabled'] is False and streaming['workflow']['edition_policy']=='complete'
        fill=completed('start_workflow',{'scene':'fill'})
        checks['native_full_album_preparation']=fill['state']=='completed' and fill.get('result',{}).get('album_count')==0
        engine=client.call('engine_control',{'action':'start'})
        cookie=client.call('engine_control',{'action':'cookie','source':'qq','cookie':'synthetic_frozen=synthetic-only'})
        checks['native_managed_engine_cookie']=engine.get('status')=='OK' and cookie.get('saved') is True
        scheduling=client.call('get_status',{'compact':True}).get('performance',{})
        checks['native_background_policy']=scheduling.get('low_priority') is True and scheduling.get('mode') in ('system','efficiency')
        duplicate_scan=completed('scan_duplicates',{})
        checks['native_empty_duplicate_scan']=duplicate_scan['state']=='completed' and duplicate_scan['result']['groups']==[] and duplicate_scan['result']['music_writes']==0
        checks['native_duplicate_history']=client.call('duplicate_history',{})['transactions']==[]
        restore_guard=completed('restore_duplicates',{'tx_id':'unknown-synthetic'})
        checks['native_duplicate_restore_guard']=restore_guard['state']=='blocked' and restore_guard.get('error_class')=='POLICY_BLOCKED'
        tag_guard=completed('normalize_qbt_tags',{})
        checks['native_tag_cleanup_auth_guard']=tag_guard['state']=='blocked' and tag_guard.get('error_class')=='AUTH_REQUIRED'
        solved=completed('browser_solver_preflight',{'settings':{'managed':True}})
        checks['native_managed_solver']=solved['state']=='completed' and solved.get('result',{}).get('status')=='OK'
        client.call('pause_pipeline',{})
        try:
            direct=client.call('test_browser_solver',{'settings':{'managed':True}})
            checks['native_solver_independent_test']=direct.get('status')=='OK'
            qbt_check=client.call('test_qbt_connection',{})
            checks['native_qbt_independent_test']=qbt_check.get('status')=='AUTH_REQUIRED'
        finally:client.call('resume_pipeline',{})
        overview=client.call('list_workflows',{'view':'albums'})
        checks['native_album_overview']=overview.get('view')=='albums' and all(row.get('overview') and row.get('chain') for row in overview['jobs'])
        queue=client.call('queue_ai_reviews',{'all':True})
        checks['native_durable_ai_registration']=queue.get('status')=='OK' and type(queue.get('registered')) is int
        phase='configuration_write';result=client.call('save_setup',{'workflow':{'fl_unlimited':True,'fl_per_batch':2}})
        checks['configuration_roundtrip']=result['config']['values']['workflow']['fl_unlimited'] is True and result['config']['values']['workflow']['fl_per_batch']==2
        phase='native_gui_exit';process.wait(25);checks['native_cocoa_window_exit_zero']=process.returncode==0;checks['native_window_rendered']=screenshot.is_file() and screenshot.stat().st_size>2000
        phase='frozen_worker_restart';client.call('shutdown_worker')
        def writer_released():
            try:lock=WriterLock(base).acquire()
            except RuntimeError:return False
            lock.release();return True
        wait_for(writer_released,10)
        restarted=launch(worker,'--base',base);status=wait_for(lambda:client.call('get_status',{'compact':True}))
        checks['frozen_worker_restart_keeps_credentials']=any(item.get('ref')==ref for item in status['secret_refs']) and vault.get(ref,host)==secret
        checks['configuration_persisted_after_restart']=status['config']['values']['workflow']['fl_unlimited'] is True
        phase='offscreen_gui';offscreen=work/'offscreen-window.png';headless=launch(gui,'--base',base,'--screenshot',offscreen,'--quit-after','5000',platform_name='offscreen');headless.wait(20)
        checks['offscreen_gui_rendered']=headless.returncode==0 and offscreen.is_file() and offscreen.stat().st_size>2000
        phase='clean_shutdown';client.call('shutdown_worker');restarted.wait(15);checks['worker_clean_shutdown']=restarted.returncode==0
        checks['app_bundle_unchanged_after_use']=before==digest_tree(app)
        checks['no_profile_inside_app']=not any(path.name in ('profile','vault','state') for path in app.rglob('*'))
        report['passed']=REQUIRED_CHECKS.issubset(checks) and all(value is True for value in checks.values())
    except Exception as exc:
        report.update(passed=False,error_class=type(exc).__name__,error_phase=phase,process_exit_codes_before_cleanup=[process.poll() for process in processes])
    finally:
        try:client.call('shutdown_worker')
        except Exception:pass
        for process in processes:
            if process.poll() is None:
                process.terminate()
                try:process.wait(8)
                except subprocess.TimeoutExpired:process.kill();process.wait(5)
        for handle in handles:handle.close()
        if not report.get('passed'):
            # Only native loader error lines from this synthetic profile are
            # public diagnostics; never upload raw child logs or vault data.
            markers=('Symbol not found:','Library not loaded:','ImportError:','ModuleNotFoundError:','[PYI-','Could not load the Qt platform plugin')
            diagnostics=[]
            for handle in handles:
                with Path(handle.name).open('rb') as stream:
                    stream.seek(max(0,Path(handle.name).stat().st_size-8192));tail=stream.read().decode('utf-8',errors='replace')
                for line in tail.splitlines():
                    if not any(marker in line for marker in markers):continue
                    for value in sensitive_values:line=line.replace(value,'[REDACTED]')
                    for path,label in ((str(base),'<profile>'),(str(work),'<work>'),(str(app),'<app>')):line=line.replace(path,label)
                    if line[:512] not in diagnostics:diagnostics.append(line[:512])
            report['native_loader_diagnostics']=diagnostics[:8]
        report_path.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n','utf-8')
    print(json.dumps(report,ensure_ascii=True))
    if not report['passed']:raise SystemExit(1)

if __name__=='__main__':main()
