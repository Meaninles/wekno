"""Additional scope-equivalence and prepared-plan tests for the projection."""
import json
from pathlib import Path
import time
import psycopg
import lab
import extended as ex
import projection as pr


def run():
    out={'scopes':[],'modes':[]}
    with psycopg.connect(**ex.DSN) as c:
        c.execute("SET statement_timeout='20s'")
        c.execute("CREATE TEMP TABLE lab_tags(knowledge_id text,tag_id text)")
        docs=c.execute("SELECT knowledge_id,knowledge_base_id,tenant_id FROM search_projection WHERE knowledge_base_id NOT IN ('1','2','3','4','5','6','7') GROUP BY 1,2,3 ORDER BY count(*) DESC LIMIT 2").fetchall()
        c.execute('INSERT INTO lab_tags VALUES(%s,%s)',(docs[0][0],'lab-tag'))
        from psycopg import sql
        tag=sql.SQL("(c.knowledge_base_id={} AND c.tenant_id={} AND EXISTS(SELECT 1 FROM lab_tags t WHERE t.knowledge_id=c.knowledge_id AND t.tag_id='lab-tag'))").format(sql.Literal(docs[0][1]),sql.Literal(docs[0][2])).as_string(c)
        file=sql.SQL('c.knowledge_id={}').format(sql.Literal(docs[1][0])).as_string(c)
        types='boolean,text,text,'+','.join(['text,integer']*7)+',text,text,integer'
        for name,scope in [('tag',tag),('file',file),('mixed',f'({lab.SCOPE} OR {tag} OR {file})')]:
            hashes=[];counts=[]
            for query in [lab.BASE,pr.PROJ]:
                c.execute('DEALLOCATE ALL')
                c.execute('PREPARE lab('+types+') AS '+query.replace(lab.SCOPE,scope))
                case=('scope','.*',list(range(1,8)),[])
                rows=c.execute(lab.execute_sql(c,ex.vals(case,query))).fetchall()
                hashes.append(lab.digest(rows));counts.append(len(rows))
            assert len(set(hashes))==1 and counts[0]>0,(name,counts,hashes)
            out['scopes'].append(dict(scope=name,counts=counts,identical_ordered_ids=True))
        golden={}
        for mode in ['force_custom_plan','force_generic_plan','auto']:
            c.execute('SET plan_cache_mode='+mode)
            lab.prepare(c,pr.PROJ)
            samples=[]
            for i in range(10):
                case=ex.CASES[i]
                t=time.perf_counter()
                rows=c.execute(lab.execute_sql(c,ex.vals(case,pr.PROJ))).fetchall()
                h=lab.digest(rows)
                if case[0] not in golden: golden[case[0]]=h
                assert golden[case[0]]==h
                samples.append(dict(case=case[0],ms=1000*(time.perf_counter()-t)))
            out['modes'].append(dict(mode=mode,samples=samples,plan_counts=c.execute("SELECT generic_plans,custom_plans FROM pg_prepared_statements WHERE name='lab'").fetchone()))
    Path(__file__).with_name('scope_modes_results.json').write_text(json.dumps(out,ensure_ascii=False,indent=2))
    print(json.dumps(out),flush=True)


if __name__=='__main__': run()
