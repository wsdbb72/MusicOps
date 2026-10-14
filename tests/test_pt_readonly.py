import copy,json,time
import pytest
from musicops.clients.http import FakeTransport,Response,PolicyBlocked
from musicops.clients.vault import import_cookies,cookie_header
from musicops.providers.pt_readonly import PTReadOnlyProvider
from musicops.app.service import AppService

def cookie(host='dicmusic.com',**extra):return {'domain':host,'path':'/','name':'session','value':'virtual-cookie',**extra}
def provider(responses,site='dicmusic'):
    host={'dicmusic':'dicmusic.com','orpheus':'orpheus.network','open.cd':'www.open.cd'}[site]
    transport=FakeTransport(responses)
    return PTReadOnlyProvider(site,{'base_url':'https://'+host,'credential_ref':'vault://test'},transport,lambda ref,h:json.dumps([cookie(host)])),transport
def response(data):return Response(200,json.dumps(data).encode())
def group():return {'groupId':10,'groupName':'Example &amp; Test','artist':'Artist','groupYear':2000,'torrents':[{'torrentId':11,'format':'FLAC','encoding':'Lossless','seeders':3,'isFreeleech':True,'canUseToken':True}]}

def test_cookie_labeled_export_filtering_expiry_and_path():
    data=[cookie(),cookie('other.invalid'),cookie(name='expired',expirationDate=1),cookie(name='scoped',path='/ajax')]
    text='opencd:\n'+json.dumps(data)
    assert len(import_cookies(text,'dicmusic.com'))==3
    assert cookie_header(text,'dicmusic.com','/ajax.php',now=10)=='session=virtual-cookie'
    assert 'scoped=' in cookie_header(text,'dicmusic.com','/ajax/item',now=10)
    with pytest.raises(ValueError):import_cookies(json.dumps([cookie(value='bad\r\nheader')]),'dicmusic.com')

def test_gazelle_candidate_id_and_unknown_eligibility_no_download_requests():
    p,t=provider([response({'status':'success','response':{'currentPage':1,'pages':2,'results':[group()]}})])
    out=p.search('Artist');assert out['status']=='OK' and out['search_complete'] is False
    c=out['candidates'][0];assert c['resource_id']=='11' and c['group_id']=='10' and c['title']=='Example & Test'
    assert c['eligibility']['state']=='UNKNOWN' and c['estimated_cost'] is None
    assert p.fetch_asset(c)['status']=='POLICY_BLOCKED' and len(t.requests)==1
    assert t.requests[0]['params']['action']=='browse' and t.requests[0]['headers']['Cookie']=='session=virtual-cookie'

def test_actual_multiartist_array_preserved():
    g=group();g['torrents'][0]['artists']=[{'id':1,'name':'A &amp; B','aliasid':0},{'id':2,'name':'C','aliasid':0}]
    p,_=provider([response({'status':'success','response':{'results':[g],'pages':1}})])
    assert p.search('Artist')['candidates'][0]['artists']==['A & B','C']

@pytest.mark.parametrize('r,status',[(Response(401), 'AUTH_FAILED'),(Response(302,headers={'Location':'https://other.invalid'}),'ACCESS_BLOCKED'),(Response(200,b'<html>login</html>'),'ACCESS_BLOCKED'),(response({'status':'success','response':{}}),'PARSE_ERROR'),(response({'status':'failure'}),'API_CONTRACT_ERROR')])
def test_network_auth_and_shape_failures_never_become_empty_results(r,status):
    p,t=provider([r]);out=p.search('Artist');assert out['status']==status and not out['candidates']

def test_no_resource_only_for_valid_empty_schema():
    p,_=provider([response({'status':'success','response':{'results':[],'pages':1}})])
    assert p.search('Missing')['status']=='NO_RESOURCE'

def test_gazelle_preflight_returns_only_safe_stats():
    p,_=provider([response({'status':'success','response':{'id':1,'username':'private-name','authkey':'private-key','passkey':'private-pass','userstats':{'tokens':12}}})])
    out=p.preflight();assert out['authenticated'] and out['token_balance']==12
    assert 'private' not in json.dumps(out)

def test_nonmusic_standalone_rows_do_not_break_music_search():
    p,_=provider([response({'status':'success','response':{'pages':1,'results':[group(),{'groupId':'22','torrentId':23,'category':'E-Books','groupName':'Book'}]}})])
    out=p.search('Artist');assert out['status']=='OK' and len(out['candidates'])==1

def test_login_redirect_is_auth_failure_without_forwarding_cookie():
    p,t=provider([Response(302,headers={'Location':'https://www.open.cd/login.php?returnto=torrents.php'})],'open.cd')
    assert p.preflight()['status']=='AUTH_FAILED' and len(t.requests)==1

def test_nexus_nested_rows_and_no_resource_classification():
    raw=b'<html><a href="logout.php">logout</a><table><tr><td><table><tr><td><a href="details.php?id=15&amp;hit=1" title="Album">Album</a></td></tr></table></td><td>100 MiB</td></tr></table></html>'
    p,_=provider([Response(200,raw)],'open.cd');out=p.search('Album');assert out['status']=='OK'
    assert out['candidates'][0]['resource_id']=='15' and out['candidates'][0]['media']['size_display']=='100 MiB'
    p,_=provider([Response(200,b'<a href="logout.php">logout</a><div>layout changed</div>')],'open.cd')
    assert p.search('Album')['status']=='PARSE_ERROR'

def test_read_endpoint_and_origin_allowlists():
    p,t=provider([])
    with pytest.raises(PolicyBlocked):p._get('/download.php',{'id':'1','usetoken':'1'})
    with pytest.raises(PolicyBlocked):p._get('/ajax.php',{'action':'upload'})
    assert not t.requests
    with pytest.raises(PolicyBlocked):PTReadOnlyProvider('dicmusic',{'base_url':'https://other.invalid'},t,lambda *_:'secret')

def test_service_online_search_does_not_touch_music_root(tmp_path,monkeypatch):
    service=AppService(tmp_path);cfg=copy.deepcopy(service.config.current()['values']);cfg['offline']=False;cfg['paths']['library']='E:/music'
    p,_=provider([response({'status':'success','response':{'results':[group()],'pages':1}})])
    cfg['providers']['orpheus']['enabled']=False;cfg['providers']['open.cd']['enabled']=False
    monkeypatch.setattr(service,'_real_pt',lambda *_:p)
    monkeypatch.setattr(service,'_safe_path',lambda *_:pytest.fail('只读检索不得访问文件系统根'))
    try:
        out=service._execute('search',{'query':'Artist'},cfg);assert out['status']=='OK' and out['candidates'][0]['evidence_level']=='live_readonly'
    finally:service.stop.set();service.thread.join(2)

def test_offline_ai_queues_and_never_calls_network(tmp_path):
    service=AppService(tmp_path)
    try:
        accepted=service.dispatch('ai_plan',{'message':'分析'})
        assert accepted['accepted']
        end=time.monotonic()+3
        while time.monotonic()<end:
            task=service.store.get('tasks',accepted['task_id'])
            if task['state']=='blocked':break
            time.sleep(.02)
        assert task['state']=='blocked' and task['error_class']=='POLICY_BLOCKED'
    finally:service.stop.set();service.thread.join(2)

def test_real_mode_does_not_run_synthetic_file_workflows(tmp_path):
    service=AppService(tmp_path);cfg=copy.deepcopy(service.config.current()['values']);cfg['offline']=False
    try:
        for command in ('demo','submit_plan','refresh_playlist'):
            with pytest.raises(PolicyBlocked):service._execute(command,{},cfg)
        assert not (tmp_path/'demo').exists()
    finally:service.stop.set();service.thread.join(2)
