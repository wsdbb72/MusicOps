from __future__ import annotations

import base64
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone
import hashlib
import io
import json
from pathlib import Path
import struct
import wave

import pytest
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.padding import PKCS7

from musicops.clients.http import FakeTransport, Response, SafeHttpTransport
from musicops.clients.metainfo import bencode, parse_metainfo
from musicops.clients.qbt import QbtClient
from musicops.media.ncm import CORE_KEY, META_KEY, decrypt_ncm, detect_media
from musicops.providers.netease import NetEaseProvider, build_snapshot, parse_playlist_input
from musicops.providers.pt import PTProvider, TokenCoordinator, choose_unique_candidate
from musicops.providers.workflow import offline_search, run_offline_download
from musicops.state.store import StateStore, utcnow


def response(payload, status=200, headers=None):
    return Response(status, json.dumps(payload).encode(), headers or {})


def song(id):
    return {"id": id, "title": "fixture", "artists": ["Fixture Artist"], "source_album_id": "10"}


def torrent(kind="v1", name="sample.wav", content=b"x" * 100):
    info = {b"name": name.encode(), b"piece length": 16384}
    if kind in ("v1", "hybrid"):
        info.update({b"length": len(content), b"pieces": hashlib.sha1(content).digest()})
    if kind in ("v2", "hybrid"):
        info.update({b"meta version": 2, b"file tree": {name.encode(): {b"": {b"length": len(content), b"pieces root": hashlib.sha256(content).digest()}}}})
    root = {b"info": info}
    if kind in ("v2", "hybrid"):
        root[b"piece layers"] = {}
    return bencode(root), info


def candidate(provider, resource="101", eligibility="ELIGIBLE", cost=1):
    raw = {"id": resource, "title": "Synthetic", "artists": ["Fixture Artist"],
           "release": {"release_id": "release-1", "edition": "original", "complete_tracklist": True},
           "eligibility": {"state": eligibility, "traffic_policy": "FREE_CONFIRMED"},
           "file_list": [{"path": "sample.wav", "length": 100}], "estimated_cost": cost, "seeders": 2}
    return provider.parse_candidate(raw)


def asset_response(resource="101", confirmed=True):
    data, _ = torrent()
    meta = parse_metainfo(data)
    headers = {"X-MusicOps-Fixture-Free": resource + ":" + meta["infohash_v1"]} if confirmed else {}
    return Response(200, data, headers)


def test_V01_local_parser_all_forms_and_errors():
    assert parse_playlist_input("123")["playlist_ids"] == ["123"]
    assert parse_playlist_input("123\n456")["playlist_ids"] == ["123", "456"]
    for text in ["https://music.163.com/playlist?id=123", "https://music.163.com/#/playlist?id=123",
                 "https://y.music.163.com/m/playlist?id=123", "分享《合成》 https://music.163.com/playlist?id=123&amp;id=123。"][::1]:
        assert parse_playlist_input(text)["playlist_ids"] == ["123"]
    for text in ["https://music.163.com/playlist?id=1&id=2", "https://music.163.com/playlist?id=bad",
                 "https://example.com/playlist?id=123", "https://music.163.com.evil.invalid/playlist?id=123", "0"]:
        assert parse_playlist_input(text)["status"] == "INVALID_INPUT"
    short = parse_playlist_input("https://163cn.tv/synthetic")
    assert short["status"] == "NEEDS_NETWORK" and short["network_used"] is False
    assert parse_playlist_input("分享 https://music.163.com/playlist?id=1 https://music.163.com/playlist?id=2")["playlist_ids"] == ["1", "2"]
    assert parse_playlist_input("https://music.163.com:invalid/playlist?id=1")["status"] == "INVALID_INPUT"


@pytest.mark.parametrize("count", [1, 9, 150, 151, 159])
def test_V02_every_tail_and_parameter_split(count):
    calls = []
    def fetch(ids):
        calls.append(list(ids))
        if len(ids) > 7:
            return {"status_code": 413, "parameter_limit": True}
        return {"songs": [song(x) for x in ids]}
    ids = [str(x) for x in range(1, count + 1)]
    snapshot = build_snapshot("123", ids, fetch, batch_size=150)
    assert snapshot["complete"]
    assert set(snapshot["returned_ids"]) == set(ids)
    assert snapshot["missing_ids"] == []
    assert calls[-1][-1] == ids[-1]


def test_V03_equal_count_changed_ID_invalidates_snapshot():
    fetch = lambda ids: [song(x) for x in ids]
    first = build_snapshot("123", [1, 2], fetch)
    second = build_snapshot("123", [1, 3], fetch)
    assert first["complete"] and second["complete"] and first["hash"] != second["hash"]
    assert first["snapshot_id"] != second["snapshot_id"]
    shared = build_snapshot("123", [1, 1, 2], fetch)
    assert shared["ordered_ids"] == ["1", "1", "2"] and shared["unique_ids"] == ["1", "2"]


@pytest.mark.parametrize("body,field", [({}, "failed_batches"), ({"songs": [{}]}, "failed_batches"),
                          ({"songs": [{"id": 1}, {"id": 1}]}, "duplicate_ids"),
                          ({"songs": [{"id": 1}, {"id": 99}]}, "unexpected_ids"),
                          ({"songs": [{"id": 1}]}, "missing_ids")])
def test_V04_bad_200_never_complete(body, field):
    snapshot = build_snapshot("123", [1, 2], lambda ids: body)
    assert snapshot["complete"] is False and snapshot[field]


@pytest.mark.parametrize("code,error", [(401, "AUTH_FAILED"), (403, "AUTH_FAILED"), (429, "RATE_LIMITED"), (500, "NETWORK_ERROR"), (400, "PARSE_ERROR")])
def test_V05_failure_classes_not_parameter_split(code, error):
    calls = []
    def fetch(ids):
        calls.append(ids)
        return {"status_code": code, "headers": {"Retry-After": "30"}}
    snapshot = build_snapshot("123", [1, 2, 3], fetch)
    assert snapshot["failed_batches"][0]["error_class"] == error
    assert len(calls) == (2 if code >= 500 else 1)
    assert all(len(batch) == 3 for batch in calls)
    if code == 429:
        assert snapshot["failed_batches"][0]["retry_after"] == "30"


@pytest.mark.parametrize("code", [401, 429])
def test_V05_auth_rate_limit_stops_all_later_batches(code):
    calls = []
    def fetch(ids):
        calls.append(ids)
        return {"status_code": code, "headers": {"Retry-After": "60"}}
    snapshot = build_snapshot("123", [1, 2, 3, 4, 5], fetch, batch_size=2)
    assert len(calls) == 1 and not snapshot["complete"]
    assert len(snapshot["failed_batches"]) == 3


def test_T08_four_sources_parse_JSON_HTML_and_fail_closed():
    sources = []
    for site in ("dicmusic", "open.cd", "orpheus"):
        provider = PTProvider(site, FakeTransport(), "https://fixture.invalid")
        raw = candidate(provider)
        raw["id"] = raw["resource_id"]
        transport = FakeTransport([response({"contract": "musicops.fixture.v1", "results": [raw]})])
        provider.transport = transport
        outcome = provider.search("original synthetic album")
        assert outcome["status"] == "OK" and len(outcome["candidates"]) == 1
        sources += outcome["candidates"]
        blocked = PTProvider(site, SafeHttpTransport(["fixture.invalid"], offline=False), "https://fixture.invalid")
        assert blocked.search("query")["status"] == "POLICY_BLOCKED"
        assert blocked.capabilities()["production_verified"] is False
    html = b'<article data-contract="musicops.fixture.v1" data-resource-id="123" data-title="Demo" data-artists="A|B" data-album="Album" data-seeders="2"></article>'
    parsed = PTProvider("dicmusic", FakeTransport([Response(200, html, {"Content-Type": "text/html"})]), "https://fixture.invalid").search("A Album")
    assert parsed["candidates"][0]["artists"] == ["A", "B"]
    netease = NetEaseProvider(FakeTransport([response({"songs": [{"id": 1, "title": "Demo", "artists": ["A"], "eligibility": {"state": "ELIGIBLE"}}]})]))
    assert netease.search("1")["status"] == "OK"
    assert NetEaseProvider(SafeHttpTransport([], offline=False)).search("query")["status"] == "POLICY_BLOCKED"
    selected = choose_unique_candidate(sources, "release-1")
    assert selected["candidate"]["source"] == "dicmusic"
    assert selected["considered_ids"] == [c["candidate_id"] for c in sources]


@pytest.mark.parametrize("balance,age,eligible,cost", [(None, 0, "ELIGIBLE", 1), (0, 0, "ELIGIBLE", 1), (2, 400, "ELIGIBLE", 1), (2, 0, "UNKNOWN", 1), (2, 0, "INELIGIBLE", 1), (2, 0, "ELIGIBLE", None)])
def test_V10_bad_balance_eligibility_zero_requests(tmp_path, balance, age, eligible, cost):
    store = StateStore(tmp_path / "state.sqlite")
    transport = FakeTransport([asset_response()])
    provider = PTProvider("dicmusic", transport, "https://fixture.invalid")
    when = (datetime.now(timezone.utc) - timedelta(seconds=age)).isoformat()
    outcome = TokenCoordinator(store, provider).fetch(candidate(provider, eligibility=eligible, cost=cost), balance, 3, when)
    assert outcome["status"] == "POLICY_BLOCKED"
    assert transport.requests == []


def test_V11_one_token_concurrent_atomic_reservation(tmp_path):
    path = tmp_path / "state.sqlite"
    StateStore(path)
    observed = utcnow()
    def reserve(resource):
        try:
            return StateStore(path).reserve_token("dicmusic", str(resource), 1, 1, 1, observed)
        except RuntimeError:
            return None
    with ThreadPoolExecutor(max_workers=2) as executor:
        outcomes = list(executor.map(reserve, [101, 102]))
    assert sum(x is not None for x in outcomes) == 1
    assert len(StateStore(path).list("token_reservations")) == 1


def test_V11_separate_batch_budgets_preserve_global_balance(tmp_path):
    store = StateStore(tmp_path / "state.sqlite")
    observed = utcnow()
    first = store.reserve_token("dicmusic", "101", 1, 2, 1, observed, budget_ref="batch-a")
    store.token_result(first["id"], "CONFIRMED")
    second = store.reserve_token("dicmusic", "102", 1, 2, 1, observed, budget_ref="batch-b")
    assert second["budget_ref"] == "batch-b"
    with pytest.raises(RuntimeError):
        store.reserve_token("dicmusic", "103", 1, 2, 1, observed, budget_ref="batch-c")


def test_V12_timeout_freezes_site_never_refetches(tmp_path):
    store = StateStore(tmp_path / "state.sqlite")
    transport = FakeTransport([TimeoutError("synthetic timeout")])
    provider = PTProvider("dicmusic", transport, "https://fixture.invalid")
    coordinator = TokenCoordinator(store, provider)
    chosen = candidate(provider)
    first = coordinator.fetch(chosen, 2, 2, utcnow())
    second = coordinator.fetch(chosen, 2, 2, utcnow())
    other = coordinator.fetch(candidate(provider, "102"), 2, 2, utcnow())
    assert first["status"] == second["status"] == "REQUEST_UNKNOWN"
    assert other["status"] == "POLICY_BLOCKED"
    assert len(transport.requests) == 1
    assert store.list("token_reservations")[0]["state"] == "UNKNOWN"
    provider.transport = FakeTransport([response({"contract": "musicops.fixture.v1", "state": "CONSUMED"})])
    reconciled = coordinator.reconcile(chosen)
    assert reconciled["task"]["state"] == "RECONCILED_CONSUMED"
    assert provider.transport.requests[0]["url"].endswith("/fixture/reconcile")


def test_V12_success_crash_after_persist_recovered_without_second_request(tmp_path):
    store = StateStore(tmp_path / "state.sqlite")
    transport = FakeTransport([asset_response()])
    provider = PTProvider("dicmusic", transport, "https://fixture.invalid")
    original = store.token_result
    def interrupted(id, state, evidence=None):
        if state == "CONFIRMED":
            raise RuntimeError("synthetic database interruption")
        return original(id, state, evidence)
    store.token_result = interrupted
    coordinator = TokenCoordinator(store, provider)
    chosen = candidate(provider)
    with pytest.raises(RuntimeError):
        coordinator.fetch(chosen, 2, 2, utcnow())
    store.token_result = original
    recovered = coordinator.fetch(chosen, 2, 2, utcnow())
    assert recovered["status"] == "READY" and recovered["recovered"]
    assert len(transport.requests) == 1
    assert store.list("token_reservations")[0]["state"] == "CONFIRMED"


def test_V13_legal_torrent_no_free_evidence_cannot_add_or_start(tmp_path):
    store = StateStore(tmp_path / "state.sqlite")
    transport = FakeTransport([asset_response(confirmed=False)])
    provider = PTProvider("orpheus", transport, "https://fixture.invalid")
    outcome = TokenCoordinator(store, provider).fetch(candidate(provider), 2, 2, utcnow())
    assert outcome["status"] == "POLICY_BLOCKED" and outcome["metainfo"]["valid"]
    client_transport = FakeTransport()
    qbt = QbtClient(client_transport, "https://qbt.invalid", tmp_path / "source", store=store)
    assert qbt.add_stopped(torrent()[0], "task-1", outcome["free_evidence"])["status"] == "POLICY_BLOCKED"
    assert client_transport.requests == []
    assert qbt.add_stopped(torrent()[0], "task-1", {"confirmed": True, "infohash": None})["status"] == "POLICY_BLOCKED"
    # Bad site confirmation blocks later resources as well, without new requests.
    assert TokenCoordinator(store, provider).fetch(candidate(provider, "102"), 2, 2, utcnow())["status"] == "POLICY_BLOCKED"
    assert len(transport.requests) == 1


@pytest.mark.parametrize("reply", [Response(200, b"<html>login required</html>"), Response(302, b"", {"Location": "/download?id=101"}), Response(403, b"<html>risk challenge</html>")])
def test_V14_login_risk_redirect_stops_token_request(tmp_path, reply):
    store = StateStore(tmp_path / "state.sqlite")
    transport = FakeTransport([reply])
    provider = PTProvider("dicmusic", transport, "https://fixture.invalid")
    outcome = TokenCoordinator(store, provider).fetch(candidate(provider), 1, 1, utcnow())
    assert outcome["status"] in ("ACCESS_BLOCKED", "AUTH_FAILED")
    assert len(transport.requests) == 1 and transport.requests[0]["params"]["usetoken"] == "1"
    assert store.list("token_reservations")[0]["state"] == "CONSUMED"


@pytest.mark.parametrize("kind", ["v1", "v2", "hybrid"])
def test_V15_v1_v2_hybrid_hashes_and_files(kind):
    data, info = torrent(kind)
    parsed = parse_metainfo(data)
    assert parsed["type"] == kind and parsed["file_list"][0]["path"] == "sample.wav"
    encoded = bencode(info)
    assert parsed["infohash_v1"] == (hashlib.sha1(encoded).hexdigest() if kind != "v2" else None)
    assert parsed["infohash_v2"] == (hashlib.sha256(encoded).hexdigest() if kind != "v1" else None)


def test_V15_noncanonical_invalid_layer_and_paths_blocked():
    bad = [b"d", b"di01e1:xe", b"d4:infode4:infodee", torrent()[0] + b"extra"]
    for data in bad:
        with pytest.raises(ValueError):
            parse_metainfo(data)
    for name in ("..", "CON", "a/b", "bad."):
        with pytest.raises(ValueError):
            parse_metainfo(torrent(name=name)[0])
    leaf_a = hashlib.sha256(b"a" * 16384).digest()
    leaf_b = hashlib.sha256(b"b").digest()
    root = hashlib.sha256(leaf_a + leaf_b).digest()
    info = {b"meta version": 2, b"name": b"large", b"piece length": 16384,
            b"file tree": {b"large.wav": {b"": {b"length": 16385, b"pieces root": root}}}}
    good = bencode({b"info": info, b"piece layers": {root: leaf_a + leaf_b}})
    assert parse_metainfo(good)["valid"]
    with pytest.raises(ValueError):
        parse_metainfo(bencode({b"info": info, b"piece layers": {root: leaf_b + leaf_a}}))


def qbt_row(tmp_path, meta, task_id="task-1", version="5.2.2", state=None):
    path = tmp_path / "source" / task_id
    return {"hash": meta["infohash_v1"] or meta["infohash_v2"][:40], "state": state or ("stoppedDL" if version.startswith("5") else "pausedDL"),
            "tags": "musicops-" + task_id, "category": "musicops", "auto_tmm": False,
            "save_path": str(path), "download_path": str(path / ".incomplete"), "content_path": str(path / "sample.wav"), "progress": 0}


def free(meta):
    return {"confirmed": True, "infohash": meta["infohash_v1"] or meta["infohash_v2"], "kind": "fixture_site_confirmation"}


@pytest.mark.parametrize("version,reply", [("4.6.7", Response(200, b"Ok.")), ("5.2.2", response({"added_torrent_ids": [], "pending_count": 1}, 202)), ("5.2.2", TimeoutError("synthetic add timeout"))])
def test_V16_add_new_old_202_timeout_recovers_by_query(tmp_path, version, reply):
    store = StateStore(tmp_path / "state.sqlite")
    data, _ = torrent()
    meta = parse_metainfo(data)
    row = qbt_row(tmp_path, meta, version=version)
    transport = FakeTransport([response([]), reply, response([row])])
    qbt = QbtClient(transport, "https://qbt.invalid", tmp_path / "source", version=version, store=store)
    outcome = qbt.add_stopped(data, "task-1", free(meta))
    assert outcome["status"] == "STOPPED"
    add = transport.requests[1]
    param = "paused" if version.startswith("4") else "stopped"
    assert add["data"][param] == "true" and add["data"]["autoTMM"] == "false"
    assert "urls" not in add["data"] and "torrents" in add["files"]
    assert "savepath" in add["data"]
    assert add["data"]["useDownloadPath"] == "true"
    assert Path(add["data"]["downloadPath"]).is_relative_to(tmp_path / "source" / "task-1")
    restarted = QbtClient(FakeTransport([response([row])]), "https://qbt.invalid", tmp_path / "source", version=version, store=store)
    assert restarted.add_stopped(data, "task-1", free(meta))["status"] == "STOPPED"
    assert all(r["method"] == "GET" for r in restarted.transport.requests)


def test_V16_async_pending_no_second_add(tmp_path):
    data, _ = torrent()
    meta = parse_metainfo(data)
    transport = FakeTransport([response([]), Response(202), response([]), response([qbt_row(tmp_path, meta)])])
    qbt = QbtClient(transport, "https://qbt.invalid", tmp_path / "source", store=StateStore(tmp_path / "db.sqlite"))
    assert qbt.add_stopped(data, "task-1", free(meta))["status"] == "ADD_UNKNOWN"
    assert qbt.add_stopped(data, "task-1", free(meta))["status"] == "STOPPED"
    assert sum(r["url"].endswith("/torrents/add") for r in transport.requests) == 1


def test_V17_existing_foreign_torrent_and_autoTMM_path_block(tmp_path):
    data, _ = torrent()
    meta = parse_metainfo(data)
    row = qbt_row(tmp_path, meta)
    foreign = FakeTransport([response([row])])
    qbt = QbtClient(foreign, "https://qbt.invalid", tmp_path / "source")
    assert qbt.add_stopped(data, "task-1", free(meta))["status"] == "POLICY_BLOCKED"
    assert len(foreign.requests) == 1
    for changed in [{"auto_tmm": True}, {"save_path": str(tmp_path / "other")}, {"download_path": str(tmp_path / "library")}, {"state": "downloading"}]:
        transport = FakeTransport([response([]), Response(200, b"Ok."), response([{**row, **changed}])])
        qbt = QbtClient(transport, "https://qbt.invalid", tmp_path / "source")
        assert qbt.add_stopped(data, "task-1", free(meta))["status"] == "POLICY_BLOCKED"
        assert all(not r["url"].endswith("/start") for r in transport.requests)


@pytest.mark.parametrize("state", ["checkingUP", "checkingDL", "moving", "error", "missingFiles", "unknown"])
def test_V18_progress_one_checking_moving_not_complete(tmp_path, state):
    data, _ = torrent()
    meta = parse_metainfo(data)
    row = qbt_row(tmp_path, meta)
    transport = FakeTransport([response([]), Response(200, b"Ok."), response([row]), response([{**row, "progress": 1, "state": state}])])
    qbt = QbtClient(transport, "https://qbt.invalid", tmp_path / "source")
    assert qbt.add_stopped(data, "task-1", free(meta))["status"] == "STOPPED"
    assert qbt.poll("task-1")["complete"] is False
    assert len(transport.requests) == 4


def test_V18_partial_or_missing_expected_track_not_album_complete(tmp_path):
    data, _ = torrent()
    meta = parse_metainfo(data)
    row = qbt_row(tmp_path, meta)
    complete_row = {**row, "progress": 1, "state": "uploading"}
    transport = FakeTransport([response([]), Response(200), response([row]), response([complete_row]),
                               response([{"name": "sample.wav", "priority": 0, "progress": 1}]),
                               response([complete_row]), response([{"name": "sample.wav", "priority": 1, "progress": 1}])])
    qbt = QbtClient(transport, "https://qbt.invalid", tmp_path / "source")
    qbt.add_stopped(data, "task-1", free(meta))
    assert qbt.poll("task-1")["status"] == "PARTIAL_DOWNLOAD"
    final = qbt.poll("task-1")
    assert final["status"] == "REQUIRES_MEDIA_VALIDATION" and final["validated"] is False


def synthetic_wav():
    stream = io.BytesIO()
    with wave.open(stream, "wb") as wav:
        wav.setparams((1, 2, 8000, 0, "NONE", "not compressed"))
        wav.writeframes(b"\x00\x00" * 800)
    return stream.getvalue()


def make_ncm(payload, metadata=None):
    """Generate a documented synthetic container, not a real music sample."""
    def encrypt(value, key):
        padder = PKCS7(128).padder()
        padded = padder.update(value) + padder.finalize()
        encryptor = Cipher(algorithms.AES(key), modes.ECB()).encryptor()
        return encryptor.update(padded) + encryptor.finalize()
    content_key = b"offline-synthetic-key-only"
    encrypted_key = bytes(x ^ 0x64 for x in encrypt(b"neteasecloudmusic" + content_key, CORE_KEY))
    metadata = metadata or {"format": "wav", "musicName": "Synthetic", "artist": [["Fixture Artist", 1]], "unknown_field": [1, 2]}
    encrypted_metadata = encrypt(b"music:" + json.dumps(metadata).encode(), META_KEY)
    encoded_metadata = bytes(x ^ 0x63 for x in b"163 key(Don't modify):" + base64.b64encode(encrypted_metadata))
    # Independent KSA and direct per-position encryption for fixture construction.
    state = bytearray(range(256))
    previous = 0
    for index in range(256):
        previous = (previous + state[index] + content_key[index % len(content_key)]) % 256
        state[index], state[previous] = state[previous], state[index]
    encrypted = bytearray()
    for position, value in enumerate(payload, start=1):
        slot = position % 256
        other = (state[slot] + slot) % 256
        mask = state[(state[slot] + state[other]) % 256]
        encrypted.append(value ^ mask)
    return b"CTENFDAM\x00\x00" + struct.pack("<I", len(encrypted_key)) + encrypted_key + struct.pack("<I", len(encoded_metadata)) + encoded_metadata + bytes(9) + struct.pack("<I", 0) + bytes(encrypted)


def test_V19_normal_NCM_unknown_tag_failure_dispatch_preserves_original(tmp_path):
    wav = synthetic_wav()
    assert detect_media(wav) == "wav" and detect_media(make_ncm(wav)) == "ncm" and detect_media(b"unknown") == "unknown"
    source = tmp_path / "sample.ncm"
    data = make_ncm(wav)
    source.write_bytes(data)
    result = decrypt_ncm(source, tmp_path / "prepared.wav")
    assert result["status"] == "REQUIRES_VALIDATION" and result["success"] is False
    assert (tmp_path / "prepared.wav").read_bytes() == wav and source.read_bytes() == data
    assert result["metadata"]["unknown_field"] == [1, 2]
    assert decrypt_ncm(source, tmp_path / "prepared.wav")["status"] == "CONFLICT"
    bad = tmp_path / "bad.ncm"
    bad.write_bytes(make_ncm(b"not audio", {"format": "flac"}))
    assert decrypt_ncm(bad, tmp_path / "unknown.flac")["status"] == "DECRYPT_FAILED"
    assert not (tmp_path / "unknown.flac").exists()
    def failing_tag(path, metadata):
        raise RuntimeError("synthetic tag failure")
    failed = decrypt_ncm(source, tmp_path / "tag.wav", tag_writer=failing_tag)
    assert failed["status"] == "TAG_FAILED" and not (tmp_path / "tag.wav").exists()
    assert source.read_bytes() == data
    assert decrypt_ncm(source, tmp_path / "wrong.flac")["status"] == "DECRYPT_FAILED"


def test_V19_ncm_stream_blocks_use_continuous_key_position(tmp_path):
    stream = io.BytesIO()
    with wave.open(stream, "wb") as wav:
        wav.setparams((1, 2, 8000, 0, "NONE", "not compressed"))
        wav.writeframes(b"\x01\x00\x02\x00" * 50000)
    payload = stream.getvalue()
    source = tmp_path / "long.ncm"
    source.write_bytes(make_ncm(payload))
    result = decrypt_ncm(source, tmp_path / "long.wav")
    assert result["status"] == "REQUIRES_VALIDATION"
    assert (tmp_path / "long.wav").read_bytes() == payload


def test_V19_golden_synthetic_NCM_fixture(tmp_path):
    fixtures = Path(__file__).parent / "fixtures" / "providers"
    source = fixtures / "synthetic.ncm"
    manifest = json.loads((fixtures / "synthetic-manifest.json").read_text(encoding="utf-8"))
    assert hashlib.sha256(source.read_bytes()).hexdigest() == manifest["ncm_sha256"]
    # The checkout may be reached through a Windows workspace junction. Keep
    # production reparse protection intact and stage the verified test bytes.
    staged = tmp_path / 'synthetic.ncm'
    staged.write_bytes(source.read_bytes())
    result = decrypt_ncm(staged, tmp_path / "golden.wav")
    assert result["status"] == "REQUIRES_VALIDATION"
    assert result["audio_payload_sha256"] == manifest["audio_sha256"]
    assert result["metadata"]["musicName"] == "Synthetic"


def test_V19_netease_unknown_entitlement_and_audio_requires_validation(tmp_path):
    provider = NetEaseProvider(FakeTransport([Response(200, synthetic_wav())]))
    chosen = provider.parse_candidate({"id": 1, "title": "Fixture", "artists": ["A"], "eligibility": {"state": "UNKNOWN"}})
    assert provider.fetch_asset(chosen, tmp_path / "a.wav")["status"] == "POLICY_BLOCKED"
    assert provider.transport.requests == []
    chosen["eligibility"]["state"] = "ELIGIBLE"
    assert provider.fetch_asset(chosen, tmp_path / "a.wav")["status"] == "REQUIRES_VALIDATION"
    provider.transport = FakeTransport([Response(200, b"<html>error</html>")])
    assert provider.fetch_asset(chosen, tmp_path / "bad.wav")["status"] == "PARSE_ERROR"
    assert not (tmp_path / "bad.wav").exists()


def test_V40_local_threshold_cancel_rollback_never_stop_or_delete(tmp_path):
    transport = FakeTransport()
    qbt = QbtClient(transport, "https://qbt.invalid", tmp_path / "source")
    for event in ("ratio_threshold", "time_threshold", "cancel", "rollback", "pause_pipeline"):
        outcome = qbt.pipeline_event("task-1", event)
        assert outcome["seeding"] == "preserved" and outcome["client_operations"] == []
    assert transport.requests == []
    assert not hasattr(qbt, "delete") and not hasattr(qbt, "stop_seeding")


def test_T08_T11_gui_workflow_runs_actual_fake_adapters_once(tmp_path):
    root = tmp_path / "source"
    root.mkdir()
    # Offline fixture contents only; extension comes from content in the real
    # demonstration generated by ffmpeg. This test checks workflow hash/ownership.
    source = root / "02 星河.flac"
    source.write_bytes(b"fLaC" + bytes(256))
    config = {"offline": True, "paths": {"source": str(root)}}
    store = StateStore(tmp_path / "state.sqlite")
    searched = offline_search(store, config, "星河")
    assert len(searched["candidates"]) == 4 and searched["selected"] is None
    for chosen in searched["candidates"]:
        outcome = run_offline_download(store, config, chosen)
        assert outcome["status"] == "REQUIRES_VALIDATION"
        assert Path(outcome["source_path"]).read_bytes() == source.read_bytes()
        resumed = run_offline_download(store, config, chosen)
        assert resumed["recovered"] is True
    assert len(store.list("token_reservations")) == 2
    assert all(x["state"] == "CONFIRMED" for x in store.list("token_reservations"))


def test_T09_T10_config_budget_category_prefix_autoTMM_are_respected(tmp_path):
    root = tmp_path / "source"
    root.mkdir()
    (root / "02 星河.flac").write_bytes(b"fLaC" + bytes(256))
    store = StateStore(tmp_path / "db.sqlite")
    config = {"offline": True, "paths": {"source": str(root)},
              "token_budget": {"dicmusic": 0, "orpheus": 3, "safety_margin": 1},
              "qbt": {"category": "离线分类", "tag_prefix": "custom:", "auto_tmm": False}}
    searched = offline_search(store, config)
    dic = next(x for x in searched["candidates"] if x["source"] == "dicmusic")
    assert run_offline_download(store, config, dic)["status"] == "POLICY_BLOCKED"
    assert store.list("token_reservations") == []
    orpheus = next(x for x in searched["candidates"] if x["source"] == "orpheus")
    outcome = run_offline_download(store, config, orpheus)
    assert outcome["status"] == "REQUIRES_VALIDATION"
    add = next(r for r in outcome["qbt_requests"] if r["endpoint"][-1] == "add")
    assert add["data"]["category"] == "离线分类" and add["data"]["tags"].startswith("custom:")
    configured = QbtClient(FakeTransport(), "https://qbt.invalid", root, auto_tmm=True)
    assert configured.capabilities()["status"] == "POLICY_BLOCKED"
    assert configured.transport.requests == []
    config["qbt"]["auto_tmm"] = True
    config["token_budget"]["dicmusic"] = 1
    assert run_offline_download(store, config, dic)["status"] == "POLICY_BLOCKED"
    assert len(store.list("token_reservations")) == 1  # Only prior orpheus result.
