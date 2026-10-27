"""Provider labels never replace artist credits; tests use only temporary data."""
import copy,hashlib,json,time
from pathlib import Path
from types import SimpleNamespace
import pytest
from mutagen.flac import FLAC
from musicops.media.source_labels import album_artist_credits,effective_album_artists,normalize_album_artist,album_label
from musicops.library.index import index_library,METADATA_INDEX_VERSION
from musicops.library.catalog import AlbumCatalog
from musicops.state.store import StateStore
from musicops.app.source_cleanup import run,normalize_job,KEY
from musicops.domain.identity import release_identity,recording_identity,identities_same
from musicops.config.schema import default_config,digest
from test_workflows import service,flac,job,until_terminal
from test_local_album_catalog import record


@pytest.fixture(autouse=True)
def isolated_environment(monkeypatch,tmp_path):
    monkeypatch.delenv('MUSICOPS_CONTAINER',raising=False)
    monkeypatch.setenv('MUSICOPS_VAULT_KEY_FILE',str(tmp_path/'synthetic-vault.key'))


@pytest.mark.parametrize('credit,expected',[
    ('Qobuz UK',[]),('Qobuz\u00a0UK',[]),('[qobuz uk]',[]),('Tidal US',[]),('Deezer FR',[]),
    ('Apple Music JP',[]),('Amazon Music DE',[]),('Spotify GB',[]),
    ('Artist · Qobuz UK',['Artist']),('Artist | Qobuz UK',['Artist'])])
def test_source_credit_is_not_an_album_artist(credit,expected):
    row={'albumartist':[credit],'artists':['Artist'],'album':'Album'}
    assert normalize_album_artist(row) and row['albumartist']==expected
    assert row['raw_albumartist']==[credit] and row['source_labels'][0]['field']=='albumartist'
    assert effective_album_artists(row)==['Artist'] and album_label(row)=='Album · Artist'


@pytest.mark.parametrize('credit',['Qobuz','Tidal','UK Subs','The Qobuz Quartet','Artist · Live','Artist - Deluxe','Artist (Remastered)','Various Artists'])
def test_regular_credits_and_versions_are_preserved(credit):
    row={'albumartist':[credit],'album':'Concert · Qobuz UK','artists':['Artist']}
    assert not normalize_album_artist(row) and album_artist_credits([credit])==[credit]
    assert row['album']=='Concert · Qobuz UK'


def test_mixed_credits_keep_actual_album_artist_and_track_identity():
    row={'albumartist':['Album Artist','Qobuz UK'],'artists':['Track Artist'],'raw_title':'Song','album':'Album','duration':123}
    assert effective_album_artists(row)==['Album Artist']
    assert release_identity(row)['release_artist']==['album artist']
    assert recording_identity(row)['artist_identity']==['track artist']
    assert not identities_same(row,{**row,'recording_version':'Live'})


def test_metadata_cache_v2_upgrades_without_rereading_audio(tmp_path,monkeypatch):
    import mutagen
    path=tmp_path/'music/Artist/Album/song.flac';path.parent.mkdir(parents=True);path.write_bytes(b'synthetic')
    monkeypatch.setattr(mutagen,'File',lambda *args,**kwargs:SimpleNamespace(tags={'title':['Song'],'artist':['Artist'],'album':['Album'],'albumartist':['Qobuz UK']},info=SimpleNamespace(length=123),pictures=[]))
    row=index_library(tmp_path/'music',metadata_only=True)[0]
    assert not row['read_error'] and row['albumartist']==[] and row['raw_albumartist']==['Qobuz UK']
    assert row['raw_tags']['albumartist']==['Qobuz UK']
    previous=copy.deepcopy(row);previous['metadata_index_version']=2;previous['albumartist']=['Qobuz UK'];previous.pop('source_labels');previous.pop('raw_albumartist')
    monkeypatch.setattr(mutagen,'File',lambda *args,**kwargs:pytest.fail('unchanged v2 cache must not reread audio'))
    fresh=index_library(tmp_path/'music',metadata_only=True,cached={previous['file_id']:previous})[0]
    assert fresh['metadata_index_version']==METADATA_INDEX_VERSION and fresh['albumartist']==[] and fresh['source_labels']
    assert previous['albumartist']==['Qobuz UK']


def test_repair_writes_real_artist_to_library_copy_and_retains_seed_source(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'source';path=flac(source/'song.flac')
    tags=FLAC(path);tags['albumartist']=['Qobuz UK'];tags.save();before=path.read_bytes()
    job(service,sources=source);finished=until_terminal(service)
    assert finished['stage']=='completed',finished
    assert path.read_bytes()==before
    output=next(Path(cfg['paths']['library']).rglob('*.flac'))
    assert FLAC(output)['albumartist']==['Artist']
    assert finished['prepared'][0]['source_labels'][0]['provider']=='qobuz'


def test_source_artist_forces_existing_album_repair_despite_positive_resource_cache(service):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library']);path=flac(root/'Album/song.flac')
    tags=FLAC(path);tags['albumartist']=['Qobuz UK'];tags['lyrics']=['[00:00.00]one\n[00:01.00]two'];tags.save()
    path.with_suffix('.lrc').write_text('[00:00.00]one\n[00:01.00]two','utf-8');before=hashlib.sha256(path.read_bytes()).hexdigest()
    row=index_library(root,metadata_only=True)[0]
    service.store.put('evidence','resources-checked:'+row['file_id'],{'resource_contract':2,'version':[row['size'],row['mtime_ns']],'settings':digest(cfg['lyrics']),'checked_at':time.time()})
    result=service.workflows.start({'scene':'complete'},cfg);assert result['album_count']==1
    finished=until_terminal(service);assert finished['stage']=='completed',finished
    # The repaired root-level album also moves into its artist folder.
    path=root/'Artist'/'Album'/'song.flac';assert FLAC(path)['albumartist']==['Artist']
    transaction=service.store.get('transactions',finished['transaction_id'])
    assert any(item['before_hash']==before for item in transaction['backup_manifest'])


def test_cover_lookup_receives_real_artist(service,monkeypatch):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'cover';path=flac(source/'song.flac')
    tags=FLAC(path);tags['albumartist']=['Qobuz UK'];tags.clear_pictures();tags.save()
    monkeypatch.setattr(service.workflows,'_catalog_fill',lambda *args,**kwargs:None);calls=[]
    monkeypatch.setattr('musicops.providers.cover_download.cover',lambda album,artists,*args,**kwargs:calls.append((album,artists)) or None)
    job(service,sources=source);finished=until_terminal(service)
    assert finished['review_kind']=='cover' and calls==[('Album',['Artist'])]


def test_migration_updates_index_catalogue_and_jobs_without_audio_or_transaction_writes(tmp_path,monkeypatch):
    import mutagen
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');row=record(root,store,'Collection/Artist/Year/Concert/song.flac')
    row.update(albumartist=['Qobuz UK'],metadata_index_version=2);store.put('library_files',row['file_id'],row)
    outside=record(tmp_path/'other',store,'song.flac');outside['albumartist']=['Qobuz UK'];store.put('library_files',outside['file_id'],outside)
    catalog=AlbumCatalog(store);catalog.sync(root,[row])
    cfg=default_config(tmp_path/'profile');cfg['paths']['library']=str(root)
    pending={'kind':'workflow','job_id':'pending','scene':'complete','stage':'metadata','label':'Album · Qobuz UK','old_files':[copy.deepcopy(row)],'prepared':[]}
    store.put('evidence','workflow:pending',pending)
    prepared={'kind':'workflow','job_id':'prepared','scene':'complete','stage':'needs_review','review_kind':'cover','label':'Album · Qobuz UK','prepared':[{'album':'Album','artists':['Artist'],'tags':{'albumartist':['Qobuz UK']}}],'publication_plan':{'plan_id':'workflow:prepared','operations':[{'source_hash':'unchanged'}]}}
    store.put('evidence','workflow:prepared',prepared);transaction={'plan_id':'workflow:prepared','state':'VALIDATED','operations':[{'source_hash':'unchanged'}]};store.put('transactions','transaction',transaction)
    before=Path(row['path']).read_bytes();monkeypatch.setattr(mutagen,'File',lambda *args,**kwargs:pytest.fail('migration must not read audio tags'))
    service=SimpleNamespace(store=store,config=SimpleNamespace(current=lambda:{'values':cfg}))
    result=run(service)
    assert result['file_count']==1 and result['job_count']==2 and result['audio_writes']==0 and result['transaction_writes']==0
    assert store.get('library_files',row['file_id'])['albumartist']==[]
    assert store.get('library_files',outside['file_id'])['albumartist']==['Qobuz UK']
    assert catalog.albums(root)['albums'][0]['artists']==['Artist']
    assert store.get('evidence','workflow:pending')['label']=='Album · Artist'
    saved=store.get('evidence','workflow:prepared');assert saved['prepared'][0]['tags']==prepared['prepared'][0]['tags'] and saved['review_kind']=='cover'
    assert saved['publication_plan']==prepared['publication_plan'] and store.get('transactions','transaction')['operations']==transaction['operations']
    assert Path(row['path']).read_bytes()==before
    events=len(store.events());assert run(service)==result and len(store.events())==events


def test_job_cleanup_tolerates_partial_legacy_checkpoints():
    row={'albumartists':['Qobuz UK'],'prepared':None,'old_files':None,'targets':{},'label':'old'}
    assert normalize_job(row) and row['albumartists']==[] and row['raw_albumartists']==['Qobuz UK']


@pytest.mark.parametrize('transaction_state',[None,'VALIDATED','RECOVERY_REQUIRED'])
def test_prepared_source_repairs_reenrich_only_before_any_file_transaction(service,monkeypatch,transaction_state):
    row=job(service);plan={'plan_id':'workflow:'+row['job_id'],'operations':[{'source_hash':'original'}]}
    row.update(stage='running',resume_stage='publishing',publication_plan=copy.deepcopy(plan),prepared=[{'tags':{'albumartist':['Qobuz UK']}}])
    service.workflows.put(row)
    if transaction_state:service.store.put('transactions','existing',{'plan_id':plan['plan_id'],'state':transaction_state})
    monkeypatch.setattr('musicops.app.storage_migration.prepare',lambda *args:True);calls=[]
    monkeypatch.setattr(service.workflows,'_enrich',lambda job:calls.append('enrich'))
    monkeypatch.setattr(service.workflows,'_publishing',lambda job:calls.append('publishing'))
    service.workflows._run_stage(row,'publishing')
    assert calls==['publishing' if transaction_state else 'enrich']
    assert row['prepared'][0]['tags']['albumartist']==['Qobuz UK']
    if transaction_state:assert row['publication_plan']==plan
    else:assert row['publication_plan'] is None and row['source_repair_previous_plans']==[plan]


def test_interrupted_metadata_migration_finishes_catalogue_without_audio_reads(tmp_path):
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');row=record(root,store,'Artist/Album/song.flac')
    row['albumartist']=['Qobuz UK'];normalize_album_artist(row);store.put('library_files',row['file_id'],row)
    store.put('evidence',KEY,{'state':'planned','file_ids':[row['file_id']]})
    cfg=default_config(tmp_path/'profile');cfg['paths']['library']=str(root)
    result=run(SimpleNamespace(store=store,config=SimpleNamespace(current=lambda:{'values':cfg})))
    assert result['state']=='completed' and result['file_count']==1 and AlbumCatalog(store).albums(root)['albums'][0]['artists']==['Artist']


def test_frozen_migration_fixture_matches_real_service_startup(tmp_path):
    import importlib.util
    from musicops.app.service import AppService
    from musicops.config.schema import ConfigManager
    fixture_path=Path(__file__).resolve().parents[1]/'packaging/smoke_source_labels.py'
    spec=importlib.util.spec_from_file_location('source_fixture_contract',fixture_path);fixture=importlib.util.module_from_spec(spec);spec.loader.exec_module(fixture)
    base=tmp_path/'profile';store=StateStore(base/'state/musicops.sqlite');manager=ConfigManager(store,base,initial_offline=False)
    cfg=manager.current()['values'];library=Path(cfg['paths']['library']);library.mkdir(parents=True)
    fixture.seed(store,library);service=AppService(base,offline=False)
    try:
        assert all(fixture.check(SimpleNamespace(call=service.dispatch)).values())
        service._index_real_library({'root':str(library),'lightweight':True},service.config.current()['values'])
        assert service.dispatch('list_library_albums')['total']==0 and not list(library.rglob('*'))
    finally:service.stop.set();service.thread.join(3)
