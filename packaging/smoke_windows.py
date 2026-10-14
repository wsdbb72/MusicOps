"""Launch native executables with no Python/media tools on PATH, via real IPC."""
import argparse,copy,hashlib,json,os,subprocess,time,platform
from pathlib import Path
from musicops.worker.client import WorkerClient

def main():
    parser=argparse.ArgumentParser();parser.add_argument('--bundle',required=True);parser.add_argument('--profile',required=True);parser.add_argument('--report',required=True);parser.add_argument('--screenshot',required=True);args=parser.parse_args()
    bundle=Path(args.bundle).resolve();base=Path(args.profile).resolve();base.mkdir(parents=True,exist_ok=True);report={'schema_version':1,'evidence_level':'packaged_exe_local_isolated_environment','os':platform.platform(),'bundle':str(bundle),'profile':str(base),'checks':{}}
    env=os.environ.copy();env['PATH']=str(Path(os.environ['SystemRoot'])/'System32');env.pop('PYTHONPATH',None);env.pop('PYTHONHOME',None);env.pop('VIRTUAL_ENV',None);env['QT_QPA_PLATFORM']='offscreen'
    from musicops.state.store import StateStore
    from musicops.config.schema import ConfigManager
    config=ConfigManager(StateStore(base/'state/musicops.sqlite'),base,initial_offline=True);current=config.current();values=current['values'];values['paths']['tools']={key:str(bundle/'tools'/(key+'.exe')) for key in ('ffmpeg','ffprobe')};config.save(values,current['version'])
    process=subprocess.Popen([str(bundle/'MusicOps.exe'),'--demo','--base',str(base),'--screenshot',args.screenshot,'--quit-after','7000'],cwd=bundle,env=env,creationflags=0x08000000)
    client=WorkerClient(base)
    def call(command,payload={}):return client.call(command,payload)
    def task(command,payload={}):
        accepted=call(command,payload);task_id=accepted['task_id'];deadline=time.monotonic()+30
        while time.monotonic()<deadline:
            found=next(t for t in call('list_tasks')['tasks'] if t['task_id']==task_id)
            if found['state']=='completed':return found['result']
            if found['state'] in ('failed','blocked','recovery_required'):raise RuntimeError(json.dumps(found,ensure_ascii=False))
            time.sleep(.05)
        raise TimeoutError(command)
    try:
        deadline=time.monotonic()+15
        while True:
            try:initial=call('get_status');break
            except (OSError,EOFError):
                if time.monotonic()>deadline:raise
                time.sleep(.1)
        report['checks']['isolated_demo_profile']=Path(initial['config']['values']['paths']['library']).resolve().is_relative_to(base)
        report['checks']['offline']=initial['offline'] is True
        tools=initial['config']['values']['paths']['tools'];report['checks']['bundled_tools']=all(Path(value).parent==bundle/'tools' for value in tools.values())
        demo=task('demo');report['checks']['native_demo_complete']=demo['evidence']=='synthetic_only'
        sources=list((base/'demo'/'source').glob('*.flac'));source_hashes={str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in sources}
        resource=task('resources',{'title':'晨光','artist':'示例艺人','album':'离线演示专辑','duration':2});report['checks']['resources_previews']=len(resource['candidates'])==2
        candidates=task('search',{'query':'星河'})['candidates'];plan=task('prepare_fill',{'candidate':candidates[0]})['plan'];plan['reviewed']=True;fill=task('submit_plan',{'plan':plan});report['checks']['fill_transaction']=fill['state']=='COMMITTED'
        suggestions=task('prepare_dedup')['suggestions'];selection=next(s for s in suggestions if s['action']=='quarantine_candidate');plan=task('prepare_dedup',{'selected_file_id':selection['quarantine_file_id']})['plan'];plan['reviewed']=True;isolated=task('submit_plan',{'plan':plan});rollback=task('request_rollback',{'tx_id':isolated['tx_id']});report['checks']['dedup_rollback']=rollback['state']=='ROLLED_BACK'
        original=base/'demo'/'library'/'示例艺人'/'离线演示专辑'/'01 晨光.flac';old=original.read_bytes();plan=task('prepare_repair',{'path':str(original),'changes':{'album':['打包修复测试']},'locks':[]})['plan'];plan['reviewed']=True;repair=task('submit_plan',{'plan':plan});rollback=task('request_rollback',{'tx_id':repair['tx_id']});report['checks']['repair_full_byte_rollback']=original.read_bytes()==old and rollback['state']=='ROLLED_BACK'
        config=call('get_status')['config'];values=copy.deepcopy(config['values']);values['lyrics']['write_mode']='embedded';call('update_config',{'values':values,'expected_version':config['version']})
        from musicops.media.tags import read_tag_fields,embedded_covers
        for kind in ('lyrics','cover'):
            candidate=next(c for c in resource['candidates'] if c['kind']==kind)
            plan=task('prepare_resource',{'path':str(original),'kind':kind,'candidate':candidate,'lyrics':candidate.get('lyrics',''),'duration':2,'approved':True})['plan'];plan['reviewed']=True;changed=task('submit_plan',{'plan':plan})
            readback=bool(read_tag_fields(original).get('lyrics')) if kind=='lyrics' else bool(embedded_covers(original))
            restored=task('request_rollback',{'tx_id':changed['tx_id']})
            report['checks']['embedded_'+kind+'_readback_rollback']=readback and original.read_bytes()==old and restored['state']=='ROLLED_BACK'
        report['checks']['source_unchanged']=all(hashlib.sha256(Path(p).read_bytes()).hexdigest()==h for p,h in source_hashes.items())
        second=WorkerClient(base);report['checks']['reattach_same_tasks']=len(second.call('list_tasks')['tasks'])==len(call('list_tasks')['tasks'])
        process.wait(timeout=15);report['checks']['gui_exit_zero']=process.returncode==0;report['checks']['qt_screenshot']=Path(args.screenshot).is_file() and Path(args.screenshot).stat().st_size>1000
        report['limitations']=['在已安装Python的本机清空子进程PATH/PYTHONPATH/PYTHONHOME/VIRTUAL_ENV启动原生EXE，证明运行不依赖这些路径；未取得独立无Python实体样机证据','真实来源、音乐库和Roon尚未验收','特定Gyan构建完整对应源码未随包，公开再分发需补齐']
        report['passed']=all(report['checks'].values())
    except Exception as exc:
        report['passed']=False;report['error']=str(exc)
    finally:
        try:call('shutdown_worker')
        except Exception:pass
        if process.poll() is None:
            try:process.wait(timeout=15)
            except subprocess.TimeoutExpired:process.terminate();process.wait(timeout=5)
        Path(args.report).write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding='utf-8')
    if not report['passed']:raise SystemExit(1)

if __name__=='__main__':main()
