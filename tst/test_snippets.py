import uuid

import lib


def test():
    with lib.Lab() as lab:
        sid = str(uuid.uuid4())
        path = lab.claude / '-home-me-proj' / f'{sid}.jsonl'
        path.write_text(lib.claude_record('user', sid, 'see [note] about grafana',
                                         '2026-01-02T10:00:00Z', role='user') + '\n')
        lab.scan()
        lab.merge()
        lab.index()
        with lab.serve() as serve:
            hits = lib.search(serve, 'grafana')
            assert hits[0]['snippet'] == 'see [note] about \x02grafana\x03', hits
            result = lib.run('search', '-api', f'http://127.0.0.1:{serve.port}', 'grafana')
            assert 'user: see [note] about [grafana]' in result.stdout, result.stdout
            assert '\x02' not in result.stdout and '\x03' not in result.stdout


if __name__ == '__main__':
    test()
    print('ok')
