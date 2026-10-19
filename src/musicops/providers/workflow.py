"""Complete source/download workflow for the offline GUI, using real adapters.

This module never contacts a real service. Each response is synthetic and marked
as such; the normal policy, metainfo, ownership and token rules still execute.
"""
from __future__ import annotations

import hashlib
import json
import math
from pathlib import Path

from musicops.clients.http import FakeTransport, Response
from musicops.clients.metainfo import bencode, parse_metainfo
from musicops.clients.qbt import QbtClient
from musicops.providers.netease import NetEaseProvider
from musicops.providers.pt import PTProvider, TokenCoordinator
from musicops.state.store import utcnow


def offline_search(store, config, query=""):
    values = config.get("values", config)
    root = Path(values["paths"]["source"]).resolve()
    fixture = next(iter(root.glob("02*.flac")), None)
    if not fixture:
        return {"schema_version": 1, "status": "POLICY_BLOCKED", "error_class": "POLICY_BLOCKED", "candidates": [], "reason": "请先生成合成演示数据"}
    candidates, outcomes = [], {}
    for site in ("dicmusic", "open.cd", "orpheus", "netease"):
        if values.get("providers", {}).get(site, {}).get("enabled") is False:
            outcomes[site] = "DISABLED"
            continue
        raw = {"id": "2", "title": "星河", "artists": ["示例艺人", "合作艺人"],
               "release": {"release_id": "demo-album", "album": "离线演示专辑", "edition": "original", "complete_tracklist": True},
               "version": "original", "seeders": 1, "estimated_cost": 1 if site in ("dicmusic", "orpheus") else 0,
               "eligibility": {"state": "ELIGIBLE", "traffic_policy": "FREE_CONFIRMED"},
               "media": {"declaration": "synthetic FLAC; verify actual parameters"},
               "file_list": [{"path": fixture.name, "length": fixture.stat().st_size}],
               "evidence": [{"kind": "synthetic_fixture", "production_verified": False}]}
        body = {"songs": [raw]} if site == "netease" else {"contract": "musicops.fixture.v1", "results": [raw]}
        transport = FakeTransport([Response(200, json.dumps(body, ensure_ascii=False).encode(), {"Content-Type": "application/json"})])
        provider = NetEaseProvider(transport) if site == "netease" else PTProvider(site, transport, "https://" + site.replace(".", "-") + ".invalid")
        outcome = provider.search(query)
        outcomes[site] = outcome["status"]
        for candidate in outcome.get("candidates", []):
            candidate.update(source_path=str(fixture), fixture_sha256=hashlib.sha256(fixture.read_bytes()).hexdigest(),
                             destination="示例艺人/离线演示专辑/02 星河.flac", album="离线演示专辑", artist="示例艺人",
                             version="original", recording_version="original", duration=2, evidence_level="synthetic_only")
            store.put("candidates", candidate["candidate_id"], candidate)
            candidates.append(candidate)
    return {"schema_version": 1, "status": "OK", "candidates": candidates, "query": query,
            "source_outcomes": outcomes, "selected": None,
            "production_state": "POLICY_BLOCKED:真实来源契约、免费证据与权限待预检"}


class _QbtFixture:
    is_fake = True
    def __init__(self, task_id, meta, root, existing_record=None, category="musicops", tag_prefix="musicops-"):
        self.requests = []
        self.task_id, self.meta, self.root = task_id, meta, root
        self.category, self.tag_prefix = category, tag_prefix
        self.existing_tag = (existing_record or {}).get("task_tag")
        self.added = bool(existing_record)
        self.started = bool(existing_record and existing_record.get("state") in ("START_REQUESTED", "DOWNLOADING", "COMPLETE"))

    def request(self, method, url, **kwargs):
        self.requests.append({"method": method, "endpoint": url.rsplit("/", 2)[-2:], "params": kwargs.get("params", {}), "data": kwargs.get("data", {})})
        if url.endswith("torrents/add"):
            self.added = True
            return Response(202, json.dumps({"success_count": 0, "pending_count": 1, "added_torrent_ids": []}).encode())
        if url.endswith("torrents/start"):
            self.started = True
            return Response(200, b"")
        if url.endswith("torrents/info"):
            rows = []
            if self.added:
                rows = [{"hash": self.meta["infohash_v1"], "infohash_v1": self.meta["infohash_v1"], "state": "uploading" if self.started else "stoppedDL",
                         "tags": (self.existing_tag or "MusicOps"), "category": self.category, "auto_tmm": False,
                         "save_path": str(self.root / self.task_id), "download_path": str(self.root / self.task_id / ".incomplete"),
                         "content_path": str(self.root / self.task_id / self.meta["name"]), "progress": 1 if self.started else 0}]
            return Response(200, json.dumps(rows).encode())
        if url.endswith("torrents/files"):
            return Response(200, json.dumps([{"name": f["path"], "size": f["length"], "priority": 1, "progress": 1} for f in self.meta["file_list"]]).encode())
        raise ValueError("unrecognized fixture operation")


def run_offline_download(store, config, candidate, checkpoint=None):
    def safe_point(phase):
        if checkpoint is not None:
            checkpoint(phase)

    values = config.get("values", config)
    if values.get("offline") is not True or candidate.get("evidence_level") != "synthetic_only":
        return {"schema_version": 1, "status": "POLICY_BLOCKED", "error_class": "POLICY_BLOCKED", "reason": "仅合成演示候选可进入离线下载"}
    source_root = Path(values["paths"]["source"]).resolve()
    source = Path(candidate["source_path"]).resolve()
    if not source.is_relative_to(source_root) or source.is_symlink() or not source.is_file():
        return {"schema_version": 1, "status": "POLICY_BLOCKED", "error_class": "POLICY_BLOCKED", "reason": "演示源越界或不存在"}
    if source.stat().st_size > 256 * 1024 * 1024:
        return {"schema_version": 1, "status": "POLICY_BLOCKED", "error_class": "POLICY_BLOCKED", "reason": "演示源过大"}
    content = source.read_bytes()
    content_hash = hashlib.sha256(content).hexdigest()
    if content_hash != candidate.get("fixture_sha256"):
        return {"schema_version": 1, "status": "CONFLICT", "error_class": "CONFLICT", "reason": "搜索后合成来源内容发生变化"}
    task_id = "fixture-" + hashlib.sha256(candidate["candidate_id"].encode()).hexdigest()[:24]
    previous = store.get("download_tasks", "offline:" + task_id)
    if previous and previous.get("state") == "REQUIRES_VALIDATION":
        file = Path(previous["source_path"])
        if file.is_file() and hashlib.sha256(file.read_bytes()).hexdigest() == previous["source_sha256"]:
            return {**previous, "recovered": True}
        return {"schema_version": 1, "status": "CONFLICT", "error_class": "CONFLICT", "reason": "已下载来源与记录不符"}
    site = candidate["source"]
    if values.get("providers", {}).get(site, {}).get("enabled") is False:
        return {"schema_version": 1, "status": "POLICY_BLOCKED", "error_class": "POLICY_BLOCKED", "reason": "配置已禁用此来源"}
    if site == "netease":
        root = source_root / "netease" / task_id
        target = root / source.name
        if previous and previous.get("state") == "REQUESTING":
            if not target.is_file() or hashlib.sha256(target.read_bytes()).hexdigest() != content_hash:
                return {"schema_version": 1, "status": "REQUEST_UNKNOWN", "error_class": "POLICY_BLOCKED", "reason": "网易下载意图结果未知，禁止自动重发"}
            evidence = {"schema_version": 1, "state": "REQUIRES_VALIDATION", "status": "REQUIRES_VALIDATION", "task_id": task_id,
                        "source_path": str(target), "source_sha256": content_hash, "evidence_level": "synthetic_only",
                        "source": site, "recovered": True, "source_preserved": True, "roon_state": "pending"}
            store.put("download_tasks", "offline:" + task_id, evidence)
            return evidence
        transport = FakeTransport([Response(200, content, {"Content-Type": "audio/flac"})])
        provider = NetEaseProvider(transport)
        safe_point("before_download")
        store.put("download_tasks", "offline:" + task_id, {"schema_version": 1, "state": "REQUESTING", "task_id": task_id,
                                                           "source": site, "expected_sha256": content_hash, "source_path": str(target)})
        asset = provider.fetch_asset(candidate, target)
        if asset["status"] != "REQUIRES_VALIDATION":
            return asset
        safe_point("after_download")
        evidence = {"schema_version": 1, "state": "REQUIRES_VALIDATION", "status": "REQUIRES_VALIDATION", "task_id": task_id,
                    "source_path": str(target), "source_sha256": content_hash, "evidence_level": "synthetic_only",
                    "source": site, "requests": len(transport.requests), "source_preserved": True, "roon_state": "pending"}
    elif site in ("dicmusic", "open.cd", "orpheus"):
        piece_length = 16384
        pieces = b"".join(hashlib.sha1(content[i:i + piece_length]).digest() for i in range(0, len(content), piece_length))
        torrent = bencode({b"info": {b"name": source.name.encode(), b"length": len(content), b"piece length": piece_length, b"pieces": pieces}})
        meta = parse_metainfo(torrent)
        root = source_root / "pt" / site
        qbt_config = values.get("qbt", {})
        category, prefix = qbt_config.get("category", "musicops"), qbt_config.get("tag_prefix", "musicops-")
        qbt_transport = _QbtFixture(task_id, meta, root, store.get("download_tasks", "qbt:" + task_id), category=category, tag_prefix=prefix)
        qbt = QbtClient(qbt_transport, "https://qbt.invalid", root, store=store,
                        category=category, tag_prefix=prefix, auto_tmm=qbt_config.get("auto_tmm", False))
        if qbt.capabilities()["status"] != "OK":
            return {"schema_version": 1, "status": "POLICY_BLOCKED", "error_class": "POLICY_BLOCKED", "reason": "下载器配置无法保证停止加种和任务独立路径"}
        transport = FakeTransport([Response(200, torrent, {"X-MusicOps-Fixture-Free": candidate["resource_id"] + ":" + meta["infohash_v1"]})])
        provider = PTProvider(site, transport, "https://" + site.replace(".", "-") + ".invalid")
        budgets = values.get("token_budget", {"dicmusic": 3, "orpheus": 3, "safety_margin": 1})
        margin = budgets.get("safety_margin", 1)
        budget = budgets.get(site, 0) if site in ("dicmusic", "orpheus") else 0
        budget_ref = candidate.get("batch_id", values.get("batch_ref", "offline-demo"))
        safe_point("before_token")
        asset = TokenCoordinator(store, provider).fetch(candidate, balance=max(0, 5 - margin), budget=budget, observed_at=utcnow(), budget_ref=budget_ref)
        if asset["status"] != "READY":
            return asset
        if asset.get("task"):
            asset = asset["task"]["asset"]
        safe_point("after_token")
        safe_point("before_add")
        added = qbt.add_stopped(torrent, task_id, asset["free_evidence"])
        safe_point("after_add")
        if added["status"] == "STOPPED":
            safe_point("before_start")
            started = qbt.start_download(task_id, asset["free_evidence"])
            if started["status"] != "START_REQUESTED":
                return started
            safe_point("after_start")
        elif added["status"] not in ("COMPLETE", "START_REQUESTED", "DOWNLOADING"):
            return added
        target = root / task_id / source.name
        target.parent.mkdir(parents=True, exist_ok=True)
        if not target.exists():
            with target.open("xb") as stream:
                stream.write(content)
        if hashlib.sha256(target.read_bytes()).hexdigest() != content_hash:
            return {"schema_version": 1, "status": "CONFLICT", "error_class": "CONFLICT", "reason": "保种源已被外部改动"}
        client_check = qbt.poll(task_id)
        if client_check["status"] != "REQUIRES_MEDIA_VALIDATION":
            return client_check
        safe_point("after_download")
        evidence = {"schema_version": 1, "state": "REQUIRES_VALIDATION", "status": "REQUIRES_VALIDATION", "task_id": task_id,
                    "source_path": str(target), "source_sha256": content_hash, "evidence_level": "synthetic_only",
                    "source": site, "requests": len(transport.requests), "qbt_requests": qbt_transport.requests,
                    "free_evidence": asset["free_evidence"], "metainfo": meta, "seeding": "preserved", "source_preserved": True,
                    "client_check": client_check, "roon_state": "pending"}
    else:
        return {"schema_version": 1, "status": "POLICY_BLOCKED", "error_class": "POLICY_BLOCKED"}
    store.put("download_tasks", "offline:" + task_id, evidence)
    store.event("offline_download_ready", {"task_id": task_id, "source": site, "evidence_level": "synthetic_only", "seeding": evidence.get("seeding")}, task_id=task_id)
    return evidence
