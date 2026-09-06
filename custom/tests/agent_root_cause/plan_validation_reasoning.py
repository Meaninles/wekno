"""Paired reasoning-capability control on a frozen real finalizer input.

The prompt, source evidence, history and sampling parameters remain fixed.
Only the provider's generic thinking parameter changes. No semantic grader.
"""
import copy
import hashlib
import json
from pathlib import Path
import time
from urllib.request import Request,urlopen
from remaining_retrieval_probe import model_config

ROOT=Path(__file__).resolve().parents[3]
AUDIT=ROOT/'.local-data/agent-audit-20260905'
OUT=AUDIT/'plan-validation'
frozen=json.loads((AUDIT/'remaining/frozen-native-final.json').read_text(encoding='utf-8'))
model=model_config('prod-deepseek-v4-flash-int8-chat');p=model['parameters']
results=[]
for repetition in range(3):
 for thinking in ([False,True] if repetition%2==0 else [True,False]):
  body={'model':model['name'],'messages':copy.deepcopy(frozen['messages']),**frozen['model_parameters'],'stream':False,'chat_template_kwargs':{'enable_thinking':thinking}}
  started=time.perf_counter()
  headers={'Content-Type':'application/json','Authorization':'Bearer '+p.get('api_key',''),**(p.get('custom_headers') or {})}
  try:
   with urlopen(Request(p['base_url'].rstrip('/')+'/chat/completions',json.dumps(body,ensure_ascii=False).encode(),headers),timeout=180) as r:response=json.load(r)
   entry={'repetition':repetition+1,'thinking_requested':thinking,'elapsed_s':time.perf_counter()-started,'request':body,'response':response}
  except Exception as e:entry={'repetition':repetition+1,'thinking_requested':thinking,'elapsed_s':time.perf_counter()-started,'error_type':type(e).__name__}
  results.append(entry)
  (OUT/'reasoning-ablation.json').write_text(json.dumps(results,ensure_ascii=False,indent=2),encoding='utf-8')
  response=entry.get('response',{});message=(response.get('choices') or [{}])[0].get('message') or {}
  print(json.dumps({'repetition':repetition+1,'thinking_requested':thinking,'elapsed_s':entry['elapsed_s'],'usage':response.get('usage'),'answer_chars':len(message.get('content') or ''),'reasoning_chars':len(message.get('reasoning_content') or '')},ensure_ascii=False),flush=True)
