"""Second-stage local experiments; requires lab.py's existing fixture/indexes.

Literal prefilters below are proven manually for these named fixtures ONLY.
They are NOT a general-purpose regex compiler or production implementation.
"""
import concurrent.futures
import json
from pathlib import Path
import threading
import time
import psycopg
import lab

DSN=dict(host="/var/run/postgresql",user="postgres",dbname="WeKnora",autocommit=True,
         prepare_threshold=None,options=f"-c search_path={lab.SCHEMA},public")
P_BODY=lab.BODY.replace("AND c.content ~* $18", "AND c.content ILIKE ANY($21) AND c.content ~* $18")
P_TITLE=lab.TITLE.replace("AND k.title ~* $19", "AND k.title ILIKE ANY($21) AND k.title ~* $19")
P_UNION=f"SELECT id,created_at FROM (({P_BODY}) UNION ({P_TITLE})) u ORDER BY {lab.ORDER} LIMIT $20"
VALUES=','.join(f'(${4+2*i}::text,${5+2*i}::integer)' for i in range(7))
LOCAL="c.knowledge_base_id=s.kb AND c.tenant_id=s.tenant"
L_OR=f"SELECT DISTINCT id,created_at FROM (VALUES {VALUES}) s(kb,tenant) CROSS JOIN LATERAL ({lab.BASE.replace(lab.SCOPE,LOCAL)}) q ORDER BY {lab.ORDER} LIMIT $20"
L_PREF=f"SELECT DISTINCT id,created_at FROM (VALUES {VALUES}) s(kb,tenant) CROSS JOIN LATERAL (({P_BODY.replace(lab.SCOPE,LOCAL)}) UNION ({P_TITLE.replace(lab.SCOPE,LOCAL)})) q ORDER BY {lab.ORDER} LIMIT $20"
CASES=[('contract','合同金额|立项金额|超支|变更审批|金额变更',list(range(1,8)),['%合同金额%','%立项金额%','%超支%','%变更审批%','%金额变更%']),
       ('short','预案',list(range(1,8)),['%预案%']),
       ('single','管理',[4],['%管理%']),
       ('broad','管理',list(range(1,8)),['%管理%']),
       ('rare','信息化基础设施',list(range(1,8)),['%信息化基础设施%']),
       ('empty','不可能命中XYZ987654321',list(range(1,8)),['%不可能命中XYZ987654321%']),
       ('regex','合同.{0,6}(金额|管理)|主数据',list(range(1,8)),['%合同%','%主数据%']),
       ('onechar','餐',list(range(1,8)),['%餐%']),
       ('matchall','.*',list(range(1,8)),['%']),
       ('optional','(合同)?',list(range(1,8)),['%'])]
VARIANTS=[('baseline',lab.BASE),('prefilter_union',P_UNION),('per_scope_original',L_OR),('per_scope_prefilter',L_PREF)]


def vals(case,sql):
    _,pattern,scope,pref=case
    v=lab.args(pattern,scope)
    v=[str(x) if i in range(3,17,2) else x for i,x in enumerate(v)]
    return v+[pref] if '$21' in sql else v


def run():
    out={'serial':[],'concurrent':[],'scope_tests':[]}
    gold={}
    with psycopg.connect(**DSN) as c:
        c.execute("SET statement_timeout='20s'")
        c.execute("SET plan_cache_mode=force_custom_plan")
        # Rotate variants within each case to reduce phase/cache-order bias.
        for case in CASES:
            for label,sql in VARIANTS:
                lab.prepare(c,sql)
                metrics=[]
                for _ in range(3):
                    p=c.execute(lab.execute_sql(c,vals(case,sql),True)).fetchone()[0][0]
                    metrics.append({'ms':p['Execution Time'],'read':p['Plan']['Shared Read Blocks'],'hit':p['Plan']['Shared Hit Blocks']})
                rows=c.execute(lab.execute_sql(c,vals(case,sql))).fetchall()
                if case[0] not in gold: gold[case[0]]=lab.digest(rows)
                record=dict(case=case[0],variant=label,metrics=metrics,count=len(rows),same_ordered_ids=lab.digest(rows)==gold[case[0]])
                out['serial'].append(record)
                print(json.dumps(record),flush=True)
        # All modes, including automatic prepared-plan reuse after the fifth call.
        for mode in ['auto','force_generic_plan','force_custom_plan']:
            c.execute('SET plan_cache_mode='+mode)
            lab.prepare(c,P_UNION)
            for i in range(8):
                case=CASES[i%len(CASES)]
                t=time.perf_counter()
                rows=c.execute(lab.execute_sql(c,vals(case,P_UNION))).fetchall()
                out['scope_tests'].append(dict(mode=mode,iteration=i+1,case=case[0],ms=1000*(time.perf_counter()-t),same_ordered_ids=lab.digest(rows)==gold[case[0]]))
            out.setdefault('prepared_counts',[]).append((mode,c.execute("SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name='lab'").fetchone()))
    def worker(sql,seed):
        data=[]
        with psycopg.connect(**DSN) as c:
            c.execute("SET statement_timeout='20s'")
            c.execute("SET plan_cache_mode=force_custom_plan")
            lab.prepare(c,sql)
            for j in range(10):
                case=CASES[(seed+j)%len(CASES)]
                t=time.perf_counter()
                rows=c.execute(lab.execute_sql(c,vals(case,sql))).fetchall()
                data.append(dict(case=case[0],ms=1000*(time.perf_counter()-t),same_ordered_ids=lab.digest(rows)==gold[case[0]]))
        return data
    for label,sql in VARIANTS:
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            samples=sum(list(pool.map(lambda i:worker(sql,i),range(4))),[])
        row=dict(variant=label,concurrency=4,samples=samples)
        out['concurrent'].append(row)
        print('CONCURRENT '+label+' '+json.dumps([round(s['ms'],1) for s in samples]),flush=True)
    Path(__file__).with_name('extended_results.json').write_text(json.dumps(out,ensure_ascii=False,indent=2))


if __name__=='__main__': run()
