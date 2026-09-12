"""PostgreSQL grep experiment in a dedicated schema of the LOCAL database.

Copies a bounded projection of LOCAL chunks; redistributes scope/status for a
multi-tenant stress fixture. Output contains metrics and hashes, not document text.
Requires Docker and psycopg. Does not alter application tables or server settings.
"""
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time

import psycopg

SCHEMA = "grep_plan_lab_20260912"
ROOT = Path(__file__).resolve().parent
COLS = "c.id,c.content,c.knowledge_id,c.knowledge_base_id,c.tenant_id,c.is_enabled,c.chunk_type,c.deleted_at,c.created_at,k.title"
ORDER = "created_at DESC,id DESC"
SCOPE = "(" + " OR ".join(f"(c.knowledge_base_id=$%d AND c.tenant_id=$%d)" % (4+2*i,5+2*i) for i in range(7)) + ")"
LIVE = "c.is_enabled=$1 AND c.chunk_type<>$2 AND c.deleted_at IS NULL"
PUB = "k.deleted_at IS NULL AND k.publication_state=$3"
MATCH = "(c.content ~* $18 OR k.title ~* $19)"
JOIN = "chunks c JOIN knowledges k ON k.id=c.knowledge_id"
SELECT = "SELECT c.id,c.created_at"
BASE = f"{SELECT} FROM {JOIN} WHERE {LIVE} AND {PUB} AND {SCOPE} AND {MATCH} AND c.deleted_at IS NULL ORDER BY c.created_at DESC,c.id DESC LIMIT $20"
SCOPED = f"WITH scope AS MATERIALIZED (SELECT c.id,c.created_at,c.content,k.title FROM {JOIN} WHERE {LIVE} AND {PUB} AND {SCOPE}) SELECT id,created_at FROM scope WHERE content ~* $18 OR title ~* $19 ORDER BY {ORDER} LIMIT $20"
BODY = f"{SELECT} FROM {JOIN} WHERE {LIVE} AND {PUB} AND {SCOPE} AND c.content ~* $18 ORDER BY c.created_at DESC,c.id DESC LIMIT $20"
TITLE = f"{SELECT} FROM {JOIN} WHERE {LIVE} AND {PUB} AND {SCOPE} AND k.title ~* $19 ORDER BY c.created_at DESC,c.id DESC LIMIT $20"
UNION = f"SELECT id,created_at FROM (({BODY}) UNION ({TITLE})) u ORDER BY {ORDER} LIMIT $20"


def docker(*args, **kwargs):
    return subprocess.run(["docker", *args], check=True, **kwargs)


def prepare(conn, sql):
    conn.execute("DEALLOCATE ALL")
    conn.execute("PREPARE lab AS " + sql)


def args(pattern, scope):
    pairs = [(i, 1+(i % 4)) for i in scope]
    pairs += [(-1,-1)] * (7-len(pairs))
    return [True, "summary", "published", *[v for p in pairs for v in p], pattern, pattern, 500]


def execute_sql(conn, values, explain=False):
    # EXECUTE itself cannot use protocol bind parameters. psycopg composes safely.
    from psycopg import sql
    return sql.SQL(("EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON,TIMING OFF) " if explain else "") + "EXECUTE lab({})").format(sql.SQL(",").join(map(sql.Literal, values)))


def digest(rows):
    return hashlib.sha256("\n".join(str(r[0]) for r in rows).encode()).hexdigest()


def main():
    in_container = os.environ.get("GREP_LAB_IN_CONTAINER") == "1"
    password = "" if in_container else subprocess.check_output(["docker","exec","WeKnora-postgres-dev","printenv","POSTGRES_PASSWORD"],text=True).strip()
    dsn = dict(host="/var/run/postgresql" if in_container else "127.0.0.1",port=5432,user="postgres",password=password,dbname="WeKnora",autocommit=True,connect_timeout=3,
               options=f"-c search_path={SCHEMA},public", prepare_threshold=None)
    result = {"fixture":"300k bounded local chunks; synthetic tenant/KB/status distribution; existing local PostgreSQL, separate schema", "serial":[],"concurrent":[]}
    try:
        for _ in range(60):
            try:
                conn=psycopg.connect(**dsn)
                break
            except psycopg.OperationalError:
                time.sleep(1)
        else:
            raise RuntimeError("Lab postgres failed to start")
        if os.environ.get("GREP_LAB_REUSE_FIXTURE") != "1":
            conn.execute("CREATE SCHEMA " + SCHEMA)
            conn.execute("CREATE TABLE chunks AS SELECT "+COLS+" FROM public.chunks c JOIN public.knowledges k ON k.id=c.knowledge_id ORDER BY c.id LIMIT 300000")
            conn.execute("ALTER TABLE chunks ADD PRIMARY KEY(id)")
            print("Fixture copied",flush=True)
            # Allocate every document to exactly one synthetic KB and tenant.
            conn.execute("UPDATE chunks SET knowledge_base_id=(1+((hashtextextended(knowledge_id,0)&2147483647)%20))::text")
            conn.execute("UPDATE chunks SET tenant_id=1+(knowledge_base_id::int%4),is_enabled=((hashtextextended(id,1)&2147483647)%10>=3),deleted_at=CASE WHEN (hashtextextended(id,2)&2147483647)%10<4 THEN now() ELSE NULL END")
            conn.execute("CREATE TABLE knowledges AS SELECT DISTINCT ON(knowledge_id) knowledge_id AS id,title,knowledge_base_id,tenant_id,NULL::timestamptz AS deleted_at,'published'::text AS publication_state FROM chunks ORDER BY knowledge_id,id")
            conn.execute("ALTER TABLE knowledges ADD PRIMARY KEY(id)")
            conn.execute("CREATE INDEX chunks_scope ON chunks(knowledge_base_id,tenant_id)")
            conn.execute("CREATE INDEX chunks_knowledge_enabled ON chunks(knowledge_id,is_enabled,deleted_at)")
            conn.execute("VACUUM ANALYZE chunks")
            conn.execute("ANALYZE knowledges")
        conn.execute("SET statement_timeout='20s'")
        result["size_before"] = conn.execute("SELECT count(*),pg_total_relation_size('chunks') FROM chunks").fetchone()
        cases=[("contract","合同金额|立项金额|超支|变更审批|金额变更",list(range(1,8))),
               ("short","预案",list(range(1,8))),
               ("single","主数据|主数据管理",[1]),
               ("broad","管理",list(range(1,8))),
               ("rare","信息化基础设施",list(range(1,8))),
               ("empty","不可能命中XYZ987654321",list(range(1,8))),
               ("regex","合同.{0,6}(金额|管理)|主数据",list(range(1,8))),
               ("onechar","餐",list(range(1,8)))]
        gold={}
        def bench(label, sql, mode, runs=3):
            conn.execute("SET plan_cache_mode="+mode)
            prepare(conn,sql)
            for name,pattern,scope in cases:
                vals=args(pattern,scope)
                # KB type is text, as in production.
                vals=[str(v) if i in range(3,17,2) else v for i,v in enumerate(vals)]
                row={"variant":label,"mode":mode,"case":name,"times_ms":[],"reads":[],"hits":[],"plans_ms":[]}
                for _ in range(runs):
                    p=conn.execute(execute_sql(conn,vals,True)).fetchone()[0][0]
                    row["times_ms"].append(p["Execution Time"])
                    row["plans_ms"].append(p["Planning Time"])
                    row["reads"].append(p["Plan"]["Shared Read Blocks"])
                    row["hits"].append(p["Plan"]["Shared Hit Blocks"])
                rows=conn.execute(execute_sql(conn,vals)).fetchall()
                h=digest(rows)
                if name not in gold: gold[name]=h
                row.update(count=len(rows),same_ordered_ids=(h==gold[name]))
                result["serial"].append(row)
                print(json.dumps(row),flush=True)
        bench("baseline",BASE,"force_generic_plan")
        bench("custom",BASE,"force_custom_plan")
        bench("scope_fence",SCOPED,"force_custom_plan")
        start=time.perf_counter()
        conn.execute("CREATE INDEX chunks_live_scope ON chunks(knowledge_base_id,tenant_id,created_at DESC,id DESC) WHERE is_enabled AND deleted_at IS NULL AND chunk_type<>'summary'")
        result["scope_index_build_s"]=time.perf_counter()-start
        bench("scope_index",BASE,"force_custom_plan")
        start=time.perf_counter()
        conn.execute("SET statement_timeout='180s'")
        conn.execute("CREATE INDEX chunks_body_trgm ON chunks USING gin(content gin_trgm_ops) WHERE is_enabled AND deleted_at IS NULL AND chunk_type<>'summary'")
        conn.execute("CREATE INDEX knowledges_title_trgm ON knowledges USING gin(title gin_trgm_ops) WHERE deleted_at IS NULL AND publication_state='published'")
        result["trgm_build_s"]=time.perf_counter()-start
        conn.execute("ANALYZE chunks")
        conn.execute("ANALYZE knowledges")
        conn.execute("SET statement_timeout='20s'")
        bench("indexes_only",BASE,"force_custom_plan")
        bench("unified_union",UNION,"force_custom_plan")
        result["index_bytes"]=conn.execute("SELECT indexrelname,pg_relation_size(indexrelid) FROM pg_stat_user_indexes ORDER BY indexrelname").fetchall()
        # Concurrent connections share the experiment's own buffers and CPU limit.
        def worker(sql, seed):
            with psycopg.connect(**dsn) as c:
                c.execute("SET statement_timeout='20s'")
                c.execute("SET plan_cache_mode=force_custom_plan")
                prepare(c,sql)
                timings=[]
                for j in range(4):
                    name,pat,scope=cases[(seed+j)%len(cases)]
                    vals=args(pat,scope)
                    vals=[str(v) if i in range(3,17,2) else v for i,v in enumerate(vals)]
                    t=time.perf_counter()
                    rows=c.execute(execute_sql(c,vals)).fetchall()
                    timings.append(dict(case=name,ms=round((time.perf_counter()-t)*1000,3),same_ordered_ids=digest(rows)==gold[name]))
                return timings
        for label,sql in [("indexes_only",BASE),("unified_union",UNION)]:
            for concurrency in [2,4]:
                with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
                    samples=sum(list(pool.map(lambda i:worker(sql,i),range(concurrency))),[])
                record=dict(variant=label,concurrency=concurrency,samples=samples)
                result["concurrent"].append(record)
                print(json.dumps(record),flush=True)
        conn.close()
    finally:
        # Benchmark artifacts are generated data; never contain source documents.
        (ROOT/"results.json").write_text(json.dumps(result,ensure_ascii=False,indent=2),encoding="utf-8")
        print("Results saved; application tables unchanged",flush=True)


if __name__ == "__main__":
    main()
