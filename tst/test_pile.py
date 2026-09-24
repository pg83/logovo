"""Many small merges keep the pile at about log2(files) objects, and
nothing is lost or doubled on the way to the index."""

import uuid

import lib


def test():
    with lib.Lab() as lab:
        codex_id = str(uuid.uuid4())
        path = lab.codex / "2026" / "01" / "02" / f"rollout-2026-01-02T11-00-00-{codex_id}.jsonl"
        turns = []
        for i in range(9):
            turns.append((f"step {i}", f"done {i}", f"cmd{i}", f"out{i}"))
            path.write_text(lib.codex_session(codex_id, turns))
            assert lab.scan().stderr.count("scan: shipped") == 1
            lab.merge()
            assert lab.keys("queue") == []
            assert len(lab.keys("pile")) <= 4, (i, lab.keys("pile"))

        # Re-shipping everything from scratch duplicates every portion.
        (path.parent / (path.name + ".latest")).write_text("0\n")
        lab.scan()
        lab.merge()
        lab.index()
        with lab.serve() as serve:
            doc = serve.json(f"/v1/sessions/{codex_id}?format=json")
            assert doc["session"]["turns"] == 9 * 4, doc["session"]
            assert [d["body"] for d in doc["docs"] if d["role"] == "user"] == [f"step {i}" for i in range(9)]


if __name__ == "__main__":
    test()
    print("ok")
