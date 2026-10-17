import copy
import pytest
from PySide6.QtCore import Qt
from PySide6.QtWidgets import QApplication, QMainWindow
from musicops.gui.review import ReviewDialog


@pytest.fixture(scope='module')
def app(): return QApplication.instance() or QApplication([])


class Owner(QMainWindow):
    def __init__(self):
        super().__init__(); self.calls = []; self.refreshed = False
        self.config = {'providers': {'orpheus': {'enabled': True}, 'dicmusic': {'enabled': True}}}
    def call(self, command, payload, done=None, failed=None): self.calls.append((command, payload))
    def refresh(self): self.refreshed = True


def job(kind='metadata'):
    return {'job_id':'fixture', 'label':'Synthetic Album','stage':'needs_review','task_type':'search','review_kind':kind,'review_revision':'current',
            'prepared':[{'raw_title':'Song','artists':['Artist'],'album':'Album','duration':120}],
            'choices':[{'candidate_id':'one','source':'orpheus','title':'Album','artists':['Artist'],'media':{'format':'FLAC'}}]}


def test_review_has_editable_fields_and_submits_only_after_user_adoption(app):
    owner=Owner(); value=job(); value['ai_assistance']={'revision':'current','message':'Observed suggestion','metadata':[{'title':'Song','artist':'Artist','album':'Observed Album','duration':'120'}]}
    dialog=ReviewDialog(owner,value); dialog.open(); app.processEvents()
    assert dialog.metadata.item(0,2).text()=='Observed Album' and not owner.calls
    dialog.metadata.item(0,2).setText('User corrected album'); dialog.submit_metadata()
    command,payload=owner.calls[-1]
    assert command=='review_workflow' and payload['metadata'][0]['album']=='User corrected album' and payload['expected_revision']=='current'
    dialog.close();owner.close()


def test_candidate_requires_selection_and_ai_only_preselects_observed_candidate(app):
    owner=Owner(); value=job('candidate'); dialog=ReviewDialog(owner,value)
    dialog.submit_candidate(); assert not owner.calls and '请选择' in dialog.result.text()
    value['ai_assistance']={'revision':'current','candidate_id':'one','message':'recommendation'}; dialog.populate(value)
    assert dialog.candidates.currentRow()==0 and not owner.calls
    dialog.submit_candidate(); assert owner.calls[-1][1]['choice']=='one'
    dialog.close();owner.close()


def test_research_accepts_custom_keyword_and_selected_enabled_sites(app):
    owner=Owner(); dialog=ReviewDialog(owner,job('search'))
    dialog.query.setText('My keyword');dialog.site_checks['dicmusic'].setChecked(False);dialog.submit_search()
    assert owner.calls[-1][1]['query']=='My keyword' and owner.calls[-1][1]['sites']==['orpheus']
    assert not dialog.site_checks['open.cd'].isEnabled()
    dialog.close();owner.close()


def test_assist_uses_task_id_instead_of_gui_details_or_private_paths(app):
    owner=Owner(); value=job();value['detail']='private local directory';dialog=ReviewDialog(owner,value)
    dialog.assist();assert owner.calls[-1]==('assist_workflows',{'job_ids':['fixture']})
    assert not dialog.ai_button.isEnabled();dialog.close();owner.close()


def test_web_search_lead_remains_explicitly_unverified_and_requires_manual_fill(app):
    owner=Owner();value=job();value['ai_fallback']={'web_queries':[{'title':'Song','artists':['Observed Artist'],'album':'Web Album'}],
        'web_sources':[{'url':'https://www.discogs.com/release/123'}],'web_reason':'Observed source lead'}
    value['lookup_links']=[{'source':'discogs','url':'https://www.discogs.com/release/123'}]
    dialog=ReviewDialog(owner,value)
    assert dialog.metadata.item(0,2).text()=='Album' and not owner.calls
    assert '待核实' in dialog.catalog.itemText(0) and '尚未核实' in dialog.ai_note.text()
    assert 'https://www.discogs.com/release/123' in dialog.source_links.text()
    dialog.use_catalog()
    assert dialog.metadata.item(0,2).text()=='Web Album' and dialog.metadata.item(0,3).text()=='120'
    assert not owner.calls
    dialog.close();owner.close()


def test_stale_ai_web_lead_is_not_offered_as_current_suggestion(app):
    owner=Owner();value=job();value['ai_assistance']={'revision':'outdated','web_queries':[{'album':'Stale','artists':[],'title':''}]}
    dialog=ReviewDialog(owner,value)
    assert dialog.catalog.count()==0 and dialog.metadata.item(0,2).text()=='Album'
    dialog.close();owner.close()


def test_manual_website_search_links_remain_visible_without_identity_claim(app):
    from musicops.providers.catalog import lookup_links
    owner=Owner();value=job();value['lookup_links']=lookup_links({'title':'Song','album':'Album','artists':['Artist']})
    dialog=ReviewDialog(owner,value)
    assert dialog.source_links.text().count('检索 · ')==6
    assert 'Album of the Year' in dialog.source_links.text()
    assert not owner.calls and all(not row['identity_evidence'] for row in value['lookup_links'])
    dialog.close();owner.close()
