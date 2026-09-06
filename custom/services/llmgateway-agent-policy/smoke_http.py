"""Run inside a gateway pod after rollout; credentials remain in its environment."""
import json
import os
import time
import httpx

for model in ('DeepSeek-V4-Flash','Qwen3.6-27B','DeepSeek-V4-Flash-Agent','Qwen3.8-27B-Agent'):
    for protocol in ('openai','anthropic'):
        for enabled in (False,True):
            body={'model':model,'messages':[{'role':'user','content':'简短回答：2+3等于多少？'}],'max_tokens':1024,'stream':True}
            body['thinking']={'type':'adaptive' if enabled else 'disabled'}
            if protocol=='anthropic':body['thinking']['display']='omitted'
            if enabled and model.endswith('-Agent'):
                effort='high' if model.startswith('Deep') else 'xhigh'
                body['output_config' if protocol=='anthropic' else 'reasoning_effort']={'effort':effort} if protocol=='anthropic' else effort
            started=time.monotonic();texts=[];reasoning=0;stop=None;errors=[];status=None
            headers={'Authorization':'Bearer '+os.environ['LITELLM_MASTER_KEY'],'anthropic-version':'2023-06-01'}
            try:
                with httpx.stream('POST','http://127.0.0.1:4000/v1/'+('messages' if protocol=='anthropic' else 'chat/completions'),headers=headers,json=body,timeout=120) as response:
                    status=response.status_code
                    for line in response.iter_lines():
                        if not line.startswith('data:'):continue
                        raw=line[5:].strip()
                        if raw=='[DONE]':continue
                        event=json.loads(raw)
                        if event.get('error'):errors.append(event['error'])
                        if protocol=='anthropic':
                            delta=event.get('delta') or {}
                            texts.append(delta.get('text') or '')
                            reasoning+=len(delta.get('thinking') or '')
                            stop=delta.get('stop_reason') or stop
                        else:
                            for choice in event.get('choices') or []:
                                delta=choice.get('delta') or {}
                                texts.append(delta.get('content') or '')
                                reasoning+=len(delta.get('reasoning_content') or delta.get('reasoning') or '')
                                stop=choice.get('finish_reason') or stop
                    if status!=200:errors.append('HTTP '+str(status))
                print(json.dumps({'model':model,'protocol':protocol,'thinking':enabled,'status':status,'seconds':round(time.monotonic()-started,2),'text':''.join(texts),'visible_reasoning_chars':reasoning,'stop':stop,'errors':errors},ensure_ascii=False),flush=True)
            except Exception as exc:
                print(json.dumps({'model':model,'protocol':protocol,'thinking':enabled,'exception':type(exc).__name__},ensure_ascii=False),flush=True)
