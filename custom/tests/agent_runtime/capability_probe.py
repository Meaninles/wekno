"""Inspect built-in profiles and real imported skills in the existing dev app."""
import argparse
import io
import json
import uuid
from zipfile import ZipFile

from dev_client import DevClient, ROOT, KB


def profiles(c):
    c.qa('general-direct-chat','用一句话解释为什么彩虹有不同颜色。',agent='builtin-general-agent')
    c.qa('restored-profile-wiki','请根据 Wiki 解释安全运营管理平台（SOC平台）的职责和相关架构，附来源。',agent='builtin-wiki-researcher',kb=['b6bb9e65-a5b3-41dd-bed5-2337f30c6b35'])
    c.qa('restored-profile-data','请查看可用的 DBAnalytics PostgreSQL 测试库（测试），先确认订单相关表及字段，再统计订单总数及金额合计，说明统计口径。只读查询，不修改数据。',agent='builtin-data-analyst')
    session=c.session('表格分析与文件输出');upload=c.upload(session,ROOT/'fixtures/sales.xlsx',agent='builtin-general-agent')
    r=c.qa('restored-profile-table','分析上传的销售表，核对每行数量、单价和小计，计算总金额，并生成含正确公式与已计算结果的核对表.xlsx。',agent='builtin-table-analyst',session=session,uploads=[upload['id']],expected=('52',))
    print(json.dumps({'artifact_checks':c.artifacts(r)},ensure_ascii=False),flush=True)
    session=c.session('文档处理多格式交付');upload=c.upload(session,ROOT/'fixtures/project.docx',agent='builtin-general-agent')
    r=c.qa('restored-profile-document','根据上传项目文件制作一页项目概况 Word 和对应 PDF，再制作两页项目介绍 PPTX。保留准确的项目编号、预算、日期和负责人，版面简洁清楚；请一次完成并给出三个可下载文件。',agent='builtin-document-processing',session=session,uploads=[upload['id']],expected=('LANTERN-7429',))
    print(json.dumps({'artifact_checks':c.artifacts(r)},ensure_ascii=False),flush=True)


def skills(c):
    suffix=uuid.uuid4().hex[:8];light='runtime-fact-review-'+suffix;pro='runtime-ledger-'+suffix
    r=c.client.post('/custom/skills',json={'name':light,'description':'核对制度依据的轻量技能','instructions':'按“已确认事实”“依据”“尚待确认”三个小标题组织回答；把直接条款和推断分开。缺乏依据时明确说明。','enabled':True});r.raise_for_status();light_id=r.json()['data']['id']
    archive=io.BytesIO()
    with ZipFile(archive,'w') as z:
        z.writestr('SKILL.md',f'---\nname: {pro}\ndescription: Verify the bundled ledger with its deterministic Python script and publish its result.\n---\nRead references/policy.md, then execute scripts/check.py with Python from this skill directory. Publish /workspace/outputs/账单核对.json and explain its result. Do not invent the ledger values.\n')
        z.writestr('references/policy.md','The ledger amount is quantity times unit price, summed exactly with integers. Report currency CNY.\n')
        z.writestr('scripts/check.py','from pathlib import Path\nimport json\nrows=[{"quantity":13,"unit_price":7},{"quantity":19,"unit_price":4}]\nout={"currency":"CNY","total":sum(r["quantity"]*r["unit_price"] for r in rows),"rows":rows}\np=Path("/workspace/outputs/账单核对.json");p.parent.mkdir(exist_ok=True);p.write_text(json.dumps(out,ensure_ascii=False),encoding="utf-8");print(json.dumps(out))\n')
    r=c.client.post('/custom/skills/professional',data={'name':pro,'display_name':'统一内核账单核对','description':'开发验证用的确定性账单核对技能'},files={'package':('ledger.zip',archive.getvalue(),'application/zip')});r.raise_for_status();pro_id=r.json()['data']['id']
    config=next(a['config'] for a in c.client.get('/agents').json()['data'] if a['id']=='builtin-general-agent')
    config.update({'professional_skills_selection_mode':'selected','selected_professional_skills':[pro],'lightweight_skills_selection_mode':'none','selected_lightweight_skills':[]})
    r=c.client.post('/agents',json={'name':'统一内核专业技能检查 '+suffix,'config':config});r.raise_for_status();agent=r.json()['data']['id']
    try:
        c.qa('skill-lightweight','公司实用新型专利奖励标准是多少？三位发明人的分配比例有明文规定吗？',agent='builtin-knowledge-qa',kb=[KB],extra={'skill_names':[light]},expected=('已确认事实','尚待确认','2000'))
        r=c.qa('skill-professional','请使用我选中的账单核对专业技能，执行其中的核对程序，给出合计并交付结果文件。',agent=agent,extra={'professional_skill_names':[pro]},expected=('167',))
        artifacts=c.artifacts(r);print(json.dumps({'artifact_checks':artifacts},ensure_ascii=False),flush=True)
        assert artifacts and json.loads((ROOT/'downloads/账单核对.json').read_text(encoding='utf-8'))['total']==167
        c.qa('skill-combined','使用选中的专业技能完成账单核对，并按轻量技能要求组织核对结论。',agent=agent,extra={'professional_skill_names':[pro],'skill_names':[light]},expected=('167','已确认事实','尚待确认'))
    finally:
        for path in ['/agents/'+agent,'/custom/skills/'+light_id,'/custom/skills/professional/'+pro_id]:
            c.client.delete(path).raise_for_status()


if __name__=='__main__':
    p=argparse.ArgumentParser();p.add_argument('group',choices=['profiles','skills']);args=p.parse_args()
    {'profiles':profiles,'skills':skills}[args.group](DevClient())
