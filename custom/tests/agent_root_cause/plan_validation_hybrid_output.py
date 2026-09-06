"""Paired next-action diagnostic: remove tool-output workflow instructions only.

Uses real current prompt/context builders and all eight tool definitions. The
first round comes from a persisted local run; this is a stage-level experiment,
not a bit-for-bit capture of the original model request or a finished QA run.
No business prompt/agent is changed and no returned tool call is executed here.
"""
import copy
import json
from pathlib import Path
import time
from urllib.request import Request, urlopen
from remaining_retrieval_probe import model_config

ROOT = Path(__file__).resolve().parents[3] / '.local-data/agent-audit-20260905/plan-validation'
export = json.loads((ROOT / 'hybrid-stage/hybrid-stage-context.json').read_text(encoding='utf-8'))
config = json.loads((ROOT / 'hybrid-template-control.json').read_text(encoding='utf-8'))['control_agent']['config']
messages = json.loads((ROOT / 'messages-10d514e5-204b-488b-ade6-583ee3ef0f01.json').read_text(encoding='utf-8'))
assistant = next(m for m in messages if m['role'] == 'assistant')
calls = [c for s in assistant['agent_steps'] for c in s.get('tool_calls') or []]
base = copy.deepcopy(export['messages'])
base.append({'role': 'assistant', 'content': '', 'tool_calls': [
    {'id': c['id'], 'type': 'function', 'function': {'name': c['name'], 'arguments': json.dumps(c['args'], ensure_ascii=False)}} for c in calls]})
for c in calls:
    base.append({'role': 'tool', 'name': c['name'], 'tool_call_id': c['id'], 'content': c['result']['output']})
base.append({'role': 'user', 'content': export['post_tool_reminder']})
model = model_config('prod-deepseek-v4-flash-int8-chat')
params = model['parameters']
headers = {'Content-Type': 'application/json', 'Authorization': 'Bearer ' + params.get('api_key', ''), **(params.get('custom_headers') or {})}
results = []
for repetition in range(3):
    for neutral in ([False, True] if repetition % 2 == 0 else [True, False]):
        body = {'model': model['name'], 'messages': copy.deepcopy(base),
                'tools': [{'type': 'function', 'function': f} for f in export['functions']],
                'temperature': config['temperature'], 'stream': False,
                'chat_template_kwargs': {'enable_thinking': config.get('thinking', False)}}
        if neutral:
            for m in body['messages']:
                if m.get('name') == 'knowledge_search':
                    m['content'] = m['content'].split('\n\n===')[0]
        started = time.perf_counter()
        try:
            with urlopen(Request(params['base_url'].rstrip('/') + '/chat/completions', json.dumps(body, ensure_ascii=False).encode(), headers), timeout=180) as r:
                response = json.load(r)
            row = {'repetition': repetition + 1, 'neutral_tool_output': neutral,
                   'elapsed_s': time.perf_counter() - started, 'request': body, 'response': response}
        except Exception as exc:
            row = {'repetition': repetition + 1, 'neutral_tool_output': neutral, 'error': type(exc).__name__}
        results.append(row)
        (ROOT / 'hybrid-tool-output-ablation.json').write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding='utf-8')
        print(json.dumps({k: v for k, v in row.items() if k not in ['request']} , ensure_ascii=False), flush=True)
