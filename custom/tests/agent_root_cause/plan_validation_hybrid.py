"""Compare preset ID-only creation with the unmodified shipped prompt resolved.

Only a new isolated test agent is created. No existing config, user prompt,
knowledge content, retrieval setting or business implementation is changed.
"""
import copy
import json
import yaml
from plan_validation_live import AUDIT, OUT, P, C, REPO

fixture = json.loads((AUDIT / 'fix-extended-resources.json').read_text(encoding='utf-8'))
original = C.request('GET', '/agents/' + fixture['agents']['hybrid-rag-wiki']['id'])['data']
config = copy.deepcopy(original['config'])
templates = yaml.safe_load((REPO / 'config/prompt_templates/agent_system_prompt.yaml').read_text(encoding='utf-8'))['templates']
template = next(t for t in templates if t['id'] == 'hybrid_rag_wiki_agent')
config['system_prompt'] = template['content']
config['system_prompt_id'] = template['id']
agent = C.request('POST', '/agents', {'name': '补充验证-混合检索配置解析-0905',
    'description': '隔离对照：将已有系统模板按其 ID 原样解析，问题和工具不变', 'config': config})['data']
(OUT / 'hybrid-template-control.json').write_text(json.dumps({'baseline_agent': original,
    'control_agent': agent, 'changed_fields': [key for key in config if config[key] != original['config'].get(key)]}, ensure_ascii=False, indent=2), encoding='utf-8')
P['run']('pv-hybrid-resolved', agent['id'], [
    '白鹭培训预算是否已经确认？请说明依据。',
    '培训现在的时间、地点和联系人是什么？',
    '投影仪用完以后应该交到哪里？',
], [fixture['kb']['data']['id']])
