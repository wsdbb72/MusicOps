import copy,json,shutil
from pathlib import Path
from types import SimpleNamespace
import pytest
from musicops.clients.qbt import QbtClient
from musicops.clients.http import FakeTransport,Response,PolicyBlocked
from musicops.library.storage import storage_paths,seed_root
from musicops.library.index import sha256_file
from musicops.media.identity_fields import usable,release_pairs
from musicops.media.lyrics import plain_lyrics,embed_sidecar
from musicops.app.publication_cleanup import finish
from musicops.config.schema import default_config,ConfigManager
from musicops.state.store import StateStore
from test_workflows import flac
from test_workflows import service
from test_workflows import job,until_terminal,image


def test_storage_is_library_adjacent_and_not_profile(tmp_path):
    paths=storage_paths(tmp_path/'music')
    assert Path(paths['source'])==tmp_path/'MusicOps/downloads'
    assert Path(paths['work'])==tmp_path/'MusicOps/work'
    assert seed_root(tmp_path/'music')==tmp_path/'MusicOps/seeding'
    assert Path(storage_paths(tmp_path/'MusicOps')['source'])==tmp_path/'MusicOps-data/downloads'


def test_existing_setup_upgrades_paths_preserving_tools_and_credentials(tmp_path):
    store=StateStore(tmp_path/'state.sqlite');manager=ConfigManager(store,tmp_path/'profile',False)
    old=manager.current();cfg=old['values'];cfg['workflow']['setup_done']=True;cfg['paths']['library']=str(tmp_path/'music')
    manager.save(cfg,old['version']);ConfigManager(store,tmp_path/'profile',False);new=manager.current()['values']
    assert new['paths']['source']==str(tmp_path/'MusicOps/downloads')
    assert new['paths']['tools']==cfg['paths']['tools'] and new['qbt']==cfg['qbt']


def qbt_fixture(tmp_path):
    source=tmp_path/'old/job';source.mkdir(parents=True);(source/'song.flac').write_bytes(b'original seed bytes')
    qbt=QbtClient(FakeTransport(), 'https://qbt.invalid',tmp_path/'MusicOps/downloads',category='MusicOps')
    record={'task_id':'job','client_id':'a'*40,'creation_intent':True,'source_path':str(source),'category':'MusicOps','task_tag':'MusicOps','state':'COMPLETE','metainfo':{'file_list':[{'path':'song.flac','padding':False}]}}
    qbt._put('job',record)
    row={'hash':'a'*40,'tags':'MusicOps','category':'MusicOps','auto_tmm':False,'save_path':str(source),'content_path':str(source/'song.flac'),'download_path':str(source/'.incomplete'),'state':'uploading','progress':1}
    qbt._query=lambda _:row
    requests=[]
    def request(method,endpoint,**kw):
        requests.append((method,endpoint,kw))
        if endpoint=='torrents/files':return Response(200,json.dumps([{'name':'song.flac','priority':1,'progress':1}]).encode())
        if endpoint=='torrents/setLocation':
            dest=Path(kw['data']['location']);dest.mkdir(parents=True);shutil.move(str(source/'song.flac'),str(dest/'song.flac'))
            row.update(save_path=str(dest),content_path=str(dest/'song.flac'))
        if endpoint=='torrents/setDownloadPath':row['download_path']=kw['data']['path']
        return Response(200,b'')
    qbt._request=request
    return qbt,row,source,requests


def test_seed_move_verified_resumable_and_preserves_exact_bytes(tmp_path):
    qbt,row,old,requests=qbt_fixture(tmp_path);before=sha256_file(old/'song.flac')
    assert qbt.archive_completed('job',tmp_path/'music')['status']=='WAITING'
    assert qbt.archive_completed('job',tmp_path/'music')['status']=='WAITING'
    assert qbt.archive_completed('job',tmp_path/'music')['status']=='OK'
    new=seed_root(tmp_path/'music')/'job'
    assert sha256_file(new/'song.flac')==before and not (old/'song.flac').exists()
    assert qbt._get('job')['source_path']==str(new) and not qbt._check_ownership(row,qbt._get('job'))
    assert [r[1] for r in requests if r[0]=='POST']==['torrents/setLocation','torrents/setDownloadPath']


def test_foreign_torrent_is_not_relocated(tmp_path):
    qbt,row,old,requests=qbt_fixture(tmp_path);row['category']='Personal'
    assert qbt.archive_completed('job',tmp_path/'music')['status']=='POLICY_BLOCKED'
    assert not requests and (old/'song.flac').is_file()


def test_migration_does_not_accept_corrupt_moved_audio(tmp_path):
    qbt,row,old,requests=qbt_fixture(tmp_path)
    qbt.archive_completed('job',tmp_path/'music');qbt.archive_completed('job',tmp_path/'music')
    (seed_root(tmp_path/'music')/'job/song.flac').write_bytes(b'changed')
    assert qbt.archive_completed('job',tmp_path/'music')['status']=='POLICY_BLOCKED'
    assert Path(qbt._get('job')['source_path'])==old


def test_roon_lyrics_embed_plain_text_and_keep_timed_sidecar(tmp_path):
    audio=flac(tmp_path/'song.flac');sidecar=audio.with_suffix('.lrc')
    lrc='[ar:Artist]\n[00:00.00]一行歌词\n[00:01.00]Second line'
    sidecar.write_text(lrc,'utf-8');assert embed_sidecar(audio,sidecar,2)
    from musicops.media.tags import read_tag_fields
    assert read_tag_fields(audio)['lyrics']==['一行歌词\nSecond line'] and sidecar.read_text('utf-8')==lrc


def test_placeholder_artist_needs_unique_track_duration_evidence():
    assert all(not usable(v) for v in ['???','？？？','未知歌手','\ufffd']) and usable('Who?')
    row={'raw_title':'Song','artists':['???'],'duration':123};track={'title':'Song','artists':['Artist'],'duration':123}
    assert release_pairs([row],[track])
    assert release_pairs([row],[{**track,'duration':1240}]) is None
    assert release_pairs([row],[{**track,'duration':None}]) is None


def cleanup_fixture(tmp_path):
    cfg=default_config(tmp_path/'profile',False);cfg['paths']['library']=str(tmp_path/'music');cfg['paths'].update(storage_paths(cfg['paths']['library']))
    prepared=Path(cfg['paths']['work'])/'prepared/job/song.flac';prepared.parent.mkdir(parents=True);prepared.write_bytes(b'prepared')
    source=Path(cfg['paths']['source'])/'job/song.flac';source.parent.mkdir(parents=True);source.write_bytes(b'raw')
    final=Path(cfg['paths']['library'])/'Artist/Album/song.flac';final.parent.mkdir(parents=True);final.write_bytes(b'prepared')
    copy=Path(cfg['paths']['work'])/'streaming/job/attempt/song.flac';copy.parent.mkdir(parents=True);copy.write_bytes(b'raw')
    job={'job_id':'job','scene':'playlist','config':cfg,'prepared_root':str(prepared.parent),'prepared':[{'path':str(prepared),'original_path':str(source),'original_hash':sha256_file(source),'sidecars':[]}],'publication_plan':{'operations':[{'destination':'Artist/Album/song.flac','source_hash':sha256_file(final)}]}}
    def put(job,**values):job.update(values)
    workflows=SimpleNamespace(store=SimpleNamespace(get=lambda *_:None),put=put)
    return workflows,job,source,prepared,copy,final


def test_cleanup_clears_both_owned_download_and_validation_copies_idempotently(tmp_path):
    workflows,job,source,prepared,copy,final=cleanup_fixture(tmp_path)
    assert finish(workflows,job) and finish(workflows,job)
    assert not source.exists() and not prepared.exists() and not copy.exists() and final.read_bytes()==b'prepared'


def test_cleanup_refuses_changed_publication_or_original(tmp_path):
    workflows,job,source,prepared,copy,final=cleanup_fixture(tmp_path);source.write_bytes(b'changed')
    with pytest.raises(PolicyBlocked):finish(workflows,job)
    assert source.read_bytes()==b'changed'


def test_native_netease_fallback_persists_without_circular_reference(service,monkeypatch,tmp_path):
    from musicops.app.streaming_fallback import run
    from musicops.providers import netease_download
    original=flac(tmp_path/'download.flac')
    cfg=service.config.current()['values'];target={'target_id':'netease:42','source':'netease','source_song_id':'42','raw_title':'Song','artists':['Artist'],'album':'Album','duration':2}
    job={'job_id':'fallback','batch_id':'batch','scene':'playlist','config':cfg,'targets':[target],'old_files':[],'stage':'fallback','album_manifest_verified':True}
    monkeypatch.setattr(netease_download,'download_track',lambda *a,**k:str(original))
    run(service.workflows,job)
    saved=service.store.get('evidence','workflow:fallback')
    assert saved['stage']=='metadata' and 'netease_match' not in saved['targets'][0]
    assert saved['targets'][0]['download_source']=='netease'


def test_active_download_relocation_never_adds_starts_or_stops(tmp_path):
    qbt,row,source,requests=qbt_fixture(tmp_path);row.update(progress=.5,state='stalledDL')
    assert qbt.relocate_active('job',tmp_path/'music')['status']=='WAITING'
    assert qbt.relocate_active('job',tmp_path/'music')['status']=='WAITING'
    assert qbt.relocate_active('job',tmp_path/'music')['status']=='OK'
    assert [r[1] for r in requests]==['torrents/setDownloadPath','torrents/setLocation']
    assert row['progress']==.5 and qbt._get('job')['source_path']==str(tmp_path/'MusicOps/downloads/job')


def test_official_lyrics_fallback_precedes_auth_error_review(service,monkeypatch):
    from musicops.providers import netease_download
    from musicops.media.tags import read_tag_fields
    cfg=service.config.current()['values'];cfg['lyrics']['primary']['base_url']='https://lyrics.virtual.invalid';cfg['providers']['netease']['enabled']=True
    source=Path(cfg['paths']['source'])/'lyrics-fallback';flac(source/'42.flac')
    target={'source':'netease','source_song_id':'42','raw_title':'Song','artists':['Artist'],'album':'Album','duration':2}
    row=job(service,sources=source,targets=[target]);row['config']=cfg;service.workflows.put(row)
    monkeypatch.setattr(service,'_real_resources',lambda *a,**k:{'lyrics':{'state':'AUTH_FAILED'},'candidates':[]})
    monkeypatch.setattr(netease_download,'lyrics_for_id',lambda *_:'[00:00.00]First\n[00:01.00]Second')
    done=until_terminal(service);assert done['stage']=='completed',done
    output=next(Path(cfg['paths']['library']).rglob('*.flac'))
    assert read_tag_fields(output)['lyrics']==['First\nSecond'] and output.with_suffix('.lrc').is_file()


def test_local_cover_uses_unique_exact_album_api_identity(monkeypatch):
    import httpx
    from musicops.providers.cover_download import cover
    real=httpx.Client;requests=[]
    def serve(request):
        requests.append(str(request.url))
        if request.url.path=='/api/search/get':return httpx.Response(200,json={'result':{'albums':[{'id':42,'name':'Album','artist':{'name':'Artist'}}]}})
        if request.url.path=='/api/album/42':return httpx.Response(200,json={'album':{'name':'Album','artist':{'name':'Artist'},'picUrl':'https://p1.music.126.net/approved.jpg'}})
        return httpx.Response(200,content=image())
    monkeypatch.setattr(httpx,'Client',lambda **kw:real(transport=httpx.MockTransport(serve),**kw))
    assert cover('Album',['Artist'])==image() and len(requests)==3


def test_album_id_mismatch_still_allows_confirmed_musicbrainz_cover(monkeypatch):
    import httpx
    from musicops.providers.cover_download import cover
    real=httpx.Client;requests=[]
    def serve(request):
        requests.append(str(request.url))
        if request.url.host=='music.163.com':return httpx.Response(200,json={'album':{'name':'Wrong','artist':{'name':'Artist'}}})
        return httpx.Response(200,content=image())
    monkeypatch.setattr(httpx,'Client',lambda **kw:real(transport=httpx.MockTransport(serve),**kw))
    assert cover('Album',['Artist'],42,'11111111-1111-1111-1111-111111111111')==image()
    assert any('coverartarchive' in url for url in requests)


def test_old_job_snapshot_moves_cached_streaming_before_resuming(service,tmp_path):
    from musicops.app.storage_migration import prepare
    cfg=service.config.current()['values'];cfg['workflow']['setup_done']=True
    source=Path(cfg['paths']['source'])/'old-job';source.mkdir(parents=True);(source/'song.flac').write_bytes(b'cached')
    stream=Path(cfg['paths']['work'])/'streaming/old-job/attempt';stream.mkdir(parents=True);(stream/'song.flac').write_bytes(b'cached')
    row={'job_id':'old-job','config':cfg,'stage':'running','resume_stage':'fallback','source_path':str(source)}
    assert prepare(service.workflows,row)
    expected=storage_paths(cfg['paths']['library'])
    assert row['config']['paths']['source']==expected['source'] and row['source_path']==str(Path(expected['source'])/'old-job')
    assert (Path(row['source_path'])/'song.flac').read_bytes()==b'cached' and not (source/'song.flac').exists()
    assert (Path(expected['work'])/'streaming/old-job/attempt/song.flac').read_bytes()==b'cached'
    assert prepare(service.workflows,row)


def test_only_confirmed_serialization_failures_are_recovered_once(service):
    from musicops.app.storage_migration import recover_serialization_failures
    for key in ('known','different','cancelled'):
        row={'job_id':key,'config':service.config.current()['values'],'stage':'failed','resume_stage':'fallback','error_class':'ValueError','paused':True,'cancel_requested':key=='cancelled'}
        service.workflows.put(row)
    for key in ('known','cancelled'):
        service.store.event('workflow_error',{'job_id':key,'error_class':'ValueError','frames':[{'file':'streaming_fallback.py'},{'file':'encoder.py','function':'iterencode'}]})
    assert recover_serialization_failures(service.workflows)==1
    known=service.store.get('evidence','workflow:known');assert known['stage']=='fallback' and known['paused']
    assert service.store.get('evidence','workflow:different')['stage']=='failed'
    assert recover_serialization_failures(service.workflows)==0


def test_pending_seed_move_resumes_same_operation_after_save_path_changes(service,monkeypatch):
    from musicops.app.storage_migration import prepare
    from musicops.clients import qbt_live
    cfg=service.config.current()['values'];cfg['workflow']['setup_done']=True
    row={'job_id':'pending','config':cfg,'stage':'running','resume_stage':'metadata'}
    service.store.put('download_tasks','qbt:pending',{'source_path':str(Path(cfg['paths']['source'])/'pending'),'relocation':{'state':'MOVING'}})
    called=[]
    client=SimpleNamespace(archive_completed=lambda *a:(called.append('archive') or {'status':'WAITING'}),transport=SimpleNamespace(close=lambda:None))
    monkeypatch.setattr(qbt_live,'live_client',lambda *a:client)
    assert prepare(service.workflows,row) is False and called==['archive']
    assert row['stage']=='metadata' and row['next_poll']>0


def test_verified_song_album_id_accepts_guest_artist_without_unrelated_search(monkeypatch):
    import httpx
    from musicops.providers.cover_download import cover
    real=httpx.Client
    def serve(request):
        if request.url.host=='music.163.com':return httpx.Response(200,json={'album':{'name':'Album','artist':{'name':'Release Artist'},'picUrl':'https://p1.music.126.net/approved.jpg'}})
        return httpx.Response(200,content=image())
    monkeypatch.setattr(httpx,'Client',lambda **kw:real(transport=httpx.MockTransport(serve),**kw))
    assert cover('Album',['Guest'],42,verified_album_id=True)==image()
    assert cover('Album',['Guest'],42) is None


def test_restart_reuses_recent_large_catalogue_and_refreshes_stale_one(service,monkeypatch):
    import datetime as dt
    from musicops.library.catalog import root_key
    current=service.config.current();cfg=current['values'];cfg['workflow']['setup_done']=True;service.config.save(cfg,current['version'])
    key='library-catalog:'+root_key(cfg['paths']['library']);queued=[]
    monkeypatch.setattr(service,'enqueue',lambda *a:queued.append(a))
    now=dt.datetime.now(dt.timezone.utc)
    service.store.put('evidence',key,{'updated_at':(now-dt.timedelta(hours=2)).isoformat(),'track_count':27631})
    service.prepare_local_catalog();assert not queued
    service.store.put('evidence',key,{'updated_at':(now-dt.timedelta(hours=25)).isoformat(),'track_count':27631})
    service.prepare_local_catalog();assert queued[0][0]=='index_library'


def test_legacy_ready_publication_revalidates_and_embeds_existing_lrc(service):
    from musicops.media.tags import read_tag_fields,apply_tag_changes
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'old-ready';flac(source/'song.flac')
    (source/'song.lrc').write_text('[00:00.00]First\n[00:01.00]Second','utf-8')
    row=job(service,sources=source);service.workflows._metadata(row)
    pending=service.store.get('evidence','workflow:test_job');audio=Path(pending['prepared'][0]['path'])
    apply_tag_changes(audio,{'lyrics':[]});pending.update(stage='publishing');pending.pop('resource_contract',None);service.workflows.put(pending)
    done=until_terminal(service);assert done['stage']=='completed',done
    published=next(Path(cfg['paths']['library']).rglob('*.flac'))
    assert read_tag_fields(published)['lyrics']==['First\nSecond'] and published.with_suffix('.lrc').is_file()


def test_long_unicode_release_paths_fit_windows_limit_without_truncating_tags(service):
    from musicops.media.tags import apply_tag_changes,read_tag_fields
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'long';audio=flac(source/'song.flac')
    title='歌曲'+('あ'*180);artist='歌手'+('い'*180);album='专辑'+('う'*180)
    apply_tag_changes(audio,{'title':title,'artist':[artist],'albumartist':[artist],'album':album})
    job(service,sources=source);done=until_terminal(service);assert done['stage']=='completed',done
    output=next(Path(cfg['paths']['library']).rglob('*.flac'));assert len(str(output))<=240 and '~' in str(output)
    fields=read_tag_fields(output);assert fields['title']==[title] and fields['album']==[album] and fields['artist']==[artist]


def test_committed_publication_can_be_retried_after_temporary_source_cleanup(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'replay';flac(source/'song.flac')
    job(service,sources=source);done=until_terminal(service);assert done['stage']=='completed'
    first=done['transaction_id'];count=len(service.store.list('transactions'));service.workflows.put(done,stage='publishing')
    replay=until_terminal(service);assert replay['stage']=='completed' and replay['transaction_id']==first
    assert len(service.store.list('transactions'))==count and len(list(Path(cfg['paths']['library']).rglob('*.flac')))==1
