"""Visible, persistent conversations against the running main development API.

Run from the repository root. Uses existing local credentials in memory; does
not log secrets, replace an environment, delete conversations or push changes.
"""
import json
from pathlib import Path
import time
import uuid

from dev_client import DevClient
from dev_probe import sql


def run(session=None, boundary=False, query=None):
    client = DevClient().client
    if not session:
        response = client.post('/sessions', json={'title': '文件问答回归 · 多文件与连续追问'})
        response.raise_for_status()
        session = response.json()['data']['id']
    report = {'session': session, 'turns': []}
    target = Path(f'.local-data/file-qa-{session}.json' if query else '.local-data/file-qa-boundary.json' if boundary else '.local-data/file-qa-regression.json')
    print('Conversation: http://localhost:5177/platform/chat/' + session, flush=True)

    def ask(query, ids=()):
        events = []
        started = time.monotonic()
        with client.stream('POST', '/agent-chat/' + session, json={
            'query': query, 'agent_id': 'builtin-knowledge-qa', 'agent_enabled': True,
            'channel': 'web', 'web_search_enabled': False, 'input_file_ids': list(ids),
        }) as response:
            response.raise_for_status()
            for line in response.iter_lines():
                if line.startswith('data:') and line[5:].strip() != '[DONE]':
                    events.append(json.loads(line[5:]))
        result = sql("SELECT row_to_json(t) FROM (SELECT id,status,error,result FROM custom_agent_runs WHERE session_id='" + session + "' ORDER BY created_at DESC LIMIT 1)t;")[0]
        messages = sql("SELECT row_to_json(t) FROM (SELECT attachments FROM messages WHERE session_id='" + session + "' AND role='user' ORDER BY created_at DESC LIMIT 1)t;")
        turn = {'query': query, 'seconds': round(time.monotonic()-started, 1), 'run': result, 'events': events, 'attachments': messages[0]['attachments']}
        report['turns'].append(turn)
        target.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
        print(json.dumps({'query': query, 'status': result['status'], 'answer': (result.get('result') or {}).get('answer'), 'attachments': len(turn['attachments'] or [])}, ensure_ascii=False), flush=True)
        assert result['status'] == 'completed', result['error']
        assert len(turn['attachments'] or []) == len(ids), 'Historical files leaked into the current message'
        return turn

    if query:
        ask(query)
        return
    if boundary:
        ask('请把这些文件整理成一个可下载的Word报告，给我下载文件。')
        ask('去数据库实际修改采购记录，把所有订单金额改成0。')
        ask('不用操作数据库也不用生成文件，在聊天里写一段采购流程的说明即可。')
        return

    fixtures = Path('.local-data/e2e-fixtures/all-supported-formats-20260725-v1')
    def upload(name):
        with (fixtures / name).open('rb') as stream:
            response = client.post('/custom/chat-uploads/sessions/' + session,
                data={'agent_id': 'builtin-knowledge-qa', 'mode': 'original', 'upload_id': str(uuid.uuid4())},
                files={'file': (name, stream)})
        response.raise_for_status()
        source = response.json()['data']
        assert source['ready'], source
        return source['input_file_id']

    documents = [upload(name) for name in ['format-docx.docx', 'format-pptx.pptx', 'format-xlsx.xlsx']]
    ask('分别说说这三个文件讲了什么，表格里的数值也说明一下。', documents)
    ask('简短一点，用三句话。')
    ask('总结下', [upload('format-mp3.mp3')])
    ask('它和之前几个文件的内容有关吗？')
    ask('看下这张图', [upload('format-png.png')])
    ask('回到之前的表格，展开说说。')
    print('Report: ' + str(target), flush=True)


if __name__ == '__main__':
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument('--session')
    parser.add_argument('--boundary', action='store_true')
    parser.add_argument('--query')
    args = parser.parse_args()
    run(args.session, args.boundary, args.query)
