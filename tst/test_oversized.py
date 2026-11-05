import base64
import json
import os
import uuid

import lib


def test():
    with lib.Lab() as lab:
        sid = str(uuid.uuid4())
        path = lab.claude / '-home-me-proj' / f'{sid}.jsonl'
        with path.open('w') as f:
            f.write(lib.claude_record('user', sid, 'before oversized', '2026-01-02T10:00:00Z', role='user') + '\n')
            json.dump({'type': 'user', 'timestamp': '2026-01-02T10:01:00Z',
                       'message': {'role': 'user', 'content': base64.b64encode(os.urandom(17 << 20)).decode()}}, f)
            f.write('\n')
            f.write(lib.claude_record('user', sid, 'after oversized', '2026-01-02T10:02:00Z', role='user') + '\n')
        result = lab.scan()
        assert result.stderr.count('scan: oversized record skipped') == 1, result.stderr
        assert result.stderr.count('scan: shipped') == 2, result.stderr
        assert len(lab.keys('queue')) == 2
        assert int(path.with_suffix('.jsonl.latest').read_text()) == path.stat().st_size
        result = lab.scan()
        assert 'shipped' not in result.stderr and 'oversized' not in result.stderr, result.stderr
        lab.merge()
        lab.index()
        with lab.serve() as serve:
            docs = serve.json(f'/v1/sessions/{sid}?format=json')['docs']
            assert [d['body'] for d in docs] == ['before oversized', 'after oversized'], docs


if __name__ == '__main__':
    test()
    print('ok')
