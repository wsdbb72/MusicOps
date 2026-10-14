"""Online scans provide bounded metadata evidence, never full audio verification."""
import json
from types import SimpleNamespace

import pytest

from musicops.domain.identity import match_track
from musicops.library.index import index_library


def audio_fixture(**overrides):
    values = dict(tags={'title': ['Song'], 'artist': ['Artist'], 'album': ['Album']},
                  info=SimpleNamespace(length=120.0, sample_rate=44100, channels=2, bits_per_sample=16), pictures=[])
    values.update(overrides)
    return SimpleNamespace(**values)


def test_metadata_index_does_not_hash_decode_or_read_cue(tmp_path, monkeypatch):
    import mutagen
    import musicops.library.index as module
    track = tmp_path / 'Song.flac'
    track.write_bytes(b'container fixture')
    (tmp_path / 'Song.lrc').write_text('[00:01]fixture', encoding='utf-8')
    (tmp_path / 'cover.jpg').write_bytes(b'cover fixture')
    (tmp_path / 'Album.cue').write_text('FILE "Song.flac" WAVE', encoding='utf-8')
    def forbidden(*args, **kwargs):
        pytest.fail('metadata scan performed full hashing, decoding, or CUE parsing')
    monkeypatch.setattr(module, 'sha256_file', forbidden)
    monkeypatch.setattr(type(tmp_path), 'read_text', forbidden)
    tags = {'title': ['Song'], 'artist': ['Artist'], 'album': ['Album'], 'binary': b'x' * 524288}
    monkeypatch.setattr(mutagen, 'File', lambda *a, **k: audio_fixture(tags=tags, pictures=[SimpleNamespace(data=b'y' * 524288)]))
    row = index_library(tmp_path, validator=SimpleNamespace(validate=forbidden), metadata_only=True)[0]
    assert row['sha256'] is None and row['metadataOnly'] is True
    assert row['evidence_level'] == 'local_metadata_readonly'
    assert row['validation'] == {'decode_state': 'not_run', 'roon_compatible': None}
    assert row['raw_tags']['binary']['byte_length'] == 524288
    assert len(json.dumps(row, allow_nan=False)) < 10000
    assert row['resource_hashes'] == {} and row['resource_signatures'] == []
    assert {c['type'] for c in row['companions']} == {'lyrics', 'cover', 'cue'}
    assert all(c['sha256'] is None and c['size'] > 0 for c in row['companions'])
    assert next(c for c in row['companions'] if c['type'] == 'cue')['relation_verified'] is False
    target = {'raw_title': 'Song', 'artists': ['Artist'], 'album': 'Album', 'duration': 120.0}
    assert match_track(target, [row])['main_state'] == 'ambiguous'


def test_metadata_scan_checkpoint_is_bounded_and_cancellation_propagates(tmp_path, monkeypatch):
    import mutagen
    for n in range(101):
        (tmp_path / f'{n:03d}.flac').write_bytes(b'fixture')
    reads = []
    checkpoints = []
    monkeypatch.setattr(mutagen, 'File', lambda path, **k: reads.append(path) or audio_fixture())
    rows = index_library(tmp_path, metadata_only=True, checkpoint=lambda: checkpoints.append(len(reads)))
    assert len(rows) == 101 and checkpoints == [0, 50, 100, 101]
    reads.clear()
    class Cancelled(Exception):
        pass
    def checkpoint():
        if len(reads) >= 50:
            raise Cancelled()
    with pytest.raises(Cancelled):
        index_library(tmp_path, metadata_only=True, checkpoint=checkpoint)
    assert len(reads) == 50


def test_metadata_scan_retries_changed_file_without_stale_busy_flag(tmp_path, monkeypatch):
    import mutagen
    import musicops.library.index as module
    source = tmp_path / 'Song.flac'
    source.write_bytes(b'fixture')
    original = module.file_version
    calls = []
    def changed(path):
        calls.append(path)
        value = original(path)
        return (value[0], value[1] + 1, value[2]) if len(calls) == 2 else value
    monkeypatch.setattr(module, 'file_version', changed)
    monkeypatch.setattr(mutagen, 'File', lambda *a, **k: audio_fixture())
    row = index_library(tmp_path, metadata_only=True)[0]
    assert len(calls) == 4 and row['read_error'] is None and row['sha256'] is None


def test_metadata_scan_records_persistent_change_and_invalid_numbers(tmp_path, monkeypatch):
    import mutagen
    import musicops.library.index as module
    (tmp_path / 'Song.flac').write_bytes(b'fixture')
    counter = []
    def changing(path):
        counter.append(path)
        return (7, len(counter), 1)
    monkeypatch.setattr(module, 'file_version', changing)
    monkeypatch.setattr(mutagen, 'File', lambda *a, **k: audio_fixture(
        tags={'title': ['Song'], 'artist': ['Artist'], 'number': float('nan')},
        info=SimpleNamespace(length=float('inf'), channels=True, sample_rate=-1)))
    row = index_library(tmp_path, metadata_only=True)[0]
    assert row['read_error'] == 'BUSY_CHANGED_DURING_INDEX' and len(counter) == 4
    assert row['duration'] is None and 'length' not in row['media_info']
    assert set(row['metadata_warnings']) == {'INVALID_MEDIA_INFO:length', 'INVALID_MEDIA_INFO:channels', 'INVALID_MEDIA_INFO:sample_rate'}
    json.dumps(row, allow_nan=False)


def test_metadata_scan_excludes_reparse_and_selected_directory(tmp_path, monkeypatch):
    import mutagen
    import musicops.library.index as module
    excluded = tmp_path / 'excluded'
    excluded.mkdir()
    (excluded / 'Hidden.flac').write_bytes(b'fixture')
    (tmp_path / 'Song.flac').write_bytes(b'fixture')
    (tmp_path / 'Linked.flac').write_bytes(b'fixture')
    original = module.reparse
    monkeypatch.setattr(module, 'reparse', lambda path: path.name == 'Linked.flac' or original(path))
    monkeypatch.setattr(mutagen, 'File', lambda *a, **k: audio_fixture())
    rows = index_library(tmp_path, exclusions=[excluded], metadata_only=True)
    assert [row['relative_path'] for row in rows] == ['Song.flac']


def test_full_index_default_still_hashes_and_uses_validator(tmp_path, monkeypatch):
    import mutagen
    import musicops.library.index as module
    track = tmp_path / 'Song.flac'
    track.write_bytes(b'fixture')
    monkeypatch.setattr(mutagen, 'File', lambda *a, **k: audio_fixture())
    called = []
    def validate(path):
        called.append(path)
        return {'validation_id': 'fixture-validation', 'decode_state': 'passed', 'roon_compatible': True}
    row = index_library(tmp_path, validator=SimpleNamespace(validate=validate))[0]
    assert row['sha256'] == module.sha256_file(track) and called == [track]
    assert 'metadataOnly' not in row and row['validation_id'] == 'fixture-validation'


def test_metadata_scan_raw_tag_summaries_have_a_total_budget(tmp_path, monkeypatch):
    import mutagen
    (tmp_path / 'Song.flac').write_bytes(b'fixture')
    tags = {f'oversized-{n}': ['文本' * 10000] * 50 for n in range(100)}
    monkeypatch.setattr(mutagen, 'File', lambda *a, **k: audio_fixture(tags=tags))
    row = index_library(tmp_path, metadata_only=True)[0]
    assert len(json.dumps(row['raw_tags'], ensure_ascii=False).encode('utf-8')) < 4096
    assert row['raw_tag_count'] == 100 and 'RAW_TAGS_TRUNCATED' in row['metadata_warnings']
