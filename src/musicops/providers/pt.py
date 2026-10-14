"""Fail-closed PT adapters and single-request token coordination.

The fixture paths below are an internal test protocol, not site API claims.
Production capabilities remain blocked until a deployed site contract is verified.
"""
from __future__ import annotations

import base64
from datetime import datetime, timezone
from html.parser import HTMLParser
import json
import re
import threading
from urllib.parse import urlsplit

from musicops.clients.metainfo import parse_metainfo

TOKEN_SITES = {"dicmusic", "orpheus"}
SITES = TOKEN_SITES | {"open.cd"}


def result(status, **data):
    return {"schema_version": 1, "status": status, **data}


class _FixtureHTML(HTMLParser):
    """Explicit sanitized fixture schema; arbitrary live HTML is never parsed."""
    def __init__(self):
        super().__init__()
        self.items = []

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if tag == "article" and attrs.get("data-contract") == "musicops.fixture.v1":
            self.items.append({"id": attrs.get("data-resource-id"), "title": attrs.get("data-title"),
                               "artists": [x for x in attrs.get("data-artists", "").split("|") if x],
                               "release": {"album": attrs.get("data-album"), "edition": attrs.get("data-edition", "unknown")},
                               "seeders": int(attrs.get("data-seeders", "0")),
                               "eligibility": {"state": "UNKNOWN"}})


class PTProvider:
    def __init__(self, site, transport, base_url):
        if site not in SITES:
            raise ValueError("unknown PT site")
        parts = urlsplit(base_url)
        if parts.scheme != "https" or not parts.hostname or parts.username or parts.password or parts.query or parts.fragment:
            raise ValueError("PT base must be a clean HTTPS origin")
        self.site, self.transport, self.base_url = site, transport, base_url.rstrip("/")

    def capabilities(self):
        fake = getattr(self.transport, "is_fake", False)
        return result("OK" if fake else "POLICY_BLOCKED", source=self.site,
                      contract="musicops.fixture.v1", production_verified=False,
                      search=fake, parse_candidate=True, check_eligibility=fake,
                      fetch_asset=fake, reconcile=fake, reliable_free_confirmation=fake,
                      token_required=self.site in TOKEN_SITES,
                      reason="实际站点检索、资格、成本、免费确认与重复请求规则待现场确认")

    def _request(self, method, path, params=None):
        if not getattr(self.transport, "is_fake", False):
            return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        response = self.transport.request(method, self.base_url + path, params=params, timeout=12)
        if response.url:
            actual, expected = urlsplit(response.url), urlsplit(self.base_url + path)
            if (actual.scheme, actual.hostname, actual.port, actual.path) != (expected.scheme, expected.hostname, expected.port, expected.path):
                return result("ACCESS_BLOCKED", error_class="ACCESS_BLOCKED", reason="响应来源/路径变化，禁止裸链接降级")
        # No redirection is followed, including same-host links dropping usetoken.
        if 300 <= response.status_code < 400:
            return result("ACCESS_BLOCKED", error_class="ACCESS_BLOCKED", reason="禁止种子请求重定向")
        if response.status_code in (401, 403):
            return result("AUTH_FAILED", error_class="AUTH_FAILED")
        if response.status_code == 429:
            return result("RATE_LIMITED", error_class="RATE_LIMITED", retry_after=response.headers.get("Retry-After"))
        if response.status_code != 200:
            return result("NETWORK_ERROR", error_class="NETWORK_ERROR")
        return response

    def search(self, query):
        try:
            response = self._request("GET", "/fixture/search", {"query": str(query)})
            if isinstance(response, dict):
                return {**response, "candidates": []}
            content_type = {k.lower(): v for k, v in response.headers.items()}.get("content-type", "").lower()
            if "html" in content_type:
                parser = _FixtureHTML()
                parser.feed(response.text)
                raw = parser.items
                if not raw and "data-contract=\"musicops.fixture.v1\"" not in response.text:
                    return result("ACCESS_BLOCKED", error_class="ACCESS_BLOCKED", candidates=[])
            else:
                payload = response.json()
                if not isinstance(payload, dict) or payload.get("contract") != "musicops.fixture.v1" or not isinstance(payload.get("results"), list):
                    raise ValueError("unverified response schema")
                raw = payload["results"]
            candidates = [self.parse_candidate(item) for item in raw]
            return result("OK" if candidates else "NO_RESOURCE", source=self.site, candidates=candidates)
        except (ValueError, TypeError, KeyError):
            return result("PARSE_ERROR", error_class="PARSE_ERROR", source=self.site, candidates=[])
        except Exception as exc:
            error = getattr(exc, "error_class", "NETWORK_ERROR")
            return result(error, error_class=error, source=self.site, candidates=[])

    def parse_candidate(self, raw):
        if not isinstance(raw, dict) or not re.fullmatch(r"[1-9][0-9]*", str(raw.get("id", ""))):
            raise ValueError("invalid resource ID")
        if not isinstance(raw.get("title"), str) or not raw["title"].strip() or not isinstance(raw.get("artists"), list) or not raw["artists"]:
            raise ValueError("candidate identity missing")
        resource_id = str(raw["id"])
        return {"schema_version": 1, "candidate_id": self.site + ":" + resource_id,
                "source": self.site, "torrent_id": resource_id, "resource_id": resource_id,
                "title": raw["title"], "artists": list(raw["artists"]), "release": raw.get("release", {}),
                "media": raw.get("media", {}), "file_list": raw.get("file_list", []),
                "seeders": raw.get("seeders", 0), "eligibility": raw.get("eligibility", {"state": "UNKNOWN"}),
                "estimated_cost": raw.get("estimated_cost"), "evidence": raw.get("evidence", [])}

    def check_eligibility(self, candidate):
        if self.capabilities()["status"] != "OK" or not self.capabilities()["reliable_free_confirmation"]:
            return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="请求前不能可靠确认免费能力")
        if candidate.get("source") != self.site or candidate.get("eligibility", {}).get("state") != "ELIGIBLE":
            return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="候选资格未知或不合资格")
        if self.site in TOKEN_SITES:
            cost = candidate.get("estimated_cost")
            if type(cost) is not int or cost <= 0:
                return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="令牌成本未知")
        elif candidate.get("eligibility", {}).get("traffic_policy") != "FREE_CONFIRMED":
            return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="open.cd 流量政策未确认")
        return result("OK", eligible=True, cost=candidate.get("estimated_cost", 0))

    def fetch_asset(self, candidate, reservation=None):
        eligibility = self.check_eligibility(candidate)
        if eligibility["status"] != "OK":
            return eligibility
        if self.site in TOKEN_SITES and not reservation:
            return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="缺少事务预算预留")
        params = {"id": candidate["resource_id"]}
        if self.site in TOKEN_SITES:
            params["usetoken"] = "1"
        response = self._request("GET", "/fixture/torrent", params)
        if isinstance(response, dict):
            return response
        if "html" in {k.lower(): v for k, v in response.headers.items()}.get("content-type", "").lower() or response.body.lstrip().lower().startswith((b"<html", b"<!doctype", b"<form", b"<body")):
            return result("ACCESS_BLOCKED", error_class="ACCESS_BLOCKED", reason="登录/风控页面")
        try:
            meta = parse_metainfo(response.body)
        except ValueError:
            return result("PARSE_ERROR", error_class="PARSE_ERROR", reason="metainfo 无效")
        binding = response.headers.get("X-MusicOps-Fixture-Free", "")
        digest = meta.get("infohash_v1") or meta["infohash_v2"]
        free = binding == candidate["resource_id"] + ":" + digest
        return result("READY" if free else "POLICY_BLOCKED", error_class=None if free else "POLICY_BLOCKED",
                      metainfo=meta, metainfo_base64=base64.b64encode(response.body).decode(),
                      free_evidence={"schema_version": 1, "source": self.site, "resource_id": candidate["resource_id"],
                                     "infohash": digest, "confirmed": free,
                                     "kind": "fixture_site_confirmation", "production_verified": False,
                                     "observed_at": datetime.now(timezone.utc).isoformat()},
                      reason=None if free else "合法 torrent 不能证明免费生效")

    def reconcile(self, candidate, reservation=None):
        if self.capabilities()["status"] != "OK":
            return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED")
        # This endpoint is read-only by definition of the fixture contract.
        try:
            response = self._request("GET", "/fixture/reconcile", {"id": candidate["resource_id"]})
            if isinstance(response, dict):
                return response
            data = response.json()
            if not isinstance(data, dict) or data.get("contract") != "musicops.fixture.v1" or data.get("state") not in ("CONSUMED", "NOT_CONSUMED", "UNKNOWN"):
                raise ValueError("invalid reconcile contract")
            return result("OK", state=data["state"], evidence={"source": self.site, "resource_id": candidate["resource_id"], "kind": "fixture_reconciliation", "production_verified": False})
        except (ValueError, TypeError):
            return result("PARSE_ERROR", error_class="PARSE_ERROR")
        except Exception as exc:
            error = getattr(exc, "error_class", "NETWORK_ERROR")
            return result(error, error_class=error)


class TokenCoordinator:
    _locks_guard = threading.Lock()
    _site_locks = {}

    def __init__(self, store, provider):
        self.store, self.provider = store, provider
        with self._locks_guard:
            self._lock = self._site_locks.setdefault(provider.site, threading.Lock())

    def fetch(self, candidate, balance, budget, observed_at, budget_ref="default"):
        with self._lock:
            eligibility = self.provider.check_eligibility(candidate)
            if eligibility["status"] != "OK":
                return eligibility
            task_id = "asset:" + candidate["candidate_id"]
            existing = self.store.get("download_tasks", task_id)
            if existing:
                if existing["state"] == "READY":
                    if existing.get("budget_ref"):
                        reservation = self.store.get("token_reservations", existing["budget_ref"])
                        evidence = existing.get("asset", {}).get("free_evidence")
                        if reservation and reservation["state"] in ("REQUESTING", "UNKNOWN", "REQUEST_UNKNOWN") and evidence and evidence.get("confirmed"):
                            self.store.token_result(existing["budget_ref"], "CONFIRMED", evidence)
                    return result("READY", task=existing, recovered=True)
                return result("REQUEST_UNKNOWN" if existing["state"] in ("REQUESTING", "REQUEST_UNKNOWN") else "POLICY_BLOCKED",
                              task=existing, error_class="POLICY_BLOCKED", reason="已有请求需恢复/对账，禁止重发")
            blocked = self.store.get("evidence", "provider-block:" + self.provider.site)
            if blocked and blocked.get("blocked"):
                return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="站点鉴权/结构/免费确认失败，需显式对账恢复", evidence=blocked)
            if self.provider.site not in TOKEN_SITES:
                task = {"schema_version": 1, "task_id": task_id, "candidate_id": candidate["candidate_id"],
                        "state": "REQUESTING", "source": self.provider.site}
                self.store.put("download_tasks", task_id, task)
                self.store.event("ASSET_REQUEST_INTENT", {"site": self.provider.site, "resource_id": candidate["resource_id"]}, task_id=task_id)
                try:
                    asset = self.provider.fetch_asset(candidate)
                except Exception:
                    task["state"] = "REQUEST_UNKNOWN"
                    self.store.put("download_tasks", task_id, task)
                    return result("REQUEST_UNKNOWN", task=task, error_class="POLICY_BLOCKED")
                task.update(state=asset["status"], asset=asset)
                self.store.put("download_tasks", task_id, task)
                if asset["status"] != "READY":
                    self.store.put("evidence", "provider-block:" + self.provider.site,
                                   {"schema_version": 1, "source": self.provider.site, "blocked": True,
                                    "error_class": asset.get("error_class"), "resource_id": candidate["resource_id"],
                                    "kind": "site_policy_block", "production_verified": False})
                return asset
            try:
                reservation = self.store.reserve_token(self.provider.site, candidate["resource_id"],
                                                       eligibility["cost"], balance, budget, observed_at, budget_ref=budget_ref)
            except (ValueError, RuntimeError) as exc:
                return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason=str(exc))
            reservation_id = reservation.get("id", reservation.get("reservation_id"))
            task = {"schema_version": 1, "task_id": task_id, "candidate_id": candidate["candidate_id"],
                    "source": self.provider.site, "resource_id": candidate["resource_id"],
                    "budget_ref": reservation_id, "state": "REQUESTING"}
            # Commit intent before network. A crash here is UNKNOWN on recovery.
            self.store.put("download_tasks", task_id, task)
            self.store.token_result(reservation_id, "REQUESTING")
            self.store.event("TOKEN_REQUEST_INTENT", {"site": self.provider.site, "resource_id": candidate["resource_id"],
                                                    "reservation_id": reservation_id}, task_id=task_id)
            try:
                asset = self.provider.fetch_asset(candidate, reservation)
            except Exception:
                # Never write raw exception text (may include sensitive URLs).
                task["state"] = "REQUEST_UNKNOWN"
                self.store.put("download_tasks", task_id, task)
                self.store.token_result(reservation_id, "UNKNOWN", {"reason": "request interrupted"})
                self.store.event("TOKEN_REQUEST_UNKNOWN", {"site": self.provider.site, "resource_id": candidate["resource_id"]}, task_id=task_id)
                return result("REQUEST_UNKNOWN", task=task, error_class="POLICY_BLOCKED")
            # Persist returned artifact first. No successful reply is refetched.
            task.update(state=asset["status"], asset=asset)
            self.store.put("download_tasks", task_id, task)
            if asset["status"] != "READY":
                self.store.put("evidence", "provider-block:" + self.provider.site,
                               {"schema_version": 1, "source": self.provider.site, "blocked": True,
                                "error_class": asset.get("error_class"), "resource_id": candidate["resource_id"],
                                "kind": "site_policy_block", "production_verified": False})
            self.store.token_result(reservation_id, "CONFIRMED" if asset["status"] == "READY" else "CONSUMED", asset.get("free_evidence"))
            self.store.event("TOKEN_REQUEST_RESULT", {"site": self.provider.site, "resource_id": candidate["resource_id"],
                                                      "status": asset["status"]}, task_id=task_id)
            return asset

    def reconcile(self, candidate):
        task_id = "asset:" + candidate["candidate_id"]
        task = self.store.get("download_tasks", task_id)
        if not task or task["state"] not in ("REQUESTING", "REQUEST_UNKNOWN"):
            return result("POLICY_BLOCKED", error_class="POLICY_BLOCKED", reason="没有待对账的请求")
        evidence = self.provider.reconcile(candidate)
        if evidence.get("state") == "NOT_CONSUMED":
            if task.get("budget_ref"):self.store.token_result(task["budget_ref"], "RELEASED", evidence.get("evidence"))
            task["state"] = "RECONCILED_NOT_CONSUMED"
        elif evidence.get("state") == "CONSUMED":
            if task.get("budget_ref"):self.store.token_result(task["budget_ref"], "CONSUMED", evidence.get("evidence"))
            task["state"] = "RECONCILED_CONSUMED"
        else:
            return result("REQUEST_UNKNOWN", error_class="POLICY_BLOCKED", evidence=evidence)
        self.store.put("download_tasks", task_id, task)
        self.store.event("TOKEN_RECONCILED", {"site": self.provider.site, "resource_id": candidate["resource_id"],
                                            "state": task["state"]}, task_id=task_id)
        return result("OK", task=task, evidence=evidence)


def choose_unique_candidate(candidates, release_id, site_order=("dicmusic", "open.cd", "orpheus", "netease")):
    """Explicit release identity and full track-list evidence precede site order."""
    valid = [c for c in candidates if c.get("release", {}).get("release_id") == release_id
             and c.get("release", {}).get("complete_tracklist") is True
             and c.get("eligibility", {}).get("state") == "ELIGIBLE"]
    if not valid:
        return result("POLICY_BLOCKED", candidate=None, reason="没有版本与曲目表均确证的候选")
    valid.sort(key=lambda c: (site_order.index(c["source"]) if c["source"] in site_order else 999, c["candidate_id"]))
    return result("OK", candidate=valid[0], considered_ids=[c["candidate_id"] for c in valid])
