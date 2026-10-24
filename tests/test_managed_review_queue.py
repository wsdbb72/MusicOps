"""Durability, bounded work and credential isolation, without real downloads."""
import copy
import json
from pathlib import Path
import threading
from test_task_controls import controls, album, read_job, COMMAND_LOOP
from musicops.app.review_queue import submit, register, schedule, recover, finish
from musicops.app.assistance import review_revision
from musicops.config.schema import digest


def enable(service):
    current=service.config.current();values=copy.deepcopy(current['values'])
    values['ai'].update(enabled=True,model='synthetic-model')
    service.config.save(values,current['version'])


def entries(service):
    return [r for r in service.store.list('evidence') if r.get('kind')=='ai_review_queue']


def test_more_than_twenty_registered_and_one_bounded_task_scheduled(controls):
    service=controls
    jobs=[album(service,str(index),stage='needs_review',review_kind='metadata') for index in range(37)]
    assert submit(service,{'job_ids':[r['job_id'] for r in jobs]})['registered']==37
    assert len(entries(service))==37
    enable(service)
    assert schedule(service)
    tasks=[r for r in service.store.list('tasks') if r['command']=='assist_workflows']
    assert len(tasks)==1 and len(tasks[0]['args']['job_ids'])==1
    assert sum(r['state']=='pending' for r in entries(service))==36
    assert submit(service,{'all':True})['registered']==0


def test_large_backlog_registration_finishes_within_ipc_budget(controls):
    import time
    from musicops.state.store import utcnow
    now=utcnow()
    with controls.store.connection() as connection:
        for index in range(1200):
            job={'kind':'workflow','job_id':'bulk-'+str(index),'stage':'needs_review','review_kind':'metadata','job_kind':'album'}
            connection.execute('INSERT INTO evidence VALUES (?,?,?)',('workflow:'+job['job_id'],json.dumps(job),now))
    started=time.monotonic()
    assert submit(controls,{'all':True})['registered']==1200
    assert time.monotonic()-started<10
    assert len(entries(controls))==1200 and submit(controls,{'all':True})['registered']==0


def test_restart_recovers_inflight_without_replaying_changed_task(controls):
    service=controls;enable(service)
    row=album(service,stage='needs_review',review_kind='metadata')
    assert schedule(service)
    recover(service)
    assert entries(service)[0]['state']=='pending'
    row['prepared']=[{'raw_title':'Changed','artists':['Other']}]
    service.workflows.put(row)
    # A bounded scheduler discards the old revision and may immediately pick
    # the new revision registered by put(), without replaying the stale one.
    assert schedule(service)
    assert any(r['state']=='stale' for r in entries(service))
    assert any(r['revision']==review_revision(row) and r['state']=='running' for r in entries(service))


def test_explicit_resubmission_retries_ai_connection_failure_without_repeating_ready_result(controls):
    row=album(controls,stage='needs_review',review_kind='metadata')
    assert submit(controls,{'job_ids':[row['job_id']]})['registered']==1
    entry=entries(controls)[0];key='ai-review:'+digest({'job':row['job_id'],'revision':review_revision(row)})
    controls.store.put('evidence',key,{**entry,'state':'blocked'})
    assert submit(controls,{'job_ids':[row['job_id']]})['registered']==1
    assert submit(controls,{'job_ids':[row['job_id']]})['registered']==0


def test_new_search_query_and_error_context_invalidate_old_ai_proposals(controls):
    row=album(controls,stage='needs_review',review_kind='search')
    original=review_revision(row)
    assert review_revision({**row,'search_query':'Alternate romanized album'})!=original
    assert review_revision({**row,'error_class':'AUTH_REQUIRED'})!=original
    assert review_revision({**row,'resume_stage':'downloading'})!=original


def test_pause_does_not_starve_other_reviews_and_token_is_human_only(controls):
    service=controls;enable(service)
    album(service,'paused',stage='needs_review',review_kind='metadata',paused=True)
    active=album(service,'active',stage='needs_review',review_kind='metadata')
    token=album(service,'paid',stage='needs_review',review_kind='token')
    assert not register(service,token)
    assert schedule(service)
    assert next(r for r in entries(service) if r['state']=='running')['job_id']==active['job_id']


def test_ai_backlog_does_not_starve_normal_album_processing(controls,monkeypatch):
    service=controls;enable(service)
    album(service,'review',stage='needs_review',review_kind='metadata')
    monkeypatch.setattr(service.workflows,'next_job',lambda:{'job_id':'ready-for-processing'})
    commands=[]
    def enqueue(command,args):
        commands.append(command);service.stop.set();return {'accepted':True}
    monkeypatch.setattr(service,'enqueue',enqueue)
    service.paused=False;service._last_workflow_tick=0
    COMMAND_LOOP(service)
    assert commands==['advance_workflows']


def test_revision_cache_caps_auto_rounds_but_explicit_retry_allowed(controls):
    service=controls;row=album(service,stage='needs_review',review_kind='search')
    for index in range(3):
        row['choices']=[{'candidate_id':str(index)}]
        assert register(service,row)
    row['choices']=[{'candidate_id':'new'}]
    assert not register(service,row)
    assert register(service,row,retry=True)


def test_platform_login_never_persists_plain_cookie_or_qr_session(controls,monkeypatch):
    service=controls;calls=[]
    monkeypatch.setattr(service.engines,'request',lambda *args,**kwargs:calls.append((args,kwargs)) or {})
    value='synthetic_session=synthetic-credential'
    result=service.dispatch('engine_control',{'action':'cookie','source':'qq','cookie':value})
    assert result['saved'] and service.vault.get('vault://engine/qq','127.0.0.1')==value
    assert not service.store.list('tasks')
    assert all(value not in json.dumps(row) for table in ('evidence','ipc_requests') for row in service.store.list(table))
    assert calls[0][1]['json']=={'qq':value}


def test_qr_cookie_saved_only_on_success_and_response_contains_no_secret(controls,monkeypatch):
    service=controls
    replies=iter([{'key':'synthetic-key','url':'https://example.org/synthetic-qr'},
                  {'status':'success','cookie':'synthetic=private-value'},{}])
    monkeypatch.setattr(service.engines,'request',lambda *args,**kwargs:next(replies))
    created=service.engines.login('create','qq')
    assert 'synthetic-key' not in str(created)
    result=service.engines.login('poll','qq',created['session_id'])
    assert result['saved'] and 'private-value' not in str(result)
    assert service.vault.get('vault://engine/qq','127.0.0.1')=='synthetic=private-value'


def test_ai_local_album_alias_mapping_is_grounded_and_cached(controls,monkeypatch):
    from musicops.app.assistance import assist_workflows
    from musicops.app.recognition import RecognitionFallback
    calls=[]
    class AI:
        def map_album_tracks(self,local,observed):
            calls.append((local,observed));return [{'local':0,'observed':0,'confidence':.99}]
    release={'source':'musicbrainz','source_id':'release:synthetic','entity_kind':'release',
        'title':'Synthetic Release','artists':['Synthetic Artist'],
        'tracklist':[{'title':'海の歌','artists':['Synthetic Artist'],'duration':120}]}
    monkeypatch.setattr(RecognitionFallback,'_provider',lambda self:(AI(),None))
    monkeypatch.setattr(controls,'_recognize_metadata',lambda *args,**kwargs:{'selected_candidate':release,'candidates':[release]})
    class Resolver:
        def resolve_candidate(self,*args):return {'candidate':release}
    monkeypatch.setattr(controls,'_catalog_resolver',lambda cfg:Resolver())
    row=album(controls,stage='needs_review',review_kind='metadata')
    row['targets'][0].update(raw_title='Umi no uta',duration=120)
    controls.workflows.put(row)
    for _ in range(2):assist_workflows(controls,{'job_ids':[row['job_id']]},controls.config.current()['values'])
    saved=read_job(controls,row['job_id'])
    assert len(calls)==1 and saved['ai_assistance']['mapped_count']==1
    assert saved['ai_assistance']['metadata'][0]['title']=='海の歌'
    assert saved['targets'][0]['raw_title']=='Umi no uta'  # Advice without apply_verified stays editable.


def test_known_album_still_gets_cached_ai_search_keywords_after_pt_miss(controls,monkeypatch):
    from musicops.app.assistance import assist_workflows
    from musicops.app.recognition import RecognitionFallback
    calls=[]
    class AI:
        def recognition_queries(self,query,candidates):
            calls.append(query);return {'queries':[{'title':'','artists':['Synthetic Artist'],'album':'Romanized Album'}],'reason':'Alternate spelling'}
    monkeypatch.setattr(RecognitionFallback,'_provider',lambda self:(AI(),None))
    monkeypatch.setattr(controls,'_recognize_metadata',lambda *args,**kwargs:{'selected_candidate':None,'candidates':[],'ai_fallback':{'status':'NOT_NEEDED'}})
    row=album(controls,stage='needs_review',task_type='search',review_kind='search')
    for _ in range(2):assist_workflows(controls,{'job_ids':[row['job_id']]},controls.config.current()['values'])
    assert len(calls)==1
    assert read_job(controls,row['job_id'])['ai_assistance']['search_query']=='Synthetic Artist Romanized Album'


def test_unavailable_web_tool_still_allows_ai_queries_and_candidate_recognition(tmp_path):
    from musicops.app.recognition import RecognitionFallback
    from musicops.config.schema import default_config
    from musicops.clients.http import HttpError
    calls=[]
    candidate={'candidate_id':'observed','source':'discogs','source_id':'release:1','entity_kind':'release',
        'title':'Observed Album','album':'Observed Album','artists':['Observed Artist']}
    class Resolver:
        def identify(self,query):return {'candidates':[candidate] if query['album']=='Observed Album' else [],'sources':[],'lookup_links':[]}
    class AI:
        def web_recognition_queries(self,*args):raise HttpError('WEB_SEARCH_UNAVAILABLE')
        def recognition_queries(self,*args):
            calls.append('queries');return {'queries':[{'title':'','artists':['Observed Artist'],'album':'Observed Album'}],'reason':'Known alias'}
        def recognize_candidate(self,*args):
            calls.append('candidate');return {'candidate_id':'observed','confidence':.99,'ambiguous':False,'reason':'Observed release'}
    cfg=default_config(tmp_path,offline=False);cfg['ai'].update(enabled=True,model='synthetic')
    result=RecognitionFallback(cfg,Resolver(),lambda *_:'synthetic',ai_provider=AI()).identify({'title':'','artists':['Alias Artist'],'album':'Alias Album'},force=True)
    assert calls==['queries','candidate'] and result['selected_candidate']['candidate_id']=='observed'


def test_popup_has_no_white_top_bottom_gutters_under_light_or_dark_palette(tmp_path):
    from PySide6.QtWidgets import QApplication,QWidget,QVBoxLayout
    from PySide6.QtGui import QPalette,QColor
    from musicops.gui.controls import QComboBox
    from musicops.gui.workbench import STYLE
    app=QApplication.instance() or QApplication([])
    for background in ('white','#0d1523'):
        widget=QWidget();widget.setStyleSheet(STYLE);palette=widget.palette();palette.setColor(QPalette.Window,QColor(background));widget.setPalette(palette)
        layout=QVBoxLayout(widget);combo=QComboBox();layout.addWidget(combo);combo.addItems(['Platform '+str(i) for i in range(35)])
        widget.resize(440,500);widget.show();app.processEvents();combo.showPopup();app.processEvents()
        view=combo.view()
        for bottom in (False,True):
            if bottom:view.scrollToBottom();app.processEvents()
            image=view.window().grab().toImage()
            assert image.width()>0 and image.height()>0
            for y in (0,image.height()-1):
                assert all(max(image.pixelColor(x,y).getRgb()[:3])<150 for x in range(3,image.width()-12))
        combo.hidePopup();widget.close()
