"""Projection boundary and bounded concurrent-vector tests in the lab schema."""
import concurrent.futures
import json
from pathlib import Path
import threading
import time
import psycopg
import lab
import extended as ex
import projection as pr


def run():
    out={'edges':[],'stress':[]}
    with psycopg.connect(**ex.DSN) as c:
        c.execute("SET statement_timeout='30s'")
        # Only experiment tables are written; edge fixtures are rolled back.
        c.execute('BEGIN')
        c.execute("INSERT INTO knowledges(id,title,knowledge_base_id,tenant_id,deleted_at,publication_state) VALUES ('lab-edge-document','标题专用探针','2',3,NULL,'published'),('lab-edge-hidden','标题专用探针','2',3,NULL,'draft')")
        c.execute("INSERT INTO chunks(id,content,knowledge_id,knowledge_base_id,tenant_id,is_enabled,chunk_type,deleted_at,created_at,title) SELECT md5('lab-edge-'||g),'正文专用探针','lab-edge-document','2',3,true,'text',NULL,'2030-01-01'::timestamptz,'标题专用探针' FROM generate_series(1,600) g")
        # Explicit inactive, generated, wrong-tenant and unpublished counterexamples.
        c.execute("INSERT INTO chunks(id,content,knowledge_id,knowledge_base_id,tenant_id,is_enabled,chunk_type,deleted_at,created_at,title) SELECT md5('lab-invalid-'||g),'边界禁入探针',CASE WHEN g=5 THEN 'lab-edge-hidden' ELSE 'lab-edge-document' END,'2',CASE WHEN g=4 THEN 999 ELSE 3 END,g<>1,CASE WHEN g=2 THEN 'summary' ELSE 'text' END,CASE WHEN g=3 THEN now() ELSE NULL END,'2031-01-01'::timestamptz,'标题专用探针' FROM generate_series(1,5) g")
        c.execute("INSERT INTO search_projection SELECT c.id,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.created_at,c.content,k.title FROM chunks c JOIN knowledges k ON c.knowledge_id=k.id WHERE c.knowledge_id LIKE 'lab-edge-%' AND c.is_enabled AND c.deleted_at IS NULL AND c.chunk_type<>'summary' AND k.deleted_at IS NULL AND k.publication_state='published'")
        for label,pattern,expected in [('title_only','标题专用探针',500),('body_only','正文专用探针',500),('both','标题专用探针|正文专用探针',500),('unauthorized','边界禁入探针',0)]:
            case=(label,pattern,list(range(1,8)),[])
            hashes=[]
            for sql in [lab.BASE,pr.PROJ,pr.LATERAL]:
                lab.prepare(c,sql)
                rows=c.execute(lab.execute_sql(c,ex.vals(case,sql))).fetchall()
                assert len(rows)==expected,(label,len(rows))
                hashes.append(lab.digest(rows))
            assert len(set(hashes))==1,label
            out['edges'].append(dict(case=label,count=expected,identical_ordered_ids=True))
        c.execute('ROLLBACK')
        # Real local vectors; this is a bounded sequential-vector contention test,
        # not an assertion that production's ANN plan or latency is reproduced.
        c.execute("CREATE TABLE vectors AS SELECT embedding FROM public.embeddings WHERE embedding IS NOT NULL LIMIT 10000")
        out['vectors']=c.execute("SELECT count(*),pg_total_relation_size('vectors') FROM vectors").fetchone()
        c.execute("ANALYZE vectors")
    def vector_worker(stop,ready):
        n=0; durations=[]
        with psycopg.connect(**ex.DSN) as c:
            c.execute("SET statement_timeout='10s'")
            dims=c.execute('SELECT vector_dims(embedding::vector) FROM vectors LIMIT 1').fetchone()[0]
            q=c.execute('SELECT embedding::text FROM vectors LIMIT 1').fetchone()[0]
            ready.set()
            while not stop.is_set() and n<100:
                t=time.perf_counter()
                c.execute('SELECT embedding <=> %s::halfvec FROM vectors WHERE vector_dims(embedding::vector)=%s ORDER BY 1 LIMIT 200',(q,dims)).fetchall()
                durations.append(1000*(time.perf_counter()-t));n+=1
        return durations
    def grep_worker(sql,seed):
        samples=[]
        with psycopg.connect(**ex.DSN) as c:
            c.execute("SET statement_timeout='20s'")
            c.execute("SET plan_cache_mode=force_custom_plan")
            lab.prepare(c,sql)
            for j in range(10):
                case=ex.CASES[(j+seed)%10]
                t=time.perf_counter()
                rows=c.execute(lab.execute_sql(c,ex.vals(case,sql))).fetchall()
                samples.append(dict(case=case[0],ms=1000*(time.perf_counter()-t),hash=lab.digest(rows)))
        return samples
    for label,sql in [('baseline',lab.BASE),('projection',pr.PROJ),('projection_per_scope',pr.LATERAL)]:
        stop=threading.Event()
        ready=[threading.Event(),threading.Event()]
        with concurrent.futures.ThreadPoolExecutor(max_workers=6) as pool:
            vf=[pool.submit(vector_worker,stop,r) for r in ready]
            for r in ready:
                if not r.wait(10): raise RuntimeError('Vector worker startup timeout')
            try:
                gf=[pool.submit(grep_worker,sql,i) for i in range(4)]
                samples=sum([f.result() for f in gf],[])
            finally:
                stop.set()
            vectors=[f.result() for f in vf]
        row=dict(variant=label,grep_concurrency=4,vector_concurrency=2,samples=samples,vector_times=vectors)
        out['stress'].append(row)
        print('STRESS '+label+' '+json.dumps([round(s['ms'],1) for s in samples]),flush=True)
    # Same case must have identical ordered IDs across concurrent plans/runs.
    groups={}
    for row in out['stress']:
        for s in row['samples']: groups.setdefault(s['case'],set()).add(s['hash'])
    out['stress_identical_ordered_ids']=all(len(h)==1 for h in groups.values())
    assert out['stress_identical_ordered_ids']
    Path(__file__).with_name('acceptance_results.json').write_text(json.dumps(out,ensure_ascii=False,indent=2))
    print(json.dumps(out['edges']),flush=True)


if __name__=='__main__': run()
