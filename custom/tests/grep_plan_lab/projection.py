"""Measure a compact active-search projection, with unchanged regex semantics."""
import concurrent.futures
import json
from pathlib import Path
import time
import psycopg
import lab
import extended as ex

PROJ=f"SELECT c.id,c.created_at FROM search_projection c WHERE {lab.SCOPE} AND (c.content ~* $18 OR c.title ~* $19) ORDER BY c.created_at DESC,c.id DESC LIMIT $20"
# Keep all PREPARE parameter types defined, although live/publication states have
# already been applied when constructing this materialized experimental fixture.
PROJ=PROJ.replace('WHERE ',"WHERE $1::boolean AND $2::text='summary' AND $3::text='published' AND ",1)
LATERAL=f"SELECT DISTINCT id,created_at FROM (VALUES {ex.VALUES}) s(kb,tenant) CROSS JOIN LATERAL ({PROJ.replace(lab.SCOPE,ex.LOCAL)}) q ORDER BY {lab.ORDER} LIMIT $20"


def run():
    result={'serial':[],'concurrent':[]}
    gold={}
    with psycopg.connect(**ex.DSN) as c:
        c.execute("SET statement_timeout='120s'")
        t=time.perf_counter()
        c.execute("CREATE TABLE search_projection AS SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title FROM chunks c JOIN knowledges k ON k.id=c.knowledge_id WHERE c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type<>'summary' AND k.deleted_at IS NULL AND k.publication_state='published' ORDER BY c.knowledge_base_id,c.tenant_id,c.created_at DESC,c.id DESC")
        c.execute("ALTER TABLE search_projection ADD PRIMARY KEY(id)")
        c.execute("CREATE INDEX projection_scope_order ON search_projection(knowledge_base_id,tenant_id,created_at DESC,id DESC)")
        c.execute("ANALYZE search_projection")
        result['build_s']=time.perf_counter()-t
        result['size']=c.execute("SELECT count(*),pg_relation_size('search_projection'),pg_total_relation_size('search_projection') FROM search_projection").fetchone()
        c.execute("SET statement_timeout='20s'")
        c.execute("SET plan_cache_mode=force_custom_plan")
        for case in ex.CASES:
            for label,sql in [('baseline',lab.BASE),('projection',PROJ),('projection_per_scope',LATERAL)]:
                lab.prepare(c,sql)
                metrics=[]
                for _ in range(3):
                    p=c.execute(lab.execute_sql(c,ex.vals(case,sql),True)).fetchone()[0][0]
                    metrics.append({'ms':p['Execution Time'],'read':p['Plan']['Shared Read Blocks'],'hit':p['Plan']['Shared Hit Blocks']})
                rows=c.execute(lab.execute_sql(c,ex.vals(case,sql))).fetchall()
                if case[0] not in gold: gold[case[0]]=lab.digest(rows)
                record=dict(case=case[0],variant=label,metrics=metrics,count=len(rows),same_ordered_ids=lab.digest(rows)==gold[case[0]])
                result['serial'].append(record)
                print(json.dumps(record),flush=True)
    def worker(sql,seed):
        samples=[]
        with psycopg.connect(**ex.DSN) as c:
            c.execute("SET statement_timeout='20s'")
            c.execute("SET plan_cache_mode=force_custom_plan")
            lab.prepare(c,sql)
            for j in range(10):
                case=ex.CASES[(seed+j)%10]
                t=time.perf_counter()
                rows=c.execute(lab.execute_sql(c,ex.vals(case,sql))).fetchall()
                samples.append(dict(case=case[0],ms=1000*(time.perf_counter()-t),same_ordered_ids=lab.digest(rows)==gold[case[0]]))
        return samples
    for label,sql in [('baseline',lab.BASE),('projection',PROJ),('projection_per_scope',LATERAL)]:
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            samples=sum(list(pool.map(lambda i:worker(sql,i),range(4))),[])
        result['concurrent'].append(dict(variant=label,samples=samples))
        print('CONCURRENT '+label+' '+json.dumps([round(s['ms'],1) for s in samples]),flush=True)
    Path(__file__).with_name('projection_results.json').write_text(json.dumps(result,ensure_ascii=False,indent=2))


if __name__=='__main__': run()
