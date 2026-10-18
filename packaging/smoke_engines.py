"""Run both real engines, a local browser fixture, restart and encrypted-cookie checks."""
import argparse
import hashlib
import json
from pathlib import Path
import sys
import threading
import re
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import httpx
from musicops.clients.managed_services import ManagedServices
from musicops.clients.vault import Vault


def smoke(root, profile):
    from musicops.platforms import private_directory
    private_directory(profile)
    diagnostic=(profile/'engine-diagnostics.log').open('w+b')
    manager=ManagedServices(profile,Vault(profile),root=root,diagnostic_output=diagnostic)
    checks={}
    try:
        base,token=manager.ensure('go-music-dl')
        checks['go_native_health']=True
        with httpx.Client(trust_env=False,timeout=5) as client:
            checks['go_rejects_unauthenticated']=client.get(base+'/cookies').status_code==403
        value='synthetic_session=synthetic-managed-runtime-only'
        checks['cookie_encrypted']=manager.save_cookie('qq',value)['saved']
        checks['cookie_absent_from_engine_database']=all(value.encode() not in file.read_bytes() for file in (profile/'services').rglob('*') if file.is_file())
        manager.close()
        base,token=manager.ensure('go-music-dl')
        checks['cookie_restored_after_restart']=manager.request('GET','/cookies').get('qq')==value
        base,token=manager.ensure('flaresolverr')
        with httpx.Client(trust_env=False,timeout=5) as client:
            checks['flare_native_browser_health']=client.get(base+'/health',headers={'X-MusicOps-Service':token}).status_code==200
            checks['flare_rejects_unauthenticated']=client.get(base+'/health').status_code==403
        class Page(BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200);self.send_header('Content-Type','text/html');self.end_headers();self.wfile.write(b'<html><body>MusicOps synthetic browser acceptance</body></html>')
            def log_message(self,*args):pass
        server=ThreadingHTTPServer(('127.0.0.1',0),Page);thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            with httpx.Client(trust_env=False,timeout=65) as client:
                result=client.post(base+'/v1',headers={'X-MusicOps-Service':token},json={'cmd':'request.get','url':f'http://127.0.0.1:{server.server_port}/','maxTimeout':45000}).json()
            checks['flare_real_local_page_render']=result.get('status')=='ok' and 'MusicOps synthetic browser acceptance' in result.get('solution',{}).get('response','')
        finally:server.shutdown();server.server_close();thread.join(5)
    except Exception:
        diagnostic.flush();diagnostic.seek(0)
        text=diagnostic.read().decode('utf-8',errors='replace')[-14000:]
        text=re.sub(r'\b[0-9a-f]{64}\b','[TRANSIENT TOKEN OR HASH]',text,flags=re.I)
        print('Native engine diagnostic tail:\n'+text,flush=True)
        raise
    finally:
        processes=[entry[0] for entry in manager.running.values()]
        manager.close()
        checks['owned_services_stopped']=all(process.poll() is not None for process in processes)
        diagnostic.close()
    return {'checks':checks,'passed':all(checks.values()),'real_downloads':0,'real_account_login':False}


if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('--root',type=Path,required=True);parser.add_argument('--profile',type=Path,required=True);parser.add_argument('--report',type=Path,required=True)
    args=parser.parse_args();report=smoke(args.root.resolve(),args.profile.resolve());args.report.parent.mkdir(parents=True,exist_ok=True);args.report.write_text(json.dumps(report,indent=2),'utf-8')
    print(json.dumps(report));raise SystemExit(0 if report['passed'] else 1)
