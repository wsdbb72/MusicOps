"""Verify bounded album waves in a frozen worker using isolated synthetic data.

No credentials, external requests, music downloads or library mutations.
The bundle points to Windows runtime or macOS Contents/MacOS.
"""
import argparse,copy,json,os,subprocess,time
from pathlib import Path
from musicops.config.schema import ConfigManager
from musicops.state.store import StateStore
from musicops.worker.client import WorkerClient
parser=argparse.ArgumentParser()
parser.add_argument('--bundle',required=True);parser.add_argument('--profile',required=True);parser.add_argument('--report',required=True)
args=parser.parse_args();base=Path(args.profile).resolve();bundle=Path(args.bundle).resolve();report_path=Path(args.report).resolve()
if (base/'state/musicops.sqlite').exists():raise SystemExit('Parallel acceptance requires a new isolated profile')
report_path.parent.mkdir(parents=True,exist_ok=True)
store=StateStore(base/'state/musicops.sqlite');manager=ConfigManager(store,base,initial_offline=False)
current=manager.current();cfg=current['values'];cfg['workflow'].update(setup_done=True,netease_fallback=False);cfg['catalog']['enabled']=False;cfg['ai']['enabled']=False
for settings in cfg['providers'].values():settings['enabled']=False
manager.save(cfg,current['version'])
store.put('evidence','workflow:batch:synthetic-batch',{'job_id':'batch:synthetic-batch','job_kind':'batch','batch_id':'synthetic-batch','stage':'completed','scene':'playlist'})
for i in range(8):
 row={'batch_id':'synthetic-batch','job_id':'parallel-'+str(i),'kind':'workflow','task_type':'identification','scene':'playlist','stage':'identifying','config':copy.deepcopy(cfg),'targets':[{'raw_title':'Synthetic Song','album':'Synthetic Album','artists':['Synthetic Artist'],'duration':120,'source':'local'}],'old_files':[],'created_at':'2026-01-01T00:00:00+00:00'}
 store.put('evidence','workflow:'+row['job_id'],row)
env=os.environ.copy()
for key in ('PYTHONPATH','PYTHONHOME','VIRTUAL_ENV'):env.pop(key,None)
process=subprocess.Popen([str(bundle/('MusicOps-worker.exe' if os.name=='nt' else 'MusicOps-worker')),'--base',str(base)],cwd=bundle,env=env,creationflags=0x08000000 if os.name=='nt' else 0,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
client=WorkerClient(base,timeout=10)
try:
 deadline=time.monotonic()+90
 while time.monotonic()<deadline:
  try:
   client.call('get_status',{'compact':True})
   jobs=[store.get('evidence','workflow:parallel-'+str(i)) for i in range(8)]
   if all(job['stage']=='completed' for job in jobs):break
  except (OSError,EOFError):pass
  time.sleep(.2)
 else:raise TimeoutError('Frozen parallel wave did not complete')
 tasks=store.list('tasks');waves=[task.get('result',{}).get('parallel_count',1) for task in tasks if task['command']=='advance_workflows' and task.get('result')]
 checks={'actual_worker_running':process.poll() is None,'eight_identifications_completed':all(job['stage']=='completed' for job in jobs),'unique_search_children':len({job.get('search_job_id') for job in jobs})==8,'parallel_waves_observed':max(waves,default=0)>=2,'bounded_at_four':max(waves,default=0)<=4,'no_downloads_or_tokens':not store.list('download_tasks') and not store.list('token_reservations')}
 report={'passed':all(checks.values()),'checks':checks,'observed_wave_sizes':waves,'synthetic_only':True,'external_requests':0}
 report_path.write_text(json.dumps(report,indent=2),'utf-8');print(json.dumps(report));assert report['passed']
finally:
 if process.poll() is None:
  # Wait for the short, offline fixture waves to finish; never cancel real work.
  for _ in range(200):
   try:client.call('shutdown_worker',{});break
   except Exception:time.sleep(.1)
  process.wait(20)
