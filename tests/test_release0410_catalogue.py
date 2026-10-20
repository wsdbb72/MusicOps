import copy
from pathlib import Path
import pytest
from musicops.library.catalog import AlbumCatalog
from musicops.library.index import index_library
from musicops.state.store import StateStore
from test_local_album_catalog import record,target
from test_workflows import service,job,until_terminal,flac


def test_targeted_refresh_changes_one_album_and_keeps_other_tracks_and_roots(tmp_path):
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');catalog=AlbumCatalog(store)
    a=record(root,store,'Artist/Album/One.flac');b=record(root,store,'Artist/Album/Two.flac')
    other=record(root,store,'Other/Other.flac',album='Other');catalog.sync(root,[a,b,other])
    foreign=record(tmp_path/'separate',store,'One.flac');catalog.sync(tmp_path/'separate',[foreign])
    changed=copy.deepcopy(a);changed['album']='Corrected';fresh=record(root,store,'Artist/Album/Three.flac',album='Corrected')
    result=catalog.refresh_files(root,[changed,fresh])
    assert (result['track_count'],result['album_count'],result['matched_track_count'])==(4,3,4)
    assert catalog.matched_targets(root,[target(a)])==set()
    assert catalog.matched_targets(root,[target(changed,'a'),target(b,'b'),target(fresh,'new'),target(other,'other')])=={'a','b','new','other'}
    assert catalog.summary(tmp_path/'separate')['track_count']==1


def test_refresh_removes_retired_album_without_removing_other_library_rows(tmp_path):
    root=tmp_path/'music';store=StateStore(tmp_path/'state.sqlite');catalog=AlbumCatalog(store)
    old=record(root,store,'Old/One.flac');other=record(root,store,'Other/Two.flac');catalog.sync(root,[old,other])
    old['location_state']='quarantine';store.put('library_files',old['file_id'],old)
    result=catalog.refresh_files(root,[],affected_directories=[Path(old['path']).parent])
    assert (result['track_count'],result['album_count'])==(1,1)
    assert not catalog.matched_targets(root,[target(old)])
    assert catalog.matched_targets(root,[target(other)])=={'selected'}


def test_selected_metadata_index_does_not_visit_unrelated_subtrees(tmp_path,monkeypatch):
    from types import SimpleNamespace
    import mutagen
    root=tmp_path/'music';picked=root/'Artist/Album/One.flac';picked.parent.mkdir(parents=True);picked.write_bytes(b'fixture')
    unselected=root/'Unrelated/Other.flac';unselected.parent.mkdir();unselected.write_bytes(b'fixture')
    calls=[]
    def read(path,*args,**kwargs):
        calls.append(Path(path));assert Path(path)==picked
        return SimpleNamespace(tags={'title':['One'],'album':['Album'],'artist':['Artist']},info=SimpleNamespace(length=120),pictures=[])
    monkeypatch.setattr(mutagen,'File',read)
    rows=index_library(root,metadata_only=True,selected_paths=[picked])
    assert len(rows)==1 and calls==[picked] and rows[0]['album']=='Album'
    with pytest.raises(Exception):index_library(root,metadata_only=True,selected_paths=[tmp_path/'outside.flac'])


def test_published_song_is_immediately_usable_by_playlist_album_catalogue(service):
    source=Path(service.config.current()['values']['paths']['source'])/'fixture'
    from mutagen.flac import FLAC
    flac(source/'one.flac');second=flac(source/'two.flac',title='Two')
    tags=FLAC(second);tags['tracknumber']=['2'];tags.save()
    pending=job(service,sources=source);until_terminal(service)
    completed=service.store.get('evidence','workflow:'+pending['job_id']);assert completed['stage']=='completed'
    catalog=AlbumCatalog(service.store);albums=catalog.albums(completed['config']['paths']['library'])
    assert albums['total']==1 and albums['albums'][0]['state']=='local_tags_matched'
    targets=[{**r,'target_id':str(i)} for i,r in enumerate(completed['prepared'])]
    assert catalog.matched_targets(completed['config']['paths']['library'],targets)=={'0','1'}


def test_existing_directory_casing_and_idempotent_verified_publication(service):
    from mutagen.flac import FLAC
    cfg=service.config.current()['values'];library=Path(cfg['paths']['library'])
    (library/'DJ Okawari'/'Album').mkdir(parents=True,exist_ok=True)
    source1=Path(cfg['paths']['source'])/'fixture1'
    track1=flac(source1/'one.flac',title='Flower Dance')
    tags=FLAC(track1);tags['artist']=['DJ OKAWARI'];tags['albumartist']=['DJ OKAWARI'];tags.save()
    track1.with_suffix('.lrc').write_text('[00:00.00]Flower Dance\n[00:01.00]End',encoding='utf-8')
    target_row={'target_id':'t1','raw_title':'Flower Dance','artists':['DJ OKAWARI'],'album':'Album','duration':2.0}
    first=job(service,sources=source1,targets=[target_row]);until_terminal(service)
    done1=service.store.get('evidence','workflow:'+first['job_id'])
    assert done1['stage']=='completed'
    published_audio=library/'DJ Okawari'/'Album'/'01 Flower Dance.flac'
    assert published_audio.is_file() and published_audio.with_suffix('.lrc').is_file()
    source2=Path(cfg['paths']['source'])/'fixture2';source2.mkdir(parents=True,exist_ok=True)
    import shutil
    shutil.copy2(track1,source2/'one.flac');shutil.copy2(track1.with_suffix('.lrc'),source2/'one.lrc')
    second={**first,'job_id':'test_job_2','source_path':str(source2),'stage':'metadata','publication_plan':None,'prepared':None,'prepared_root':None,'engine_paths':None,'temporary_cleanup':None,'temporary_cleanup_done':False}
    service.workflows.put(second)
    for _ in range(8):
        service.workflows.advance()
        done2=service.store.get('evidence','workflow:test_job_2')
        if done2['stage'] in ('completed','needs_review','failed','skipped'):break
    assert done2['stage']=='completed' and done2.get('temporary_cleanup_done') is True
    assert not list(Path(done2['prepared_root']).glob('*'))
    assert AlbumCatalog(service.store).summary(library)['track_count']==1

