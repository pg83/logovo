import http.server
import os
from pathlib import Path
import threading
import time
import uuid

import lib


def wait_for(predicate, timeout=5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.02)
    raise AssertionError('condition did not become true')


def test():
    with lib.Lab() as lab:
        sid = str(uuid.uuid4())
        path = lab.claude / '-home-me-proj' / f'{sid}.jsonl'
        path.write_text(lib.claude_session(sid, [('refresh needle', 'answer', 'ls', 'ok')]))
        lab.scan()
        non_database = (lab.store / lab.keys('queue')[0]).read_bytes()
        lab.merge()
        lab.index()
        index = lab.store / 'index/logovo.sqlite.zst'
        original = index.read_bytes()
        cache = lab.dir / 'cache'
        port = lib.free_port()
        with lib.Daemon('serve', '-listen', f'127.0.0.1:{port}', '-store', f'dir:{lab.store}',
                        '-dir', str(cache), '-refresh', '50ms', port=port) as serve:
            version = serve.json('/v1/status')['version']
            fd_root = Path(f'/proc/{serve.proc.pid}/fd')
            baseline_fds = len(list(fd_root.iterdir())) if fd_root.exists() else None
            for bad in (b'broken zstd', non_database):
                index.write_bytes(bad)
                time.sleep(0.35)
                assert serve.json('/v1/status')['version'] == version
                wait_for(lambda: len(list(cache.glob('index-*.sqlite'))) == 1)
                assert lib.search(serve, 'needle')
            if baseline_fds is not None:
                assert len(list(fd_root.iterdir())) <= baseline_fds + 2
            index.write_bytes(original)
            wait_for(lambda: serve.json('/v1/status')['version'] != version)

        stream_cache = lab.dir / 'stream-cache'
        streamed = threading.Event()

        class Handler(http.server.BaseHTTPRequestHandler):
            def send_headers(self):
                self.send_response(200)
                self.send_header('Content-Length', str(len(original)))
                self.send_header('ETag', '"review-index"')
                self.end_headers()

            def do_HEAD(self):
                self.send_headers()

            def do_GET(self):
                self.send_headers()
                middle = len(original) // 2
                self.wfile.write(original[:middle])
                self.wfile.flush()
                deadline = time.monotonic() + 2
                while time.monotonic() < deadline:
                    if list(stream_cache.glob('index-*.sqlite')):
                        streamed.set()
                        break
                    time.sleep(0.02)
                self.wfile.write(original[middle:])

            def log_message(self, *args):
                pass

        with http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler) as s3:
            worker = threading.Thread(target=s3.serve_forever, daemon=True)
            worker.start()
            values = {'S3_ENDPOINT': f'http://127.0.0.1:{s3.server_port}',
                      'AWS_ACCESS_KEY_ID': 'test', 'AWS_SECRET_ACCESS_KEY': 'test', 'AWS_REGION': 'us-east-1'}
            previous = {key: os.environ.get(key) for key in values}
            os.environ.update(values)
            try:
                port = lib.free_port()
                with lib.Daemon('serve', '-listen', f'127.0.0.1:{port}', '-store', 's3://logovo',
                                '-dir', str(stream_cache), port=port) as serve:
                    assert streamed.is_set(), 'compressed index was buffered before creating the local file'
                    assert lib.search(serve, 'needle')
            finally:
                for key, value in previous.items():
                    if value is None:
                        os.environ.pop(key, None)
                    else:
                        os.environ[key] = value
                s3.shutdown()


if __name__ == '__main__':
    test()
    print('ok')
