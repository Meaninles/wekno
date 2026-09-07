"""Run named, inspectable development scenarios; no legacy eval thresholds."""
import argparse
import json

from dev_client import DevClient, ROOT, KB
from dev_probe import sql


def uploads(c):
    names=['project.docx','project.pdf','project.pptx','project.txt','project.md','project.json','sales.csv','sales.xlsx','facts.png','facts.jpg']
    for name in names:
        try:
            # Reuse files already accepted by earlier transport probes.
            rows=sql("SELECT row_to_json(t) FROM (SELECT k.id,kb.chat_session_id AS session FROM knowledges k JOIN knowledge_bases kb ON kb.id=k.knowledge_base_id JOIN sessions s ON s.id=kb.chat_session_id WHERE s.title='统一内核场景检查 · 上传 "+name+"' AND k.deleted_at IS NULL ORDER BY k.created_at DESC LIMIT 1)t;")
            if rows:
                session=rows[0]['session'];item=c.wait_upload(session,rows[0]['id'])
            else:
                session=c.session('上传 '+name);item=c.upload(session,ROOT/'fixtures'/name)
            query='请准确提取上传文件的项目编号、预算和截止日期；若文件只含商品表，则计算 A 和 B 的总金额。不需要创建新文件。'
            c.qa('upload-'+name.replace('.','-'),query,agent='builtin-general-agent',session=session,uploads=[item['id']])
        except Exception as exc:
            error={'file':name,'error':str(exc)[:1500]}
            print(json.dumps(error,ensure_ascii=False),flush=True)
            (ROOT/('upload-'+name.replace('.','-')+'-error.json')).write_text(json.dumps(error,ensure_ascii=False),encoding='utf-8')


def company(c):
    cases=[
      ('company-colloquial','我利用公司的设备做了一项小发明，拿到了国内实用新型专利，后来同一项又获国际授权。公司奖金怎么算，能领两次吗？给我依据。'),
      ('company-cross-doc','公司研发成果涉及职务发明专利奖励时，《专利管理实施办法》和《人才发展经费管理使用办法》分别规定了什么？请区分奖励标准、经费来源和发放职责，分别引用来源。'),
      ('company-similar','请区分公司《风险管理办法》和《风险管理操作细则》的作用，员工发现重大风险时应向谁报告？仅按查到的制度作答并引用。'),
      ('company-no-evidence','公司有没有规定员工在火星出差时每天可报销多少星际燃料费？请找准确条款；找不到就明确说明。'),
    ]
    for name,query in cases:
        c.qa(name,query,agent='builtin-knowledge-qa',kb=[KB])


def long_chat(c):
    session=c.session('16轮长上下文')
    facts=[
      '项目代号是LANTERN-7429，最初预算186400元，负责人陈岚。',
      '计划上线日期为2026年11月19日，试点区域贵州。',
      '采购方案A为3件单价12元，方案B为2件单价8元。',
      '更正：负责人改为周宁，陈岚负责验收。',
      '质量红线：不能删减审计日志；审计日志保留180天。',
      '有两个发布窗口：每周二和周四的22点。',
      '环境只有开发和生产，演示使用开发环境。',
      '更正：预算调减6400元，其余不变。',
      '回滚负责人为李林，故障告警要同步给周宁。',
      '数据核对要关注明细条数、金额汇总、重复记录三个项目。',
      '验收要求包括手机、桌面和嵌入页面。',
      '允许维护时间最长30分钟，超过必须回滚。',
      '不要把测试数据当成公司制度。本对话是项目记录。',
      '更正：上线日期顺延7天；发布窗口仍然不变。',
      '采购数量保持第3轮给出的值，金额不计入项目预算调整。',
    ]
    # Long, distinct user records trigger real history budgeting/read handles.
    for index,fact in enumerate(facts,1):
        notes='\n'.join(f'记录{index}-{j:03d}：本项为部署检查背景，核对日志是否完整、告警是否及时、文档是否更新，不改变已确认的项目决策。' for j in range(35))
        c.qa(f'long-turn-{index:02d}',fact+'\n背景记录：\n'+notes+'\n请只简短确认当前这条决定。',session=session,agent='builtin-general-agent')
    c.qa('long-turn-16','请根据前15轮给出最新的项目代号、预算、负责人、验收人、上线日期、审计保留天数、允许维护时长，以及两种采购的总金额。区分被更正的旧值，不要询问我重复信息。',session=session,agent='builtin-general-agent',expected=('LANTERN-7429','180000','周宁','陈岚','26','180','30','52'))


def large(c):
    for name in ('large-near-limit.pdf','large-near-limit.xlsx'):
        record=json.loads((ROOT/(name+'.upload.json')).read_text(encoding='utf-8'))
        item=c.wait_upload(record['session'],record['item']['id'])
        c.qa('large-'+name.rsplit('.',1)[1],
             '请检查上传大文件的第1和第10页（Excel对应第1和第10工作表），给出两个Control value并计算差值。不能只看文件开头。',
             agent='builtin-general-agent',session=record['session'],uploads=[item['id']],expected=('1000','1063','63'))


def company_long(c):
    session=c.session('公司制度18轮切题与深度追问')
    questions=[
      '依据公司《专利管理实施办法》，详细说明职务发明创造的认定、专利申请归口管理部门和申请流程。请分清条款原文与解释，并引用依据。',
      '那如果我是在下班后利用公司设备完成的小发明，专利归我个人吗？沿用刚才的制度逐项分析，不要仅凭下班时间下结论。',
      '继续追问：既然是我完成的，是否可以先以个人名义向外申请，再通知归口部门？请把需要的审批和提交环节讲清楚。',
      '现在看奖励：同一项成果先获得国内实用新型授权，之后又获国际授权，金额怎样算？可否重复领取？请列明对应条款和计算。',
      '这里的金额是税前还是税后？假如发明人有三位，制度明确规定平均分配了吗？没有明确规定就说明缺口。',
      '换个话题，查看《人才发展经费管理使用办法》。这笔经费来源和使用范围是什么？与我们刚才讨论的专利奖励有哪些关联？请展开并分别引用。',
      '深入一点：该经费从提出使用申请到审批、发放分别由谁负责？如果制度没有完整写出某一环节，请标出来。',
      '刚才两份制度是否存在奖励重复或经费来源冲突？请用对照表区分明文规定、能合理推断的关系和尚待确认事项，不能把推断当规定。',
      '暂时离开奖励话题。公司《风险管理办法》和《风险管理操作细则》分别解决什么问题？请详细比较适用范围、职责和流程并给出处。',
      '如果基层员工发现一项可能造成重大损失的风险，应向谁、按什么链路报告？这里继续说刚才的风险制度。',
      '继续深挖：风险识别、评估、应对和监控分别怎样衔接？请按先后顺序给一个不添加制度外要求的操作说明。',
      '你刚才说的报告链路是否有明确时限和金额门槛？请找直接证据；查不到准确数字就别自行补充。',
      '再切回最开始的小发明案例：还记得是下班后使用公司设备的情形吗？请复述这个前提，然后结合风险管理要求和专利管理要求列出应做的事项，明确哪些是直接条款、哪些仅为建议。',
      '不要重新泛讲风险。回到第4轮同一成果先国内实用新型、再国际授权的奖励计算，请给最终金额、税务口径和能否重复领取，引用原制度。',
      '如果有人说三位发明人必须平均分这笔钱，你会如何依据之前查到的内容核实或纠正？这里的这笔钱仍指上轮那项专利奖励。',
      '假设我刚才口误，实际上只有国内实用新型授权、没有国际授权，其余不变。请更新奖励金额，并指出哪些结论随这个更正发生变化。',
      '把之前讨论过的三组制度做成索引：专利管理、人才经费、风险管理。每组列出已确认结论、来源和仍没有找到明确规定的点。注意奖励案例已经更正。',
      '最后检查上下文：当前案例的发明时间和设备来源是什么、当前授权类型是什么、当前奖励应是多少、先前国际授权情况下的金额是多少、是否能重复领取、三人分配比例是否有明文？请逐项回答并引用依据，别把旧假设当成现状。',
    ]
    reports=[]
    for index,question in enumerate(questions,1):
        reports.append(c.qa(f'company-long-{index:02d}',question,session=session,agent='builtin-knowledge-qa',kb=[KB]))
        if not reports[-1]['checks']['completed']:
            raise RuntimeError('Inspect failed turn before continuing the conversation')
    (ROOT/'company-long-summary.json').write_text(json.dumps([{'name':r['name'],'session':session,'checks':r['checks'],'seconds':r['seconds'],'model_requests':r['run']['model_requests']} for r in reports],ensure_ascii=False,indent=2),encoding='utf-8')


if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('group',choices=('uploads','company','long','large','company-long'));args=parser.parse_args()
    {'uploads':uploads,'company':company,'long':long_chat,'large':large,'company-long':company_long}[args.group](DevClient())
