"""Offline failure scenarios. Real ffmpeg checks synthetic data only; no live music/services."""
import copy
import io
import json
import math
import os
import shutil
import struct
import subprocess
import uuid
import wave
from pathlib import Path

import pytest
from mutagen.flac import FLAC, Picture
from PIL import Image, ImageDraw

from musicops.domain.errors import Conflict, PathBlocked, RecoveryRequired, ValidationFailed
from musicops.domain.identity import match_track, recording_identity, release_identity
from musicops.domain.quality import compare_quality, propose_duplicates
from musicops.library.index import index_library, sha256_file
from musicops.library.paths import release_path, safe_destination
from musicops.library.resources import companion_relations, rewrite_cue_reference
from musicops.library.transactions import TransactionEngine
from musicops.media.validation import MediaValidator, prepare_media_copy
from musicops.state.store import StateStore


@pytest.fixture(scope='session')
def media_tools():
    ffmpeg, ffprobe = shutil.which('ffmpeg'), shutil.which('ffprobe')
    assert ffmpeg and ffprobe, 'V20真实合成媒体测试必须具备验证器，不能静默skip'
    return ffmpeg, ffprobe


def synthetic_flac(path, ffmpeg, *, title='Song', artists=None, disc='1', track='1'):
    path.parent.mkdir(parents=True, exist_ok=True)
    wav = path.with_suffix('.wav')
    with wave.open(str(wav), 'wb') as output:
        output.setparams((1, 2, 44100, 0, 'NONE', 'not compressed'))
        output.writeframes(b''.join(struct.pack('<h', int(16000 * math.sin(2 * math.pi * 439 * i / 44100))) for i in range(88200)))
    value = subprocess.run([ffmpeg, '-v', 'error', '-i', str(wav), '-c:a', 'flac', '-n', str(path)], capture_output=True,
                           creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
    assert value.returncode == 0
    wav.unlink()
    tags = FLAC(path)
    tags['title'] = [title]
    tags['artist'] = artists or ['Artist A', 'Artist B']
    tags['albumartist'] = ['Various Artists']
    tags['album'] = ['Album']
    tags['tracknumber'] = [track]
    tags['discnumber'] = [disc]
    tags['edition'] = ['Original']
    tags['musicops_private'] = ['one', 'two']
    tags.save()
    return path


@pytest.fixture
def core_env(tmp_path, media_tools):
    roots = {k: str(tmp_path / k) for k in ('library', 'source', 'work', 'backup', 'quarantine')}
    roots['tools'] = {'ffmpeg': media_tools[0], 'ffprobe': media_tools[1]}
    for key in ('library', 'source', 'work', 'backup', 'quarantine'):
        Path(roots[key]).mkdir()
    source = synthetic_flac(Path(roots['source']) / '01 Song.flac', media_tools[0])
    store = StateStore(tmp_path / 'state/musicops.sqlite')
    validator = MediaValidator(*media_tools)
    engine = TransactionEngine(store, roots, validator)
    return roots, source, store, validator, engine


def target(**updates):
    value = {'schema_version': 1, 'target_id': 'fixture:1', 'raw_title': 'Song', 'artists': ['Artist A', 'Artist B'], 'duration': 2,
             'album': 'Album', 'edition': 'Original', 'recording_version': '', 'disc': '1', 'track': '1'}
    value.update(updates)
    return value


def file_record(file_id='file-1', **updates):
    value = target()
    value.pop('target_id')
    value.update(file_id=file_id, location_state='library', sha256='samehash', validation={'decode_state': 'passed', 'roon_compatible': True},
                 media_info={'codec_name': 'flac', 'channels': 1, 'bits_per_raw_sample': 16})
    value.update(updates)
    return value


def plan(source, destination='Artist/Album/01 Song.flac', *, before_hash=None, kind='publish', plan_id=None, **operation):
    return {'schema_version': 1, 'plan_id': plan_id or str(uuid.uuid4()), 'config_hash': 'fixture-config', 'operations': [
        {'operation_id': str(uuid.uuid4()), 'source': str(source), 'destination': destination, 'before_hash': before_hash, 'kind': kind, **operation}]}


def test_V06_same_name_different_artist_duration_is_only_gate():
    different = file_record(artists=['Other Artist'])
    assert match_track(target(), [different])['main_state'] == 'missing'
    different_recording = file_record(external_ids={'isrc': 'BBB'})
    assert match_track(target(external_ids={'isrc': 'AAA'}), [different_recording])['main_state'] == 'ambiguous'
    assert match_track(target(), [file_record(duration=5.5)])['main_state'] == 'ambiguous'


@pytest.mark.parametrize('version', ['Live', 'Remix', 'Remaster', 'Deluxe', 'Instrumental'])
def test_V07_original_live_remix_remaster_deluxe_are_separate(version):
    candidate = file_record(raw_title=f'Song ({version})', recording_version=version)
    assert match_track(target(), [candidate])['main_state'] == 'version_mismatch'
    candidate = file_record(edition=version)
    assert match_track(target(), [candidate])['main_state'] == 'version_mismatch'
    assert propose_duplicates([file_record(), candidate])[0]['action'] == 'review'


def test_V08_filename_tag_conflict_unknown_artist_and_verified():
    assert match_track(target(), [file_record(filename_tag_conflict=True)])['main_state'] == 'ambiguous'
    assert match_track(target(), [file_record(artists=[])])['main_state'] == 'ambiguous'
    assert match_track(target(), [file_record(artists=['Unknown Artist'])])['main_state'] == 'ambiguous'
    assert match_track(target(), [file_record()])['main_state'] == 'verified_present'
    assert match_track(target(snapshot_complete=False), [file_record()])['main_state'] == 'snapshot_incomplete'
    assert match_track(target(source_available=False), [])['main_state'] == 'source_unavailable'


def test_V09_multivalues_multidisc_raw_pictures_and_counts(core_env, media_tools):
    roots, source, store, validator, engine = core_env
    first = Path(roots['library']) / 'Album/CD1/01 Song.flac'
    second = Path(roots['library']) / 'Album/CD2/01 Song.flac'
    synthetic_flac(first, media_tools[0], disc='1')
    synthetic_flac(second, media_tools[0], disc='2')
    image = Image.new('RGB', (400, 400), 'blue')
    ImageDraw.Draw(image).rectangle((50, 50, 250, 250), fill='yellow')
    bytes_io = io.BytesIO(); image.save(bytes_io, 'JPEG')
    audio = FLAC(first); picture = Picture(); picture.data = bytes_io.getvalue(); picture.mime = 'image/jpeg'; picture.type = 3; audio.add_picture(picture); audio.save()
    records = index_library(roots['library'], validator)
    assert len(records) == 2
    assert sum(1 for f in records if f['media_info']['codec_name'] == 'flac') == len(records)
    assert records[0]['artists'] == ['Artist A', 'Artist B']
    assert records[0]['albumartist'] == ['Various Artists']
    assert records[0]['raw_tags']['musicops_private'] == ['one', 'two']
    assert records[0]['pictures'][0]['fields']['data']['encoding'] == 'base64'
    assert {r['disc'] for r in records} == {'1', '2'}
    one = release_path('Various Artists', 'Album', 'Song', track=1, disc=1, multidisc=True, release_id='release-1')
    two = release_path('Various Artists', 'Album', 'Song', track=1, disc=2, multidisc=True, release_id='release-1')
    assert one['relative_path'] != two['relative_path']
    assert match_track(target(), [records[0]])['main_state'] == 'verified_present'


def test_V20_real_middle_tail_corruption_missing_tools_and_unsupported(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    assert validator.validate(source)['decode_state'] == 'passed'
    original = source.read_bytes()
    middle = Path(roots['work']) / 'middle.flac'; data = bytearray(original); pos = len(data) // 2
    data[pos:pos+1000] = bytes(byte ^ 0xAA for byte in data[pos:pos+1000]); middle.write_bytes(data)
    tail = Path(roots['work']) / 'tail.flac'; tail.write_bytes(original[:-500])
    assert validator.validate(middle)['decode_state'] == 'corrupt'
    assert validator.validate(tail)['decode_state'] == 'corrupt'
    missing = MediaValidator('musicops-no-such-ffmpeg', 'musicops-no-such-ffprobe')
    assert missing.validate(source)['decode_state'] == 'tool_error'
    import musicops.media.validation as module
    actual = module._run
    def unsupported(command, timeout):
        if '-show_streams' in command:
            return subprocess.CompletedProcess(command, 1, b'', b'Unsupported version')
        return actual(command, timeout)
    monkeypatch.setattr(module, '_run', unsupported)
    unsupported_result = MediaValidator(validator.ffmpeg, validator.ffprobe).validate(source)
    assert unsupported_result['decode_state'] == 'unsupported'
    assert unsupported_result['roon_compatible'] is False


def test_V20_timeout_is_not_corrupt(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    import musicops.media.validation as module
    actual = module._run
    def timeout(command, timeout):
        if '-map' in command:
            raise subprocess.TimeoutExpired(command, timeout)
        return actual(command, timeout)
    monkeypatch.setattr(module, '_run', timeout)
    assert validator.validate(source)['decode_state'] == 'timeout'


def test_V21_replacement_same_path_same_size_same_mtime_invalidates_hash_cache(core_env):
    roots, source, store, validator, engine = core_env
    first = validator.validate(source)
    assert not first['cache_hit']
    assert validator.validate(source)['cache_hit']
    timestamp = source.stat().st_mtime_ns
    data = bytearray(source.read_bytes()); data[len(data)//2] ^= 0xFF; source.write_bytes(data)
    os.utime(source, ns=(timestamp, timestamp))
    second = validator.validate(source)
    assert second['file_hash'] != first['file_hash']
    assert not second['cache_hit'] and second['validation_id'] != first['validation_id']
    # A one-byte change can be tag/padding data and remain decodable; cache invalidation must happen either way.
    assert second['decode_state'] in {'passed', 'corrupt'}


def test_V22_real_copy_tags_conversion_preserve_seeding_source(core_env):
    roots, source, store, validator, engine = core_env
    before = sha256_file(source)
    staged = Path(roots['work']) / 'changed.flac'
    prepare_media_copy(source, staged, validator)
    tags = FLAC(staged); tags['album'] = ['Corrected']; tags.save()
    assert not os.path.samefile(source, staged)
    assert sha256_file(source) == before
    assert FLAC(source)['album'] == ['Album']
    assert FLAC(staged)['musicops_private'] == ['one', 'two']
    converted = Path(roots['work']) / 'converted.flac'
    result = prepare_media_copy(source, converted, validator, convert_to='flac')
    assert result['converted'] and validator.pcm_hash(source) == validator.pcm_hash(converted)
    assert sha256_file(source) == before


def test_V29_lyrics_images_shared_cover_and_cue_reference_relationships(core_env):
    roots, source, store, validator, engine = core_env
    audio = Path(roots['library']) / 'Album/01 Song.flac'; audio.parent.mkdir(); shutil.copy2(source, audio)
    (audio.parent / '01 Song.lrc').write_text('[00:00.00]Song', encoding='utf-8')
    (audio.parent / '01 Song.jpg').write_bytes(b'fake-for-relationship-only')
    (audio.parent / 'cover.jpg').write_bytes(b'shared-cover')
    cue = audio.parent / 'whole.cue'; cue.write_text('FILE "01 Song.flac" WAVE\n  TRACK 01 AUDIO\n    INDEX 01 00:00:00\n', encoding='utf-8')
    relations = companion_relations(audio)
    assert sum(r['type'] == 'lyrics' for r in relations) == 1
    assert sum(r['type'] == 'cover' for r in relations) == 2
    assert all(r['shared'] for r in relations if r['type'] == 'cover')
    assert next(r for r in relations if r['type'] == 'cue')['file_references'] == ['01 Song.flac']
    new = rewrite_cue_reference(cue.read_text(), '01 Song.flac', '01 Renamed.flac', audio.parent)
    assert 'FILE "01 Renamed.flac" WAVE' in new
    with pytest.raises(PathBlocked):
        rewrite_cue_reference(cue.read_text(), '01 Song.flac', '../outside.flac', audio.parent)


@pytest.mark.parametrize('name', ['CON.flac', 'a/NUL.lrc', '../outside.flac', 'C:\\outside.flac', 'a/track.flac:stream', 'a /track.flac', 'a./track.flac', 'x'*240+'.flac'])
def test_V30_windows_reserved_invalid_absolute_long_paths(core_env, name):
    roots, source, store, validator, engine = core_env
    with pytest.raises(PathBlocked):
        safe_destination(roots['library'], name)


def test_V30_case_collision_and_junction_cross_boundary(core_env):
    roots, source, store, validator, engine = core_env
    (Path(roots['library']) / 'Album').mkdir()
    with pytest.raises(PathBlocked):
        safe_destination(roots['library'], 'album/track.flac')
    junction = Path(roots['library']) / 'escape'
    if os.name == 'nt':
        # Creating a directory junction is not a data move; target is an isolated synthetic source root.
        command = subprocess.run(['cmd', '/c', 'mklink', '/J', str(junction), roots['source']], capture_output=True,
                                 creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))
        assert command.returncode == 0, 'Junction scenario must be tested, not silently skipped'
    else:
        junction.symlink_to(roots['source'], target_is_directory=True)
    with pytest.raises(PathBlocked):
        safe_destination(roots['library'], 'escape/01 Song.flac')
    assert index_library(roots['library'], validator) == []


@pytest.mark.parametrize('phase,expected', [('after_backup','ROLLED_BACK'), ('after_copy','ROLLED_BACK'), ('after_publish','COMMITTED'), ('before_commit','COMMITTED')])
def test_V31_manifest_recovery_at_copy_publish_and_commit_boundaries(core_env, phase, expected):
    roots, source, store, validator, engine = core_env
    proposed = plan(source)
    with pytest.raises(RecoveryRequired):
        engine.execute(proposed, fault=phase)
    assert store.list('transactions')[0]['state'] == 'RECOVERY_REQUIRED'
    recovered = TransactionEngine(store, roots, validator).recover()
    assert recovered[0]['state'] == expected
    target_path = Path(roots['library']) / proposed['operations'][0]['destination']
    assert target_path.exists() == (expected == 'COMMITTED')
    if expected == 'COMMITTED':
        assert engine.execute(proposed)['idempotent']


def test_V31_partial_copy_and_partial_batch_are_rolled_back(core_env):
    roots, source, store, validator, engine = core_env
    proposed = plan(source)
    (Path(roots['library'])/'Artist/Album').mkdir(parents=True)
    proposed['operations'].append({**proposed['operations'][0], 'operation_id': str(uuid.uuid4()), 'destination': 'Artist/Album/02 Song.flac'})
    with pytest.raises(RecoveryRequired):
        engine.execute(proposed, fault='after_publish')
    assert engine.recover()[0]['state'] == 'ROLLED_BACK'
    assert not any(Path(roots['library']).rglob('*.flac'))


def test_V31_new_release_directory_is_complete_before_single_atomic_rename(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    import musicops.library.transactions as module
    proposed=plan(source,'Artist/Album/CD1/01 Song.flac')
    proposed['operations'].append({**proposed['operations'][0],'operation_id':str(uuid.uuid4()),'destination':'Artist/Album/CD2/01 Song.flac'})
    actual=module.os.replace; seen=[]
    def inspect_directory_rename(src,dst):
        if Path(src).is_dir() and str(dst).startswith(roots['library']):
            expected=['CD1\\01 Song.flac','CD2\\01 Song.flac'] if os.name=='nt' else ['CD1/01 Song.flac','CD2/01 Song.flac']
            assert sorted(str(p.relative_to(src)) for p in Path(src).rglob('*.flac')) == expected
            assert not Path(dst).exists(); seen.append((str(src),str(dst)))
        return actual(src,dst)
    monkeypatch.setattr(module.os,'replace',inspect_directory_rename)
    tx=engine.execute(proposed)
    assert len(seen)==1 and tx['directory_publication']['renamed']
    assert len(list(Path(roots['library']).rglob('*.flac')))==2
    engine.rollback(tx['tx_id']); assert not (Path(roots['library'])/'Artist').exists()


def test_V31_physical_cross_volume_source_copy_target_local_rename(tmp_path, media_tools):
    destination=os.environ.get('MUSICOPS_TEST_CROSS_VOLUME_ROOT','')
    if os.name!='nt' or not destination or not Path(destination).is_absolute() or not Path(destination).is_dir():
        pytest.skip('Set MUSICOPS_TEST_CROSS_VOLUME_ROOT to an approved temporary directory on a different Windows volume')
    approved=Path(destination).resolve()
    if approved.stat().st_dev == tmp_path.stat().st_dev:
        pytest.skip('The approved test directory is on the same volume as pytest temporary files')
    root=approved/str(uuid.uuid4())
    root.mkdir(parents=True)
    try:
        roots={k:str(root/k) for k in ('library','work','backup','quarantine')}; roots['source']=str(tmp_path/'source')
        roots['tools']={'ffmpeg':media_tools[0],'ffprobe':media_tools[1]}
        for key in ('library','work','backup','quarantine','source'): Path(roots[key]).mkdir()
        source=synthetic_flac(Path(roots['source'])/'01 Song.flac',media_tools[0])
        before=sha256_file(source); store=StateStore(tmp_path/'state.db'); engine=TransactionEngine(store,roots)
        assert Path(roots['source']).stat().st_dev != Path(roots['library']).stat().st_dev
        tx=engine.execute(plan(source)); assert tx['state']=='COMMITTED' and tx['directory_publication']['renamed']
        assert sha256_file(tx['operations'][0]['destination'])==before
        engine.rollback(tx['tx_id']); assert sha256_file(source)==before
    finally:
        # Verify exact absolute boundary before recursive cleanup of this synthetic fixture.
        resolved=root.resolve(); assert resolved.is_relative_to(approved) and resolved!=approved
        shutil.rmtree(resolved)


def test_V31_actual_copy_interruption_leaves_original_and_recovery_is_safe(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    import musicops.library.transactions as module
    actual = module.durable_copy
    source_hash = sha256_file(source)
    def partial_copy(source_path, destination):
        if 'staging' in Path(destination).parts:
            Path(destination).parent.mkdir(parents=True, exist_ok=True)
            Path(destination).write_bytes(Path(source_path).read_bytes()[:1000])
            raise OSError('offline mid-copy interruption')
        return actual(source_path, destination)
    monkeypatch.setattr(module, 'durable_copy', partial_copy)
    with pytest.raises(OSError):
        engine.execute(plan(source))
    monkeypatch.setattr(module, 'durable_copy', actual)
    assert engine.recover()[0]['state'] == 'ROLLED_BACK'
    assert sha256_file(source) == source_hash and not any(Path(roots['library']).rglob('*.flac'))


def test_V31_power_loss_after_rename_before_manifest_adopts_fresh_hashes(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    import musicops.library.transactions as module
    actual = module.os.replace
    fired = False
    def crash_after_rename(src, dst):
        nonlocal fired
        actual(src, dst)
        if not fired and str(dst).startswith(roots['library']):
            fired = True
            raise SystemExit('offline sudden power-loss fixture')
    monkeypatch.setattr(module.os, 'replace', crash_after_rename)
    proposed = plan(source)
    with pytest.raises(SystemExit):
        engine.execute(proposed)
    monkeypatch.setattr(module.os, 'replace', actual)
    tx = store.list('transactions')[0]
    assert tx['state'] == 'PUBLISHING' and not tx['operations'][0]['published']
    assert engine.recover()[0]['state'] == 'COMMITTED'
    assert engine.execute(proposed)['idempotent']


def test_V31_initial_database_intent_failure_creates_no_backup_or_publication(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    monkeypatch.setattr(store, 'put', lambda *args: (_ for _ in ()).throw(OSError('offline intent write failure')))
    with pytest.raises(OSError):
        engine.execute(plan(source))
    assert list(Path(roots['backup']).iterdir()) == []
    assert not any(Path(roots['library']).rglob('*.flac'))


def test_V31_database_failure_after_publish_never_fakes_commit(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    actual = store.put
    def fail_commit(kind, key, value):
        if kind == 'transactions' and value.get('state') == 'COMMITTED':
            raise OSError('offline database failure')
        return actual(kind, key, value)
    monkeypatch.setattr(store, 'put', fail_commit)
    with pytest.raises(OSError):
        engine.execute(plan(source))
    tx = store.list('transactions')[0]
    assert tx['state'] != 'COMMITTED'
    assert Path(tx['operations'][0]['destination']).is_file()
    monkeypatch.setattr(store, 'put', actual)
    assert engine.recover()[0]['state'] == 'COMMITTED'


def test_V32_fresh_write_and_rollback_hash_conflicts_preserve_external_bytes(core_env):
    roots, source, store, validator, engine = core_env
    destination = Path(roots['library']) / '01 Song.flac'; shutil.copy2(source, destination)
    before = sha256_file(destination)
    changed = plan(source, destination.name, before_hash=before, kind='replace')
    destination.write_bytes(b'external-before-write')
    with pytest.raises(Conflict):
        engine.execute(changed)
    assert destination.read_bytes() == b'external-before-write'
    destination.unlink()
    tx = engine.execute(plan(source, destination.name))
    destination.write_bytes(b'external-after-publish')
    with pytest.raises(Conflict):
        engine.rollback(tx['tx_id'])
    assert destination.read_bytes() == b'external-after-publish'
    assert store.get('transactions',tx['tx_id'])['state'] == 'CONFLICT'


def test_V33_same_plan_operation_identity_is_idempotent_and_plan_id_reuse_blocked(core_env):
    roots, source, store, validator, engine = core_env
    proposed = plan(source)
    first = engine.execute(proposed)
    second = engine.execute(proposed)
    assert first['tx_id'] == second['tx_id'] and second['idempotent']
    assert len(list(Path(roots['library']).rglob('*.flac'))) == 1
    changed = copy.deepcopy(proposed); changed['config_hash'] = 'different-config'
    with pytest.raises(Conflict):
        engine.execute(changed)
    operation_reuse = copy.deepcopy(proposed); operation_reuse['plan_id'] = 'other-plan'; operation_reuse['operations'][0]['destination'] = 'other.flac'
    with pytest.raises(Conflict):
        engine.execute(operation_reuse)
    assert store.list('token_reservations') == []


@pytest.mark.parametrize('reason', ['duplicate', 'corrupt', 'manual_suppression'])
def test_V34_isolation_reasons_not_healthy_coverage_or_reverse_sync(core_env, reason):
    roots, source, store, validator, engine = core_env
    destination = Path(roots['library']) / '01 Song.flac'; shutil.copy2(source, destination)
    tx = engine.execute(plan(destination, 'isolated/01 Song.flac', before_hash=sha256_file(destination), kind='quarantine', quarantine_reason=reason))
    assert not destination.exists()
    assert store.list('suppressions')[0]['reason'] == reason
    quarantined = file_record(location_state='quarantine', quarantine_reason=reason)
    decision = match_track(target(), [quarantined])
    assert decision['main_state'] == 'missing' and reason in decision['flags']
    engine.rollback(tx['tx_id'])
    assert destination.is_file() and not store.list('suppressions')[0]['active']
    assert sha256_file(source) == sha256_file(destination)


def test_V35_full_original_media_lyrics_cover_cue_multivalues_and_state_restore(core_env):
    roots, source, store, validator, engine = core_env
    directory = Path(roots['library']) / 'Album'; directory.mkdir()
    audio = directory / source.name; shutil.copy2(source, audio)
    lyrics = directory / '01 Song.lrc'; lyrics.write_text('[00:00.00]Original\n', encoding='utf-8')
    cue = directory / 'Album.cue'; cue.write_text('FILE "01 Song.flac" WAVE\n TRACK 01 AUDIO\n INDEX 01 00:00:00\n', encoding='utf-8')
    image = Image.new('RGB',(400,400),'blue'); ImageDraw.Draw(image).rectangle((10,10,200,200),fill='orange'); image.save(directory/'cover.jpg')
    originals = {p.name: p.read_bytes() for p in directory.iterdir()}
    source_hash = sha256_file(source)
    for row in index_library(roots['library'], validator):
        store.put('library_files', row['file_id'], row)
    original_row = store.list('library_files')[0]
    staged_audio = Path(roots['work']) / source.name; shutil.copy2(source, staged_audio)
    tags = FLAC(staged_audio); tags['album'] = ['Changed']; tags['artist'] = ['New A','New B']; tags.save()
    staged_lrc = Path(roots['work']) / lyrics.name; staged_lrc.write_text('[00:00.00]Changed\n',encoding='utf-8')
    proposed = plan(staged_audio, 'Album/'+audio.name, before_hash=sha256_file(audio), kind='replace')
    proposed['operations'].append({'operation_id':str(uuid.uuid4()), 'source':str(staged_lrc), 'destination':'Album/'+lyrics.name, 'before_hash':sha256_file(lyrics), 'kind':'replace'})
    tx = engine.execute(proposed)
    assert {b['role'] for b in tx['backup_manifest']} >= {'affected','cover','lyrics','cue'}
    assert all(Path(b['backup']).read_bytes() == originals[Path(b['path']).name] for b in tx['backup_manifest'])
    result = engine.rollback(tx['tx_id'])
    assert result['state'] == 'ROLLED_BACK'
    assert {p.name:p.read_bytes() for p in directory.iterdir()} == originals
    assert FLAC(audio)['artist'] == ['Artist A','Artist B']
    assert store.get('library_files',original_row['file_id']) == original_row
    assert sha256_file(source) == source_hash


def test_V35_quarantine_single_track_keeps_shared_cover_and_cue(core_env):
    roots, source, store, validator, engine = core_env
    directory = Path(roots['library'])/'Album'; directory.mkdir(); audio=directory/source.name; shutil.copy2(source,audio)
    lyrics=audio.with_suffix('.lrc'); lyrics.write_text('[00:00.00]Song',encoding='utf-8')
    (directory/'cover.jpg').write_bytes(b'shared-resource')
    (directory/'Album.cue').write_text('FILE "01 Song.flac" WAVE\n',encoding='utf-8')
    proposed=plan(audio,'Album/'+audio.name,before_hash=sha256_file(audio),kind='quarantine',quarantine_reason='duplicate')
    proposed['operations'].append({'operation_id':str(uuid.uuid4()),'source':str(lyrics),'destination':'Album/'+lyrics.name,'before_hash':sha256_file(lyrics),'kind':'quarantine','quarantine_reason':'duplicate'})
    tx=engine.execute(proposed)
    assert (directory/'cover.jpg').exists() and (directory/'Album.cue').exists()
    assert not audio.exists() and not lyrics.exists()
    engine.rollback(tx['tx_id']); assert audio.exists() and lyrics.exists()


def test_V35_rollback_power_loss_continues_restore_not_recommit(core_env, monkeypatch):
    roots, source, store, validator, engine = core_env
    import musicops.library.transactions as module
    proposed = plan(source)
    proposed['operations'].append({**proposed['operations'][0], 'operation_id': str(uuid.uuid4()), 'destination': 'Artist/Album/02 Song.flac'})
    tx = engine.execute(proposed)
    actual = module.Path.unlink
    fired = False
    def crash_after_delete(path, *args, **kwargs):
        nonlocal fired
        actual(path, *args, **kwargs)
        if not fired and str(path).startswith(roots['library']):
            fired = True
            raise SystemExit('offline rollback power-loss fixture')
    monkeypatch.setattr(module.Path, 'unlink', crash_after_delete)
    with pytest.raises(SystemExit):
        engine.rollback(tx['tx_id'])
    monkeypatch.setattr(module.Path, 'unlink', actual)
    assert engine.recover()[0]['state'] == 'ROLLED_BACK'
    assert not any(Path(roots['library']).rglob('*.flac'))


def test_V35_rollback_withdraws_reindexed_track_coverage(core_env):
    roots, source, store, validator, engine = core_env
    item=target(); store.put('target_tracks',item['target_id'],item)
    tx=engine.execute(plan(source))
    files=index_library(roots['library'],validator)
    for row in files: store.put('library_files',row['file_id'],row)
    decision=match_track(item,files); assert decision['main_state']=='verified_present'
    store.put('match_decisions',item['target_id'],decision)
    engine.rollback(tx['tx_id'])
    assert store.get('match_decisions',item['target_id'])['main_state']=='missing'


def test_V09_recording_release_aliases_ape_multivalue_and_numeric_title(core_env):
    roots, source, store, validator, engine = core_env
    from musicops.library.index import tag_fields, _filename_evidence
    from mutagen.apev2 import APETextValue
    fields = tag_fields({'title':APETextValue('Song'), 'artist':APETextValue('A\x00B'), 'recording_version':['Live'], 'edition':['Deluxe'], 'release_id':['release-1'], 'date':['1990']})
    assert fields['artists'] == ['A','B']
    assert fields['recording_version'] == 'Live' and fields['edition'] == 'Deluxe' and fields['release_id'] == 'release-1'
    evidence = _filename_evidence(Path('99 Luftballons.flac'), {'raw_title':'99 Luftballons', 'track':'1'})
    assert not evidence['filename_tag_conflict']


def test_T16_resources_same_content_renamed_lyrics_do_not_change_identity():
    a=file_record('a',resource_hashes={'Song.lrc':'lrc-hash','cover.jpg':'cover-hash'})
    b=file_record('b',resource_hashes={'Renamed.lrc':'lrc-hash','cover.jpg':'cover-hash'})
    assert propose_duplicates([a,b])[0]['action']=='quarantine_candidate'
    b['resource_hashes'].pop('Renamed.lrc')
    assert propose_duplicates([a,b])[0]['action']=='review'


def test_V40_publish_cancel_rollback_ratio_have_no_client_commands(core_env):
    roots, source, store, validator, engine = core_env
    source_hash = sha256_file(source)
    tx = engine.execute(plan(source, client_ratio=9999, client_seed_time=9999))
    assert tx['client_actions'] == []
    engine.rollback(tx['tx_id'])
    assert tx['client_actions'] == [] and sha256_file(source) == source_hash
    assert not store.list('download_tasks') and not store.list('token_reservations')


def test_V31_each_meaningful_transaction_step_has_requirement_result_and_artifact(core_env):
    roots, source, store, validator, engine = core_env
    tx=engine.execute(plan(source)); engine.rollback(tx['tx_id'])
    events=[row for row in store.events() if row['tx_id']==tx['tx_id']]
    phases={row['phase'] for row in events}
    assert phases >= {'transaction_planned','transaction_prepared','work_copy_validated','transaction_validated','publication_intent','directory_published','publication_verified','transaction_committed','rollback_intent','file_rollback_verified','transaction_rolled_back'}
    assert all(row['payload']['requirements'] and row['payload']['action'] and row['payload']['result'] and row['payload']['artifact'] for row in events)


def test_V31_staging_copy_hash_corruption_stops_before_publish(core_env, monkeypatch):
    roots, source, store, validator, engine=core_env
    import musicops.library.transactions as module
    actual=module.durable_copy
    def wrong_bytes(src,dst):
        actual(src,dst)
        if 'staging' in Path(dst).parts:
            data=bytearray(Path(dst).read_bytes()); data[100]^=0xFF; Path(dst).write_bytes(data)
    monkeypatch.setattr(module,'durable_copy',wrong_bytes)
    with pytest.raises(Conflict): engine.execute(plan(source))
    assert not any(Path(roots['library']).rglob('*.flac'))


def test_T16_quality_damage_and_different_master_never_win():
    good = file_record('good', media_info={'codec_name':'flac','channels':2,'bits_per_raw_sample':16})
    damaged = file_record('bad', media_info={'codec_name':'flac','channels':2,'bits_per_raw_sample':24},validation={'decode_state':'corrupt','roon_compatible':True})
    assert compare_quality(good,damaged,{'prefer_24bit':True})['preferred_file_id'] == 'good'
    other_master = file_record('other',edition='Remaster')
    assert compare_quality(good,other_master)['action'] == 'coexist'
    no_content_evidence = file_record('different',sha256='different')
    assert propose_duplicates([good,no_content_evidence]) == []


def test_V06_quality_duration_external_identity_and_filename_conflicts_require_review():
    a=file_record('a')
    b=file_record('b',duration=120)
    assert compare_quality(a,b)['action']=='coexist'
    b=file_record('b',external_ids={'isrc':'B'}); a['external_ids']={'isrc':'A'}
    assert propose_duplicates([a,b])[0]['action']=='review'
    b=file_record('b',filename_tag_conflict=True)
    assert propose_duplicates([file_record('a'),b])[0]['action']=='review'
    assert match_track(target(artists=['Unknown Artist']),[file_record()])['main_state']=='ambiguous'
