"""Version-aware stopped add, persisted ownership and safe download recovery.

Only recorded, hash/path-bound torrents can be relocated after publication.
No stop/delete or global preference operation is exposed by this adapter.
"""
from __future__ import annotations

from pathlib import Path
import re
from urllib.parse import urlsplit

from musicops.clients.metainfo import parse_metainfo

def readonly_preflight(config,secret_getter):
    """Log in and read versions/paths/counts; expose no torrent mutation endpoint."""
    import httpx,json,collections
    try:
        u=urlsplit(config['base_url'])
        u.port
    except (ValueError,TypeError,KeyError):return _result('POLICY_BLOCKED',reason='qBittorrent服务地址格式无效')
    if u.scheme not in ('http','https') or not u.hostname or u.username or u.password or u.query or u.fragment or u.path not in ('','/'):
        return _result('POLICY_BLOCKED',reason='qBittorrent需干净服务origin')
    if u.scheme=='http' and u.hostname not in ('127.0.0.1','localhost','::1'):
        return _result('POLICY_BLOCKED',reason='明文qBittorrent凭据仅允许本机回环地址')
    try:
        raw=secret_getter(config['credential_ref'],u.hostname)
        if not raw:return _result('AUTH_REQUIRED',reason='qBittorrent登录信息未配置')
        creds=json.loads(raw)
        if not isinstance(creds,dict) or not isinstance(creds.get('username'),str) or not isinstance(creds.get('password'),str):return _result('AUTH_FAILED',reason='qBittorrent凭据格式无效')
        base=config['base_url'].rstrip('/')+'/api/v2/'
        with httpx.Client(follow_redirects=False,verify=True,trust_env=False,timeout=15) as client:
            login=client.post(base+'auth/login',data={k:creds[k] for k in ('username','password')},headers={'Referer':config['base_url']})
            if not (login.status_code==204 or (login.status_code==200 and login.text.strip()=='Ok.')):return _result('AUTH_FAILED',reason='qBittorrent登录失败')
            responses=[client.get(base+endpoint) for endpoint in ('app/version','app/webapiVersion','app/preferences','torrents/info')]
        if any(r.status_code!=200 for r in responses):return _result('ACCESS_BLOCKED',reason='qBittorrent只读API请求未通过')
        prefs=responses[2].json();rows=responses[3].json()
        versions=[r.text.strip() for r in responses[:2]]
        if (not all(re.fullmatch(r'v?\d+\.\d+(?:\.\d+)?',v) for v in versions)
                or not isinstance(prefs,dict) or not isinstance(rows,list)
                or not all(isinstance(row,dict) and isinstance(row.get('state','unknown'),str) for row in rows)):
            raise ValueError('readonly response schema invalid')
        return _result('OK',version=versions[0],webapi_version=versions[1],
                       paths={k:prefs.get(k) for k in ('save_path','temp_path','temp_path_enabled','auto_tmm_enabled')},
                       torrent_count=len(rows),state_counts=dict(collections.Counter(r.get('state','unknown') for r in rows)),
                       read_only=True,client_changes=0)
    except httpx.HTTPError:return _result('NETWORK_ERROR',reason='qBittorrent网络请求失败')
    except (ValueError,TypeError,KeyError):return _result('PARSE_ERROR',reason='qBittorrent凭据或响应结构无效')


def _result(status, **data):
    return {"schema_version": 1, "status": status, **data}


PROJECT_TAG = "MusicOps"


class QbtClient:
    def __init__(self, transport, base_url, source_root, version="5.2.2", store=None, category="musicops", tag_prefix="musicops-", auto_tmm=False):
        parts = urlsplit(base_url)
        if parts.scheme not in ("http", "https") or not parts.hostname or parts.username or parts.password or parts.query or parts.fragment or parts.path not in ('','/'):
            raise ValueError("invalid qBittorrent base")
        self.transport, self.base_url = transport, base_url.rstrip("/")
        self.source_root, self.version, self.store = Path(source_root).resolve(), version, store
        if not isinstance(category, str) or not category or re.search(r"[\r\n]", category):
            raise ValueError("invalid project category")
        if not isinstance(tag_prefix, str) or not tag_prefix or re.search(r"[,\r\n]", tag_prefix):
            raise ValueError("invalid project tag prefix")
        self.category, self.tag_prefix, self.auto_tmm = category, tag_prefix, auto_tmm
        self._memory_records = {}
        match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)", version)
        self.version_tuple = tuple(map(int, match.groups())) if match else None

    def capabilities(self):
        supported = self.version_tuple is not None and ((4, 6, 0) <= self.version_tuple < (5, 0, 0) or (5, 0, 0) <= self.version_tuple <= (5, 2, 2))
        fake = getattr(self.transport, "is_fake", False)
        verified = getattr(self.transport, "authenticated_qbt", False)
        return _result("OK" if (fake or verified) and supported and self.auto_tmm is False else "POLICY_BLOCKED", production_verified=verified,
                       initial_stop_parameter="stopped" if self.version_tuple and self.version_tuple >= (5, 0, 0) else "paused",
                       reason="实际应用/WebAPI版本、自动管理与路径待授权预检")

    def _put(self, task_id, record):
        if self.store:
            self.store.put("download_tasks", "qbt:" + task_id, record)
        else:
            self._memory_records[task_id] = dict(record)

    def _get(self, task_id):
        return self.store.get("download_tasks", "qbt:" + task_id) if self.store else self._memory_records.get(task_id)

    def _request(self, method, endpoint, **kwargs):
        return self.transport.request(method, self.base_url + "/api/v2/" + endpoint, timeout=12, **kwargs)

    def _query(self, record):
        response = self._request("GET", "torrents/info", params={"hashes": record["client_id"]})
        if response.status_code != 200:
            raise ValueError("client query failed")
        rows = response.json()
        if not isinstance(rows, list) or not all(isinstance(row,dict) for row in rows):
            raise ValueError("client info schema invalid")
        hashes = {x for x in (record.get("infohash_v1"), record.get("infohash_v2"), record["client_id"]) if x}
        matching = [r for r in rows if hashes.intersection({r.get("hash"), r.get("infohash_v1"), r.get("infohash_v2")})]
        if len(matching)!=len(rows):raise ValueError('unexpected client identity')
        rows=matching
        if len(rows) > 1:
            raise ValueError("ambiguous client identity")
        return rows[0] if rows else None

    def _bound_path(self, path, expected, exact=False):
        if not path or not Path(path).is_absolute():
            return False
        actual = Path(path).resolve()
        if exact:
            return actual == expected
        return actual == expected or actual.is_relative_to(expected)

    def _check_ownership(self, row, record, require_stopped=False):
        tags = {x.strip() for x in str(row.get("tags", "")).split(",")}
        if record.get("creation_intent") is not True or not tags.intersection({record["task_tag"], PROJECT_TAG}) or row.get("category") != record.get("category", self.category):
            return "没有本项目创建记录/任务标签/分类，禁止接管"
        expected = Path(record["source_path"]).resolve()
        if not self._bound_path(row.get("save_path"), expected, exact=True):
            return "实际保存目录不等于任务来源目录"
        if row.get("auto_tmm") is not False:
            return "自动管理状态未知或开启"
        if row.get("content_path") and not self._bound_path(row["content_path"], expected):
            return "内容路径越过任务来源目录"
        download_path = row.get("download_path")
        if download_path and not self._bound_path(download_path, expected):
            return "全局临时路径越过任务来源目录"
        if not download_path:
            return "临时路径未返回，必须现场确认"
        stopped = {"stoppedDL", "stoppedUP"} if self.version_tuple >= (5, 0, 0) else {"pausedDL", "pausedUP"}
        if require_stopped and row.get("state") not in stopped:
            return "新增任务未确认处于停止状态"
        return None

    def normalize_tags(self, task_id, record=None, row=None):
        """Rename only an authenticated, hash/path-bound task's generated tag."""
        record = record or self._get(task_id)
        if not record:return _result('POLICY_BLOCKED', changed=False)
        try:
            row = row or self._query(record)
            if self._check_ownership(row or {}, record):return _result('POLICY_BLOCKED', changed=False)
            old = record.get('legacy_task_tag') or record['task_tag']
            if record['task_tag'] != PROJECT_TAG:
                response = self._request('POST', 'torrents/addTags', data={'hashes':record['client_id'],'tags':PROJECT_TAG})
                if response.status_code != 200:return _result('WAITING', changed=False)
                row = self._query(record)
                proposed = {**record, 'task_tag':PROJECT_TAG}
                if self._check_ownership(row or {}, proposed):return _result('WAITING', changed=False)
                record.update(task_tag=PROJECT_TAG, legacy_task_tag=old)
                self._put(task_id, record)
            if old != PROJECT_TAG:
                if old in {t.strip() for t in str(row.get('tags','')).split(',')}:
                    response = self._request('POST','torrents/removeTags',data={'hashes':record['client_id'],'tags':old})
                    if response.status_code != 200:return _result('WAITING', changed=True)
                # A label is deleted globally only after it is confirmed unused.
                response = self._request('GET','torrents/info',params={'tag':old})
                rows = response.json() if response.status_code == 200 else None
                if not isinstance(rows,list):return _result('WAITING', changed=True)
                if not rows:
                    response = self._request('POST','torrents/deleteTags',data={'tags':old})
                    if response.status_code != 200:return _result('WAITING', changed=True)
                record.pop('legacy_task_tag',None);self._put(task_id,record)
            return _result('OK',changed=old!=PROJECT_TAG)
        except Exception:
            # Tag maintenance never retries an add/start or changes seeding.
            return _result('WAITING',changed=False)

    def _free_matches(self,evidence, meta,task_id=None):
        hashes = {x for x in (meta.get("infohash_v1"), meta.get("infohash_v2")) if x}
        if not (isinstance(evidence, dict) and evidence.get("confirmed") is True and evidence.get("infohash") in hashes):return False
        if evidence.get('free') is not False:return True
        if not self.store or not task_id or evidence.get('source') not in ('dicmusic','orpheus'):return False
        consent=self.store.get('evidence','nonfree-consent:'+str(evidence.get('consent_id'))) or {}
        job=self.store.get('evidence','workflow:'+task_id) or {}
        return consent.get('approved_by')=='user' and consent.get('batch_id')==job.get('batch_id') and bool(job.get('batch_id')) and (consent.get('scope')=='batch' or consent.get('scope')=='once' and consent.get('job_id')==task_id and consent.get('candidate_id')==evidence.get('source')+':'+str(evidence.get('resource_id')))

    def add_stopped(self, torrent_bytes, task_id, free_evidence):
        if self.capabilities()["status"] != "OK":
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        if not isinstance(task_id, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,120}", task_id):
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="任务ID不适合作为Windows目录")
        try:
            meta = parse_metainfo(torrent_bytes)
        except ValueError:
            return _result("PARSE_ERROR", error_class="PARSE_ERROR")
        if not self._free_matches(free_evidence, meta,task_id):
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="缺少与当前种子绑定的免费证据")
        existing_record = self._get(task_id)
        if existing_record:
            if existing_record.get("metainfo_sha256") != meta["metainfo_sha256"]:
                return _result("CONFLICT", error_class="CONFLICT", reason="任务ID对应另一资源")
            return self.reconcile_add(task_id)
        source = (self.source_root / task_id).resolve()
        if not source.is_relative_to(self.source_root):
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="任务目录越界")
        record = {"schema_version": 1, "task_id": task_id, "client_id": meta["infohash_v1"] or meta["infohash_v2"][:40],
                  "infohash_v1": meta["infohash_v1"], "infohash_v2": meta["infohash_v2"],
                  "source_path": str(source), "task_tag": PROJECT_TAG, "category": self.category,
                  "metainfo_sha256": meta["metainfo_sha256"], "metainfo": meta, "creation_intent": False,
                  "free_evidence": free_evidence, "state": "PRECHECK"}
        try:
            if self._query(record):
                return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="客户端已有该资源；无本项目创建记录，不接管")
        except Exception:
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="无法确认客户端已有资源")
        record.update(creation_intent=True, state="ADD_INTENT")
        self._put(task_id, record)
        params = {"savepath": str(source), "category": self.category, "tags": record["task_tag"],
                  "autoTMM": "false", "skip_checking": "false", "contentLayout": "Original",
                  self.capabilities()["initial_stop_parameter"]: "true",
                  # Per-task unlimited seed thresholds; no local stop rule.
                  "ratioLimit": "-1", "seedingTimeLimit": "-1", "inactiveSeedingTimeLimit": "-1"}
        params.update(downloadPath=str(source / ".incomplete"), useDownloadPath="true")
        try:
            response = self._request("POST", "torrents/add", data=params,
                                     files={"torrents": ("asset.torrent", torrent_bytes, "application/x-bittorrent")})
            record["add_http_status"] = response.status_code
            if response.status_code not in (200, 202):
                record["state"] = "ADD_UNKNOWN"
            else:
                # Text Ok. and versioned JSON/202 both require actual info query.
                record["state"] = "ADD_UNKNOWN"
        except Exception:
            record["state"] = "ADD_UNKNOWN"
        self._put(task_id, record)
        return self.reconcile_add(task_id)

    def reconcile_add(self, task_id):
        if self.capabilities()["status"] != "OK":
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        record = self._get(task_id)
        if not record:
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="没有创建意图")
        try:
            row = self._query(record)
            if not row:
                return _result("ADD_UNKNOWN", error_class="POLICY_BLOCKED", record=record, reason="异步加种未确认；保留意图，禁止再次加种")
            problem = self._check_ownership(row, record, require_stopped=record["state"] not in ("START_REQUESTED", "DOWNLOADING", "COMPLETE"))
            if problem:
                return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason=problem, record=record)
            if record["task_tag"] != PROJECT_TAG or record.get("legacy_task_tag"):self.normalize_tags(task_id,record,row)
            record["state"] = "STOPPED" if record["state"] not in ("START_REQUESTED", "DOWNLOADING", "COMPLETE") else record["state"]
            self._put(task_id, record)
            return _result(record["state"], record=record, client_state=row.get("state"))
        except Exception:
            return _result("ADD_UNKNOWN", error_class="POLICY_BLOCKED", record=record, reason="客户端查询失败；禁止重加")

    def start_download(self, task_id, free_evidence):
        if self.capabilities()["status"] != "OK":
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        record = self._get(task_id)
        if not record or record["state"] != "STOPPED" or not self._free_matches(free_evidence, record["metainfo"],task_id):
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="归属、停止状态或免费证据未确证")
        try:
            row = self._query(record)
            problem = self._check_ownership(row or {}, record, require_stopped=True)
            if problem:
                return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason=problem)
            record["state"] = "START_REQUESTED"
            self._put(task_id, record)
            endpoint = "torrents/start" if self.version_tuple >= (5, 0, 0) else "torrents/resume"
            response = self._request("POST", endpoint, data={"hashes": record["client_id"]})
            return _result("START_REQUESTED" if response.status_code == 200 else "START_UNKNOWN", record=record)
        except Exception:
            return _result("START_UNKNOWN", error_class="POLICY_BLOCKED", record=record)

    def poll(self, task_id, expected_files=None):
        if self.capabilities()["status"] != "OK":
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        record = self._get(task_id)
        if not record:
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        try:
            row = self._query(record)
            problem = self._check_ownership(row or {}, record)
            if problem:
                return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason=problem)
            if record["task_tag"] != PROJECT_TAG or record.get("legacy_task_tag"):self.normalize_tags(task_id,record,row)
            state = row.get("state")
            if state in {"checkingUP", "checkingDL", "checkingResumeData", "moving", "error", "missingFiles", "unknown"}:
                return _result("WAITING" if state.startswith("checking") or state == "moving" else "BLOCKED", client_state=state, complete=False)
            finished_states = {"uploading", "stalledUP", "queuedUP", "forcedUP", "stoppedUP", "pausedUP"}
            if state not in finished_states or row.get("progress") != 1:
                return _result("DOWNLOADING", client_state=state, complete=False)
            response = self._request("GET", "torrents/files", params={"hash": record["client_id"]})
            files = response.json() if response.status_code == 200 else None
            if not isinstance(files, list):
                raise ValueError("file manifest unavailable")
            canonical = [x["path"] for x in record["metainfo"]["file_list"] if not x["padding"]]
            expected = expected_files if expected_files is not None else canonical
            if not set(canonical).issubset(set(expected)):
                return _result("PARTIAL_DOWNLOAD", complete=False, reason="预期清单不能缩成歌单子集，必须覆盖整发行")
            actual = {x.get("name"): x for x in files if isinstance(x, dict)}
            if len(actual) != len(files) or not expected or any(path not in actual or actual[path].get("priority", 0) == 0 or actual[path].get("progress") != 1 for path in expected):
                return _result("PARTIAL_DOWNLOAD", complete=False, reason="预期整发行曲目缺失/跳过/未完成")
            record["state"] = "COMPLETE"
            self._put(task_id, record)
            return _result("REQUIRES_MEDIA_VALIDATION", complete=True, source_path=record["source_path"], client_state=state,
                           validated=False, seeding="preserved")
        except Exception:
            return _result("BLOCKED", error_class="PARSE_ERROR", complete=False)

    def pipeline_event(self, task_id, event):
        """Cancellation/rollback/local ratio limits have no client side effects."""
        return _result("OK", task_id=task_id, event=event, seeding="preserved", client_operations=[])

    def archive_completed(self,task_id,library):
        """Move originals through the client, with persisted intent and byte verification."""
        from musicops.library.storage import seed_root
        from musicops.library.paths import reject_reparse_chain,safe_destination
        from musicops.library.index import sha256_file
        record=self._get(task_id)
        if not record or self.capabilities()['status']!='OK':return _result('POLICY_BLOCKED',reason='缺少客户端创建记录')
        if not re.fullmatch(r'[A-Za-z0-9_-]{1,120}',task_id):return _result('POLICY_BLOCKED',reason='任务目录无效')
        destination=seed_root(library)/task_id;reject_reparse_chain(destination)
        intent=record.get('relocation')
        row=self._query(record)
        if not row:return _result('POLICY_BLOCKED',reason='客户端任务不存在')
        if not intent:
            problem=self._check_ownership(row,record)
            if problem:return _result('POLICY_BLOCKED',reason=problem)
            finished={'uploading','stalledUP','queuedUP','forcedUP','stoppedUP','pausedUP'}
            if row.get('progress')!=1 or row.get('state') not in finished:return _result('WAITING',reason='等待完整下载后迁移')
            old=Path(record['source_path']).resolve()
            if old==destination.resolve():return _result('OK',source_path=str(old))
            if old.is_relative_to(Path(library).resolve()):return _result('POLICY_BLOCKED',reason='不能自动迁移音乐库内的做种文件')
            response=self._request('GET','torrents/files',params={'hash':record['client_id']})
            files=response.json() if response.status_code==200 else None
            expected=[x for x in record['metainfo']['file_list'] if not x['padding']]
            if not isinstance(files,list) or len({x.get('name') for x in files})!=len(files):return _result('POLICY_BLOCKED',reason='客户端文件清单不完整')
            actual={x.get('name'):x for x in files};hashes={}
            for item in expected:
                name=item['path'];entry=actual.get(name,{})
                if entry.get('progress')!=1 or entry.get('priority',0)==0:return _result('POLICY_BLOCKED',reason='发行文件未完整下载')
                path=safe_destination(old,name,max_length=32760)
                if not path.is_file():return _result('POLICY_BLOCKED',reason='原始做种文件缺失')
                target=safe_destination(destination,name,max_length=32760)
                if target.exists():return _result('POLICY_BLOCKED',reason='做种目标已存在，不能覆盖')
                hashes[name]=sha256_file(path)
            intent={'from':str(old),'to':str(destination),'hashes':hashes,'state':'INTENT'}
            record['relocation']=intent;self._put(task_id,record)
        if Path(intent['to']).resolve()!=destination.resolve():return _result('POLICY_BLOCKED',reason='迁移目标配置已改变，请核对原迁移')
        # Verify identity/category/tag even while the save path is in transition.
        allowed={str(Path(intent[k]).resolve()).casefold() for k in ('from','to')}
        if (not record.get('creation_intent') or row.get('category')!=record['category'] or row.get('auto_tmm') is not False
            or not {record['task_tag'],PROJECT_TAG}.intersection(x.strip() for x in row.get('tags','').split(','))
            or str(Path(row.get('save_path','')).resolve()).casefold() not in allowed):
            return _result('POLICY_BLOCKED',reason='迁移中客户端归属发生改变')
        if row.get('state')=='moving':return _result('WAITING',reason='qBittorrent 正在迁移原始文件')
        if any(row.get(key) and not any(self._bound_path(row[key],Path(intent[k])) for k in ('from','to')) for key in ('content_path','download_path')):
            return _result('POLICY_BLOCKED',reason='迁移中客户端内容或临时路径越界')
        if Path(row['save_path']).resolve()!=destination.resolve():
            response=self._request('POST','torrents/setLocation',data={'hashes':record['client_id'],'location':str(destination)})
            if response.status_code!=200:return _result('WAITING',reason='客户端未接受做种目录迁移')
            intent['state']='MOVING';self._put(task_id,record)
            return _result('WAITING',reason='已请求客户端迁移，等待实际文件确认')
        if not self._bound_path(row.get('download_path'),destination):
            response=self._request('POST','torrents/setDownloadPath',data={'id':record['client_id'],'path':str(destination/'.incomplete')})
            if response.status_code!=200:return _result('WAITING',reason='客户端临时路径尚未同步')
            return _result('WAITING',reason='正在确认客户端临时路径')
        if row.get('progress')!=1 or row.get('state') in {'error','missingFiles','checkingUP','checkingDL','checkingResumeData'}:return _result('WAITING',reason='等待客户端确认完整做种状态')
        for name,expected in intent['hashes'].items():
            target=safe_destination(destination,name,max_length=32760)
            if not target.is_file() or sha256_file(target)!=expected:return _result('POLICY_BLOCKED',reason='迁移后原始文件字节校验未通过')
            old=safe_destination(intent['from'],name,max_length=32760)
            if old.exists():return _result('POLICY_BLOCKED',reason='客户端迁移后旧目录仍有文件，请核对')
        record['source_path']=str(destination);intent['state']='VERIFIED';self._put(task_id,record)
        return _result('OK',source_path=str(destination),verified_files=len(intent['hashes']),seeding='preserved')

    def relocate_active(self,task_id,library):
        """Repair an unfinished download through qBittorrent, without add/start."""
        from musicops.library.storage import storage_paths
        from musicops.library.paths import reject_reparse_chain
        record=self._get(task_id)
        if not record or not re.fullmatch(r'[A-Za-z0-9_-]{1,120}',task_id) or self.capabilities()['status']!='OK':return _result('POLICY_BLOCKED')
        destination=Path(storage_paths(library)['source'])/task_id;reject_reparse_chain(destination)
        row=self._query(record)
        if not row:return _result('POLICY_BLOCKED')
        intent=record.get('active_relocation')
        if not intent:
            problem=self._check_ownership(row,record)
            if problem:return _result('POLICY_BLOCKED',reason=problem)
            if Path(record['source_path']).resolve()==destination.resolve():return _result('OK',source_path=str(destination))
            intent={'from':record['source_path'],'to':str(destination)};record['active_relocation']=intent;self._put(task_id,record)
        if Path(intent['to']).resolve()!=destination.resolve():return _result('POLICY_BLOCKED',reason='迁移配置发生改变')
        tags={x.strip() for x in row.get('tags','').split(',')}
        if (not record.get('creation_intent') or not tags.intersection({PROJECT_TAG,record['task_tag']}) or row.get('category')!=record['category'] or row.get('auto_tmm') is not False
            or any(row.get(key) and not any(self._bound_path(row[key],Path(intent[k])) for k in ('from','to')) for key in ('save_path','download_path','content_path'))):return _result('POLICY_BLOCKED',reason='下载迁移归属发生改变')
        if row.get('state')=='moving':return _result('WAITING',reason='正在迁移未完成文件')
        if not self._bound_path(row.get('download_path'),destination):
            r=self._request('POST','torrents/setDownloadPath',data={'id':record['client_id'],'path':str(destination/'.incomplete')})
            return _result('WAITING' if r.status_code==200 else 'POLICY_BLOCKED',reason='正在更新临时下载位置')
        if Path(row['save_path']).resolve()!=destination.resolve():
            r=self._request('POST','torrents/setLocation',data={'hashes':record['client_id'],'location':str(destination)})
            return _result('WAITING' if r.status_code==200 else 'POLICY_BLOCKED',reason='正在更新最终下载位置')
        if row.get('state') in ('error','missingFiles','unknown'):return _result('POLICY_BLOCKED',reason='客户端报告文件异常')
        if Path(intent['from']).is_dir():
            for path in Path(intent['from']).rglob('*'):
                reject_reparse_chain(path)
                if path.is_file():return _result('WAITING',reason='等待客户端移走全部旧下载文件')
        record['source_path']=str(destination);intent['state']='VERIFIED';self._put(task_id,record)
        return _result('OK',source_path=str(destination))
