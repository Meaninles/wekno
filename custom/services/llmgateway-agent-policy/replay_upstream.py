"""Diagnostic replay to the configured Qwen upstream. Never executes returned tools."""
import copy
import json
from pathlib import Path
import time
import httpx
import yaml

entry=next(m['litellm_params'] for m in yaml.safe_load(Path('/app/config.yaml').read_text())['model_list'] if m['model_name']=='Qwen3.6-27B')
base=entry['api_base'];key=entry.get('api_key','')
if key.startswith('os.environ/'):
    import os
    key=os.environ[key.split('/',1)[1]]
original=json.loads(Path('/tmp/qwen-replay-request.json').read_text())
for skip in (True,False):
    for separate in (True,False):
        body={**copy.deepcopy(original),'model':entry['model'].removeprefix('hosted_vllm/'),
            'max_tokens':2048,'temperature':1.0,'top_p':.95,'top_k':20,'min_p':0,'presence_penalty':0,'repetition_penalty':1,
            'seed':42,'stream':False,'skip_special_tokens':skip,'separate_reasoning':separate,
            'chat_template_kwargs':{'enable_thinking':True,'preserve_thinking':True,'reasoning_effort':'xhigh'},'logprobs':True}
        started=time.monotonic()
        r=httpx.post(base+'/chat/completions',headers={'Authorization':'Bearer '+key},json=body,timeout=150)
        data=r.json();Path(f'/tmp/qwen-replay-{skip}-{separate}.json').write_text(json.dumps(data,ensure_ascii=False))
        choice=(data.get('choices') or [{}])[0];message=choice.get('message') or {}
        tokens=(choice.get('logprobs') or {}).get('content') or []
        raw=''.join(t.get('token','') for t in tokens)
        print(json.dumps({'skip_special_tokens':skip,'separate_reasoning':separate,'status':r.status_code,'seconds':round(time.monotonic()-started,2),
            'finish':choice.get('finish_reason'),'content':message.get('content'),'reasoning':message.get('reasoning_content'),
            'calls':message.get('tool_calls'),'raw_tokens':raw[:2500],'error':data.get('error')},ensure_ascii=False),flush=True)
