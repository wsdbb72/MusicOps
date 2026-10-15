import base64
import copy
import hashlib
import json
import math
import os
import re
import uuid
from pathlib import Path

from musicops.domain.identity import search_text
from musicops.domain.errors import PathBlocked
from .paths import reparse, reject_reparse_chain, within
from .resources import companion_relations

AUDIO_EXTENSIONS = {'.flac', '.mp3', '.m4a', '.mp4', '.ogg', '.opus', '.wav', '.aif', '.aiff', '.dsf', '.dff', '.ape', '.wma', '.ncm'}
METADATA_INDEX_VERSION = 2


def sha256_file(path):
    result = hashlib.sha256()
    with Path(path).open('rb') as stream:
        while block := stream.read(1024 * 1024):
            result.update(block)
    return result.hexdigest()


def file_version(path):
    item = Path(path).stat()
    return (item.st_size, item.st_mtime_ns, getattr(item, 'st_ino', None))


def serial_native(value):
    """Lossless JSON representation of tag values, frame attributes and binary pictures."""
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, bytes):
        result = {'type': type(value).__name__, 'encoding': 'base64', 'data': base64.b64encode(value).decode('ascii'), 'sha256': hashlib.sha256(value).hexdigest()}
        if hasattr(value, 'dataformat'):
            result['dataformat'] = int(value.dataformat)
            result['version'] = getattr(value, 'version', None)
        return result
    if isinstance(value, dict):
        return {str(k): serial_native(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [serial_native(v) for v in value]
    if hasattr(value, '__dict__'):
        return {'type': type(value).__name__, 'fields': {str(k): serial_native(v) for k, v in vars(value).items()}}
    return {'type': type(value).__name__, 'text': str(value)}


def _list(value):
    if value is None:
        return []
    if hasattr(value, 'text'):
        value = value.text
    if type(value).__name__ == 'APETextValue':
        return [str(v) for v in value]
    return [str(v) for v in value] if isinstance(value, (list, tuple)) else [str(value)]


def tag_fields(tags):
    keys = {str(k).casefold(): k for k in tags.keys()}
    aliases = {'raw_title': ['title', 'tit2', '\xa9nam'], 'artists': ['artist', 'tpe1', '\xa9art'],
               'album': ['album', 'talb', '\xa9alb'], 'albumartist': ['albumartist', 'album artist', 'tpe2', 'aart'],
               'track': ['tracknumber', 'trck', 'trkn', 'track'], 'disc': ['discnumber', 'tpos', 'disk', 'disc'],
               'date_raw': ['date', 'tdrc', '\xa9day', 'year'], 'edition': ['edition', 'release_version'],
               'recording_version': ['recording_version', 'version'], 'release_id': ['release_id', 'musicbrainz_albumid'],
               'source_album_id': ['netease_album_id', 'music163_album_id'],
               'catalogue': ['catalognumber', 'catalogue', 'catalog'], 'source_song_id': ['netease_song_id', 'music163_id']}
    result = {}
    for name, options in aliases.items():
        found = next((tags[keys[key]] for key in options if key in keys), None)
        data = _list(found)
        if name in {'track', 'disc'} and found and isinstance(found, list) and isinstance(found[0], tuple):
            data = [str(found[0][0])]
        result[name] = data if name in {'artists', 'albumartist'} else (data[0] if data else '')
    return result


def _filename_evidence(path, fields):
    stem = path.stem
    prefix = re.match(r'^(\d+)\s*[-. ]\s*(.+)$', stem)
    track = str(fields.get('track', '')).split('/')[0]
    if prefix and track.isdigit() and int(prefix.group(1)) == int(track):
        stem = prefix.group(2)
    artist, title = None, stem
    if ' - ' in stem:
        artist, title = stem.split(' - ', 1)
    tags_title = fields.get('raw_title', '')
    conflict = bool(tags_title) and search_text(tags_title) != search_text(title)
    if artist and fields.get('artists'):
        conflict |= search_text(artist) not in {search_text(a) for a in fields['artists']}
    return {'filename_title': title, 'filename_artist': artist, 'filename_tag_conflict': conflict}


def _metadata_summary(value, depth=0):
    """Bounded, JSON-safe tag evidence; never copy or hash embedded pictures."""
    if isinstance(value, (bytes, bytearray, memoryview)):
        return {'type': type(value).__name__, 'byte_length': len(value), 'content_omitted': True}
    if value is None or isinstance(value, (bool, int)):
        return value
    if isinstance(value, float):
        return value if math.isfinite(value) else {'invalid_number': True}
    if isinstance(value, str):
        return value[:128] if len(value) <= 128 else {'text': value[:128], 'truncated': True, 'character_length': len(value)}
    if depth >= 3:
        return {'type': type(value).__name__, 'content_omitted': True}
    if isinstance(value, dict):
        items = list(value.items())
        return {'items': {str(k)[:64]: _metadata_summary(v, depth + 1) for k, v in items[:8]}, 'item_count': len(items)}
    if isinstance(value, (list, tuple)):
        result = [_metadata_summary(v, depth + 1) for v in value[:4]]
        return result if len(value) <= 4 else {'items': result, 'item_count': len(value), 'truncated': True}
    if hasattr(value, '__dict__'):
        return {'type': type(value).__name__, 'fields': _metadata_summary(vars(value), depth + 1)}
    return {'type': type(value).__name__, 'text': str(value)[:128]}


def _metadata_companions(directory):
    """List only sidecar metadata. CUE references require a separate validation."""
    companions = []
    try:
        paths = sorted(directory.iterdir())
    except OSError:
        return companions
    for path in paths:
        if path.suffix.casefold() not in {'.lrc', '.cue', '.jpg', '.jpeg', '.png', '.webp', '.bmp'} or reparse(path):
            continue
        try:
            reject_reparse_chain(path, directory)
            if not path.is_file():
                continue
            info = path.stat()
            companions.append({'schema_version': 1, 'path': str(path), 'size': info.st_size, 'mtime_ns': info.st_mtime_ns,
                               'sha256': None, 'evidence_level': 'local_metadata_readonly'})
        except (OSError, PathBlocked):
            continue
    return companions


def _metadata_relations(path, sidecars):
    companions = []
    for item in sidecars:
        sidecar = Path(item['path'])
        suffix = sidecar.suffix.casefold()
        if suffix == '.lrc' and sidecar.stem.casefold() == path.stem.casefold():
            companions.append(dict(item, type='lyrics', scope='track', shared=False))
        elif suffix in {'.jpg', '.jpeg', '.png', '.webp', '.bmp'} and sidecar.stem.casefold() in {'cover', 'folder', 'front', path.stem.casefold()}:
            companions.append(dict(item, type='cover', scope='release', shared=True))
        elif suffix == '.cue':
            companions.append(dict(item, type='cue', scope='release', shared=True, relation_verified=False))
    return companions


def _index_metadata(root, excluded, checkpoint, cached=None, progress=None):
    records = []
    reused = 0
    cached = cached or {}
    if checkpoint:
        checkpoint()
    for directory, dirs, names in os.walk(root, followlinks=False):
        directory = Path(directory)
        # Recheck the directory: it can have been replaced since os.walk listed it.
        reject_reparse_chain(directory, root)
        dirs[:] = [d for d in dirs if not reparse(directory / d) and not any(within(directory / d, e) for e in excluded)]
        if progress:
            progress({'scanned_count': len(records), 'cached_count': reused, 'current_directory': str(directory.relative_to(root)), 'phase': 'scanning'})
        sidecars = None
        for name in sorted(names):
            path = directory / name
            if path.suffix.casefold() not in AUDIO_EXTENSIONS or reparse(path) or any(within(path, e) for e in excluded):
                continue
            file_id = str(uuid.uuid5(uuid.NAMESPACE_URL, str(path).casefold()))
            previous = cached.get(file_id)
            # Only reuse our bounded metadata format. A same-size file with a
            # changed timestamp/inode, prior read error, or older schema is read again.
            if previous and str(previous.get('path','')).casefold()==str(path).casefold() and previous.get('metadata_index_version') == METADATA_INDEX_VERSION and not previous.get('read_error'):
                try:
                    reject_reparse_chain(path, root)
                    before = file_version(path)
                    if list(before) == previous.get('metadata_source_version'):
                        if sidecars is None:
                            sidecars = _metadata_companions(directory)
                        row = copy.deepcopy(previous)
                        row['companions'] = _metadata_relations(path, sidecars)
                        if file_version(path) == before:
                            records.append(row)
                            reused += 1
                            if progress:
                                progress({'scanned_count': len(records), 'cached_count': reused, 'current_directory': str(directory.relative_to(root)), 'phase': 'scanning'})
                            if checkpoint and len(records) % 50 == 0:
                                checkpoint()
                            continue
                except (OSError, PathBlocked):
                    pass
            template = {'schema_version': 1, 'file_id': str(uuid.uuid5(uuid.NAMESPACE_URL, str(path).casefold())),
                        'volume': path.drive, 'path': str(path), 'relative_path': str(path.relative_to(root)), 'location_state': 'library',
                        'size': None, 'mtime_ns': None, 'sha256': None, 'media_info': {}, 'raw_tags': {}, 'read_error': None,
                        'validation_id': None, 'validation': {'decode_state': 'not_run', 'roon_compatible': None},
                        'normalized_search': {}, 'metadataOnly': True, 'metadata_only': True,
                        'evidence_level': 'local_metadata_readonly', 'metadata_warnings': [],
                        'resource_hashes': {}, 'resource_signatures': [], 'pictures': []}
            for attempt in range(2):
                row = dict(template)
                row.update(media_info={}, raw_tags={}, pictures=[], metadata_warnings=[])
                fields = {}
                try:
                    reject_reparse_chain(path, root)
                    before = file_version(path)
                    try:
                        import mutagen
                        audio = mutagen.File(path, easy=False)
                        if audio is None:
                            row['read_error'] = 'UNSUPPORTED_TAG_FORMAT'
                        else:
                            tags = audio.tags or {}
                            row['raw_tag_count'] = len(tags)
                            summary_bytes = 0
                            for key, value in tags.items():
                                summary = _metadata_summary(value)
                                cost = len(json.dumps({str(key)[:128]: summary}, ensure_ascii=False, allow_nan=False).encode('utf-8'))
                                if len(row['raw_tags']) >= 32 or summary_bytes + cost > 4096:
                                    break
                                row['raw_tags'][str(key)[:128]] = summary
                                summary_bytes += cost
                            if len(row['raw_tags']) < len(tags):
                                row['metadata_warnings'].append('RAW_TAGS_TRUNCATED')
                            row['tag_format'] = type(tags).__name__
                            row['pictures'] = [_metadata_summary(p) for p in getattr(audio, 'pictures', [])[:8]]
                            fields = tag_fields(tags)
                            fields = {k: [v[:1024] for v in value[:64]] if isinstance(value, list) else str(value)[:1024]
                                      for k, value in fields.items()}
                            info = audio.info
                            for key in ('length', 'sample_rate', 'channels', 'bits_per_sample', 'bitrate'):
                                value = getattr(info, key, None)
                                if value is None:
                                    continue
                                if not isinstance(value, (int, float)) or isinstance(value, bool) or not math.isfinite(value) or value < 0:
                                    row['metadata_warnings'].append('INVALID_MEDIA_INFO:' + key)
                                    continue
                                row['media_info'][key] = value
                            row['media_info']['codec'] = type(audio).__name__.casefold()
                            fields['duration'] = row['media_info'].get('length')
                            if not tags:
                                row['tag_status'] = 'NO_TAGS'
                    except ImportError:
                        row['read_error'] = 'TAG_TOOL_MISSING'
                    except Exception as exc:
                        fields = {}
                        row['read_error'] = 'TAG_READ_ERROR:' + type(exc).__name__
                    row.update(fields)
                    row['version'] = row.get('recording_version', '')
                    row['date_source'] = 'local_tag' if row.get('date_raw') else None
                    row['date_semantics'] = 'unconfirmed' if row.get('date_raw') else None
                    row.update(_filename_evidence(path, fields))
                    row['raw_title'] = row.get('raw_title') or row['filename_title']
                    row['artists'] = row.get('artists', [])
                    reject_reparse_chain(path, root)
                    after = file_version(path)
                    row.update(size=after[0], mtime_ns=after[1], metadata_index_version=METADATA_INDEX_VERSION, metadata_source_version=list(after))
                    if before != after:
                        row['read_error'] = 'BUSY_CHANGED_DURING_INDEX'
                        if attempt == 0:
                            continue
                    row['normalized_search'] = {'title': search_text(row.get('raw_title')), 'artists': [search_text(a) for a in row['artists']], 'album': search_text(row.get('album'))}
                    if sidecars is None:
                        sidecars = _metadata_companions(directory)
                    row['companions'] = _metadata_relations(path, sidecars)
                    break
                except (OSError, PathBlocked) as exc:
                    row['read_error'] = 'PATH_BLOCKED' if isinstance(exc, PathBlocked) else 'FILE_READ_ERROR:' + type(exc).__name__
                    row['raw_title'] = path.stem
                    row['artists'] = []
                    break
            records.append(row)
            if progress:
                progress({'scanned_count': len(records), 'cached_count': reused, 'current_directory': str(directory.relative_to(root)), 'phase': 'scanning'})
            if checkpoint and len(records) % 50 == 0:
                checkpoint()
    if checkpoint:
        checkpoint()
    if progress:
        progress({'scanned_count': len(records), 'cached_count': reused, 'current_directory': '', 'phase': 'scanned'})
    return sorted(records, key=lambda x: x['path'].casefold())


def index_library(root, validator=None, exclusions=None, metadata_only=False, checkpoint=None, cached=None, progress=None):
    root = Path(root).absolute()
    reject_reparse_chain(root)
    if not root.is_dir():
        raise FileNotFoundError('扫描根不存在')
    excluded = [Path(p).resolve() for p in (exclusions or [])]
    if metadata_only:
        return _index_metadata(root, excluded, checkpoint, cached=cached, progress=progress)
    records = []
    if checkpoint:
        checkpoint()
    for directory, dirs, names in os.walk(root, followlinks=False):
        directory = Path(directory)
        dirs[:] = [d for d in dirs if not reparse(directory / d) and not any(within(directory / d, e) for e in excluded)]
        for name in sorted(names):
            path = directory / name
            if path.suffix.casefold() not in AUDIO_EXTENSIONS or reparse(path) or any(within(path, e) for e in excluded):
                continue
            row = {'schema_version': 1, 'file_id': str(uuid.uuid5(uuid.NAMESPACE_URL, str(path).casefold())),
                   'volume': path.drive, 'path': str(path), 'relative_path': str(path.relative_to(root)), 'location_state': 'library',
                   'size': None, 'mtime_ns': None, 'sha256': None, 'media_info': {}, 'raw_tags': {}, 'read_error': None,
                   'validation_id': None, 'validation': {}, 'normalized_search': {}}
            template = dict(row)
            for attempt in range(2):
                try:
                    # A successful retry must not retain a previous busy/error
                    # flag or tags from a different revision of the file.
                    row = dict(template)
                    row.update(read_error=None, raw_tags={}, media_info={}, validation={}, validation_id=None)
                    before = file_version(path)
                    content_hash = sha256_file(path)
                    try:
                        import mutagen
                        audio = mutagen.File(path, easy=False)
                        if audio is None:
                            row['read_error'] = 'UNSUPPORTED_TAG_FORMAT'
                            fields = {}
                        else:
                            tags = audio.tags or {}
                            row['raw_tags'] = {str(k): serial_native(v) for k, v in tags.items()}
                            row['tag_format'] = type(tags).__name__
                            row['pictures'] = [serial_native(p) for p in getattr(audio, 'pictures', [])]
                            fields = tag_fields(tags)
                            info = audio.info
                            row['media_info'] = {k: getattr(info, k) for k in ('length', 'sample_rate', 'channels', 'bits_per_sample', 'bitrate') if hasattr(info, k)}
                            row['media_info']['codec'] = type(audio).__name__.casefold()
                            fields['duration'] = getattr(info, 'length', None)
                            if not tags:
                                row['tag_status'] = 'NO_TAGS'
                    except ImportError:
                        fields = {}
                        row['read_error'] = 'TAG_TOOL_MISSING'
                    except Exception as exc:
                        fields = {}
                        row['read_error'] = 'TAG_READ_ERROR:' + type(exc).__name__
                    row.update(fields)
                    row['version'] = row.get('recording_version', '')
                    row['date_source'] = 'local_tag' if row.get('date_raw') else None
                    row['date_semantics'] = 'unconfirmed' if row.get('date_raw') else None
                    row.update(_filename_evidence(path, fields))
                    row['raw_title'] = row.get('raw_title') or row['filename_title']
                    row['artists'] = row.get('artists', [])
                    if validator is not None:
                        validation = validator.validate(path)
                        row['validation'] = validation
                        row['validation_id'] = validation.get('validation_id')
                        row['media_info'].update(validation.get('media_info', {}))
                        row['media_info']['codec'] = row['media_info'].get('codec_name', row['media_info'].get('codec', 'unknown'))
                        row['duration'] = validation.get('duration', row.get('duration'))
                    after = file_version(path)
                    if before != after:
                        row['read_error'] = 'BUSY_CHANGED_DURING_INDEX'
                        if attempt == 0:
                            continue
                    row.update(size=after[0], mtime_ns=after[1], sha256=content_hash)
                    row['normalized_search'] = {'title': search_text(row.get('raw_title')), 'artists': [search_text(a) for a in row['artists']], 'album': search_text(row.get('album'))}
                    row['companions'] = companion_relations(path)
                    row['resource_hashes'] = {str(Path(r['path']).name): sha256_file(r['path']) for r in row['companions'] if not r.get('read_error')}
                    row['resource_signatures'] = sorted((r['type'], r['scope'], row['resource_hashes'][Path(r['path']).name]) for r in row['companions'] if not r.get('read_error'))
                    break
                except OSError as exc:
                    row['read_error'] = 'FILE_READ_ERROR:' + type(exc).__name__
                    break
            records.append(row)
            if checkpoint and len(records) % 50 == 0:
                checkpoint()
    if checkpoint:
        checkpoint()
    return sorted(records, key=lambda x: x['path'].casefold())
