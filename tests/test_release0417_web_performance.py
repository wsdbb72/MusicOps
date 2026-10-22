import base64
import copy
import json
from pathlib import Path
import sqlite3
import threading
import time
from types import SimpleNamespace
import httpx
import pytest
from musicops.state.store import StateStore
from musicops.config.schema import default_config,upgrade_config,validate_config
from musicops.app.pipeline import Workflows
from musicops.web.server import Server
from test_workflows import service


def test_projection_raw_update_delete_and_reopen(tmp_path):
    store=StateStore(tmp_path/'state.sqlite')
    store.put('evidence','workflow:a',{'job_id':'a','stage':'queued','label':'专辑','created_at':'2026-01-01'})
    store.put('evidence','workflow:a',{'job_id':'a','stage':'searching','paused':True})
    with store.connection() as db:
        assert dict(db.execute('SELECT * FROM workflow_overview').fetchone())['paused']==1
        db.execute("UPDATE evidence SET payload=json_set(payload,'$.stage','completed') WHERE id='workflow:a'")
    assert Workflows(SimpleNamespace(store=store)).next_job() is None
    store=StateStore(store.path)
    with store.connection() as db:
        assert db.execute('SELECT stage FROM workflow_overview').fetchone()[0]=='completed'
        db.execute("DELETE FROM evidence WHERE id='workflow:a'")
        assert db.execute('SELECT count(*) FROM workflow_overview').fetchone()[0]==0


def test_projection_initial_migration_preserves_payloads(tmp_path):
    path=tmp_path/'legacy.sqlite'
    original=json.dumps({'job_id':'a','stage':'needs_review','prepared':[{'synthetic':'untouched'}]})
    with sqlite3.connect(path) as db:
        db.execute('CREATE TABLE evidence (id TEXT PRIMARY KEY,payload TEXT,updated_at TEXT)')
        db.execute('INSERT INTO evidence VALUES(?,?,?)',('workflow:a',original,'2026-01-01'))
    store=StateStore(path)
    with store.connection() as db:
        assert db.execute('SELECT payload FROM evidence').fetchone()[0]==original
        assert db.execute('SELECT stage FROM workflow_overview').fetchone()[0]=='needs_review'


def test_scheduler_uses_small_summary_and_respects_dependencies(tmp_path):
    store=StateStore(tmp_path/'state.sqlite');w=Workflows(SimpleNamespace(store=store))
    store.put_many('evidence',[(f'workflow:old{i}',{'job_id':f'old{i}','stage':'completed','task_type':'album','targets':['large-history']*200}) for i in range(300)])
    store.put('evidence','workflow:identify',{'job_id':'identify','stage':'needs_review'})
    store.put('evidence','workflow:search',{'job_id':'search','stage':'searching','identification_job_id':'identify','task_type':'search'})
    assert w.next_job() is None
    store.put('evidence','workflow:identify',{'job_id':'identify','stage':'completed'})
    assert w.next_job()['job_id']=='search'
    assert w.next_job(network_only=True)['job_id']=='search'


@pytest.fixture
def web(tmp_path):
    server=Server(('127.0.0.1',0),tmp_path,'synthetic-web-password')
    server.client=SimpleNamespace(base=tmp_path,call=lambda command,payload,request_id=None:{'command':command,'received':payload})
    thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
    with httpx.Client(base_url='http://127.0.0.1:'+str(server.server_port),trust_env=False) as client:
        yield client,server
    server.shutdown();server.server_close();thread.join()


def test_native_web_auth_static_and_csrf(web):
    client,server=web
    assert client.get('/healthz').status_code==200
    assert client.get('/').status_code==401
    auth=('musicops','synthetic-web-password')
    page=client.get('/',auth=auth)
    assert page.status_code==200 and 'app.js' in page.text and 'noVNC' not in page.text
    assert client.get('/app.js',auth=auth).status_code==200
    assert client.get('/../../vault',auth=auth).status_code==404
    csrf=client.get('/api/bootstrap',auth=auth).json()['csrf']
    payload={'command':'get_status','payload':{'compact':True}}
    assert client.post('/api/command',json=payload).status_code==401
    assert client.post('/api/command',auth=auth,json=payload).status_code==403
    headers={'X-MusicOps-CSRF':csrf}
    for _ in range(10):
        assert client.post('/api/command',auth=auth,json=payload,headers={**headers,'Origin':'https://attacker.invalid'}).status_code==403
    response=client.post('/api/command',auth=auth,json=payload,headers=headers)
    assert response.json()['result']['command']=='get_status'
    assert client.post('/api/command',auth=auth,json={'command':'shutdown_worker'},headers=headers).status_code==400


def test_new_platform_settings_upgrade_without_losing_secrets(tmp_path):
    cfg=default_config(tmp_path);del cfg['music_servers']['plex'];del cfg['streaming']['musicdl']
    cfg['lyrics']['primary']['auth_secret_ref']='vault://synthetic/existing'
    updated=upgrade_config(cfg,tmp_path)
    assert not updated['music_servers']['plex']['enabled']
    assert updated['lyrics']['primary']['auth_secret_ref']=='vault://synthetic/existing'
    updated['streaming']['order']=['netease','tidal','qobuz','deezer','audius','streetvoice']
    validate_config(updated)


def test_native_upload_stays_inside_profile_and_rejects_executables(web):
    client,server=web;auth=('musicops','synthetic-web-password')
    token=client.get('/api/bootstrap',auth=auth).json()['csrf']
    headers={'X-MusicOps-CSRF':token}
    payload={'kind':'playlist','filename':'../../outside.csv','content':base64.b64encode(b'title,artist\nFixture,Artist').decode()}
    response=client.post('/api/upload',auth=auth,headers=headers,json=payload)
    path=Path(response.json()['file_path'])
    assert path.is_relative_to(server.client.base/'work'/'web-uploads') and path.suffix=='.csv'
    assert path.read_bytes()==b'title,artist\nFixture,Artist'
    assert client.post('/api/upload',auth=auth,headers=headers,json={**payload,'filename':'bad.exe'}).status_code==400
    assert client.post('/api/upload',auth=auth,headers=headers,json={**payload,'kind':'cover','filename':'fake.png'}).status_code==400


def test_native_qr_url_is_rendered_locally_without_external_image_service(web):
    client,server=web;auth=('musicops','synthetic-web-password')
    token=client.get('/api/bootstrap',auth=auth).json()['csrf']
    server.client.call=lambda *args,**kwargs:{'url':'https://music.163.com/login?fixture=synthetic','session_id':'synthetic-session'}
    result=client.post('/api/command',auth=auth,headers={'X-MusicOps-CSRF':token},json={'command':'engine_control','payload':{'action':'create','source':'netease'}}).json()['result']
    assert result['image'].startswith('data:image/png;base64,')
    assert base64.b64decode(result['image'].split(',',1)[1]).startswith(b'\x89PNG')


def test_changed_platform_cookie_is_applied_without_restarting_engine(tmp_path,monkeypatch):
    from musicops.clients.managed_services import ManagedServices
    class Vault:
        def get(self,ref,host):
            if ref=='vault://engine/netease':return 'MUSIC_U=synthetic-new'
            raise KeyError(ref)
    engine=ManagedServices(tmp_path,Vault());process=SimpleNamespace(poll=lambda:None)
    engine.running['go-music-dl']=(process,'http://127.0.0.1:12345','synthetic-service-token')
    calls=[]
    class Client:
        def __init__(self,**kwargs):assert kwargs['trust_env'] is False
        def __enter__(self):return self
        def __exit__(self,*a):pass
        def post(self,url,**kwargs):calls.append((url,kwargs));return SimpleNamespace(status_code=200)
    monkeypatch.setattr('musicops.clients.managed_services.httpx.Client',Client)
    engine.credentials_changed()
    assert engine.ensure('go-music-dl')==('http://127.0.0.1:12345','synthetic-service-token')
    engine.ensure('go-music-dl')
    assert len(calls)==1 and calls[0][1]['json']['netease']=='MUSIC_U=synthetic-new'
    assert engine.running['go-music-dl'][0] is process
    engine.close()


def test_plex_only_music_sections_and_token_header(monkeypatch):
    from musicops.clients.plex import sections,scan
    seen=[]
    class Client:
        def __init__(self,**kwargs):assert kwargs['follow_redirects'] is False
        def __enter__(self):return self
        def __exit__(self,*args):pass
        def get(self,url,headers):
            seen.append((url,headers));assert 'synthetic-token' not in url
            return SimpleNamespace(status_code=200,content=b'<MediaContainer><Directory key="2" type="artist" title="Music"><Location path="/music"/></Directory><Directory key="3" type="movie" title="Films"/></MediaContainer>')
    monkeypatch.setattr('musicops.clients.plex.httpx.Client',Client)
    cfg={'enabled':True,'url':'http://192.168.1.2:32400','section_id':'2','auth_secret_ref':'vault://plex/synthetic'}
    result=sections(cfg,lambda *args:'synthetic-token');assert [r['id'] for r in result['sections']]==['2']
    assert scan(cfg,lambda *args:'synthetic-token')['state']=='requested'
    assert seen[-1][0].endswith('/library/sections/2/refresh')
    with pytest.raises(Exception,match='不是可访问的音乐库'):scan({**cfg,'section_id':'3'},lambda *args:'synthetic-token')


def test_lrc_extra_metadata_normalized_without_global_poison(tmp_path):
    from musicops.providers.lrcapi import normalize_lrc
    from musicops.media.resources import validate_lrc
    value='[ar:艺人]\n[hash:abc]\n[qq:]\n[sign:x]\n[total:30000]\n[offset:0]\n[00:01.00]合成歌词'
    normalized=normalize_lrc(value)
    assert validate_lrc(normalized)['valid']
    assert '[offset:0]' in normalized and '[ar:艺人]' in normalized
    assert '[unknown:keep]' in normalize_lrc('[unknown:keep]\n[00:01.00]synthetic')


def test_cookie_share_reuses_official_hosts_not_api_tokens(tmp_path):
    from musicops.app.platform_credentials import share
    from musicops.config.schema import ConfigManager
    from musicops.clients.vault import Vault
    store=StateStore(tmp_path/'state.sqlite');service=SimpleNamespace(config=ConfigManager(store,tmp_path),vault=Vault(tmp_path))
    assert share(service,'netease','MUSIC_U=synthetic-cookie',from_engine=True)
    cfg=service.config.current()['values'];ref=cfg['netease']['credential_ref']
    assert cfg['providers']['netease']['credential_ref']==ref
    assert 'synthetic-cookie' in service.vault.get(ref,'music.163.com')
    assert share(service,'applemusic','developer-jwt',from_engine=True) is False
    with pytest.raises(Exception):service.vault.get(ref,'attacker.invalid')


def test_selected_album_fill_does_not_queue_the_entire_library(service):
    from test_workflows import flac
    from mutagen.flac import FLAC
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    one=flac(root/'One/song.flac');two=flac(root/'Two/song.flac')
    tags=FLAC(two);tags['album']=['Another Album'];tags.save()
    service._index_real_library({'root':str(root),'lightweight':True},cfg)
    albums=service.dispatch('list_library_albums')['albums']
    album=next(a for a in albums if a['album']=='Album')
    result=service.workflows.start({'scene':'fill','album_id':album['album_id']},cfg)
    assert result['album_count']==1
    rows=[j for j in service.workflows.rows() if j.get('scene')=='fill']
    assert len(rows)==1 and {r['path'] for r in rows[0]['old_files']}=={str(one)}
    assert two.is_file() and not service.store.list('download_tasks')


def test_disabling_netease_does_not_disable_other_streaming_sources(tmp_path):
    from musicops.app.streaming_fallback import available,effective_order
    cfg=default_config(tmp_path);cfg['workflow']['netease_fallback']=False
    cfg['streaming'].update(enabled=True,order=['netease','qqmusic','tidal'])
    assert available(cfg) and effective_order(cfg)==['qqmusic']
    cfg['streaming']['musicdl']['enabled']=True
    assert effective_order(cfg)==['qqmusic','tidal']
    cfg['streaming']['enabled']=False
    assert not available(cfg)


def test_old_profile_config_manager_migration_does_not_require_plex_key(tmp_path):
    from musicops.config.schema import ConfigManager
    store=StateStore(tmp_path/'state.sqlite');cfg=default_config(tmp_path)
    del cfg['music_servers']['plex'];del cfg['streaming']['musicdl']
    store.put('config_versions','1',{'version':1,'values':cfg,'hash':'legacy-hash'})
    manager=ConfigManager(store,tmp_path)
    assert manager.current()['values']['music_servers']['plex']['enabled'] is False
    assert not manager.current()['values']['streaming']['musicdl']['enabled']


@pytest.mark.parametrize('count,status',[(0,'NO_MATCH'),(1,'OK'),(2,'AMBIGUOUS')])
def test_external_musicdl_original_runner_uses_documented_api_and_keeps_identity(tmp_path,monkeypatch,count,status):
    import io
    import sys
    from musicops.providers.musicdl_runner import main
    called=[];output=tmp_path/'fixture.flac'
    class Client:
        def __init__(self,**kwargs):
            assert kwargs['music_sources']==['QobuzMusicClient']
            assert kwargs['clients_threadings']=={'QobuzMusicClient':1}
        def search(self,keyword):
            return {'QobuzMusicClient':[SimpleNamespace(song_name='Fixture',singers='AC/DC',album='Album',duration_s=180) for _ in range(count)]}
        def download(self,rows):
            called.append(rows);output.write_bytes(b'synthetic-contract-only-not-real-audio')
            return [SimpleNamespace(save_path=str(output))]
    monkeypatch.setitem(sys.modules,'musicdl',SimpleNamespace(musicdl=SimpleNamespace(MusicClient=Client)))
    monkeypatch.setattr(sys,'stdin',io.StringIO(json.dumps({'client':'QobuzMusicClient','directory':str(tmp_path),'target':{'raw_title':'Fixture','artists':['AC/DC'],'album':'Album','duration':180}})))
    main();receipt=json.loads((tmp_path/'receipt.json').read_text())
    assert receipt['status']==status and len(called)==(count==1)
    if status=='OK':assert receipt['artist']==['AC/DC'] and Path(receipt['path']).is_relative_to(tmp_path)
