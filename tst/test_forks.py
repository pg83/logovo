import http.client
import json

import lib


def test():
    with lib.Lab() as lab:
        ids = [f'{i:08d}-1111-1111-1111-111111111111' for i in range(1, 7)]
        parent, child, grandchild, sibling, independent, orphan = ids

        def message(role, text, minute):
            return {'type': role, 'timestamp': f'2026-01-02T10:{minute:02d}:00Z',
                    'message': {'role': role, 'content': text}}

        common = [message('user', 'sharedquestion', 0), message('assistant', 'sharedanswer', 1)]
        sessions = {
            parent: common + [message('user', 'forkanchor', 2), message('assistant', 'parentresponse', 3)],
            child: common + [message('user', 'forkanchor', 10), message('assistant', 'childresponse', 11)],
            grandchild: common + [message('user', 'forkanchor', 10), message('assistant', 'childresponse', 20),
                                 message('user', 'grandchildprompt', 21)],
            sibling: [common[0], message('assistant', 'sharedanswer', 15), message('user', 'siblingprompt', 16)],
            independent: [message('user', 'sharedquestion', 30), message('assistant', 'sharedanswer', 31)],
            orphan: [message('user', 'orphanprompt', 40)],
        }
        for sid, records in sessions.items():
            path = lab.claude / '-home-me-proj' / f'{sid}.jsonl'
            path.write_text('\n'.join(json.dumps(r) for r in records) + '\n')
        lab.scan()
        lab.merge()
        lab.index()
        with lab.serve() as serve:
            def session(sid):
                return serve.json(f'/v1/sessions/{sid}?format=json')

            for sid, expected_parent, copied in ((child, parent, 3), (grandchild, child, 4), (sibling, parent, 2)):
                result = session(sid)
                assert result['session']['parent'] == expected_parent, result
                assert result['session']['copied'] == copied, result
                assert len(result['docs']) == len(sessions[sid]), result
            assert session(independent)['session']['parent'] == ''
            assert session(orphan)['session']['parent'] == ''
            assert len(lib.search(serve, 'sharedquestion')) == 2
            assert len(lib.search(serve, 'forkanchor')) == 1
            assert len(lib.search(serve, 'childresponse')) == 1
            assert len(lib.search(serve, 'grandchildprompt')) == 1
            with lab.web(serve) as web:
                def rpc(method, params=None, protocol='2025-11-25', notification=False):
                    body = {'jsonrpc': '2.0', 'method': method, 'params': params or {}}
                    if not notification:
                        body['id'] = 1
                    conn = http.client.HTTPConnection('127.0.0.1', web.port, timeout=10)
                    try:
                        conn.request('POST', '/v1/mcp', json.dumps(body),
                                     {'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream',
                                      'MCP-Protocol-Version': protocol})
                        response = conn.getresponse()
                        data = response.read().decode()
                        return response.status, json.loads(data) if data.startswith('{') else data
                    finally:
                        conn.close()

                status, result = rpc('initialize', {'protocolVersion': '2025-11-25'})
                assert status == 200 and result['result']['protocolVersion'] == '2025-11-25', result
                _, result = rpc('tools/list')
                assert {tool['name'] for tool in result['result']['tools']} == {'search', 'show'}
                _, result = rpc('tools/call', {'name': 'search', 'arguments': {'query': 'forkanchor'}})
                text = result['result']['content'][0]['text']
                assert parent in text and child not in text, text
                _, result = rpc('tools/call', {'name': 'show', 'arguments': {'session': child, 'from': 0, 'to': 3}})
                assert f'forked from {parent}' in result['result']['content'][0]['text'], result
                _, result = rpc('tools/call', {'name': 'search', 'arguments': {'query': ''}})
                assert result['result']['isError'] is True, result
                _, result = rpc('tools/call', {'name': 'unknown', 'arguments': {}})
                assert result['error']['code'] == -32602, result
                assert rpc('notifications/initialized', notification=True)[0] == 202
                assert rpc('ping', protocol='unsupported')[0] == 400


if __name__ == '__main__':
    test()
    print('ok')
