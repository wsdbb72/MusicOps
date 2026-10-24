"""Manifest-backed reversible publication, independent of qBittorrent and token state."""
import contextlib
import hashlib
import json
import os
import shutil
import uuid
from datetime import datetime, timezone
from pathlib import Path

from musicops.domain.errors import Conflict, PathBlocked, RecoveryRequired, ValidationFailed
from musicops.media.validation import MediaValidator
from .index import AUDIO_EXTENSIONS, file_version, sha256_file
from .paths import reject_reparse_chain, safe_destination, validate_roots, within
from .resources import companion_relations

TERMINAL = {'COMMITTED', 'ROLLED_BACK', 'CONFLICT'}


def now():
    return datetime.now(timezone.utc).isoformat()


def digest(data):
    return hashlib.sha256(json.dumps(data, sort_keys=True, ensure_ascii=False, separators=(',', ':')).encode()).hexdigest()


def atomic_json(path, data):
    path = Path(path)
    reject_reparse_chain(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + '.' + str(uuid.uuid4()) + '.tmp')
    reject_reparse_chain(temporary)
    with temporary.open('x', encoding='utf-8') as stream:
        json.dump(data, stream, ensure_ascii=False, indent=2)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def durable_copy(source, destination):
    """Always copy bytes: neither link nor move the source."""
    source, destination = Path(source), Path(destination)
    reject_reparse_chain(source)
    reject_reparse_chain(destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    reject_reparse_chain(destination)
    with source.open('rb') as src, destination.open('xb') as dst:
        shutil.copyfileobj(src, dst, 1024 * 1024)
        dst.flush()
        os.fsync(dst.fileno())
    shutil.copystat(source, destination)
    if os.path.samefile(source, destination):
        raise ValidationFailed('复制变成相同文件/硬链接')


class TransactionEngine:
    def __init__(self, store, paths, validator=None):
        self.store, self.paths = store, dict(paths)
        self.roots = validate_roots(paths)
        tools = paths.get('tools', {})
        self.validator = validator or MediaValidator(tools.get('ffmpeg') or 'ffmpeg', tools.get('ffprobe') or 'ffprobe')

    @contextlib.contextmanager
    def _lock(self):
        # Service owns its writer lock; this second lock protects direct/CLI transaction calls too.
        validate_roots(self.paths)
        self.roots['work'].mkdir(parents=True, exist_ok=True)
        lock_path = self.roots['work'] / '.transaction-writer.lock'
        reject_reparse_chain(lock_path, self.roots['work'])
        with lock_path.open('a+b') as stream:
            if stream.tell() == 0:
                stream.write(b'0')
                stream.flush()
            stream.seek(0)
            try:
                if os.name == 'nt':
                    import msvcrt
                    msvcrt.locking(stream.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(stream.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except OSError as exc:
                raise Conflict('另一文件事务正在运行') from exc
            try:
                yield
            finally:
                stream.seek(0)
                if os.name == 'nt':
                    msvcrt.locking(stream.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    fcntl.flock(stream.fileno(), fcntl.LOCK_UN)

    def _manifest_path(self, tx_id):
        return safe_destination(self.roots['backup'], f'{tx_id}/manifest.json')

    def _write(self, tx, *, database=True):
        tx['updated_at'] = now()
        atomic_json(self._manifest_path(tx['tx_id']), tx)
        if database:
            self.store.put('transactions', tx['tx_id'], tx)

    def _event(self, phase, tx, details=None):
        self.store.event(phase, {'plan_id': tx['plan_id'], 'state': tx['state'],
                                'requirements': ['02 §15', 'T15', 'F16', 'F22', 'F23', 'F26'],
                                'action': phase, 'result': details or {'state': tx['state']},
                                'artifact': str(self._manifest_path(tx['tx_id']))}, tx_id=tx['tx_id'])

    def _fault(self, fault, phase, tx):
        if callable(fault):
            fault(phase, tx)
        elif fault == phase:
            raise RecoveryRequired('离线注入中断:' + phase)

    def _check_hash(self, path, expected, *, must_exist=None):
        path = Path(path)
        reject_reparse_chain(path)
        if expected is None:
            if path.exists() or must_exist:
                raise Conflict('预期路径不存在，但当前已有文件')
        elif not path.is_file() or sha256_file(path) != expected:
            raise Conflict('文件与新鲜前置内容哈希不一致')

    def _operation(self, operation, index, tx_id):
        kind = operation.get('kind', 'publish')
        if kind not in {'publish', 'replace', 'quarantine'}:
            raise ValidationFailed('未知文件操作类型')
        if not operation.get('operation_id') or 'before_hash' not in operation:
            raise ValidationFailed('操作缺operation_id/before_hash')
        source = Path(operation['source']).absolute()
        reject_reparse_chain(source)
        if not source.is_file():
            raise ValidationFailed('来源不是现有普通文件')
        if kind == 'quarantine':
            if not within(source, self.roots['library']):
                raise PathBlocked('仅主库副本可以隔离，不动下载/保种源')
            destination = safe_destination(self.roots['quarantine'], operation['destination'])
            self._check_hash(source, operation['before_hash'])
            self._check_hash(destination, None)
            reason = operation.get('quarantine_reason')
            if reason not in {'duplicate', 'corrupt', 'manual_suppression', 'compilation_merge'}:
                raise ValidationFailed('隔离原因须明确duplicate/corrupt/manual_suppression/compilation_merge')
        else:
            if not any(within(source, self.roots[key]) for key in ['source', 'work']):
                raise PathBlocked('发布来源必须位于来源或工作根目录')
            destination = safe_destination(self.roots['library'], operation['destination'])
            self._check_hash(destination, operation['before_hash'])
            if kind == 'publish' and operation['before_hash'] is not None:
                raise Conflict('新增发布不能静默覆盖；请使用replace')
            if kind == 'replace' and operation['before_hash'] is None:
                raise ValidationFailed('替换必须给旧内容哈希')
        if operation.get('field_locks'):
            if any(field in operation['field_locks'] for field in operation.get('tag_changes', {})):
                raise Conflict('所选字段被用户锁定')
        source_hash = sha256_file(source)
        if operation.get('source_hash') and source_hash != operation['source_hash']:
            raise Conflict('来源内容在计划后发生变化')
        staging = safe_destination(self.roots['work'], f'{tx_id}/staging/{index:04d}{source.suffix}')
        # Reserve enough room for same-volume atomic publish/restore temporary names too.
        temp_name = destination.with_name(f'.{destination.name}.{tx_id}.publish')
        destination_root = self.roots['quarantine'] if kind == 'quarantine' else self.roots['library']
        safe_destination(destination_root, temp_name.relative_to(destination_root))
        return {**operation, 'schema_version': 1, 'kind': kind, 'source': str(source), 'destination': str(destination),
                'source_hash': source_hash, 'staging': str(staging), 'after_hash': None,
                'published': False, 'source_removed': False, 'validation': None}

    def _build(self, plan, tx_id):
        operations = [self._operation(op, i, tx_id) for i, op in enumerate(plan['operations'])]
        ids = [op['operation_id'] for op in operations]
        if len(ids) != len(set(ids)):
            raise Conflict('同计划operation_id重复')
        destinations = [op['destination'].casefold() for op in operations]
        if len(destinations) != len(set(destinations)):
            raise Conflict('同计划目标路径重复/大小写碰撞')
        with self.store.connection() as connection:
            for offset in range(0,len(ids),500):
                page=ids[offset:offset+500]
                if connection.execute("SELECT 1 FROM transaction_operations WHERE operation_id IN ("+','.join('?' for _ in page)+") AND transaction_id!=? AND (state IS NULL OR state NOT IN ('ROLLED_BACK','CONFLICT')) LIMIT 1",(*page,tx_id)).fetchone():
                    raise Conflict('operation_id已归属于其他事务，不重复执行')
        group = None
        if all(op['kind'] == 'publish' for op in operations):
            common = Path(os.path.commonpath([str(Path(op['destination']).parent) for op in operations]))
            if common != self.roots['library'] and not common.exists():
                # Complete new directory is prepared outside the watched root and renamed once.
                group_staging = safe_destination(self.roots['work'], f'{tx_id}/release_directory')
                def volume(path):
                    while not path.exists():
                        path = path.parent
                    return path.stat().st_dev
                if volume(self.roots['work']) != volume(self.roots['library']):
                    raise ValidationFailed('新发行整目录原子发布要求工作根与主库同卷；跨卷先配置同卷工作区')
                group = {'destination': str(common), 'staging': str(group_staging), 'renamed': False}
        affected = {}
        for op in operations:
            old_path = Path(op['source'] if op['kind'] == 'quarantine' else op['destination'])
            if old_path.is_file():
                affected.setdefault(str(old_path), {'path': str(old_path), 'before_hash': sha256_file(old_path), 'role': 'affected'})
                affected[str(old_path)]['affected'] = True
                if old_path.suffix.casefold() in AUDIO_EXTENSIONS:
                    for resource in companion_relations(old_path):
                        related = Path(resource['path'])
                        if not within(related, self.roots['library']):
                            raise PathBlocked('伴随资源越出主库')
                        reject_reparse_chain(related)
                        affected.setdefault(str(related), {'path': str(related), 'before_hash': sha256_file(related), 'role': resource['type'], 'relation': resource})
        backups = []
        for index, item in enumerate(affected.values()):
            item['backup'] = str(safe_destination(self.roots['backup'], f'{tx_id}/originals/{index:04d}{Path(item["path"]).suffix}'))
            item['backed_up'] = False
            backups.append(item)
        estimated = sum(Path(op['source']).stat().st_size * 2 for op in operations) + sum(Path(item['path']).stat().st_size for item in backups)
        # Conservative per-volume peak includes work + publication + originals; originals are never deleted to free space.
        for root in self.roots.values():
            existing = root
            while not existing.exists():
                existing = existing.parent
            if shutil.disk_usage(existing).free < estimated + int(plan.get('safety_margin_bytes', 1024 * 1024)):
                raise ValidationFailed('磁盘峰值预算不足')
        paths=list(set(affected)|{op['destination'] for op in operations});before_rows=[]
        with self.store.connection() as connection:
            for offset in range(0,len(paths),500):
                page=paths[offset:offset+500]
                before_rows.extend(json.loads(row[0]) for row in connection.execute("SELECT payload FROM library_files WHERE json_extract(payload,'$.path') IN ("+','.join('?' for _ in page)+')',page))
        return {'schema_version': 1, 'tx_id': tx_id, 'plan_id': plan['plan_id'], 'plan_hash': digest(plan), 'config_hash': plan['config_hash'],
                'snapshot_hash': plan.get('snapshot_hash'), 'state': 'PLANNED', 'created_at': now(), 'operations': operations,
                'backup_manifest': backups, 'staging_manifest': [], 'publish_manifest': [], 'state_before': before_rows,
                'error': None, 'roon_state': 'pending', 'estimated_peak_bytes': estimated, 'client_actions': [], 'created_directories': [], 'directory_publication': group}

    def execute(self, plan, fault=None):
        if plan.get('schema_version', 1) != 1 or not plan.get('plan_id') or not plan.get('config_hash') or not plan.get('operations'):
            raise ValidationFailed('计划缺schema/plan_id/config_hash/operations')
        tx_id = str(uuid.uuid5(uuid.NAMESPACE_URL, 'musicops:transaction:' + str(plan['plan_id'])))
        with self._lock():
            previous = self.store.get('transactions', tx_id)
            manifest = self._manifest_path(tx_id)
            if manifest.is_file():
                previous = self._load(tx_id)
            if previous:
                if previous['plan_hash'] != digest(plan):
                    raise Conflict('plan_id已用于不同内容的计划')
                if previous['state'] == 'COMMITTED':
                    self._verify_published(previous, verify_sources=False)
                    return {**previous, 'idempotent': True}
                if previous['state'] in {'ROLLED_BACK', 'CONFLICT'}:
                    return {**previous, 'idempotent': True}
                raise RecoveryRequired('已有未完成事务，先recover/rollback；不能重复复制')
            tx = self._build(plan, tx_id)
            self.store.put('transactions', tx_id, tx)  # Intent must succeed before the first backup/copy side effect.
            self._write(tx)
            try:
                self._event('transaction_planned', tx)
                for item in tx['backup_manifest']:
                    self._check_hash(item['path'], item['before_hash'])
                    durable_copy(item['path'], item['backup'])
                    self._check_hash(item['backup'], item['before_hash'])
                    item['backed_up'] = True
                    self._write(tx)
                    self._event('original_backup_verified', tx, {'path': item['path'], 'backup': item['backup'], 'hash': item['before_hash'], 'role': item['role']})
                tx['state'] = 'PREPARED'
                self._write(tx)
                self._event('transaction_prepared', tx)
                self._fault(fault, 'after_backup', tx)
                for op in tx['operations']:
                    self._check_hash(op['source'], op['source_hash'])
                    durable_copy(op['source'], op['staging'])
                    self._check_hash(op['staging'], op['source_hash'])
                    if op.get('tag_changes'):
                        from musicops.media.tags import apply_tag_changes
                        apply_tag_changes(op['staging'], op['tag_changes'])
                    op['after_hash'] = sha256_file(op['staging'])
                    if op['kind'] != 'quarantine':
                        if Path(op['staging']).suffix.casefold() in AUDIO_EXTENSIONS:
                            op['validation'] = self.validator.validate(op['staging'])
                            if op['validation']['decode_state'] != 'passed' or not op['validation'].get('roon_compatible'):
                                raise ValidationFailed('音频全解码/兼容验证未通过', details=op['validation'])
                            if Path(op['destination']).suffix.casefold() != Path(op['staging']).suffix.casefold():
                                raise ValidationFailed('发布扩展名与已验证实际格式不一致')
                        else:
                            self._validate_resource(op, tx)
                    tx['staging_manifest'].append({'operation_id': op['operation_id'], 'path': op['staging'], 'hash': op['after_hash'], 'validation': op['validation']})
                    self._write(tx)
                    self._event('work_copy_prepared' if op['kind'] == 'quarantine' else 'work_copy_validated', tx, {'operation_id': op['operation_id'], 'path': op['staging'], 'source_hash': op['source_hash'], 'after_hash': op['after_hash'], 'validation': op['validation']})
                    self._fault(fault, 'after_copy', tx)
                tx['state'] = 'VALIDATED'
                self._write(tx)
                self._event('transaction_validated', tx)
                tx['state'] = 'PUBLISHING'
                self._write(tx)
                self._event('publication_intent', tx)
                if tx.get('directory_publication'):
                    self._publish_directory(tx)
                    self._write(tx)
                    self._event('directory_published', tx, tx['directory_publication'])
                    self._fault(fault, 'after_publish', tx)
                else:
                    for op in tx['operations']:
                        self._publish(op, tx)
                        self._write(tx)
                        self._event('file_published', tx, {'operation_id': op['operation_id'], 'path': op['destination'], 'before_hash': op['before_hash'], 'after_hash': op['after_hash'], 'kind': op['kind']})
                        self._fault(fault, 'after_publish', tx)
                self._verify_published(tx)
                tx['state'] = 'PUBLISHED'
                self._write(tx)
                self._event('publication_verified', tx)
                self._fault(fault, 'before_commit', tx)
                self._commit(tx)
                return tx
            except Exception as exc:
                tx['state'] = 'CONFLICT' if isinstance(exc, Conflict) else 'RECOVERY_REQUIRED'
                tx['error'] = {'error_class': getattr(exc, 'error_class', 'STATE_ERROR'), 'type': type(exc).__name__, 'message': str(exc)[:500]}
                self._write(tx, database=False)
                # Database errors are never swallowed; manifest remains available even if this write fails.
                self.store.put('transactions', tx_id, tx)
                self._event('transaction_recovery_required' if tx['state'] != 'CONFLICT' else 'transaction_conflict', tx, tx['error'])
                raise

    def _validate_resource(self, op, tx):
        path = Path(op['staging'])
        suffix = path.suffix.casefold()
        if suffix == '.lrc':
            from musicops.media.resources import validate_lrc
            result = validate_lrc(path.read_text(encoding='utf-8-sig'), duration=op.get('duration'))
        elif suffix in {'.jpg', '.jpeg', '.png', '.webp'}:
            from musicops.media.resources import validate_cover
            result = validate_cover(path.read_bytes(), min_side=op.get('min_side', 300))
        elif suffix == '.cue':
            import re
            references = re.findall(r'^\s*FILE\s+(?:"([^"]+)"|(\S+))\s+\S+', path.read_text(encoding='utf-8-sig'), re.I | re.M)
            if not references:
                raise ValidationFailed('CUE缺FILE引用')
            for first, second in references:
                referenced = safe_destination(Path(op['destination']).parent, first or second)
                if not referenced.is_file() and str(referenced) not in {item['destination'] for item in tx['operations'] if item['kind'] != 'quarantine'}:
                    raise ValidationFailed('CUE引用不存在且不在同事务发布清单')
            result = {'valid': True, 'file_references': [a or b for a, b in references]}
        else:
            raise ValidationFailed('不支持的发布资源类型')
        valid = result.get('valid', result.get('passed', result.get('state') in {'passed', 'valid'}))
        if not valid:
            raise ValidationFailed('资源结构验证失败', details=result)
        op['validation'] = result

    def _publish(self, op, tx):
        target = Path(op['destination'])
        target_root = self.roots['quarantine'] if op['kind'] == 'quarantine' else self.roots['library']
        safe_destination(target_root, target.relative_to(target_root))
        self._check_hash(op['staging'], op['after_hash'])
        self._check_hash(op['source'], op['source_hash'])
        self._check_hash(target, None if op['kind'] == 'quarantine' else op['before_hash'])
        parent = target.parent
        missing = []
        while not parent.exists() and parent != target_root:
            missing.append(str(parent))
            parent = parent.parent
        tx['created_directories'].extend(p for p in missing if p not in tx['created_directories'])
        self._write(tx)
        target.parent.mkdir(parents=True, exist_ok=True)
        temporary = target.with_name(f'.{target.name}.{tx["tx_id"]}.publish')
        reject_reparse_chain(temporary)
        if temporary.exists():
            raise Conflict('发布临时路径已存在，进入恢复')
        durable_copy(op['staging'], temporary)  # Cross-volume copying is explicit; replace happens only on target volume.
        self._check_hash(temporary, op['after_hash'])
        self._check_hash(target, None if op['kind'] == 'quarantine' else op['before_hash'])
        os.replace(temporary, target)
        op['published'] = True
        if op['kind'] == 'quarantine':
            self._check_hash(op['source'], op['before_hash'])
            Path(op['source']).unlink()  # Only the approved library copy, never source root.
            op['source_removed'] = True
        tx['publish_manifest'].append({'operation_id': op['operation_id'], 'path': str(target), 'before_hash': op['before_hash'], 'after_hash': op['after_hash'], 'kind': op['kind']})

    def _publish_directory(self, tx):
        group = tx['directory_publication']
        staging, destination = Path(group['staging']), Path(group['destination'])
        safe_destination(self.roots['work'], staging.relative_to(self.roots['work']))
        safe_destination(self.roots['library'], destination.relative_to(self.roots['library']))
        if staging.exists() or destination.exists():
            raise Conflict('新发行发布目录已有内容，不能合并或覆盖')
        for op in tx['operations']:
            self._check_hash(op['source'], op['source_hash'])
            self._check_hash(op['staging'], op['after_hash'])
            relative = Path(op['destination']).relative_to(destination)
            staged_target = safe_destination(staging, relative)
            durable_copy(op['staging'], staged_target)
            self._check_hash(staged_target, op['after_hash'])
        missing, current = [], destination.parent
        while not current.exists() and current != self.roots['library']:
            missing.append(str(current)); current = current.parent
        tx['created_directories'].extend(p for p in missing if p not in tx['created_directories'])
        # All new subdirectories disappear on rollback only if still empty.
        tx['created_directories'].append(str(destination))
        for sub in staging.rglob('*'):
            if sub.is_dir():
                tx['created_directories'].append(str(destination / sub.relative_to(staging)))
        self._write(tx)
        destination.parent.mkdir(parents=True, exist_ok=True)
        reject_reparse_chain(destination)
        if destination.exists():
            raise Conflict('新发行目录在发布前被外部创建')
        for op in tx['operations']:
            self._check_hash(op['source'], op['source_hash'])
        os.replace(staging, destination)  # Both paths are on the same volume, outside-to-inside watched-root rename.
        group['renamed'] = True
        for op in tx['operations']:
            op['published'] = True
            tx['publish_manifest'].append({'operation_id':op['operation_id'],'path':op['destination'],'before_hash':None,'after_hash':op['after_hash'],'kind':'publish'})

    def _verify_published(self, tx, *, verify_sources=True):
        for op in tx['operations']:
            self._check_hash(op['destination'], op['after_hash'])
            if op['kind'] == 'quarantine':
                self._check_hash(op['source'], None)
            elif verify_sources:
                self._check_hash(op['source'], op['source_hash'])

    def _commit(self, tx):
        self._verify_published(tx)
        for op in tx['operations']:
            path = op['source'] if op['kind'] == 'quarantine' else op['destination']
            file_id = op.get('file_id') or str(uuid.uuid5(uuid.NAMESPACE_URL, str(path).casefold()))
            old = self.store.get('library_files', file_id) or {}
            row = {**old, 'schema_version': 1, 'file_id': file_id, 'path': path, 'sha256': op['after_hash'], 'tx_id': tx['tx_id'],
                   'location_state': 'quarantine' if op['kind'] == 'quarantine' else 'library', 'validation': op['validation'],
                   'roon_state': 'pending', 'quarantine_path': op['destination'] if op['kind'] == 'quarantine' else None,
                   'quarantine_reason': op.get('quarantine_reason')}
            self._store_audio(file_id, row)
            self.store.put('transaction_files', op['operation_id'], {**op, 'tx_id': tx['tx_id'], 'state': 'COMMITTED'})
            if op['kind'] == 'quarantine':
                self.store.put('suppressions', op['operation_id'], {'schema_version': 1, 'identity': file_id, 'reason': op['quarantine_reason'], 'location_state': 'quarantine', 'tx_id': tx['tx_id'], 'active': True, 'created_at': now()})
        # Final COMMITTED marker is written only after publication and state writes succeed.
        tx['state'] = 'COMMITTED'
        try:
            self.store.put('transactions', tx['tx_id'], tx)
        except Exception:
            tx['state'] = 'PUBLISHED'
            self._write(tx, database=False)
            raise
        self._write(tx, database=False)
        self._event('transaction_committed', tx)

    def _store_audio(self, file_id, row):
        # Companion files remain in transaction history, never in the song index.
        if Path(row.get('path','')).suffix.casefold() in AUDIO_EXTENSIONS:
            self.store.put('library_files', file_id, row)
        else:
            with self.store.connection() as connection:
                connection.execute('DELETE FROM library_files WHERE id=?',(file_id,))

    def _load(self, tx_id):
        path = self._manifest_path(tx_id)
        tx = json.loads(path.read_text(encoding='utf-8'))
        if tx.get('schema_version') != 1 or tx.get('tx_id') != tx_id:
            raise RecoveryRequired('manifest版本或身份无效')
        for op in tx['operations']:
            root = self.roots['quarantine'] if op['kind'] == 'quarantine' else self.roots['library']
            target = Path(op['destination'])
            if not within(target, root):
                raise PathBlocked('manifest目标越界')
            safe_destination(root, target.relative_to(root))
            if not within(op['staging'], self.roots['work'] / tx_id):
                raise PathBlocked('manifest工作路径越界')
            reject_reparse_chain(op['staging'])
            source_roots = ['library'] if op['kind'] == 'quarantine' else ['source', 'work']
            if not any(within(op['source'], self.roots[k]) for k in source_roots):
                raise PathBlocked('manifest来源越界')
            reject_reparse_chain(op['source'])
        for item in tx['backup_manifest']:
            if not within(item['path'], self.roots['library']) or not within(item['backup'], self.roots['backup'] / tx_id):
                raise PathBlocked('manifest备份恢复路径越界')
            reject_reparse_chain(item['path'])
            reject_reparse_chain(item['backup'])
        if tx.get('directory_publication'):
            group = tx['directory_publication']
            if not within(group['staging'], self.roots['work'] / tx_id) or not within(group['destination'], self.roots['library']):
                raise PathBlocked('manifest整目录发布边界无效')
            reject_reparse_chain(group['staging'])
            reject_reparse_chain(group['destination'])
        return tx

    def recover(self):
        results = []
        with self._lock():
            root = self.roots['backup']
            if not root.exists():
                return results
            for path in sorted(root.glob('*/manifest.json')):
                tx = self._load(path.parent.name)
                if tx['state'] in TERMINAL:
                    continue
                try:
                    # Adopt only when all after-hashes exist. Unknown/mixed publication rolls back from originals.
                    all_published = all(op.get('after_hash') and Path(op['destination']).is_file() and sha256_file(op['destination']) == op['after_hash'] for op in tx['operations'])
                    if all_published and tx['state'] != 'ROLLING_BACK':
                        for op in tx['operations']:
                            op['published'] = True
                            if op['kind'] == 'quarantine' and Path(op['source']).exists():
                                self._check_hash(op['source'], op['before_hash'])
                                Path(op['source']).unlink()
                                op['source_removed'] = True
                        tx['state'] = 'PUBLISHED'
                        self._write(tx)
                        self._commit(tx)
                    else:
                        self._rollback(tx)
                    results.append({'schema_version': 1, 'tx_id': tx['tx_id'], 'state': tx['state']})
                except Conflict as exc:
                    tx.update(state='CONFLICT', error={'error_class': 'CONFLICT', 'message': str(exc)})
                    self._write(tx)
                    results.append({'schema_version': 1, 'tx_id': tx['tx_id'], 'state': 'CONFLICT'})
        return results

    def rollback(self, tx_id):
        with self._lock():
            tx = self._load(tx_id)
            if tx['state'] == 'ROLLED_BACK':
                return {**tx, 'idempotent': True}
            try:
                self._rollback(tx)
            except Conflict as exc:
                tx.update(state='CONFLICT', error={'error_class': 'CONFLICT', 'message': str(exc)})
                self._write(tx)
                raise
            return tx

    def _rollback(self, tx):
        # Preflight every path before restoring any byte, including unchanged companions.
        for op in tx['operations']:
            target = Path(op['destination'])
            if target.exists():
                current = sha256_file(target)
                allowed = {op.get('after_hash')}
                if op['kind'] != 'quarantine':
                    allowed.add(op.get('before_hash'))
                if current not in allowed:
                    raise Conflict('回滚目标已被外部修改')
            elif op.get('published') and op['kind'] != 'quarantine' and not op.get('rollback_started'):
                raise Conflict('已发布目标被外部删除')
            if op['kind'] == 'quarantine' and Path(op['source']).exists():
                self._check_hash(op['source'], op['before_hash'])
            # Rollback only changes library/quarantine paths and uses retained
            # originals. Cleanup or changes to a seeding/work source must not
            # prevent restoring the approved library bytes.
        changed_paths = {op['source'] if op['kind'] == 'quarantine' else op['destination'] for op in tx['operations']}
        for item in tx['backup_manifest']:
            if item['backed_up']:
                self._check_hash(item['backup'], item['before_hash'])
            if item['path'] not in changed_paths:
                self._check_hash(item['path'], item['before_hash'])
        tx['state'] = 'ROLLING_BACK'
        self._write(tx)
        self._event('rollback_intent', tx)
        for op in reversed(tx['operations']):
            op['rollback_started'] = True
            self._write(tx)
            target = Path(op['destination'])
            if op['kind'] == 'quarantine':
                original = Path(op['source'])
                item = next((b for b in tx['backup_manifest'] if b['path'] == str(original)), None)
                if not original.exists():
                    if not item or not item['backed_up']:
                        raise RecoveryRequired('隔离恢复缺完整原件')
                    temp = original.with_name(f'.{original.name}.{tx["tx_id"]}.restore')
                    if temp.exists():
                        self._check_hash(temp, item['before_hash'])
                        temp.unlink()
                    durable_copy(item['backup'], temp)
                    os.replace(temp, original)
                if target.exists():
                    self._check_hash(target, op['after_hash'])
                    target.unlink()
            elif op['before_hash'] is None:
                if target.exists():
                    self._check_hash(target, op['after_hash'])
                    target.unlink()
            else:
                item = next(b for b in tx['backup_manifest'] if b['path'] == str(target))
                if target.exists() and sha256_file(target) == op['before_hash']:
                    continue
                if not item['backed_up']:
                    raise RecoveryRequired('替换恢复缺完整原件')
                temp = target.with_name(f'.{target.name}.{tx["tx_id"]}.restore')
                if temp.exists():
                    self._check_hash(temp, item['before_hash'])
                    temp.unlink()
                durable_copy(item['backup'], temp)
                os.replace(temp, target)
                self._check_hash(target, op['before_hash'])
            op['rolled_back'] = True
            self._write(tx)
            self._event('file_rollback_verified', tx, {'operation_id': op['operation_id'], 'before_path': op['source'] if op['kind'] == 'quarantine' else op['destination'], 'restored_hash': op['before_hash']})
        old_ids = {item['file_id'] for item in tx['state_before'] if item.get('file_id')}
        for item in tx['state_before']:
            self._store_audio(item.get('file_id') or item['id'], item)
        for op in tx['operations']:
            file_path = op['source'] if op['kind'] == 'quarantine' else op['destination']
            file_id = op.get('file_id') or str(uuid.uuid5(uuid.NAMESPACE_URL, str(file_path).casefold()))
            if file_id not in old_ids:
                # A tombstone is explicit; never misrepresent a deleted new publication as healthy.
                self._store_audio(file_id, {'schema_version': 1, 'file_id': file_id, 'path': file_path, 'location_state': 'library' if op['before_hash'] else 'rolled_back', 'sha256': op['before_hash'], 'read_error': 'RESTORED_REINDEX_REQUIRED' if op['before_hash'] else None, 'tx_id': tx['tx_id'], 'roon_state': 'pending'})
            self.store.put('transaction_files', op['operation_id'], {**op, 'tx_id': tx['tx_id'], 'state': 'ROLLED_BACK'})
            if op['kind'] == 'quarantine':
                self.store.put('suppressions', op['operation_id'], {'schema_version': 1, 'identity': file_id, 'reason': op['quarantine_reason'], 'tx_id': tx['tx_id'], 'active': False, 'rolled_back_at': now()})
        # App may have reindexed/reconfirmed coverage since commit. Withdraw affected coverage after restoring files.
        from musicops.domain.identity import match_track
        affected_ids = {op.get('file_id') or str(uuid.uuid5(uuid.NAMESPACE_URL, str(op['source'] if op['kind'] == 'quarantine' else op['destination']).casefold())) for op in tx['operations']}
        affected_targets = {op['target_id'] for op in tx['operations'] if op.get('target_id')}
        for decision in self.store.list('match_decisions'):
            if affected_ids.intersection(decision.get('candidate_file_ids', [])):
                affected_targets.add(decision.get('target_id', decision.get('id')))
        if affected_targets:
            files = self.store.list('library_files')
            for target in self.store.list('target_tracks'):
                if target.get('target_id') in affected_targets:
                    decision = match_track(target, files)
                    decision['rollback_tx_id'] = tx['tx_id']
                    self.store.put('match_decisions', target['target_id'], decision)
        for directory in sorted(set(tx.get('created_directories', [])), key=len, reverse=True):
            path = Path(directory)
            if any(within(path, self.roots[k]) for k in ['library', 'quarantine']):
                reject_reparse_chain(path)
                if path.is_dir() and not any(path.iterdir()):
                    path.rmdir()
        tx.update(state='ROLLED_BACK', rolled_back_at=now(), roon_state='pending')
        self._write(tx)
        self._event('transaction_rolled_back', tx)
