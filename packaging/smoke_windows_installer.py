"""Install and upgrade the actual frozen app under a unique, isolated test AppId."""
import argparse,hashlib,json,os,subprocess,sys,time,uuid
from pathlib import Path
from musicops.config.schema import ConfigManager
from musicops.state.store import StateStore
from musicops.clients.vault import Vault
from musicops.worker.client import WorkerClient

parser=argparse.ArgumentParser()
parser.add_argument('--bundle',required=True);parser.add_argument('--compiler',required=True)
parser.add_argument('--work',required=True);parser.add_argument('--report',required=True)
args=parser.parse_args()
if sys.platform!='win32':raise SystemExit('Native Windows acceptance requires Windows')
from musicops import __version__
work=Path(args.work).resolve();work.mkdir(parents=True,exist_ok=True)
if any(work.iterdir()):raise SystemExit('Installer acceptance requires a new empty work directory')
repo=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(repo/'tests'))
from test_windows_installer import Installation,run_process
bundle=Path(args.bundle).resolve();target=work/'installed-runtime';identity='MusicOps.QA.'+uuid.uuid4().hex[:12]
compiler=Path(args.compiler).resolve();qa=work/'qa-installer'
active_profile=Path(os.environ.get('LOCALAPPDATA',''))/'MusicOps/active-profile.json'
previous_active=active_profile.read_bytes() if active_profile.is_file() else None
qa.mkdir(exist_ok=True)
build=run_process([compiler,'/DBundleDir='+str(bundle),'/DAppIdentity='+identity,'/O'+str(qa),repo/'packaging/MusicOps.iss'],timeout=900)
assert build.returncode==0,build.stdout+build.stderr
installation=Installation(next(qa.glob('*-Setup.exe')),target,identity);checks={};process=None;vault=None;tool=None
profile=target/'profile';client=WorkerClient(profile)
def digest(p):return hashlib.sha256(p.read_bytes()).hexdigest()
try:
    code,_=installation.run(directory=target);checks['fresh_native_installer']=code==0
    assert checks['fresh_native_installer']
    # Production GUI attachment enables online mode. Seed that mode explicitly;
    # checking an offline fixture raced the asynchronous enable_online request.
    # Every external source remains disabled in this isolated installation.
    store=StateStore(profile/'state/musicops.sqlite');cfg=ConfigManager(store,profile,initial_offline=False)
    current=cfg.current();values=current['values'];values['workflow']['setup_done']=True;values['catalog']['enabled']=False
    for provider in values['providers'].values():provider['enabled']=False
    cfg.save(values,current['version'])
    vault=Vault(profile);vault.set('vault://qa/synthetic','synthetic-credential-'+uuid.uuid4().hex,'example.invalid')
    tool=target/'tools/preserved-tool.txt';tool.parent.mkdir(exist_ok=True);tool.write_text('synthetic preserved tool','utf-8')
    preserve=[p for root in (profile,target/'tools') for p in root.rglob('*') if p.is_file()]
    before={str(p):digest(p) for p in preserve}
    installation.registry('DisplayVersion','0.4.1')
    code,log=installation.run();checks['upgrade_reuses_existing_registration']=code==0 and str(target).casefold() in log.casefold()
    checks['profile_vault_and_tools_preserved']=all(digest(Path(p))==value for p,value in before.items())
    checks['installed_executables_match_release']=all(digest(target/name)==digest(bundle/name) for name in ('MusicOps.exe','MusicOps-worker.exe'))
    from smoke_engines import smoke as smoke_engines
    engines=smoke_engines(target/'tools/services',work/'installed-engine-profile')
    checks['installed_executables_match_release'] &= engines['passed']
    (work/'installed-engines.json').write_text(json.dumps(engines,indent=2),'utf-8')
    env=os.environ.copy()
    for k in ('PYTHONPATH','PYTHONHOME','VIRTUAL_ENV','QT_QPA_PLATFORM'):env.pop(k,None)
    process=subprocess.Popen([str(target/'MusicOps.exe'),'--base',str(profile),'--quit-after','5500','--screenshot',str(work/'installed-runtime.png')],cwd=target,env=env,creationflags=0x08000000)
    deadline=time.monotonic()+20
    while True:
        try:status=client.call('get_status',{'compact':True});break
        except (OSError,EOFError):
            if time.monotonic()>deadline:raise
            time.sleep(.1)
    checks['installed_frozen_worker_connects']=status['offline'] is False and Path(status['base']).resolve()==profile.resolve() and any(v['ref']=='vault://qa/synthetic' for v in status['secret_refs'])
    code,_=installation.run();checks['active_frozen_app_blocks_upgrade']=code!=0
    process.wait(20);checks['installed_gui_runs_and_renders']=process.returncode==0 and (work/'installed-runtime.png').stat().st_size>2000
    client.call('shutdown_worker')
    deadline=time.monotonic()+10
    while time.monotonic()<deadline:
        code,_=installation.run()
        if code==0:break
        time.sleep(.2)
    checks['upgrade_after_safe_worker_exit']=code==0
finally:
    try:client.call('shutdown_worker')
    except Exception:pass
    if process and process.poll() is None:process.terminate();process.wait(10)
    uninstaller=target/'unins000.exe'
    if uninstaller.exists():
        result=run_process([uninstaller,'/VERYSILENT','/SUPPRESSMSGBOXES','/NORESTART'])
        checks['uninstall_keeps_private_profile_and_tools']=result.returncode==0 and vault is not None and tool is not None and vault.path.is_file() and tool.is_file() and not (target/'MusicOps.exe').exists()
    if previous_active is not None:active_profile.write_bytes(previous_active)
    else:active_profile.unlink(missing_ok=True)
report={'version':__version__,'evidence':'actual_frozen_runtime_native_installer_upgrade_reinstall','checks':checks,'passed':len(checks)==9 and all(checks.values()),'real_music_library_writes':0}
Path(args.report).parent.mkdir(parents=True,exist_ok=True)
Path(args.report).write_text(json.dumps(report,ensure_ascii=False,indent=2),'utf-8')
print(json.dumps(report,ensure_ascii=True))
if not report['passed']:raise SystemExit(1)
