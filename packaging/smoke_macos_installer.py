"""Install/reinstall the real PKG on a clean disposable GitHub macOS runner."""
import argparse,hashlib,json,os,plistlib,subprocess,sys,time,uuid
from pathlib import Path
from smoke_macos import digest_tree,wait_for,valid_acceptance
from macos_installer import IDENTIFIER

def run(*args,**kwargs):return subprocess.run([str(a) for a in args],check=True,**kwargs)

def main():
    p=argparse.ArgumentParser();p.add_argument('--pkg',required=True);p.add_argument('--app',required=True);p.add_argument('--work',required=True);p.add_argument('--report',required=True);p.add_argument('--arch',required=True);args=p.parse_args()
    if sys.platform!='darwin' or os.environ.get('GITHUB_ACTIONS')!='true':raise SystemExit('Only run installer acceptance on a disposable GitHub macOS runner')
    from musicops.clients.vault import Vault
    from musicops.worker.client import WorkerClient
    from musicops.worker.lock import WriterLock
    source=Path(args.app).resolve();installed=Path('/Applications/MusicOps.app');work=Path(args.work).resolve();work.mkdir(parents=True,exist_ok=True)
    if installed.exists() or installed.is_symlink():raise RuntimeError('Refuse to overwrite a pre-existing app on the acceptance machine')
    profile=Path.home()/'Library/Application Support/MusicOps/profile'
    if profile.exists() or profile.is_symlink():raise RuntimeError('Installer acceptance requires a new disposable user profile')
    profile.mkdir(parents=True);sentinel=profile/'installer-preservation.txt';sentinel.write_text('synthetic profile must survive','utf-8')
    vault=Vault(profile);secret='synthetic-installer-'+uuid.uuid4().hex;ref='vault://installer/preservation';host='installer.example.invalid';vault.set(ref,secret,host)
    encrypted_before=vault.path.read_bytes();checks={};process=None;client=None
    report={'evidence':'native_pkg_install_reinstall_and_installed_app','checks':checks,'external_requests':0,'developer_id_installer_signed':False,'notarized':False}
    phase='installer_structure'
    try:
        listing=run('/usr/sbin/installer','-showChoicesXML','-pkg',args.pkg,'-target','/',capture_output=True,text=True).stdout
        checks['native_installer_choices_readable']='musicops' in listing
        phase='first_install';run('/usr/bin/sudo','-n','/usr/sbin/installer','-pkg',args.pkg,'-target','/')
        expected=digest_tree(source);checks['installed_to_applications']=installed.is_dir() and not installed.is_symlink()
        checks['installed_payload_matches_built_app']=expected==digest_tree(installed)
        checks['app_owned_by_root']=installed.stat().st_uid==0 and not os.access(installed,os.W_OK)
        run('/usr/bin/codesign','--verify','--deep','--strict',installed);checks['installed_app_signature_valid']=True
        receipt=plistlib.loads(run('/usr/sbin/pkgutil','--pkg-info-plist',IDENTIFIER,capture_output=True).stdout)
        info=plistlib.loads((installed/'Contents/Info.plist').read_bytes());checks['receipt_version_matches_app']=receipt['pkg-version']==info['CFBundleShortVersionString']
        checks['profile_and_credentials_survive_first_install']=sentinel.read_text('utf-8')=='synthetic profile must survive' and vault.path.read_bytes()==encrypted_before and vault.get(ref,host)==secret
        phase='running_worker_guard';base=work/'busy-profile';client=WorkerClient(base)
        process=subprocess.Popen([str(installed/'Contents/MacOS/MusicOps-worker'),'--base',str(base)],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        wait_for(lambda:client.call('get_status',{'compact':True}))
        refused=subprocess.run(['/usr/bin/sudo','-n','/usr/sbin/installer','-pkg',str(args.pkg),'-target','/'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=90)
        checks['running_worker_prevents_replacement']=refused.returncode!=0 and process.poll() is None and expected==digest_tree(installed)
        client.call('shutdown_worker');process.wait(15)
        phase='reinstall';stale=installed/'Contents/obsolete-from-previous-version.txt'
        run('/usr/bin/sudo','-n','/usr/bin/touch',stale)
        run('/usr/bin/sudo','-n','/usr/sbin/installer','-pkg',args.pkg,'-target','/')
        checks['reinstall_removes_obsolete_bundle_files']=not stale.exists() and expected==digest_tree(installed)
        checks['reinstall_preserves_profile_and_credentials']=sentinel.read_text('utf-8')=='synthetic profile must survive' and vault.path.read_bytes()==encrypted_before and vault.get(ref,host)==secret
        phase='installed_app_acceptance';installed_report=Path(args.report).with_name(Path(args.report).stem+'-app.json')
        run(sys.executable,Path(__file__).with_name('smoke_macos.py'),'--app',installed,'--work',work/'installed-app','--report',installed_report,'--expected-arch',args.arch)
        result=json.loads(installed_report.read_text('utf-8'));checks['installed_app_acceptance_checks']=valid_acceptance(result)
        report['installed_app_check_count']=len(result.get('checks',{}))
        report['passed']=all(checks.values()) and len(checks)==11
    except Exception as exc:report.update(passed=False,error_class=type(exc).__name__,error_phase=phase)
    finally:
        if client:
            try:client.call('shutdown_worker')
            except Exception:pass
        if process and process.poll() is None:process.terminate();process.wait(10)
        Path(args.report).write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n','utf-8')
    print(json.dumps(report,ensure_ascii=True))
    if not report['passed']:raise SystemExit(1)

if __name__=='__main__':main()
