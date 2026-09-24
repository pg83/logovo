"""E2E helpers: a temporary store, fixture sessions, and the daemons."""

import http.client
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.parse
import uuid
from pathlib import Path

LOGOVO = Path(os.environ["LOGOVO_TEST_BINARY"]).resolve()


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def run(*args, check=True, timeout=120, env=None):
    result = subprocess.run([LOGOVO, *args], capture_output=True, text=True, timeout=timeout, env=env)
    if check and result.returncode != 0:
        raise AssertionError(f"{args}: exit {result.returncode}\n{result.stderr}")
    return result


class Daemon:
    def __init__(self, *args, port, path="/healthz"):
        self.port = port
        self.proc = subprocess.Popen([LOGOVO, *args], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                self.get(path)
                return
            except (ConnectionError, OSError):
                if self.proc.poll() is not None:
                    raise AssertionError(f"daemon died: {self.proc.stdout.read()}")
                time.sleep(0.05)
        raise AssertionError("daemon did not come up")

    def get(self, path, timeout=30):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=timeout)
        try:
            conn.request("GET", path)
            resp = conn.getresponse()
            return resp.status, resp.read().decode()
        finally:
            conn.close()

    def json(self, path):
        status, body = self.get(path)
        assert status == 200, (status, body)
        return json.loads(body)

    def stop(self):
        self.proc.terminate()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()
            self.proc.wait()
        return self.proc.stdout.read()

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.stop()


class Lab:
    """A store directory, roots for both agents, a collector."""

    def __init__(self):
        self.dir = Path(tempfile.mkdtemp(prefix="logovo-e2e-"))
        self.store = self.dir / "store"
        self.claude = self.dir / "claude"
        self.codex = self.dir / "codex"
        (self.claude / "-home-me-proj").mkdir(parents=True)
        (self.codex / "2026" / "01" / "02").mkdir(parents=True)
        self.port = free_port()
        self.collect = Daemon("collect", "-listen", f"127.0.0.1:{self.port}", "-store", f"dir:{self.store}", port=self.port)

    @property
    def url(self):
        return f"http://127.0.0.1:{self.port}/v1/portions"

    def scan(self, host="e2e"):
        return run("scan", "-once", "-url", self.url, "-host", host,
                   "-root", f"claude={self.claude}", "-root", f"codex={self.codex}")

    def merge(self):
        return run("merge", "-store", f"dir:{self.store}")

    def index(self):
        return run("index", "-store", f"dir:{self.store}")

    def serve(self):
        port = free_port()
        return Daemon("serve", "-listen", f"127.0.0.1:{port}", "-store", f"dir:{self.store}", "-refresh", "1s", port=port)

    def web(self, serve):
        port = free_port()
        return Daemon("web", "-listen", f"127.0.0.1:{port}", "-api", f"http://127.0.0.1:{serve.port}",
                      "-collect", f"http://127.0.0.1:{self.port}", port=port, path="/")

    def keys(self, prefix):
        root = self.store / prefix
        if not root.exists():
            return []
        return sorted(str(p.relative_to(self.store)) for p in root.rglob("*") if p.is_file())

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.collect.stop()
        shutil.rmtree(self.dir, ignore_errors=True)


def claude_record(kind, session, text, ts, role=None, content=None, cwd="/home/me/proj", **extra):
    rec = {"type": kind, "sessionId": session, "uuid": str(uuid.uuid4()), "timestamp": ts, "cwd": cwd, **extra}
    if role:
        rec["message"] = {"role": role, "content": content if content is not None else text}
    return json.dumps(rec, ensure_ascii=False)


def claude_session(session, turns, start=0):
    """A user/assistant/tool exchange per turn, like Claude Code writes it."""
    lines = []
    for i, (prompt, answer, cmd, result) in enumerate(turns, start):
        ts = f"2026-01-02T10:{i:02d}:00.000Z"
        lines.append(claude_record("user", session, prompt, ts, role="user"))
        lines.append(claude_record("assistant", session, None, ts, role="assistant",
                                   content=[{"type": "thinking", "thinking": "secret musings"},
                                            {"type": "text", "text": answer}]))
        lines.append(claude_record("assistant", session, None, ts, role="assistant",
                                   content=[{"type": "tool_use", "name": "Bash", "input": {"command": cmd}}]))
        lines.append(claude_record("user", session, None, ts, role="user",
                                   content=[{"type": "tool_result", "content": result}]))
    return "\n".join(lines) + "\n"


def codex_session(session, turns, cwd="/home/me/codex-proj"):
    lines = [json.dumps({"timestamp": "2026-01-02T11:00:00.000Z", "type": "session_meta",
                         "payload": {"id": session, "cwd": cwd, "base_instructions": {"text": "long instructions"}}})]
    for i, (prompt, answer, cmd, result) in enumerate(turns):
        ts = f"2026-01-02T11:{i:02d}:00.000Z"
        lines.append(json.dumps({"timestamp": ts, "type": "response_item",
                                 "payload": {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "<environment_context>noise</environment_context>"}]}}))
        lines.append(json.dumps({"timestamp": ts, "type": "response_item",
                                 "payload": {"type": "message", "role": "user", "content": [{"type": "input_text", "text": prompt}]}}, ensure_ascii=False))
        lines.append(json.dumps({"timestamp": ts, "type": "response_item",
                                 "payload": {"type": "reasoning", "summary": [], "encrypted_content": "gibberish"}}))
        lines.append(json.dumps({"timestamp": ts, "type": "response_item",
                                 "payload": {"type": "function_call", "name": "shell", "arguments": json.dumps({"cmd": cmd}), "call_id": "c1"}}))
        lines.append(json.dumps({"timestamp": ts, "type": "response_item",
                                 "payload": {"type": "function_call_output", "call_id": "c1", "output": json.dumps({"output": result})}}))
        lines.append(json.dumps({"timestamp": ts, "type": "response_item",
                                 "payload": {"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": answer}]}}, ensure_ascii=False))
    return "\n".join(lines) + "\n"


def search(daemon, q, **params):
    query = urllib.parse.urlencode({"q": q, "format": "json", **params})
    return daemon.json("/v1/search?" + query)["hits"]
