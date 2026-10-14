"""Full-file audio validation tied to fresh content, tool versions and exact parameters."""
import hashlib
import copy
import json
import math
import os
import shutil
import subprocess
import tempfile
import uuid
from datetime import datetime, timezone
from pathlib import Path
from collections import OrderedDict

from musicops.domain.errors import Conflict, ValidationFailed
from musicops.library.index import file_version, sha256_file
from musicops.library.paths import reject_reparse_chain

DECODE_ARGS = ['-nostdin', '-v', 'error', '-xerror', '-err_detect', 'explode']
SUPPORTED_SUFFIXES = {'.flac', '.m4a', '.aac', '.aif', '.aiff', '.mp3', '.wav', '.ogg', '.dsf', '.dff'}
SUFFIX_CODECS = {'.flac': {'flac'}, '.m4a': {'alac', 'aac'}, '.aac': {'aac'}, '.aif': {'pcm_s16be', 'pcm_s24be', 'pcm_s32be', 'pcm_f32be', 'pcm_f64be'},
                 '.aiff': {'pcm_s16be', 'pcm_s24be', 'pcm_s32be', 'pcm_f32be', 'pcm_f64be'}, '.mp3': {'mp3'},
                 '.wav': {'pcm_s16le', 'pcm_s24le', 'pcm_s32le', 'pcm_u8', 'pcm_f32le', 'pcm_f64le'}, '.ogg': {'vorbis'},
                 '.dsf': {'dsd_lsbf', 'dsd_lsbf_planar'}, '.dff': {'dsd_msbf', 'dsd_msbf_planar'}}


def _run(command, timeout):
    return subprocess.run(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          timeout=timeout, creationflags=getattr(subprocess, 'CREATE_NO_WINDOW', 0))


def _unsupported(text):
    lowered = text.casefold()
    return any(fragment in lowered for fragment in ['unknown decoder', 'decoder not found', 'decoding requested, but no decoder found', 'unsupported codec', 'not yet implemented', 'unsupported version'])


class MediaValidator:
    def __init__(self, ffmpeg='ffmpeg', ffprobe='ffprobe', timeout=60):
        self.ffmpeg, self.ffprobe, self.timeout = str(ffmpeg), str(ffprobe), timeout
        self._cache = OrderedDict()

    def tool_versions(self):
        versions = {}
        for name, executable in [('ffmpeg', self.ffmpeg), ('ffprobe', self.ffprobe)]:
            value = _run([executable, '-version'], min(self.timeout, 10))
            if value.returncode:
                raise OSError('媒体验证器版本查询失败')
            versions[name] = value.stdout.decode('utf-8', 'replace').splitlines()[0]
        return versions

    def validate(self, path):
        path = Path(path)
        result = {'schema_version': 1, 'validation_id': str(uuid.uuid4()), 'path': str(path), 'file_hash': None,
                  'size': None, 'mtime_ns': None, 'tool_versions': {}, 'parameters': DECODE_ARGS + ['-map', '0:a', '-f', 'null', '-'],
                  'format': None, 'streams': [], 'media_info': {}, 'duration': None, 'decode_state': 'tool_error',
                  'returncode': None, 'error_class': None, 'checked_at': datetime.now(timezone.utc).isoformat(),
                  'roon_compatible': False, 'cache_hit': False}
        try:
            before = file_version(path)
            content_hash = sha256_file(path)
            versions = self.tool_versions()
            result.update(file_hash=content_hash, size=before[0], mtime_ns=before[1], tool_versions=versions)
            key = (content_hash, before, path.suffix.casefold(), json.dumps(versions, sort_keys=True), tuple(result['parameters']))
            if key in self._cache:
                # Hash is freshly calculated even for a cache hit, never path-only.
                cached = copy.deepcopy(self._cache[key])
                if file_version(path) != before or sha256_file(path) != content_hash:
                    result.update(decode_state='changed_during_check', error_class='CHANGED_DURING_CHECK')
                    return result
                cached.update(path=str(path), cache_hit=True)
                self._cache.move_to_end(key)
                return cached
            probe = _run([self.ffprobe, '-v', 'error', '-show_format', '-show_streams', '-of', 'json', str(path)], self.timeout)
            probe_text = probe.stderr.decode('utf-8', 'replace')
            if probe.returncode:
                result.update(decode_state='unsupported' if _unsupported(probe_text) else 'corrupt', returncode=probe.returncode,
                              error_class='UNSUPPORTED' if _unsupported(probe_text) else 'PROBE_FAILED', error_summary=probe_text[:2000])
            else:
                info = json.loads(probe.stdout)
                if not isinstance(info,dict) or not isinstance(info.get('streams',[]),list) or not isinstance(info.get('format',{}),dict):
                    raise ValueError('probe output shape invalid')
                streams = info.get('streams', [])
                if any(not isinstance(stream,dict) for stream in streams):
                    raise ValueError('probe stream shape invalid')
                audios = [s for s in streams if s.get('codec_type') == 'audio']
                result.update(format=info.get('format', {}), streams=streams)
                if not audios:
                    result.update(decode_state='unsupported', error_class='NO_AUDIO_STREAM')
                else:
                    first = audios[0]
                    result['media_info'] = {k: first.get(k) for k in ['codec_name', 'sample_fmt', 'sample_rate', 'channels', 'channel_layout', 'bits_per_sample', 'bits_per_raw_sample', 'bit_rate']}
                    try:
                        result['duration'] = float(info.get('format', {}).get('duration', first.get('duration')))
                    except (TypeError, ValueError):
                        result['duration'] = None
                    decode = _run([self.ffmpeg] + DECODE_ARGS + ['-i', str(path), '-map', '0:a', '-f', 'null', '-'], self.timeout)
                    decode_text = decode.stderr.decode('utf-8', 'replace')
                    result.update(returncode=decode.returncode, error_summary=decode_text[:2000])
                    if decode.returncode or decode_text.strip():
                        result.update(decode_state='unsupported' if _unsupported(decode_text) else 'corrupt', error_class='UNSUPPORTED' if _unsupported(decode_text) else 'DECODE_FAILED')
                    elif result['duration'] is None or not math.isfinite(result['duration']) or result['duration'] <= 0:
                        result.update(decode_state='corrupt', error_class='DURATION_INVALID')
                    else:
                        result.update(decode_state='passed', error_class=None)
                        codec = first.get('codec_name', '')
                        result['roon_compatible'] = codec in SUFFIX_CODECS.get(path.suffix.casefold(), set())
            after = file_version(path)
            if before != after or sha256_file(path) != content_hash:
                result.update(decode_state='changed_during_check', error_class='CHANGED_DURING_CHECK', roon_compatible=False)
            if result['decode_state'] in {'passed', 'corrupt', 'unsupported'}:
                self._cache[key] = copy.deepcopy(result)
                self._cache.move_to_end(key)
                if len(self._cache)>1024:
                    self._cache.popitem(last=False)
        except subprocess.TimeoutExpired:
            result.update(decode_state='timeout', error_class='TIMEOUT')
        except (OSError, ValueError, json.JSONDecodeError) as exc:
            result.update(decode_state='tool_error', error_class='TOOL_ERROR', error_summary=type(exc).__name__)
        return result

    def pcm_hash(self, path):
        # Full decoded PCM identity; codec/container and tags are excluded. Channels/sample rate remain grouping fields.
        before = file_version(path)
        content_hash = sha256_file(path)
        value = _run([self.ffmpeg] + DECODE_ARGS + ['-i', str(path), '-map', '0:a:0', '-c:a', 'pcm_s32le', '-f', 'hash', '-hash', 'sha256', '-'], self.timeout)
        if value.returncode or value.stderr.strip():
            raise ValidationFailed('PCM全解码校验失败')
        line = value.stdout.decode('ascii').strip()
        if not line.startswith('SHA256=') or len(line) != 71 or any(c not in '0123456789abcdefABCDEF' for c in line[7:]):
            raise ValidationFailed('PCM哈希工具输出契约错误')
        if before != file_version(path) or content_hash != sha256_file(path):
            raise Conflict('来源在PCM解码过程中被外部修改')
        return line.split('=', 1)[1]


def prepare_media_copy(source, destination, validator, *, convert_to=None):
    """Copy/conversion only; never modifies the seeding source. Caller supplies approved work path."""
    source, destination = Path(source), Path(destination)
    reject_reparse_chain(source)
    reject_reparse_chain(destination)
    if destination.exists():
        raise Conflict('工作输出已经存在，不能覆盖')
    source_hash = sha256_file(source)
    source_validation = validator.validate(source)
    if source_validation['decode_state'] != 'passed':
        raise ValidationFailed('来源未通过全解码')
    destination.parent.mkdir(parents=True, exist_ok=True)
    reject_reparse_chain(destination)
    handle, name = tempfile.mkstemp(prefix='.media-', suffix=destination.suffix, dir=destination.parent)
    os.close(handle)
    prepared = Path(name)
    written_hash, owned_version = hashlib.sha256(), None
    success = False
    try:
        if convert_to:
            if convert_to != 'flac' or str(source_validation['media_info'].get('codec_name', '')).startswith('dsd'):
                raise ValidationFailed('仅实现PCM到FLAC；DSD转换必须独立策略')
            process = _run([validator.ffmpeg, '-nostdin', '-v', 'error', '-xerror', '-i', str(source), '-map', '0:a:0', '-map_metadata', '0', '-c:a', 'flac', '-y', str(prepared)], validator.timeout)
            if process.returncode or process.stderr.strip():
                raise ValidationFailed('工作副本转换失败')
        else:
            shutil.copy2(source, prepared)
        after = validator.validate(prepared)
        if after['decode_state'] != 'passed' or not after['roon_compatible']:
            raise ValidationFailed('工作副本未通过完整性/兼容门')
        if convert_to and validator.pcm_hash(source) != validator.pcm_hash(prepared):
            raise ValidationFailed('转换前后解码PCM不一致')
        if sha256_file(source) != source_hash:
            raise Conflict('来源在工作副本准备过程中被外部修改')
        reject_reparse_chain(destination)
        try:
            # Exclusive create also protects against a target appearing while
            # the expensive decode/conversion is in progress.
            with prepared.open('rb') as src, destination.open('xb') as dst:
                owned_version = os.fstat(dst.fileno())
                while block := src.read(1024 * 1024):
                    dst.write(block)
                    written_hash.update(block)
                dst.flush()
                os.fsync(dst.fileno())
        except FileExistsError as exc:
            raise Conflict('工作输出在准备过程中被创建，不能覆盖') from exc
        if sha256_file(source) != source_hash:
            raise Conflict('来源在工作副本准备过程中被外部修改')
        if sha256_file(destination) != after['file_hash']:
            raise Conflict('工作输出在准备过程中被外部修改')
        after = {**after, 'path': str(destination), 'mtime_ns': destination.stat().st_mtime_ns}
        success = True
        return {'schema_version': 1, 'source_hash': source_hash, 'destination': str(destination), 'after_hash': after['file_hash'], 'validation': after, 'converted': bool(convert_to)}
    finally:
        prepared.unlink(missing_ok=True)
        if not success and owned_version is not None and destination.is_file():
            current = destination.stat()
            if (current.st_dev,current.st_ino)==(owned_version.st_dev,owned_version.st_ino) and sha256_file(destination)==written_hash.hexdigest():
                destination.unlink()
