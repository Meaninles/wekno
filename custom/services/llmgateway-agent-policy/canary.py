"""Explicit operator-run live upstream smoke, isolated from the serving process."""
import asyncio
import copy
import json
import time
from pathlib import Path
import yaml
import sitecustomize as adapter
from litellm import Router
from generation_policy import ROUTES


async def main():
    source=yaml.safe_load(Path('/app/config.yaml').read_text())
    entries=[]
    for name, (_, parent, _) in ROUTES.items():
        entry=copy.deepcopy(next(m for m in source['model_list'] if m['model_name']==parent))
        entry['model_name']=name
        entry.pop('model_info',None)
        entries.append(entry)
    router=Router(model_list=entries,num_retries=0,timeout=150)
    # The same task through each protocol, with an actual tool response round-trip.
    for name in ROUTES:
        for enabled in (False,True):
            for anthropic in (False,True):
                started=time.monotonic()
                messages=[{'role':'user','content':'请调用 lookup_record 获取编号 R17 的记录，并根据工具返回的信息简短回答这条记录的状态。'}]
                tool={'name':'lookup_record','description':'根据记录编号读取记录的真实状态。','parameters':{'type':'object','properties':{'id':{'type':'string'}},'required':['id'],'additionalProperties':False}}
                records=[]
                try:
                    for turn in range(3):
                        controls={'thinking':{'type':'adaptive' if enabled else 'disabled'}}
                        if enabled: controls['output_config' if anthropic else 'reasoning_effort']={'effort':ROUTES[name][2]} if anthropic else ROUTES[name][2]
                        kwargs={'model':name,'messages':messages,'max_tokens':4096,**controls}
                        if anthropic:
                            kwargs['tools']=[{'name':tool['name'],'description':tool['description'],'input_schema':tool['parameters']}]
                            kwargs,_=adapter.A().translate_anthropic_to_openai(kwargs)
                        else:kwargs['tools']=[{'type':'function','function':tool}]
                        result=await router.acompletion(**kwargs)
                        if anthropic:
                            answer=adapter.A().translate_openai_response_to_anthropic(result)
                            blocks=answer['content'];calls=[b for b in blocks if b['type']=='tool_use']
                            text=''.join(b.get('text','') for b in blocks if b['type']=='text')
                            records.append({'stop':answer['stop_reason'],'calls':[b['name'] for b in calls],'text':text[:800]})
                            if not calls:break
                            messages += [{'role':'assistant','content':blocks},{'role':'user','content':[{'type':'tool_result','tool_use_id':b['id'],'content':'{"id":"R17","status":"等待复核"}'} for b in calls]}]
                        else:
                            choice=result.choices[0];message=choice.message;calls=message.tool_calls or []
                            records.append({'stop':choice.finish_reason,'calls':[c.function.name for c in calls],'text':(message.content or '')[:800]})
                            if not calls:break
                            messages.append(message.model_dump(exclude_none=True))
                            messages.extend({'role':'tool','tool_call_id':c.id,'content':'{"id":"R17","status":"等待复核"}'} for c in calls)
                    print(json.dumps({'model':name,'thinking':enabled,'protocol':'anthropic' if anthropic else 'openai','seconds':round(time.monotonic()-started,2),'rounds':records},ensure_ascii=False),flush=True)
                except Exception as exc:
                    print(json.dumps({'model':name,'thinking':enabled,'protocol':'anthropic' if anthropic else 'openai','seconds':round(time.monotonic()-started,2),'error_type':type(exc).__name__,'error':str(exc)[:500]},ensure_ascii=False),flush=True)
    router.reset()


if __name__=='__main__': asyncio.run(main())
