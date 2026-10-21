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
        if not status['Running']:raise RuntimeError('Container exited: '+docker('logs',name))
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
    url='http://127.0.0.1:'+str(port)+'/vnc.html'
    try:urllib.request.urlopen(url,timeout=5);checks['auth_required']=False
    except urllib.error.HTTPError as error:checks['auth_required']=error.code==401
    with urllib.request.urlopen(urllib.request.Request(url,headers={'Authorization':auth}),timeout=10) as response:
        checks['browser_page']=response.status==200 and b'noVNC' in response.read()
    key=base64.b64encode(os.urandom(16)).decode()
    with socket.create_connection(('127.0.0.1',port),timeout=10) as connection:
        request=('GET /websockify HTTP/1.1\r\nHost: 127.0.0.1\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: '+key+'\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\nAuthorization: '+auth+'\r\n\r\n')
        connection.sendall(request.encode());data=b''
        while b'\r\n\r\n' not in data:data+=connection.recv(4096)
        expected=base64.b64encode(hashlib.sha1((key+'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').encode()).digest())
        checks['authenticated_desktop_websocket']=b'101 Switching Protocols' in data and expected in data
    checks['worker_ipc']=json.loads(execute("import json;from musicops.worker.client import WorkerClient;print(json.dumps(WorkerClient('/config/profile').call('get_status',{'compact':True})))")) is not None
    cfg=json.loads(execute("import json;from musicops.worker.client import WorkerClient;print(json.dumps(WorkerClient('/config/profile').call('attach',{'compact':True})['config']['values']))"))
    checks['sibling_media_paths']=cfg['paths']['library']=='/media/music' and cfg['paths']['source']=='/media/MusicOps/downloads'
    checks['ffmpeg_and_ffprobe']=all(cfg['paths']['tools'][key] for key in ('ffmpeg','ffprobe'))
    checks['encrypted_vault']=execute("from musicops.clients.vault import Vault;v=Vault('/config/profile');v.set('vault://ci/synthetic','synthetic-cookie-only','example.invalid');print(b'synthetic-cookie-only' not in v.path.read_bytes())")=='True'
    execute("from PySide6.QtWidgets import QApplication;app=QApplication([]);assert app.primaryScreen().grabWindow(0).save('/tmp/linux-desktop.png')")
    docker('cp',name+':/tmp/linux-desktop.png',str(report.parent/'linux-desktop.png'))
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
    logs=subprocess.run(['docker','logs',name],capture_output=True,text=True).stdout
    (report.parent/'container.log').write_text(logs.replace(password,'[MASKED]'))
    subprocess.run(['docker','rm','-f',name],capture_output=True)
    for volume in volumes:subprocess.run(['docker','volume','rm',volume],capture_output=True)
