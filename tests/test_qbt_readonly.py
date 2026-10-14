import json
import httpx
import pytest
from musicops.clients.qbt import readonly_preflight

@pytest.mark.parametrize('login_status',[200,204])
def test_qbt_login_versions_and_only_read_api_calls(monkeypatch,login_status):
    calls=[]
    class Client:
        def __init__(self,**kwargs):assert kwargs['follow_redirects'] is False
        def __enter__(self):return self
        def __exit__(self,*args):pass
        def post(self,url,**kwargs):
            calls.append(('POST',url));return httpx.Response(login_status,text='Ok.' if login_status==200 else '')
        def get(self,url):
            calls.append(('GET',url))
            if url.endswith('app/version'):return httpx.Response(200,text='v5.2.2')
            if url.endswith('app/webapiVersion'):return httpx.Response(200,text='2.15.1')
            return httpx.Response(200,json=[{'state':'uploading'}] if url.endswith('torrents/info') else {'save_path':'E:/music'})
    monkeypatch.setattr(httpx,'Client',Client)
    out=readonly_preflight({'base_url':'http://127.0.0.1:8085','credential_ref':'vault://test'},lambda *_:json.dumps({'username':'virtual','password':'virtual'}))
    assert out['status']=='OK' and out['torrent_count']==1 and out['client_changes']==0
    assert [u.rsplit('/',2)[-2:] for method,u in calls]==[['auth','login'],['app','version'],['app','webapiVersion'],['app','preferences'],['torrents','info']]

def test_nonlocal_http_qbt_does_not_read_secret_or_network():
    out=readonly_preflight({'base_url':'http://example.invalid:8085'},lambda *_:pytest.fail('不应读取凭据'))
    assert out['status']=='POLICY_BLOCKED'
