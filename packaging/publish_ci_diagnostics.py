"""Keep native build errors readable even when Actions artifact upload stalls."""
import json
import os
from pathlib import Path
import subprocess
from urllib.request import Request, urlopen

log=Path('dist/macos/reports/native-build.log')
tail=log.read_text('utf-8',errors='replace')[-12000:] if log.is_file() else 'The native build did not produce a log.'
for name,value in os.environ.items():
    if len(value)>=8 and any(word in name.upper() for word in ('TOKEN','PASSWORD','SECRET')):
        tail=tail.replace(value,'[REDACTED]')
outcome=os.environ.get('BUILD_OUTCOME','failure')
conclusion='success' if outcome=='success' else 'cancelled' if outcome=='cancelled' else 'failure'
commit=subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
payload={'name':'macOS build diagnostics','head_sha':commit,'status':'completed','conclusion':conclusion,
    'output':{'title':'Native macOS build: '+outcome,'summary':'Native build output (tail):\n\n```text\n'+tail.replace('```',"'''")+'\n```'}}
request=Request('https://api.github.com/repos/'+os.environ['GITHUB_REPOSITORY']+'/check-runs',
    data=json.dumps(payload).encode(),method='POST',headers={'Authorization':'Bearer '+os.environ['GH_TOKEN'],
        'Accept':'application/vnd.github+json','Content-Type':'application/json','X-GitHub-Api-Version':'2022-11-28'})
with urlopen(request,timeout=30) as response:result=json.load(response)
print('Native diagnostic check: '+result['html_url'])
