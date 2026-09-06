"""Run on the authorized llmgateway host: prepare, then apply the reviewed plan.

No credentials are printed. Original Kubernetes objects remain in a private backup
directory. Immutable ConfigMaps allow rollback by restoring the old pod template.
"""
import argparse
import copy
import hashlib
import json
import os
import subprocess
from pathlib import Path

BASELINE_SHA='b30a4a8f9b547906a906ed0914eb286daffd4ae9f1a4820be11a25c77afee326'
VERSION='agent-policy-20260906-v1'
BACKUP=Path('/root/llmgateway-backups')/VERSION
STAGED='/tmp/llmgateway-agent-policy-20260906'


def kube(*args,input=None):
    result=subprocess.run(['kubectl','-n','litellm',*args],input=input,text=True,capture_output=True,check=True)
    return result.stdout


def get(kind,name):return json.loads(kube('get',kind,name,'-o','json'))
def sha(s):return hashlib.sha256(s.encode()).hexdigest()


def prepare(pod, expected_sha):
    if BACKUP.exists():raise RuntimeError('Plan already exists; inspect it rather than overwriting backup')
    deploy=get('deployment','litellm')
    active={v['name']:v['configMap']['name'] for v in deploy['spec']['template']['spec']['volumes'] if 'configMap' in v}
    old=get('configmap',active['patch']);config=get('configmap',active['config'])
    assert sha(old['data']['sitecustomize.py'])==expected_sha,'Active adapter drifted'
    assert deploy['spec']['replicas']==3
    strategy=deploy['spec']['strategy'];assert strategy['type']=='RollingUpdate' and strategy['rollingUpdate']['maxUnavailable']==0
    assert strategy['rollingUpdate']['maxSurge']==1
    patch=kube('exec',pod,'--','cat',STAGED+'/sitecustomize.py')
    policy=kube('exec',pod,'--','cat',STAGED+'/generation_policy.py')
    # YAML stays on the gateway. The source still contains the same upstream
    # endpoints and credentials; clone aliases rather than deploy new models.
    transform='''import copy,json,sys,yaml
from generation_policy import ROUTES
config=yaml.safe_load(sys.stdin.read())
for name, (_, parent, _) in ROUTES.items():
 existing=next((m for m in config['model_list'] if m['model_name']==name),None)
 if existing:
  assert existing['litellm_params']==next(m['litellm_params'] for m in config['model_list'] if m['model_name']==parent)
  continue
 entry=copy.deepcopy(next(m for m in config['model_list'] if m['model_name']==parent))
 entry['model_name']=name
 entry.get('model_info',{}).pop('id',None)
 config['model_list'].append(entry)
sys.stdout.write(yaml.safe_dump(config,allow_unicode=True,sort_keys=False))
'''
    new_yaml=kube('exec','-i',pod,'--','env','LITELLM_LOCAL_MODEL_COST_MAP=True','PYTHONPATH='+STAGED,'python','-c',transform,input=config['data']['config.yaml'])
    maps=[]
    for name,data in [('thinking-patch-'+VERSION,{'sitecustomize.py':patch,'generation_policy.py':policy}),('litellm-config-'+VERSION,{**config['data'],'config.yaml':new_yaml})]:
        maps.append({'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':name,'namespace':'litellm'},'immutable':True,'data':data})
    template=copy.deepcopy(deploy['spec']['template']);spec=template['spec']
    for volume in spec['volumes']:
        cm=volume.get('configMap',{})
        if volume['name']=='patch':cm['name']='thinking-patch-'+VERSION
        elif volume['name']=='config':cm['name']='litellm-config-'+VERSION
    container=next(c for c in spec['containers'] if any(m.get('subPath')=='sitecustomize.py' for m in c.get('volumeMounts',[])))
    mount=next(m for m in container['volumeMounts'] if m.get('subPath')=='sitecustomize.py')
    container['volumeMounts']=[m for m in container['volumeMounts'] if m.get('subPath')!='generation_policy.py']
    container['volumeMounts'].append({**mount,'subPath':'generation_policy.py','mountPath':mount['mountPath'].replace('sitecustomize.py','generation_policy.py')})
    spec['terminationGracePeriodSeconds']=720
    container['env']=[e for e in container.get('env',[]) if e['name']!='GRACEFUL_SHUTDOWN_TIMEOUT']+[{'name':'GRACEFUL_SHUTDOWN_TIMEOUT','value':'650'}]
    template.setdefault('metadata',{}).setdefault('annotations',{})['llmgateway.moutai.com.cn/generation-policy']=VERSION
    os.umask(0o077);BACKUP.mkdir(parents=True,mode=0o700)
    for filename,value in [('deployment-before.json',deploy),('thinking-before.json',old),('config-before.json',config),('maps.json',maps),('template.json',template)]:
        (BACKUP/filename).write_text(json.dumps(value,ensure_ascii=False))
    print(json.dumps({'prepared':str(BACKUP),'adapter_sha256':sha(patch),'policy_sha256':sha(policy),'replicas':3,'new_routes':['DeepSeek-V4-Flash-Agent','Qwen3.8-27B-Agent'],'grace_seconds':720,'drain_seconds':650}))


def apply():
    deploy=json.loads((BACKUP/'deployment-before.json').read_text())
    for file in ['thinking-before.json','config-before.json']:
        before=json.loads((BACKUP/file).read_text())
        assert get('configmap',before['metadata']['name'])['data']==before['data'],'Gateway configuration drifted'
    current=get('deployment','litellm')
    assert current['spec']==deploy['spec'],'Deployment drifted; re-review before apply'
    for configmap in json.loads((BACKUP/'maps.json').read_text()):
        kube('create','-f','-',input=json.dumps(configmap))
    patch=[{'op':'test','path':'/metadata/resourceVersion','value':current['metadata']['resourceVersion']},
           {'op':'replace','path':'/spec/template','value':json.loads((BACKUP/'template.json').read_text())}]
    kube('patch','deployment','litellm','--type=json','-p',json.dumps(patch))
    print('Applied reviewed gateway template; monitor rollout. Production WeKnora was not touched.')


if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('action',choices=['prepare','apply']);p.add_argument('--pod');p.add_argument('--version',default=VERSION);p.add_argument('--expected-sha',default=BASELINE_SHA);a=p.parse_args()
    VERSION=a.version;BACKUP=Path('/root/llmgateway-backups')/VERSION
    prepare(a.pod,a.expected_sha) if a.action=='prepare' else apply()
