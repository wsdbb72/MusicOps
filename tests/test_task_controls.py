"""Durable task controls and identify/search handoff in isolated temporary state.

The actual command loop is parked. These tests cannot contact sites, download,
scan an existing library, or run a media transaction.
"""
import copy
import threading
import time
from pathlib import Path

import pytest

from musicops.app.pipeline import TERMINAL
from musicops.app.service import AppService, TaskCancelled
from musicops.clients.http import HttpError, PolicyBlocked
from musicops.state.store import utcnow

COMMAND_LOOP = AppService._loop


@pytest.fixture
def controls(tmp_path, monkeypatch):
    monkeypatch.setattr(AppService, '_loop', lambda self: self.stop.wait())
    service = AppService(tmp_path/'profile', offline=False)
    service.paused = True
    current = service.config.current()
    cfg = copy.deepcopy(current['values'])
    library = tmp_path/'music'
    library.mkdir()
    sentinel = library/'untouched.flac'
    sentinel.write_bytes(b'synthetic library must not be changed')
    cfg['paths']['library'] = str(library)
    service.config.save(cfg, current['version'])
    for method in ('_index_real_library', '_real_pt', '_engine', '_validator'):
        monkeypatch.setattr(service, method, lambda *a, **k: pytest.fail('task-control test must never perform library or network work'))
    monkeypatch.setattr(service.workflows, 'provider', lambda *a, **k: pytest.fail('task-control test must never contact a download provider'))
    yield service
    service.stop.set()
    service.thread.join(2)
    assert sentinel.read_bytes() == b'synthetic library must not be changed'
    assert sorted(path.name for path in library.iterdir()) == ['untouched.flac']


def activity(service, command='manual_album_search'):
    accepted = service.dispatch(command, {'query': 'Synthetic Album'})
    return 'activity:'+accepted['task_id'], accepted['task_id']


def read_job(service, job_id):
    return service.store.get('evidence', 'workflow:'+job_id)


def control(service, action, ids):
    return service.dispatch('bulk_workflows', {'action': action, 'job_ids': ids})


def album(service, job_id='identify-fixture', *, stage='identifying', **extra):
    row = {'kind': 'workflow', 'job_id': job_id, 'job_kind': 'album',
           'batch_id': 'synthetic-batch', 'scene': 'playlist', 'stage': stage,
           'task_group': 'identify', 'task_type': 'identification', 'paused': False,
           'config': copy.deepcopy(service.config.current()['values']),
           'targets': [{'raw_title': 'Synthetic Song', 'album': 'Synthetic Album',
                        'artists': ['Synthetic Artist'], 'duration': 120}],
           'old_files': [], 'created_at': utcnow(), **extra}
    return service.workflows.put(row)


def test_bulk_queued_activities_pause_resume_and_cancel_without_scheduling_work(controls):
    service = controls
    first, first_task = activity(service)
    second, second_task = activity(service, 'identify_metadata')
    assert service.workflows.next_job() is None
    assert service.workflows.advance()['idle']
    paused = control(service, 'pause', [first, second, first])
    assert paused['succeeded'] == 2 and len(paused['results']) == 2
    for job_id, task_id in ((first, first_task), (second, second_task)):
        assert read_job(service, job_id)['paused']
        assert service.store.get('tasks', task_id)['state'] == 'paused'
    assert control(service, 'resume', [first, second])['succeeded'] == 2
    for job_id, task_id in ((first, first_task), (second, second_task)):
        assert not read_job(service, job_id)['paused']
        assert service.store.get('tasks', task_id)['state'] == 'queued'
    assert service.workflows.next_job() is None
    assert control(service, 'cancel', [first, second])['succeeded'] == 2
    for job_id, task_id in ((first, first_task), (second, second_task)):
        assert read_job(service, job_id)['stage'] == 'cancelled'
        assert service.store.get('tasks', task_id)['state'] == 'cancelled'
    assert control(service, 'resume', [first, second])['skipped'] == 2


def test_queued_preparation_pause_controls_its_durable_start_task(controls):
    service = controls
    accepted = service.dispatch('start_workflow', {'scene': 'complete'})
    assert control(service, 'pause', [accepted['job_id']])['succeeded'] == 1
    assert service.store.get('tasks', accepted['task_id'])['state'] == 'paused'
    assert read_job(service, accepted['job_id'])['paused']
    assert control(service, 'resume', [accepted['job_id']])['succeeded'] == 1
    assert service.store.get('tasks', accepted['task_id'])['state'] == 'queued'
    control(service, 'pause', [accepted['job_id']])
    control(service, 'cancel', [accepted['job_id']])
    assert service.store.get('tasks', accepted['task_id'])['state'] == 'cancelled'
    assert read_job(service, accepted['job_id'])['stage'] == 'cancelled'


def test_never_started_paused_activity_survives_restart_and_can_resume(controls):
    service = controls
    job_id, task_id = activity(service)
    control(service, 'pause', [job_id])
    reopened = AppService(service.base, offline=False)
    try:
        assert reopened.store.get('tasks', task_id)['state'] == 'paused'
        job = read_job(reopened, job_id)
        assert job['job_kind'] == 'activity' and job['paused']
        assert not job.get('legacy_batch_requires_review')
        assert control(reopened, 'resume', [job_id])['succeeded'] == 1
        assert reopened.store.get('tasks', task_id)['state'] == 'queued'
        assert reopened.workflows.next_job() is None
    finally:
        reopened.stop.set()
        reopened.thread.join(2)


def test_interrupted_running_activity_requires_review_without_legacy_migration(controls):
    service = controls
    job_id, task_id = activity(service)
    task = service.store.get('tasks', task_id)
    task.update(state='paused', started_at=utcnow())
    service.store.put('tasks', task_id, task)
    service.workflows.put(read_job(service, job_id), stage='running', paused=True)
    reopened = AppService(service.base, offline=False)
    try:
        assert reopened.store.get('tasks', task_id)['state'] == 'recovery_required'
        job = read_job(reopened, job_id)
        assert job['stage'] == 'needs_review' and not job.get('legacy_batch_requires_review')
        assert control(reopened, 'resume', [job_id])['skipped'] == 1
        assert reopened.workflows.next_job() is None
    finally:
        reopened.stop.set()
        reopened.thread.join(2)


def test_cancel_at_paused_checkpoint_unblocks_without_resuming_work(controls):
    service = controls
    job_id, task_id = activity(service)
    task = service.store.get('tasks', task_id)
    task.update(state='running', started_at=utcnow())
    service.store.put('tasks', task_id, task)
    service.workflows.put(read_job(service, job_id), stage='running')
    service.active = task_id
    results = []
    def wait_at_checkpoint():
        try:
            service._checkpoint('synthetic_safe_pause')
            results.append('unexpected_continue')
        except TaskCancelled:
            results.append('cancelled')
    waiter = threading.Thread(target=wait_at_checkpoint)
    waiter.start()
    try:
        until = time.monotonic()+2
        while time.monotonic() < until:
            if service.store.get('tasks', task_id).get('safe_phase') == 'synthetic_safe_pause':
                break
            time.sleep(.01)
        assert service.store.get('tasks', task_id)['state'] == 'paused'
        assert control(service, 'cancel', [job_id])['succeeded'] == 1
        waiter.join(2)
        assert not waiter.is_alive() and results == ['cancelled']
        assert read_job(service, job_id)['stage'] == 'cancelled'
    finally:
        service.active = None
        service.stop.set()
        waiter.join(2)


@pytest.mark.parametrize('stage', sorted(TERMINAL))
def test_bulk_resume_never_replays_terminal_album(stage, controls):
    service = controls
    row = album(service, stage=stage)
    assert control(service, 'resume', [row['job_id']])['skipped'] == 1
    assert read_job(service, row['job_id'])['stage'] == stage
    assert service.workflows.next_job() is None


def test_completed_batch_controls_children_without_replaying_completed_child(controls):
    service = controls
    batch = service.workflows.put({'kind': 'workflow', 'job_kind': 'batch',
                                  'job_id': 'batch:synthetic-batch', 'batch_id': 'synthetic-batch',
                                  'scene': 'playlist', 'stage': 'completed', 'created_at': utcnow()})
    pending = album(service)
    finished = album(service, 'finished-fixture', stage='completed')
    assert control(service, 'pause', [batch['job_id']])['succeeded'] == 1
    assert read_job(service, pending['job_id'])['paused']
    assert read_job(service, finished['job_id'])['stage'] == 'completed'
    assert service.workflows.next_job() is None
    control(service, 'resume', [batch['job_id']])
    assert service.workflows.next_job()['job_id'] == pending['job_id']
    control(service, 'cancel', [batch['job_id']])
    assert read_job(service, batch['job_id'])['stage'] == 'completed'
    assert read_job(service, pending['job_id'])['stage'] == 'cancelled'
    assert read_job(service, finished['job_id'])['stage'] == 'completed'


def test_bulk_cancel_can_dismiss_review_items_while_bulk_resume_cannot_replay_them(controls):
    service = controls
    album_job = album(service, stage='needs_review')
    activity_id, _ = activity(service)
    service.workflows.put(read_job(service, activity_id), stage='needs_review')
    batch_job = service.workflows.put({'kind': 'workflow', 'job_kind': 'batch',
                                      'job_id': 'batch:synthetic-batch', 'batch_id': 'synthetic-batch',
                                      'scene': 'playlist', 'stage': 'needs_review', 'created_at': utcnow()})
    assert control(service, 'resume', [album_job['job_id'], activity_id])['skipped'] == 2
    assert control(service, 'cancel', [batch_job['job_id'], activity_id])['succeeded'] == 2
    for job_id in (batch_job['job_id'], album_job['job_id'], activity_id):
        assert read_job(service, job_id)['stage'] == 'cancelled'
    assert service.workflows.next_job() is None


def test_cancelled_identification_cannot_create_search_from_stale_result(controls):
    service = controls
    stale = album(service)
    control(service, 'cancel', [stale['job_id']])
    try:
        service.workflows._queue_search(stale)
    except TaskCancelled:
        pass
    assert read_job(service, stale['job_id'])['stage'] == 'cancelled'
    assert all(row.get('task_type') != 'search' for row in service.workflows.rows())
    assert service.workflows.next_job() is None


def test_identification_handoff_reads_latest_pause_and_never_replays_finished_search(controls):
    service = controls
    stale = album(service)
    control(service, 'pause', [stale['job_id']])
    service.workflows._queue_search(stale)
    parent = read_job(service, stale['job_id'])
    child = read_job(service, parent['search_job_id'])
    assert parent['stage'] == 'completed' and child['task_type'] == 'search'
    assert child['paused'] and child['identification_job_id'] == parent['job_id']
    assert service.workflows.next_job() is None
    service.workflows.put(child, stage='completed', paused=False)
    service.workflows._queue_search(parent)
    assert read_job(service, child['job_id'])['stage'] == 'completed'
    assert len([row for row in service.workflows.rows() if row.get('task_type') == 'search']) == 1


def test_search_child_is_not_schedulable_before_parent_is_completed(controls):
    service = controls
    parent = album(service, stage='needs_review')
    child = album(service, 'premature-search', stage='searching', task_group='search',
                  task_type='search', identification_job_id=parent['job_id'])
    assert service.workflows.next_job() is None
    service.workflows.put(parent, stage='completed')
    assert service.workflows.next_job()['job_id'] == child['job_id']


@pytest.mark.parametrize('failure', [HttpError('NETWORK_ERROR', 'synthetic unavailable'),
                                  TimeoutError('synthetic timeout'),
                                  PolicyBlocked('synthetic invalid site settings')])
def test_one_site_construction_failure_does_not_block_other_sites_or_matching_album(controls, monkeypatch, failure):
    service = controls
    row = album(service, stage='searching', task_type='search', task_group='search', prefer_24bit=True)
    cfg = row['config']
    cfg['ai']['enabled'] = False
    sites = cfg['workflow']['source_order']
    for site in sites:
        cfg['providers'][site]['enabled'] = True
    calls = []
    class Provider:
        def __init__(self, site):
            self.site = site
        def search(self, query):
            calls.append(self.site)
            if self.site != 'orpheus':
                return {'status': 'NO_RESOURCE', 'candidates': []}
            return {'status': 'OK', 'search_complete': True, 'candidates': [{
                'candidate_id': 'orpheus:synthetic', 'source': 'orpheus', 'resource_id': '1',
                'title': 'Synthetic Album', 'artists': ['Synthetic Artist'], 'seeders': 2,
                'media': {'format': 'FLAC', 'encoding': 'Lossless'},
                'release': {'album': 'Synthetic Album'}}]}
    def provider(site, cfg):
        if site == 'dicmusic':
            raise failure
        return Provider(site)
    monkeypatch.setattr(service.workflows, 'provider', provider)
    service.workflows._searching(row)
    saved = read_job(service, row['job_id'])
    assert calls == [site for site in sites if site != 'dicmusic' for _ in range(1 if site=='orpheus' else 2)]
    processing = read_job(service, saved['processing_job_id'])
    assert saved['stage'] == 'completed' and saved['task_type'] == 'search'
    assert processing['stage'] == 'requesting' and processing['selected']['source'] == 'orpheus'
    assert processing['task_group'] == 'process' and processing['search_job_id'] == saved['job_id']
    assert saved['search_summary']['unavailable_sites'] == ['dicmusic']
    assert 'open.cd' in saved['search_summary']['successful_sites']
    assert not service.store.list('token_reservations') and not service.store.list('download_tasks')


def test_site_wrapper_preserves_cancellation_instead_of_reporting_network_failure():
    from musicops.app.search_status import search_site
    def cancelled():
        raise TaskCancelled('synthetic cancellation')
    with pytest.raises(TaskCancelled):
        search_site(cancelled, 'Synthetic Album')


def test_cancelled_search_cannot_queue_processing_and_paused_search_hands_off_pause(controls):
    service = controls
    cancelled = album(service, 'cancelled-search', stage='searching', task_type='search', task_group='search')
    control(service, 'cancel', [cancelled['job_id']])
    try:
        service.workflows._queue_processing(cancelled, 'requesting', 'Synthetic handoff')
    except TaskCancelled:
        pass
    assert not read_job(service, cancelled['job_id']).get('processing_job_id')
    assert all(row.get('task_group') != 'process' for row in service.workflows.rows())
    stale = album(service, 'paused-search', stage='searching', task_type='search', task_group='search')
    control(service, 'pause', [stale['job_id']])
    service.workflows._queue_processing(stale, 'requesting', 'Synthetic handoff')
    saved = read_job(service, stale['job_id'])
    processing = read_job(service, saved['processing_job_id'])
    assert saved['stage'] == 'completed' and processing['paused']
    assert service.workflows.next_job() is None
    service.workflows.put(processing, paused=False)
    assert service.workflows.next_job()['job_id'] == processing['job_id']
    # Even a leftover child must wait for its parent to have committed success.
    service.workflows.put(saved, stage='needs_review')
    assert service.workflows.next_job() is None


@pytest.mark.parametrize('action', ['pause', 'cancel'])
def test_user_control_cannot_be_lost_between_worker_read_and_write(controls, monkeypatch, action):
    service = controls
    stale = album(service)
    original_get = service.store.get
    read_old = threading.Event()
    release_write = threading.Event()
    control_entered = threading.Event()
    control_finished = threading.Event()
    failures = []
    writer = None
    def gated_get(kind, key):
        value = original_get(kind, key)
        if threading.current_thread() is writer and key == 'workflow:'+stale['job_id'] and not read_old.is_set():
            read_old.set()
            if not release_write.wait(3):
                raise RuntimeError('test did not release synthetic writer')
        return value
    monkeypatch.setattr(service.store, 'get', gated_get)
    original_bulk = service.workflows.bulk
    def record_control(*args):
        control_entered.set()
        return original_bulk(*args)
    monkeypatch.setattr(service.workflows, 'bulk', record_control)
    def worker_result():
        try:
            service.workflows.put(stale, stage='searching', message='synthetic earlier result')
        except Exception as exc:
            failures.append(exc)
    def user_control():
        try:
            control(service, action, [stale['job_id']])
        except Exception as exc:
            failures.append(exc)
        finally:
            control_finished.set()
    writer = threading.Thread(target=worker_result)
    caller = threading.Thread(target=user_control)
    writer.start()
    try:
        assert read_old.wait(2)
        caller.start()
        # Without the shared mutex the control commits while the worker holds an
        # obsolete snapshot. With it, the control waits until the result commits.
        if control_entered.wait(.15):
            assert control_finished.wait(2)
    finally:
        release_write.set()
        writer.join(3)
        if caller.ident is not None:
            caller.join(3)
    assert not writer.is_alive() and not caller.is_alive() and not failures
    final = read_job(service, stale['job_id'])
    if action == 'pause':
        assert final['paused']
    else:
        assert final['cancel_requested'] and final['stage'] == 'cancelled'


def test_running_activity_pause_finishes_current_stage_without_remaining_active_or_replay(controls, monkeypatch):
    service = controls
    job_id, task_id = activity(service)
    entered = threading.Event()
    finish = threading.Event()
    executions = []
    def execute(command, args, cfg):
        assert command == 'manual_album_search'
        executions.append(command)
        entered.set()
        assert finish.wait(3)
        return {'status': 'OK', 'message': 'Synthetic read-only search completed',
                'candidates': [], 'read_only': True, 'music_writes': 0}
    monkeypatch.setattr(service, '_execute', execute)
    runner = threading.Thread(target=COMMAND_LOOP, args=(service,))
    service.paused = False
    runner.start()
    try:
        assert entered.wait(2)
        assert control(service, 'pause', [job_id])['succeeded'] == 1
        listing = service.dispatch('list_workflows')
        visible = next(row for row in listing['jobs'] if row['job_id'] == job_id)
        assert visible['stage'] == 'running' and visible['paused'] and visible['pause_pending']
        finish.set()
        deadline = time.monotonic()+3
        while time.monotonic() < deadline:
            if service.store.get('tasks', task_id)['state'] == 'completed' and service.active is None:
                break
            time.sleep(.01)
        assert service.store.get('tasks', task_id)['state'] == 'completed'
        listing = service.dispatch('list_workflows')
        visible = next(row for row in listing['jobs'] if row['job_id'] == job_id)
        assert visible['stage'] == 'completed' and not visible['pause_pending']
        assert listing['active'] == listing['paused_count'] == listing['total_active'] == 0
        assert listing['finished'] == 1
        assert control(service, 'resume', [job_id])['skipped'] == 1
        assert executions == ['manual_album_search']
    finally:
        finish.set()
        service.stop.set()
        runner.join(3)
    assert not runner.is_alive()


@pytest.mark.parametrize('blocked_parent', ['identification', 'search'])
@pytest.mark.parametrize('stage', ['cancelled', 'failed', 'needs_review', 'skipped', 'restored'])
def test_process_child_requires_both_parents_completed(controls, blocked_parent, stage):
    service = controls
    identify = album(service, 'identify-parent', stage='completed', paused=True)
    search = album(service, 'search-parent', stage='completed', paused=True,
                   task_type='search', task_group='search', identification_job_id=identify['job_id'])
    processing = album(service, 'process-child', stage='requesting', task_type='album',
                       task_group='process', identification_job_id=identify['job_id'],
                       search_job_id=search['job_id'])
    parent = identify if blocked_parent == 'identification' else search
    service.workflows.put(parent, stage=stage)
    assert service.workflows.next_job() is None
    service.workflows.put(parent, stage='completed')
    assert service.workflows.next_job()['job_id'] == processing['job_id']


def test_queued_and_terminal_pauses_have_consistent_task_counts(controls):
    service = controls
    first, _ = activity(service)
    second, _ = activity(service, 'identify_metadata')
    ended = album(service, 'ended-paused', stage='completed', paused=True)
    control(service, 'pause', [first, second])
    listing = service.dispatch('list_workflows')
    assert listing['active'] == 0 and listing['paused_count'] == listing['total_active'] == 2
    control(service, 'resume', [first])
    listing = service.dispatch('list_workflows')
    assert listing['active'] == listing['paused_count'] == 1 and listing['total_active'] == 2
    control(service, 'cancel', [second])
    listing = service.dispatch('list_workflows')
    assert listing['active'] == listing['total_active'] == 1 and listing['paused_count'] == 0
    assert read_job(service, ended['job_id'])['stage'] == 'completed'
