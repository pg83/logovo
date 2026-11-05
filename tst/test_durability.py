import os
import shutil
import subprocess
import sys
import uuid

import lib


def test():
    strace = shutil.which('strace')
    if sys.platform != 'linux' or strace is None:
        print('fsync fault injection requires Linux and strace')
        return

    for precreate_pile in (False, True):
        with lib.Lab() as lab:
            sid = str(uuid.uuid4())
            path = lab.claude / '-home-me-proj' / f'{sid}.jsonl'
            path.write_text(lib.claude_session(sid, [('durable needle', 'answer', 'ls', 'ok')]))
            lab.scan()
            queue = lab.keys('queue')
            if precreate_pile:
                (lab.store / 'pile').mkdir()
            trace = lab.dir / 'fsync.trace'
            result = subprocess.run([
                strace, '-f', '-o', str(trace), '-e', 'trace=fsync',
                '-e', 'inject=fsync:error=EIO:when=1',
                str(lib.LOGOVO), 'merge', '-store', f'dir:{lab.store}',
            ], capture_output=True, text=True, timeout=30)
            traced = trace.read_text()
            if 'ptrace' in result.stderr and 'Operation not permitted' in result.stderr:
                print('fsync fault injection requires ptrace permission')
                return
            assert result.returncode != 0, (result.stderr, traced)
            assert 'EIO' in traced and 'INJECTED' in traced, traced
            assert lab.keys('queue') == queue
            assert not list(lab.store.rglob('*.tmp'))
            lab.merge()
            lab.index()
            with lab.serve() as serve:
                doc = serve.json(f'/v1/sessions/{sid}?format=json')
                assert doc['session']['turns'] == 4, doc
                assert len(lib.search(serve, 'needle')) == 1


if __name__ == '__main__':
    test()
    print('ok')
