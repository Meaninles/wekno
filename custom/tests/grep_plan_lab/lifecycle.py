"""Read-model lifecycle prototype validation, not production migration code."""
import json
from pathlib import Path
import time
import psycopg
import extended as ex


def run():
    out=[]
    with psycopg.connect(**ex.DSN) as a,psycopg.connect(**ex.DSN) as b:
        for c in [a,b]: c.execute("SET statement_timeout='20s'")
        def check(label,expected):
            n=a.execute("SELECT count(*) FROM search_projection WHERE knowledge_id='lab-lifecycle'").fetchone()[0]
            assert n==expected,(label,n)
            out.append(dict(case=label,count=n,passed=True))
        a.execute("BEGIN")
        a.execute("INSERT INTO knowledges(id,title,knowledge_base_id,tenant_id,publication_state) VALUES ('lab-lifecycle','旧标题','2',3,'published')")
        a.execute("INSERT INTO chunks(id,content,knowledge_id,knowledge_base_id,tenant_id,is_enabled,chunk_type,created_at,title) SELECT md5('lab-lifecycle-'||g),'旧正文','lab-lifecycle','2',3,true,'text',now(),'旧标题' FROM generate_series(1,100) g")
        check('insert_100',100)
        a.execute('COMMIT')
        try:
            a.execute('BEGIN')
            a.execute("UPDATE knowledges SET publication_state='draft' WHERE id='lab-lifecycle'")
            check('unpublish_in_writer',0)
            assert b.execute("SELECT count(*) FROM search_projection WHERE knowledge_id='lab-lifecycle'").fetchone()[0]==100
            out.append(dict(case='reader_sees_previous_committed_version',passed=True))
            a.execute('COMMIT')
            assert b.execute("SELECT count(*) FROM search_projection WHERE knowledge_id='lab-lifecycle'").fetchone()[0]==0
            out.append(dict(case='reader_sees_unpublish_after_commit',passed=True))
            a.execute('BEGIN')
            a.execute("UPDATE knowledges SET publication_state='published',title='新标题' WHERE id='lab-lifecycle'")
            check('republish',100)
            assert a.execute("SELECT count(*) FROM search_projection WHERE knowledge_id='lab-lifecycle' AND title='新标题'").fetchone()[0]==100
            out.append(dict(case='title_update',passed=True))
            for label,assignment,expected in [('disable','is_enabled=false',0),('enable','is_enabled=true',100),('soft_delete','deleted_at=now()',0),('restore','deleted_at=NULL',100),('summary',"chunk_type='summary'",0),('physical_text',"chunk_type='text'",100)]:
                a.execute("UPDATE chunks SET "+assignment+" WHERE knowledge_id='lab-lifecycle'")
                check(label,expected)
            a.execute("UPDATE chunks SET content='新正文' WHERE knowledge_id='lab-lifecycle'")
            assert a.execute("SELECT count(*) FROM search_projection WHERE knowledge_id='lab-lifecycle' AND content='新正文'").fetchone()[0]==100
            out.append(dict(case='content_update',passed=True))
            a.execute("DELETE FROM chunks WHERE knowledge_id='lab-lifecycle'")
            check('physical_delete',0)
            a.execute('ROLLBACK')
            check('rollback_restores_committed_unpublished_state',0)
            # Quantify a large document title update including projection upkeep.
            doc,n=a.execute("SELECT knowledge_id,count(*) FROM search_projection GROUP BY knowledge_id ORDER BY count(*) DESC LIMIT 1").fetchone()
            a.execute('BEGIN')
            t=time.perf_counter()
            a.execute("UPDATE knowledges SET title=title||'实验后缀' WHERE id=%s",(doc,))
            out.append(dict(case='large_document_title_write',active_chunks=n,ms=1000*(time.perf_counter()-t)))
            a.execute('ROLLBACK')
        finally:
            a.execute('ROLLBACK')
            a.execute("DELETE FROM chunks WHERE knowledge_id='lab-lifecycle'")
            a.execute("DELETE FROM knowledges WHERE id='lab-lifecycle'")
    Path(__file__).with_name('lifecycle_results.json').write_text(json.dumps(out,ensure_ascii=False,indent=2))
    print(json.dumps(out),flush=True)


if __name__=='__main__': run()
