"""Read-only review regressions; all writes remain in pytest temporary profiles.

The E: guard test intercepts every attempted write before it can reach a real
drive. URL samples are virtual review strings, never credentials or live hosts.
"""
from __future__ import annotations

import copy
import json
from pathlib import Path
import shutil
import time

import pytest

from musicops.app.service import AppService
from musicops.clients.http import PolicyBlocked
from musicops.config.schema import default_config, validate_config
from musicops.providers.workflow import run_offline_download


@pytest.fixture
def profile(tmp_path):
    service = AppService(tmp_path / "审查合成档案")
    try:
        yield service
    finally:
        service.stop.set()
        service.thread.join(2)


def assert_policy_blocked(operation):
    try:
        value = operation()
    except PolicyBlocked:
        return
    assert isinstance(value, dict) and (value.get("status") == "POLICY_BLOCKED" or value.get("error_class") == "POLICY_BLOCKED"), value


@pytest.mark.parametrize("section,key", [("primary", "base_url"), ("direct_backup", "base_url"),
                                         ("public_fallback", "lyrics_url"), ("public_fallback", "cover_url")])
def test_F31_V36_secret_URL_never_enters_plain_configuration(tmp_path, section, key):
    values = default_config(tmp_path)
    values["lyrics"][section][key] = "https://virtual_user:virtual_review_password@fixture.invalid/path?token=virtual_review_token"
    with pytest.raises(ValueError):
        validate_config(values)


def test_F31_V36_qbt_secret_URL_rejected(tmp_path):
    values = default_config(tmp_path)
    values["qbt"]["base_url"] = "http://virtual_user:virtual_review_password@127.0.0.1:8080"
    with pytest.raises(ValueError):
        validate_config(values)


@pytest.mark.parametrize("command,args", [("prepare_repair", {"path": "dummy", "changes": {"album": ["fixture"]}}),
                                          ("prepare_resource", {"path": "dummy", "kind": "lyrics", "lyrics": "[00:00.00]Fixture"}),
                                          ("resources", {}), ("search", {"query": "fixture"})])
def test_G1_V36_external_E_root_blocked_before_any_write(profile, monkeypatch, command, args):
    cfg = copy.deepcopy(profile.config.current()["values"])
    cfg["paths"]["work"] = "E:/music"
    attempts = []
    original_mkdir = Path.mkdir
    original_copy = shutil.copy2

    def guarded_mkdir(path, *a, **kw):
        if str(path).lower().startswith(("e:", "e/")):
            attempts.append(("mkdir", str(path)))
            raise AssertionError("write guard intercepted a prohibited E: operation")
        return original_mkdir(path, *a, **kw)

    def guarded_copy(source, destination, *a, **kw):
        if str(destination).lower().startswith(("e:", "e/")):
            attempts.append(("copy", str(destination)))
            raise AssertionError("write guard intercepted a prohibited E: operation")
        return original_copy(source, destination, *a, **kw)

    monkeypatch.setattr(Path, "mkdir", guarded_mkdir)
    monkeypatch.setattr(shutil, "copy2", guarded_copy)
    assert_policy_blocked(lambda: profile._execute(command, args, cfg))
    assert attempts == []


def test_F08_V04_real_playlist_ID_not_fabricated_from_demo_targets(profile):
    cfg = profile.config.current()["values"]
    profile.store.put("target_tracks", "netease:1", {"schema_version": 1, "target_id": "netease:1", "source_song_id": "1",
                                                    "raw_title": "Fixture", "artists": ["Virtual Artist"], "source_album_id": "virtual"})
    assert_policy_blocked(lambda: profile._execute("refresh_playlist", {"playlist_id": "987654321"}, cfg))
    assert not [s for s in profile.store.list("playlist_snapshots") if s["playlist_id"] == "987654321" and s.get("complete")]


def test_F08_V04_demo_snapshot_is_explicitly_synthetic(profile):
    cfg = profile.config.current()["values"]
    profile._execute("demo", {}, cfg)
    result = profile._execute("refresh_playlist", {"playlist_id": "10001"}, cfg)
    assert result["complete"] and result["evidence"] == "synthetic_only"
    assert result["snapshots"] and all(s.get("evidence_level") == "synthetic_only" for s in result["snapshots"])


def test_F08_V04_incomplete_snapshot_stops_plan_and_download_requests(profile, monkeypatch):
    cfg = profile.config.current()["values"]
    profile._execute("demo", {}, cfg)
    snapshot = profile._execute("refresh_playlist", {"playlist_id": "10001"}, cfg)["snapshots"][0]
    candidate = profile._execute("search", {"query": "星河"}, cfg)["candidates"][0]
    prepared = profile._execute("prepare_fill", {"candidate": candidate}, cfg)["plan"]
    assert prepared.get("snapshot_hash") or prepared.get("input_snapshot_hash")
    prepared["reviewed"] = True
    snapshot["complete"] = False
    snapshot["missing_ids"] = ["2"]
    profile.store.put("playlist_snapshots", snapshot["snapshot_id"], snapshot)
    calls = []

    def forbidden_download(*a, **kw):
        calls.append("download")
        raise AssertionError("an incomplete snapshot reached the source workflow")

    monkeypatch.setattr("musicops.providers.workflow.run_offline_download", forbidden_download)
    assert_policy_blocked(lambda: profile._execute("prepare_fill", {"candidate": candidate}, cfg))
    assert_policy_blocked(lambda: profile._execute("submit_plan", {"plan": prepared}, cfg))
    assert_policy_blocked(lambda: profile._execute("submit_download", {"candidate": candidate}, cfg))
    assert calls == []
    assert profile.store.list("token_reservations") == []


def test_F23_V31_conflict_result_never_green_completed(profile, monkeypatch):
    monkeypatch.setattr(profile, "_execute", lambda *a, **kw: {"schema_version": 1, "state": "CONFLICT", "error": {"error_class": "CONFLICT"}})
    response = profile.dispatch("search", {"query": "virtual"})
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        task = profile.store.get("tasks", response["task_id"])
        if task["state"] not in ("queued", "running"):
            assert task["state"] in ("blocked", "recovery_required", "failed")
            return
        time.sleep(.02)
    raise AssertionError("task result not persisted")


def test_F36_V37_cancel_before_token_has_zero_budget_consumption(profile):
    cfg = profile.config.current()["values"]
    profile._execute("demo", {}, cfg)
    candidate = profile._execute("search", {"query": "星河"}, cfg)["candidates"][0]
    phases = []
    def cancelled(phase):
        phases.append(phase)
        if phase == "before_token":
            raise PolicyBlocked("virtual cancellation at a safe point")
    with pytest.raises(PolicyBlocked):
        run_offline_download(profile.store, cfg, candidate, checkpoint=cancelled)
    assert phases == ["before_token"]
    assert profile.store.list("token_reservations") == []
    assert not [t for t in profile.store.list("download_tasks") if t.get("creation_intent")]


def test_F36_V37_cancel_after_token_keeps_evidence_no_qbt_then_recovers(profile):
    cfg = profile.config.current()["values"]
    profile._execute("demo", {}, cfg)
    candidate = profile._execute("search", {"query": "星河"}, cfg)["candidates"][0]
    def cancelled(phase):
        if phase == "after_token":
            raise PolicyBlocked("virtual cancellation after acknowledged token")
    with pytest.raises(PolicyBlocked):
        run_offline_download(profile.store, cfg, candidate, checkpoint=cancelled)
    reservations = profile.store.list("token_reservations")
    assert len(reservations) == 1 and reservations[0]["state"] == "CONFIRMED"
    assert not [t for t in profile.store.list("download_tasks") if t.get("creation_intent")]
    recovered = run_offline_download(profile.store, cfg, candidate)
    assert recovered["status"] == "REQUIRES_VALIDATION"
    assert reservations == profile.store.list("token_reservations")
