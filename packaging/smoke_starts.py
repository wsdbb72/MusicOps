"""Exercise all three user entry points against the actual packaged worker.

Uses a new profile, an empty library and one synthetic target. No external
requests, FL consumption or writes to an existing library are involved.
"""
import argparse,json,os,subprocess,time
from pathlib import Path
from musicops.state.store import StateStore
from musicops.config.schema import ConfigManager
from musicops.worker.client import WorkerClient


def main():
    parser=argparse.ArgumentParser();parser.add_argument('--bundle',required=True);parser.add_argument('--profile',required=True);parser.add_argument('--report',required=True)
    args=parser.parse_args();bundle=Path(args.bundle).resolve();base=Path(args.profile).resolve()
    store=StateStore(base/'state/musicops.sqlite');config=ConfigManager(store,base,initial_offline=False)
    current=config.current();values=current['values'];Path(values['paths']['library']).mkdir(parents=True,exist_ok=True)
    values['workflow'].update(setup_done=True,netease_fallback=False)
    values['catalog']['enabled']=False
    for provider in values['providers'].values():provider['enabled']=False
    config.save(values,current['version'])
    store.put('target_tracks','fixture-track',{'target_id':'fixture-track','raw_title':'Fixture Song','artists':['Fixture Artist'],'album':'Fixture Album','duration':120})
    env=os.environ.copy();env['QT_QPA_PLATFORM']='offscreen'
    for key in ('PYTHONPATH','PYTHONHOME','VIRTUAL_ENV'):env.pop(key,None)
    process=subprocess.Popen([str(bundle/'MusicOps.exe'),'--base',str(base),'--quit-after','180000'],cwd=bundle,env=env,creationflags=0x08000000)
    client=WorkerClient(base);report={'version':__import__('musicops').__version__,'evidence':'packaged_worker_three_entry_points','external_requests':0,'checks':{}}
    try:
        deadline=time.monotonic()+90
        while True:
            try:client.call('get_status',{'compact':True});break
            except (OSError,EOFError):
                if time.monotonic()>deadline:raise
                time.sleep(.1)
        from concurrent.futures import ThreadPoolExecutor
        from multiprocessing.connection import Client
        from musicops.worker.client import pipe_address,auth_key
        from musicops.platforms import ipc_family
        with Client(pipe_address(base),family=ipc_family(),authkey=auth_key(base)) as idle:
            started=time.monotonic()
            with ThreadPoolExecutor(max_workers=6) as pool:
                replies=list(pool.map(lambda _:WorkerClient(base,timeout=5).call('get_status',{'compact':True}),range(6)))
            report['checks']['native_concurrent_polling']=len(replies)==6 and time.monotonic()-started<5
        for scene in ('complete','upgrade','playlist'):
            before=time.monotonic();accepted=client.call('start_workflow',{'scene':scene,'target_ids':['fixture-track'] if scene=='playlist' else []})
            visible=client.call('list_workflows')['jobs']
            report['checks'][scene+'_immediately_visible']=time.monotonic()-before<5 and any(j['job_id']==accepted['job_id'] and j.get('job_kind')=='batch' for j in visible)
            deadline=time.monotonic()+15
            while True:
                task=client.call('get_task',{'task_id':accepted['task_id']})['task']
                if task['state'] not in ('queued','running','paused'):break
                if time.monotonic()>deadline:raise TimeoutError(scene+' preparation')
                time.sleep(.1)
            report['checks'][scene+'_preparation_completed']=task['state']=='completed'
        deadline=time.monotonic()+10
        while True:
            jobs=client.call('list_workflows')
            if not jobs['active']:break
            if time.monotonic()>deadline:raise TimeoutError('synthetic album task')
            time.sleep(.1)
        report['checks']['synthetic_target_visible_as_album']=any(j.get('task_type')=='search' and j['scene']=='playlist' and j['stage']=='needs_review' for j in jobs['jobs'])
        report['checks']['identification_and_search_are_separate']=any(j.get('task_type')=='identification' and j['stage']=='completed' and j.get('search_job_id') for j in jobs['jobs']) and any(j.get('task_type')=='search' and j.get('identification_job_id') for j in jobs['jobs'])
        review_ids=[j['job_id'] for j in jobs['jobs'] if j.get('task_type')=='search' and j['stage']=='needs_review']
        retried=client.call('bulk_workflows',{'action':'retry','job_ids':review_ids})
        report['checks']['native_bulk_retry_search']=bool(review_ids) and retried['succeeded']==len(review_ids) and all(client.call('get_workflow',{'job_id':jid})['job']['job_id']==jid for jid in review_ids)
        snapshot=client.call('get_workflow',{'job_id':review_ids[0]})['job']
        report['checks']['native_review_snapshot']=bool(snapshot.get('review_revision')) and bool(snapshot.get('prepared'))
        cancelled=client.call('bulk_workflows',{'action':'cancel','job_ids':review_ids})
        report['checks']['native_bulk_cancel_review_tasks']=cancelled['succeeded']==len(review_ids) and cancelled['failed']==0
        report['checks']['three_preparation_records']=len([j for j in jobs['jobs'] if j.get('job_kind')=='batch'])==3
        result=client.call('save_setup',{'workflow':{'fl_unlimited':True,'fl_per_batch':0}})
        report['checks']['native_unlimited_fl_roundtrip']=result['config']['values']['workflow']['fl_unlimited'] is True and result['config']['values']['workflow']['fl_per_batch']==0
        fixture=base/'playlist.csv';fixture.write_text('title,artist,album,duration,source,id\nSong,Artist,Album,180,spotify,synthetic-track\n','utf-8')
        def completed(command,payload):
            accepted=client.call(command,payload);deadline=time.monotonic()+(150 if command=='browser_solver_preflight' and payload.get('settings',{}).get('managed') else 12)
            while time.monotonic()<deadline:
                task=client.call('get_task',{'task_id':accepted['task_id']})['task']
                if task['state'] not in ('queued','running','paused'):return task
                time.sleep(.05)
            raise TimeoutError(command)
        imported=completed('refresh_playlist',{'file_path':str(fixture),'platform':'auto'})
        report['checks']['native_multiplatform_file_import']=imported['state']=='completed' and len(imported['result'].get('targets',[]))==1 and imported['result']['targets'][0]['source']=='spotify'
        searched=completed('manual_album_search',{'mode':'literal','query':'Synthetic Album'})
        report['checks']['native_manual_search_readonly']=searched['state']=='blocked' and searched['result']['status']=='NOT_CONFIGURED' and searched['result']['music_writes']==0
        artist=completed('manual_album_search',{'mode':'artist','query':'Synthetic Artist'})
        report['checks']['native_artist_discography_readonly']=artist['state']=='completed' and artist['result']['mode']=='artist' and artist['result']['albums']==[] and artist['result']['music_writes']==0 and artist['result']['complete'] is False
        report['checks']['native_search_activity_visible']=any(j.get('job_kind')=='activity' and j.get('task_group')=='search' for j in client.call('list_workflows')['jobs'])
        album_db=client.call('list_library_albums',{})
        report['checks']['native_local_album_database']=album_db['total']==0 and album_db['summary']['updated_at'] is not None
        ai_guard=completed('assist_workflows',{'job_ids':review_ids})
        report['checks']['native_ai_assistance_guard']=ai_guard['state']=='blocked' and ai_guard.get('error_class')=='POLICY_BLOCKED'
        activity_id='activity:'+searched['task_id']
        removed=client.call('bulk_workflows',{'action':'delete','job_ids':[activity_id]})
        report['checks']['native_delete_records_only']=removed['succeeded']==1 and not any(j['job_id']==activity_id for j in client.call('list_workflows')['jobs']) and not any(Path(values['paths']['library']).rglob('*'))
        update_setting=client.call('save_setup',{'updates':{'enabled':False}})
        report['checks']['native_update_setting']=update_setting['config']['values']['updates']['enabled'] is False
        report['checks']['native_fixed_qbt_tag']=client.call('get_status',{'compact':True})['config']['values']['qbt']['tag_prefix']=='MusicOps'
        solver_guard=completed('browser_solver_preflight',{'settings':{'base_url':'http://192.0.2.1:8191'}})
        report['checks']['native_browser_solver_guard']=solver_guard['state']=='failed' and solver_guard.get('error_class')=='ValueError'
        filtered=client.call('list_workflows',{'state':'completed'})
        report['checks']['native_task_status_filter']=bool(filtered['jobs']) and all(j['stage'] in ('completed','restored','cancelled','skipped') for j in filtered['jobs'])
        selected=client.call('list_workflows',{'state':'completed','selection':True})['job_ids']
        report['checks']['native_full_queue_selection']=bool(selected) and all(client.call('get_workflow',{'job_id':jid})['job'].get('job_kind') not in ('batch','activity') for jid in selected)
        streaming=client.call('save_setup',{'streaming':{'enabled':False,'order':['netease','qqmusic']},'workflow':{'edition_policy':'complete'}})['config']['values']
        report['checks']['native_streaming_and_edition_settings']=streaming['streaming']['order']==['netease','qqmusic'] and streaming['streaming']['enabled'] is False and streaming['workflow']['edition_policy']=='complete'
        fill=completed('start_workflow',{'scene':'fill'})
        report['checks']['native_full_album_preparation']=fill['state']=='completed' and fill.get('result',{}).get('album_count')==0
        try:
            client.call('start_workflow',{'scene':'expand','album_id':'0'*64})
            report['checks']['native_single_expansion_selection_guard']=False
        except RuntimeError as exc:report['checks']['native_single_expansion_selection_guard']='专辑不存在' in str(exc)
        engine=client.call('engine_control',{'action':'start'})
        cookie=client.call('engine_control',{'action':'cookie','source':'qq','cookie':'synthetic_frozen=synthetic-only'})
        report['checks']['native_managed_engine_cookie']=engine.get('status')=='OK' and cookie.get('saved') is True
        scheduling=client.call('get_status',{'compact':True}).get('performance',{})
        report['checks']['native_background_policy']=scheduling.get('low_priority') is True and scheduling.get('mode') in ('system','efficiency')
        duplicate_scan=completed('scan_duplicates',{})
        report['checks']['native_empty_duplicate_scan']=duplicate_scan['state']=='completed' and duplicate_scan['result']['groups']==[] and duplicate_scan['result']['music_writes']==0
        report['checks']['native_duplicate_history']=client.call('duplicate_history',{})['transactions']==[]
        restore_guard=completed('restore_duplicates',{'tx_id':'unknown-synthetic'})
        report['checks']['native_duplicate_restore_guard']=restore_guard['state']=='blocked' and restore_guard.get('error_class')=='POLICY_BLOCKED'
        tag_guard=completed('normalize_qbt_tags',{})
        report['checks']['native_tag_cleanup_auth_guard']=tag_guard['state']=='blocked' and tag_guard.get('error_class')=='AUTH_REQUIRED'
        solved=completed('browser_solver_preflight',{'settings':{'managed':True}})
        report['checks']['native_managed_solver']=solved['state']=='completed' and solved.get('result',{}).get('status')=='OK'
        client.call('pause_pipeline',{})
        try:
            direct=client.call('test_browser_solver',{'settings':{'managed':True}})
            report['checks']['native_solver_independent_test']=direct.get('status')=='OK'
            qbt_check=client.call('test_qbt_connection',{})
            report['checks']['native_qbt_independent_test']=qbt_check.get('status')=='AUTH_REQUIRED'
        finally:client.call('resume_pipeline',{})
        overview=client.call('list_workflows',{'view':'albums'})
        report['checks']['native_album_overview']=overview.get('view')=='albums' and all(row.get('overview') and row.get('chain') for row in overview['jobs'])
        queue=client.call('queue_ai_reviews',{'all':True})
        report['checks']['native_durable_ai_registration']=queue.get('status')=='OK' and type(queue.get('registered')) is int
        process.wait(195);report['checks']['gui_exit_zero']=process.returncode==0
        report['passed']=all(report['checks'].values())
    except Exception as exc:report.update(passed=False,error_class=type(exc).__name__,error=str(exc))
    finally:
        try:client.call('shutdown_worker')
        except Exception:pass
        if process.poll() is None:process.terminate();process.wait(10)
        Path(args.report).write_text(json.dumps(report,ensure_ascii=False,indent=2),'utf-8')
    print(json.dumps(report,ensure_ascii=True))
    if not report['passed']:raise SystemExit(1)


if __name__=='__main__':main()
