import copy,threading
from pathlib import Path
import pytest
from test_workflows import service,flac
from musicops.app.existing_album import offer,choose
from musicops.library.catalog import AlbumCatalog
from musicops.library.index import index_library,sha256_file
from musicops.media.tags import apply_tag_changes
from musicops.app.publication_cleanup import finish
from musicops.clients.http import PolicyBlocked


def test_existing_legacy_album_requires_choice_and_reuses_its_directory(service,tmp_path):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    old=flac(root/'Artist - Album'/'old.flac')
    rows=index_library(root,metadata_only=True)
    service.store.put_many('library_files',[(r['file_id'],r) for r in rows]);AlbumCatalog(service.store).sync(root,rows)
    incoming=copy.deepcopy(rows[0]);incoming['path']=str(tmp_path/'prepared.flac')
    job={'job_id':'reuse','scene':'playlist','config':cfg,'album':'Album','albumartists':['Artist'],'prepared':[incoming],'stage':'publishing'}
    assert offer(service.workflows,job) and job['review_kind']=='existing_album'
    assert old.exists()
    choose(service.workflows,job,'existing_album:'+job['existing_album_choices'][0]['album_id'])
    assert job['stage']=='publishing' and job['existing_album_directory']=='Artist - Album'
    assert job['existing_album_files'][0]['path']==str(old)
    assert not offer(service.workflows,job)


def test_changed_valid_publication_is_completed_note_but_all_sources_are_kept(service,tmp_path):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    final=flac(root/'Artist/Album/song.flac');expected=sha256_file(final)
    raw=tmp_path/'source.flac';raw.write_bytes(b'original')
    prepared=Path(cfg['paths']['work'])/'prepared/notes'
    prepared.mkdir(parents=True);copy=prepared/'song.flac';copy.write_bytes(final.read_bytes())
    job={'job_id':'notes','scene':'playlist','config':cfg,'prepared_root':str(prepared),'source_path':str(raw.parent),
         'prepared':[{'path':str(copy),'original_path':str(raw),'raw_title':'Song','artists':['Artist'],'duration':2}],
         'publication_plan':{'operations':[{'destination':'Artist/Album/song.flac','source_hash':expected}]}}
    apply_tag_changes(final,{'genre':['Changed by a player']})
    assert sha256_file(final)!=expected
    assert finish(service.workflows,job)
    assert job['cleanup_deferred'] and job['warnings'][0]['skip_reason']=='target_changed'
    assert raw.exists() and copy.exists()
    job['publication_plan']['operations'].append({'destination':'Artist/Album/missing.flac','source_hash':'none'})
    with pytest.raises(PolicyBlocked):finish(service.workflows,job)
    assert raw.exists() and copy.exists()


def test_heavy_wave_continues_refilling_network_lane(service,monkeypatch):
    from test_parallel_albums import seed
    flows=service.workflows;seed(service,9)
    template=copy.deepcopy(flows.rows()[0]);template.update(job_id='heavy',stage='publishing');flows.put(template)
    monkeypatch.setattr(flows,'parallel_limit',lambda:4)
    monkeypatch.setattr(service,'_checkpoint',lambda *_:None)
    # The fixture's scheduler is stopped, but the coordinator should run here.
    monkeypatch.setattr(service.stop,'is_set',lambda:False)
    seen=[];all_done=threading.Event()
    def identify(job):
        seen.append(job['job_id']);flows.put(job,stage='completed')
        if len(seen)==9:all_done.set()
    def publish(job):
        assert all_done.wait(10),'light lane stalled behind publication'
        flows.put(job,stage='completed')
    monkeypatch.setattr(flows,'_identifying',identify);monkeypatch.setattr(flows,'_publishing',publish)
    result=flows.advance()
    assert result['parallel_count']==4 and len(result['job_ids'])==10 and len(set(seen))==9


def test_reused_directory_harmonizes_album_tags_without_editing_existing_audio(service,tmp_path):
    from musicops.media.tags import read_tag_fields
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    old=flac(root/'Artist - 反深代詞'/'old.flac');apply_tag_changes(old,{'album':'反深代詞'})
    original=sha256_file(old);rows=index_library(root,metadata_only=True)
    service.store.put_many('library_files',[(r['file_id'],r) for r in rows]);AlbumCatalog(service.store).sync(root,rows)
    private=Path(cfg['paths']['work'])/'prepared/traditional'
    incoming=flac(private/'new.flac');apply_tag_changes(incoming,{'album':'反深代词'})
    prepared=index_library(private,metadata_only=True)
    job={'job_id':'traditional','scene':'playlist','config':cfg,'album':'反深代词','albumartists':['Artist'],
         'prepared':prepared,'prepared_root':str(private),'stage':'publishing'}
    assert offer(service.workflows,job)
    choose(service.workflows,job,'existing_album:'+job['existing_album_choices'][0]['album_id'])
    assert read_tag_fields(incoming)['album']==['反深代詞']
    assert read_tag_fields(incoming)['artist']==['Artist'] and sha256_file(old)==original


def test_saving_unrelated_settings_does_not_register_entire_ai_backlog(service,monkeypatch):
    import musicops.app.review_queue as reviews
    current=service.config.current();cfg=copy.deepcopy(current['values']);cfg['ai'].update(enabled=True,model='synthetic-model')
    service.config.save(cfg,current['version']);calls=[]
    monkeypatch.setattr(reviews,'submit',lambda *args:calls.append(args))
    service.dispatch('save_setup',{'updates':{'enabled':False}})
    assert calls==[]
    service.dispatch('save_setup',{'ai':{'model':'another synthetic model'}})
    assert len(calls)==1
