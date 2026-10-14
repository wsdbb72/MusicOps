
import copy, json
import pytest
from musicops.config.schema import default_config, validate_config
from musicops.providers.ai import AIProvider
from musicops.clients.http import Response

def real_values(tmp_path):
    c=default_config(tmp_path/'profile'); c['offline']=False
    c['music_servers']['roon']['enabled']=True;c['music_servers']['roon']['library']='E:/music'
    c['providers']['dicmusic']['credential_ref']='vault://pt/dicmusic/cookies'
    c['providers']['orpheus']['credential_ref']='vault://pt/orpheus/cookies'
    c['providers']['open.cd']['credential_ref']='vault://pt/open.cd/cookies'
    c['ai'].update(enabled=True,base_url='https://ai.example.invalid/v1',model='fixture-model');c['qbt']['base_url']='http://127.0.0.1:8085'
    return c

def test_enabled_music_server_requires_path_but_online_does_not_require_pt_cookies(tmp_path):
    c=real_values(tmp_path); validate_config(c); assert c['ai']['model']=='fixture-model'
    bad=copy.deepcopy(c);bad['music_servers']['roon']['library']='';bad['music_servers']['navidrome']['library']=''
    with pytest.raises(ValueError):validate_config(bad)
    bad=copy.deepcopy(c);bad['providers']['orpheus']['credential_ref']=''
    validate_config(bad);assert bad['offline'] is False

def test_ai_responses_protocol_bearer_and_strict_schema(tmp_path):
    c=real_values(tmp_path)['ai']; captured={}
    class T:
        def request(self,method,url,params=None,headers=None,content=None,timeout=None):
            captured.update(method=method,url=url,headers=headers,payload=json.loads(content))
            return Response(200,json.dumps({'status':'completed','output':[{'content':[{'type':'output_text','text':json.dumps({'intent':'analyze','proposed_actions':[],'requires_review':True,'explanation':'ok'})}]}]}).encode(),{},url)
    p=AIProvider(c,T(),lambda ref,host:'virtual-key')
    result=p.plan('看看缺口',{})
    assert captured['url'].endswith('/v1/responses') and captured['headers']['Authorization']=='Bearer virtual-key'
    assert captured['payload']['model']=='fixture-model' and captured['payload']['text']['format']['strict'] is True
    assert result['requires_review'] is True

def test_ai_rejects_invalid_plans_and_handles_endpoint_url(tmp_path):
    cfg=real_values(tmp_path)['ai'];cfg['base_url']+='/'+'responses'
    class T:
        def request(self,method,url,**kwargs):
            assert url.count('/responses')==1
            return Response(200,json.dumps({'status':'completed','output':[{'content':[{'type':'output_text','text':'{"intent":"delete","requires_review":false}'}]}]}).encode())
    with pytest.raises(ValueError,match='AI_INVALID_PLAN'):AIProvider(cfg,T(),lambda *_:'virtual').plan('test',{})

def test_missing_ai_key_is_auth_required(tmp_path):
    from musicops.clients.http import FakeTransport,HttpError
    transport=FakeTransport([])
    with pytest.raises(HttpError,match='AI密钥未配置'):AIProvider(real_values(tmp_path)['ai'],transport,lambda *_:None).analyze({})
    assert not transport.requests

def test_ai_large_context_is_rejected_before_network(tmp_path):
    from musicops.clients.http import FakeTransport,HttpError
    transport=FakeTransport([])
    with pytest.raises(HttpError,match='AI上下文超过'):AIProvider(real_values(tmp_path)['ai'],transport,lambda *_:'virtual-key').analyze({'text':'x'*140000})
    assert not transport.requests

def test_service_ai_plan_passes_bounded_context_and_explicit_total(tmp_path,monkeypatch):
    from musicops.app.service import AppService
    s=AppService(tmp_path);cfg=real_values(tmp_path);captured={}
    for i in range(60):s.store.put('target_tracks',str(i),{'target_id':str(i)})
    monkeypatch.setattr(AIProvider,'plan',lambda self,message,state:captured.update(state=state) or {'requires_review':True})
    try:
        s._execute('ai_plan',{'message':'analyze'},cfg)
        assert captured['state']['target_count']==60 and len(captured['state']['targets'])==50 and captured['state']['targets_truncated'] is True
    finally:s.stop.set();s.thread.join(2)
