"""Real macOS runner acceptance of the frozen .app and worker.

Creates a new profile and an empty synthetic library. No site requests, audio
downloads, FL use or writes to an existing library are part of this check.
"""
from __future__ import annotations
import argparse,hashlib,json,os,platform,plistlib,subprocess,sys,time,uuid
from pathlib import Path

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
    parser=argparse.ArgumentParser();parser.add_argument('--app',required=True);parser.add_argument('--work',required=True);parser.add_argument('--report',required=True);parser.add_argument('--expected-arch',choices=('arm64','x86_64'),required=True);args=parser.parse_args()
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
    report={'version':__version__,'platform':'macOS','architecture':platform.machine(),'runner_macos':platform.mac_ver()[0],'evidence':'native_packaged_app_worker_keychain_three_entries','external_requests':0,'developer_id_signed':False,'notarized':False,'checks':{}}
    checks=report['checks'];client=WorkerClient(base,timeout=20);processes=[];handles=[]
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
        # The worker receives a new token over authenticated local IPC and writes
        # encrypted data. No token value is printed or added to the report.
        phase='keychain_vault_roundtrip';secret='synthetic-macos-'+uuid.uuid4().hex;ref='vault://smoke/macos/token';host='smoke.example.invalid'
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
        report['passed']=all(checks.values())
    except Exception as exc:
        report.update(passed=False,error_class=type(exc).__name__,error_phase=phase)
    finally:
        try:client.call('shutdown_worker')
        except Exception:pass
        for process in processes:
            if process.poll() is None:
                process.terminate()
                try:process.wait(8)
                except subprocess.TimeoutExpired:process.kill();process.wait(5)
        for handle in handles:handle.close()
        report_path.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n','utf-8')
    print(json.dumps(report,ensure_ascii=True))
    if not report['passed']:raise SystemExit(1)

if __name__=='__main__':main()
