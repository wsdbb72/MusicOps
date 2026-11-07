"""Executable integration tests; no user profile or library is opened."""
import os
from pathlib import Path
import subprocess
import sys

import pytest

from musicops.state.native_client import NativeDataClient, NativeDataError


@pytest.fixture
def executable():
    default = Path(__file__).resolve().parents[1] / "work/native/musicops-data.exe"
    binary = Path(os.environ.get("MUSICOPS_TEST_DATA_BINARY", default))
    if not binary.is_file():
        pytest.skip("build the native data engine before executable integration tests")
    return binary.resolve()


def spec(task_id="a"):
    return {"task_id": task_id, "command": "identify_metadata", "lane": "identify", "priority": 1, "created_ns": 1000}


def test_owned_lifecycle_replay_checkpoint_and_reopen(executable, tmp_path):
    directory, backups = tmp_path / "native", tmp_path / "backups"
    with NativeDataClient(executable, directory, backups) as client:
        first = client.enqueue("enqueue", spec(), b"arguments")
        assert first["replayed"] is False
        assert client.enqueue("enqueue", spec(), b"arguments")["replayed"] is True
        with pytest.raises(NativeDataError, match="REQUEST_ID_CONFLICT"):
            client.enqueue("enqueue", spec(), b"different")
        with pytest.raises(NativeDataError, match="TASK_CHANGED"):
            client.control("rejected", "a", "retry", 2000)
        claim = client.claim("claim", "identify", "unique-boot-owner", 2100)
        token = client.token(claim["task"])
        client.begin("begin", token, 2200)
        client.finish("finish", token, "completed", 2300, payload=b"result")
        assert client.request_status("enqueue")["state"] == "COMMITTED"
        assert client.request_status("rejected")["state"] == "REJECTED"
        assert client.task("a")["state"] == "completed"
        client.checkpoint("verified-copy")
        process = client.process
    assert process.poll() == 0
    client.close()
    with NativeDataClient(executable, directory, backups) as reopened:
        assert reopened.task("a")["state"] == "completed"
        assert reopened.data("a", "result") == b"result"
        assert reopened.request_status("rejected")["state"] == "REJECTED"
    with NativeDataClient(executable, backups / "verified-copy", tmp_path / "other-backups") as checkpoint:
        assert checkpoint.request_status("enqueue")["state"] == "COMMITTED"
        assert checkpoint.counts()["states"]["completed"] == 1


def test_parent_exit_closes_engine(executable, tmp_path):
    code = """
import sys
from pathlib import Path
from musicops.state.native_client import NativeDataClient
c = NativeDataClient(Path(sys.argv[1]), Path(sys.argv[2]), Path(sys.argv[3]))
print(c.process.pid, flush=True)
# Interpreter exit closes the parent pipe without explicitly closing the client.
"""
    environment = os.environ.copy()
    environment["PYTHONPATH"] = str(Path(__file__).resolve().parents[1] / "src")
    parent = subprocess.run([sys.executable, "-c", code, str(executable), str(tmp_path / "data"),
                             str(tmp_path / "backups")], env=environment,
                            capture_output=True, timeout=30)
    assert parent.returncode == 0, parent.stderr.decode(errors="replace")
    # Reopening also verifies that the previous process released its Pebble lock.
    import time
    for attempt in range(20):
        try:
            with NativeDataClient(executable, tmp_path / "data", tmp_path / "backups"):
                return
        except NativeDataError:
            if attempt == 19:
                raise
            time.sleep(0.1)


def test_foreign_directory_unchanged_on_failed_start(executable, tmp_path):
    foreign = tmp_path / "existing"
    foreign.mkdir()
    marker = foreign / "important.txt"
    marker.write_text("preserve", encoding="utf-8")
    with pytest.raises(NativeDataError, match="未能启动"):
        NativeDataClient(executable, foreign, tmp_path / "backups")
    assert marker.read_text(encoding="utf-8") == "preserve"
    assert list(foreign.iterdir()) == [marker]


def test_typed_queue_controls_replays_and_preserves_attempt_results(executable, tmp_path):
    with NativeDataClient(executable, tmp_path / "data", tmp_path / "backups") as client:
        raw = b"large config snapshot " * 50000
        client.enqueue("enqueue", spec(), raw)
        assert len(str(client.list_tasks("identify", "queued"))) < 1024
        claim = client.claim("claim", "identify", "unique-worker-boot", 2000)
        token = client.token(claim["task"])
        client.begin("begin", token, 2100)
        client.progress(token, 1, 20, "copying", 2200)
        client.control("pause", "a", "pause", 2300)
        reply = client.progress(token, 1, 20, "copying", 2200)
        assert reply["decision"] == "pause" and reply["task"]["state"] == "paused"
        client.control("cancel", "a", "cancel", 2400)
        assert client.progress(token, 2, 25, "copying", 2500)["decision"] == "cancel"
        client.finish("cancelled", token, "cancelled", 2600, payload=b"original result")
        client.control("retry", "a", "retry", 2700)
        assert client.data("a", "result", fence=token["fence"]) == b"original result"
        assert client.data("a") == raw
        second = client.claim("next-claim", "identify", "unique-worker-boot", 2800)
        new_token = client.token(second["task"])
        assert new_token["fence"] > token["fence"]
        client.begin("next-begin", new_token, 2900)
        with pytest.raises(NativeDataError, match="TASK_CHANGED"):
            client.finish("stale", token, "completed", 3000)
        client.finish("done", new_token, "completed", 3100)
        replay = client.claim("claim", "identify", "unique-worker-boot", 3200)
        assert replay["replayed"] is True
        assert client.task("a")["state"] == "completed"
        assert client.recent()["tasks"][0]["task_id"] == "a"
        assert client.counts()["states"]["completed"] == 1


def test_completion_and_search_enqueue_share_one_durable_response(executable, tmp_path):
    with NativeDataClient(executable, tmp_path / "data", tmp_path / "backups") as client:
        client.enqueue("enqueue", spec(), b"synthetic input")
        claim = client.claim("claim", "identify", "unique-boot", 2000)
        token = client.token(claim["task"])
        client.begin("begin", token, 2100)
        child = {"spec": {**spec("search"), "command": "search_sites"}, "payload": b"observed identity"}
        result = client.finish("handoff", token, "completed", 2200, payload=b"identity evidence", followups=[child])
        assert result["followups"][0]["parent_id"] == "a"
        assert client.task("a")["state"] == "completed"
        assert client.children("a")["tasks"][0]["task_id"] == "search"
        assert client.data("search") == b"observed identity"
        replay = client.finish("handoff", token, "completed", 2300, payload=b"identity evidence", followups=[child])
        assert replay["replayed"] is True
        assert client.counts()["states"]["queued"] == 1


def test_workflow_stage_evidence_and_successor_share_one_response(executable, tmp_path):
    evidence = {"evidence_id": "evidence-1", "kind": "api", "source": "musicbrainz", "subject_id": "release1",
                "tracklist_verified": True, "identity_confirmed": True, "payload": b"synthetic tracklist"}
    with NativeDataClient(executable, tmp_path / "data", tmp_path / "backups") as client:
        started = client.workflow_start(
            "start", job_id="album1", batch_id="batch1", scene="playlist", stage="identifying",
            source_track_ids=["track-1", "track-2"], library_file_count=9, matched_count=1,
            payload=b"synthetic playlist", now_ns=1000)
        assert started["job"]["stage"] == "identifying"
        assert started["job"]["source_track_count"] == 2
        assert started["job"]["library_file_count"] == 9
        claim = client.claim("claim", "identify", "transport-owner", 2000)
        token = client.token(claim["task"])
        client.begin("begin", token, 2100)
        with pytest.raises(NativeDataError, match="WORKFLOW_OWNED"):
            client.finish("bare", token, "completed", 2200)
        done = client.workflow_complete(
            "handoff", token, job_id="album1", edge="queue_search", task_state="completed",
            message="识别完成", result=b"identity", evidence=[evidence], now_ns=3000)
        assert done["replayed"] is False
        assert done["job"]["stage"] == "completed"
        assert done["children"][0]["stage"] == "searching"
        assert done["children"][0]["source_track_count"] == 2
        assert done["children"][0]["library_file_count"] == 9
        assert client.workflow_evidence("evidence-1") == b"synthetic tracklist"
        replay = client.workflow_complete(
            "handoff", token, job_id="album1", edge="queue_search", task_state="completed",
            message="识别完成", result=b"identity", evidence=[evidence], now_ns=3000)
        assert replay["replayed"] is True
        assert client.counts()["states"]["queued"] == 1
        assert client.task("search:album1")["command"] == "search_release"
        inspected = client.workflow_inspect("album1")
        assert inspected["job"]["stage"] == "completed"
        assert inspected["children"][0]["job_id"] == "search:album1"
        assert inspected["task"]["state"] == "completed"
        with pytest.raises(NativeDataError, match="REQUEST_ID_CONFLICT"):
            changed = dict(evidence, payload=b"different tracklist")
            client.workflow_complete(
                "handoff", token, job_id="album1", edge="queue_search", task_state="completed",
                result=b"identity", evidence=[changed], now_ns=3000)
