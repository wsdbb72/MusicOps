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
from musicops.library.index import AUDIO_EXTENSIONS
from musicops.state.store import utcnow
from musicops.media.source_labels import effective_album_artists


def album_directory(row, library):
    path = Path(row['path']).parent
    if re.match(r'^(?:CD|Disc|Disk)\s*\d+(?:\b|\s|[-_])', path.name, re.I): path = path.parent
    return path if path != Path(library) else Path(row['path']).parent


def root_key(root): return digest(os.path.normcase(str(Path(root).resolve())))
def artist_key(values): return json.dumps(sorted(strict_text(a) for a in values if a), ensure_ascii=False, separators=(',', ':'))

def _facts(root,files,key,now,checkpoint):
    groups=collections.defaultdict(list);matches=[];albums=[]
    for index,row in enumerate(files):
        if Path(row.get('path','')).suffix.casefold() not in AUDIO_EXTENSIONS:continue
        if index%100==0:checkpoint('album_catalog_grouping')
        directory=str(album_directory(row,root));album=row.get('album') or ''
        album_id=digest([key,directory,strict_text(album)]);groups[album_id].append(row)
        duration=row.get('duration')
        if row.get('read_error') or not row.get('raw_title') or not row.get('artists') or not album or type(duration) not in (int,float) or not math.isfinite(duration) or duration<=0:continue
        matches.append((key,row['file_id'],strict_text(row['raw_title']),artist_key(row['artists']),strict_text(album),duration,album_id))
    for album_id,tracks in groups.items():
        first=tracks[0];artists=list(dict.fromkeys(a for row in tracks for a in effective_album_artists(row)))
        complete=all(row.get('raw_title') and row.get('artists') and row.get('album') and row.get('duration') and not row.get('read_error') for row in tracks)
        albums.append((key,album_id,json.dumps({'album_id':album_id,'album':first.get('album') or '待识别专辑','artists':artists,'directory':str(album_directory(first,root)),'track_count':len(tracks),'file_ids':[row['file_id'] for row in tracks],'state':'local_tags_matched' if complete else 'needs_metadata','release_confirmed':False,'updated_at':now},ensure_ascii=False)))
    return matches,albums


_UNKNOWN_ARTISTS={'unknown','未知','unknown artist','未知艺人'}


def _version(track):
    from musicops.domain.identity import version_from_title
    # Normalize tags and titles through the same version markers. An empty or
    # generic tag such as "Original"/"Album Version" is "not recorded", not
    # a different recording, so the title markers decide instead.
    explicit=track.get('recording_version') or track.get('version') or ''
    explicit=' '.join(map(str,explicit)) if isinstance(explicit,(list,tuple)) else str(explicit)
    if explicit and not explicit.lstrip().startswith(('(','[','（')):explicit='('+explicit+')'
    return version_from_title(explicit) or version_from_title(track.get('raw_title') or track.get('title',''))


def same_local_recording(target,row):
    """Fast-path identity gate: title/artist/album/duration are only candidates."""
    from musicops.domain.identity import _position
    if row.get('read_error') or row.get('filename_tag_conflict'):return False
    artists={strict_text(a) for a in row.get('artists') or [] if a}
    if not artists or artists&_UNKNOWN_ARTISTS:return False
    if _version(target)!=_version(row):return False
    for field in ('edition','release_id','catalogue'):
        wanted,found=strict_text(target.get(field)),strict_text(row.get(field))
        if wanted and wanted!=found:return False
    if (target.get('source') or 'netease')=='netease':
        # Local NetEase tags share the target's namespace; other platforms'
        # song/album IDs are not comparable with those tags.
        for field in ('source_song_id','source_album_id'):
            if target.get(field) and row.get(field) and str(target[field])!=str(row[field]):return False
    for field in ('disc','track'):
        wanted,found=_position(target.get(field)),_position(row.get(field))
        if wanted and found and wanted!=found:return False
    return True


def unchanged_library_file(row,root):
    path=Path(row['path']);reject_reparse_chain(path)
    if row.get('location_state')!='library' or type(row.get('size')) is not int or type(row.get('mtime_ns')) is not int:return False
    if not path.resolve().is_relative_to(Path(root).resolve()):return False
    stat=path.stat()
    return path.is_file() and (stat.st_size,stat.st_mtime_ns)==(row.get('size'),row.get('mtime_ns'))


class AlbumCatalog:
    def __init__(self, store): self.store = store

    def sync(self, root, files, checkpoint=lambda *_: None):
        files=[r for r in files if Path(r.get('path','')).suffix.casefold() in AUDIO_EXTENSIONS]
        key = root_key(root)
        versions = [{k: row.get(k) for k in ('file_id', 'size', 'mtime_ns', 'raw_title', 'artists', 'album', 'albumartist', 'duration', 'read_error', 'metadata_resolution')} for row in files]
        fingerprint = digest({'album_artist_policy':2,'files':versions})
        previous = self.store.get('evidence', 'library-catalog:' + key)
        if previous and previous.get('fingerprint') == fingerprint:
            return {**previous, 'reused': True}
        now=utcnow();matches,albums=_facts(root,files,key,now,checkpoint)
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

    def refresh_files(self,root,files,checkpoint=lambda *_:None,affected_directories=()):
        """Replace publication stubs and refresh touched albums without a library walk."""
        files=[r for r in files if Path(r.get('path','')).suffix.casefold() in AUDIO_EXTENSIONS]
        root=Path(root).absolute();key=root_key(root);now=utcnow()
        directories={str(album_directory(row,root)) for row in files}|{str(Path(p).absolute()) for p in affected_directories}
        for directory in directories:
            reject_reparse_chain(directory,root)
            if not Path(directory).is_relative_to(root):raise PathBlocked('目录刷新越过音乐库')
        if not directories:return self.summary(root)
        known={row['file_id']:row for row in files}
        for row in files:
            reject_reparse_chain(row['path'],root)
            if not Path(row['path']).is_relative_to(root):raise PathBlocked('文件刷新越过音乐库')
        with self.store.connection() as db:
            db.execute('BEGIN IMMEDIATE');old=[]
            for directory in directories:
                old.extend(json.loads(r[0]) for r in db.execute("SELECT payload FROM library_albums WHERE root_key=? AND json_extract(payload,'$.directory')=?",(key,directory)))
            ids=sorted({fid for album in old for fid in album['file_ids']}-set(known))
            for start in range(0,len(ids),400):
                batch=ids[start:start+400]
                known.update((r['file_id'],r) for r in (json.loads(v[0]) for v in db.execute('SELECT payload FROM library_files WHERE id IN ('+','.join('?' for _ in batch)+')',batch)))
            rows=[r for r in known.values() if r.get('location_state','library')=='library' and str(album_directory(r,root)) in directories]
            matches,albums=_facts(root,rows,key,now,checkpoint)
            for album in old:
                db.execute('DELETE FROM library_matches WHERE root_key=? AND album_id=?',(key,album['album_id']))
                db.execute('DELETE FROM library_albums WHERE root_key=? AND album_id=?',(key,album['album_id']))
            for row in files:
                db.execute('INSERT INTO library_files VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at',(row['file_id'],json.dumps(row,ensure_ascii=False),now))
                db.execute('DELETE FROM library_matches WHERE root_key=? AND file_id=?',(key,row['file_id']))
            db.executemany('INSERT OR REPLACE INTO library_matches VALUES(?,?,?,?,?,?,?)',matches)
            db.executemany('INSERT OR REPLACE INTO library_albums VALUES(?,?,?)',albums)
            totals=db.execute("SELECT COUNT(*),COALESCE(SUM(json_extract(payload,'$.track_count')),0) FROM library_albums WHERE root_key=?",(key,)).fetchone()
            matched=db.execute('SELECT COUNT(*) FROM library_matches WHERE root_key=?',(key,)).fetchone()[0]
            info={'root_key':key,'fingerprint':None,'album_count':totals[0],'track_count':totals[1],'matched_track_count':matched,'needs_metadata_count':totals[1]-matched,'updated_at':now}
            db.execute('INSERT INTO evidence VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET payload=excluded.payload,updated_at=excluded.updated_at',('library-catalog:'+key,json.dumps(info),now))
        return info

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
                    if not same_local_recording(target, row): continue
                    if fid not in verified:
                        valid = False
                        try: valid = unchanged_library_file(row, root)
                        except (OSError, RuntimeError, ValueError, PathBlocked): pass
                        verified[fid] = valid
                    if verified[fid]: present.add(target['target_id']); break
        return present
