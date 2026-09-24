"""scan -> collect -> merge -> index -> serve, on both agents' formats."""

import time
import uuid

import lib


def test():
    with lib.Lab() as lab:
        claude_id = str(uuid.uuid4())
        codex_id = str(uuid.uuid4())
        claude_path = lab.claude / "-home-me-proj" / f"{claude_id}.jsonl"
        codex_path = lab.codex / "2026" / "01" / "02" / f"rollout-2026-01-02T11-00-00-{codex_id}.jsonl"

        claude_path.write_text(lib.claude_session(claude_id, [
            ("почини сборку ядра", "смотрю molot exec", "ls /ix/store | grep kernel", "kernel-7-2"),
            ("а теперь loki", "кольцо нездорово", "curl loki/ring", "UNHEALTHY lab1"),
        ]))
        # A partial last line must wait for its newline.
        with claude_path.open("a") as f:
            f.write('{"type":"user","sessionId":"' + claude_id + '","message":{"role":"user","content":"unfinished')
        codex_path.write_text(lib.codex_session(codex_id, [
            ("rewrite the parser", "done, tests pass", "go test ./...", "ok"),
        ]))
        # Not sessions: scratch under a session directory, a memory file.
        (lab.claude / "-home-me-proj" / claude_id).mkdir()
        (lab.claude / "-home-me-proj" / claude_id / "agent-x.jsonl").write_text("{}\n")
        (lab.claude / "-home-me-proj" / "notes.jsonl").write_text("{}\n")

        result = lab.scan()
        assert result.stderr.count("scan: shipped") == 2, result.stderr
        queue = lab.keys("queue")
        assert len(queue) == 2, queue
        assert all(k.startswith("queue/") and "." in k for k in queue), queue
        assert (claude_path.parent / f"{claude_id}.jsonl.latest").read_text().strip() == str(
            len(lib.claude_session(claude_id, [
                ("почини сборку ядра", "смотрю molot exec", "ls /ix/store | grep kernel", "kernel-7-2"),
                ("а теперь loki", "кольцо нездорово", "curl loki/ring", "UNHEALTHY lab1"),
            ]).encode()))

        # Nothing new: nothing shipped, and a resend of the same portion lands on the same key.
        assert "shipped" not in lab.scan().stderr
        (claude_path.parent / f"{claude_id}.jsonl.latest").write_text("0\n")
        assert lab.scan().stderr.count("scan: shipped") == 1
        assert lab.keys("queue") == queue

        # The unfinished line completes, plus one more turn: a second portion.
        with claude_path.open("a") as f:
            f.write('"}}\n')
            f.write(lib.claude_session(claude_id, [
                ("что с hostname", "заглушка box shim", "readlink /bin/hostname", "box-shim"),
            ], start=3))
        assert lab.scan().stderr.count("scan: shipped") == 1
        assert len(lab.keys("queue")) == 3

        lab.merge()
        assert lab.keys("queue") == []
        sessions = lab.keys("sessions")
        assert sessions == sorted([f"sessions/{claude_id}", f"sessions/{codex_id}"]), sessions

        lab.index()
        assert lab.keys("index") == ["index/logovo.sqlite.zst"]

        with lab.serve() as serve:
            status = serve.json("/v1/status")
            assert status["sessions"] == "2", status
            docs_before = status["docs"]

            hits = lib.search(serve, "loki")
            assert hits and hits[0]["session"] == claude_id, hits
            assert "[loki]" in hits[0]["snippet"], hits[0]
            assert hits[0]["agent"] == "claude" and hits[0]["host"] == "e2e", hits[0]

            # Thinking, encrypted reasoning and Codex's tagged context are not indexed.
            assert lib.search(serve, "musings") == []
            assert lib.search(serve, "gibberish") == []
            assert lib.search(serve, "environment_context") == []

            # Tool calls and results are, and the shell command is the body.
            hits = lib.search(serve, "kernel")
            bodies = {h["role"]: h["snippet"] for h in hits}
            assert "tool" in bodies and "assistant" in bodies, hits

            hits = lib.search(serve, "parser")
            assert hits and hits[0]["session"] == codex_id and hits[0]["agent"] == "codex", hits

            # The transcript keeps the turns in order across portions, once each.
            doc = serve.json(f"/v1/sessions/{claude_id}?format=json")
            assert doc["session"]["turns"] == 13, doc["session"]
            assert doc["session"]["title"] == "почини сборку ядра", doc["session"]
            assert doc["session"]["cwd"] == "/home/me/proj"
            users = [d["body"] for d in doc["docs"] if d["role"] == "user"]
            assert users == ["почини сборку ядра", "а теперь loki", "unfinished", "что с hostname"], users
            assert doc["docs"][2]["body"] == "▶ Bash: ls /ix/store | grep kernel", doc["docs"][2]
            assert doc["docs"][3]["body"] == "◀ kernel-7-2", doc["docs"][3]

            status, text = serve.get(f"/v1/sessions/{claude_id}?from=4&to=5")
            assert status == 200 and "[4]" in text and "[6]" not in text, text
            status, _ = serve.get(f"/v1/sessions/{uuid.uuid4()}")
            assert status == 404
            status, _ = serve.get("/v1/search")
            assert status == 400

            codex = serve.json(f"/v1/sessions/{codex_id}?format=json")
            assert [d["body"] for d in codex["docs"]] == [
                "rewrite the parser", "▶ shell: go test ./...", "◀ ok", "done, tests pass",
            ], codex["docs"]
            assert codex["session"]["cwd"] == "/home/me/codex-proj"

            # A rebuilt index is picked up by the running server.
            codex_path.write_text(lib.codex_session(codex_id, [
                ("rewrite the parser", "done, tests pass", "go test ./...", "ok"),
                ("now the lexer", "lexer rewritten", "go vet", "clean"),
            ]))
            lab.scan()
            lab.merge()
            lab.index()
            deadline = time.time() + 10
            while time.time() < deadline and serve.json("/v1/status")["docs"] == docs_before:
                time.sleep(0.2)
            assert lib.search(serve, "lexer"), serve.json("/v1/status")

            api = f"http://127.0.0.1:{serve.port}"
            out = lib.run("search", "-api", api, "loki").stdout
            assert claude_id in out and "[loki]" in out, out
            out = lib.run("show", "-api", api, claude_id, "-from", "0", "-to", "1").stdout
            assert "[0]" in out and "[2]" not in out, out

            # One name for everything: the page, the API and uploads go through web.
            with lab.web(serve) as web:
                status, page = web.get("/")
                assert status == 200 and "<title>logovo</title>" in page, page[:200]
                assert lib.search(web, "lexer"), "search through web"
                out = lib.run("show", "-api", f"http://127.0.0.1:{web.port}", codex_id).stdout
                assert "lexer rewritten" in out, out
                (lab.claude / "-home-me-proj" / f"{claude_id}.jsonl.latest").write_text("0\n")
                result = lib.run("scan", "-once", "-url", f"http://127.0.0.1:{web.port}/v1/portions", "-host", "e2e",
                                 "-root", f"claude={lab.claude}", "-root", f"codex={lab.codex}")
                assert result.stderr.count("scan: shipped") == 1, result.stderr
                assert len(lab.keys("queue")) == 1, lab.keys("queue")


if __name__ == "__main__":
    test()
    print("ok")
