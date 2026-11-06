import concurrent.futures,copy,json,sqlite3,threading
from types import SimpleNamespace
from pathlib import Path
import pytest
from musicops.state.store import StateStore
from musicops.app.pipeline import Workflows
from musicops.library.storage import storage_paths,seed_root
from musicops.performance import configure_io_limit,io_bound,policy_status
from musicops.config.schema import default_config,ConfigManager


def test_small_list_queries_do_not_read_large_evidence_payload(tmp_path):
    store=StateStore(tmp_path/'state.sqlite')
    large={'job_id':'large','stage':'needs_review','error_class':'AUTH_FAILED','label':'专辑',
           'prepared':[{'synthetic':'x'*1000000}],'targets':['unused']*100,
           'ai_assistance':{'status':'READY','kind':'metadata','message':'建议','metadata':['x'*100000]},
           'ai_fallback':{'status':'NO_RESOURCE','calls':1,'reason':'无资源'}}
    store.put('evidence','workflow:large',large)
    with store.connection() as c:
        row=c.execute('SELECT summary,error_class FROM workflow_overview').fetchone()
        assert len(row[0])<2000 and row[1]=='AUTH_FAILED'
        assert json.loads(row[0])['ai_assistance']=={'status':'READY','kind':'metadata','message':'建议'}
        sqlplan=c.execute("EXPLAIN QUERY PLAN SELECT id FROM workflow_overview WHERE stage=? AND error_class=?",('needs_review','AUTH_FAILED')).fetchall()
        assert 'overview_error' in str([tuple(r) for r in sqlplan])
    original=store.connection
    from contextlib import contextmanager
    @contextmanager
    def forbid_full_payload():
        with original() as c:
            c.set_authorizer(lambda action,table,column,*_:sqlite3.SQLITE_DENY if action==sqlite3.SQLITE_READ and table=='evidence' and column=='payload' else sqlite3.SQLITE_OK)
            yield c
    store.connection=forbid_full_payload
    flows=Workflows(SimpleNamespace(store=store))
    assert flows.list(state='review')['jobs'][0]['job_id']=='large'
    assert flows.list(state='review',view='albums')['jobs'][0]['job_id']=='large'


def test_incremental_projection_migration_keeps_raw_records_and_direct_sql_updates(tmp_path):
    store=StateStore(tmp_path/'legacy.sqlite');store.put('evidence','workflow:a',{'job_id':'a','stage':'metadata','prepared':[{'kept':True}]})
    with store.connection() as c:
        original=c.execute("SELECT payload FROM evidence WHERE id='workflow:a'").fetchone()[0]
        for suffix in ('insert','update','delete'):c.execute('DROP TRIGGER workflow_projection_'+suffix)
        c.execute('DROP TABLE workflow_overview')
        c.execute('CREATE TABLE workflow_overview(id TEXT PRIMARY KEY,stage TEXT)')
    store=StateStore(store.path)
    with store.connection() as c:
        assert c.execute('SELECT payload FROM evidence').fetchone()[0]==original
        c.execute("UPDATE evidence SET payload=json_set(payload,'$.stage','failed','$.error_class','BROKEN') WHERE id='workflow:a'")
        row=c.execute('SELECT stage,error_class,summary FROM workflow_overview').fetchone()
        assert row[0:2]==('failed','BROKEN') and json.loads(row[2])['stage']=='failed'
    assert store.get('evidence','workflow:a')['prepared']==[{'kept':True}]


def test_task_poll_summary_upserts_atomically_and_excludes_result_and_config(tmp_path):
    store=StateStore(tmp_path/'state.sqlite')
    original={'task_id':'a','command':'advance_workflows','state':'queued','config_snapshot':{'large':'x'*100000},'result':{'huge':['x'*200000]}}
    for state in ('queued','running','completed'):store.put('tasks','a',{**original,'state':state})
    with store.connection() as c:
        row=c.execute('SELECT state,summary FROM task_queue_overview').fetchone()
        assert row[0]=='completed' and len(row[1])<500
        c.execute("UPDATE tasks SET payload=json_set(payload,'$.state','paused') WHERE id='a'")
        assert c.execute('SELECT state FROM task_queue_overview').fetchone()[0]=='paused'
        assert 'config_snapshot' not in json.loads(row[1]) and 'result' not in json.loads(row[1])
    assert store.get('tasks','a')['result']==original['result']


def test_io_slots_bound_disk_work_without_blocking_network_and_support_nested_calls():
    configure_io_limit(2);entered=threading.Event();release=threading.Event();lock=threading.Lock();counts=[0,0]
    @io_bound
    def inner():return policy_status()['io_active']
    @io_bound
    def work():
        with lock:
            counts[0]+=1;counts[1]=max(counts)
            if counts[0]==2:entered.set()
        try:
            assert inner()<=2
            assert release.wait(4)
        finally:
            with lock:counts[0]-=1
    with concurrent.futures.ThreadPoolExecutor(max_workers=9) as pool:
        futures=[pool.submit(work) for _ in range(8)]
        try:
            assert entered.wait(2)
            assert pool.submit(lambda:'network remains available').result(timeout=1)=='network remains available'
        finally:release.set()
        for future in futures:future.result(timeout=4)
    assert counts[1]==2 and policy_status()['io_active']==0


def test_optional_ssd_storage_roundtrips_but_backups_and_seeding_stay_on_library_volume(tmp_path):
    library=tmp_path/'hdd/music';temporary={'enabled':True,'directory':str(tmp_path/'ssd/temp')}
    paths=storage_paths(library,temporary);normal=storage_paths(library)
    assert paths['source']==str(tmp_path/'ssd/temp/downloads') and paths['work']==str(tmp_path/'ssd/temp/work')
    assert all(paths[key]==normal[key] for key in ('backup','quarantine','reports'))
    assert seed_root(library)==tmp_path/'hdd/MusicOps/seeding'
    store=StateStore(tmp_path/'profile/state/musicops.sqlite');manager=ConfigManager(store,tmp_path/'profile',initial_offline=False)
    current=manager.current();cfg=copy.deepcopy(current['values']);cfg['paths']['library']=str(library);cfg['temporary_storage']=temporary;cfg['workflow']['setup_done']=True
    saved=manager.save(cfg,current['version'])
    reopened=ConfigManager(store,tmp_path/'profile').current()
    assert reopened['values']['paths']['source']==paths['source']
    assert reopened['values']['temporary_storage']==temporary
    cfg=copy.deepcopy(reopened['values']);cfg['temporary_storage']['enabled']=False;cfg['paths'].update(normal)
    manager.save(cfg,reopened['version'])
    assert ConfigManager(store,tmp_path/'profile').current()['values']['paths']['source']==normal['source']


@pytest.mark.parametrize('directory',['relative','', 'inside'])
def test_unsafe_fast_temp_selection_is_rejected(tmp_path,directory):
    library=tmp_path/'music'
    if directory=='inside':directory=str(library/'temporary')
    with pytest.raises(ValueError):storage_paths(library,{'enabled':True,'directory':directory})
