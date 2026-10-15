"""Read user-selected playlist metadata; never open track URLs or media paths.

Formats are implemented independently against their documented interchange
fields. No exporter scripts or executable workbook content are evaluated.
"""
from __future__ import annotations

import csv
import hashlib
import io
import json
import math
import posixpath
import re
import stat
import zipfile
from pathlib import Path
from urllib.parse import parse_qs, urlsplit
from xml.etree import ElementTree as ET

from musicops.domain.identity import version_from_title

MAX_BYTES = 20 * 1024 * 1024
MAX_TRACKS = 50_000
MAX_FIELD = 10_000
PLATFORMS = ('netease', 'qqmusic', 'qishui', 'kugou', 'kuwo', 'migu', 'bilibili', 'applemusic', 'spotify', 'youtube')
ALIASES = {'qq': 'qqmusic', 'qq音乐': 'qqmusic', '网易': 'netease', '网易云': 'netease', '网易云音乐': 'netease',
           '汽水': 'qishui', '汽水音乐': 'qishui', '酷狗': 'kugou', '酷狗音乐': 'kugou', '酷我': 'kuwo',
           '酷我音乐': 'kuwo', '咪咕': 'migu', '咪咕音乐': 'migu', '哔哩哔哩': 'bilibili', 'b站': 'bilibili',
           'apple': 'applemusic', 'apple music': 'applemusic', 'youtube music': 'youtube', 'youtubemusic': 'youtube'}


class PlaylistFileError(ValueError):
    def __init__(self, code, reason):
        self.error_class = code
        super().__init__(reason)


def _fail(reason, code='INVALID_PLAYLIST_FILE'):
    raise PlaylistFileError(code, reason)


def _key(value):
    return re.sub(r'[\s_\-()（）\[\].:/]+', '', str(value).casefold())


FIELDS = {
    'title': ('raw_title', 'title', 'name', 'songname', 'trackname', 'song name', 'track title', '歌曲名', '歌曲名称', '歌名', '曲名', '歌曲', '曲目名称'),
    'artists': ('artists', 'artist', 'artist name', 'artist names', 'artist name(s)', 'artist(s)', 'singer', 'singerName', 'singers', 'ar', 'author', '歌手', '歌手名', '艺人', '演唱者'),
    'album': ('album', 'al', 'albumname', 'album name', 'albumObj', '专辑', '专辑名', '专辑名称'),
    'id': ('source_song_id', 'songmid', 'mid', 'songid', 'trackid', 'videoid', 'id', '歌曲id', '歌曲mid'),
    'album_id': ('source_album_id', 'albummid', 'albumid', '专辑id'),
    'duration_ms': ('durationMs', 'duration_ms', 'duration (ms)', 'length_ms', 'dt', '时长(毫秒)', '时长ms'),
    'duration': ('duration', 'duration_seconds', 'duration_sec', 'interval', 'length', '时长', '时长(秒)', '时长s'),
    'uri': ('track uri', 'track url', 'song url', 'sourceUrl', 'url', 'uri', '链接', '歌曲链接'),
    'source': ('source', 'platform', '平台', '来源平台'),
}
LOOKUP = {kind: {_key(alias) for alias in aliases} for kind, aliases in FIELDS.items()}


def _value(row, field):
    # Known synonyms are read in declared preference order. Unknown columns are
    # ignored, never reinterpreted by their position or their cell contents.
    normalized = {_key(k): v for k, v in row.items()}
    for alias in FIELDS[field]:
        value = normalized.get(_key(alias))
        if value is not None and value != '':
            return value
    return None


def _text(value, label, *, blank=True):
    if value is None:
        return ''
    if not isinstance(value, str):
        _fail(label + '必须是文本')
    value = value.strip()
    if len(value) > MAX_FIELD or any(ord(c) < 32 and c not in '\t\r\n' for c in value):
        _fail(label + '包含无效字符或过长')
    if not blank and not value:
        _fail(label + '不能为空')
    return value


def _platform(value, default='imported'):
    if value in (None, '', 'auto'):
        return default
    if not isinstance(value, str):
        _fail('来源平台必须是文本', 'INVALID_INPUT')
    value = value.strip().casefold()
    value = ALIASES.get(value, value)
    if not re.fullmatch(r'[a-z][a-z0-9_-]{0,31}', value):
        _fail('来源平台标识无效，请选择平台或使用 auto', 'INVALID_INPUT')
    return value


def _duration(value, milliseconds=False):
    if value is None or value == '':
        return None
    if isinstance(value, str):
        value = value.strip()
        if re.fullmatch(r'\d{1,3}:\d{2}(?::\d{2})?', value):
            parts = [int(x) for x in value.split(':')]
            if any(x >= 60 for x in parts[1:]):
                _fail('时长中的分或秒必须小于 60')
            value = sum(v * 60 ** i for i, v in enumerate(reversed(parts)))
            milliseconds = False
        else:
            try:
                value = float(value.removesuffix('s').strip())
            except ValueError:
                _fail('时长需要秒数、毫秒列或 mm:ss / hh:mm:ss')
    if type(value) not in (int, float) or not math.isfinite(value):
        _fail('时长必须是有限数值')
    value = value / 1000 if milliseconds else value
    if value <= 0:
        return None
    if value > 604800:
        _fail('单曲时长超过七天，检查秒与毫秒单位')
    return value


def _artists(value, canonical=False):
    if value is None:
        return []
    if isinstance(value, str):
        if len(value) > MAX_FIELD:
            _fail('艺人文本过长', 'LIMIT_EXCEEDED')
        # Canonical PlaylistOut defines comma-space as its artist separator.
        # Other text keeps commas (e.g. an artist named Earth, Wind & Fire).
        separator = r',\s+|\s+/\s+|;' if canonical else r'\s+/\s+|;'
        values = re.split(separator, value)
    elif isinstance(value, list):
        if len(value) > 100:
            _fail('单曲艺人数超过 100，请核对导出范围', 'LIMIT_EXCEEDED')
        values = []
        for item in value:
            if isinstance(item, dict):
                item = item.get('name') or item.get('title')
            values.append(item)
    elif isinstance(value, dict):
        values = [value.get('name') or value.get('title')]
    else:
        _fail('歌手需要文本或名称数组')
    if len(values) > 100:
        _fail('单曲艺人数超过 100，请核对导出范围', 'LIMIT_EXCEEDED')
    return list(dict.fromkeys(_text(item, '歌手') for item in values if item is not None and item != ''))


def _uri_identity(value):
    """Extract a platform identifier from a known detail URI without requests."""
    if not isinstance(value, str) or len(value) > MAX_FIELD:
        return None, None
    if value.startswith('spotify:track:'):
        return 'spotify', value.split(':')[-1]
    try:
        u = urlsplit(value)
        host = (u.hostname or '').lower()
        if u.scheme not in ('http', 'https') or u.username or u.password:
            return None, None
        query = parse_qs(u.query)
        if host in ('music.163.com', 'm.music.163.com') and u.path.rstrip('/') in ('/song', '/m/song'):
            return 'netease', query.get('id', [None])[0]
        if host == 'open.spotify.com' and u.path.startswith('/track/'):
            return 'spotify', u.path.split('/')[2]
        if host in ('youtube.com', 'www.youtube.com', 'music.youtube.com') and u.path == '/watch':
            return 'youtube', query.get('v', [None])[0]
        if host == 'youtu.be':
            return 'youtube', u.path.strip('/')
        if host in ('y.qq.com', 'i.y.qq.com'):
            match = re.search(r'/songDetail/([^/]+)', u.path)
            return 'qqmusic', match.group(1) if match else query.get('songmid', [None])[0]
    except ValueError:
        pass
    return None, None


def _identifier(value, source):
    if value is None or value == '':
        return None
    if type(value) is int:
        value = str(value)
    if not isinstance(value, str):
        _fail('歌曲或专辑 ID 必须是文本或整数')
    value = value.strip()
    if value.startswith(source + ':'):
        value = value[len(source) + 1:]
    if not re.fullmatch(r'[A-Za-z0-9_.:-]{1,256}', value):
        _fail('歌曲或专辑 ID 格式无效，不能用路径或下载地址代替 ID')
    return value


def _song(row, source, canonical=False):
    if not isinstance(row, dict):
        _fail('歌曲列表中的每一项必须是对象')
    # Spotify API playlist item and common QQ exports wrap their track metadata.
    for key in ('track', 'songInfo', 'songinfo'):
        if isinstance(row.get(key), dict):
            row = row[key]
            break
    known_fields = set().union(*LOOKUP.values())
    if not any(_key(key) in known_fields for key in row):
        _fail('歌曲对象没有已识别的歌名、艺人或 ID 字段；不会按字段位置猜测', 'NEEDS_MAPPING')
    uri_source, uri_id = _uri_identity(_value(row, 'uri'))
    row_source = _platform(_value(row, 'source'), source)
    if row_source == 'imported':
        if uri_source:
            row_source = uri_source
        elif 'songmid' in row or 'songname' in row or 'mid' in row and 'singer' in row:
            row_source = 'qqmusic'
        elif 'ar' in row and 'al' in row:
            row_source = 'netease'
    if uri_source and row_source != uri_source:
        _fail('歌曲链接平台与文件声明的来源不一致', 'SOURCE_MISMATCH')
    title = _text(_value(row, 'title'), '歌名')
    artists = [a for a in _artists(_value(row, 'artists'), canonical) if a]
    raw_album = _value(row, 'album')
    album_id = _value(row, 'album_id')
    if isinstance(raw_album, dict):
        album_id = album_id or raw_album.get('mid') or raw_album.get('id')
        raw_album = raw_album.get('name') or raw_album.get('title')
    album = _text(raw_album, '专辑')
    ms = _value(row, 'duration_ms')
    duration = _value(row, 'duration')
    legacy_netease = row_source == 'netease' and isinstance(row.get('album'), dict) and isinstance(row.get('artists'), list) and 'raw_title' not in row
    duration = _duration(ms if ms is not None else duration, ms is not None or legacy_netease)
    sid = _identifier(_value(row, 'id') or uri_id, row_source)
    aid = _identifier(album_id, row_source) if album_id not in (None, '', 0, '0') else None
    missing = [name for name, value in (('title', title), ('artists', artists), ('album', album), ('duration', duration)) if not value]
    if not sid:
        identity = json.dumps([title, artists, album, duration], ensure_ascii=False, separators=(',', ':'))
        sid = 'file-' + hashlib.sha256(identity.encode()).hexdigest()[:32]
        original_id = None
    else:
        original_id = sid
    namespaced = row_source + ':' + sid
    version = version_from_title(title)
    return {'id': namespaced, 'target_id': namespaced, 'source': row_source, 'source_song_id': original_id,
            'raw_title': title, 'artists': artists, 'album': album, 'source_album_id': aid, 'duration': duration,
            'version': version, 'recording_version': version, 'track': str(row.get('trackNumber') or row.get('tracknumber') or row.get('no') or ''),
            'disc': str(row.get('discNumber') or row.get('discnumber') or row.get('cd') or ''), 'aliases': [],
            'evidence_level': 'user_import', 'read_only': True, 'download_eligibility': 'UNKNOWN',
            'metadata_complete': not missing, 'metadata_missing_fields': missing}


def _json_rows(value):
    meta = {}
    for _ in range(6):
        if isinstance(value, list):
            return value, meta
        if not isinstance(value, dict):
            break
        meta.update({k: v for k, v in value.items() if not isinstance(v, (dict, list))})
        for key in ('tracks', 'songs', 'songlist', 'songList', 'items'):
            if key in value and isinstance(value[key], (list, dict)):
                value = value[key]
                break
        else:
            if isinstance(value.get('cdlist'), list):
                if len(value['cdlist']) != 1:
                    _fail('文件包含多个歌单，请逐个导出再导入', 'NEEDS_MAPPING')
                value = value['cdlist'][0]
            else:
                wrapper = next((value[k] for k in ('playlist', 'result', 'data') if isinstance(value.get(k), (dict, list))), None)
                if wrapper is None:
                    break
                value = wrapper
            continue
    _fail('JSON 没有已识别的 tracks / songs / songlist 数组；请导出 Canonical JSON 或带表头 CSV')


def _table(rows):
    iterator = iter(rows)
    header = next(iterator, None)
    if not isinstance(header, (list, tuple)) or not header:
        _fail('表格没有列名')
    names = [_text(x, '列名') if isinstance(x, str) else str(x or '') for x in header]
    normalized = [_key(x) for x in names]
    if len(names) > 128 or len(set(normalized)) != len(normalized):
        _fail('表格列数过多或包含重复列名')
    if not any(k in LOOKUP['title'] for k in normalized):
        _fail('无法识别歌名列，请使用 title / 歌名 等明确列名；不会按列位置猜测', 'NEEDS_MAPPING')
    result = []
    for row in iterator:
        if not isinstance(row, (list, tuple)):
            _fail('表格行结构无效')
        if not any(v is not None and str(v).strip() for v in row):
            continue
        if len(row) > len(names):
            _fail('表格行比列名多，检查 CSV 引号与分隔符')
        result.append(dict(zip(names, [*row, *[''] * (len(names) - len(row))])))
        if len(result) > MAX_TRACKS:
            _fail('单个文件最多导入 50000 首', 'LIMIT_EXCEEDED')
    return result


def _text_rows(text, order=None):
    if order not in (None, 'artist-title', 'title-artist'):
        _fail('文本顺序只能是 artist-title 或 title-artist', 'INVALID_INPUT')
    rows, meta = [], {}
    lines = text.splitlines()
    # PlaylistOut's documented TXT has a bordered metadata header and a fixed
    # title-artist order. Generic TXT has no such reliable order indication.
    first = next((i for i, line in enumerate(lines) if line.strip()), None)
    if first is not None and re.fullmatch(r'={5,}', lines[first].strip()):
        last = next((i for i in range(first + 1, len(lines)) if re.fullmatch(r'={5,}', lines[i].strip())), None)
        header = lines[first + 1:last] if last is not None else []
        if any(re.match(r'\s*导出工具\s*[:：]\s*Playlist Out\b', line) for line in header):
            if order not in (None, 'title-artist'):
                _fail('PlaylistOut TXT 声明为歌名 - 歌手，请选择该顺序', 'NEEDS_MAPPING')
            order = 'title-artist'
            meta['generator'] = 'PlaylistOut'
            for line in header:
                match = re.match(r'\s*歌单名称\s*[:：]\s*(.*)', line)
                if match:
                    meta['name'] = match[1]
                count = re.match(r'\s*歌曲总数\s*[:：]\s*(\d+)\s*首', line)
                partial = re.match(r'\s*已解析歌曲\s*[:：]\s*(\d+)\s*/\s*(\d+)\s*首', line)
                if count:
                    meta['trackCount'] = int(count[1])
                if partial:
                    meta['trackCount'] = int(partial[2]);meta['isPartial'] = int(partial[1]) < int(partial[2])
            lines = lines[last + 1:]
    for number, raw in enumerate(lines, 1):
        value = raw.strip()
        if not value:
            continue
        directive = re.match(r'^#\s*(order|format|platform|playlist)\s*:\s*(.*)$', value, re.I)
        if directive:
            key, item = directive[1].lower(), directive[2].strip()
            if key in ('order', 'format'):
                if item not in ('artist-title', 'title-artist'):
                    _fail('文本 order 注释无效')
                if order and item != order:
                    _fail('所选文本顺序与文件 order 注释不一致', 'NEEDS_MAPPING')
                order = item
            else:
                meta['name' if key == 'playlist' else key] = item
            continue
        if value.startswith('#'):
            continue
        if not order:
            _fail('TXT 歌名与歌手顺序不明确，请选择“歌手 - 歌名”或“歌名 - 歌手”，或改用带列名 CSV', 'NEEDS_MAPPING')
        parts = value.split(' - ')
        if len(parts) not in (2, 3) or not all(p.strip() for p in parts):
            _fail(f'第 {number} 行需要明确的“歌手 - 歌名”或“歌名 - 歌手”；有歧义时请改用 CSV', 'NEEDS_MAPPING')
        first, second = parts[:2]
        row = {'artist': first, 'title': second} if order == 'artist-title' else {'title': first, 'artist': second}
        if len(parts) == 3:
            row['album'] = parts[2]
        rows.append(row)
        if len(rows) > MAX_TRACKS:
            _fail('单个文件最多导入 50000 首', 'LIMIT_EXCEEDED')
    return rows, meta


def _m3u_rows(text):
    rows, meta, pending = [], {}, None
    for raw in text.splitlines():
        value = raw.strip()
        if not value:
            continue
        if value.startswith('#PLAYLIST:'):
            meta['name'] = value.split(':', 1)[1].strip()
        elif value.startswith('#EXTINF:'):
            if pending is not None:
                _fail('M3U 的 EXTINF 缺少对应条目行')
            match = re.fullmatch(r'#EXTINF:([+-]?[\d.]+)(?:\s+[^,]*)?,(.*)', value)
            if not match:
                _fail('M3U EXTINF 格式无效')
            description = match[2]
            parts = description.split(' - ', 1)
            pending = {'duration': match[1], 'artist': parts[0] if len(parts) == 2 else '', 'title': parts[-1]}
        elif not value.startswith('#'):
            # The following URI is metadata only. It is never opened or fetched.
            if pending is None:
                _fail('M3U 仅支持带 EXTINF 歌名元数据的条目，不从音源路径猜歌名')
            rows.append(pending)
            pending = None
            if len(rows) > MAX_TRACKS:
                _fail('单个文件最多导入 50000 首', 'LIMIT_EXCEEDED')
    if pending is not None:
        _fail('M3U 的最后一项缺少条目行')
    return rows, meta


def _xml(data):
    if b'<!DOCTYPE' in data.upper() or b'<!ENTITY' in data.upper():
        _fail('工作簿包含不支持的 XML 实体声明')
    return ET.fromstring(data)


def _xlsx_rows(data, checkpoint):
    """Read bounded OOXML tables, no extraction, formula evaluation or macros."""
    ns = {'s': 'http://schemas.openxmlformats.org/spreadsheetml/2006/main'}
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        entries = archive.infolist()
        if len(entries) > 200 or len({e.filename for e in entries}) != len(entries):
            _fail('工作簿 ZIP 成员过多或重名', 'LIMIT_EXCEEDED')
        expanded = 0
        for entry in entries:
            name = entry.filename.replace('\\', '/')
            if name.startswith('/') or '..' in name.split('/') or entry.flag_bits & 1:
                _fail('工作簿包含非法路径或加密成员')
            if 'vbaproject' in name.lower() or name.lower().endswith('.bin'):
                _fail('不支持带宏或二进制内容的工作簿')
            expanded += entry.file_size
            if entry.file_size > MAX_BYTES or entry.file_size > max(entry.compress_size, 1) * 250 or expanded > 60 * 1024 * 1024:
                _fail('工作簿解压大小或压缩比超过限制', 'LIMIT_EXCEEDED')
        def read(name):
            checkpoint('playlist_file_xlsx')
            return _xml(archive.read(name))
        shared = []
        if 'xl/sharedStrings.xml' in archive.namelist():
            for item in read('xl/sharedStrings.xml').findall('s:si', ns):
                shared.append(''.join(t.text or '' for t in item.iter('{'+ns['s']+'}t')))
                if len(shared) > 200_000 or len(shared[-1]) > MAX_FIELD:
                    _fail('工作簿文本过多或过长', 'LIMIT_EXCEEDED')
        sheets = read('xl/workbook.xml').findall('s:sheets/s:sheet', ns)
        if not sheets or len(sheets) > 50:
            _fail('工作簿缺少可读取工作表')
        relationships = read('xl/_rels/workbook.xml.rels')
        targets = {}
        for rel in relationships:
            if rel.get('TargetMode') == 'External' or not str(rel.get('Type')).endswith('/worksheet'):
                continue
            target = str(rel.get('Target', ''))
            if target.startswith('/'):
                target = target.lstrip('/')
            else:
                target = posixpath.normpath('xl/' + target)
            if not target.startswith('xl/worksheets/') or not target.endswith('.xml') or ':' in target:
                _fail('工作簿工作表路径不受支持')
            targets[rel.get('Id')] = target
        def table(sheet):
            target = targets.get(sheet.get('{http://schemas.openxmlformats.org/officeDocument/2006/relationships}id'))
            if not target:
                _fail('工作表引用缺失或指向外部地址')
            rows = []
            for index, row in enumerate(read(target).findall('s:sheetData/s:row', ns)):
                if index > MAX_TRACKS:
                    _fail('单个文件最多导入 50000 首', 'LIMIT_EXCEEDED')
                if index % 100 == 0:
                    checkpoint('playlist_file_xlsx_rows')
                cells = []
                for cell in row.findall('s:c', ns):
                    if cell.find('s:f', ns) is not None:
                        _fail('不支持公式单元格，请另存为纯值 CSV 或 JSON')
                    ref = re.fullmatch(r'([A-Z]+)[1-9][0-9]*', cell.get('r', ''))
                    if not ref:
                        _fail('工作簿单元格引用无效')
                    col = 0
                    for c in ref[1]:
                        col = col * 26 + ord(c) - 64
                    if col > 128 or col <= len(cells):
                        _fail('工作簿列数过多或顺序无效', 'LIMIT_EXCEEDED')
                    cells.extend([''] * (col - len(cells)))
                    kind = cell.get('t')
                    value = cell.findtext('s:v', '', ns)
                    if kind == 's':
                        if not value.isdigit() or int(value) >= len(shared):
                            _fail('工作簿共享文本引用无效')
                        value = shared[int(value)]
                    elif kind == 'inlineStr':
                        value = ''.join(t.text or '' for t in cell.findall('s:is//s:t', ns))
                    if len(value) > MAX_FIELD:
                        _fail('工作簿单元格过长', 'LIMIT_EXCEEDED')
                    cells[-1] = value
                if any(cells):
                    rows.append(cells)
            return rows
        tracks = next((s for s in sheets if s.get('name') == 'Tracks'), sheets[0])
        meta = {}
        info = next((s for s in sheets if s.get('name') == 'Playlist Info'), None)
        if info is not None:
            for row in table(info)[1:]:
                if len(row) >= 2:
                    meta[row[0]] = row[1]
        return _table(table(tracks)), meta


def import_playlist_file(path, platform='auto', checkpoint=None, text_order=None):
    """Import one explicit absolute metadata file, without modifying any file.

    Bare TXT order is intentionally unresolved unless ``text_order`` or a file
    directive states ``artist-title`` / ``title-artist``. Cancellation exceptions
    raised by ``checkpoint(phase)`` propagate to the caller.
    """
    checkpoint = checkpoint or (lambda *_: None)
    warnings = []
    try:
        selected = _platform(platform)
        file = Path(path)
        if not file.is_absolute():
            _fail('请选择歌单文件的完整路径', 'INVALID_INPUT')
        if file.suffix.lower() not in ('.json', '.csv', '.tsv', '.txt', '.m3u', '.m3u8', '.xlsx'):
            _fail('支持 JSON、CSV、TSV、TXT、M3U / M3U8 和不含宏的 XLSX', 'UNSUPPORTED_FORMAT')
        before = file.stat()
        if not stat.S_ISREG(before.st_mode):
            _fail('请选择普通歌单文件', 'INVALID_INPUT')
        if before.st_size > MAX_BYTES:
            _fail('歌单文件不能超过 20 MB', 'LIMIT_EXCEEDED')
        checkpoint('playlist_file_read')
        with file.open('rb') as stream:
            data = stream.read(MAX_BYTES + 1)
        after = file.stat()
        if len(data) > MAX_BYTES:
            _fail('歌单文件不能超过 20 MB', 'LIMIT_EXCEEDED')
        if (before.st_size, before.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
            _fail('文件在读取过程中发生变化，请重新选择', 'SOURCE_CHANGED')
        meta = {}
        canonical = False
        if file.suffix.lower() == '.xlsx':
            rows, meta = _xlsx_rows(data, checkpoint)
            encoding = 'OOXML'
            canonical = meta.get('generator') == 'PlaylistOut'
        else:
            try:
                text = data.decode('utf-8-sig')
                encoding = 'UTF-8'
            except UnicodeDecodeError:
                text = data.decode('gb18030')
                encoding = 'GB18030'
                warnings.append({'error_class': 'ENCODING_FALLBACK', 'reason': '文件按 GB18030 解码；建议导出 UTF-8 JSON / CSV'})
            if '\x00' in text:
                _fail('文件包含二进制或 NUL 内容')
            suffix = file.suffix.lower()
            if suffix == '.json':
                def reject_constant(_):
                    _fail('JSON 不接受 NaN 或 Infinity')
                def unique_keys(pairs):
                    result = {}
                    for key, value in pairs:
                        if key in result:
                            _fail('JSON 含重复字段，无法确认应采用的值', 'SOURCE_CONFLICT')
                        result[key] = value
                    return result
                rows, meta = _json_rows(json.loads(text, parse_constant=reject_constant, object_pairs_hook=unique_keys))
                canonical = meta.get('generator') == 'PlaylistOut' or bool(meta.get('platform') and 'trackCount' in meta)
                if rows and isinstance(rows[0], list):
                    rows = _table(rows)
                elif rows and isinstance(rows[0], str):
                    rows, _ = _text_rows('\n'.join(rows), text_order)
            elif suffix in ('.m3u', '.m3u8'):
                rows, meta = _m3u_rows(text)
            elif suffix in ('.csv', '.tsv') or '\t' in text.partition('\n')[0]:
                lines = text.splitlines(keepends=True)
                start = next((i for i, line in enumerate(lines) if not line.lstrip().startswith('#')), len(lines))
                lines = lines[start:]
                delimiter = '\t' if suffix == '.tsv' or '\t' in (lines[0] if lines else '') else ','
                if suffix == '.csv' and lines and ',' not in lines[0] and ';' in lines[0]:
                    delimiter = ';'
                rows = _table(csv.reader(io.StringIO(''.join(lines)), delimiter=delimiter, strict=True))
                canonical = bool(rows and {'title', 'artist', 'album', 'isrc', 'duration', 'url', 'index', 'status'}.issubset({_key(k) for k in rows[0]}))
            else:
                rows, meta = _text_rows(text, text_order)
        canonical = canonical or meta.get('generator') == 'PlaylistOut'
        if len(rows) > MAX_TRACKS:
            _fail('单个文件最多导入 50000 首', 'LIMIT_EXCEEDED')
        declared_source = _platform(meta.get('platform') or meta.get('source'))
        if selected != 'imported' and declared_source != 'imported' and selected != declared_source:
            _fail('所选平台与文件声明的平台不一致', 'SOURCE_MISMATCH')
        source = selected if selected != 'imported' else declared_source
        songs, seen = [], {}
        for index, row in enumerate(rows):
            if index % 50 == 0:
                checkpoint('playlist_file_normalize')
            song = _song(row, source, canonical)
            previous = seen.get(song['target_id'])
            if previous is not None:
                if any(previous[k] != song[k] for k in ('raw_title', 'artists', 'album', 'duration')):
                    _fail('同一来源歌曲 ID 对应冲突资料，请核对导出文件', 'SOURCE_CONFLICT')
                continue
            songs.append(song)
            seen[song['target_id']] = song
        sources = {s['source'] for s in songs}
        if source != 'imported' and sources - {source}:
            _fail('曲目来源与歌单声明的平台不一致', 'SOURCE_MISMATCH')
        if source == 'imported' and len(sources) == 1:
            source = next(iter(sources))
        declared = next((meta[k] for k in ('trackCount', 'declared_count', 'total', 'total_song_num', 'songnum', 'songcount', 'totalCount') if k in meta), len(rows))
        if isinstance(declared, str) and declared.isdigit():
            declared = int(declared)
        if type(declared) is not int or declared < 0 or declared > MAX_TRACKS:
            _fail('歌单声明曲目数无效或超过 50000 首', 'LIMIT_EXCEEDED')
        partial = meta.get('isPartial') in (True, 'true', 'True', '1') or meta.get('complete') is False
        if declared != len(rows):
            warnings.append({'error_class': 'COUNT_MISMATCH', 'declared_count': declared, 'imported_rows': len(rows)})
            partial = True
        if len(songs) != len(rows):
            warnings.append({'error_class': 'DUPLICATE_TRACKS', 'count': len(rows) - len(songs), 'reason': '相同来源歌曲 ID 已合并，原始条目数保留'})
            partial = True
        incomplete = sum(not s['metadata_complete'] for s in songs)
        if incomplete:
            warnings.append({'error_class': 'INCOMPLETE_SONG_METADATA', 'count': incomplete, 'reason': '缺失资料保留为空，需要核对；不会伪造歌手或专辑'})
        if not rows:
            warnings.append({'error_class': 'EMPTY_PLAYLIST', 'reason': '文件没有曲目条目'})
        digest = hashlib.sha256(data).hexdigest()
        variant = hashlib.sha256((source + ':' + str(text_order) + ':' + digest).encode()).hexdigest()
        complete = not partial and not incomplete
        snapshot = {'playlist_id': 'file:' + variant, 'snapshot_id': source + ':file:' + variant,
                    'source': source, 'name': _text(meta.get('name') or meta.get('dissname') or file.stem, '歌单名'),
                    'songs': songs, 'complete': complete, 'visible_catalogue_complete': not partial, 'metadata_complete': not incomplete,
                    'declared_count': declared, 'observed_id_count': len(rows), 'imported_count': len(songs),
                    'warnings': warnings, 'read_only': True, 'network_used': False, 'evidence_level': 'user_import',
                    'contract': 'musicops.playlist-file.v1', 'file_sha256': digest, 'encoding': encoding}
        return {'status': 'OK' if complete else 'SNAPSHOT_INCOMPLETE', 'snapshot': snapshot, 'network_used': False, 'read_only': True}
    except PlaylistFileError as exc:
        return {'status': exc.error_class, 'error_class': exc.error_class, 'reason': str(exc), 'read_only': True, 'network_used': False}
    except (OSError, UnicodeError, json.JSONDecodeError, csv.Error, zipfile.BadZipFile, ET.ParseError, RecursionError, KeyError, TypeError, ValueError, NotImplementedError):
        return {'status': 'INVALID_PLAYLIST_FILE', 'error_class': 'INVALID_PLAYLIST_FILE', 'reason': '歌单文件无法读取或格式不符；请选择 UTF-8 JSON / 带列名 CSV 等支持格式', 'read_only': True, 'network_used': False}
