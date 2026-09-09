"""Visible local regression for immutable answer plus evidence-only citation pass."""
import argparse
import json
import re

from dev_client import DevClient, ROOT
from dev_probe import sql


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--name', required=True)
    parser.add_argument('--query', required=True)
    parser.add_argument('--agent', default='builtin-knowledge-qa')
    parser.add_argument('--session', required=True)
    parser.add_argument('--model', required=True)
    parser.add_argument('--kb', required=True)
    parser.add_argument('--inspect-only', action='store_true', help='Recheck saved output without a model request')
    args = parser.parse_args()
    report = (json.loads((ROOT / (args.name+'.json')).read_text(encoding='utf-8')) if args.inspect_only
              else DevClient().qa(args.name, args.query, agent=args.agent,
                  session=args.session, model=args.model, kb=[args.kb]))
    run_id = report['run']['id']
    row = sql("SELECT row_to_json(t) FROM (SELECT \"references\", checkpoint->'agent'->'context' AS context, checkpoint->'agent'->'middle_context'->'citation_pass' AS citation_pass, "
              "payload->'runtime_config'->>'agent_type' AS agent_type FROM custom_agent_runs WHERE id='"+run_id+"')t;")[0]
    calls = [block for message in row['context'] for block in message.get('content', [])
             if block.get('type') == 'tool_call' and block.get('name') == 'GenerateStructuredOutput']
    raw = json.loads(calls[-1]['input']) if calls else {}
    registry = {ref.get('metadata', {}).get('citation_id'): ref for ref in row.get('references') or []}
    result = report['run']['result']
    citations = result.get('citations') or []
    used = re.findall(r'<src id="(S[1-9][0-9]*)" />', result['answer'])
    unique = list(dict.fromkeys(used))
    final_refs = {ref.get('metadata', {}).get('citation_id') for ref in result.get('references') or []}
    check = {
        'one_final_submission': len(calls) == 1,
        'body_only_first_pass': set(raw) == {'answer'},
        'second_pass_complete': (row.get('citation_pass') or {}).get('status') == 'complete' and result.get('citation_status') == 'complete',
        'retained_citations': bool(citations),
        'all_anchors_exact': all(item['text'] in raw.get('answer', '') for item in citations),
        'all_offsets_exact': all(raw.get('answer', '').encode()[item['end']-len(item['text'].encode()):item['end']].decode() == item['text'] for item in citations),
        'all_ids_current_and_data_present': all(identity in registry and registry[identity].get('evidence_content')
             for item in citations for identity in item['source_ids']),
        'only_cited_refs_persisted': final_refs == set(unique),
        'prose_preserved': re.sub(r'<src id="S[1-9][0-9]*" />', '', result['answer']) == raw.get('answer'),
        'answer_published_once_after_commit': len([e for e in report['events'] if e.get('response_type') in ('answer', 'final_answer') and e.get('content')]) == 1,
    }
    report['citation_regression'] = {'checks': check, 'raw_model_json': raw,
        'agent_type': row['agent_type'], 'complete_source_count': len(registry),
        'final_source_ids': unique, 'frontend_display_mapping': {identity:i+1 for i, identity in enumerate(unique)},
        'final_submission_count': len(calls)}
    (ROOT / (args.name+'.json')).write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding='utf-8')
    print(json.dumps(report['citation_regression'], ensure_ascii=False), flush=True)
    if not all(report['checks'].values()) or not all(check.values()):
        raise SystemExit('Citation regression did not pass; inspect the saved report before another live probe.')


if __name__ == '__main__':
    main()
