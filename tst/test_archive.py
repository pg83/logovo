import uuid

import lib


def test():
    with lib.Lab() as lab:
        sid = str(uuid.uuid4())
        path = lab.claude / '-home-me-proj' / f'{sid}.jsonl'
        path.write_text(lib.claude_session(sid, [('archive needle', 'answer', 'ls', 'ok')]))
        lab.scan()
        bad = lab.store / 'queue/ffffffff-ffff-ffff-ffff-ffffffffffff.broken'
        bad.write_bytes(b'broken zstd')
        lab.merge()
        assert not bad.exists()
        assert (lab.store / 'bad/queue' / bad.name).read_bytes() == b'broken zstd'
        lab.index()
        published = lab.store / 'index/logovo.sqlite.zst'
        previous = published.read_bytes()
        pile = lab.store / lab.keys('pile')[0]
        original = pile.read_bytes()
        pile.write_bytes(b'broken zstd')
        result = lib.run('index', '-store', f'dir:{lab.store}', check=False)
        assert result.returncode != 0 and pile.name in result.stderr, result.stderr
        assert published.read_bytes() == previous

        lib.run('index', '-store', f'dir:{lab.store}', '-allow-partial')
        with lab.serve() as serve:
            status = serve.json('/v1/status')
            assert status['complete'] == 'false' and status['broken_files'] == '1', status

        pile.write_bytes(original)
        legacy = pile.with_suffix('')
        pile.rename(legacy)
        before_migration = published.read_bytes()
        result = lib.run('index', '-store', f'dir:{lab.store}', check=False)
        assert result.returncode != 0 and 'run merge before index' in result.stderr, result.stderr
        assert published.read_bytes() == before_migration
        lab.merge()
        assert not legacy.exists()
        assert all(key.endswith('.sorted') for key in lab.keys('pile'))
        lab.index()
        with lab.serve() as serve:
            status = serve.json('/v1/status')
            assert status['complete'] == 'true' and status['broken_files'] == '0', status
            assert len(lib.search(serve, 'needle')) == 1
            doc = serve.json(f'/v1/sessions/{sid}?format=json')
            assert doc['session']['turns'] == 4, doc


if __name__ == '__main__':
    test()
    print('ok')
