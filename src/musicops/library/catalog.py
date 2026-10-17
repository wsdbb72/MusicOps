"""Persistent local album pre-matching and indexed playlist lookups."""
import collections
import json
import math
import os
from pathlib import Path
import re
from musicops.config.schema import digest
from musicops.domain.identity import strict_text
from musicops.domain.errors import PathBlocked
from musicops.library.paths import reject_reparse_chain
from musicops.state.store import utcnow


def album_directory(row, library):
    path = Path(row['path']).parent
    if re.match(r'^(?:CD|Disc|Disk)\s*\d+(?:\b|\s|[-_])', path.name, re.I): path = path.parent
    return path if path != Path(library) else Path(row['path']).parent


def root_key(root): return digest(os.path.normcase(str(Path(root).resolve())))
def artist_key(values): return json.dumps(sorted(strict_text(a) for a in values if a), ensure_ascii=False, separators=(',', ':'))


class AlbumCatalog:
    def __init__(self, store): self.store = store

    def sync(self, root, files, checkpoint=lambda *_: None):
        key = root_key(root)
        versions = [{k: row.get(k) for k in ('file_id', 'size', 'mtime_ns', 'raw_title', 'artists', 'album', 'albumartist', 'duration', 'read_error', 'metadata_resolution')} for row in files]
        fingerprint = digest(versions)
        previous = self.store.get('evidence', 'library-catalog:' + key)
        if previous and previous.get('fingerprint') == fingerprint:
            return {**previous, 'reused': True}
        groups = collections.defaultdict(list); matches = []; albums = []; now = utcnow()
        for index, row in enumerate(files):
            if index % 100 == 0: checkpoint('album_catalog_grouping')
            directory = str(album_directory(row, root)); album = row.get('album') or ''
            album_id = digest([key, directory, strict_text(album)])
            groups[album_id].append(row)
            duration = row.get('duration')
            if row.get('read_error') or not row.get('raw_title') or not row.get('artists') or not album or type(duration) not in (int, float) or not math.isfinite(duration) or duration <= 0:
                continue
            matches.append((key, row['file_id'], strict_text(row['raw_title']), artist_key(row['artists']), strict_text(album), duration, album_id))
        for album_id, tracks in groups.items():
            first = tracks[0]; artists = list(dict.fromkeys(a for row in tracks for a in (row.get('albumartist') or row.get('artists', []))))
            complete = all(row.get('raw_title') and row.get('artists') and row.get('album') and row.get('duration') and not row.get('read_error') for row in tracks)
            albums.append((key, album_id, json.dumps({'album_id': album_id, 'album': first.get('album') or '待识别专辑',
                'artists': artists, 'directory': str(album_directory(first, root)), 'track_count': len(tracks),
                'file_ids': [row['file_id'] for row in tracks], 'state': 'local_tags_matched' if complete else 'needs_metadata',
                'release_confirmed': False, 'updated_at': now}, ensure_ascii=False)))
        checkpoint('album_catalog_commit')
        with self.store.connection() as connection:
            connection.execute('BEGIN IMMEDIATE')
            connection.execute('DELETE FROM library_matches WHERE root_key=?', (key,))
            connection.execute('DELETE FROM library_albums WHERE root_key=?', (key,))
            connection.executemany('INSERT INTO library_matches VALUES(?,?,?,?,?,?,?)', matches)
            connection.executemany('INSERT INTO library_albums VALUES(?,?,?)', albums)
            info = {'root_key': key, 'fingerprint': fingerprint, 'album_count': len(albums), 'track_count': len(files),
                    'matched_track_count': len(matches), 'needs_metadata_count': len(files) - len(matches), 'updated_at': now}
            connection.execute('INSERT INTO evidence VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at', ('library-catalog:' + key, json.dumps(info), now))
        return {**info, 'reused': False}

    def summary(self, root):
        return self.store.get('evidence', 'library-catalog:' + root_key(root)) or {'album_count': 0, 'track_count': 0, 'matched_track_count': 0, 'updated_at': None}

    def albums(self, root, offset=0):
        if type(offset) is not int or offset < 0: raise ValueError('专辑库分页参数无效')
        key = root_key(root)
        with self.store.connection() as connection:
            total = connection.execute('SELECT COUNT(*) FROM library_albums WHERE root_key=?', (key,)).fetchone()[0]
            rows = connection.execute('SELECT payload FROM library_albums WHERE root_key=? ORDER BY album_id LIMIT 200 OFFSET ?', (key, offset)).fetchall()
        return {'albums': [json.loads(row[0]) for row in rows], 'total': total, 'offset': offset, 'summary': self.summary(root)}

    def matched_targets(self, root, targets, checkpoint=lambda *_: None):
        """Reuse normalized album facts; verify only matching files still exist unchanged."""
        key = root_key(root); present = set(); verified = {}
        with self.store.connection() as connection:
            for index, target in enumerate(targets):
                if index % 50 == 0: checkpoint('playlist_album_lookup')
                duration=target.get('duration')
                if type(duration) not in (int,float) or not math.isfinite(duration) or duration<=0:continue
                rows = connection.execute('SELECT f.payload FROM library_matches AS m JOIN library_files AS f ON f.id=m.file_id WHERE m.root_key=? AND m.title_key=? AND m.artist_key=? AND m.album_key=? AND ABS(m.duration-?)<=2',
                    (key, strict_text(target.get('raw_title') or target.get('title', '')), artist_key(target.get('artists', [])), strict_text(target.get('album') or ''), target['duration'])).fetchall()
                for raw in rows:
                    row = json.loads(raw[0]); fid = row['file_id']
                    if fid not in verified:
                        valid = False
                        try:
                            path = Path(row['path']); reject_reparse_chain(path)
                            stat = path.stat()
                            valid = row.get('location_state') == 'library' and path.resolve().is_relative_to(Path(root).resolve()) and (stat.st_size, stat.st_mtime_ns) == (row.get('size'), row.get('mtime_ns'))
                        except (OSError, RuntimeError, PathBlocked): pass
                        verified[fid] = valid
                    if verified[fid]: present.add(target['target_id']); break
        return present
