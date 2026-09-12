"""Include original payload fetch and the existing per-document chunk counts."""
import json
from pathlib import Path
import time
import psycopg
import lab
import extended as ex
import projection as pr


def run():
    out=[]
    with psycopg.connect(**ex.DSN) as c:
        c.execute("SET statement_timeout='20s'")
        c.execute("SET plan_cache_mode=force_custom_plan")
        for case in ex.CASES:
            gold=None
            for name,sql in [('baseline',lab.BASE),('projection',pr.PROJ)]:
                full=f"WITH selected AS MATERIALIZED ({sql}) SELECT c.id,c.content,c.knowledge_id,c.knowledge_base_id,c.chunk_type,c.created_at,k.title FROM selected s JOIN chunks c ON c.id=s.id JOIN knowledges k ON k.id=c.knowledge_id ORDER BY c.created_at DESC,c.id DESC"
                lab.prepare(c,full)
                t=time.perf_counter()
                rows=c.execute(lab.execute_sql(c,ex.vals(case,sql))).fetchall()
                fetch_ms=1000*(time.perf_counter()-t)
                ids=list({r[2] for r in rows})
                t=time.perf_counter()
                counts=c.execute('SELECT knowledge_id,count(*) FROM chunks WHERE knowledge_id=ANY(%s) AND is_enabled AND deleted_at IS NULL GROUP BY knowledge_id ORDER BY knowledge_id',(ids,)).fetchall()
                count_ms=1000*(time.perf_counter()-t)
                if gold is None: gold=(rows,counts)
                assert gold==(rows,counts),case[0]
                r=dict(case=case[0],variant=name,rows=len(rows),fetch_ms=fetch_ms,count_ms=count_ms,identical_payload_and_counts=True)
                out.append(r)
                print(json.dumps(r),flush=True)
    Path(__file__).with_name('payload_results.json').write_text(json.dumps(out,ensure_ascii=False,indent=2))


if __name__=='__main__': run()
