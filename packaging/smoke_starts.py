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
    process=subprocess.Popen([str(bundle/'MusicOps.exe'),'--base',str(base),'--quit-after','20000'],cwd=bundle,env=env,creationflags=0x08000000)
    client=WorkerClient(base);report={'version':'0.4.1','evidence':'packaged_worker_three_entry_points','external_requests':0,'checks':{}}
    try:
        deadline=time.monotonic()+25
        while True:
            try:client.call('get_status',{'compact':True});break
            except (OSError,EOFError):
                if time.monotonic()>deadline:raise
                time.sleep(.1)
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
        report['checks']['synthetic_target_visible_as_album']=any(j.get('job_kind')!='batch' and j['scene']=='playlist' and j['stage']=='skipped' for j in jobs['jobs'])
        report['checks']['three_preparation_records']=len([j for j in jobs['jobs'] if j.get('job_kind')=='batch'])==3
        result=client.call('save_setup',{'workflow':{'fl_unlimited':True,'fl_per_batch':0}})
        report['checks']['native_unlimited_fl_roundtrip']=result['config']['values']['workflow']['fl_unlimited'] is True and result['config']['values']['workflow']['fl_per_batch']==0
        fixture=base/'playlist.csv';fixture.write_text('title,artist,album,duration,source,id\nSong,Artist,Album,180,spotify,synthetic-track\n','utf-8')
        def completed(command,payload):
            accepted=client.call(command,payload);deadline=time.monotonic()+12
            while time.monotonic()<deadline:
                task=client.call('get_task',{'task_id':accepted['task_id']})['task']
                if task['state'] not in ('queued','running','paused'):return task
                time.sleep(.05)
            raise TimeoutError(command)
        imported=completed('refresh_playlist',{'file_path':str(fixture),'platform':'auto'})
        report['checks']['native_multiplatform_file_import']=imported['state']=='completed' and len(imported['result'].get('targets',[]))==1 and imported['result']['targets'][0]['source']=='spotify'
        searched=completed('manual_album_search',{'mode':'literal','query':'Synthetic Album'})
        report['checks']['native_manual_search_readonly']=searched['state']=='completed' and searched['result']['status']=='NOT_CONFIGURED' and searched['result']['music_writes']==0
        process.wait(30);report['checks']['gui_exit_zero']=process.returncode==0
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
