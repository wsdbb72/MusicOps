"""Unprivileged native HTML interface and durable worker supervisor."""
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
    children=[];stopping=False
    def stop(*_):
        nonlocal stopping
        stopping=True
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    def launch(name,args):
        process=subprocess.Popen(args,start_new_session=True)
        children.append((name,process));return process
    try:
        launch('worker',[sys.executable,'-m','musicops.worker.server','--base','/config/profile'])
        launch('http',[sys.executable,'-m','musicops.web.server','--base','/config/profile','--port','8080','--password-file',str(Path(os.environ.get('MUSICOPS_WEB_PASSWORD_FILE','/config/web-password')))])
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
        # A paused operation waits inside its checkpoint and remains active.
        # Let it finish instead, then catch the boundary between queue items.
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
