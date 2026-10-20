import copy
import pytest
from PySide6.QtCore import Qt
from musicops.domain.editions import rank,describe,compatible,needs_version_choice
from musicops.media.identity_fields import metadata_subset_pairs
from musicops.gui.review import ReviewDialog
from test_review_dialog import app,Owner,job as review_job
from test_workflows import service


def candidate(id='ordinary',edition='',source='orpheus',kind='Album'):
    return {'candidate_id':id,'source':source,'resource_id':id,'title':'Album'+(' (Live)' if edition=='Live' else ''),'artists':['Artist'],'seeders':3,'release':{'album':'Album','edition':edition,'year':2007,'release_type_name':kind},'media':{'format':'FLAC','encoding':'Lossless','media':'CD'}}


def test_single_is_last_even_when_higher_resolution_and_special_policy():
    ordinary=candidate();single=candidate('single',kind='Single');single['media']['encoding']='24bit 192kHz'
    for policy in ('quality','complete','special'):assert rank(ordinary,policy)>rank(single,policy)


def test_live_studio_are_symmetric_incompatible_and_need_choice():
    assert not compatible('Album (Live)','Album') and not compatible('Album','Album (Live)')
    assert needs_version_choice([candidate(),candidate('live','Live')])
    assert describe(candidate('remaster','2011 Remastered'))['special']


def test_same_edition_is_grouped_across_sites_but_unknown_year_not_asserted():
    a=candidate();b=candidate('elsewhere',source='dicmusic')
    assert describe(a)['group_id']==describe(b)['group_id'] and not describe(a)['identity_confirmed']
    a['release']['year']=None;b['release']['year']=None
    assert describe(a)['group_id']!=describe(b)['group_id']


def pending(service):
    cfg=service.config.current()['values'];a=candidate();b=candidate('live','Live')
    row={'job_id':'version-test','batch_id':'batch','scene':'upgrade','task_type':'search','task_group':'search','stage':'needs_review','review_kind':'candidate','version_choice':True,'config':cfg,'targets':[{'raw_title':'Song','artists':['Artist'],'album':'Album','duration':100}],'old_files':[{'path':'old.flac'}],'choices':[a,b]}
    service.workflows.put(row);return row


def test_multi_version_selection_creates_separate_idempotent_jobs_and_keeps_old(service):
    row=pending(service);result=service.workflows.review(row['job_id'],['ordinary','live'])
    assert len(set(result['processing_job_ids']))==2
    for ident in result['processing_job_ids']:
        child=service.store.get('evidence','workflow:'+ident)
        assert child['stage']=='requesting' and child['selected_edition'] and child['scene']=='playlist'
        assert child['targets']==[] and child['old_files']==[] and child['edition_origin_targets']==row['targets']
    assert service.store.get('evidence','workflow:'+row['job_id'])['old_files']==row['old_files']
    visible=service.workflows.list(view='albums')['jobs']
    assert set(result['processing_job_ids']).issubset({r['job_id'] for r in visible})
    assert service.workflows._review_versions(row,['ordinary','live'])['processing_job_ids']==result['processing_job_ids']


def test_two_resources_from_same_edition_cannot_spend_tokens_twice(service):
    row=pending(service);row['choices']=[candidate(),candidate('same',source='dicmusic')];service.workflows.put(row)
    with pytest.raises(ValueError,match='同一版本'):service.workflows.review(row['job_id'],['ordinary','same'])


def test_ai_queue_does_not_spend_tokens_on_human_version_preference(service):
    from musicops.app.review_queue import register,submit
    row=pending(service)
    assert not register(service,row) and submit(service,{'all':True})['registered']==0
    with pytest.raises(Exception,match='版本取舍'):service.workflows.review(row['job_id'],'ordinary')


def test_review_can_choose_multiple_versions_explicitly(app):
    owner=Owner();row=review_job('candidate');row.update(version_choice=True,choices=[candidate(),candidate('same_group',source='dicmusic'),candidate('live','Live')]);dialog=ReviewDialog(owner,row)
    dialog.candidates.item(0,0).setCheckState(Qt.Checked)
    assert dialog.candidates.item(0,0).checkState()==Qt.Checked
    dialog.candidates.item(1,0).setCheckState(Qt.Checked)
    assert dialog.candidates.item(0,0).checkState()==Qt.Unchecked and dialog.candidates.item(1,0).checkState()==Qt.Checked
    dialog.candidates.item(2,0).setCheckState(Qt.Checked)
    assert '2 个版本' in dialog.candidate_use.text()
    dialog.submit_candidate()
    assert owner.calls[-1][1]['choice']==['same_group','live'];dialog.close();owner.close()


def test_partial_metadata_uses_unique_filename_artist_duration_but_not_completeness():
    rows=[{'path':f'{i}.Artist - Song{i}.flac','duration':100+i} for i in range(1,6)]
    tracks=[{'title':f'Song{i}','artists':['Artist'],'duration':100+i} for i in range(1,7)]
    pairs=metadata_subset_pairs(rows,tracks);assert len(pairs)==5 and all(not row.get('raw_title') for row in rows)
    tracks[0]['duration']+=10;assert len(metadata_subset_pairs(rows,tracks))==4
    tracks[1]['artists']=['Other'];assert metadata_subset_pairs(rows,tracks) is None
