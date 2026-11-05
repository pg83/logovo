import json
import uuid

import lib


def test():
    with lib.Lab() as lab:
        renamed = str(uuid.uuid4())
        fallback = str(uuid.uuid4())
        queued = str(uuid.uuid4())
        codex = str(uuid.uuid4())

        def write_claude(sid, records):
            path = lab.claude / '-home-me-proj' / f'{sid}.jsonl'
            path.write_text('\n'.join(json.dumps(r) for r in records) + '\n')

        def message(text, minute, **extra):
            return {'type': 'user', 'timestamp': f'2026-01-02T10:{minute:02d}:00Z',
                    'message': {'role': 'user', 'content': text}, **extra}

        write_claude(renamed, [
            message('original question', 0),
            {'type': 'ai-title', 'aiTitle': 'Automatic title'},
            {'type': 'custom-title', 'customTitle': 'Chosen title'},
            {'type': 'ai-title', 'aiTitle': 'Later automatic title'},
            message('compaction context', 1, isCompactSummary=True),
        ])
        write_claude(fallback, [
            message('summary before real prompt', 2, isCompactSummary=True),
            message('<command-name>/model</command-name>\n<command-message>model</command-message>', 3),
            message('deploy the thing', 4),
        ])
        write_claude(queued, [
            {'type': 'attachment', 'timestamp': '2026-01-02T10:05:00Z',
             'attachment': {'type': 'queued_command', 'prompt': 'queued prompt', 'origin': {'kind': 'human'}}},
            message('queued prompt', 6, origin={'kind': 'task-notification'}),
            message('queued prompt', 7, origin={'kind': 'human'}),
            {'type': 'assistant', 'timestamp': '2026-01-02T10:08:00Z',
             'message': {'role': 'assistant', 'content': 'queued answer'}},
            message('queued prompt', 9),
            {'type': 'attachment', 'timestamp': '2026-01-02T10:10:00Z',
             'attachment': {'type': 'queued_command', 'prompt': [{'type': 'text', 'text': 'legacy prompt'}],
                            'commandMode': 'prompt'}},
            {'type': 'attachment', 'timestamp': '2026-01-02T10:11:00Z',
             'attachment': {'type': 'queued_command', 'prompt': 'peer attachment', 'origin': {'kind': 'peer'}}},
        ])
        path = lab.codex / '2026/01/02' / f'rollout-2026-01-02T11-00-00-{codex}.jsonl'
        records = [{'type': 'session_meta', 'payload': {'cwd': '/codex-review'}}]
        for i, payload in enumerate([
            {'type': 'message', 'role': 'user', 'content': [{'type': 'input_text', 'text': 'codex outputs'}]},
            {'type': 'local_shell_call', 'action': {'type': 'exec', 'command': ['ls', '-la']}},
            {'type': 'function_call_output', 'output': [{'type': 'input_text', 'text': 'zebrafile.txt'},
                                                       {'type': 'input_image', 'image_url': 'irrelevant'},
                                                       {'type': 'input_text', 'text': 'second line'}]},
            {'type': 'function_call_output', 'output': 'plain output'},
            {'type': 'custom_tool_call_output', 'output': json.dumps({'output': 'wrapped output'})},
            {'type': 'custom_tool_call_output', 'output': [{'type': 'input_text', 'text': 'custom array'}]},
        ]):
            records.append({'type': 'response_item', 'timestamp': f'2026-01-02T11:{i:02d}:00Z', 'payload': payload})
        path.write_text('\n'.join(json.dumps(r) for r in records) + '\n')

        lab.scan()
        lab.merge()
        lab.index()
        with lab.serve() as serve:
            def session(sid):
                return serve.json(f'/v1/sessions/{sid}?format=json')

            result = session(renamed)
            assert result['session']['title'] == 'Chosen title', result
            assert result['docs'][-1]['role'] == 'summary', result
            assert session(fallback)['session']['title'] == 'deploy the thing'
            result = session(queued)
            assert [d['role'] for d in result['docs']] == ['user', 'task-notification', 'assistant', 'user', 'user'], result
            assert [d['body'] for d in result['docs']].count('queued prompt') == 3, result
            assert not lib.search(serve, '"peer attachment"')
            result = session(codex)
            assert [d['body'] for d in result['docs']] == [
                'codex outputs', '▶ shell: ls -la', '◀ zebrafile.txt\nsecond line',
                '◀ plain output', '◀ wrapped output', '◀ custom array',
            ], result
            assert lib.search(serve, 'zebrafile')


if __name__ == '__main__':
    test()
    print('ok')
