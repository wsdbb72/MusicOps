"""Quality preferences are considered only inside confirmed recording/release groups."""
from .identity import identities_same, recording_identity, release_identity
from .errors import PolicyBlocked
from pathlib import Path

MAX_DUPLICATE_PAIRS = 50000


def resource_evidence(item):
    if 'resource_signatures' in item:
        return sorted(tuple(row) for row in item['resource_signatures'])
    # Filename changes only alter association, not resource content identity; missing resources still differ.
    result = []
    for name, content_hash in item.get('resource_hashes', {}).items():
        suffix = Path(name).suffix.casefold()
        kind = 'lyrics' if suffix == '.lrc' else 'cue' if suffix == '.cue' else 'cover' if suffix in {'.jpg', '.jpeg', '.png', '.webp'} else 'unknown'
        result.append((kind, 'track' if kind == 'lyrics' else 'release', content_hash))
    return sorted(result)


def compare_quality(a, b, preferences=None):
    preferences = preferences or {}
    if not identities_same(a, b):
        return {'schema_version': 1, 'action': 'coexist', 'reason': '录音或发行身份不同/证据不足，不跨母带比较'}
    def score(item):
        info = item.get('media_info', {})
        validation = item.get('validation', {})
        passed = validation.get('decode_state', validation.get('state')) == 'passed' and not item.get('read_error')
        compatible = validation.get('roon_compatible', info.get('roon_compatible', False))
        codec = str(info.get('codec_name', info.get('codec', '')))
        formats = preferences.get('formats', ['flac', 'alac', 'wav', 'aiff', 'mp3'])
        pref = len(formats) - formats.index(codec) if codec in formats else 0
        try:
            depth = max(0, int(info.get('bits_per_raw_sample') or info.get('bits_per_sample') or 0))
        except (ValueError, TypeError, OverflowError):
            depth = 0
        return (passed, compatible, bool(item.get('source_trusted')), pref, depth if preferences.get('prefer_24bit') else 0)
    # DSD/PCM and channel layouts are separate policies, never sorted by sample rate.
    ai, bi = a.get('media_info', {}), b.get('media_info', {})
    if str(ai.get('codec_name', '')).startswith('dsd') != str(bi.get('codec_name', '')).startswith('dsd') or ai.get('channels') != bi.get('channels'):
        return {'schema_version': 1, 'action': 'coexist', 'reason': 'DSD/PCM或声道策略分组不同'}
    sa, sb = score(a), score(b)
    if not sa[0] and not sb[0]:
        return {'schema_version': 1, 'action': 'review', 'reason': '双方均无完整验证，不能保留侧自动淘汰'}
    return {'schema_version': 1, 'action': 'prefer', 'preferred_file_id': a['file_id'] if sa >= sb else b['file_id'], 'reason': '同身份内按完整性、兼容性、来源可信与用户偏好比较；高位深不是母带更好证明'}


def propose_duplicates(files):
    # Only equal hashes can produce a proposal. Avoid all-pairs work on large
    # catalogues while preserving the original pair ordering and evidence.
    groups = {}
    pairs = set()
    for index, item in enumerate(files):
        if item.get('location_state', 'library') != 'library':
            continue
        for key in ('sha256', 'pcm_hash'):
            value = item.get(key)
            if value:
                bucket = groups.setdefault((key, value), [])
                pairs.update((previous, index) for previous in bucket)
                if len(pairs)>MAX_DUPLICATE_PAIRS:
                    raise PolicyBlocked('重复内容候选对超过50000；请按发行或目录缩小扫描范围后复核，未生成部分隔离计划')
                bucket.append(index)
    proposals = []
    for first, second in sorted(pairs):
        a, b = files[first], files[second]
        exact = bool(a.get('sha256')) and a.get('sha256') == b.get('sha256')
        same_identity = identities_same(a, b)
        locked = bool(a.get('field_locks') or b.get('field_locks') or a.get('suppression') or b.get('suppression'))
        companions_differ = resource_evidence(a) != resource_evidence(b)
        quality = compare_quality(a, b)
        metadata_conflict = bool(a.get('filename_tag_conflict') or b.get('filename_tag_conflict') or a.get('read_error') or b.get('read_error'))
        actionable = same_identity and not locked and not companions_differ and not metadata_conflict and quality['action'] == 'prefer'
        keep = quality.get('preferred_file_id')
        proposals.append({'schema_version': 1, 'file_ids': [a['file_id'], b['file_id']], 'evidence': 'exact_bytes' if exact else 'decoded_pcm',
                          'recording_confirmed': same_identity, 'release_confirmed': same_identity,
                          'action': 'quarantine_candidate' if actionable else 'review', 'keep_file_id': keep if actionable else None,
                          'quarantine_file_id': (b['file_id'] if keep == a['file_id'] else a['file_id']) if actionable else None,
                          'quarantine_reason': 'duplicate', 'requires_review': True,
                          'reason': quality['reason'] if same_identity and not locked and not companions_differ else '不同发行、资源或人工锁定须并存/复核；只提出可回滚隔离，不删除'})
    return proposals
