import csv
import io
import json
import zipfile
from xml.sax.saxutils import escape

import pytest

from musicops.providers.playlist_files import import_playlist_file, MAX_BYTES, MAX_TRACKS, PLATFORMS


def write(tmp_path, name, data, encoding='utf-8'):
    path = tmp_path / name
    path.write_bytes(data if isinstance(data, bytes) else data.encode(encoding))
    return path


def canonical(platform='netease', **changes):
    return {'generator': 'PlaylistOut', 'platform': platform, 'id': 'playlist-1', 'name': 'Example list',
            'trackCount': 1, 'tracks': [{'id': '42', 'title': 'Example song', 'artist': 'Singer A, Singer B',
                                        'album': 'Example album', 'durationMs': 180500}], **changes}


@pytest.mark.parametrize('platform', PLATFORMS)
def test_all_ten_platforms_canonical_and_collision_free(tmp_path, platform):
    file = write(tmp_path, 'songs.json', json.dumps(canonical(platform)))
    before = file.read_bytes()
    result = import_playlist_file(file)
    snapshot = result['snapshot']; song = snapshot['songs'][0]
    assert result['status'] == 'OK' and snapshot['complete'] and snapshot['read_only']
    assert snapshot['source'] == song['source'] == platform
    assert song['id'] == song['target_id'] == platform + ':42'
    assert song['source_song_id'] == '42' and song['artists'] == ['Singer A', 'Singer B']
    assert song['duration'] == 180.5 and song['evidence_level'] == 'user_import'
    assert song['download_eligibility'] == 'UNKNOWN'
    assert snapshot['snapshot_id'].startswith(platform + ':file:')
    assert import_playlist_file(file)['snapshot']['snapshot_id'] == snapshot['snapshot_id']
    assert not result['network_used'] and file.read_bytes() == before


def test_partial_manifest_missing_album_and_artist_not_invented(tmp_path):
    payload = canonical('youtube', isPartial=True, trackCount=3)
    payload['tracks'] = [{'id': 'video-1', 'title': 'A video', 'durationMs': 123000}]
    result = import_playlist_file(write(tmp_path, 'list.json', json.dumps(payload)))
    snapshot = result['snapshot']; song = snapshot['songs'][0]
    assert result['status'] == 'SNAPSHOT_INCOMPLETE' and not snapshot['complete']
    assert snapshot['declared_count'] == 3 and snapshot['observed_id_count'] == 1
    assert song['artists'] == [] and song['album'] == '' and song['source_album_id'] is None
    assert set(song['metadata_missing_fields']) == {'artists', 'album'}
    assert any(w['error_class'] == 'COUNT_MISMATCH' for w in snapshot['warnings'])


def test_netease_raw_and_qq_raw_json_shapes(tmp_path):
    netease = {'playlist': {'name': 'NE', 'trackCount': 1, 'tracks': [{'id': 45, 'name': 'Title', 'ar': [{'name': 'Artist'}], 'al': {'id': 9, 'name': 'Album'}, 'dt': 125000}]}}
    song = import_playlist_file(write(tmp_path, 'ne.json', json.dumps(netease)))['snapshot']['songs'][0]
    assert song['target_id'] == 'netease:45' and song['duration'] == 125 and song['source_album_id'] == '9'
    qq = {'data': {'cdlist': [{'dissname': 'QQ List', 'songlist': [{'songmid': 'mid1', 'songname': 'T', 'singer': [{'name': 'A'}], 'albumname': 'B', 'albummid': 'mid9', 'interval': 200}]}]}}
    result = import_playlist_file(write(tmp_path, 'qq.json', json.dumps(qq)))
    song = result['snapshot']['songs'][0]
    assert result['snapshot']['name'] == 'QQ List' and song['target_id'] == 'qqmusic:mid1'
    assert song['album'] == 'B' and song['source_album_id'] == 'mid9' and song['duration'] == 200


def test_spotify_wrapped_json_and_legacy_netease_duration(tmp_path):
    spotify = {'name': 'S', 'tracks': {'total': 1, 'items': [{'track': {'id': 'abc', 'name': 'T', 'artists': [{'name': 'A'}], 'album': {'name': 'B', 'id': 'alb'}, 'duration_ms': 251000, 'uri': 'spotify:track:abc'}}]}}
    song = import_playlist_file(write(tmp_path, 'spotify.json', json.dumps(spotify)))['snapshot']['songs'][0]
    assert song['target_id'] == 'spotify:abc' and song['duration'] == 251
    legacy = [{'id': 4, 'name': 'T', 'artists': [{'name': 'A'}], 'album': {'name': 'B'}, 'duration': 180000}]
    song = import_playlist_file(write(tmp_path, 'legacy.json', json.dumps(legacy)), 'netease')['snapshot']['songs'][0]
    assert song['duration'] == 180


def test_exportify_old_and_new_headers_youtube_gist_table_and_chinese_tsv(tmp_path):
    old = 'Track Name,Artist Name,Album Name,Track URI,Duration (ms)\nSong,Artist,Album,spotify:track:abc,222000\n'
    song = import_playlist_file(write(tmp_path, 'old.csv', old))['snapshot']['songs'][0]
    assert song['target_id'] == 'spotify:abc' and song['duration'] == 222
    new = 'Track Name,Artist Name(s),Album Name,Track URI,Duration (ms)\nSong,A;B,Album,spotify:track:abc,222000\n'
    song = import_playlist_file(write(tmp_path, 'new.csv', new))['snapshot']['songs'][0]
    assert song['artists'] == ['A', 'B']
    yt = [['TITLE', 'ARTIST', 'ALBUM'], ['A video', 'Creator', 'Album']]
    song = import_playlist_file(write(tmp_path, 'yt.json', json.dumps(yt)), 'youtube')['snapshot']['songs'][0]
    assert song['raw_title'] == 'A video' and song['artists'] == ['Creator'] and song['source'] == 'youtube'
    zh = '歌名\t歌手\t专辑\t时长\n歌曲\t歌手甲\t专辑乙\t03:20\n'
    snapshot = import_playlist_file(write(tmp_path, 'list.tsv', zh, 'utf-8-sig'), 'qqmusic')['snapshot']
    assert snapshot['complete'] and snapshot['songs'][0]['duration'] == 200


def test_rfc_csv_quoted_newlines_commas_and_canonical_artist_separator(tmp_path):
    out = io.StringIO(); writer = csv.writer(out)
    writer.writerow(['title', 'artist', 'album', 'isrc', 'duration', 'url', 'index', 'status'])
    writer.writerow(['Title, part\nTwo', 'A, B', 'Album', '', '120', '', '1', 'playable'])
    result = import_playlist_file(write(tmp_path, 'canonical.csv', out.getvalue(), 'utf-8-sig'))
    assert result['snapshot']['songs'][0]['artists'] == ['A', 'B']
    assert result['snapshot']['songs'][0]['raw_title'] == 'Title, part\nTwo'
    # Commas are not a universal artist separator outside that known contract.
    payload = [{'title': 'T', 'artist': 'Earth, Wind & Fire', 'album': 'B', 'duration': 180}]
    song = import_playlist_file(write(tmp_path, 'generic.json', json.dumps(payload)))['snapshot']['songs'][0]
    assert song['artists'] == ['Earth, Wind & Fire']


def test_txt_needs_explicit_order_and_preserves_missing_album(tmp_path):
    file = write(tmp_path, 'plain.txt', 'Song - Artist\n')
    assert import_playlist_file(file)['status'] == 'NEEDS_MAPPING'
    song = import_playlist_file(file, 'netease', text_order='title-artist')['snapshot']['songs'][0]
    assert song['raw_title'] == 'Song' and song['artists'] == ['Artist'] and song['album'] == ''
    other = write(tmp_path, 'ordered.txt', '# order:artist-title\n# platform:spotify\n# playlist:Test\nArtist - Song - Album\n')
    snapshot = import_playlist_file(other)['snapshot']
    assert snapshot['name'] == 'Test' and snapshot['source'] == 'spotify'
    assert snapshot['songs'][0]['raw_title'] == 'Song' and snapshot['songs'][0]['album'] == 'Album'
    assert import_playlist_file(other, text_order='title-artist')['status'] == 'NEEDS_MAPPING'


def test_playlistout_documented_txt_header_and_partial_count(tmp_path):
    text = '==================================================\n  导出工具: Playlist Out (https://example.invalid)\n  歌单名称: Exported\n  已解析歌曲: 1 / 3 首\n==================================================\n\nSong - Artist - Album\n'
    snapshot = import_playlist_file(write(tmp_path, 'playlistout.txt', text))['snapshot']
    assert snapshot['name'] == 'Exported' and snapshot['declared_count'] == 3 and not snapshot['complete']
    assert snapshot['songs'][0]['raw_title'] == 'Song' and snapshot['songs'][0]['artists'] == ['Artist']


def test_m3u_metadata_only_does_not_open_any_media_path_or_url(tmp_path, monkeypatch):
    import socket
    monkeypatch.setattr(socket, 'socket', lambda *a, **kw: pytest.fail('network must not be used'))
    media = tmp_path / 'do-not-read.exe'; media.write_bytes(b'unused')
    text = '#EXTM3U\n#PLAYLIST:Example\n#EXTINF:180,Artist - Song\n' + str(media) + '\n#EXTINF:-1,Another title\nhttps://private.invalid/token\n'
    result = import_playlist_file(write(tmp_path, 'list.m3u8', text))
    snapshot = result['snapshot']; songs = snapshot['songs']
    assert snapshot['name'] == 'Example' and len(songs) == 2
    assert songs[0]['duration'] == 180 and songs[0]['artists'] == ['Artist']
    assert songs[1]['duration'] is None and songs[1]['artists'] == []
    assert 'private.invalid' not in json.dumps(result) and media.read_bytes() == b'unused'
    assert not result['network_used']


def test_gb18030_explicit_fallback_and_stable_generated_ids(tmp_path):
    file = write(tmp_path, 'gb.csv', '歌名,歌手,专辑,时长\n歌曲,艺人,专辑,150\n', 'gb18030')
    first = import_playlist_file(file)['snapshot']; second = import_playlist_file(file)['snapshot']
    assert first['encoding'] == 'GB18030' and first['complete']
    assert first['warnings'][0]['error_class'] == 'ENCODING_FALLBACK'
    assert first['songs'][0]['source_song_id'] is None
    assert first['songs'][0]['target_id'].startswith('imported:file-')
    assert first['snapshot_id'] == second['snapshot_id']


def test_unknown_columns_never_guessed_or_swapped(tmp_path):
    result = import_playlist_file(write(tmp_path, 'unknown.csv', 'first,second\nArtist,Song\n'))
    assert result['status'] == 'NEEDS_MAPPING' and 'snapshot' not in result
    text = 'artist,title,album,duration\nArtist,Song,Album,120\n'
    song = import_playlist_file(write(tmp_path, 'reverse.csv', text))['snapshot']['songs'][0]
    assert song['raw_title'] == 'Song' and song['artists'] == ['Artist']
    unknown = import_playlist_file(write(tmp_path, 'unknown.json', '[{"first":"Artist","second":"Song"}]'))
    assert unknown['status'] == 'NEEDS_MAPPING'


def test_duplicate_json_keys_rejected_instead_of_selecting_last_value(tmp_path):
    result = import_playlist_file(write(tmp_path, 'repeated.json', '[{"title":"A","title":"B"}]'))
    assert result['status'] == 'SOURCE_CONFLICT'


def test_id_only_target_and_explicitly_missing_title_keep_metadata_incomplete(tmp_path):
    payload = [{'id': 'abc', 'title': '', 'artist': [], 'album': None}]
    song = import_playlist_file(write(tmp_path, 'missing.json', json.dumps(payload)), 'spotify')['snapshot']['songs'][0]
    assert song['target_id'] == 'spotify:abc' and song['source_song_id'] == 'abc'
    assert song['raw_title'] == '' and song['artists'] == [] and song['album'] == ''
    assert set(song['metadata_missing_fields']) == {'title', 'artists', 'album', 'duration'}


@pytest.mark.parametrize('name,text', [('bad.json', '{}'), ('bad.json', '[NaN]'), ('bad.csv', 'title,title\na,b\n'), ('bad.csv', 'title,artist\na,b,c\n'), ('bad.m3u8', '#EXTM3U\nhttps://example.invalid/song.mp3\n'), ('bad.json', '[{"title":"T","id":"../audio.exe"}]')])
def test_invalid_shapes_and_path_like_ids_rejected(tmp_path, name, text):
    result = import_playlist_file(write(tmp_path, name, text))
    assert result['status'] not in ('OK', 'SNAPSHOT_INCOMPLETE') and 'snapshot' not in result


def test_source_conflict_duplicates_and_separate_namespace(tmp_path):
    file = write(tmp_path, 'canonical.json', json.dumps(canonical('qqmusic')))
    assert import_playlist_file(file, 'netease')['status'] == 'SOURCE_MISMATCH'
    a = {'source': 'qqmusic', 'id': '1', 'title': 'S', 'artist': 'A', 'album': 'B', 'duration': 1}
    b = {**a, 'source': 'netease'}
    result = import_playlist_file(write(tmp_path, 'mixed.json', json.dumps([a, b])))
    assert len(result['snapshot']['songs']) == 2 and result['snapshot']['source'] == 'imported'
    assert result['snapshot']['songs'][0]['target_id'] != result['snapshot']['songs'][1]['target_id']
    result = import_playlist_file(write(tmp_path, 'duplicates.json', json.dumps([a, a])))
    assert len(result['snapshot']['songs']) == 1 and not result['snapshot']['complete']
    assert result['snapshot']['observed_id_count'] == 2
    result = import_playlist_file(write(tmp_path, 'conflict.json', json.dumps([a, {**a, 'title': 'Changed'}])))
    assert result['status'] == 'SOURCE_CONFLICT'
    payload = canonical('qqmusic');payload['tracks'][0]['source'] = 'netease'
    assert import_playlist_file(write(tmp_path, 'row-source.json', json.dumps(payload)))['status'] == 'SOURCE_MISMATCH'


def test_limits_and_explicit_absolute_file_path(tmp_path):
    assert import_playlist_file('relative.json')['status'] == 'INVALID_INPUT'
    assert import_playlist_file(write(tmp_path, 'macro.xlsm', b'not executed'))['status'] == 'UNSUPPORTED_FORMAT'
    assert import_playlist_file(write(tmp_path, 'huge.csv', b' ' * (MAX_BYTES + 1)))['status'] == 'LIMIT_EXCEEDED'
    assert import_playlist_file(write(tmp_path, 'many.json', json.dumps([{}] * (MAX_TRACKS + 1))))['status'] == 'LIMIT_EXCEEDED'
    assert import_playlist_file(write(tmp_path, 'binary.csv', b'title\x00artist'))['status'] == 'INVALID_PLAYLIST_FILE'


def test_large_export_keeps_full_tail_beyond_one_thousand(tmp_path):
    payload = canonical('qqmusic', trackCount=4101)
    payload['tracks'] = [{'id': str(i), 'title': 'Song ' + str(i), 'artist': 'A', 'album': 'B', 'durationMs': 120000} for i in range(1, 4102)]
    snapshot = import_playlist_file(write(tmp_path, 'large.json', json.dumps(payload)))['snapshot']
    assert snapshot['complete'] and len(snapshot['songs']) == 4101
    assert snapshot['songs'][-1]['target_id'] == 'qqmusic:4101'


def test_checkpoint_cancellation_propagates_without_side_effect(tmp_path):
    file = write(tmp_path, 'songs.json', json.dumps(canonical()))
    before = file.read_bytes(); phases = []
    class Cancelled(RuntimeError):
        pass
    def checkpoint(phase):
        phases.append(phase)
        if phase == 'playlist_file_normalize':
            raise Cancelled('stop')
    with pytest.raises(Cancelled):
        import_playlist_file(file, checkpoint=checkpoint)
    assert phases == ['playlist_file_read', 'playlist_file_normalize'] and file.read_bytes() == before


def workbook(tmp_path, *, formula=False, extra=None, external=False):
    path = tmp_path / 'list.xlsx'
    def sheet(rows):
        pieces = []
        for index, row in enumerate(rows, 1):
            cells = []
            for col, value in enumerate(row):
                cells.append(f'<c r="{chr(65 + col)}{index}" t="inlineStr"><is><t>{escape(str(value))}</t></is></c>')
            pieces.append(f'<row r="{index}">' + ''.join(cells) + '</row>')
        xml = '<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>' + ''.join(pieces) + '</sheetData></worksheet>'
        return xml.replace('<is><t>Song</t></is>', '<f>HYPERLINK("https://invalid","Song")</f><v>Song</v>') if formula else xml
    entries = {
        'xl/workbook.xml': '<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Tracks" sheetId="1" r:id="rId1"/><sheet name="Playlist Info" sheetId="2" r:id="rId2"/></sheets></workbook>',
        'xl/_rels/workbook.xml.rels': '<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"' + (' TargetMode="External"' if external else '') + '/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet2.xml"/></Relationships>',
        'xl/worksheets/sheet1.xml': sheet([['title', 'artist', 'album', 'duration'], ['Song', 'Artist', 'Album', '120']]),
        'xl/worksheets/sheet2.xml': sheet([['field', 'value'], ['name', 'XLSX List'], ['platform', 'qishui'], ['trackCount', 1], ['generator', 'PlaylistOut']]),
    }
    entries.update(extra or {})
    with zipfile.ZipFile(path, 'w', zipfile.ZIP_DEFLATED) as archive:
        for name, data in entries.items():
            archive.writestr(name, data)
    return path


def test_xlsx_plain_tables_and_playlist_info_without_new_dependencies(tmp_path):
    result = import_playlist_file(workbook(tmp_path))
    snapshot = result['snapshot']
    assert result['status'] == 'OK' and snapshot['source'] == 'qishui' and snapshot['name'] == 'XLSX List'
    assert snapshot['songs'][0]['duration'] == 120 and snapshot['encoding'] == 'OOXML'


@pytest.mark.parametrize('kwargs', [{'formula': True}, {'external': True}, {'extra': {'xl/vbaProject.bin': b'macro'}}, {'extra': {'../outside.xml': '<unused/>'}}, {'extra': {'bomb.xml': 'A' * (1024 * 1024)}}, {'extra': {'xl/sharedStrings.xml': '<!DOCTYPE x [<!ENTITY data "x">]><sst/>'}}])
def test_xlsx_rejects_formulas_external_sheets_macros_and_bombs(tmp_path, kwargs):
    result = import_playlist_file(workbook(tmp_path, **kwargs))
    assert result['status'] not in ('OK', 'SNAPSHOT_INCOMPLETE') and 'snapshot' not in result
    assert sorted(p.name for p in tmp_path.iterdir()) == ['list.xlsx']
