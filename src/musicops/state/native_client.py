"""Private transport to the bundled engine; no SQL translation or fallback.

Not wired into production until repository migration is verified. A lost write
response is ambiguous: retain its request ID and query/replay that same intent.
"""
from __future__ import annotations

import base64
import json
import os
from pathlib import Path
import queue
import secrets
import subprocess
import threading
import urllib.error
import urllib.parse
import urllib.request


class NativeDataError(RuntimeError):
    pass


class NativeDataUncertain(NativeDataError):
    """Transport failed; this does not prove that a write failed."""


class NativeDataClient:
    def __init__(self, executable: Path, directory: Path, backups: Path, *, startup_timeout=20):
        self._token = secrets.token_urlsafe(48)
        self._closed = False
        self._opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        environment = os.environ.copy()
        environment["MUSICOPS_DATA_TOKEN"] = self._token
        self.process = subprocess.Popen(
            [str(executable), "--parent-pipe", "--state-dir", str(directory),
             "--backup-dir", str(backups)], env=environment, stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
            creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0),
        )
        from musicops.performance import configure_child
        configure_child(self.process)
        readiness = queue.Queue(maxsize=1)

        def read_ready():
            try:
                readiness.put(self.process.stdout.readline(4097))
            except Exception:
                readiness.put(b"")

        threading.Thread(target=read_ready, daemon=True, name="musicops-data-ready").start()
        try:
            line = readiness.get(timeout=startup_timeout)
            if len(line) > 4096:
                raise ValueError("oversized readiness")
            ready = json.loads(line)
            url = urllib.parse.urlsplit(ready["url"])
            if (not ready.get("ready") or ready.get("engine") != "pebble"
                    or ready.get("data_schema") != 2 or ready.get("pid") != self.process.pid
                    or url.scheme != "http" or url.hostname != "127.0.0.1"
                    or not url.port or url.path or url.query or url.fragment or url.username):
                raise ValueError("invalid engine readiness")
            self._url = ready["url"]
            self._request("/health", None, timeout=5)
        except Exception:
            self.process.terminate()
            self.process.wait(timeout=10)
            self._close_pipes()
            raise NativeDataError("内置数据引擎未能启动；正式档案未切换") from None

    def _request(self, path, payload, *, timeout):
        if self._closed:
            raise NativeDataError("内置数据连接已关闭")
        body = None if payload is None else json.dumps(payload, allow_nan=False).encode("utf-8")
        request = urllib.request.Request(self._url + path, data=body, headers={
            "Authorization": "Bearer " + self._token,
            "Content-Type": "application/json",
        })
        try:
            with self._opener.open(request, timeout=timeout) as response:
                data = response.read((90 << 20) + 1)
                if len(data) > 90 << 20:
                    raise NativeDataError("内置数据响应超过上限")
                result = json.loads(data)
        except urllib.error.HTTPError as exc:
            try:
                code = json.loads(exc.read(4096)).get("code", "DATA_OPERATION_FAILED")
            except Exception:
                code = "DATA_OPERATION_FAILED"
            finally:
                exc.close()
            raise NativeDataError(str(code)) from None
        except (OSError, ValueError):
            raise NativeDataUncertain("数据请求响应未确认，请查询原 request_id，勿生成新请求重做") from None
        if result.get("ok") is not True:
            raise NativeDataError("无效的内置数据响应")
        return result.get("result", result)

    def _rpc(self, operation, request_id="", *, timeout=30, **params):
        try:
            return self._request("/rpc", {"operation": operation, "request_id": request_id, "params": params}, timeout=timeout)
        except NativeDataUncertain as exc:
            exc.request_id = request_id
            raise

    @staticmethod
    def token(task):
        return {key: task[key] for key in ("task_id", "owner", "fence")}

    def enqueue(self, request_id, spec, payload):
        return self._rpc("task_enqueue", request_id, spec=spec, payload=base64.b64encode(payload).decode("ascii"))

    def task(self, task_id):
        result = self._rpc("task_get", task_id=task_id)
        return result["task"] if result["found"] else None

    def data(self, task_id, part="arguments", *, fence=0):
        result = self._rpc("task_data", task_id=task_id, part=part, fence=fence)
        return base64.b64decode(result["data"], validate=True) if result["found"] else None

    def attempt(self, task_id, fence):
        return self._rpc("task_attempt", task_id=task_id, fence=fence)

    def list_tasks(self, lane, state, *, cursor=None, limit=200):
        return self._rpc("task_list", lane=lane, state=state, after=cursor, limit=limit)

    def recent(self, *, cursor=None, limit=200):
        return self._rpc("task_recent", after=cursor, limit=limit)

    def counts(self):
        return self._rpc("task_counts")

    def claim(self, request_id, lane, owner, now_ns):
        return self._rpc("task_claim", request_id, lane=lane, owner=owner, now_ns=now_ns)

    def begin(self, request_id, token, now_ns):
        return self._rpc("task_begin", request_id, token=token, now_ns=now_ns)

    def progress(self, token, sequence, percent, phase, now_ns, message=""):
        return self._rpc("task_progress", token=token, point={"sequence": sequence, "percent": percent,
                         "phase": phase, "now_ns": now_ns, "message": message})

    def control(self, request_id, task_id, action, now_ns):
        return self._rpc("task_control", request_id, task_id=task_id, action=action, now_ns=now_ns)

    def finish(self, request_id, token, state, now_ns, *, payload=None, error_class="", message="", followups=()):
        completion = {"state": state, "now_ns": now_ns, "error_class": error_class, "message": message}
        if payload is not None:
            completion["payload"] = base64.b64encode(payload).decode("ascii")
        children = [{"spec": item["spec"], "payload": base64.b64encode(item.get("payload", b"")).decode("ascii")}
                    for item in followups]
        return self._rpc("task_finish", request_id, token=token, completion=completion, followups=children)

    def children(self, task_id, *, cursor=None, limit=200):
        return self._rpc("task_children", task_id=task_id, after=cursor, limit=limit)

    def interrupt(self, request_id, token, now_ns, reason):
        return self._rpc("task_interrupt", request_id, token=token, now_ns=now_ns, reason=reason)

    def request_status(self, request_id):
        return self._rpc("task_request", request_id)

    @staticmethod
    def _bytes(value):
        if isinstance(value, (bytes, bytearray)):
            return base64.b64encode(value).decode("ascii")
        return value

    def _evidence(self, items):
        encoded = []
        for item in items or ():
            copied = dict(item)
            copied["payload"] = self._bytes(copied.get("payload", b""))
            encoded.append(copied)
        return encoded

    def workflow_start(self, request_id, **job):
        payload = dict(job)
        payload["payload"] = self._bytes(payload.get("payload", b""))
        payload["evidence"] = self._evidence(payload.get("evidence"))
        return self._rpc("workflow_start", request_id, **payload)

    def workflow_complete(self, request_id, token, **job):
        body = dict(job)
        body["result"] = self._bytes(body.get("result", b""))
        body["evidence"] = self._evidence(body.get("evidence"))
        return self._rpc("workflow_complete", request_id, token=token, input=body)

    def workflow_control(self, request_id, job_id, action, now_ns):
        return self._rpc("workflow_control", request_id, job_id=job_id, action=action, now_ns=now_ns)

    def workflow_job(self, job_id):
        result = self._rpc("workflow_get", job_id=job_id)
        return result["job"] if result["found"] else None

    def workflow_inspect(self, job_id):
        return self._rpc("workflow_inspect", job_id=job_id)

    def workflow_evidence(self, evidence_id):
        result = self._rpc("workflow_evidence_data", evidence_id=evidence_id)
        return base64.b64decode(result["data"], validate=True) if result["found"] else None

    def checkpoint(self, checkpoint_id):
        return self._rpc("checkpoint", checkpoint_id=checkpoint_id, timeout=120)

    def _close_pipes(self):
        for pipe in (self.process.stdin, self.process.stdout):
            if pipe is not None and not pipe.closed:
                pipe.close()

    def close(self, *, timeout=30):
        if self._closed:
            return
        if self.process.stdin is not None and not self.process.stdin.closed:
            self.process.stdin.close()
        try:
            status = self.process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            # Never kill a data process in the middle of a checkpoint/write.
            raise NativeDataError("数据引擎仍在完成已接收操作，暂未关闭") from None
        self._closed = True
        self._close_pipes()
        if status != 0:
            raise NativeDataError("内置数据引擎异常退出，需要检查恢复状态")

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()
