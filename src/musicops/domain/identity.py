"""Conservative recording/release matching. Search normalisation never decides identity."""
import re
import math
import unicodedata
from datetime import datetime, timezone

RULE_VERSION = 'identity-1'
MATCH_STATES = {'verified_present', 'missing', 'ambiguous', 'version_mismatch', 'source_unavailable', 'snapshot_incomplete'}


def strict_text(value):
    return ' '.join(unicodedata.normalize('NFKC', str(value or '')).casefold().split())


def search_text(value):
    # Used only for candidate discovery. Versions are retained in strict_text/raw_title.
    return re.sub(r'[\W_]+', '', strict_text(value))


def values(value):
    if value is None:
        return []
    if isinstance(value, (list, tuple, set)):
        return [str(v) for v in value if str(v).strip()]
    return [str(value)] if str(value).strip() else []


def version_from_title(title):
    markers = re.findall(r'\([^)]*\)|\[[^]]*\]|（[^）]*）', str(title or ''))
    return ' '.join(strict_text(m) for m in markers if re.search(r'live|remix|instrumental|acoustic|remaster|deluxe|edit|mono|stereo|现场|重录|重製|重制|伴奏', m, re.I))


def recording_identity(track):
    title = track.get('raw_title', track.get('title', ''))
    artists = track.get('artists', track.get('artist', []))
    version = track.get('recording_version', track.get('version'))
    if version is None:
        version = version_from_title(title)
    return {'schema_version': 1, 'artist_identity': sorted({strict_text(a) for a in values(artists)}),
            'title': strict_text(title), 'recording_version': strict_text(version), 'duration': track.get('duration'),
            'external_ids': track.get('external_ids', {}), 'fingerprint': track.get('fingerprint'),
            'pcm_hash': track.get('pcm_hash')}


def release_identity(track):
    from musicops.media.source_labels import album_artist_credits
    return {'schema_version': 1, 'release_id': track.get('release_id'),
            'source_album_id': track.get('source_album_id'), 'album': strict_text(track.get('album', '')),
            'release_artist': sorted({strict_text(a) for a in album_artist_credits(track.get('albumartist', track.get('release_artist', [])))}),
            'edition': strict_text(track.get('edition', '')), 'catalogue': strict_text(track.get('catalogue', '')),
            'disc': _position(track.get('disc')), 'track': _position(track.get('track')),
            'date_raw': track.get('date_raw'), 'date_source': track.get('date_source'), 'date_semantics': track.get('date_semantics')}


def _position(value):
    # ID3/Vorbis commonly store "01/12"; the total is not the track identity.
    text = str(value or '').strip()
    match = re.fullmatch(r'([0-9]+)(?:/[0-9]+)?', text)
    return str(int(match.group(1))) if match else text


def release_conflicts(a, b):
    for field in ('release_id', 'source_album_id', 'album', 'catalogue'):
        if a[field] and b[field] and str(a[field]) != str(b[field]):
            return True
    return a['edition'] != b['edition']


def identities_same(a, b, require_release=True):
    x, y = recording_identity(a), recording_identity(b)
    if not x['title'] or not x['artist_identity'] or x['title'] != y['title'] or x['artist_identity'] != y['artist_identity'] or x['recording_version'] != y['recording_version']:
        return False
    if not _duration_close(x['duration'], y['duration']):
        return False
    if any(k in y['external_ids'] and str(y['external_ids'][k]) != str(v) for k, v in x['external_ids'].items()):
        return False
    if require_release:
        p, q = release_identity(a), release_identity(b)
        if release_conflicts(p, q):
            return False
        if any(p[field] and q[field] and p[field] != q[field] for field in ('disc','track','release_artist')):
            return False
        if p['release_id'] and q['release_id']:
            return True
        if not p['album'] or p['album'] != q['album'] or p['edition'] != q['edition']:
            return False
        if p['catalogue'] and q['catalogue'] and p['catalogue'] != q['catalogue']:
            return False
    return True


def _duration_close(a, b):
    try:
        if isinstance(a,bool) or isinstance(b,bool):
            return False
        first, second = float(a), float(b)
        return math.isfinite(first) and math.isfinite(second) and first > 0 and second > 0 and abs(first - second) <= 3
    except (ValueError, TypeError):
        return False


def _healthy(file):
    validation = file.get('validation', file.get('media_validation', {}))
    return not file.get('read_error') and validation.get('decode_state', validation.get('state')) == 'passed' and validation.get('roon_compatible') is True


def match_track(target, files):
    result = {'schema_version': 1, 'target_id': target.get('target_id', target.get('source_song_id')),
              'candidate_file_ids': [], 'main_state': 'missing', 'rule_version': RULE_VERSION,
              'evidence_ids': [], 'reason': '', 'flags': [], 'reviewer': None, 'roon_state': 'pending',
              'recording_identity': recording_identity(target), 'release_identity': release_identity(target)}
    if target.get('snapshot_complete') is False:
        result.update(main_state='snapshot_incomplete', reason='输入快照不完整，不驱动下载')
        return result
    if target.get('source_available') is False:
        result.update(main_state='source_unavailable', reason='来源访问失败或目标不可用')
        return result
    ti = recording_identity(target)
    candidates, conflicts, verified, versions, unavailable = [], [], [], [], []
    for file in files:
        fi = recording_identity(file)
        same_external = bool(target.get('source_song_id')) and str(target.get('source_song_id')) == str(file.get('source_song_id')) and target.get('source', 'netease') == file.get('source', 'netease')
        title_candidate = ti['title'] == fi['title'] or (search_text(ti['title']) and search_text(ti['title']) == search_text(fi['title']))
        # Version-bearing titles remain candidates, never an identity shortcut.
        base_target = re.sub(r'\([^)]*\)|\[[^]]*\]', '', ti['title']).strip()
        base_file = re.sub(r'\([^)]*\)|\[[^]]*\]', '', fi['title']).strip()
        version_candidate = bool(base_target) and base_target == base_file
        if not (same_external or title_candidate or version_candidate):
            continue
        candidates.append(file)
        if file.get('location_state', 'library') != 'library':
            unavailable.append(file)
            continue
        if file.get('filename_tag_conflict') or file.get('read_error') or not ti['artist_identity'] or not fi['artist_identity'] or any(a in {'unknown', '未知', 'unknown artist', '未知艺人'} for a in ti['artist_identity'] + fi['artist_identity']):
            conflicts.append(file)
            continue
        if ti['artist_identity'] != fi['artist_identity']:
            result['flags'].append('same_title_different_artist')
            continue
        if ti['recording_version'] != fi['recording_version'] or (version_candidate and ti['title'] != fi['title']):
            versions.append(file)
            continue
        target_release, file_release = release_identity(target), release_identity(file)
        release_conflict = release_conflicts(target_release, file_release)
        if release_conflict:
            versions.append(file)
            continue
        external_conflict = any(k in fi['external_ids'] and str(fi['external_ids'][k]) != str(v) for k, v in ti['external_ids'].items())
        if external_conflict:
            conflicts.append(file)
            continue
        # Missing duration/release evidence is not a wildcard. Matching duration is only a candidate gate.
        release_confirmed = ((target_release['release_id'] and target_release['release_id'] == file_release['release_id']) or
                             (target_release['album'] and target_release['album'] == file_release['album'] and target_release['edition'] == file_release['edition']))
        if ti['title'] != fi['title'] or not _duration_close(ti['duration'], fi['duration']) or not release_confirmed or not _healthy(file):
            conflicts.append(file)
            continue
        if target_release['disc'] and target_release['disc'] != file_release['disc'] or target_release['track'] and target_release['track'] != file_release['track']:
            conflicts.append(file)
            continue
        verified.append(file)
    result['candidate_file_ids'] = [f['file_id'] for f in candidates]
    result['evidence_ids'] = [f.get('validation_id') for f in candidates if f.get('validation_id')]
    if conflicts:
        result.update(main_state='ambiguous', reason='标签/文件名冲突、完整性未确证或身份与发行证据不足；须复核')
        result['flags'].append('evidence_incomplete')
    elif verified:
        result.update(main_state='verified_present', reason='逐曲标题、全部艺人、录音版本、发行、时长门及完整解码证据一致')
    elif versions:
        result.update(main_state='version_mismatch', reason='录音版本或发行身份冲突，保留多版本并复核')
    elif unavailable:
        result.update(reason='仅隔离/人工抑制位置有候选；不算健康主库覆盖')
        result['flags'] += [f.get('quarantine_reason', 'quarantined') for f in unavailable]
    else:
        result.update(reason='没有确证的健康主库曲目；同名异人不算命中')
    result['flags'] = sorted(set(result['flags']))
    return result
