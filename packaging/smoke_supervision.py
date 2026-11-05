"""Crash/restart/intentional-stop acceptance against an isolated frozen Windows app."""
import argparse,ctypes,json,os,subprocess,time
from pathlib import Path
from musicops.config.schema import ConfigManager
from musicops.state.store import StateStore
from musicops.worker.client import WorkerClient
from musicops.worker.supervision import stop


def wait_for(check,seconds=90):
    deadline=time.monotonic()+seconds
    while time.monotonic()<deadline:
        try:
            value=check()
            if value:return value
        except (OSError,ValueError,KeyError,RuntimeError):pass
        time.sleep(.2)
    raise RuntimeError('Frozen supervision acceptance timed out')


def main():
    parser=argparse.ArgumentParser();parser.add_argument('--bundle',required=True);parser.add_argument('--profile',required=True);parser.add_argument('--report',required=True)
    args=parser.parse_args();bundle=Path(args.bundle).resolve();base=Path(args.profile).resolve()
    if os.name!='nt':raise RuntimeError('Windows-only acceptance')
    if (base/'state/musicops.sqlite').exists():raise RuntimeError('Acceptance needs a new isolated profile')
    store=StateStore(base/'state/musicops.sqlite');manager=ConfigManager(store,base)
    current=manager.current();cfg=current['values'];cfg['updates']['enabled']=False;manager.save(cfg,current['version'])
    env=os.environ.copy();env['QT_QPA_PLATFORM']='offscreen'
    for name in ('PYTHONPATH','PYTHONHOME','VIRTUAL_ENV'):env.pop(name,None)
    gui=subprocess.Popen([str(bundle/'MusicOps.exe'),'--base',str(base)],env=env,cwd=bundle,creationflags=0x08000000,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
    client=WorkerClient(base,timeout=5);checks={}
    def health():return json.loads((base/'state/worker-health.json').read_text('utf-8'))
    try:
        first=wait_for(lambda:health() if health().get('queue_alive') else None)
        wait_for(lambda:client.call('get_status',{'compact':True}))
        wait_for(lambda:(base/'state/worker-supervisor.ps1').is_file())
        # The test owns this GUI's Popen handle. Its disappearance must not stop
        # background work or its independently running supervisor.
        gui.terminate();gui.wait(15);checks['worker_survives_gui_exit']=bool(wait_for(lambda:client.call('get_status',{'compact':True})))
        # Hold a handle to the exact test worker; verify the image before kill.
        from ctypes import wintypes
        kernel=ctypes.WinDLL('kernel32',use_last_error=True)
        kernel.OpenProcess.argtypes=[wintypes.DWORD,wintypes.BOOL,wintypes.DWORD];kernel.OpenProcess.restype=wintypes.HANDLE
        kernel.QueryFullProcessImageNameW.argtypes=[wintypes.HANDLE,wintypes.DWORD,wintypes.LPWSTR,ctypes.POINTER(wintypes.DWORD)]
        kernel.TerminateProcess.argtypes=[wintypes.HANDLE,wintypes.UINT];kernel.CloseHandle.argtypes=[wintypes.HANDLE]
        handle=kernel.OpenProcess(0x1001,False,first['pid'])
        if not handle:raise RuntimeError('Cannot open the isolated worker')
        try:
            size=wintypes.DWORD(32768);name=ctypes.create_unicode_buffer(size.value)
            if not kernel.QueryFullProcessImageNameW(handle,0,name,ctypes.byref(size)) or Path(name.value).resolve()!=bundle/'MusicOps-worker.exe':raise RuntimeError('Worker image identity changed')
            if not kernel.TerminateProcess(handle,91):raise RuntimeError('Could not stop isolated worker')
        finally:kernel.CloseHandle(handle)
        second=wait_for(lambda:health() if health().get('pid')!=first['pid'] and health().get('queue_alive') else None)
        checks['worker_restarted_without_gui']=second['pid']!=first['pid']
        checks['restarted_worker_answers']=bool(wait_for(lambda:client.call('get_status',{'compact':True})))
        client.call('shutdown_worker',{});time.sleep(8)
        try:client.call('get_status',{'compact':True})
        except (OSError,EOFError,RuntimeError):checks['intentional_shutdown_not_restarted']=True
        else:raise RuntimeError('Supervisor replayed an intentional shutdown')
        report=Path(args.report);report.parent.mkdir(parents=True,exist_ok=True)
        report.write_text(json.dumps({'passed':all(checks.values()),'checks':checks,'external_requests':0},indent=2),encoding='utf-8')
        print(json.dumps(checks))
    finally:
        stop(base)
        try:wait_for(lambda:client.call('shutdown_worker',{}),seconds=10)
        except (OSError,EOFError,RuntimeError):pass
        if gui.poll() is None:gui.terminate();gui.wait(15)


if __name__=='__main__':main()
