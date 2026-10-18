import time
from PySide6.QtCore import Qt
from PySide6.QtWidgets import QPushButton
from test_task_center import app,window_for,job,wait


def test_solver_is_only_in_settings_and_test_failure_finishes(app,tmp_path):
    window,client=window_for(tmp_path)
    assert not any('FlareSolverr' in w.text() for w in window.findChildren(QPushButton))
    window.setup();wizard=window.wizard;wizard.open_solver_settings();app.processEvents()
    assert 'FlareSolverr' in wizard.currentPage().title()
    original=client.call
    def fail(command,payload):
        if command=='test_browser_solver':
            time.sleep(.03);raise RuntimeError('synthetic connection failure')
        return original(command,payload)
    client.call=fail;wizard.solver_test_button.click()
    assert not wizard.solver_test_button.isEnabled() and wizard.solver_test_timer.isActive()
    wait(app,window)
    assert wizard.solver_test_button.isEnabled() and not wizard.solver_test_timer.isActive()
    assert 'synthetic connection failure' in wizard.solver_note.text()
    wizard.close();window.close()


def test_overview_promotes_processing_and_shows_chain_with_context_actions(app,tmp_path):
    window,_=window_for(tmp_path)
    row=job('process','process',task_type='album',stage='downloading',overview=True,
            chain=[dict(job_id='identity',task_type='identification',stage='completed'),dict(job_id='process',task_type='album',stage='downloading')])
    window.show_jobs(dict(view='albums',jobs=[row],total=1,stage_counts={'downloading':1}))
    assert window.job_table.topLevelItem(0).text(0)=='下载与入库'
    assert '识别：已完成' in window.job_detail.toPlainText() and '下载与入库：下载中' in window.job_detail.toPlainText()
    assert not window.review_current.isEnabled() and not window.retry_current.isEnabled()
    assert window.task_view.currentData()=='albums' and window.task_filter.currentData()=='active'
    window.close()


def test_album_selection_follows_successor_and_view_switch_clears_selection(app,tmp_path):
    window,_=window_for(tmp_path);window.checked_jobs={'identity':{'stage':'identifying'}}
    row=job('search','search',overview=True,chain=[dict(job_id='identity',task_type='identification',stage='completed'),dict(job_id='search',task_type='search',stage='searching')])
    window.show_jobs(dict(view='albums',jobs=[row],total=1))
    assert set(window.checked_jobs)=={'search'}
    assert window.job_items['search'].checkState(0)==Qt.Checked
    window.task_view.setCurrentIndex(window.task_view.findData('steps'));wait(app,window)
    assert not window.checked_jobs
    window.close()


def test_task_query_is_sent_to_whole_queue_not_limited_to_current_page(app,tmp_path):
    window,client=window_for(tmp_path);window.task_query.setText('Artist');window.change_task_filter();wait(app,window)
    queries=[payload for command,payload in client.calls if command=='list_workflows']
    assert queries[-1]['query']=='Artist' and queries[-1]['view']=='albums'
    window.close()
