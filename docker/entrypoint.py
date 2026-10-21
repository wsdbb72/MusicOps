"""Unprivileged browser desktop; only the authenticated HTTP port is published."""
import os
from pathlib import Path
import re
import secrets
import signal
import subprocess
import sys
import time

def web_password(config):
    selected=os.environ.get('MUSICOPS_WEB_PASSWORD_FILE','')
    path=Path(selected) if selected else config/'web-password'
    if not path.exists() and not selected:
        fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        with os.fdopen(fd,'w') as stream:stream.write(secrets.token_urlsafe(24)+'\n')
        print('首次启动：网页登录用户 musicops，密码保存在 /config/web-password。',flush=True)
    value=path.read_text('utf-8').rstrip('\r\n')
    if len(value)<12 or len(value)>256 or any(ch in value for ch in '\r\n\x00'):
        raise ValueError('网页登录密码需要 12–256 个字符的单行文本')
    return value

def main():
    os.umask(0o077)
    config=Path('/config');home=Path(os.environ['HOME'])
    home.mkdir(parents=True,exist_ok=True)
    password=web_password(config)
    hashed=subprocess.run(['openssl','passwd','-apr1','-stdin'],input=password+'\n',capture_output=True,text=True,check=True).stdout.strip()
    runtime=Path('/tmp/musicops-web');runtime.mkdir(mode=0o700,exist_ok=True)
    (runtime/'htpasswd').write_text('musicops:'+hashed+'\n')
    (runtime/'nginx.conf').write_text('''
pid /tmp/musicops-web/nginx.pid;
error_log stderr warn;
events { worker_connections 128; }
http {
  access_log off;
  client_body_temp_path /tmp/musicops-web/body;
  proxy_temp_path /tmp/musicops-web/proxy;
  fastcgi_temp_path /tmp/musicops-web/fastcgi;
  uwsgi_temp_path /tmp/musicops-web/uwsgi;
  scgi_temp_path /tmp/musicops-web/scgi;
  server {
    listen 8080;
    auth_basic "MusicOps";
    auth_basic_user_file /tmp/musicops-web/htpasswd;
    location = /healthz { auth_basic off; return 200 "ok"; }
    location = / { return 302 /vnc.html?autoconnect=true&resize=remote; }
    location / {
      proxy_pass http://127.0.0.1:6080;
      proxy_http_version 1.1;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection "upgrade";
      proxy_set_header Host $host;
      proxy_read_timeout 86400s;
      proxy_buffering off;
    }
  }
}
''')
    subprocess.run(['nginx','-t','-p',str(runtime)+'/','-c',str(runtime/'nginx.conf')],check=True)
    children=[];stopping=False
    def stop(*_):
        nonlocal stopping
        stopping=True
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    def launch(name,args):
        process=subprocess.Popen(args,start_new_session=True)
        children.append((name,process));return process
    try:
        launch('display',['Xvfb',':99','-screen','0','1440x900x24','-nolisten','tcp'])
        for _ in range(100):
            if Path('/tmp/.X11-unix/X99').exists():break
            if children[0][1].poll() is not None:raise RuntimeError('虚拟显示启动失败')
            time.sleep(.1)
        else:raise RuntimeError('虚拟显示启动超时')
        launch('window-manager',['openbox','--sm-disable'])
        launch('vnc',['x11vnc','-display',':99','-forever','-shared','-localhost','-rfbport','5900','-nopw','-quiet','-noxdamage'])
        launch('websocket',['websockify','--web=/usr/share/novnc','127.0.0.1:6080','127.0.0.1:5900'])
        launch('http',['nginx','-p',str(runtime)+'/','-c',str(runtime/'nginx.conf'),'-g','daemon off;'])
        launch('MusicOps',[sys.executable,'-m','musicops.gui.main','--base','/config/profile'])
        print('MusicOps browser interface: port 8080; profile: /config/profile; library: /media/music',flush=True)
        while not stopping:
            exited=next(((name,p.returncode) for name,p in children if p.poll() is not None),None)
            if exited:
                print('Service exited: '+str(exited),flush=True)
                return 1
            time.sleep(.5)
        return 0
    finally:
        # Let the current durable operation reach a safe boundary before stopping.
        from musicops.worker.client import WorkerClient
        client=WorkerClient('/config/profile',timeout=3)
        deadline=time.monotonic()+95
        while time.monotonic()<deadline:
            try:client.call('shutdown_worker',{});break
            except (OSError,EOFError):break
            except Exception:time.sleep(.5)
        for _,process in reversed(children):
            if process.poll() is None:
                try:os.killpg(process.pid,signal.SIGTERM)
                except ProcessLookupError:pass
        for _,process in children:
            try:process.wait(5)
            except subprocess.TimeoutExpired:
                try:os.killpg(process.pid,signal.SIGKILL)
                except ProcessLookupError:pass

if __name__=='__main__':raise SystemExit(main())
