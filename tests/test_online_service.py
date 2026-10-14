import copy,json,wave
from pathlib import Path
import pytest
from musicops.app.service import AppService
from musicops.clients.http import PolicyBlocked
from musicops.config.schema import ConfigManager,default_config,validate_config
from musicops.state.store import StateStore

@pytest.fixture
def service(tmp_path):
    s=AppService(tmp_path/'profile',offline=False)
    yield s
    s.stop.set();s.thread.join(3)

def test_formal_config_online_without_pt_roon_or_ai_credentials(tmp_path):
    cfg=default_config(tmp_path,offline=False);validate_config(cfg)
    assert cfg['offline'] is False and cfg['catalog']['enabled'] is True and cfg['providers']['netease']['enabled'] is True
    assert not cfg['ai']['enabled'] and all(not cfg['providers'][p]['enabled'] for p in ('dicmusic','orpheus','open.cd'))
    assert '.example.invalid' in cfg['lyrics']['primary']['base_url']

def test_config_migration_preserves_private_endpoints_and_mode(tmp_path):
    store=StateStore(tmp_path/'state.sqlite');old=default_config(tmp_path,offline=False);old.pop('catalog');old['lyrics']['primary']['base_url']='https://custom.private.invalid';old['lyrics']['primary']['auth_secret_ref']='vault://custom/key'
    for key in ('host','port','timeout'):old['music_servers']['roon'].pop(key)
    store.put('config_versions','1',{'version':1,'values':old})
    manager=ConfigManager(store,tmp_path,initial_offline=False);cfg=manager.current()
    assert cfg['version']==2 and cfg['values']['offline'] is False
    assert cfg['values']['lyrics']['primary']['base_url']=='https://custom.private.invalid' and cfg['values']['lyrics']['primary']['auth_secret_ref']=='vault://custom/key'
    assert cfg['values']['catalog']['enabled'] and cfg['values']['music_servers']['roon']['host']=='127.0.0.1'

def test_online_and_demo_modes_are_isolated(service,tmp_path):
    assert service.dispatch('enable_online')['config']['values']['offline'] is False
    with pytest.raises(PolicyBlocked,match='独立'):service.dispatch('enable_demo')
    other=AppService(tmp_path/'demo-profile',offline=False)
    try:assert other.dispatch('enable_demo')['config']['values']['offline'] is True
    finally:other.stop.set();other.thread.join(3)
    assert service.config.current()['values']['offline'] is False

def test_one_missing_cookie_does_not_disable_all_network(service):
    cfg=service.config.current();values=copy.deepcopy(cfg['values']);values['providers']['dicmusic']['enabled']=True
    saved=service.config.save(values,cfg['version']);assert saved['values']['offline'] is False
    assert service._execute('pt_preflight',{'site':'dicmusic'},saved['values'])['status']=='AUTH_REQUIRED'

def audio(path):
    path.parent.mkdir(parents=True,exist_ok=True)
    with wave.open(str(path),'wb') as f:f.setparams((1,2,44100,0,'NONE','not compressed'));f.writeframes(b'\x00'*4410)

def test_real_metadata_index_paginated_and_never_decodes(service,tmp_path,monkeypatch):
    root=tmp_path/'music';audio(root/'one.wav');cfg=service.config.current()['values']
    monkeypatch.setattr(service,'_validator',lambda *_:pytest.fail('metadata-only must not decode'))
    result=service._execute('index_library',{'root':str(root),'lightweight':True},cfg)
    assert result['count']==1 and result['metadata_only'] and result['music_writes']==0
    page=service.dispatch('list_library_files',{'query':'one','limit':200})
    assert page['total']==1 and page['files'][0]['sha256'] is None and page['files'][0]['library_root']==str(root)
    assert service.dispatch('list_library_files',{'query':'%','limit':200})['total']==0
    assert service.dispatch('get_status',{'compact':True})['library_file_count']==1

def test_library_metadata_does_not_claim_verified_or_roon_visibility(service):
    cfg=service.config.current()['values']
    service.store.put('target_tracks','target',{'target_id':'target','raw_title':'Song','artists':['Artist'],'album':'Album','duration':1,'snapshot_complete':True})
    service.store.put('library_files','file',{'file_id':'file','raw_title':'Song','artists':['Artist'],'album':'Album','duration':1,'validation':{'decode_state':'not_run'},'metadata_only':True})
    result=service._execute('compare',{},cfg)
    assert result['counts']=={'ambiguous':1} and result['roon_state']=='pending'

class Resolver:
    def __init__(self,candidate):self.candidate=candidate
    def identify(self,query):return {'status':'OK','candidates':[copy.deepcopy(self.candidate)],'sources':[],'lookup_links':[]}
    def resolve_candidate(self,source,source_id):
        return {'status':'OK','candidate':{'candidate_id':'different-provider-generated-id','source':source,'source_id':source_id,'entity_kind':'release','title':'Album','album':'Album','artists':['Artist'],'release_id':'release-fixture','tracklist_complete':True,'tracklist':[{'title':'Song','artists':['Artist'],'recording_id':'recording-fixture','duration':1}]}}

def setup_catalog(service,monkeypatch):
    target={'target_id':'target','source_song_id':'1234','raw_title':'Song','artists':['Artist'],'album':'','source_album_id':None,'duration':1,'snapshot_complete':False,'metadata_complete':False}
    service.store.put('target_tracks','target',target)
    candidate={'candidate_id':'musicbrainz:stable-recording-release','source':'musicbrainz','source_id':'release:fixture','recording_id':'recording-fixture','title':'Song','artists':['Artist'],'album':'Album'}
    monkeypatch.setattr(service,'_catalog_resolver',lambda _:Resolver(candidate))
    query={'target_id':'target','title':'Song','artists':['Artist'],'album':'','duration':1}
    return query,candidate

def test_catalog_accept_keeps_source_snapshot_separate_and_no_music_writes(service,monkeypatch):
    cfg=service.config.current()['values'];query,candidate=setup_catalog(service,monkeypatch)
    service._execute('identify_metadata',query,cfg)
    with pytest.raises(PolicyBlocked,match='完整'):service._execute('accept_metadata',{'target_id':'target','candidate_id':candidate['candidate_id']},cfg)
    detail=service._execute('resolve_metadata',{'candidate_id':candidate['candidate_id']},cfg)['candidate']
    assert detail['candidate_id']==candidate['candidate_id']
    result=service._execute('accept_metadata',{'target_id':'target','candidate_id':candidate['candidate_id']},cfg)
    assert result['music_writes']==0 and result['target']['album']=='Album'
    assert result['target']['source_metadata']['album']=='' and result['target']['snapshot_complete'] is False
    assert service.store.list('change_plans')==[]

def test_catalog_target_changed_after_query_requires_fresh_search(service,monkeypatch):
    cfg=service.config.current()['values'];query,candidate=setup_catalog(service,monkeypatch)
    service._execute('identify_metadata',query,cfg);service._execute('resolve_metadata',{'candidate_id':candidate['candidate_id']},cfg)
    target=service.store.get('target_tracks','target');target['raw_title']='Changed';service.store.put('target_tracks','target',target)
    with pytest.raises(PolicyBlocked,match='目标资料'):service._execute('accept_metadata',{'target_id':'target','candidate_id':candidate['candidate_id']},cfg)

def test_repeated_partial_source_refresh_preserves_user_resolution(service,monkeypatch):
    cfg=service.config.current()['values'];query,candidate=setup_catalog(service,monkeypatch)
    service._execute('identify_metadata',query,cfg);service._execute('resolve_metadata',{'candidate_id':candidate['candidate_id']},cfg)
    accepted=service._execute('accept_metadata',{'target_id':'target','candidate_id':candidate['candidate_id']},cfg)['target']
    refresh={**accepted['source_metadata'],'target_id':'target','source_song_id':'1234','snapshot_complete':False,'playlist_ids':['9876'],'snapshot_ids':['partial'],'snapshot_id':'partial'}
    service._persist_playlist_targets({'target':refresh},set())
    assert service.store.get('target_tracks','target')['album']=='Album'
    refresh['artists']=['Other'];service._persist_playlist_targets({'target':refresh},set())
    assert service.store.get('target_tracks','target')['metadata_resolution_state']=='needs_review_source_changed'

def test_roon_preflight_diagnostic_is_not_verified_library(service,monkeypatch):
    import musicops.clients.roon as roon
    monkeypatch.setattr(roon.RoonReadOnlyClient,'preflight',lambda _: {'status':'AUTH_REQUIRED','connected':True,'library_visibility':'AUTH_REQUIRED'})
    monkeypatch.setattr(roon,'inspect_local_logs',lambda **_: {'status':'OK','matching_storage_roots':['X:/fixture'],'historical_errors':1})
    result=service._execute('roon_preflight',{},service.config.current()['values'])
    assert result['connected'] and result['status']=='AUTH_REQUIRED'
    assert service.dispatch('get_status',{'compact':True})['roon']['library_visibility']=='AUTH_REQUIRED'

def test_portable_formal_launch_remembers_configured_profile_without_hardcoded_drive(tmp_path):
    from musicops.startup import default_profile,remember_profile
    data=tmp_path/'localappdata';profile=tmp_path/'configured';(profile/'state').mkdir(parents=True);(profile/'state/musicops.sqlite').write_bytes(b'fixture')
    remember_profile(profile,local_appdata=data)
    assert default_profile(executable=tmp_path/'portable/MusicOps.exe',local_appdata=data)==profile.resolve()
    (data/'MusicOps/active-profile.json').write_text('invalid')
    assert default_profile(executable=tmp_path/'portable/MusicOps.exe',local_appdata=data)==data/'MusicOps/profile'
