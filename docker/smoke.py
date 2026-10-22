"""Exercise the actual Linux image with synthetic data and temporary volumes."""
import base64
import hashlib
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request

image=sys.argv[1];report=Path(sys.argv[2]);report.parent.mkdir(parents=True,exist_ok=True)
name='musicops-ci-'+str(os.getpid());volumes=[name+'-config',name+'-media'];checks={}
password='synthetic-container-password-only'
def docker(*args,**kwargs):
    return subprocess.run(['docker',*args],check=True,text=True,capture_output=True,**kwargs).stdout.strip()
def execute(code):
    return docker('exec',name,'python','-c',code)
def wait_healthy():
    deadline=time.monotonic()+150
    while time.monotonic()<deadline:
        status=json.loads(docker('inspect',name))[0]['State']
        if not status['Running']:
            logged=subprocess.run(['docker','logs',name],capture_output=True,text=True)
            raise RuntimeError('Container exited: '+logged.stdout+logged.stderr)
        if status.get('Health',{}).get('Status')=='healthy':return
        time.sleep(2)
    raise RuntimeError('Container did not become healthy: '+docker('logs',name))
try:
    docker('run','-d','--name',name,'--platform','linux/amd64','--shm-size','1g',
           '-p','127.0.0.1::8080','-v',volumes[0]+':/config','-v',volumes[1]+':/media',image)
    wait_healthy()
    info=json.loads(docker('inspect',name))[0]
    port=int(info['NetworkSettings']['Ports']['8080/tcp'][0]['HostPort'])
    checks['non_root']=execute('import os;print(os.getuid())')=='1000'
    checks['amd64']=json.loads(docker('image','inspect',image))[0]['Architecture']=='amd64'
    # Generated password stays private; use it in memory, never print it.
    password=execute("from pathlib import Path;print(Path('/config/web-password').read_text().strip())")
    auth='Basic '+base64.b64encode(('musicops:'+password).encode()).decode()
    url='http://127.0.0.1:'+str(port)+'/'
    try:urllib.request.urlopen(url,timeout=5);checks['auth_required']=False
    except urllib.error.HTTPError as error:checks['auth_required']=error.code==401
    with urllib.request.urlopen(urllib.request.Request(url,headers={'Authorization':auth}),timeout=10) as response:
        checks['browser_page']=response.status==200 and b'app.js' in response.read()
    with urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:'+str(port)+'/api/bootstrap',headers={'Authorization':auth}),timeout=10) as response:
        bootstrap=json.load(response)
    payload=json.dumps({'command':'get_status','payload':{'compact':True}}).encode()
    with urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:'+str(port)+'/api/command',data=payload,headers={'Authorization':auth,'Content-Type':'application/json','X-MusicOps-CSRF':bootstrap['csrf']}),timeout=15) as response:
        checks['native_html_shared_commands']=json.load(response)['ok']
    checks['no_vnc_processes']=execute("from pathlib import Path;print(not any(b'websockify' in p.read_bytes() or b'x11vnc' in p.read_bytes() for p in Path('/proc').glob('[0-9]*/comm')))")=='True'
    checks['worker_ipc']=json.loads(execute("import json;from musicops.worker.client import WorkerClient;print(json.dumps(WorkerClient('/config/profile').call('get_status',{'compact':True})))")) is not None
    cfg=json.loads(execute("import json;from musicops.worker.client import WorkerClient;print(json.dumps(WorkerClient('/config/profile').call('attach',{'compact':True})['config']['values']))"))
    checks['sibling_media_paths']=cfg['paths']['library']=='/media/music' and cfg['paths']['source']=='/media/MusicOps/downloads'
    checks['ffmpeg_and_ffprobe']=all(cfg['paths']['tools'][key] for key in ('ffmpeg','ffprobe'))
    checks['encrypted_vault']=execute("from musicops.clients.vault import Vault;v=Vault('/config/profile');v.set('vault://ci/synthetic','synthetic-cookie-only','example.invalid');print(b'synthetic-cookie-only' not in v.path.read_bytes())")=='True'
    engines=docker('exec',name,'python','/opt/musicops/packaging/smoke_engines.py','--root','/opt/musicops/engines','--profile','/tmp/engine-acceptance','--report','/tmp/engine-acceptance.json')
    engine_report=json.loads(execute("from pathlib import Path;print(Path('/tmp/engine-acceptance.json').read_text())"))
    checks['both_engines_and_real_browser']=engine_report['passed'] and len(engine_report['checks'])==9
    docker('stop','--time','120',name)
    checks['clean_shutdown']=json.loads(docker('inspect',name))[0]['State']['ExitCode']==0
    docker('start',name);wait_healthy()
    checks['restart_decrypts_existing_vault']=execute("from musicops.clients.vault import Vault;print(Vault('/config/profile').get('vault://ci/synthetic','example.invalid')=='synthetic-cookie-only')")=='True'
    checks['web_password_preserved']=execute("from pathlib import Path;print(Path('/config/web-password').read_text().strip())")==password
    result={'passed':all(checks.values()),'checks':checks,'synthetic_only':True,'real_downloads':0,'real_account_login':False}
    report.write_text(json.dumps(result,indent=2));print(json.dumps(result));assert result['passed']
finally:
    logged=subprocess.run(['docker','logs',name],capture_output=True,text=True)
    logs=logged.stdout+logged.stderr
    (report.parent/'container.log').write_text(logs.replace(password,'[MASKED]'))
    subprocess.run(['docker','rm','-f',name],capture_output=True)
    for volume in volumes:subprocess.run(['docker','volume','rm',volume],capture_output=True)
