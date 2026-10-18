"""Verify the public Windows executable starts online and uses private DPAPI settings."""
import argparse,json,os,subprocess,time,wave
from pathlib import Path
from musicops.worker.client import WorkerClient

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--bundle',required=True);parser.add_argument('--profile',required=True);parser.add_argument('--report',required=True);parser.add_argument('--screenshot',required=True);args=parser.parse_args()
    args.screenshot=str(Path(args.screenshot).resolve())
    bundle=Path(args.bundle).resolve();base=Path(args.profile).resolve();base.mkdir(parents=True,exist_ok=True);Path(args.screenshot).parent.mkdir(parents=True,exist_ok=True)
    env=os.environ.copy();env['PATH']=str(Path(os.environ['SystemRoot'])/'System32');env['QT_QPA_PLATFORM']='offscreen'
    for key in ('PYTHONPATH','PYTHONHOME','VIRTUAL_ENV'):env.pop(key,None)
    process=subprocess.Popen([str(bundle/'MusicOps.exe'),'--base',str(base),'--screenshot',args.screenshot,'--quit-after','10000'],cwd=bundle,env=env,creationflags=0x08000000)
    client=WorkerClient(base);report={'evidence':'public_packaged_exe_real_named_pipe','version':'0.4.6','checks':{},'external_requests':0,'music_writes':0}
    def task(command,payload):
        accepted=client.call(command,payload);deadline=time.monotonic()+30
        while time.monotonic()<deadline:
            result=client.call('get_task',{'task_id':accepted['task_id']})['task']
            if result['state'] not in ('queued','running','paused'):return result
            time.sleep(.05)
        raise TimeoutError(command)
    try:
        deadline=time.monotonic()+20
        while True:
            try:initial=client.call('get_status',{'compact':True});break
            except (OSError,EOFError):
                if time.monotonic()>deadline:raise
                time.sleep(.1)
        report['checks']['default_online']=initial['offline'] is False
        report['checks']['first_start_no_scan_or_network_task']=not initial['files'] and not initial['tasks']
        report['checks']['private_defaults_removed']='.example.invalid' in initial['config']['values']['lyrics']['primary']['base_url'] and not initial['config']['values']['ai']['enabled']
        report['checks']['required_credential_settings_present']=all(key in initial['config']['values'] for key in ('ai','lyrics','qbt','providers','music_servers','catalog'))
        library=base/'library';library.mkdir(exist_ok=True);audio=library/'Fixture.wav'
        with wave.open(str(audio),'wb') as f:f.setparams((1,2,44100,0,'NONE','not compressed'));f.writeframes(b'\x00'*8820)
        indexed=task('index_library',{'root':str(library),'lightweight':True})
        report['checks']['native_online_metadata_index']=indexed['state']=='completed' and indexed['result']['count']==1 and indexed['result']['metadata_only']
        page=client.call('list_library_files',{'query':'Fixture','limit':200});report['checks']['native_library_paging']=page['total']==1 and page['files'][0]['sha256'] is None
        cfg=client.call('get_status',{'compact':True})['config'];values=cfg['values'];values['catalog']['musicbrainz']['enabled']=False;values['catalog']['discogs']['enabled']=False
        client.call('update_config',{'values':values,'expected_version':cfg['version']})
        found=task('identify_metadata',{'title':'Fixture Song','artists':['Fixture Artist'],'album':'Fixture Album'})
        report['checks']['six_catalog_source_links_imported']=len(found.get('result',{}).get('lookup_links',[]))==6
        client.call('set_secret',{'ref':'vault://catalog/discogs/token','value':'virtual-native-token','host':'api.discogs.com'})
        state=client.call('get_status',{'compact':True});report['checks']['native_dpapi_credential_editing']=any(r['ref']=='vault://catalog/discogs/token' for r in state['secret_refs'])
        report['checks']['configured_secret_not_in_status']='virtual-native-token' not in json.dumps(state)
        invalid=task('refresh_playlist',{'playlist_id':'0'});report['checks']['netease_guard_before_network']=invalid.get('error_class')=='ValueError'
        process.wait(timeout=20);report['checks']['gui_exit_zero']=process.returncode==0
        report['checks']['formal_startup_screenshot']=Path(args.screenshot).is_file() and Path(args.screenshot).stat().st_size>1000
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
