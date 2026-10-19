import copy,json,shutil,struct,threading,time,subprocess,wave
from pathlib import Path
import pytest
from mutagen.flac import FLAC
from musicops.app.duplicates import scan,merge
from musicops.config.schema import digest
from musicops.domain.errors import Conflict
from musicops.performance import parse_cpu_sets,efficiency_sets,bounded_map
from musicops.library.index import sha256_file
from test_workflows import service,flac
from test_task_center import app,Client
from musicops.gui.workbench import Workbench


def pair(service,release=None,bits=16):
    cfg=service.config.current()['values'];root=Path(cfg['paths']['library'])
    a=flac(root/'First/01 Song.flac',release=release)
    b=root/'Second/01 Song.flac';b.parent.mkdir()
    if bits==16:shutil.copy2(a,b)
    else:flac(b,release=release,bits=bits)
    return cfg,a,b


def args(report):return {'scan_id':report['scan_id'],'group_ids':[g['group_id'] for g in report['groups'] if g['state']=='ready']}


def test_exact_song_dedup_is_reversible_and_keeps_a_survivor(service):
    cfg,a,b=pair(service);original={str(p):sha256_file(p) for p in (a,b)}
    result=scan(service,cfg);assert result['ready_count']==1 and a.exists() and b.exists()
    finished=merge(service,cfg,args(result));assert finished['merged_groups']==1 and int(a.exists())+int(b.exists())==1
    tx=finished['transactions'][0]['tx_id'];assert service._engine(cfg).rollback(tx)['state']=='ROLLED_BACK'
    assert all(sha256_file(p)==h for p,h in original.items())


def test_three_duplicates_make_one_survivor_not_pairwise_cyclic_plans(service):
    cfg,a,b=pair(service);c=a.parent/'Copy.flac';shutil.copy2(a,c)
    report=scan(service,cfg);assert report['ready_count']==1
    merge(service,cfg,args(report));assert sum(p.exists() for p in (a,b,c))==1


def test_same_release_merges_complementary_albums_and_lyrics(service):
    cfg,a,b=pair(service,release='same-release');root=a.parents[1]
    extra=flac(b.parent/'02 Two.flac',title='Two',release='same-release');t=FLAC(extra);t['tracknumber']=['2'];t.save()
    a.with_suffix('.lrc').write_text('[00:00.00]Song\n[00:01.00]End',encoding='utf-8')
    report=scan(service,cfg);assert report['ready_count']==1
    group=report['groups'][0];target=Path(group['keep_directory']);merge(service,cfg,args(report))
    assert len(list(root.rglob('*.flac')))==2 and len(list(target.glob('*.flac')))==2
    assert any(target.glob('*.lrc'))


def test_better_quality_can_replace_same_name_and_restore_originals(service):
    cfg,a,b=pair(service,release='same-release',bits=24)
    # Keep First as canonical by adding another track there.
    extra=flac(a.parent/'02 Two.flac',title='Two',release='same-release');t=FLAC(extra);t['tracknumber']=['2'];t.save()
    old=sha256_file(a);report=scan(service,cfg);assert report['ready_count']==1,report
    result=merge(service,cfg,args(report));assert FLAC(a).info.bits_per_sample==24 and not b.exists()
    assert service._engine(cfg).rollback(result['transactions'][0]['tx_id'])['state']=='ROLLED_BACK'
    assert sha256_file(a)==old and b.exists()


def test_conflicting_lyrics_remain_untouched(service):
    cfg,a,b=pair(service)
    for path,text in ((a,'One'),(b,'Different')):path.with_suffix('.lrc').write_text('[00:00.00]'+text,encoding='utf-8')
    report=scan(service,cfg);assert report['ready_count']==0 and report['review_count']==1
    assert a.exists() and b.exists() and '歌词' in report['groups'][0]['reason']


def test_missing_release_identity_does_not_merge_different_audio(service):
    cfg,a,b=pair(service,bits=24)
    wav=b.with_suffix('.wav')
    with wave.open(str(wav),'wb') as output:output.setparams((1,2,44100,0,'NONE','not compressed'));output.writeframes(b'\x01\x00'*88200)
    subprocess.run([cfg['paths']['tools']['ffmpeg'],'-v','error','-i',str(wav),'-c:a','flac','-y',str(b)],check=True,capture_output=True,creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0));wav.unlink()
    tags=FLAC(b);tags.update(dict(FLAC(a).tags));tags.save();report=scan(service,cfg)
    assert report['ready_count']==0 and report['review_count']==1 and a.exists() and b.exists()


def test_keep_disappearing_or_changing_blocks_dedup(service):
    cfg,a,b=pair(service);report=scan(service,cfg)
    plan=service.store.get('change_plans',report['groups'][0]['plan_id']);keep=Path(plan['keep_files'][0]['path']);other=b if keep==a else a
    keep.write_bytes(b'changed')
    with pytest.raises(Conflict,match='保留版本'):merge(service,cfg,args(report))
    assert other.exists()


def test_source_and_seeding_paths_are_not_duplicate_candidates(service):
    cfg,a,b=pair(service);source=Path(cfg['paths']['source'])/'seed.flac';source.parent.mkdir(parents=True,exist_ok=True);shutil.copy2(a,source)
    before=sha256_file(source);report=scan(service,cfg);merge(service,cfg,args(report));assert sha256_file(source)==before


def test_locked_and_cue_albums_require_review(service):
    cfg,a,b=pair(service);(a.parent/'album.cue').write_text('FILE "01 Song.flac" WAVE',encoding='utf-8')
    report=scan(service,cfg);assert report['ready_count']==0 and 'CUE' in report['groups'][0]['reason']


def cpu(ident,logical,efficiency,flags=0,group=0):
    return struct.pack('<IIIHBBBBBBIQ',32,0,ident,group,logical,logical,0,0,efficiency,flags,0,0)


def test_efficiency_detection_uses_reported_class_not_processor_number():
    rows=parse_cpu_sets(cpu(100,0,5)+cpu(101,1,0)+cpu(102,2,0,2)+cpu(103,3,0,6))
    assert [r['id'] for r in efficiency_sets(rows)]==[101,103]
    assert efficiency_sets(parse_cpu_sets(cpu(1,0,0)+cpu(2,1,0)))==[]
    with pytest.raises(ValueError):parse_cpu_sets(b'broken')


def test_bounded_parallel_io_has_concurrency_without_eager_submission():
    lock=threading.Lock();active=0;peak=0
    def run(i):
        nonlocal active,peak
        with lock:active+=1;peak=max(peak,active)
        time.sleep(.01)
        with lock:active-=1
        return i*2
    assert list(bounded_map(run,range(20),workers=4))==list(range(0,40,2))
    assert 1<peak<=4


def test_rapid_page_switches_have_no_cached_opacity_surface_or_overflow(app,tmp_path):
    from PySide6.QtWidgets import QScrollArea
    window=Workbench(tmp_path,client=Client(tmp_path),autostart=False);window.resize(1040,740);window.show()
    for index in (2,3,2,3,5,0,3):window.nav.setCurrentRow(index);app.processEvents()
    assert window.stack.count()==6 and isinstance(window.stack.currentWidget(),QScrollArea)
    assert all(window.stack.widget(i).graphicsEffect() is None for i in range(6))
    assert not window.prefer24.isVisible() and window.job_table.isVisible()
    assert window.job_table.height()>=240
    assert window.stack.currentWidget().widget().width()<=window.stack.currentWidget().viewport().width()
    window.close()

def test_merge_fills_missing_metadata_and_embedded_cover_in_work_copy(service):
    cfg,a,b=pair(service,release='same-release',bits=24)
    low=FLAC(a);low['genre']=['Jazz'];low.save();best=FLAC(b);best.clear_pictures();best.save()
    report=scan(service,cfg);assert report['ready_count']==1,report
    result=merge(service,cfg,args(report));survivor=next(Path(cfg['paths']['library']).rglob('*.flac'))
    tags=FLAC(survivor);assert tags.info.bits_per_sample==24 and tags['genre']==['Jazz'] and tags.pictures
    assert service.dispatch('duplicate_history')['transactions'][0]['tx_id']==result['transactions'][0]['tx_id']


def test_repeating_same_merge_uses_existing_transaction(service):
    cfg,a,b=pair(service,release='same-release',bits=24);report=scan(service,cfg)
    first=merge(service,cfg,args(report));second=merge(service,cfg,args(report))
    assert first['transactions']==second['transactions'] and len(service.store.list('transactions'))==1


def test_scan_configuration_change_blocks_stale_merge(service):
    cfg,a,b=pair(service);report=scan(service,cfg);changed=copy.deepcopy(cfg);changed['quality']['prefer_24bit']=True
    with pytest.raises(Conflict,match='过期'):merge(service,changed,args(report))
    assert a.exists() and b.exists()


def test_bulk_match_write_is_atomic_on_invalid_table(service):
    with pytest.raises(ValueError):service.store.put_many('injected_table',[])
    service.store.put_many('match_decisions',(('track-'+str(i),{'target_id':'track-'+str(i)}) for i in range(100)))
    assert len(service.store.list('match_decisions'))==100

def test_queued_failure_runs_failure_callback_and_restores_buttons(app,tmp_path):
    from PySide6.QtTest import QTest
    class FailedClient(Client):
        def call(self,command,payload):
            if command=='get_task':return {'task':{'state':'failed','error':'Synthetic conflict','result':None}}
            return super().call(command,payload)
    window=Workbench(tmp_path,client=FailedClient(tmp_path),autostart=False);done=[];failed=[]
    window.tasks['synthetic-task']=done.append;window.task_failures['synthetic-task']=failed.append
    window.poll_tasks()
    for _ in range(50):
        app.processEvents();QTest.qWait(10)
        if failed:break
    assert failed==['Synthetic conflict'] and not done and not window.tasks and not window.task_failures
    window.close()

def test_late_history_reply_does_not_unlock_an_active_scan(app,tmp_path):
    from musicops.gui.duplicates import DuplicatePage
    # QWidget parent must be a QWidget, use an actual workbench for the page.
    owner=Workbench(tmp_path,client=Client(tmp_path),autostart=False);callbacks=[]
    owner.call=lambda command,payload,done,failed:callbacks.append(done)
    page=owner.duplicate_page;page.load_history();page.set_busy(True)
    callbacks[0]({'transactions':[{'tx_id':'synthetic'}]})
    assert page.busy and not page.scan_button.isEnabled() and not page.restore_button.isEnabled()
    page.set_busy(False);assert page.restore_button.isEnabled();owner.close()

def test_missing_media_tool_is_actionable_without_changing_audio(service):
    cfg,a,b=pair(service);cfg['paths']['tools']['ffmpeg']='missing-synthetic-tool'
    report=scan(service,cfg)
    assert report['ready_count']==0 and 'FFmpeg' in report['groups'][0]['reason'] and a.exists() and b.exists()

def test_slow_existing_worker_is_retried_without_spawning_another(tmp_path,monkeypatch):
    import musicops.worker.client as module
    from musicops.worker.client import WorkerClient,WorkerConnectionError
    class Busy:
        def __init__(self,*_):pass
        def acquire(self):raise RuntimeError('WORKER_ALREADY_RUNNING')
        def release(self):pass
    monkeypatch.setattr(module,'WriterLock',Busy);monkeypatch.setattr(module.time,'sleep',lambda *_:None)
    spawned=[];monkeypatch.setattr(module.subprocess,'Popen',lambda *a,**k:spawned.append(a))
    client=WorkerClient(tmp_path);calls=[]
    def attach(*args):
        calls.append(args)
        if len(calls)<3:raise WorkerConnectionError('busy')
        return {'ready':True}
    monkeypatch.setattr(client,'call',attach)
    assert client.ensure_running()=={'ready':True} and len(calls)==3 and not spawned

def test_online_mode_has_a_scoped_real_restore_entry(service):
    cfg,a,b=pair(service);report=scan(service,cfg);result=merge(service,cfg,args(report))
    restored=service._execute('restore_duplicates',{'tx_id':result['transactions'][0]['tx_id']},cfg)
    assert restored['state']=='ROLLED_BACK' and a.exists() and b.exists()
    from musicops.domain.errors import PolicyBlocked
    with pytest.raises(PolicyBlocked):service._execute('restore_duplicates',{'tx_id':'not-a-merge'},cfg)

def test_identical_decoded_audio_with_different_tags_can_merge_without_release_id(service):
    cfg,a,b=pair(service);tag=FLAC(b);tag['genre']=['Jazz'];tag.save();assert sha256_file(a)!=sha256_file(b)
    report=scan(service,cfg);assert report['ready_count']==1,report
    result=merge(service,cfg,args(report));survivor=next(Path(cfg['paths']['library']).rglob('*.flac'))
    assert FLAC(survivor)['genre']==['Jazz']
    assert service._execute('restore_duplicates',{'tx_id':result['transactions'][0]['tx_id']},cfg)['state']=='ROLLED_BACK'
    assert a.exists() and b.exists()


def test_lossless_raw_bitrate_does_not_outrank_compressed_lossless():
    from musicops.app.duplicates import _rank
    a={'path':'song.flac','media_info':{'codec_name':'flac','bits_per_sample':16,'sample_rate':44100,'bit_rate':500000}}
    b={'path':'song.wav','media_info':{'codec_name':'pcm_s16le','bits_per_sample':16,'sample_rate':44100,'bit_rate':1411200}}
    assert _rank(a)>_rank(b)

def test_user_confirmed_catalog_release_can_merge_without_embedded_release_tag(service):
    cfg,a,b=pair(service)
    extra=flac(b.parent/'02 Two.flac',title='Two');t=FLAC(extra);t['tracknumber']=['2'];t.save()
    service._index_real_library({'lightweight':True},cfg)
    for row in service.store.list('library_files'):
        row['metadata_proposal']={k:row[k] for k in ('raw_title','artists','album','duration')}
        row['metadata_proposal'].update(release_id='confirmed-release',source='musicbrainz',reviewed_by='current_user')
        service.store.put('library_files',row['file_id'],row)
    report=scan(service,cfg);assert report['ready_count']==1,report
    result=merge(service,cfg,args(report));target=Path(report['groups'][0]['keep_directory'])
    assert len(list(target.glob('*.flac')))==2
    assert service._execute('restore_duplicates',{'tx_id':result['transactions'][0]['tx_id']},cfg)['state']=='ROLLED_BACK'
    assert a.exists() and b.exists() and extra.exists()

def test_new_lyrics_after_scan_blocks_stale_merge(service):
    cfg,a,b=pair(service);report=scan(service,cfg)
    b.with_suffix('.lrc').write_text('[00:00.00]New lyrics',encoding='utf-8')
    with pytest.raises(Conflict,match='扫描后发生变化'):merge(service,cfg,args(report))
    assert a.exists() and b.exists() and b.with_suffix('.lrc').exists()


def test_nested_cue_reference_blocks_album_move(service):
    cfg,a,b=pair(service,release='same-release');extra=a.parent/'Notes';extra.mkdir();(extra/'release.cue').write_text('FILE "../01 Song.flac" WAVE',encoding='utf-8')
    report=scan(service,cfg);assert report['ready_count']==0 and 'CUE' in report['groups'][0]['reason']
    assert a.exists() and b.exists()

def test_stop_targets_only_the_owned_duplicate_task_and_cancels_queued_activity(app,service,tmp_path):
    owner=Workbench(tmp_path,client=Client(tmp_path),autostart=False);calls=[]
    owner.call=lambda command,payload,done=None,failed=None,accepted=None:calls.append((command,payload,done,accepted))
    page=owner.duplicate_page;page.scan();assert not page.stop_button.isEnabled()
    calls[0][3]({'task_id':'synthetic-own-task'});assert page.stop_button.isEnabled();page.stop_current()
    assert calls[-1][:2]==('cancel_task',{'task_id':'synthetic-own-task'}) and not page.stop_button.isEnabled()
    queued=service.dispatch('scan_duplicates',{});service.dispatch('cancel_task',{'task_id':queued['task_id']})
    assert service.store.get('evidence','workflow:activity:'+queued['task_id'])['stage']=='cancelled'
    owner.close()
