"""Local playlist parsing and versioned, offline NetEase fixture adapter.

No undocumented production API is guessed. Fixture protocol is intentionally
separate from a real deployment contract and cannot be enabled by changing Base.
"""
from __future__ import annotations

from collections import Counter
from datetime import datetime, timezone
import hashlib
import html
import json
import re
import time
from urllib.parse import parse_qs, urlsplit
from pathlib import Path

ALLOWED_HOSTS = {"music.163.com", "m.music.163.com", "y.music.163.com"}
SHORT_HOSTS = {"163cn.tv"}


def _result(status, **kwargs):
    return {"schema_version": 1, "status": status, **kwargs}


def parse_playlist_input(text: str) -> dict:
    """Parse only explicitly pasted values; no clipboard/network/file access."""
    if not isinstance(text, str) or not text.strip():
        return _result("INVALID_INPUT", playlist_ids=[], errors=["请输入歌单ID或链接"])
    if len(text)>100_000:return _result("INVALID_INPUT",playlist_ids=[],errors=["歌单输入超出长度限制"])
    raw = html.unescape(text.strip())
    ids, links, errors, short = [], [], [], []
    if re.fullmatch(r"\d+(?:[\s,，;；]+\d+)*", raw):
        ids = re.split(r"[\s,，;；]+", raw)
        if any(len(value)>19 for value in ids):return _result("INVALID_INPUT",playlist_ids=[],errors=["歌单ID最多19位"])
    else:
        urls = re.findall(r"https?://[^\s<>\"“”]+", raw)
        if not urls:
            return _result("INVALID_INPUT", playlist_ids=[], errors=["没有可识别的歌单链接"])
        for url in urls:
            url = url.rstrip(")）]】。，,;；")
            try:
                parts = urlsplit(url)
                port = parts.port
            except ValueError:
                errors.append("链接结构或端口无效")
                continue
            host = (parts.hostname or "").lower()
            if parts.username or parts.password or (port not in (None, 80, 443)):
                errors.append("链接含不允许的身份或端口")
                continue
            if host in SHORT_HOSTS:
                short.append(parts._replace(query='',fragment='').geturl())
                continue
            if host not in ALLOWED_HOSTS:
                errors.append("链接域名不在网易云允许范围")
                continue
            values = []
            paths = [(parts.path, parts.query)]
            if parts.fragment:
                frag = urlsplit(parts.fragment)
                paths.append((frag.path, frag.query))
            recognized = False
            for path, query in paths:
                if path.rstrip("/") not in ("/playlist", "/m/playlist"):
                    continue
                recognized = True
                values += parse_qs(query, keep_blank_values=True).get("id", [])
            if not recognized or not values or any(not re.fullmatch(r"[0-9]+", value) for value in values):
                errors.append("歌单ID缺失或不是纯数字")
                continue
            if any(len(value)>19 for value in values):
                errors.append("歌单ID最多19位")
                continue
            normalized = {str(int(value)) for value in values}
            if len(normalized) != 1:
                errors.append("同一链接含冲突的歌单ID，需选择")
                continue
            playlist_id = normalized.pop()
            if playlist_id == "0":
                errors.append("歌单ID必须大于0")
                continue
            ids.append(playlist_id)
            links.append({"canonical_url": "https://music.163.com/playlist?id="+playlist_id, "playlist_id": playlist_id, "network_used": False})
    normalized_ids = list(dict.fromkeys(str(int(x)) for x in ids if int(x) > 0))
    if ids and any(int(x) == 0 for x in ids):
        errors.append("歌单ID必须大于0")
    status = "INVALID_INPUT" if errors else "NEEDS_NETWORK" if short else "OK"
    return _result(status, source="netease", playlist_ids=normalized_ids,
                   links=links, short_links=short, errors=errors, network_used=False)


def _decode_batch(response):
    if hasattr(response, "status_code"):
        code = response.status_code
        if code != 200:
            return {"status_code": code, "headers": response.headers}
        try:
            response = response.json()
        except (ValueError, UnicodeError):
            return {"error_class": "PARSE_ERROR"}
    if isinstance(response, list):
        return {"songs": response}
    return response if isinstance(response, dict) else {"error_class": "PARSE_ERROR"}


def build_snapshot(playlist_id, ordered_ids, fetch_batch, batch_size=150) -> dict:
    """Fetch every tail, split only explicit parameter-limit errors, reconcile IDs."""
    if not 1 <= batch_size <= 10000:
        raise ValueError("batch_size must be 1..10000")
    ordered = [str(x) for x in ordered_ids]
    unique = list(dict.fromkeys(ordered))
    if any(not re.fullmatch(r"[1-9][0-9]*", value) for value in unique):
        raise ValueError("invalid track ID")
    returned, songs, failures, unavailable, batch_records = [], [], [], [], []
    halted = None

    def fetch(ids, attempt=0):
        nonlocal halted
        if halted:
            failures.append({"requested_ids": ids, "error_class": halted, "attempts": 0, "reason": "停止后续批次，等待来源恢复或Retry-After"})
            return
        try:
            response = _decode_batch(fetch_batch(list(ids)))
        except Exception as exc:
            error = getattr(exc, "error_class", "NETWORK_ERROR")
            if error == "CANCELLED":
                raise
            if error in ("AUTH_REQUIRED", "AUTH_FAILED", "RATE_LIMITED", "ACCESS_BLOCKED", "POLICY_BLOCKED"):
                halted = error
            if error in ("NETWORK_ERROR", "TRANSIENT_ERROR") and attempt < 1:
                time.sleep(.1)
                return fetch(ids, attempt + 1)
            failures.append({"requested_ids": ids, "error_class": error, "attempts": attempt + 1})
            return
        code = response.get("status_code", response.get("code", 200))
        error = response.get("error_class")
        if error == "PARAM_LIMIT" or (code in (400, 413, 414) and response.get("parameter_limit") is True):
            if len(ids) > 1:
                split = len(ids) // 2
                fetch(ids[:split])
                fetch(ids[split:])
                return
            error = "PARAM_LIMIT"
        elif code in (401, 403):
            error = "AUTH_FAILED"
        elif code == 429:
            error = "RATE_LIMITED"
        elif isinstance(code, int) and code >= 500:
            if attempt < 1:
                time.sleep(.1)
                return fetch(ids, attempt + 1)
            error = "NETWORK_ERROR"
        elif code != 200:
            error = error or "PARSE_ERROR"
        if error:
            if error in ("AUTH_REQUIRED", "AUTH_FAILED", "RATE_LIMITED", "ACCESS_BLOCKED", "POLICY_BLOCKED"):
                halted = error
            failures.append({"requested_ids": ids, "error_class": error,
                             "retry_after": response.get("headers", {}).get("Retry-After"), "attempts": attempt + 1})
            return
        items = response.get("songs")
        if not isinstance(items, list):
            failures.append({"requested_ids": ids, "error_class": "PARSE_ERROR", "reason": "missing songs array"})
            return
        for item_error in response.get("item_errors", []):
            failures.append({"requested_ids": ids, "error_class": "PARSE_ERROR",
                             "reason": "song metadata rejected", **item_error})
        valid_items = []
        for item in items:
            if not isinstance(item, dict) or not re.fullmatch(r"[1-9][0-9]*", str(item.get("id", ""))):
                failures.append({"requested_ids": ids, "error_class": "PARSE_ERROR", "reason": "song missing ID"})
                continue
            song_id = str(item["id"])
            returned.append(song_id)
            songs.append({**item, "id": song_id})
            valid_items.append(song_id)
            if item.get("available") is False:
                unavailable.append(song_id)
            else:
                title = item.get("raw_title", item.get("title", item.get("name")))
                artists = item.get("artists", item.get("ar"))
                album = item.get("source_album_id", item.get("album", item.get("al")))
                if not title or not isinstance(artists, list) or not artists or not album:
                    failures.append({"requested_ids": ids, "error_class": "PARSE_ERROR", "reason": "song identity fields incomplete", "song_id": song_id})
        actual = set(valid_items)
        requested = set(ids)
        batch_records.append({"requested_ids": ids, "returned_ids": valid_items,
                              "missing_ids": sorted(requested - actual), "unexpected_ids": sorted(actual - requested)})

    for start in range(0, len(unique), batch_size):
        fetch(unique[start:start + batch_size])
    counts = Counter(returned)
    missing = sorted(set(unique) - set(returned))
    unexpected = sorted(set(returned) - set(unique))
    duplicate = sorted(x for x, count in counts.items() if count > 1)
    batch_mismatch = any(x["missing_ids"] or x["unexpected_ids"] for x in batch_records)
    complete = not (missing or unexpected or duplicate or failures or batch_mismatch)
    content = {"playlist_id": str(playlist_id), "ordered_ids": ordered, "songs": songs}
    digest = hashlib.sha256(json.dumps(content, ensure_ascii=False, sort_keys=True).encode()).hexdigest()
    return {"schema_version": 1, "snapshot_id": "netease:" + str(playlist_id) + ":" + digest[:16],
            "source": "netease", "playlist_id": str(playlist_id), "ordered_ids": ordered,
            "unique_ids": unique, "requested_ids": unique, "returned_ids": returned,
            "missing_ids": missing, "duplicate_ids": duplicate, "unexpected_ids": unexpected,
            "unavailable_ids": unavailable, "complete": complete, "fetched_at": datetime.now(timezone.utc).isoformat(),
            "hash": digest, "songs": songs, "failed_batches": failures, "batches": batch_records}


class NetEaseProvider:
    """Six-interface adapter. musicops.fixture.v1 is never claimed as an online API."""
    def __init__(self, transport, base_url="https://netease.invalid"):
        self.transport, self.base_url = transport, base_url.rstrip("/")
        self.site = "netease"

    def capabilities(self):
        fake = getattr(self.transport, "is_fake", False)
        return _result("OK" if fake else "POLICY_BLOCKED", source=self.site,
                       contract="musicops.fixture.v1", production_verified=False,
                       search=fake, fetch_asset=fake, reason="生产 API/权限契约待现场核实")

    def search(self, query):
        if not getattr(self.transport, "is_fake", False):
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", candidates=[])
        try:
            response = self.transport.request("GET", self.base_url + "/fixture/search", params={"query": query}, timeout=12)
            if response.status_code in (401, 403):
                return _result("AUTH_FAILED", error_class="AUTH_FAILED", candidates=[])
            if response.status_code == 429:
                return _result("RATE_LIMITED", error_class="RATE_LIMITED", candidates=[])
            if response.status_code != 200:
                return _result("NETWORK_ERROR", error_class="NETWORK_ERROR", candidates=[])
            body = response.json()
            if not isinstance(body, dict) or not isinstance(body.get("songs"), list):
                raise ValueError("fixture schema")
            candidates = [self.parse_candidate(item) for item in body["songs"]]
            return _result("OK" if candidates else "NO_RESOURCE", candidates=candidates)
        except (ValueError, TypeError, KeyError):
            return _result("PARSE_ERROR", error_class="PARSE_ERROR", candidates=[])
        except Exception as exc:
            error = getattr(exc, "error_class", "NETWORK_ERROR")
            return _result(error, error_class=error, candidates=[])

    def parse_candidate(self, raw):
        if not isinstance(raw, dict) or not re.fullmatch(r"[1-9][0-9]*", str(raw.get("id", ""))):
            raise ValueError("candidate requires song ID")
        if not isinstance(raw.get("artists"), list) or not raw["artists"] or not isinstance(raw.get("title"), str) or not raw["title"].strip():
            raise ValueError("candidate identity missing")
        return {"schema_version": 1, "source": "netease", "song_id": str(raw["id"]),
                "candidate_id": "netease:" + str(raw["id"]), "release": raw.get("release", {}),
                "title": raw["title"], "artists": raw["artists"], "version": raw.get("version", "unknown"),
                "media": raw.get("media", {}), "file_list": raw.get("file_list", []),
                "eligibility": raw.get("eligibility", {"state": "UNKNOWN"}), "estimated_cost": 0,
                "evidence": raw.get("evidence", [])}

    def check_eligibility(self, candidate):
        eligibility = candidate.get("eligibility", {})
        if not getattr(self.transport, "is_fake", False):
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        if eligibility.get("state") != "ELIGIBLE":
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="登录、权益或曲目可用性未确认")
        return _result("OK", eligible=True)

    def fetch_asset(self, candidate, destination):
        if self.check_eligibility(candidate)["status"] != "OK":
            return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        target = Path(destination)
        if target.exists():
            return _result("CONFLICT", error_class="CONFLICT")
        try:
            response = self.transport.request("GET", self.base_url + "/fixture/audio", params={"id": candidate["song_id"]}, timeout=12)
            if response.status_code != 200:
                return _result("AUTH_FAILED" if response.status_code in (401, 403) else "NETWORK_ERROR", error_class="ACCESS_BLOCKED")
            from musicops.media.ncm import detect_media
            kind = detect_media(response.body)
            if kind == "unknown" or len(response.body) > 256 * 1024 * 1024:
                return _result("PARSE_ERROR", error_class="PARSE_ERROR", reason="未知媒体或超出大小限制")
            target.parent.mkdir(parents=True, exist_ok=True)
            with target.open("xb") as stream:
                stream.write(response.body)
            return _result("REQUIRES_DECRYPTION" if kind == "ncm" else "REQUIRES_VALIDATION",
                           path=str(target), format=kind, sha256=hashlib.sha256(response.body).hexdigest(),
                           success=False, validation_state="pending")
        except Exception as exc:
            error = getattr(exc, "error_class", "NETWORK_ERROR")
            return _result(error, error_class=error)

    def reconcile(self, candidate):
        return _result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="真实权限及下载恢复契约待现场核实")
