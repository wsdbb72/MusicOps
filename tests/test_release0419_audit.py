"""Private-profile audit regressions; no live download or library modifications."""
import json
import sqlite3
from pathlib import Path
from types import SimpleNamespace

import pytest

from test_task_controls import controls,album
from test_managed_review_queue import enable,entries
from test_workflows import service,flac,image,job,until_terminal
from test_release0410_editions import pending
from test_audit_core import fake_runner
from musicops.app.pipeline import identity
from musicops.app.review_queue import schedule
from musicops.app.workflow_actions import retry
from musicops.clients.http import PolicyBlocked
from musicops.library.index import AUDIO_EXTENSIONS,index_library
from musicops.media.tags import apply_tag_changes,read_tag_fields,apply_cover_changes,embedded_covers
from musicops.media.validation import MediaValidator
from musicops.state.store import StateStore


def test_selected_versions_are_schedulable_independently(service):
    parent=pending(service)
    ids=service.workflows.review(parent['job_id'],['ordinary','live'])['processing_job_ids']
    for _ in ids:
        chosen=service.workflows.next_job()
        assert chosen and chosen['job_id'] in ids
        assert chosen['identification_job_id'] is None
        service.workflows.put(chosen,stage='completed')
    assert service.workflows.next_job() is None


def test_recovery_repairs_self_dependency_without_replaying_terminal_versions(controls):
    batch='synthetic-batch'
    controls.workflows.put({'job_id':'batch:'+batch,'batch_id':batch,'job_kind':'batch','stage':'completed'})
    active=album(controls,'legacy-edition',stage='requesting',task_type='album',identification_job_id='legacy-edition',version_parent_id='old-parent')
    done=album(controls,'done-edition',stage='completed',task_type='album',identification_job_id='done-edition',version_parent_id='old-parent')
    controls.workflows.recover()
    assert controls.store.get('evidence','workflow:'+active['job_id'])['identification_job_id'] is None
    assert controls.store.get('evidence','workflow:'+done['job_id'])['identification_job_id']==done['job_id']


def test_background_fairness_and_publication_priority(controls,monkeypatch):
    enable(controls)
    album(controls,'review',stage='needs_review',review_kind='metadata')
    ordinary={'stage':'metadata'}
    monkeypatch.setattr(controls.workflows,'next_job',lambda:ordinary)
    scheduled=[]
    monkeypatch.setattr(controls,'enqueue',lambda command,args:(scheduled.append(command) or {'task_id':'fake'}))
    for _ in range(4):
        controls._last_workflow_tick=0
        assert controls._schedule_background()
    assert scheduled==['advance_workflows']*3+['assist_workflows']
    assert not controls._schedule_background()
    controls._workflow_waves=3;controls._last_workflow_tick=0;ordinary['stage']='publishing'
    assert controls._schedule_background() and scheduled[-1]=='advance_workflows'


def test_ai_scheduler_prunes_orphans_and_completed_entries_before_valid_review(controls):
    enable(controls)
    old=album(controls,'old',stage='needs_review',review_kind='metadata')
    album(controls,'valid',stage='needs_review',review_kind='metadata')
    controls.store.put('evidence','workflow:old',{**old,'stage':'completed'})
    controls.store.put('evidence','ai-review:orphan',{'job_id':'missing','state':'pending','revision':'old'})
    assert schedule(controls)
    assert any(entry['job_id']=='valid' and entry['state']=='running' for entry in entries(controls))
    assert all(entry['state']!='pending' for entry in entries(controls) if entry['job_id']!='valid')


def test_transaction_owner_projection_tracks_updates_deletions_and_restart(tmp_path):
    store=StateStore(tmp_path/'state.sqlite')
    tx={'tx_id':'first','state':'COMMITTED','operations':[{'operation_id':'shared'}]}
    store.put('transactions','first',tx)
    with store.connection() as connection:
        assert tuple(connection.execute('SELECT transaction_id,operation_id,state FROM transaction_operations').fetchone())==('first','shared','COMMITTED')
    store.put('transactions','first',{**tx,'state':'ROLLED_BACK'})
    store=StateStore(store.path)
    with store.connection() as connection:
        assert connection.execute('SELECT state FROM transaction_operations').fetchone()[0]=='ROLLED_BACK'
        connection.execute('DELETE FROM transactions WHERE id=?',('first',))
        assert connection.execute('SELECT COUNT(*) FROM transaction_operations').fetchone()[0]==0


def test_transaction_owner_projection_backfills_legacy_state(tmp_path):
    store=StateStore(tmp_path/'legacy.sqlite')
    with store.connection() as connection:
        for action in ('insert','update','delete'):connection.execute('DROP TRIGGER transaction_operations_'+action)
        connection.execute('DROP TABLE transaction_operations')
    store.put('transactions','old',{'tx_id':'old','state':'COMMITTED','operations':[{'operation_id':'old-op'}]})
    store=StateStore(store.path)
    with store.connection() as connection:
        assert tuple(connection.execute('SELECT transaction_id,operation_id FROM transaction_operations').fetchone())==('old','old-op')


def test_interrupted_projection_migration_rolls_back_ddl_and_retries(tmp_path,monkeypatch):
    store=StateStore(tmp_path/'interrupted.sqlite')
    store.put('transactions','old',{'tx_id':'old','state':'COMMITTED','operations':[{'operation_id':'preserved'}]})
    with store.connection() as connection:
        for action in ('insert','update','delete'):connection.execute('DROP TRIGGER transaction_operations_'+action)
        connection.execute('DROP TABLE transaction_operations')
    connect=sqlite3.connect
    class Interrupted(sqlite3.Connection):
        def execute(self,sql,*args,**kwargs):
            if sql.startswith('INSERT OR IGNORE INTO transaction_operations SELECT t.id'):
                raise sqlite3.OperationalError('synthetic interruption before backfill')
            return super().execute(sql,*args,**kwargs)
    with monkeypatch.context() as patch:
        patch.setattr(sqlite3,'connect',lambda *a,**k:connect(*a,**k,factory=Interrupted))
        with pytest.raises(sqlite3.OperationalError,match='synthetic interruption'):StateStore(store.path)
    with connect(store.path) as connection:
        assert connection.execute("SELECT 1 FROM sqlite_master WHERE name='transaction_operations'").fetchone() is None
    store=StateStore(store.path)
    with store.connection() as connection:
        assert connection.execute('SELECT operation_id FROM transaction_operations').fetchone()[0]=='preserved'


def test_interrupted_task_projection_does_not_hide_existing_jobs(tmp_path,monkeypatch):
    store=StateStore(tmp_path/'task-migration.sqlite')
    store.put('evidence','workflow:kept',{'job_id':'kept','stage':'needs_review'})
    with store.connection() as connection:
        for action in ('insert','update','delete'):connection.execute('DROP TRIGGER workflow_projection_'+action)
        connection.execute('DROP TABLE workflow_overview')
    connect=sqlite3.connect
    class Interrupted(sqlite3.Connection):
        def execute(self,sql,*args,**kwargs):
            if sql.startswith('INSERT OR REPLACE INTO workflow_overview'):
                raise sqlite3.OperationalError('synthetic task-index interruption')
            return super().execute(sql,*args,**kwargs)
    with monkeypatch.context() as patch:
        patch.setattr(sqlite3,'connect',lambda *a,**k:connect(*a,**k,factory=Interrupted))
        with pytest.raises(sqlite3.OperationalError,match='synthetic task-index'):StateStore(store.path)
    store=StateStore(store.path)
    with store.connection() as connection:
        assert connection.execute('SELECT job_id FROM workflow_overview').fetchone()[0]=='kept'


def test_preparation_retry_archives_failed_work_then_rebuilds(service):
    cfg=service.config.current()['values'];source=Path(cfg['paths']['source'])/'retry'
    original=flac(source/'song.flac');before=original.read_bytes()
    row=job(service,sources=source)
    work=Path(service.workflows.work_paths(row)['work'])/'prepared'/row['job_id']
    work.mkdir(parents=True);(work/'partial.flac').write_bytes(b'failed work copy')
    row.update(stage='needs_review',review_kind='error',resume_stage='metadata',prepared_root=str(work))
    service.workflows.put(row)
    retry(service.workflows,row)
    refreshed=service.store.get('evidence','workflow:'+row['job_id'])
    assert not work.exists() and (Path(refreshed['failed_preparations'][0])/'partial.flac').read_bytes()==b'failed work copy'
    finished=until_terminal(service)
    assert finished['stage']=='completed',finished.get('message')
    assert original.read_bytes()==before


@pytest.mark.parametrize('ownership,transaction',[(False,False),(True,True)])
def test_preparation_retry_refuses_unknown_work_and_existing_transaction(service,ownership,transaction):
    row=job(service)
    work=Path(service.workflows.work_paths(row)['work'])/'prepared'/row['job_id']
    work.mkdir(parents=True);(work/'keep').write_bytes(b'keep')
    row.update(stage='needs_review',review_kind='error',resume_stage='metadata',prepared_root=str(work) if ownership else None)
    if transaction:service.store.put('change_plans','workflow:'+row['job_id'],{'plan_id':'workflow:'+row['job_id']})
    with pytest.raises(PolicyBlocked):retry(service.workflows,row)
    assert (work/'keep').read_bytes()==b'keep'


def test_chinese_aliases_preserve_recording_markers():
    a={'raw_title':'魔幻季節','artists':['張學友']}
    assert identity(a)==identity({'raw_title':'魔幻季节','artists':['张学友']})
    assert identity(a)!=identity({'raw_title':'魔幻季节 (Live)','artists':['张学友']})
    assert identity(a)!=identity({'raw_title':'魔幻季节','artists':['Other']})


def test_aac_is_indexed_instead_of_disappearing(tmp_path,monkeypatch):
    assert '.aac' in AUDIO_EXTENSIONS
    path=tmp_path/'audio.aac';path.write_bytes(b'synthetic')
    monkeypatch.setattr('mutagen.File',lambda *a,**k:None)
    assert any(row['path']==str(path) for row in index_library(tmp_path,metadata_only=True))


def test_tool_version_cache_invalidates_replaced_executable(tmp_path,monkeypatch):
    tools=[tmp_path/'ffmpeg.exe',tmp_path/'ffprobe.exe']
    for path in tools:path.write_bytes(b'tool')
    calls=[]
    def run(command,timeout):calls.append(command);return fake_runner(command,timeout)
    monkeypatch.setattr('musicops.media.validation._run',run)
    validator=MediaValidator(*tools)
    assert validator.tool_versions()==validator.tool_versions() and len(calls)==2
    tools[0].write_bytes(b'changed tool')
    validator.tool_versions();assert len(calls)==4


def test_shared_validation_cache_rehashes_changed_media(tmp_path,monkeypatch):
    audio=tmp_path/'audio.flac';audio.write_bytes(b'first')
    monkeypatch.setattr('musicops.media.validation._run',fake_runner)
    validator=MediaValidator('synthetic-ffmpeg','synthetic-ffprobe')
    first=validator.validate(audio);assert first['decode_state']=='passed'
    assert validator.validate(audio)['cache_hit']
    audio.write_bytes(b'other')
    changed=validator.validate(audio)
    assert not changed['cache_hit'] and first['file_hash']!=changed['file_hash']


def test_apev2_fields_and_front_cover_roundtrip_without_losing_back_cover(tmp_path,monkeypatch):
    from mutagen.apev2 import APEv2
    tags=APEv2();tags['Cover Art (Back)']=b'back.jpg\0back-image'
    path=tmp_path/'work.ape';path.write_bytes(b'synthetic audio')
    tags.save(path)
    def read_audio(path):
        audio=SimpleNamespace(tags=APEv2(path))
        audio.save=lambda:audio.tags.save(path)
        return audio
    monkeypatch.setattr('musicops.media.tags.mutagen.File',read_audio)
    values={'artist':['Artist A','Artist B'],'albumartist':['Various Artists'],'tracknumber':'2/12','discnumber':'1/2','date':'2007'}
    assert apply_tag_changes(path,values)['verified']
    assert read_tag_fields(path)['artist']==['Artist A','Artist B']
    persisted=APEv2(path)
    assert str(persisted['Track'])=='2/12' and str(persisted['Disc'])=='1/2'
    cover=image();assert apply_cover_changes(path,cover)['verified']
    assert cover in embedded_covers(path) and b'back-image' in embedded_covers(path)


def test_album_search_collects_later_pages_and_preserves_partial_failure():
    from musicops.app.search_status import search_album
    calls=[]
    class Provider:
        def search(self,query,page=1):
            calls.append(page)
            return {'status':'OK','candidates':[{'source':'test','candidate_id':str(page)}],'page':page,'pages':3,'search_complete':page==3}
    result=search_album(Provider,'Artist Album')
    assert calls==[1,2,3] and len(result['candidates'])==3 and result['search_complete']
    class Failure:
        def search(self,query,page=1):
            return Provider().search(query,page) if page==1 else {'status':'RATE_LIMITED','candidates':[]}
    result=search_album(Failure,'Artist Album')
    assert result['status']=='RATE_LIMITED' and len(result['candidates'])==1


def test_album_search_stops_repeated_pages_and_bounded_huge_results():
    from musicops.app.search_status import search_album
    calls=[]
    class Provider:
        def search(self,query,page=1):
            calls.append(page)
            return {'status':'OK','candidates':[{'source':'test','candidate_id':str(page)}],'page':page,'pages':100,'search_complete':False}
    result=search_album(Provider,'query')
    assert len(calls)==5 and not result['search_complete'] and result['limit_reason']
    calls.clear()
    class Repeated:
        def search(self,query,page=1):
            result=Provider().search(query,page);result['candidates'][0]['candidate_id']='same';return result
    assert len(search_album(Repeated,'query')['candidates'])==1 and len(calls)==2


def test_missing_lyrics_credential_is_actionable_without_request(controls,monkeypatch):
    from musicops.app.connection_tests import run
    monkeypatch.setattr('musicops.app.connection_tests.SafeHttpTransport',lambda *a,**k:pytest.fail('missing credential must not make a request'))
    result=run(controls,{'kind':'lyrics'},controls.config.current()['values'])
    assert result['status']=='NOT_CONFIGURED' and 'Authorization' in result['message']


def test_cross_platform_matching_closes_only_owned_transport(monkeypatch):
    from musicops.providers.netease_match import resolve_netease_target
    from musicops.clients.http import Response
    transports=[]
    class Transport:
        def __init__(self,*a,**k):self.closed=False;transports.append(self)
        def request(self,*a,**k):return Response(200,json.dumps({'code':200,'result':{'songs':[]}}).encode())
        def close(self):self.closed=True
    monkeypatch.setattr('musicops.providers.netease_match.SafeHttpTransport',Transport)
    target={'source':'spotify','raw_title':'Song','artists':['Artist'],'album':'Album','duration':100}
    with pytest.raises(PolicyBlocked):resolve_netease_target(target,{'netease':{}},lambda *a:None)
    assert len(transports)==1 and transports[0].closed
    injected=Transport()
    with pytest.raises(PolicyBlocked):resolve_netease_target(target,{'netease':{}},lambda *a:None,transport=injected)
    assert not injected.closed


def test_validation_failure_persists_track_cause_and_leaves_source(service,tmp_path,monkeypatch):
    source=tmp_path/'damaged.flac';source.write_bytes(b'corrupt fixture')
    row=job(service);row.update(stage='validating',prepared=[{'path':str(source),'sidecars':[]}])
    service.workflows.put(row)
    validator=SimpleNamespace(validate=lambda path:{'decode_state':'corrupt','error_class':'DECODE_FAILED','roon_compatible':False})
    monkeypatch.setattr(service,'_validator',lambda cfg:validator)
    with pytest.raises(PolicyBlocked,match='damaged.flac.*DECODE_FAILED'):service.workflows._validating(row)
    saved=service.store.get('evidence','workflow:'+row['job_id'])
    assert saved['validation_failure']['error_class']=='DECODE_FAILED'
    assert saved['prepared'][0]['validation']['decode_state']=='corrupt'
    assert source.read_bytes()==b'corrupt fixture'


def test_native_platform_dependency_locks_cover_project_runtime_dependencies():
    import tomllib
    from packaging.requirements import Requirement
    from packaging.utils import canonicalize_name
    root=Path(__file__).resolve().parents[1]
    project=tomllib.loads((root/'pyproject.toml').read_text('utf-8'))
    runtime={canonicalize_name(Requirement(value).name) for value in project['project']['dependencies']}
    for filename in ('requirements.lock','packaging/requirements-macos.txt','docker/requirements.txt'):
        pinned={canonicalize_name(Requirement(line).name) for line in (root/filename).read_text('utf-8').splitlines() if line.strip() and not line.startswith('#')}
        expected=runtime-({'pyside6'} if filename.startswith('docker/') else set())
        assert expected<=pinned,(filename,expected-pinned)


def test_non_chinese_identity_does_not_load_conversion_dictionary(monkeypatch):
    from musicops.media.chinese import alias_key
    monkeypatch.setattr('musicops.media.chinese._converter',lambda:pytest.fail('non-Han identity must not load Chinese dictionaries'))
    assert alias_key('  Synthetic ARTIST  ')=='synthetic artist'
    assert alias_key('カタカナ')=='カタカナ'


def test_publisher_reads_notes_from_verified_source_without_checkout(tmp_path,monkeypatch):
    import pathlib,textwrap,zipfile
    root=Path(__file__).resolve().parents[1]
    workflow=(root/'.github/workflows/desktop-release.yml').read_text('utf-8')
    # Execute the real publisher fragment in an empty directory. Its job
    # downloads build artifacts and intentionally does not check out the repo.
    fragment=workflow[workflow.index("          notes = pathlib.Path('release-notes.md')"):]
    fragment=fragment[:fragment.index('          def gh(')]
    artifacts=tmp_path/'release-assets';artifacts.mkdir()
    with zipfile.ZipFile(artifacts/'MusicOps-0.0.1-source.zip','w') as archive:
        archive.writestr('MusicOps-0.0.1/docs/发布说明.md','合成发行说明')
    monkeypatch.chdir(tmp_path)
    assert not (tmp_path/'docs').exists()
    exec(compile(textwrap.dedent(fragment),'publisher-notes','exec'),{'pathlib':pathlib,'zipfile':zipfile,'root':artifacts,'version':'0.0.1','commit':'f'*40})
    assert (tmp_path/'release-notes.md').read_text('utf-8')=='合成发行说明\n\nSource commit: '+'f'*40+'\n'
