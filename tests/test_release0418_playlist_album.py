import copy
from types import SimpleNamespace
import pytest
from musicops.app.playlist_album import offer,prepare,validate
from musicops.clients.http import PolicyBlocked
from test_search_resilience import service
from test_release0416_discovery import song,release
from test_task_center import app,window_for


def setup_job(service,monkeypatch,ident='scope-one',title='Song'):
    album=release(source_id='release:full',candidate_id='full-album',tracklist=[song(position=1),song('Second',position=2)])
    single=release(source_id='release:single',candidate_id='single',release_type='Single',album='Single',title='Single',tracklist=[song(title)])
    rows=[song(title,source='netease',source_song_id=ident,target_id=ident)]
    job={'kind':'workflow','batch_id':'batch','config':service.config.current()['values'],'prefer_24bit':True,'progress':0,'label':'Single',
         'job_id':ident,'scene':'playlist','task_type':'identification','task_group':'identify',
         'stage':'identifying','targets':rows,'old_files':[],'prefer_full_album':True,'identified_release':single}
    service.workflows.put(job)
    resolver=SimpleNamespace(identify=lambda query:{'candidates':[album]},
        resolve_candidate=lambda source,ident:{'candidate':single if ident=='release:single' else album})
    monkeypatch.setattr(service,'_catalog_resolver',lambda cfg:resolver)
    monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**kw:{'candidates':[single]})
    return job,album,single,resolver


def select_full(service,job):
    service.workflows.review(job['job_id'],'playlist_album:full-album')
    current=service.store.get('evidence','workflow:'+job['job_id'])
    service.workflows._identifying(current)
    return service.store.get('evidence','workflow:'+job['job_id'])


def test_single_choice_precedes_search_and_full_choice_queues_whole_manifest(service,monkeypatch):
    job,album,_,_=setup_job(service,monkeypatch)
    service.workflows._identifying(job)
    assert job['stage']=='needs_review' and job['review_kind']=='album_scope'
    assert len(job['targets'])==1 and not job.get('search_job_id')
    assert not service.store.list('download_tasks') and not service.store.list('token_reservations')
    current=select_full(service,job)
    search=service.store.get('evidence','workflow:'+current['search_job_id'])
    assert current['stage']=='completed' and search['stage']=='searching'
    assert search['scene']=='playlist' and len(search['targets'])==2
    assert search['album_manifest_verified'] and search['album_scope_release']['source_id']=='release:full'
    assert search['playlist_original_targets'][0]['target_id']=='scope-one'
    assert {t['album'] for t in search['targets']}=={'Album'} and not search.get('retire_single')


def test_keep_single_never_silently_expands_on_retry(service,monkeypatch):
    job,_,_,_=setup_job(service,monkeypatch)
    assert offer(service.workflows,job)
    service.workflows.review(job['job_id'],'keep_single')
    current=service.store.get('evidence','workflow:'+job['job_id'])
    service.workflows._identifying(current)
    search=service.store.get('evidence','workflow:'+current['search_job_id'])
    assert len(search['targets'])==1 and search['targets'][0]['album']=='Single'
    assert search['album_scope_decision']=='single'


def test_option_off_has_no_additional_catalog_queries(service,monkeypatch):
    job,_,_,_=setup_job(service,monkeypatch);job['prefer_full_album']=False
    monkeypatch.setattr(service,'_catalog_resolver',lambda _:pytest.fail('unrequested catalogue request'))
    service.workflows._identifying(job)
    assert job['search_job_id'] and not job.get('album_scope_decision')


@pytest.mark.parametrize('kind',['Album','EP','',None])
def test_track_count_never_invents_a_single_release(service,monkeypatch,kind):
    job,_,single,_=setup_job(service,monkeypatch);single['release_type']=kind
    assert not offer(service.workflows,job)
    assert job['album_scope_decision']=='original' and job['stage']=='identifying'


def test_only_complete_parent_covering_every_selected_recording_is_offered(service,monkeypatch):
    job,album,single,_=setup_job(service,monkeypatch)
    job['targets'].append(song('B side',target_id='b-side'));single['tracklist'].append(song('B side'))
    assert not offer(service.workflows,job)
    assert len(job['targets'])==2 and job['album_scope_decision']=='original'
    assert not job.get('search_job_id')


def test_two_singles_for_same_parent_merge_to_one_search_and_keep_provenance(service,monkeypatch):
    one,_,_,_=setup_job(service,monkeypatch,'first')
    assert offer(service.workflows,one)
    first=select_full(service,one)
    two,_,_,_=setup_job(service,monkeypatch,'second','Second')
    assert offer(service.workflows,two)
    second=select_full(service,two)
    assert second['stage']=='skipped' and second['merged_into_job_id']==first['job_id']
    searches=[r for r in service.workflows.rows() if r.get('task_type')=='search']
    assert len(searches)==1
    owner=service.store.get('evidence','workflow:'+first['job_id'])
    assert owner['merged_playlist_targets'][0]['target_id']=='second'
    assert not service.store.list('token_reservations')


def test_changed_parent_returns_to_explicit_scope_choice_and_preserves_single(service,monkeypatch):
    job,album,_,resolver=setup_job(service,monkeypatch)
    assert offer(service.workflows,job)
    service.workflows.review(job['job_id'],'playlist_album:full-album')
    current=service.store.get('evidence','workflow:'+job['job_id'])
    album['tracklist_complete']=False
    assert prepare(service.workflows,current)
    assert current['stage']=='needs_review' and current['review_kind']=='album_scope' and current['catalog_choices']==[]
    assert current['targets'][0]['album']=='Single' and len(current['targets'])==1
    service.workflows.review(current['job_id'],'keep_single')


def test_scope_cannot_be_bypassed_by_ai_metadata_bulk_retry_or_stale_choice(service,monkeypatch):
    from musicops.app.review_queue import register,submit
    from musicops.app.workflow_actions import retry
    from musicops.app.assistance import review_revision
    job,_,_,_=setup_job(service,monkeypatch);assert offer(service.workflows,job)
    revision=review_revision(job)
    assert not register(service,job) and submit(service,{'all':True})['registered']==0
    with pytest.raises(PolicyBlocked):retry(service.workflows,job,allow_legacy=True)
    with pytest.raises(PolicyBlocked):service.workflows.review(job['job_id'],'edit_metadata',metadata=[{'title':'Song','artist':'Artist','album':'Album'}])
    with pytest.raises(ValueError):service.workflows.review(job['job_id'],'playlist_album:invented')
    service.workflows.review(job['job_id'],'keep_single',expected_revision=revision)
    with pytest.raises(PolicyBlocked):service.workflows.review(job['job_id'],'playlist_album:full-album',expected_revision=revision)


def test_selected_full_album_validation_rejects_missing_extra_or_changed_recording():
    expected=[song(album='Album'),song('Second',album='Album')]
    job={'album_scope_decision':'full','album':'Album','expected_album_tracks':expected}
    validate(job,copy.deepcopy(expected))
    for bad in (expected[:1],expected+[song('Third')],[{**expected[0],'duration':200},expected[1]]):
        with pytest.raises(PolicyBlocked):validate(job,bad)


def test_native_scope_dialog_has_explicit_single_and_full_actions(app,tmp_path):
    from musicops.gui.review import ReviewDialog
    window,client=window_for(tmp_path);calls=[]
    window.call=lambda command,payload,*args,**kw:calls.append((command,payload))
    candidate=release(candidate_id='full-album')
    job={'job_id':'scope','scene':'playlist','stage':'needs_review','task_type':'identification','review_kind':'album_scope',
         'prepared':[song()],'catalog_choices':[candidate],'review_revision':'synthetic-revision'}
    dialog=ReviewDialog(window,job)
    assert not dialog.keep_single.isHidden() and not dialog.complete_release.isHidden()
    assert dialog.save_metadata.isHidden() and not dialog.ai_button.isEnabled()
    dialog.complete_release.click()
    assert calls[-1][1]['choice']=='playlist_album:full-album'
    dialog.keep_single.click()
    assert calls[-1][1]['choice']=='keep_single'
    dialog.close();window.close()


def test_local_same_title_does_not_certify_selected_reissue(service,monkeypatch):
    from pathlib import Path
    from musicops.app.single_upgrade import in_library
    job,album,_,_=setup_job(service,monkeypatch)
    root=Path(job['config']['paths']['library']);root.mkdir(parents=True,exist_ok=True)
    rows=[]
    for index,title in enumerate(('Song','Second')):
        path=root/'Album'/('fixture-'+str(index)+'.flac');path.parent.mkdir(parents=True,exist_ok=True);path.write_bytes(b'synthetic-stat-only')
        stat=path.stat();row=song(title,album='Album',file_id=str(index),path=str(path),location_state='library',size=stat.st_size,mtime_ns=stat.st_mtime_ns)
        rows.append(row);service.store.put('library_files',row['file_id'],row)
    job['album_scope_release']=album;targets=[song(album='Album'),song('Second',album='Album')]
    assert not in_library(service.workflows,job,targets)
    for row in rows:
        row['release_id']='different-reissue';service.store.put('library_files',row['file_id'],row)
    assert not in_library(service.workflows,job,targets)
    for row in rows:
        row['release_id']='full';service.store.put('library_files',row['file_id'],row)
    assert in_library(service.workflows,job,targets)


def test_full_album_rejects_different_recording_id_and_unlabelled_live_metadata():
    from musicops.app.single_upgrade import full_album
    original=song(recording_id='recording-one')
    assert not full_album(release(tracklist=[song(recording_id='recording-two'),song('Second')]),[original])
    assert not full_album(release(release_secondary_types=['Live']),[song()])


@pytest.mark.parametrize('descriptions,expected',[(['Album','Reissue'],'Album'),(['Single'],'Single'),(['LP'],None),(['Album','Single'],None)])
def test_discogs_type_uses_only_explicit_format_descriptions(service,monkeypatch,descriptions,expected):
    from musicops.providers.catalog import CatalogResolver
    resolver=CatalogResolver(service.config.current()['values'])
    data={'id':123,'title':'Album','artists':[{'name':'Artist'}],'formats':[{'name':'CD','descriptions':descriptions}],
          'tracklist':[{'type_':'track','position':'1','title':'Song','duration':'3:00'}]}
    monkeypatch.setattr(resolver,'_get',lambda *args:data)
    candidate=resolver._discogs_detail('release','123')
    assert candidate.get('release_type')==expected


def test_ai_or_manual_metadata_correction_returns_to_album_scope_identification(service,monkeypatch):
    job,_,_,_=setup_job(service,monkeypatch)
    job.update(stage='needs_review',review_kind='metadata',album_scope_decision='original')
    service.workflows.put(job)
    service.workflows.review(job['job_id'],'edit_metadata',metadata=[{'title':'Song','artist':'Artist','album':'Single','duration':180}])
    current=service.store.get('evidence','workflow:'+job['job_id'])
    assert current['stage']=='identifying' and not current.get('search_job_id') and current['album_scope_decision'] is None


def test_late_source_identity_single_resolution_does_not_skip_scope_choice(service,monkeypatch):
    from musicops.app.source_identity import recover
    job,_,single,resolver=setup_job(service,monkeypatch)
    job.update(stage='searching',task_type='search',album_scope_decision='original')
    job['config']['catalog']['enabled']=True
    single.update(entity_kind='release')
    monkeypatch.setattr(service,'_recognize_metadata',lambda *a,**kw:{'selected_candidate':single,'candidates':[single]})
    service.workflows.put(job)
    assert recover(service.workflows,job)
    assert job['stage']=='identifying' and job['album_scope_decision'] is None


@pytest.mark.parametrize('choice',['keep_single','playlist_album:full-album'])
def test_late_single_scope_reuses_search_and_original_album_chain(service,monkeypatch,choice):
    job,_,_,_=setup_job(service,monkeypatch)
    job.update(task_type='search',task_group='search',identification_job_id='original-identification')
    service.workflows.put(job)
    assert offer(service.workflows,job)
    service.workflows.review(job['job_id'],choice)
    current=service.store.get('evidence','workflow:'+job['job_id'])
    service.workflows._identifying(current)
    saved=service.store.get('evidence','workflow:'+job['job_id'])
    assert saved['stage']=='searching' and not saved.get('search_job_id')
    assert '已创建独立搜索任务' not in saved['message']
    assert saved['identification_job_id']=='original-identification'
    assert len(service.workflows.rows())==1
