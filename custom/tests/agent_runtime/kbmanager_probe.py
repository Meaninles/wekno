"""Exercise the distinct KB manager profile against a disposable dev knowledge base."""
import json,time,uuid
from dev_client import DevClient,ROOT,KB
from dev_probe import sql


def main():
    c=DevClient();kb_id=agent=None
    try:
        base=c.client.get('/knowledge-bases/'+KB).json()['data']
        payload={k:base[k] for k in ('embedding_model_id','summary_model_id','chunking_config','storage_provider_config') if k in base}
        payload.update({'name':'统一内核知识库管理验证 '+uuid.uuid4().hex[:8],'type':'document',
            'indexing_strategy':{'vector_enabled':False,'keyword_enabled':True,'wiki_enabled':False,'graph_enabled':False},
            'question_generation_config':{'enabled':False}})
        r=c.client.post('/knowledge-bases',json=payload);r.raise_for_status();kb_id=r.json()['data']['id']
        presets=c.client.get('/agents/type-presets').json()
        rows=presets.get('data',[])
        if isinstance(rows,dict): rows=rows.get('presets',[])
        preset=next(a for a in rows if a['id']=='knowledge-base-manager')['config']
        base_cfg=next(a['config'] for a in c.client.get('/agents').json()['data'] if a['id']=='builtin-general-agent')
        cfg={**base_cfg,**preset,'agent_type':'knowledge-base-manager','kb_selection_mode':'selected','knowledge_bases':[kb_id],
             'professional_skills_selection_mode':'none','selected_professional_skills':[],
             'knowledge_management':{'default_permissions':{'add':True,'modify':True,'delete':True},'knowledge_base_overrides':{}}}
        r=c.client.post('/agents',json={'name':'知识库管理开发验证 '+uuid.uuid4().hex[:8],'config':cfg});r.raise_for_status();agent=r.json()['data']['id']
        s=c.session('知识库管理新增替换删除')
        path=ROOT/'fixtures/manager-fixture.txt';path.write_text('知识库管理验证\n项目编号 KBM-58317\n预算 120 元\n版本 1\n',encoding='utf-8')
        upload=c.upload(s,path,agent=agent)
        report=c.qa('kb-manager-add','请把本轮上传的 manager-fixture.txt 加入所选知识库，然后等待处理完成并核对项目编号和预算。只操作本测试文件。',agent=agent,session=s,kb=[kb_id],uploads=[upload['id']],expected=('KBM-58317','120'))
        assert all(report['checks'].values()),report['checks']
        docs=sql("SELECT row_to_json(t) FROM (SELECT id,title,parse_status FROM knowledges WHERE knowledge_base_id='"+kb_id+"' AND deleted_at IS NULL)t;")
        assert len(docs)==1,docs
        path.write_text('知识库管理验证\n项目编号 KBM-58317\n预算 145 元\n版本 2\n',encoding='utf-8')
        upload=c.upload(s,path,agent=agent)
        report=c.qa('kb-manager-replace','将测试库中的原 manager-fixture.txt 替换为本轮上传的版本 2，完成后等待新版本可检索，再确认预算已经变为 145 元。',agent=agent,session=s,kb=[kb_id],uploads=[upload['id']],expected=('145',))
        assert all(report['checks'].values()),report['checks']
        report=c.qa('kb-manager-delete','现在删除本测试库中的 manager-fixture.txt，等待删除完成后重新列出测试库文档，确认文件已删除。只删除这一份测试文件。',agent=agent,session=s,kb=[kb_id])
        assert all(report['checks'].values()),report['checks']
        docs=sql("SELECT json_build_object('count',count(*)) FROM knowledges WHERE knowledge_base_id='"+kb_id+"' AND deleted_at IS NULL;")
        assert docs[0]['count']==0,docs
        print(json.dumps({'kb_manager':'passed','session':s,'operations':['add','replace','delete']},ensure_ascii=False),flush=True)
    finally:
        if agent:c.client.delete('/agents/'+agent).raise_for_status()
        if kb_id:c.client.delete('/knowledge-bases/'+kb_id).raise_for_status()


if __name__=='__main__':main()

